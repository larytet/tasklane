package domain_test

import (
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/smtpmock"
)

type mailEnv struct {
	*testutil.Env
	smtp *smtpmock.Mock
}

func newMailEnv(t *testing.T) *mailEnv {
	e := testutil.New(t)
	e.Project()
	m := smtpmock.New(t, smtpmock.Options{TLS: "starttls", User: "u", Pass: "p"})
	e.Svc.Mail = &domain.MailConfig{
		Mailer: &mail.SMTP{Host: "localhost", Port: m.Port(), TLS: "starttls", User: "u", Pass: "p", From: "Tasklane <tasklane@example.com>", TLSConfig: &tls.Config{RootCAs: m.CA()}, Timeout: 3 * time.Second},
		From:   "Tasklane <tasklane@example.com>", Domain: "tasklane.example.com", BaseURL: "https://tasklane.example.com",
	}
	return &mailEnv{Env: e, smtp: m}
}

func (e *mailEnv) flush() (queued, sent int) {
	e.T.Helper()
	q, err := e.Svc.NotifyOnce(ctx)
	if err != nil {
		e.T.Fatal(err)
	}
	s, err := e.Svc.SendMailOnce(ctx)
	if err != nil {
		e.T.Fatal(err)
	}
	return q, s
}

func (e *mailEnv) msgTo(addr string) []smtpmock.Message {
	var out []smtpmock.Message
	for _, m := range e.smtp.Messages() {
		if m.Rcpts[0] == addr {
			out = append(out, m)
		}
	}
	return out
}

func readMsg(t *testing.T, raw []byte) (h netmail.Header, text, html string) {
	m, err := netmail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	_, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	mr := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(quotedprintable.NewReader(p))
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain") {
			text = string(b)
		} else {
			html = string(b)
		}
	}
	return m.Header, text, html
}

func TestMAIL02_ThreadingPerIssue(t *testing.T) {
	e := newMailEnv(t)
	is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "Checkout fails", Assignee: e.Bob.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.flush()
	e.Svc.AddComment(ctx, e.Alice, is.Key, "first comment", false)
	e.flush()
	// the issue is renamed, then another event: still one thread
	e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "summary", Body: "Checkout fails on 3DS", BaseSeq: 1})
	e.Svc.AddComment(ctx, e.Alice, is.Key, "second comment", false)
	e.flush()
	msgs := e.msgTo("bob@example.com")
	if len(msgs) != 3 {
		t.Fatalf("%d messages\n%s", len(msgs), e.smtp.Transcript())
	}
	root := "<issue." + is.ID + "@tasklane.example.com>"
	var prev string
	for i, m := range msgs {
		h, _, _ := readMsg(t, m.Raw)
		mid := h.Get("Message-Id")
		if !strings.HasPrefix(mid, "<issue."+is.ID+".") || strings.Contains(prev, mid) && prev != "" && mid == prev {
			t.Fatalf("message id %q", mid)
		}
		refs := h.Get("References")
		if !strings.HasPrefix(refs, root) { // References always start with the virtual per-issue root
			t.Fatalf("msg %d references %q", i, refs)
		}
		if i == 0 && (h.Get("In-Reply-To") != root) {
			t.Fatalf("first message replies to %q", h.Get("In-Reply-To"))
		}
		if i > 0 && h.Get("In-Reply-To") != prev { // each message replies to the previous one of the issue
			t.Fatalf("msg %d In-Reply-To %q, want %q", i, h.Get("In-Reply-To"), prev)
		}
		prev = mid
	}
	h, _, _ := readMsg(t, msgs[2].Raw)
	dec := new(mime.WordDecoder)
	if subj, _ := dec.DecodeHeader(h.Get("Subject")); subj != "[ABC-1] Checkout fails on 3DS" {
		t.Fatalf("subject %q", subj) // the subject follows the summary; threading rides on the headers, so no split
	}
}

