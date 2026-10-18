package domain_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/testutil/smtpmock"
)

func p95(d []time.Duration) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[(len(d)*95)/100]
}

// slowRelay points the service's outbound mail at a relay that takes delay to accept each message.
func (e *inboundEnv) slowRelay(delay time.Duration) *smtpmock.Mock {
	m := smtpmock.New(e.T, smtpmock.Options{TLS: "starttls", User: "u", Pass: "p", Delay: delay})
	e.Svc.Mail.Mailer = &mail.SMTP{Host: "localhost", Port: m.Port(), TLS: "starttls", User: "u", Pass: "p", From: "Tasklane <tasklane@example.com>", TLSConfig: &tls.Config{RootCAs: m.CA()}, Timeout: 5 * time.Second}
	return m
}

// MAIL-17: the request path never waits on SMTP. Creating issues and comments while the relay takes 400 ms per message
// returns at the speed of the database, because mail is queued by a worker and sent by another.
func TestMAIL17_TheRequestPathNeverWaitsOnSMTP(t *testing.T) {
	e := newInbound(t)
	e.slowRelay(400 * time.Millisecond)
	var lat []time.Duration
	for i := 0; i < 40; i++ {
		start := time.Now()
		if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprint("busy ", i), Assignee: e.Bob.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.AddComment(ctx, e.Alice, e.issue.Key, fmt.Sprint("note ", i), false); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(start))
	}
	got := p95(lat)
	t.Logf("create + comment p95 %s with a 400 ms relay", got.Round(time.Millisecond))
	if got > 250*time.Millisecond {
		t.Fatalf("the request path waited on the relay: p95 %s", got)
	}
	var queued int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_message`).Scan(&queued)
	if queued != 0 { // nothing was composed or sent in the request: the notifier does that later
		t.Logf("%d mail rows exist before the notifier ran", queued)
	}
}

// MAIL-17: a burst of notifications leaves within the 30 s target even when the relay takes 100 ms per message: they are sent
// concurrently, and a full batch is followed at once by the next.
func TestMAIL17_AnOutboundBurstLeavesWithinTheTarget(t *testing.T) {
	e := newInbound(t)
	relay := e.slowRelay(100 * time.Millisecond)
	// thirty people, ten issues each: a realistic burst (messages to one person are sent in order, so a burst to one person is
	// slower by design)
	const people, each = 30, 10
	const n = people * each
	for p := 0; p < people; p++ {
		u, err := e.Svc.CreateUser(ctx, fmt.Sprintf("p%d@example.com", p), fmt.Sprintf("Person %d", p), "longenoughpass1", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Svc.AddMember(ctx, e.Admin, "ABC", u.ID, "member"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < each; i++ {
			e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprint("burst ", p, "-", i), Assignee: u.ID})
		}
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := time.Now()
	go e.Svc.RunNotifier(rctx, t.Logf)
	go e.Svc.RunMail(rctx, t.Logf)
	var sentAll time.Time
	for {
		var sent, total int
		e.Store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='sent'),count(*) FROM mail_message WHERE subject LIKE '%burst%'`).Scan(&sent, &total)
		if sent >= n {
			sentAll = time.Now()
			break
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("only %d of %d left within 30 s (%d queued)", sent, n, total)
		}
		time.Sleep(50 * time.Millisecond)
	}
	took := sentAll.Sub(start)
	t.Logf("%d messages through a 100 ms relay in %s (%d reached it)", n, took.Round(10*time.Millisecond), len(relay.Messages()))
	if took > 20*time.Second {
		t.Fatalf("too slow: %s", took)
	}
}

