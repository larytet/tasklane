package domain_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil/imapmock"
)

type imapEnv struct {
	*inboundEnv
	box  *imapmock.Mock
	acct domain.IMAPAccount
}

func newIMAPEnv(t *testing.T, trusted bool) *imapEnv {
	e := &imapEnv{inboundEnv: newInbound(t)}
	e.box = imapmock.New(t, imapmock.Options{User: "tasklane", Pass: `s3cr3t"pw`})
	e.Svc.IMAPAllowPrivate = true
	a, err := e.Svc.CreateIMAP(ctx, e.Admin, domain.IMAPIn{Name: "support", Host: "127.0.0.1", Port: e.box.Port(), TLS: "none", User: "tasklane", Password: `s3cr3t"pw`, Trusted: trusted})
	if err != nil {
		t.Fatal(err)
	}
	e.acct = a
	return e
}

// reply is a message from Bob that becomes a comment on ABC-1 (trusted receiver's verdict in the headers).
func (e *imapEnv) reply(text string) []byte {
	return e.eml("bob@example.com", "tasklane@example.com", "Re: [ABC-1] Printer is on fire", []string{authOK}, text)
}

func (e *imapEnv) state() (lastUID int64, resets int, received int64, lastErr string) {
	e.Store.Pool.QueryRow(ctx, `SELECT last_uid,resets,received,last_error FROM mail_imap WHERE id=$1`, e.acct.ID).Scan(&lastUID, &resets, &received, &lastErr)
	return
}

func (e *imapEnv) poll() int {
	e.T.Helper()
	n, err := e.Svc.PollIMAP(ctx, e.acct.ID)
	if err != nil {
		e.T.Fatal(err)
	}
	return n
}

func TestMAIL09_IMAPReadsFromACheckpointAndEachMessageOnce(t *testing.T) {
	e := newIMAPEnv(t, true)
	for i := 0; i < 3; i++ {
		e.box.Append(e.reply(fmt.Sprint("reply ", i)))
	}
	if n := e.poll(); n != 3 {
		t.Fatalf("%d", n)
	}
	if last, _, rec, errs := e.state(); last != 3 || rec != 3 || errs != "" || len(e.comments()) != 3 {
		t.Fatalf("%d %d %q %d", last, rec, errs, len(e.comments()))
	}
	// nothing new: nothing is read again
	f := e.box.Fetches()
	if n := e.poll(); n != 0 || e.box.Fetches() != f {
		t.Fatalf("%d new, %d fetches", n, e.box.Fetches()-f)
	}
	e.box.Append(e.reply("a fourth"))
	if n := e.poll(); n != 1 || len(e.comments()) != 4 {
		t.Fatalf("%d %d", n, len(e.comments()))
	}
	// the password is sealed at rest and never listed
	var stored []byte
	e.Store.Pool.QueryRow(ctx, `SELECT password FROM mail_imap WHERE id=$1`, e.acct.ID).Scan(&stored)
	if strings.Contains(string(stored), "s3cr3t") {
		t.Fatal("the password is stored in the clear")
	}
	l, _ := e.Svc.ListIMAP(ctx, e.Admin)
	if len(l) != 1 || l[0].Received != 4 || l[0].LastOK == nil {
		t.Fatalf("%+v", l)
	}
	if _, err := e.Svc.ListIMAP(ctx, e.Alice); err == nil {
		t.Fatal("a non-administrator listed mailboxes")
	}
	if _, err := e.Svc.CreateIMAP(ctx, e.Alice, domain.IMAPIn{Name: "x", Host: "h", Port: 993, User: "u", Password: "p"}); err == nil {
		t.Fatal("a non-administrator added a mailbox")
	}
	// the Check button
	if r, err := e.Svc.CheckIMAP(ctx, e.Admin, e.acct.ID); err != nil || r["messages"] != 4 || r["idle"] != true {
		t.Fatalf("%v %v", r, err)
	}
}

