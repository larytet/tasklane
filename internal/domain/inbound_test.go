package domain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

type inboundEnv struct {
	*mailEnv
	src     domain.MailSource
	trusted domain.MailSource
	issue   domain.Issue
	n       int
}

const authOK = "Authentication-Results: mx.example.net; dkim=pass header.d=example.com; spf=pass"

func newInbound(t *testing.T) *inboundEnv {
	e := &inboundEnv{mailEnv: newMailEnv(t)}
	e.WithBlobs()
	var err error
	if e.trusted, err = e.Svc.CreateMailSource(ctx, e.Admin, "relay", true); err != nil {
		t.Fatal(err)
	}
	if e.src, err = e.Svc.CreateMailSource(ctx, e.Admin, "plain", false); err != nil {
		t.Fatal(err)
	}
	e.issue, _ = e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "Printer is on fire", Assignee: e.Bob.ID})
	e.flush()
	return e
}

// replyAddr is the Reply-To of the notification Bob received.
func (e *inboundEnv) replyAddr(to string) string {
	e.T.Helper()
	msgs := e.msgTo(to)
	if len(msgs) == 0 {
		e.T.Fatalf("no mail to %s", to)
	}
	h, _, _ := readMsg(e.T, msgs[len(msgs)-1].Raw)
	a := h.Get("Reply-To")
	if !strings.HasPrefix(a, "reply+") || !strings.HasSuffix(a, "@tasklane.example.com") {
		e.T.Fatalf("Reply-To %q", a)
	}
	return a
}

func (e *inboundEnv) eml(from, to, subject string, extra []string, body string) []byte {
	e.n++
	h := []string{"From: " + from, "To: " + to, "Subject: " + subject, fmt.Sprintf("Message-ID: <m%d@mail.example.com>", e.n), "Date: Tue, 3 Mar 2026 10:00:00 +0000", "MIME-Version: 1.0", "Content-Type: text/plain; charset=utf-8"}
	h = append(h, extra...)
	return []byte(strings.Join(h, "\r\n") + "\r\n\r\n" + body)
}

func (e *inboundEnv) ingest(trusted bool, raw []byte) domain.InboundResult {
	e.T.Helper()
	src := e.src
	if trusted {
		src = e.trusted
	}
	r, err := e.Svc.IngestMail(ctx, src.ID, trusted, raw)
	if err != nil {
		e.T.Fatal(err)
	}
	return r
}

func (e *inboundEnv) comments() []domain.Comment {
	v, err := e.Svc.View(ctx, e.Alice, e.issue.Key)
	if err != nil {
		e.T.Fatal(err)
	}
	return v.Comments
}

const quoted = "\r\n\r\nOn Tue, 3 Mar 2026 at 09:00, Tasklane <tasklane@example.com> wrote:\r\n> -- Reply above this line --\r\n> Alice K. assigned you\r\n"

func TestMAIL07_ReplyByEmailCommentsAsTheSenderAndNothingElseDoes(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	raw := e.eml("Bob R. <bob@example.com>", addr, "Re: [ABC-1] Printer is on fire", []string{authOK}, "Thanks, I will look today."+quoted)
	r := e.ingest(true, raw)
	if r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	cs := e.comments()
	if len(cs) != 1 || cs[0].Author.ID != e.Bob.ID || cs[0].Body != "Thanks, I will look today." || !cs[0].ViaEmail {
		t.Fatalf("%+v", cs)
	}
	// the original message is kept and an administrator can fetch it
	got, err := e.Svc.InboundRaw(ctx, e.Admin, r.ID)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("raw not kept: %v", err)
	}
	if _, err := e.Svc.InboundRaw(ctx, e.Alice, r.ID); err == nil {
		t.Fatal("a non-administrator read the raw message")
	}
	// delivered again (the relay retries): counted, not processed again
	if d := e.ingest(true, raw); !d.Duplicate || len(e.comments()) != 1 {
		t.Fatalf("%+v", d)
	}
	var dupes int
	e.Store.Pool.QueryRow(ctx, `SELECT dupes FROM mail_inbound WHERE id=$1`, r.ID).Scan(&dupes)
	if dupes != 1 {
		t.Fatalf("dupes %d", dupes)
	}
}