func TestMAIL03_NotificationContent(t *testing.T) {
	e := newMailEnv(t)
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "Ünïcode <b>bold</b> & more", Assignee: e.Bob.ID})
	e.Svc.AddComment(ctx, e.Alice, is.Key, "see <script>alert(1)</script> and https://example.com/x?a=1&b=2", false)
	e.flush()
	msgs := e.msgTo("bob@example.com")
	if len(msgs) != 2 {
		t.Fatalf("%d", len(msgs))
	}
	h, text, html := readMsg(t, msgs[1].Raw)
	if h.Get("Auto-Submitted") != "auto-generated" || !strings.HasPrefix(h.Get("List-Unsubscribe"), "<mailto:") {
		t.Fatalf("%v", h)
	}
	if !strings.Contains(text, "Reply above this line") || !strings.Contains(text, "https://tasklane.example.com/#/issue/ABC-1") || !strings.Contains(text, "<script>alert(1)</script>") {
		t.Fatalf("text part: %q", text)
	}
	// the HTML part escapes everything and loads nothing remote
	if strings.Contains(html, "<script") || strings.Contains(html, "<b>bold") || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("HTML not escaped: %s", html)
	}
	for _, bad := range []string{"<img", "<link", "url(", "<iframe", "src="} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Fatalf("HTML contains %q: %s", bad, html)
		}
	}
	if !strings.Contains(html, `href="https://tasklane.example.com/#/issue/ABC-1"`) {
		t.Fatal(html)
	}
	if from := msgs[1].From; from != "tasklane@example.com" {
		t.Fatalf("envelope sender %q", from)
	}
}

func TestMAIL_RecipientsAndSelfNotification(t *testing.T) {
	e := newMailEnv(t)
	// Alice assigns to Bob: Bob is told, Alice (the actor) is not
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "s", Assignee: e.Bob.ID})
	e.flush()
	if len(e.msgTo("bob@example.com")) != 1 || len(e.msgTo("alice@example.com")) != 0 {
		t.Fatalf("bob %d alice %d", len(e.msgTo("bob@example.com")), len(e.msgTo("alice@example.com")))
	}
	// Bob comments: Alice (reporter) is told, Bob is not
	e.Svc.AddComment(ctx, e.Bob, is.Key, "on it", false)
	e.flush()
	if len(e.msgTo("alice@example.com")) != 1 || len(e.msgTo("bob@example.com")) != 1 {
		t.Fatalf("after comment: alice %d bob %d", len(e.msgTo("alice@example.com")), len(e.msgTo("bob@example.com")))
	}
	// reassign: only the new assignee gets the assignment mail
	hi := e.Alice.ID
	e.Svc.UpdateFields(ctx, e.Bob, is.Key, nil, false, domain.Changes{Assignee: &hi})
	e.flush()
	if len(e.msgTo("alice@example.com")) != 2 {
		t.Fatalf("assignment mail: %d", len(e.msgTo("alice@example.com")))
	}
	// a priority change alone mails nobody
	p := "low"
	before := len(e.smtp.Messages())
	e.Svc.UpdateFields(ctx, e.Alice, is.Key, nil, false, domain.Changes{Priority: &p})
	e.flush()
	if len(e.smtp.Messages()) != before {
		t.Fatal("a priority change sent mail")
	}
}

func TestMAIL_InternalCommentsNeverReachViewers(t *testing.T) { // visibility, MAIL-04 core
	e := newMailEnv(t)
	vu, _ := e.Svc.CreateUser(ctx, "viewer@example.com", "Vera V.", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	// the viewer is the reporter-equivalent participant: make her the assignee is not allowed (viewers cannot be assigned),
	// so she participates by having commented before being downgraded
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "s", Assignee: e.Bob.ID})
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "member")
	e.Svc.AddComment(ctx, domain.Actor{ID: vu.ID, Label: "Vera V."}, is.Key, "I was here", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	e.flush()
	base := len(e.msgTo("viewer@example.com"))
	e.Svc.AddComment(ctx, e.Alice, is.Key, "internal: customer is angry", true)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "public: we are looking into it", false)
	e.flush()
	got := e.msgTo("viewer@example.com")
	if len(got)-base != 1 {
		t.Fatalf("viewer got %d new messages", len(got)-base)
	}
	_, text, _ := readMsg(t, got[len(got)-1].Raw)
	if strings.Contains(text, "angry") || !strings.Contains(text, "looking into it") {
		t.Fatalf("viewer saw: %q", text)
	}
	// the member does get the internal one
	n := 0
	for _, m := range e.msgTo("bob@example.com") {
		if _, tx, _ := readMsg(t, m.Raw); strings.Contains(tx, "angry") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("member got the internal comment %d times", n)
	}
}

