package domain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/mail"
)

// Bounces and complaints (MAIL-06). Every message leaves with its own return path, bounce+TOKEN@domain, where the token names
// the message and is signed with the reply-address keys. A delivery report that arrives there is matched to the message it
// answers and to the address that message went to, so nobody can suppress an address by sending a report about another one.
// A hard bounce suppresses the address at once, soft bounces after three in a week, a complaint at once. Suppression stops
// email to that address and nothing else.

const (
	softBounceLimit  = 3
	softBounceWindow = 7 * 24 * time.Hour
)

func bounceMAC(key []byte, id []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("tasklane bounce v1"))
	m.Write(id)
	return m.Sum(nil)[:8]
}

// returnPath is the VERP address for one queued message.
func (s *Service) returnPath(ctx context.Context, msgID int64) string {
	d := s.mailDomain()
	if d == "" {
		return ""
	}
	ks, err := s.replyKeys(ctx, s.DB)
	if err != nil || len(ks) == 0 {
		return ""
	}
	id := make([]byte, 8)
	binary.BigEndian.PutUint64(id, uint64(msgID))
	tok := append(append([]byte{ks[0].kid}, id...), bounceMAC(ks[0].key, id)...)
	return "bounce+" + base64.RawURLEncoding.EncodeToString(tok) + "@" + d
}

// verifyReturnPath returns the message id a token names, if its signature stands under an unrevoked key.
func (s *Service) verifyReturnPath(ctx context.Context, token string) (int64, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 1+8+8 {
		return 0, false
	}
	ks, err := s.replyKeys(ctx, s.DB)
	if err != nil {
		return 0, false
	}
	for _, k := range ks {
		if k.kid == raw[0] && hmac.Equal(raw[9:], bounceMAC(k.key, raw[1:9])) {
			return int64(binary.BigEndian.Uint64(raw[1:9])), true
		}
	}
	return 0, false
}

func (s *Service) suppressed(ctx context.Context, email string) bool {
	var one int
	return s.DB.QueryRow(ctx, `SELECT 1 FROM mail_suppression WHERE email=$1 AND cleared_at IS NULL`, strings.ToLower(email)).Scan(&one) == nil
}