func TestMAIL07_ForwardedForgedExpiredAndRevokedAddressesNeverComment(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	send := func(from, to string) domain.InboundResult {
		return e.ingest(true, e.eml(from, to, "Re: x", []string{authOK}, "a comment"))
	}
	// Alice got a forward of Bob's notification and replies to the address in it
	if r := send("alice@example.com", addr); r.Outcome != "quarantined" || !strings.Contains(r.Reason, "not issued to this sender") {
		t.Fatalf("%+v", r)
	}
	// a forged address: one character changed
	i := strings.Index(addr, "+") + 5
	forged := addr[:i] + map[bool]string{true: "B", false: "A"}[addr[i] == 'A'] + addr[i+1:]
	if r := send("bob@example.com", forged); r.Outcome != "quarantined" {
		t.Fatalf("%+v", r)
	}
	// garbage in the token position
	if r := send("bob@example.com", "reply+short@tasklane.example.com"); r.Outcome == "accepted" {
		t.Fatalf("%+v", r)
	}
	if len(e.comments()) != 0 {
		t.Fatal("a comment was made")
	}
	// the forwarded one cannot be released (it acts on nothing); an administrator can delete it, and that is audited
	q, _ := e.Svc.ListInbound(ctx, e.Admin, "quarantined", 10)
	if len(q) < 2 {
		t.Fatalf("%+v", q)
	}
	var fwd int64
	for _, r := range q {
		if strings.Contains(r.Reason, "not issued") {
			fwd = r.ID
		}
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, fwd); err == nil {
		t.Fatal("released a message that names no issue")
	}
	if err := e.Svc.DeleteInbound(ctx, e.Admin, fwd); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.DeleteInbound(ctx, e.Alice, fwd); err == nil {
		t.Fatal("a non-administrator deleted")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='mail.inbound.delete'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit rows %d", n)
	}
	// expiry: thirty days pass
	e.Clock.Advance(31 * 24 * time.Hour)
	if r := send("bob@example.com", addr); r.Outcome != "quarantined" || !strings.Contains(r.Reason, "expired") {
		t.Fatalf("%+v", r)
	}
	e.Clock.Advance(-31 * 24 * time.Hour)
	// key rotation: the old address still works, then the old key is revoked and it does not
	if _, err := e.Svc.RotateReplyKey(ctx, e.Admin); err != nil {
		t.Fatal(err)
	}
	if r := send("bob@example.com", addr); r.Outcome != "accepted" {
		t.Fatalf("an address signed before the rotation stopped working: %+v", r)
	}
	if err := e.Svc.RevokeReplyKey(ctx, e.Admin, 2); err == nil {
		t.Fatal("the newest key was revoked")
	}
	if err := e.Svc.RevokeReplyKey(ctx, e.Admin, 1); err != nil {
		t.Fatal(err)
	}
	if r := send("bob@example.com", addr); r.Outcome != "quarantined" || !strings.Contains(r.Reason, "revoked") {
		t.Fatalf("%+v", r)
	}
	// new notifications carry the new key
	e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "another", Assignee: e.Bob.ID})
	e.flush()
	if a2 := e.replyAddr("bob@example.com"); a2 == addr {
		t.Fatal("the address did not change")
	} else if r := send("bob@example.com", a2); r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
}