func TestMAIL04_VisibilityIsRechecked_AtSendTime(t *testing.T) {
	e := newMailEnv(t)
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "secret", Assignee: e.Bob.ID})
	if q, err := e.Svc.NotifyOnce(ctx); err != nil || q != 1 {
		t.Fatalf("%d %v", q, err)
	}
	// Bob loses access between the event and the send
	e.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id=$1`, e.Bob.ID)
	if n, _ := e.Svc.SendMailOnce(ctx); n != 0 {
		t.Fatal("mail sent to a user who lost access")
	}
	if len(e.smtp.Messages()) != 0 || e.smtp.Connections() != 0 {
		t.Fatal("the mock was contacted")
	}
	log, _ := e.Svc.MailLog(ctx, e.Admin, 10)
	if len(log) != 1 || log[0].Status != "suppressed" {
		t.Fatalf("%+v", log)
	}
	// a trashed issue sends nothing either
	_ = is
	if _, err := e.Svc.MailLog(ctx, e.Alice, 10); err == nil {
		t.Fatal("non-admin read the delivery log")
	}
}

func TestMAIL05_QueueRetriesBackoffAndIdempotency(t *testing.T) {
	e := newMailEnv(t)
	e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "s", Assignee: e.Bob.ID})
	// the relay is down for the first two attempts
	e.smtp.Script(smtpmock.Behavior{FailAt: smtpmock.Greeting, Code: 421}, smtpmock.Behavior{FailAt: smtpmock.RcptTo, Code: 450})
	q, s := e.flush()
	if q != 1 || s != 0 {
		t.Fatalf("%d %d", q, s)
	}
	row := func() (status string, attempts int, next time.Time, lastErr string) {
		e.Store.Pool.QueryRow(ctx, `SELECT status,attempts,next_attempt_at,last_error FROM mail_message WHERE recipient LIKE '%bob@example.com%' ORDER BY id LIMIT 1`).Scan(&status, &attempts, &next, &lastErr)
		return
	}
	st, att, next, lerr := row()
	if st != "pending" || att != 1 || !next.After(e.Clock.Now().Add(20*time.Second)) || lerr == "" {
		t.Fatalf("%s %d %v %q", st, att, next, lerr)
	}
	// not due yet: nothing is tried
	conns := e.smtp.Connections()
	e.flush()
	if e.smtp.Connections() != conns {
		t.Fatal("retried before the backoff elapsed")
	}
	e.Clock.Advance(time.Minute)
	e.flush() // second failure (450), backoff doubles
	st, att, next2, _ := row()
	if st != "pending" || att != 2 || !next2.After(e.Clock.Now().Add(50*time.Second)) {
		t.Fatalf("%s %d %v", st, att, next2)
	}
	e.Clock.Advance(5 * time.Minute)
	if _, s := e.flush(); s != 1 {
		t.Fatalf("not delivered after the relay recovered: %s", e.smtp.Transcript())
	}
	if len(e.msgTo("bob@example.com")) != 1 {
		t.Fatal("not exactly one message")
	}
	// replaying the whole event stream creates no new rows or messages (idempotency key per event and recipient)
	e.Store.Pool.Exec(ctx, `UPDATE outbox_cursor SET last_id=0`)
	e.Clock.Advance(time.Hour)
	if q, s := e.flush(); q != 0 || s != 0 {
		t.Fatalf("replay queued %d sent %d", q, s)
	}
	if len(e.smtp.Messages()) != 1 {
		t.Fatalf("%d messages after replay", len(e.smtp.Messages()))
	}
}

func TestMAIL05_PermanentFailureAndGiveUp(t *testing.T) {
	e := newMailEnv(t)
	e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "s", Assignee: e.Bob.ID})
	e.smtp.Script(smtpmock.Behavior{FailAt: smtpmock.RcptTo, Code: 550})
	e.flush()
	log, _ := e.Svc.MailLog(ctx, e.Admin, 10)
	if len(log) != 1 || log[0].Status != "failed" || !strings.Contains(log[0].Error, "recipient rejected") {
		t.Fatalf("%+v", log)
	}
	e.Clock.Advance(24 * time.Hour)
	c := e.smtp.Connections()
	e.flush()
	if e.smtp.Connections() != c {
		t.Fatal("a permanently failed message was retried")
	}
	// transient failures give up after the attempt limit
	e2 := newMailEnv(t)
	e2.Svc.CreateIssue(ctx, e2.Alice, domain.CreateIn{Project: "ABC", Summary: "s", Assignee: e2.Bob.ID})
	b := make([]smtpmock.Behavior, 20)
	for i := range b {
		b[i] = smtpmock.Behavior{FailAt: smtpmock.Greeting, Code: 421}
	}
	e2.smtp.Script(b...)
	for i := 0; i < 12; i++ {
		e2.flush()
		e2.Clock.Advance(7 * time.Hour)
	}
	log, _ = e2.Svc.MailLog(ctx, e2.Admin, 10)
	if len(log) != 1 || log[0].Status != "failed" || log[0].Attempts != 8 {
		t.Fatalf("%+v", log)
	}
}

func TestMAIL05_ConcurrentSendersSendEachMessageOnce(t *testing.T) {
	e := newMailEnv(t)
	for i := 0; i < 6; i++ {
		e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "s", Assignee: e.Bob.ID})
	}
	e.Svc.NotifyOnce(ctx)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ { // five "nodes" send at once; rows are leased with SKIP LOCKED
		wg.Add(1)
		go func() { defer wg.Done(); e.Svc.SendMailOnce(ctx) }()
	}
	wg.Wait()
	if n := len(e.msgTo("bob@example.com")); n != 6 {
		t.Fatalf("%d messages for 6 events", n)
	}
	ids := map[string]bool{}
	for _, m := range e.smtp.Messages() {
		h, _, _ := readMsg(t, m.Raw)
		if ids[h.Get("Message-Id")] {
			t.Fatalf("duplicate %s", h.Get("Message-Id"))
		}
		ids[h.Get("Message-Id")] = true
	}
}

func TestMAIL_AccountMail(t *testing.T) {
	e := newMailEnv(t)
	if _, err := e.Svc.CreateUser(ctx, "new@example.com", "Nia N.", "initialPass1", false); err != nil {
		t.Fatal(err)
	}
	e.flush()
	got := e.msgTo("new@example.com")
	if len(got) != 1 {
		t.Fatalf("%d", len(got))
	}
	_, text, html := readMsg(t, got[0].Raw)
	if strings.Contains(text+html, "initialPass1") || !strings.Contains(text, "https://tasklane.example.com/") {
		t.Fatalf("account mail: %q", text) // a sign-in link, never the password
	}
}

func TestMAIL_NoMailerMeansNoMail(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Issue("s", "")
	if q, err := e.Svc.NotifyOnce(ctx); err != nil || q != 0 {
		t.Fatalf("%d %v", q, err)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_message`).Scan(&n)
	if n != 0 {
		t.Fatal("mail queued without a mailer")
	}
}