func TestMAIL09_IMAPUntrustedMailboxNeedsDKIMAndTheEgressPolicyApplies(t *testing.T) {
	e := newIMAPEnv(t, false)
	e.box.Append(e.reply("claims a pass"))
	e.poll()
	if len(e.comments()) != 0 {
		t.Fatal("an untrusted mailbox's Authentication-Results counted")
	}
	if q, _ := e.Svc.ListInbound(ctx, e.Admin, "quarantined", 10); len(q) != 1 {
		t.Fatalf("%+v", q)
	}
	// a mailbox on a private address is refused unless the operator allowed it, and nothing connects
	e.Svc.IMAPAllowPrivate = false
	before := e.box.Connections()
	if _, err := e.Svc.PollIMAP(ctx, e.acct.ID); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("%v", err)
	}
	if e.box.Connections() != before {
		t.Fatal("a blocked address was contacted")
	}
	if _, _, _, msg := e.state(); !strings.Contains(msg, "egress") {
		t.Fatalf("the failure is not visible to administrators: %q", msg)
	}
	// a wrong password is an error that does not repeat the password
	e.Svc.IMAPAllowPrivate = true
	e.Store.Pool.Exec(ctx, `DELETE FROM mail_imap`)
	bad, _ := e.Svc.CreateIMAP(ctx, e.Admin, domain.IMAPIn{Name: "bad", Host: "127.0.0.1", Port: e.box.Port(), TLS: "none", User: "tasklane", Password: "guess"})
	if _, err := e.Svc.CheckIMAP(ctx, e.Admin, bad.ID); err == nil || strings.Contains(err.Error(), "guess") {
		t.Fatalf("%v", err)
	}
	// settings are validated
	for name, in := range map[string]domain.IMAPIn{
		"no port":        {Name: "a", Host: "h", User: "u", Password: "p"},
		"bad tls":        {Name: "a", Host: "h", Port: 1, TLS: "starttls?", User: "u", Password: "p"},
		"line break":     {Name: "a", Host: "h", Port: 1, User: "u\r\nx", Password: "p"},
		"no password":    {Name: "a", Host: "h", Port: 1, User: "u"},
		"duplicate name": {Name: "bad", Host: "h", Port: 1, User: "u", Password: "p"},
	} {
		if _, err := e.Svc.CreateIMAP(ctx, e.Admin, in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMAIL09_IMAPCrashesAndFailuresLoseNothingAndRepeatNothing(t *testing.T) {
	e := newIMAPEnv(t, true)
	for i := 0; i < 5; i++ {
		e.box.Append(e.reply(fmt.Sprint("message ", i)))
	}
	// the connection dies in the middle of the second fetch: the first is kept, the checkpoint says so
	e.box.DropAfterFetches(2)
	if _, err := e.Svc.PollIMAP(ctx, e.acct.ID); err == nil {
		t.Fatal("a dropped connection was not an error")
	}
	if last, _, rec, errs := e.state(); last != 1 || rec != 1 || errs == "" {
		t.Fatalf("%d %d %q", last, rec, errs)
	}
	// the next poll picks up exactly where it stopped
	if n := e.poll(); n != 4 || len(e.comments()) != 5 {
		t.Fatalf("%d %d", n, len(e.comments()))
	}
	// the process dies after a message was handled and before the checkpoint moved: the message comes again and is ignored
	e.Store.Pool.Exec(ctx, `UPDATE mail_imap SET last_uid=3 WHERE id=$1`, e.acct.ID)
	if n := e.poll(); n != 2 || len(e.comments()) != 5 {
		t.Fatalf("a re-read message was handled twice: %d %d", n, len(e.comments()))
	}
	// the store refuses (no blob store): the message is not acknowledged and the checkpoint stays
	e.box.Append(e.reply("needs the store"))
	saved := e.Svc.Blobs
	e.Svc.Blobs = nil
	if _, err := e.Svc.PollIMAP(ctx, e.acct.ID); err == nil {
		t.Fatal("a message that could not be kept was passed over")
	}
	if last, _, _, _ := e.state(); last != 5 {
		t.Fatalf("checkpoint moved to %d", last)
	}
	e.Svc.Blobs = saved
	if n := e.poll(); n != 1 || len(e.comments()) != 6 {
		t.Fatalf("%d %d", n, len(e.comments()))
	}
	// a message that was removed under us is skipped, the others still arrive
	a := e.box.Append(e.reply("will vanish"))
	e.box.Append(e.reply("survives"))
	e.box.FailFetch(a, true)
	if n := e.poll(); n != 1 || len(e.comments()) != 7 {
		t.Fatalf("%d %d", n, len(e.comments()))
	}
	if last, _, _, _ := e.state(); last != int64(a)+1 {
		t.Fatalf("the checkpoint did not move past the removed message: %d", last)
	}
	// two nodes polling at once: one reads, the other skips; no duplicates
	for i := 0; i < 20; i++ {
		e.box.Append(e.reply(fmt.Sprint("concurrent ", i)))
	}
	var wg sync.WaitGroup
	total := make([]int, 2)
	for i := range total {
		wg.Add(1)
		go func(i int) { defer wg.Done(); total[i], _ = e.Svc.PollIMAP(ctx, e.acct.ID) }(i)
	}
	wg.Wait()
	if total[0]+total[1] != 20 || len(e.comments()) != 27 {
		t.Fatalf("%v %d", total, len(e.comments()))
	}
}

func TestMAIL09_IMAPUIDValidityChangeReadsAgainWithoutDuplicates(t *testing.T) {
	e := newIMAPEnv(t, true)
	one, two := e.reply("first"), e.reply("second")
	e.box.Append(one)
	e.box.Append(two)
	e.poll()
	if len(e.comments()) != 2 {
		t.Fatal(len(e.comments()))
	}
	// the server rebuilt the mailbox: the same messages come back under new UIDs, with one more
	e.box.Reset(two, one, e.reply("third, new"))
	if n := e.poll(); n != 3 {
		t.Fatalf("%d", n)
	}
	if last, resets, _, _ := e.state(); resets != 1 || last != 3 {
		t.Fatalf("last %d resets %d", last, resets)
	}
	if cs := e.comments(); len(cs) != 3 || cs[2].Body != "third, new" {
		t.Fatalf("%d comments (the re-read messages must be ignored as duplicates)", len(cs))
	}
	var dupes int
	e.Store.Pool.QueryRow(ctx, `SELECT COALESCE(sum(dupes),0) FROM mail_inbound`).Scan(&dupes)
	if dupes != 2 {
		t.Fatalf("%d duplicates recorded", dupes)
	}
}

func TestMAIL09_14_IMAPOversizedMessagesAndABacklog(t *testing.T) {
	e := newIMAPEnv(t, true)
	e.Svc.Mail.Inbound.MaxBytes = 3000
	e.box.Append(e.reply(strings.Repeat("x", 20000)))
	e.box.Append(e.reply("after the big one"))
	if n := e.poll(); n != 2 {
		t.Fatalf("%d", n)
	}
	if r, _ := e.Svc.ListInbound(ctx, e.Admin, "rejected", 10); len(r) != 1 || !strings.Contains(r[0].Reason, "larger than the limit") {
		t.Fatalf("%+v", r)
	}
	if len(e.comments()) != 1 {
		t.Fatalf("%d comments", len(e.comments()))
	}
	// a backlog is read in batches; every message is recorded once and the checkpoint is at the end
	e.Svc.Mail.Inbound.MaxBytes = 0
	const backlog = 2500
	for i := 0; i < backlog; i++ {
		e.box.Append(e.eml("newsletter@example.org", "tasklane@example.com", fmt.Sprint("news ", i), []string{"Precedence: bulk"}, "x"))
	}
	start := time.Now()
	if n := e.poll(); n != backlog {
		t.Fatalf("%d of %d", n, backlog)
	}
	took := time.Since(start)
	var rows, distinct int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT message_id) FROM mail_inbound WHERE outcome='ignored' AND reason LIKE 'bulk%'`).Scan(&rows, &distinct)
	if rows != backlog || distinct != backlog {
		t.Fatalf("%d rows %d distinct", rows, distinct)
	}
	if last, _, _, _ := e.state(); last != int64(backlog)+2 {
		t.Fatalf("checkpoint %d", last)
	}
	t.Logf("%d messages in %s (%.0f per second)", backlog, took.Round(time.Millisecond), float64(backlog)/took.Seconds())
	if took > 2*time.Minute {
		t.Fatalf("too slow: %s", took)
	}
}

func TestMAIL09_17_IMAPIdleDeliversWithinSecondsAndStopsWhenRemoved(t *testing.T) {
	e := newIMAPEnv(t, true)
	e.Svc.IMAPIdle = time.Minute
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.Svc.RunIMAP(rctx, t.Logf)
	deadline := time.Now().Add(10 * time.Second)
	for e.box.Idlers() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the reader never started waiting")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// a message arrives while it waits: it is a comment within a second or two, not at the next poll
	start := time.Now()
	e.box.Append(e.reply("by IDLE"))
	for len(e.comments()) == 0 {
		if time.Since(start) > 3*time.Second {
			t.Fatal("the message did not arrive through IDLE")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("IDLE delivery took %s", time.Since(start).Round(time.Millisecond))
	// and again, to prove it goes back to waiting
	deadline = time.Now().Add(5 * time.Second)
	for e.box.Idlers() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	e.box.Append(e.reply("by IDLE, again"))
	start = time.Now()
	for len(e.comments()) < 2 {
		if time.Since(start) > 3*time.Second {
			t.Fatal("the second message did not arrive")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// removing the mailbox stops the reader within a few seconds
	if err := e.Svc.DeleteIMAP(ctx, e.Admin, e.acct.ID); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(12 * time.Second)
	for e.box.Idlers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the reader kept waiting on a removed mailbox")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMAIL09_IMAPServerWithoutIdlePollsOnItsInterval(t *testing.T) {
	e := &imapEnv{inboundEnv: newInbound(t)}
	e.box = imapmock.New(t, imapmock.Options{User: "tasklane", Pass: "pw", NoIdle: true})
	e.Svc.IMAPAllowPrivate = true
	e.Svc.IMAPPoll = 200 * time.Millisecond
	a, err := e.Svc.CreateIMAP(ctx, e.Admin, domain.IMAPIn{Name: "noidle", Host: "127.0.0.1", Port: e.box.Port(), TLS: "none", User: "tasklane", Password: "pw", Trusted: true})
	if err != nil {
		t.Fatal(err)
	}
	e.acct = a
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.Svc.RunIMAP(rctx, t.Logf)
	time.Sleep(500 * time.Millisecond)
	e.box.Append(e.reply("found by polling"))
	start := time.Now()
	for len(e.comments()) == 0 {
		if time.Since(start) > 3*time.Second {
			t.Fatal("polling did not find the message")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