func TestMAIL10_NoProofOfOriginNoAction(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	cases := []struct {
		name    string
		trusted bool
		from    string
		hdrs    []string
	}{
		{"untrusted path claiming a pass", false, "bob@example.com", []string{authOK}},
		{"no verdict at all", true, "bob@example.com", nil},
		{"dkim failed", true, "bob@example.com", []string{"Authentication-Results: mx; dkim=fail header.d=example.com"}},
		{"signing domain is a lookalike", true, "bob@example.com", []string{"Authentication-Results: mx; dkim=pass header.d=example.com.evil.net"}},
		{"signing domain is a different domain", true, "bob@example.com", []string{"Authentication-Results: mx; dkim=pass header.d=notexample.com"}},
		{"signing domain is a subdomain of the sender's", true, "bob@example.com", []string{"Authentication-Results: mx; dkim=pass header.d=mail.example.com"}},
	}
	for _, c := range cases {
		r := e.ingest(c.trusted, e.eml(c.from, addr, "Re: x", c.hdrs, "from an attacker"))
		if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "proof of origin") {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
	// a display name that looks like a user does not make the address one
	r := e.ingest(true, e.eml(`"bob@example.com" <attacker@evil.example>`, addr, "Re: x", []string{authOK}, "hi"))
	if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "not a known user") {
		t.Fatalf("%+v", r)
	}
	// two From addresses are refused outright
	if r := e.ingest(true, e.eml("bob@example.com, alice@example.com", addr, "Re: x", []string{authOK}, "hi")); r.Outcome != "rejected" {
		t.Fatalf("%+v", r)
	}
	if len(e.comments()) != 0 {
		t.Fatal("something was posted")
	}
	// nothing went out to any of these senders (no backscatter)
	if n := len(e.msgTo("attacker@evil.example")); n != 0 {
		t.Fatalf("%d messages to an unauthenticated sender", n)
	}
	// the legitimate sender with a proven origin on a subdomain-signed message: the signing domain may be a parent
	r = e.ingest(true, e.eml("bob@example.com", addr, "Re: x", []string{"Authentication-Results: mx; dkim=pass header.d=example.com"}, "proven"))
	if r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	// an administrator can release a message that was only unproven, once, with the sender's permissions
	q, _ := e.Svc.ListInbound(ctx, e.Admin, "quarantined", 50)
	var id int64
	for _, row := range q {
		if strings.Contains(row.Reason, "proof of origin") {
			id = row.ID
			break
		}
	}
	c, err := e.Svc.ReleaseInbound(ctx, e.Admin, id)
	if err != nil || c.Author.ID != e.Bob.ID {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, id); err == nil {
		t.Fatal("released twice")
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Alice, id); err == nil {
		t.Fatal("a non-administrator released")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE mail_inbound_id=$1`, id).Scan(&n)
	if n != 1 {
		t.Fatalf("%d comments for one release", n)
	}
}

func TestMAIL11_AutomaticMailAndLoopsAreIgnored(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	cases := map[string][]string{
		"Auto-Submitted":       {"Auto-Submitted: auto-replied"},
		"Precedence bulk":      {"Precedence: bulk"},
		"Precedence list":      {"Precedence: list"},
		"Suppress header":      {"X-Auto-Response-Suppress: All"},
		"mailing list":         {"List-Id: <dev.example.com>"},
		"null return path":     {"Return-Path: <>"},
		"our loop counter":     {"X-Tasklane-Loop: 1"},
		"another installation": {"X-Tasklane-Instance: other.example.org"},
		"x-autoreply":          {"X-Autoreply: yes"},
		"delivery report":      {"Content-Type: multipart/report; report-type=delivery-status; boundary=b"},
	}
	for name, hs := range cases {
		raw := e.eml("bob@example.com", addr, "Re: x", append([]string{authOK}, hs...), "text")
		if strings.HasPrefix(hs[0], "Content-Type") { // replace, not duplicate, the content type
			raw = bytes.Replace(raw, []byte("Content-Type: text/plain; charset=utf-8\r\n"), nil, 1)
		}
		if r := e.ingest(true, raw); r.Outcome != "ignored" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	for _, subj := range []string{"Automatic reply: Re: x", "Out of Office: back Monday", "Auto: away"} {
		if r := e.ingest(true, e.eml("bob@example.com", addr, subj, []string{authOK}, "away")); r.Outcome != "ignored" {
			t.Errorf("%s: %+v", subj, r)
		}
	}
	if r := e.ingest(true, e.eml("MAILER-DAEMON@example.com", addr, "Undelivered", []string{authOK}, "x")); r.Outcome != "ignored" {
		t.Errorf("%+v", r)
	}
	// our own notification fed back in (a loop through a mailbox that forwards everything) is ignored
	own := e.msgTo("bob@example.com")[0].Raw
	if r := e.ingest(true, own); r.Outcome != "ignored" {
		t.Fatalf("%+v", r)
	}
	if len(e.comments()) != 0 {
		t.Fatal("something was posted")
	}
	// the sender limit stops a flood: the limit is three, the fourth is ignored
	e.Svc.Mail.Inbound.PerSenderHour = 3
	var outcomes []string
	for i := 0; i < 5; i++ {
		outcomes = append(outcomes, e.ingest(true, e.eml("flood@example.com", addr, "x", []string{authOK}, fmt.Sprint("n", i))).Outcome)
	}
	if outcomes[0] != "quarantined" || outcomes[4] != "ignored" {
		t.Fatalf("%v", outcomes)
	}
	// and the window moves on
	e.Clock.Advance(61 * time.Minute)
	if r := e.ingest(true, e.eml("flood@example.com", addr, "x", []string{authOK}, "later")); r.Outcome != "quarantined" {
		t.Fatalf("%+v", r)
	}
}

func TestMAIL16_InboundMailNeverWidensAccess(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	reply := func() domain.InboundResult {
		return e.ingest(true, e.eml("bob@example.com", addr, "Re: x", []string{authOK}, "a comment"))
	}
	// Bob is made a viewer: the signed address is not a permission
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Bob.ID, "viewer")
	if r := reply(); r.Outcome != "quarantined" || !strings.Contains(r.Reason, "may not comment") {
		t.Fatalf("%+v", r)
	}
	// Bob loses access entirely
	e.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id=$1`, e.Bob.ID)
	if r := reply(); r.Outcome != "quarantined" {
		t.Fatalf("%+v", r)
	}
	// even if released by an administrator, the comment is not made: the release acts with Bob's permissions
	q, _ := e.Svc.ListInbound(ctx, e.Admin, "quarantined", 10)
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, q[0].ID); err == nil {
		t.Fatal("release created a comment for a user without access")
	}
	again, _ := e.Svc.ListInbound(ctx, e.Admin, "quarantined", 10)
	if len(again) != len(q) {
		t.Fatal("a failed release changed the outcome")
	}
	// membership back, then the issue is archived: archived work stays read-only
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Bob.ID, "member")
	if r := reply(); r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	tid, ok := trans(t, e.Env, e.Alice, e.issue.Key, "Resolve")
	if !ok {
		t.Fatal("no Resolve transition")
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, e.issue.Key, tid, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ArchiveIssue(ctx, e.Alice, e.issue.Key); err != nil {
		t.Fatal(err)
	}
	if r := reply(); r.Outcome != "quarantined" {
		t.Fatalf("archived issue accepted a comment: %+v", r)
	}
	// an unknown sender is quarantined and cannot be released
	r := e.ingest(true, e.eml("stranger@example.org", addr, "Re: x", []string{"Authentication-Results: mx; dkim=pass header.d=example.org"}, "hello"))
	if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "not a known user") {
		t.Fatalf("%+v", r)
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, r.ID); err == nil {
		t.Fatal("released a message from an unknown sender")
	}
}

