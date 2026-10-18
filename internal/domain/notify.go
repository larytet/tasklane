package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"math"
	netmail "net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/mail"
)

// MailConfig turns on notification email. Without it, events are not turned into mail.
type MailConfig struct {
	Mailer  mail.Mailer
	From    string // "Tasklane <tasklane@example.com>"
	Domain  string // message-id domain
	BaseURL string
}

const (
	consumerNotifier = "notifier"
	maxAttempts      = 8
	leaseFor         = 5 * time.Minute
)

type notifyEvent struct {
	ID      int64
	Type    string
	Payload struct {
		Key     string   `json:"key"`
		Actor   string   `json:"actor"`
		ActorID string   `json:"actor_id"`
		Comment string   `json:"comment"`
		From    string   `json:"from"`
		To      string   `json:"to"`
		Fields  []string `json:"fields"`
	}
}

// NotifyOnce turns new outbox events into queued mail: one row per event and recipient, keyed so that a
// retry or a second node can never create a duplicate. It reads the outbox through its own cursor.
func (s *Service) NotifyOnce(ctx context.Context) (int, error) {
	if s.Mail == nil {
		return 0, nil
	}
	queued := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO outbox_cursor(consumer) VALUES($1) ON CONFLICT DO NOTHING`, consumerNotifier); err != nil {
			return err
		}
		var last int64
		if err := tx.QueryRow(ctx, `SELECT last_id FROM outbox_cursor WHERE consumer=$1 FOR UPDATE`, consumerNotifier).Scan(&last); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,type,payload FROM outbox WHERE id>$1 ORDER BY id LIMIT 100`, last)
		if err != nil {
			return err
		}
		var evs []notifyEvent
		for rows.Next() {
			var e notifyEvent
			var raw []byte
			if err := rows.Scan(&e.ID, &e.Type, &raw); err != nil {
				rows.Close()
				return err
			}
			json.Unmarshal(raw, &e.Payload)
			evs = append(evs, e)
		}
		rows.Close()
		for _, e := range evs {
			n, err := s.queueEvent(ctx, tx, e)
			if err != nil {
				return err
			}
			queued += n
			last = e.ID
		}
		_, err = tx.Exec(ctx, `UPDATE outbox_cursor SET last_id=$2 WHERE consumer=$1`, consumerNotifier, last)
		return err
	})
	return queued, err
}

type recipient struct{ id, name, email string }