// recordBounce stores one event and applies the suppression rules. It returns a sentence for the outcome log.
func (s *Service) recordBounce(ctx context.Context, msgID *int64, email, kind, status, source string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	now := s.Now()
	var reason string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO mail_bounce(mail_message_id,email,kind,status,source,at) VALUES($1,$2,$3,$4,$5,$6)`, msgID, email, kind, status, source, now); err != nil {
			return err
		}
		suppress := kind == "hard" || kind == "complaint"
		why := kind + " " + source + " " + status
		if kind == "soft" {
			var n int
			tx.QueryRow(ctx, `SELECT count(*) FROM mail_bounce WHERE email=$1 AND kind='soft' AND at>$2`, email, now.Add(-softBounceWindow)).Scan(&n)
			suppress = n >= softBounceLimit
			why = fmt.Sprintf("%d soft bounces in a week (%s)", n, status)
		}
		if !suppress {
			reason = fmt.Sprintf("soft bounce %s recorded; the address is suppressed after %d in a week", status, softBounceLimit)
			return nil
		}
		// a suppression that was cleared starts again; one already standing keeps its first reason
		if _, err := tx.Exec(ctx, `INSERT INTO mail_suppression(email,kind,reason,since) VALUES($1,$2,$3,$4)
			ON CONFLICT (email) DO UPDATE SET kind=EXCLUDED.kind, reason=EXCLUDED.reason, since=EXCLUDED.since, cleared_at=NULL, cleared_by=NULL WHERE mail_suppression.cleared_at IS NOT NULL`, email, kind, why, now); err != nil {
			return err
		}
		reason = kind + ": email to the address is suppressed (" + status + ")"
		return nil
	})
	return reason, err
}

var reBounceAddr = regexp.MustCompile(`(?i)bounce\+([A-Za-z0-9_-]{20,40})@`)

// classifyBounce recognises a message that is a bounce or a complaint (it came to a return path, or it is a report) and
// handles it. The second result says whether it was one; a message that is not one goes on to ordinary classification.
func (s *Service) classifyBounce(ctx context.Context, p *mail.Parsed, raw []byte) (verdict, bool) {
	h := p.Header
	var tok string
	dom := strings.ToLower(s.mailDomain())
	for _, f := range []string{"To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To"} {
		for _, v := range h[textproto.CanonicalMIMEHeaderKey(f)] {
			if m := reBounceAddr.FindStringSubmatch(v); m != nil && strings.Contains(strings.ToLower(v), "@"+dom) {
				tok = m[1]
			}
		}
	}
	rep := mail.ParseReport(raw)
	isReport := rep.Kind != "" || strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "multipart/report")
	if tok == "" && !isReport {
		return verdict{}, false
	}
	if tok == "" {
		return ignore("a delivery report that did not come to a return path of ours: not acted on"), true
	}
	msgID, ok := s.verifyReturnPath(ctx, tok)
	if !ok {
		return ignore("the return-path token is not valid (altered, or signed by a revoked key): not acted on"), true
	}
	var rcpt string
	if err := s.DB.QueryRow(ctx, `SELECT recipient FROM mail_message WHERE id=$1`, msgID).Scan(&rcpt); err != nil {
		return ignore("the return-path token names no message of ours"), true
	}
	to := strings.ToLower(addrOnly(rcpt))
	if rep.Kind == "" {
		return ignore("mail to a return path that is not a delivery report"), true
	}
	if rep.Recipient != "" && rep.Recipient != to {
		return ignore("the report is about " + rep.Recipient + " but the message went to a different address: not acted on"), true
	}
	var kind string
	switch {
	case rep.Kind == "feedback":
		kind = "complaint"
	case rep.Action != "failed": // delayed, delivered, relayed, expanded: nothing has gone wrong yet, whatever the status code says
		return ignore("delivery report: " + rep.Action + " " + rep.Status + " (no change)"), true
	case strings.HasPrefix(rep.Status, "5"):
		kind = "hard"
	default:
		kind = "soft"
	}
	src := "dsn"
	if rep.Kind == "feedback" {
		src = "feedback"
	}
	out, err := s.recordBounce(ctx, &msgID, to, kind, rep.Status+rep.FeedType, src)
	if err != nil {
		return verdict{outcome: "rejected", reason: "the bounce could not be recorded: " + err.Error()}, true
	}
	return ignore("bounce handled: " + out), true
}

// ---------- provider events ----------

// ReceiveBounceEvents is the webhook for providers that report bounces and complaints themselves: a JSON body
// {"events":[{"type":"bounce|soft|complaint","email":"...","status":"..."}]} signed like an inbound message (the source's secret,
// X-Tasklane-Signature). An event only suppresses an address that we have sent mail to.
func (s *Service) ReceiveBounceEvents(ctx context.Context, sourceID, signature string, body []byte) (int, error) {
	var sealed []byte
	var enabled bool
	if err := s.DB.QueryRow(ctx, `SELECT secret,enabled FROM mail_source WHERE id::text=$1`, sourceID).Scan(&sealed, &enabled); err != nil || !enabled {
		return 0, ErrForbidden
	}
	secret, err := s.unseal(ctx, sealed)
	if err != nil || !hmac.Equal([]byte(signature), []byte(SignInbound(secret, body))) {
		return 0, ErrForbidden
	}
	var in struct {
		Events []struct {
			Type, Email, Status string
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &in); err != nil || len(in.Events) == 0 || len(in.Events) > 1000 {
		return 0, invalid("the body is not a list of 1 to 1000 events")
	}
	n := 0
	for _, e := range in.Events {
		kind := map[string]string{"bounce": "hard", "hard": "hard", "soft": "soft", "complaint": "complaint", "spam": "complaint"}[strings.ToLower(e.Type)]
		em := strings.ToLower(strings.TrimSpace(e.Email))
		if kind == "" || !strings.Contains(em, "@") {
			continue
		}
		var sent int
		if s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_message WHERE lower(recipient) LIKE '%'||$1||'%' AND status='sent'`, em).Scan(&sent); sent == 0 {
			continue // an address we never mailed cannot be suppressed by an event
		}
		if _, err := s.recordBounce(ctx, nil, em, kind, e.Status, "provider"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------- administration ----------

type Suppression struct {
	Email   string    `json:"email"`
	Kind    string    `json:"kind"`
	Reason  string    `json:"reason"`
	Since   time.Time `json:"since"`
	Cleared bool      `json:"cleared"`
	User    string    `json:"user,omitempty"` // the person this address belongs to
}

func (s *Service) ListSuppressions(ctx context.Context, a Actor) ([]Suppression, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT m.email,m.kind,m.reason,m.since,m.cleared_at IS NOT NULL,COALESCE(u.name,'') FROM mail_suppression m LEFT JOIN app_user u ON lower(u.email)=m.email ORDER BY m.since DESC LIMIT 500`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suppression{}
	for rows.Next() {
		var x Suppression
		if err := rows.Scan(&x.Email, &x.Kind, &x.Reason, &x.Since, &x.Cleared, &x.User); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ClearSuppression lets mail go to an address again (the soft-bounce count starts over). Audited.
func (s *Service) ClearSuppression(ctx context.Context, a Actor, email string) error {
	if !a.Admin {
		return ErrForbidden
	}
	email = strings.ToLower(strings.TrimSpace(email))
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE mail_suppression SET cleared_at=$2,cleared_by=$3 WHERE email=$1 AND cleared_at IS NULL`, email, s.Now(), a.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM mail_bounce WHERE email=$1 AND kind='soft'`, email); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "mail.suppression.clear", email, nil)
	})
}

// EmailBouncing says whether mail to this user's address is being held back, for the indicator on their own account.
func (s *Service) EmailBouncing(ctx context.Context, userID string) bool {
	var email string
	if s.DB.QueryRow(ctx, `SELECT email FROM app_user WHERE id=$1`, userID).Scan(&email) != nil {
		return false
	}
	return s.suppressed(ctx, email)
}