func TestMAIL07_FallbacksWhenTheAddressIsLost(t *testing.T) {
	e := newInbound(t)
	msgs := e.msgTo("bob@example.com")
	h, _, _ := readMsg(t, msgs[0].Raw)
	mid := h.Get("Message-Id")
	// In-Reply-To names a message we sent
	r := e.ingest(true, e.eml("bob@example.com", "tasklane@example.com", "Re: whatever", []string{authOK, "In-Reply-To: " + mid}, "by header"))
	if r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	// only the key in the subject
	r = e.ingest(true, e.eml("bob@example.com", "tasklane@example.com", "Re: [ABC-1] Printer is on fire", []string{authOK}, "by subject"))
	if r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	// a subject key for an issue the sender cannot see is not a way in
	e.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id=$1`, e.Bob.ID)
	r = e.ingest(true, e.eml("bob@example.com", "tasklane@example.com", "Re: [ABC-1] x", []string{authOK}, "no access"))
	if r.Outcome != "quarantined" {
		t.Fatalf("%+v", r)
	}
	// nothing identifies an issue: ignored with the reason, nothing is created
	if r := e.ingest(true, e.eml("alice@example.com", "tasklane@example.com", "hello", []string{authOK}, "nothing")); r.Outcome != "ignored" {
		t.Fatalf("%+v", r)
	}
}

func TestMAIL09_14_RawKeptBeforeAckLimitsAndRecovery(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	// too large: rejected with a recorded outcome, nothing parsed or stored
	e.Svc.Mail.Inbound.MaxBytes = 2000
	big := e.eml("bob@example.com", addr, "Re: x", []string{authOK}, strings.Repeat("x", 5000))
	if r := e.ingest(true, big); r.Outcome != "rejected" {
		t.Fatalf("%+v", r)
	}
	e.Svc.Mail.Inbound.MaxBytes = 0
	// a huge header section is refused
	huge := e.eml("bob@example.com", addr, "Re: x", []string{"X-Pad: " + strings.Repeat("y", 300000)}, "x")
	if r := e.ingest(true, huge); r.Outcome != "rejected" {
		t.Fatalf("%+v", r)
	}
	// not a message
	if r := e.ingest(true, []byte("\x00\x01 not mail at all")); r.Outcome != "rejected" {
		t.Fatalf("%+v", r)
	}
	// the process died after claiming a message and before finishing it: the relay's retry finishes it,
	// but not within the first two minutes (another node may still be working on it)
	raw := e.eml("bob@example.com", addr, "Re: x", []string{authOK}, "finished after a crash")
	mid := regexp.MustCompile(`Message-ID: (<[^>]+>)`).FindSubmatch(raw)[1]
	e.Store.Pool.Exec(ctx, `INSERT INTO mail_inbound(message_id,received_at) VALUES($1,$2)`, string(mid), e.Clock.Now())
	if r := e.ingest(true, raw); !r.Duplicate {
		t.Fatalf("a retry within two minutes took over: %+v", r)
	}
	e.Clock.Advance(3 * time.Minute)
	if r := e.ingest(true, raw); r.Outcome != "accepted" || r.Duplicate {
		t.Fatalf("%+v", r)
	}
	if r := e.ingest(true, raw); !r.Duplicate {
		t.Fatalf("%+v", r)
	}
	if n := len(e.comments()); n != 1 {
		t.Fatalf("%d comments", n)
	}
	// without a blob store nothing is acknowledged (the sender retries)
	e.Svc.Blobs = nil
	if _, err := e.Svc.IngestMail(ctx, e.trusted.ID, true, e.eml("bob@example.com", addr, "Re: y", []string{authOK}, "x")); err == nil {
		t.Fatal("accepted a message it could not keep")
	}
	var pending int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE outcome='pending'`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d pending rows left by a failed store", pending)
	}
}

