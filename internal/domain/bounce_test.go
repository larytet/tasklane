package domain_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

var reVERP = regexp.MustCompile(`^bounce\+[A-Za-z0-9_-]{20,40}@tasklane\.example\.com$`)

// verp is the envelope sender of the n-th message the relay saw for this recipient (0 is the first).
func (e *inboundEnv) verp(to string, n int) string {
	e.T.Helper()
	ms := e.msgTo(to)
	if len(ms) <= n {
		e.T.Fatalf("only %d messages to %s", len(ms), to)
	}
	return ms[n].From
}

func (e *inboundEnv) origID(to string, n int) string {
	h, _, _ := readMsg(e.T, e.msgTo(to)[n].Raw)
	return h.Get("Message-Id")
}

func dsn(to, rcpt, action, status, origID string) []byte {
	b := "BOUNDARY"
	return []byte("From: MAILER-DAEMON@mx.example.org\r\nTo: " + to + "\r\nSubject: Delivery Status Notification\r\nMessage-ID: <dsn-" + fmt.Sprint(time.Now().UnixNano()) + "@mx.example.org>\r\nReturn-Path: <>\r\nAuto-Submitted: auto-replied\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=" + b + "\r\n\r\n" +
		"--" + b + "\r\nContent-Type: text/plain\r\n\r\nYour message could not be delivered.\r\n" +
		"--" + b + "\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: dns; mx.example.org\r\n\r\nFinal-Recipient: rfc822; " + rcpt + "\r\nAction: " + action + "\r\nStatus: " + status + "\r\nDiagnostic-Code: smtp; 550 mailbox unavailable\r\n\r\n" +
		"--" + b + "\r\nContent-Type: text/rfc822-headers\r\n\r\nMessage-ID: " + origID + "\r\nSubject: [ABC-1] x\r\n\r\n" +
		"--" + b + "--\r\n")
}

func complaint(to, rcpt, origID string) []byte {
	b := "ARF"
	return []byte("From: abuse@isp.example.org\r\nTo: " + to + "\r\nSubject: FW: abuse report\r\nMessage-ID: <arf-" + fmt.Sprint(time.Now().UnixNano()) + "@isp.example.org>\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/report; report-type=feedback-report; boundary=" + b + "\r\n\r\n" +
		"--" + b + "\r\nContent-Type: text/plain\r\n\r\nThis is an abuse report.\r\n" +
		"--" + b + "\r\nContent-Type: message/feedback-report\r\n\r\nFeedback-Type: abuse\r\nUser-Agent: test/1.0\r\nOriginal-Rcpt-To: <" + rcpt + ">\r\n\r\n" +
		"--" + b + "\r\nContent-Type: message/rfc822\r\n\r\nMessage-ID: " + origID + "\r\nSubject: [ABC-1] x\r\n\r\n" +
		"--" + b + "--\r\n")
}

func (e *inboundEnv) suppressedList() []domain.Suppression {
	l, err := e.Svc.ListSuppressions(ctx, e.Admin)
	if err != nil {
		e.T.Fatal(err)
	}
	var out []domain.Suppression
	for _, s := range l {
		if !s.Cleared {
			out = append(out, s)
		}
	}
	return out
}

func (e *inboundEnv) assignToBob(summary string) {
	e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: summary, Assignee: e.Bob.ID})
	e.flush()
}

