package domain_test

import (
	"bytes"
	"fmt"
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