func TestMAIL09_WebhookTransportVerifiesTheSignature(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	raw := e.eml("bob@example.com", addr, "Re: x", []string{authOK}, "via the webhook")
	sign := func(secret string, b []byte) string { return domain.SignInbound(secret, b) }
	if _, err := e.Svc.ReceiveInboundMail(ctx, e.trusted.ID, "sha256=00", raw); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.ReceiveInboundMail(ctx, e.trusted.ID, sign(e.src.Secret, raw), raw); err != domain.ErrForbidden {
		t.Fatalf("another source's secret was accepted: %v", err)
	}
	if _, err := e.Svc.ReceiveInboundMail(ctx, "00000000-0000-0000-0000-000000000000", sign(e.trusted.Secret, raw), raw); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.ReceiveInboundMail(ctx, e.trusted.ID, sign(e.trusted.Secret, raw), append(raw, 'x')); err != domain.ErrForbidden {
		t.Fatalf("a changed body was accepted: %v", err)
	}
	r, err := e.Svc.ReceiveInboundMail(ctx, e.trusted.ID, sign(e.trusted.Secret, raw), raw)
	if err != nil || r.Outcome != "accepted" {
		t.Fatalf("%+v %v", r, err)
	}
	// a source marked untrusted cannot make its own Authentication-Results count
	raw2 := e.eml("bob@example.com", addr, "Re: x", []string{authOK}, "from the untrusted source")
	if r, err := e.Svc.ReceiveInboundMail(ctx, e.src.ID, sign(e.src.Secret, raw2), raw2); err != nil || r.Outcome != "quarantined" {
		t.Fatalf("%+v %v", r, err)
	}
	// the secret is shown once and not listed
	l, _ := e.Svc.ListMailSources(ctx, e.Admin)
	for _, s := range l {
		if s.Secret != "" {
			t.Fatal("a secret is listed")
		}
	}
	if _, err := e.Svc.CreateMailSource(ctx, e.Alice, "x", true); err == nil {
		t.Fatal("a non-administrator created a source")
	}
	if err := e.Svc.DeleteMailSource(ctx, e.Admin, e.trusted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ReceiveInboundMail(ctx, e.trusted.ID, sign(e.trusted.Secret, raw), raw); err != domain.ErrForbidden {
		t.Fatalf("a deleted source still delivers: %v", err)
	}
}

func (e *inboundEnv) issueCount() (n int) {
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id=(SELECT id FROM project WHERE key='ABC')`).Scan(&n)
	return
}

func TestMAIL08_CreateIssueByEmail(t *testing.T) {
	e := newInbound(t)
	before := e.issueCount()
	// Alice writes to the project address, copying Bob and someone who is not a user
	raw := e.eml("Alice K. <alice@example.com>", "ABC@tasklane.example.com", "Re: Fwd: Login page broken on Safari",
		[]string{authOK, "Cc: Bob R. <bob@example.com>, outsider@example.org"}, "Steps:\r\n1. open the page\r\n2. click Sign in\r\n")
	r := e.ingest(true, raw)
	if r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	if e.issueCount() != before+1 {
		t.Fatal("no issue was created")
	}
	var key, summary, desc, typ, reporter string
	e.Store.Pool.QueryRow(ctx, `SELECT i.key,i.summary,(SELECT body FROM issue_text WHERE issue_id=i.id AND field='description'),t.name,u.email FROM issue i JOIN issue_type t ON t.id=i.issue_type_id JOIN app_user u ON u.id=i.reporter_id
		WHERE i.id=(SELECT issue_id FROM mail_inbound WHERE id=$1)`, r.ID).Scan(&key, &summary, &desc, &typ, &reporter)
	if summary != "Login page broken on Safari" || typ != "Task" || reporter != "alice@example.com" || !strings.Contains(desc, "click Sign in") {
		t.Fatalf("%q %q %q %q", summary, typ, reporter, desc)
	}
	// the Cc'd user watches it, the outsider is nothing
	if w, _ := e.Svc.WatchState(ctx, e.Bob, key); !w.Watching || !w.Explicit {
		t.Fatalf("%+v", w)
	}
	// a plus-tag names the type, the rest are labels
	r = e.ingest(true, e.eml("alice@example.com", "ABC+bug+urgent+safari@tasklane.example.com", "Crash on save", []string{authOK}, "it crashes"))
	var typ2 string
	var labels []string
	e.Store.Pool.QueryRow(ctx, `SELECT t.name,i.labels FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.id=(SELECT issue_id FROM mail_inbound WHERE id=$1)`, r.ID).Scan(&typ2, &labels)
	if r.Outcome != "accepted" || typ2 != "Bug" || strings.Join(labels, ",") != "safari,urgent" {
		t.Fatalf("%+v %q %v", r, typ2, labels)
	}
	// a reply to the message that created an issue continues there
	cs0 := len(e.comments())
	first := e.eml("alice@example.com", "ABC@tasklane.example.com", "Thread root", []string{authOK}, "root")
	r = e.ingest(true, first)
	mid := regexp.MustCompile(`Message-ID: (<[^>]+>)`).FindSubmatch(first)[1]
	r2 := e.ingest(true, e.eml("bob@example.com", "ABC@tasklane.example.com", "Re: Thread root", []string{authOK, "In-Reply-To: " + string(mid)}, "a follow-up"))
	var cnt int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM comment c WHERE c.mail_inbound_id=$1 AND c.issue_id=(SELECT issue_id FROM mail_inbound WHERE id=$2)`, r2.ID, r.ID).Scan(&cnt)
	if r2.Outcome != "accepted" || cnt != 1 {
		t.Fatalf("%+v %d", r2, cnt)
	}
	_ = cs0
	// an empty subject and a very long one
	r = e.ingest(true, e.eml("alice@example.com", "ABC@tasklane.example.com", "", []string{authOK}, "no subject"))
	var sum string
	e.Store.Pool.QueryRow(ctx, `SELECT summary FROM issue WHERE id=(SELECT issue_id FROM mail_inbound WHERE id=$1)`, r.ID).Scan(&sum)
	if sum != "(no subject)" {
		t.Fatalf("%q", sum)
	}
	r = e.ingest(true, e.eml("alice@example.com", "ABC@tasklane.example.com", strings.Repeat("long ", 100), []string{authOK}, "x"))
	e.Store.Pool.QueryRow(ctx, `SELECT summary FROM issue WHERE id=(SELECT issue_id FROM mail_inbound WHERE id=$1)`, r.ID).Scan(&sum)
	if len([]rune(sum)) > 200 || r.Outcome != "accepted" {
		t.Fatalf("%d %+v", len(sum), r)
	}
	// an address that is no project creates nothing
	n := e.issueCount()
	if r := e.ingest(true, e.eml("alice@example.com", "NOPE@tasklane.example.com", "hello", []string{authOK}, "x")); r.Outcome != "ignored" {
		t.Fatalf("%+v", r)
	}
	if e.issueCount() != n {
		t.Fatal("an issue appeared")
	}
	// the project shows its address
	pm, err := e.Svc.ProjectMail(ctx, e.Alice, "ABC")
	if err != nil || pm.Address != "abc@tasklane.example.com" || pm.Policy != "quarantine" {
		t.Fatalf("%+v %v", pm, err)
	}
}