func TestMAIL06_EveryMessageHasItsOwnReturnPathAndAHardBounceSuppressesTheAddress(t *testing.T) {
	e := newInbound(t)
	e.assignToBob("second")
	a, b := e.verp("bob@example.com", 0), e.verp("bob@example.com", 1)
	if !reVERP.MatchString(a) || !reVERP.MatchString(b) || a == b {
		t.Fatalf("return paths %q %q", a, b)
	}
	// the relay reports the first message undeliverable
	r := e.ingest(false, dsn(a, "bob@example.com", "failed", "5.1.1", e.origID("bob@example.com", 0)))
	if r.Outcome != "ignored" || !strings.Contains(r.Reason, "bounce handled: hard") {
		t.Fatalf("%+v", r)
	}
	sl := e.suppressedList()
	if len(sl) != 1 || sl[0].Email != "bob@example.com" || sl[0].Kind != "hard" || sl[0].User != "Bob R." {
		t.Fatalf("%+v", sl)
	}
	// from now on Bob gets no email, and the delivery log says why; the in-app notification is not touched
	connsBefore := len(e.smtp.Messages())
	var inboxBefore int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM notification WHERE user_id=$1`, e.Bob.ID).Scan(&inboxBefore)
	e.assignToBob("third")
	if len(e.smtp.Messages()) != connsBefore {
		t.Fatal("mail was sent to a suppressed address")
	}
	var inboxAfter int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM notification WHERE user_id=$1`, e.Bob.ID).Scan(&inboxAfter)
	if inboxAfter != inboxBefore+1 {
		t.Fatalf("the inbox lost a notification: %d -> %d", inboxBefore, inboxAfter)
	}
	var st, why string
	e.Store.Pool.QueryRow(ctx, `SELECT status,last_error FROM mail_message WHERE recipient LIKE '%bob@example.com%' ORDER BY id DESC LIMIT 1`).Scan(&st, &why)
	if st != "suppressed" || !strings.Contains(why, "bounce") {
		t.Fatalf("%s %s", st, why)
	}
	// Bob sees the indicator; Alice does not
	if !e.Svc.MyNotifyPrefs(ctx, e.Bob).EmailBouncing || e.Svc.MyNotifyPrefs(ctx, e.Alice).EmailBouncing {
		t.Fatal("the indicator is wrong")
	}
	// only an administrator clears it, it is audited, and then mail flows again
	if err := e.Svc.ClearSuppression(ctx, e.Alice, "bob@example.com"); err == nil {
		t.Fatal("a non-administrator cleared a suppression")
	}
	if err := e.Svc.ClearSuppression(ctx, e.Admin, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.ClearSuppression(ctx, e.Admin, "bob@example.com"); err == nil {
		t.Fatal("cleared twice")
	}
	var audits int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='mail.suppression.clear'`).Scan(&audits)
	if audits != 1 || len(e.suppressedList()) != 0 || e.Svc.MyNotifyPrefs(ctx, e.Bob).EmailBouncing {
		t.Fatalf("%d %v", audits, e.suppressedList())
	}
	e.assignToBob("fourth")
	if len(e.smtp.Messages()) != connsBefore+1 {
		t.Fatal("mail did not resume after the clear")
	}
}

func TestMAIL06_SoftBouncesCountInAWindowAndComplaintsSuppressAtOnce(t *testing.T) {
	e := newInbound(t)
	for i := 0; i < 4; i++ {
		e.assignToBob(fmt.Sprint("n", i))
	}
	soft := func(n int) domain.InboundResult {
		return e.ingest(false, dsn(e.verp("bob@example.com", n), "bob@example.com", "failed", "4.2.2", e.origID("bob@example.com", n)))
	}
	for i := 0; i < 2; i++ {
		if r := soft(i); !strings.Contains(r.Reason, "soft bounce 4.2.2 recorded") || len(e.suppressedList()) != 0 {
			t.Fatalf("%d: %+v", i, r)
		}
	}
	// a week later the first two no longer count
	e.Clock.Advance(8 * 24 * time.Hour)
	if soft(2); len(e.suppressedList()) != 0 {
		t.Fatal("old soft bounces were counted")
	}
	soft(3)
	e.Clock.Advance(time.Hour)
	// two recent ones are still below the limit of three; one more tips it
	e.assignToBob("more")
	if r := soft(4); len(e.suppressedList()) != 1 || !strings.Contains(r.Reason, "suppressed") {
		t.Fatalf("%+v %v", r, e.suppressedList())
	}
	if e.suppressedList()[0].Kind != "soft" {
		t.Fatal(e.suppressedList()[0].Kind)
	}
	// a delayed notice is not a bounce
	e.Svc.ClearSuppression(ctx, e.Admin, "bob@example.com")
	n := len(e.suppressedList())
	if r := e.ingest(false, dsn(e.verp("bob@example.com", 0), "bob@example.com", "delayed", "4.4.1", e.origID("bob@example.com", 0))); !strings.Contains(r.Reason, "no change") || len(e.suppressedList()) != n {
		t.Fatalf("%+v", r)
	}
	// a complaint (abuse feedback report) suppresses immediately
	if r := e.ingest(false, complaint(e.verp("bob@example.com", 1), "bob@example.com", e.origID("bob@example.com", 1))); !strings.Contains(r.Reason, "complaint") {
		t.Fatalf("%+v", r)
	}
	if sl := e.suppressedList(); len(sl) != 1 || sl[0].Kind != "complaint" {
		t.Fatalf("%+v", sl)
	}
}

func TestMAIL06_ForgedMisdirectedAndUnsignedReportsDoNothing(t *testing.T) {
	e := newInbound(t)
	good := e.verp("bob@example.com", 0)
	orig := e.origID("bob@example.com", 0)
	cases := []struct {
		name, want string
		raw        []byte
	}{
		{"altered token", "not valid", dsn(strings.Replace(good, "bounce+", "bounce+A", 1), "bob@example.com", "failed", "5.1.1", orig)},
		{"token of another kind of address", "not valid", dsn("bounce+AAAAAAAAAAAAAAAAAAAAAAAAAA@tasklane.example.com", "bob@example.com", "failed", "5.1.1", orig)},
		{"report names someone else", "different address", dsn(good, "alice@example.com", "failed", "5.1.1", orig)},
		{"no return path at all", "not acted on", dsn("tasklane@tasklane.example.com", "bob@example.com", "failed", "5.1.1", orig)},
		{"a person writes to the return path", "not a delivery report", []byte("From: carol@example.org\r\nTo: " + good + "\r\nSubject: hello\r\nMessage-ID: <p1@example.org>\r\n\r\nplease suppress bob\r\n")},
	}
	for _, c := range cases {
		r := e.ingest(false, c.raw)
		if r.Outcome != "ignored" || !strings.Contains(r.Reason, c.want) {
			t.Errorf("%s: %+v", c.name, r)
		}
	}
	if len(e.suppressedList()) != 0 {
		t.Fatalf("a forged report suppressed an address: %+v", e.suppressedList())
	}
	// a token signed by a key that was revoked is no longer believed
	e.Svc.RotateReplyKey(ctx, e.Admin)
	e.Svc.RevokeReplyKey(ctx, e.Admin, 1)
	if r := e.ingest(false, dsn(good, "bob@example.com", "failed", "5.1.1", orig)); !strings.Contains(r.Reason, "not valid") || len(e.suppressedList()) != 0 {
		t.Fatalf("%+v", r)
	}
	// a bounce never becomes a comment or an issue and sends nothing back
	if len(e.comments()) != 0 || len(e.msgTo("MAILER-DAEMON@mx.example.org")) != 0 {
		t.Fatal("a bounce had an effect beyond suppression")
	}
}

func TestMAIL06_ProviderEventsAreSignedAndOnlyForAddressesWeMailed(t *testing.T) {
	e := newInbound(t)
	body := func(s string) []byte { return []byte(s) }
	ok := body(`{"events":[{"type":"bounce","email":"bob@example.com","status":"550"},{"type":"complaint","email":"never-mailed@example.org"},{"type":"nonsense","email":"bob@example.com"}]}`)
	if _, err := e.Svc.ReceiveBounceEvents(ctx, e.trusted.ID, "sha256=00", ok); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.ReceiveBounceEvents(ctx, e.trusted.ID, domain.SignInbound(e.src.Secret, ok), ok); err != domain.ErrForbidden {
		t.Fatalf("another source's secret: %v", err)
	}
	if _, err := e.Svc.ReceiveBounceEvents(ctx, e.trusted.ID, domain.SignInbound(e.trusted.Secret, body("not json")), body("not json")); err == nil || err == domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	n, err := e.Svc.ReceiveBounceEvents(ctx, e.trusted.ID, domain.SignInbound(e.trusted.Secret, ok), ok)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	sl := e.suppressedList()
	if len(sl) != 1 || sl[0].Email != "bob@example.com" || sl[0].Kind != "hard" || !strings.Contains(sl[0].Reason, "provider") {
		t.Fatalf("%+v", sl)
	}
}