func (s *Service) queueEvent(ctx context.Context, tx pgx.Tx, e notifyEvent) (int, error) {
	switch e.Type {
	case "issue.created", "issue.transitioned", "comment.added", "issue.updated":
	default:
		return 0, nil
	}
	if e.Payload.Key == "" {
		return 0, nil
	}
	var id, pid, summary, reporter string
	var assignee *string
	err := tx.QueryRow(ctx, `SELECT id,project_id,summary,reporter_id,assignee_id FROM issue WHERE key=$1 AND deleted_at IS NULL`, e.Payload.Key).Scan(&id, &pid, &summary, &reporter, &assignee)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var ids []string
	var what, body string
	internal := false
	switch e.Type {
	case "issue.created":
		if assignee == nil {
			return 0, nil
		}
		ids = []string{*assignee}
		what = e.Payload.Actor + " created this issue and assigned it to you"
	case "issue.updated":
		assigned := false
		for _, f := range e.Payload.Fields {
			if f == "assignee" {
				assigned = true
			}
		}
		if !assigned || assignee == nil {
			return 0, nil
		}
		ids = []string{*assignee}
		what = e.Payload.Actor + " assigned this issue to you"
	case "issue.transitioned":
		ids = []string{reporter}
		if assignee != nil {
			ids = append(ids, *assignee)
		}
		what = fmt.Sprintf("%s changed the status from %s to %s", e.Payload.Actor, e.Payload.From, e.Payload.To)
	case "comment.added":
		var c string
		if err := tx.QueryRow(ctx, `SELECT body,internal FROM comment WHERE id=$1`, e.Payload.Comment).Scan(&c, &internal); err != nil {
			return 0, nil
		}
		ids = []string{reporter}
		if assignee != nil {
			ids = append(ids, *assignee)
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT author_id FROM comment WHERE issue_id=$1`, id)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var a string
			rows.Scan(&a)
			ids = append(ids, a)
		}
		rows.Close()
		what = e.Payload.Actor + " commented"
		body = c
	}
	seen := map[string]bool{e.Payload.ActorID: true} // people are not notified of their own actions
	n := 0
	for _, uid := range ids {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		var r recipient
		var active bool
		if err := tx.QueryRow(ctx, `SELECT id,name,email,active FROM app_user WHERE id=$1`, uid).Scan(&r.id, &r.name, &r.email, &active); err != nil || !active {
			continue
		}
		if !s.mayReceive(ctx, tx, r.id, pid, internal) { // visibility is decided now and again at send time
			continue
		}
		msg := s.compose(ctx, tx, id, e, e.Payload.Key, summary, what, body, r)
		tag, err := tx.Exec(ctx, `INSERT INTO mail_message(idempotency_key,issue_id,recipient_id,recipient,internal,subject,message_id,in_reply_to,refs,body_text,body_html)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (idempotency_key) DO NOTHING`,
			fmt.Sprintf("ev:%d:%s", e.ID, r.id), id, r.id, (&netmail.Address{Name: r.name, Address: r.email}).String(), internal, msg.Subject, msg.MessageID, msg.InReplyTo, msg.Refs, msg.Text, msg.HTML)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// mayReceive: the recipient must have read access to the project, and internal comments need write access.
func (s *Service) mayReceive(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, userID, projectID string, internal bool) bool {
	var admin bool
	var role string
	q.QueryRow(ctx, `SELECT is_admin FROM app_user WHERE id=$1 AND active`, userID).Scan(&admin)
	q.QueryRow(ctx, `SELECT role FROM project_member WHERE project_id=$1 AND user_id=$2`, projectID, userID).Scan(&role)
	if admin {
		role = "admin"
	}
	if role == "" {
		return false
	}
	return !internal || canWrite(role)
}

// Stable per-issue threading: a virtual root id, a deterministic Message-ID per event, and
// In-Reply-To pointing at the previous message of that issue to the same recipient.
func (s *Service) compose(ctx context.Context, tx pgx.Tx, issueID string, e notifyEvent, key, summary, what, body string, r recipient) mail.Message {
	dom := s.Mail.Domain
	root := fmt.Sprintf("<issue.%s@%s>", issueID, dom)
	var prev string
	tx.QueryRow(ctx, `SELECT message_id FROM mail_message WHERE issue_id=$1 AND recipient_id=$2 ORDER BY id DESC LIMIT 1`, issueID, r.id).Scan(&prev)
	m := mail.Message{From: s.Mail.From, To: (&netmail.Address{Name: r.name, Address: r.email}).String(), Subject: fmt.Sprintf("[%s] %s", key, summary),
		MessageID: fmt.Sprintf("<issue.%s.%d@%s>", issueID, e.ID, dom), InReplyTo: root, Refs: root}
	if prev != "" {
		m.InReplyTo, m.Refs = prev, root+" "+prev
	}
	link := strings.TrimRight(s.Mail.BaseURL, "/") + "/#/issue/" + key
	m.ListUnsubscribe = "<mailto:" + addrOnly(s.Mail.From) + "?subject=unsubscribe>"
	var t strings.Builder
	t.WriteString("-- Reply above this line --\r\n\r\n" + what + "\r\n")
	if body != "" {
		t.WriteString("\r\n" + strings.ReplaceAll(body, "\n", "\r\n") + "\r\n")
	}
	t.WriteString("\r\n" + key + ": " + summary + "\r\n" + link + "\r\n")
	m.Text = t.String()
	// light, dark-safe HTML (inherits the client's colors), no remote images, no tracking
	var h strings.Builder
	h.WriteString(`<div style="font-family:system-ui,sans-serif;font-size:14px"><p style="color:#6b7280;font-size:12px">-- Reply above this line --</p><p>` + html.EscapeString(what) + `</p>`)
	if body != "" {
		h.WriteString(`<blockquote style="margin:0 0 12px;padding:0 0 0 12px;border-left:3px solid #9ca3af">` + strings.ReplaceAll(html.EscapeString(body), "\n", "<br>") + `</blockquote>`)
	}
	h.WriteString(`<p><a href="` + html.EscapeString(link) + `">` + html.EscapeString(key+": "+summary) + `</a></p></div>`)
	m.HTML = h.String()
	return m
}

func addrOnly(from string) string {
	if i := strings.LastIndex(from, "<"); i >= 0 {
		return strings.TrimSuffix(from[i+1:], ">")
	}
	return from
}

// queueAccountMail queues the welcome message for a new account: a sign-in link, never a password.
func (s *Service) queueAccountMail(ctx context.Context, u User) {
	if s.Mail == nil {
		return
	}
	link := strings.TrimRight(s.Mail.BaseURL, "/") + "/"
	text := "-- Reply above this line --\r\n\r\nAn account was created for you on Tasklane.\r\nSign in at " + link + " with this email address.\r\nAsk the person who invited you for your initial password.\r\n"
	htm := `<div style="font-family:system-ui,sans-serif;font-size:14px"><p>An account was created for you on Tasklane.</p><p><a href="` + html.EscapeString(link) + `">Sign in</a> with this email address. Ask the person who invited you for your initial password.</p></div>`
	s.DB.Exec(ctx, `INSERT INTO mail_message(idempotency_key,recipient_id,recipient,subject,message_id,body_text,body_html) VALUES($1,$2,$3,'Your Tasklane account',$4,$5,$6) ON CONFLICT DO NOTHING`,
		"account:"+u.ID, u.ID, (&netmail.Address{Name: u.Name, Address: u.Email}).String(), fmt.Sprintf("<account.%s@%s>", u.ID, s.Mail.Domain), text, htm)
}

// SendMailOnce sends due mail. A row is leased before sending (so two nodes never send the same one), the
// recipient's access is checked again at send time, and failures back off exponentially.
func (s *Service) SendMailOnce(ctx context.Context) (sent int, err error) {
	if s.Mail == nil {
		return 0, nil
	}
	type job struct {
		id                          int64
		issueID, userID             *string
		internal                    bool
		attempts                    int
		to, subject, mid, irt, refs string
		text, html                  string
	}
	now := s.Now()
	rows, err := s.DB.Query(ctx, `UPDATE mail_message SET next_attempt_at=$2 WHERE id IN (
		SELECT id FROM mail_message WHERE status='pending' AND next_attempt_at<=$1 ORDER BY id LIMIT 20 FOR UPDATE SKIP LOCKED)
		RETURNING id,issue_id,recipient_id,internal,attempts,recipient,subject,message_id,in_reply_to,refs,body_text,body_html`, now, now.Add(leaseFor))
	if err != nil {
		return 0, err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.issueID, &j.userID, &j.internal, &j.attempts, &j.to, &j.subject, &j.mid, &j.irt, &j.refs, &j.text, &j.html); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		// permission may have changed between the event and now
		ok := j.issueID == nil // account mail has no issue to check
		if j.issueID != nil && j.userID != nil {
			var pid string
			if s.DB.QueryRow(ctx, `SELECT project_id FROM issue WHERE id=$1 AND deleted_at IS NULL`, *j.issueID).Scan(&pid) == nil {
				ok = s.mayReceive(ctx, s.DB, *j.userID, pid, j.internal)
			}
		}
		if !ok {
			s.DB.Exec(ctx, `UPDATE mail_message SET status='suppressed', last_error='recipient no longer has access' WHERE id=$1`, j.id)
			continue
		}
		serr := s.Mail.Mailer.Send(ctx, mail.Message{From: s.Mail.From, To: j.to, Subject: j.subject, MessageID: j.mid, InReplyTo: j.irt, Refs: j.refs,
			Text: j.text, HTML: j.html, ListUnsubscribe: "<mailto:" + addrOnly(s.Mail.From) + "?subject=unsubscribe>", Date: s.Now()})
		if serr == nil {
			s.DB.Exec(ctx, `UPDATE mail_message SET status='sent', sent_at=$2, attempts=attempts+1, last_error='' WHERE id=$1`, j.id, s.Now())
			sent++
			continue
		}
		att := j.attempts + 1
		var perm *mail.Permanent
		if asPermanent(serr, &perm) || att >= maxAttempts {
			s.DB.Exec(ctx, `UPDATE mail_message SET status='failed', attempts=$2, last_error=$3 WHERE id=$1`, j.id, att, serr.Error())
			continue
		}
		back := time.Duration(math.Min(float64(30*time.Second)*math.Pow(2, float64(j.attempts)), float64(6*time.Hour)))
		s.DB.Exec(ctx, `UPDATE mail_message SET attempts=$2, last_error=$3, next_attempt_at=$4 WHERE id=$1`, j.id, att, serr.Error(), s.Now().Add(back))
	}
	return sent, nil
}

// RunMail is the worker loop: queue new events, send due mail.
func (s *Service) RunMail(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if _, err := s.NotifyOnce(ctx); err != nil && ctx.Err() == nil {
			logf("notify: %v", err)
		}
		if _, err := s.SendMailOnce(ctx); err != nil && ctx.Err() == nil {
			logf("mail: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type MailLogEntry struct {
	ID       int64      `json:"id"`
	To       string     `json:"to"`
	Subject  string     `json:"subject"`
	Status   string     `json:"status"`
	Attempts int        `json:"attempts"`
	Error    string     `json:"error,omitempty"`
	Created  time.Time  `json:"created"`
	Sent     *time.Time `json:"sent,omitempty"`
}

// MailLog is the admin delivery log.
func (s *Service) MailLog(ctx context.Context, a Actor, limit int) ([]MailLogEntry, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT id,recipient,subject,status,attempts,last_error,created_at,sent_at FROM mail_message ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailLogEntry{}
	for rows.Next() {
		var e MailLogEntry
		rows.Scan(&e.ID, &e.To, &e.Subject, &e.Status, &e.Attempts, &e.Error, &e.Created, &e.Sent)
		out = append(out, e)
	}
	return out, rows.Err()
}