func TestMAIL08_16_UnknownSendersAndPermissionsForNewIssues(t *testing.T) {
	e := newInbound(t)
	stranger := func(subj string) domain.InboundResult {
		return e.ingest(true, e.eml("customer@example.org", "ABC@tasklane.example.com", subj, []string{"Authentication-Results: mx; dkim=pass header.d=example.org"}, "please help"))
	}
	n := e.issueCount()
	if r := stranger("a"); r.Outcome != "quarantined" || !strings.Contains(r.Reason, "not a known user") {
		t.Fatalf("%+v", r)
	}
	if err := e.Svc.SetMailPolicy(ctx, e.Alice, "ABC", "reject"); err == nil {
		t.Fatal("a non-administrator changed the policy")
	}
	if err := e.Svc.SetMailPolicy(ctx, e.Admin, "ABC", "accept-all"); err == nil {
		t.Fatal("an unknown policy was accepted")
	}
	if err := e.Svc.SetMailPolicy(ctx, e.Admin, "ABC", "reject"); err != nil {
		t.Fatal(err)
	}
	if r := stranger("b"); r.Outcome != "rejected" || !strings.Contains(r.Reason, "rejects mail from unknown senders") {
		t.Fatalf("%+v", r)
	}
	if e.issueCount() != n {
		t.Fatal("an unknown sender created an issue")
	}
	// the policy does not apply to a known user, and a reply to a notification is not affected by it
	if r := e.ingest(true, e.eml("alice@example.com", "ABC@tasklane.example.com", "known", []string{authOK}, "x")); r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	// a viewer cannot create: quarantined, and a release does not create either
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Bob.ID, "viewer")
	n = e.issueCount()
	r := e.ingest(true, e.eml("bob@example.com", "ABC@tasklane.example.com", "viewer tries", []string{authOK}, "x"))
	if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "refused") {
		t.Fatalf("%+v", r)
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, r.ID); err == nil || e.issueCount() != n {
		t.Fatalf("a release created an issue for a viewer: %v", err)
	}
	// unproven mail to a project address waits; releasing opens the issue once, as the sender
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Bob.ID, "member")
	r = e.ingest(false, e.eml("bob@example.com", "ABC+bug@tasklane.example.com", "unproven report", []string{authOK}, "details"))
	if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "proof of origin") || e.issueCount() != n {
		t.Fatalf("%+v", r)
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, r.ID); err != nil {
		t.Fatal(err)
	}
	var typ, rep string
	e.Store.Pool.QueryRow(ctx, `SELECT t.name,u.email FROM issue i JOIN issue_type t ON t.id=i.issue_type_id JOIN app_user u ON u.id=i.reporter_id WHERE i.summary='unproven report'`).Scan(&typ, &rep)
	if typ != "Bug" || rep != "bob@example.com" || e.issueCount() != n+1 {
		t.Fatalf("%q %q %d", typ, rep, e.issueCount())
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, r.ID); err == nil || e.issueCount() != n+1 {
		t.Fatal("released twice")
	}
}