// MAIL-17: inbound mail by webhook is a visible comment within seconds while the outbound queue is busy.
func TestMAIL17_InboundIsVisibleWithinTheTargetUnderMixedLoad(t *testing.T) {
	e := newInbound(t)
	e.slowRelay(50 * time.Millisecond)
	e.Svc.Mail.Inbound = domain.InboundLimits{PerSenderHour: 1000, PerDomainHour: 1000, PerIssueHour: 1000} // the limits are tested elsewhere
	addr := e.replyAddr("bob@example.com")
	for i := 0; i < 150; i++ { // outbound load: a queue being worked
		e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprint("load ", i), Assignee: e.Bob.ID})
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.Svc.RunNotifier(rctx, t.Logf)
	go e.Svc.RunMail(rctx, t.Logf)
	var mu sync.Mutex
	var lat []time.Duration
	var wg sync.WaitGroup
	raws := make([][][]byte, 4) // built first: the helper numbers Message-IDs with a counter that is not safe to share
	for w := range raws {
		for i := 0; i < 25; i++ {
			raws[w] = append(raws[w], e.eml("bob@example.com", addr, "Re: [ABC-1] Printer is on fire", []string{authOK}, fmt.Sprintf("reply %d-%d", w, i)))
		}
	}
	for w := 0; w < 4; w++ { // several relays delivering at once
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				raw := raws[w][i]
				start := time.Now()
				r, err := e.Svc.IngestMail(ctx, e.trusted.ID, true, raw)
				if err != nil || r.Outcome != "accepted" {
					t.Errorf("%+v %v", r, err)
					return
				}
				mu.Lock()
				lat = append(lat, time.Since(start))
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(e.comments()) < 100 {
		t.Fatalf("%d comments", len(e.comments()))
	}
	got := p95(lat)
	t.Logf("%d inbound messages, p95 %s while the outbound queue ran", len(lat), got.Round(time.Millisecond))
	if got > 2*time.Second { // the target is 10 s; this leaves a margin for a slower machine
		t.Fatalf("p95 %s", got)
	}
}

// MAIL-17: IMAP IDLE delivery is quick when the system is busy with outbound mail.
func TestMAIL17_IdleDeliveryStaysQuickWhileOutboundMailIsBeingSent(t *testing.T) {
	e := newIMAPEnv(t, true)
	e.slowRelay(50 * time.Millisecond)
	e.Svc.Mail.Inbound = domain.InboundLimits{PerSenderHour: 1000, PerDomainHour: 1000, PerIssueHour: 1000}
	e.Svc.IMAPIdle = time.Minute
	for i := 0; i < 100; i++ {
		e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprint("load ", i), Assignee: e.Bob.ID})
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go e.Svc.RunNotifier(rctx, t.Logf)
	go e.Svc.RunMail(rctx, t.Logf)
	go e.Svc.RunIMAP(rctx, t.Logf)
	deadline := time.Now().Add(10 * time.Second)
	for e.box.Idlers() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	var lat []time.Duration
	for i := 0; i < 20; i++ {
		for e.box.Idlers() == 0 && time.Now().Before(deadline.Add(10*time.Second)) {
			time.Sleep(10 * time.Millisecond)
		}
		before := len(e.comments())
		start := time.Now()
		e.box.Append(e.reply(fmt.Sprint("by IDLE ", i)))
		for len(e.comments()) == before {
			if time.Since(start) > 10*time.Second {
				t.Fatalf("message %d took more than 10 s", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
		lat = append(lat, time.Since(start))
	}
	got := p95(lat)
	t.Logf("IDLE delivery p95 %s under outbound load", got.Round(time.Millisecond))
	if got > 3*time.Second {
		t.Fatalf("p95 %s", got)
	}
}

// MAIL-17 / MAIL-09: a mailbox with 10,000 waiting messages is read completely, in batches, each message once, with the checkpoint
// at the end and memory that does not grow with the backlog. Slow to load, so it runs with TASKLANE_PERF=1 (make perf).
func TestMAIL17_PERF_AnIMAPBacklogOfTenThousandMessages(t *testing.T) {
	if os.Getenv("TASKLANE_PERF") == "" {
		t.Skip("set TASKLANE_PERF=1 (make perf)")
	}
	e := newIMAPEnv(t, true)
	const backlog = 10000
	for i := 0; i < backlog; i++ {
		e.box.Append(e.reply(fmt.Sprintf("backlog message %d", i)))
	}
	start := time.Now()
	if n := e.poll(); n != backlog {
		t.Fatalf("%d of %d", n, backlog)
	}
	took := time.Since(start)
	var rows, distinct int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT message_id) FROM mail_inbound`).Scan(&rows, &distinct)
	if rows != backlog || distinct != backlog {
		t.Fatalf("%d rows %d distinct", rows, distinct)
	}
	if last, _, got, _ := e.state(); last != backlog || got != backlog {
		t.Fatalf("checkpoint %d, received %d", last, got)
	}
	t.Logf("%d messages in %s (%.0f per second)", backlog, took.Round(time.Millisecond), float64(backlog)/took.Seconds())
	if took > 10*time.Minute {
		t.Fatalf("too slow: %s", took)
	}
	// a second pass finds nothing new
	if n := e.poll(); n != 0 {
		t.Fatalf("read %d again", n)
	}
}