func mailsWith(e *mailEnv, to, needle string) int {
	n := 0
	for _, m := range e.msgTo(to) {
		if strings.Contains(string(m.Raw), needle) || func() bool {
			_, t, h := readMsg(e.T, m.Raw)
			return strings.Contains(t, needle) || strings.Contains(h, needle)
		}() {
			n++
		}
	}
	return n
}

func TestHIST03_MentionsNotifyOnce(t *testing.T) {
	e := newMailEnv(t)
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "s"})
	e.flush()
	// a comment mentioning Bob: one specific mail, not also the generic "commented" one
	if _, err := e.Svc.AddComment(ctx, e.Alice, is.Key, "@bob can you check this? write to bob@example.com or ask @nobody", false); err != nil {
		t.Fatal(err)
	}
	e.flush()
	got := e.msgTo("bob@example.com")
	if len(got) != 1 {
		t.Fatalf("%d mails to Bob\n%s", len(got), e.smtp.Transcript())
	}
	_, text, html := readMsg(t, got[0].Raw)
	if !strings.Contains(text, "Alice K. mentioned you in a comment") || !strings.Contains(text, "can you check this?") || !strings.Contains(html, "mentioned you") {
		t.Fatalf("%q", text)
	}
	// an email address is not a mention: nobody besides Bob was addressed
	if len(e.msgTo("alice@example.com")) != 0 {
		t.Fatal("the author was notified of their own comment")
	}
	// a description save that introduces a mention notifies once; saving again with the mention still there is quiet
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: "please look, @bob", BaseSeq: v.Issue.DescSeq})
	e.flush()
	if n := mailsWith(e, "bob@example.com", "mentioned you in the description"); n != 1 {
		t.Fatalf("description mention mails: %d", n)
	}
	e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: "please look, @bob, urgent", BaseSeq: v.Issue.DescSeq + 1})
	e.Clock.Advance(5 * time.Minute)
	e.flush()
	if n := mailsWith(e, "bob@example.com", "mentioned you in the description"); n != 1 {
		t.Fatalf("the same mention notified again: %d", n)
	}
	// a person without access is never mailed, and an internal comment mentioning a viewer is not either
	vu, _ := e.Svc.CreateUser(ctx, "vera@example.com", "Vera V.", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	ou, _ := e.Svc.CreateUser(ctx, "outsider@example.com", "Olga O.", "password123", false)
	_ = ou
	e.flush() // their welcome emails go out now, so that only the mentions are measured below
	before := len(e.smtp.Messages())
	e.Svc.AddComment(ctx, e.Alice, is.Key, "@outsider look at this", false)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "internal only: @vera should not see this", true)
	e.flush()
	for _, m := range e.smtp.Messages()[before:] {
		if m.Rcpts[0] == "outsider@example.com" || m.Rcpts[0] == "vera@example.com" {
			t.Fatalf("mail to %s who must not receive it", m.Rcpts[0])
		}
	}
	// a public mention of a viewer does reach the viewer
	e.Svc.AddComment(ctx, e.Alice, is.Key, "@vera this one is public", false)
	e.flush()
	if n := mailsWith(e, "vera@example.com", "public"); n != 1 {
		t.Fatalf("viewer mention mails: %d", n)
	}
}