type fixtureDNS map[string]string

func (m fixtureDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := m[name]; ok {
		return []string{v}, nil
	}
	return nil, errors.New("NXDOMAIN")
}

func loadDKIM(t *testing.T) (fixtureDNS, func(string) []byte) {
	b, err := os.ReadFile("../mail/testdata/dkim/dns.json")
	if err != nil {
		t.Fatal(err)
	}
	var m fixtureDNS
	json.Unmarshal(b, &m)
	return m, func(n string) []byte {
		r, err := os.ReadFile("../mail/testdata/dkim/" + n)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
}

// MAIL-10: Tasklane verifies DKIM on the stored raw message itself, so a plain source (no trusted receiver) can prove origin.
func TestMAIL10_DKIMProvesOriginWithoutATrustedReceiver(t *testing.T) {
	e := newInbound(t)
	dns, file := loadDKIM(t)
	e.Svc.DNS = dns
	// signed by the sender's own domain, delivered by a source nobody vouches for: accepted
	if r := e.ingest(false, file("reply_aligned.eml")); r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	cs := e.comments()
	if len(cs) != 1 || cs[0].Author.ID != e.Bob.ID || cs[0].Body != "Signed reply from Bob." {
		t.Fatalf("%+v", cs)
	}
	// a valid signature by a different domain is no proof that bob@example.com wrote it
	r := e.ingest(false, file("reply_misaligned.eml"))
	if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "not aligned") {
		t.Fatalf("%+v", r)
	}
	// the signed message with its body changed in transit
	forged := bytes.Replace(file("reply_aligned.eml"), []byte("Signed reply from Bob."), []byte("Give Eve access."), 1)
	forged = bytes.Replace(forged, []byte("sig1@"), []byte("sig4@"), 1) // a different Message-ID, so it is not a duplicate
	r = e.ingest(false, forged)
	if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "body hash") && !strings.Contains(r.Reason, "does not verify") {
		t.Fatalf("%+v", r)
	}
	// the key is gone from DNS (or DNS is down): nothing is proven
	e.Svc.DNS = fixtureDNS{}
	again := bytes.Replace(file("reply_aligned.eml"), []byte("sig1@"), []byte("sig5@"), 1)
	if r := e.ingest(false, again); r.Outcome != "quarantined" || !strings.Contains(r.Reason, "could not be found") {
		t.Fatalf("%+v", r)
	}
	// an untrusted source's own Authentication-Results still counts for nothing
	e.Svc.DNS = dns
	if r := e.ingest(false, e.eml("bob@example.com", "tasklane@example.com", "Re: [ABC-1] x", []string{authOK}, "claims a pass")); r.Outcome != "quarantined" || !strings.Contains(r.Reason, "no DKIM signature") {
		t.Fatalf("%+v", r)
	}
	// a trusted receiver's verdict still works when there is no signature
	if r := e.ingest(true, e.eml("bob@example.com", "tasklane@example.com", "Re: [ABC-1] y", []string{authOK}, "vouched for")); r.Outcome != "accepted" {
		t.Fatalf("%+v", r)
	}
	if len(e.comments()) != 2 {
		t.Fatalf("%d comments", len(e.comments()))
	}
	// without a resolver configured, signatures are not looked at (a trusted receiver is then the only proof)
	e.Svc.DNS = nil
	third := bytes.Replace(file("reply_aligned.eml"), []byte("sig1@"), []byte("sig6@"), 1)
	if r := e.ingest(false, third); r.Outcome != "quarantined" {
		t.Fatalf("%+v", r)
	}
}