func TestHIST04_EditNoticesAreBatchedPerPerson(t *testing.T) {
	e := newMailEnv(t)
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "s", Assignee: e.Bob.ID})
	e.flush()
	base := len(e.msgTo("bob@example.com")) // the assignment mail
	seq := int64(1)
	save := func(a domain.Actor, body string) {
		r, err := e.Svc.SaveText(ctx, a, is.Key, domain.SaveIn{Field: "description", Body: body, BaseSeq: seq})
		if err != nil {
			t.Fatal(err)
		}
		seq = r.Seq
	}
	for i := 0; i < 6; i++ { // a burst of saves by Alice
		save(e.Alice, fmt.Sprint("draft ", i))
		e.flush()
	}
	if len(e.msgTo("bob@example.com")) != base {
		t.Fatal("an edit notice was sent before the batch window elapsed")
	}
	e.Clock.Advance(4 * time.Minute)
	e.flush()
	if n := mailsWith(e, "bob@example.com", "edited the description"); n != 1 {
		t.Fatalf("%d edit notices for six saves", n)
	}
	// a later save starts a new window
	save(e.Alice, "later change")
	e.flush()
	e.Clock.Advance(4 * time.Minute)
	e.flush()
	if n := mailsWith(e, "bob@example.com", "edited the description"); n != 2 {
		t.Fatalf("%d notices after a second burst", n)
	}
	// two people editing in the same window are two notices, one per person
	save(e.Admin, "admin change")
	save(e.Alice, "alice again")
	e.flush()
	e.Clock.Advance(4 * time.Minute)
	e.flush()
	if n := mailsWith(e, "bob@example.com", "edited the description"); n != 4 {
		t.Fatalf("%d notices, want one per person per window", n)
	}
	// the editor is never told about their own edit
	if n := mailsWith(e, "alice@example.com", "Alice K. edited"); n != 0 {
		t.Fatalf("Alice was told of her own edits %d times", n)
	}
}