// SEC-09: mail from a person of another organization cannot create or comment in this organization's projects, and releasing it does not either.
func TestSEC09_InboundMailDoesNotCrossOrganizations(t *testing.T) {
	e := newInbound(t)
	if _, err := e.Svc.CreateOrganization(ctx, e.Admin, "Globex", "boss@globex.example", "Boss", "password123"); err != nil {
		t.Fatal(err)
	}
	var bid string
	e.Store.Pool.QueryRow(ctx, `SELECT id FROM app_user WHERE email='boss@globex.example'`).Scan(&bid)
	boss, _ := e.Svc.ActorFor(ctx, bid)
	if _, err := e.Svc.CreateUserAs(ctx, boss, "gina@globex.example", "Gina", "password123", false, ""); err != nil {
		t.Fatal(err)
	}
	n := e.issueCount()
	cs := len(e.comments())
	for name, raw := range map[string][]byte{
		"create at the project address": e.eml("gina@globex.example", "ABC@tasklane.example.com", "planted issue", []string{authOK}, "from another organization"),
		"reply by the subject key":      e.eml("gina@globex.example", "tasklane@example.com", "Re: [ABC-1] x", []string{authOK}, "planted comment"),
		"reply by the signed address":   e.eml("gina@globex.example", e.replyAddr("bob@example.com"), "Re: x", []string{authOK}, "planted via bob's address"),
	} {
		r := e.ingest(true, raw)
		if r.Outcome != "quarantined" {
			t.Errorf("%s: %+v", name, r)
		}
		if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, r.ID); err == nil {
			t.Errorf("%s: a release acted across organizations", name)
		}
	}
	if e.issueCount() != n || len(e.comments()) != cs {
		t.Fatalf("something was planted: %d issues (was %d), %d comments (was %d)", e.issueCount(), n, len(e.comments()), cs)
	}
}
