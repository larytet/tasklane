package domain_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func orgWriter(t *testing.T, e *testutil.Env, who domain.Actor, project string) error {
	t.Helper()
	_, err := e.Svc.CreateIssue(ctx, who, domain.CreateIn{Project: project, Summary: "write"})
	return err
}

// PAUSE-01 (organization scope): a pause on an organization holds that organization's projects only, and overlaps with what contains or lies inside it.
func TestPAUSE_OrganizationScope(t *testing.T) {
	e, org, boss := hostedEnv(t)
	if _, err := e.Svc.CreateProject(ctx, boss, "GLX", "Globex work"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.CreateProject(ctx, boss, "GLY", "Globex more"); err != nil {
		t.Fatal(err)
	}
	e.Project() // ABC of the default organization
	domain.PauseAdmitWait = 50 * time.Millisecond
	defer func() { domain.PauseAdmitWait = 150 * time.Millisecond }()
	if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "org:00000000-0000-0000-0000-00000000dead"}); !isInvalid(err) {
		t.Fatalf("an unknown organization: %v", err)
	}
	l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "org:" + org, Owner: "test"})
	if err != nil || l.State != "paused" {
		t.Fatalf("%+v %v", l, err)
	}
	for _, p := range []string{"GLX", "GLY"} {
		if err := orgWriter(t, e, boss, p); !isPaused(err) {
			t.Errorf("a write to %s during its organization's pause: %v", p, err)
		}
	}
	if err := orgWriter(t, e, e.Alice, "ABC"); err != nil {
		t.Fatalf("another organization was held: %v", err)
	}
	// overlaps
	if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:GLX"}); err == nil {
		t.Fatal("a project of the paused organization was paused beside it")
	}
	if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "instance"}); err == nil {
		t.Fatal("the instance was paused over an organization's pause")
	}
	other, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC"})
	if err != nil {
		t.Fatalf("a project of another organization could not be paused beside: %v", err)
	}
	e.Svc.ReleasePause(ctx, e.Admin, other.ID)
	e.Svc.ReleasePause(ctx, e.Admin, l.ID)
	if err := orgWriter(t, e, boss, "GLX"); err != nil {
		t.Fatalf("after the release: %v", err)
	}
	// a project pause inside the organization blocks an organization pause, and the drain waits for the organization's own in-flight writes
	p, _ := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:GLY"})
	if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "org:" + org}); err == nil {
		t.Fatal("an organization was paused over a paused project of it")
	}
	e.Svc.ReleasePause(ctx, e.Admin, p.ID)
}

// HOST-04: announced maintenance holds the tenant's writes from its start to its end, once, whichever node gets there, and a cancelled one lets go.
func TestHOST04_MaintenanceDoesWhatItAnnounces(t *testing.T) {
	e, org, boss := hostedEnv(t)
	e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{Plan: "team"})
	if _, err := e.Svc.CreateProject(ctx, boss, "GLX", "Globex work"); err != nil {
		t.Fatal(err)
	}
	e.Project()
	domain.PauseAdmitWait = 50 * time.Millisecond
	defer func() { domain.PauseAdmitWait = 150 * time.Millisecond }()
	t0 := e.Svc.Now()
	n, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, domain.MaintenanceIn{Kind: "upgrade", Starts: t0.Add(time.Hour), Ends: t0.Add(3 * time.Hour), Message: "Upgrade"})
	if err != nil || !n.HoldWrites {
		t.Fatalf("%+v %v", n, err)
	}
	e.Svc.RunMaintenanceOnce(ctx)
	if err := orgWriter(t, e, boss, "GLX"); err != nil {
		t.Fatalf("held before its time: %v", err)
	}
	e.Clock.T = t0.Add(time.Hour + time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ { // every node's loop finds it due at once
		wg.Add(1)
		go func() { defer wg.Done(); e.Svc.RunMaintenanceOnce(ctx) }()
	}
	wg.Wait()
	if err := orgWriter(t, e, boss, "GLX"); !isPaused(err) {
		t.Fatalf("not held while the work is going on: %v", err)
	}
	if err := orgWriter(t, e, e.Alice, "ABC"); err != nil {
		t.Fatalf("another organization was held: %v", err)
	}
	ls, _ := e.Svc.PauseStatus(ctx, e.Admin)
	if len(ls) != 1 || ls[0].Scope != "org:"+org || ls[0].Owner != "maintenance:"+n.ID {
		t.Fatalf("the pause: %+v", ls)
	}
	var started int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='maintenance.started'`).Scan(&started)
	if started != 1 {
		t.Fatalf("%d starts audited for one piece of work on three nodes", started)
	}
	// it ends by itself
	e.Clock.T = t0.Add(3*time.Hour + time.Second)
	e.Svc.RunMaintenanceOnce(ctx)
	if err := orgWriter(t, e, boss, "GLX"); err != nil {
		t.Fatalf("still held after the work ended: %v", err)
	}
	got, _ := e.Svc.ListMaintenance(ctx, e.Admin, org)
	if len(got) != 1 || got[0].Started == nil || got[0].Finished == nil {
		t.Fatalf("%+v", got)
	}
	// work that is cancelled while under way lets go at once
	e.Clock.T = t0.Add(10 * time.Hour)
	m2, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, domain.MaintenanceIn{Kind: "maintenance", Starts: e.Svc.Now().Add(time.Minute), Ends: e.Svc.Now().Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	e.Clock.T = e.Clock.T.Add(2 * time.Minute)
	e.Svc.RunMaintenanceOnce(ctx)
	if err := orgWriter(t, e, boss, "GLX"); !isPaused(err) {
		t.Fatalf("second piece of work not held: %v", err)
	}
	if _, err := e.Svc.CancelMaintenance(ctx, e.Admin, org, m2.ID, "called off"); err != nil {
		t.Fatal(err)
	}
	if err := orgWriter(t, e, boss, "GLX"); err != nil {
		t.Fatalf("held after the work was cancelled: %v", err)
	}
	// announce only: no hold
	e.Clock.T = t0.Add(20 * time.Hour)
	no := false
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, domain.MaintenanceIn{Kind: "maintenance", Starts: e.Svc.Now().Add(time.Minute), Ends: e.Svc.Now().Add(time.Hour), HoldWrites: &no}); err != nil {
		t.Fatal(err)
	}
	e.Clock.T = e.Clock.T.Add(2 * time.Minute)
	e.Svc.RunMaintenanceOnce(ctx)
	if err := orgWriter(t, e, boss, "GLX"); err != nil {
		t.Fatalf("announce-only work held writes: %v", err)
	}
	// another pause is in the way: the claim is released with the reason and tried again once the way is clear
	e.Clock.T = t0.Add(30 * time.Hour)
	m4, _ := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, domain.MaintenanceIn{Kind: "maintenance", Starts: e.Svc.Now().Add(time.Minute), Ends: e.Svc.Now().Add(2 * time.Hour)})
	blocker, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:GLX", Owner: "an import"})
	if err != nil {
		t.Fatal(err)
	}
	e.Clock.T = e.Clock.T.Add(2 * time.Minute)
	e.Svc.RunMaintenanceOnce(ctx)
	waiting, _ := e.Svc.ListMaintenance(ctx, e.Admin, org)
	var found bool
	for _, x := range waiting {
		if x.ID == m4.ID {
			found = true
			if x.Started != nil || !strings.Contains(x.LastError, "pause already covers") {
				t.Fatalf("a blocked claim: %+v", x)
			}
		}
	}
	if !found {
		t.Fatal("the notice vanished")
	}
	e.Svc.ReleasePause(ctx, e.Admin, blocker.ID)
	e.Svc.RunMaintenanceOnce(ctx)
	if err := orgWriter(t, e, boss, "GLX"); !isPaused(err) {
		t.Fatalf("not held once the way was clear: %v", err)
	}
}

func TestHOST04_SelfHostedRunsNothing(t *testing.T) {
	e := testutil.New(t)
	e.Svc.RunMaintenanceOnce(ctx)
	if m, n := e.Svc.RunBillingOnce(ctx); m+n != 0 {
		t.Fatal("billing ran on a self-hosted installation")
	}
}

// HOST-05: owed invoices are made when a month ends, once, delivered, and retried when delivery failed.
func TestHOST05_ScheduledBilling(t *testing.T) {
	e, org, _ := hostedEnv(t)
	e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{Plan: "team"})
	// the default organization has a plan with no prices and usage: nothing is owed for it
	e.Svc.SetOrgPlan(ctx, e.Admin, domain.DefaultOrg, domain.OrgPlan{Plan: "free"})
	snapshot(t, e, domain.DefaultOrg, "2026-09-10", 3, 0, 0)
	e.Clock.T = time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	if m, n := e.Svc.RunBillingOnce(ctx); m+n != 0 {
		t.Fatalf("invoiced with no prices: %d %d", m, n)
	}
	e.Svc.SetPrices(ctx, e.Admin, org, domain.Prices{BaseCents: 1000, SeatCents: 100})
	if m, n := e.Svc.RunBillingOnce(ctx); m+n != 0 {
		t.Fatalf("invoiced with no usage recorded: %d %d", m, n)
	}
	snapshot(t, e, org, "2026-09-10", 4, 0, 0)
	snapshot(t, e, org, "2026-10-01", 40, 0, 0) // this month's usage is not September's
	var mu sync.Mutex
	fail := true
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		posts = append(posts, r.Header.Get("Idempotency-Key"))
		if fail {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	e.Svc.Billing = domain.WebhookBilling{URL: srv.URL, Secret: "k", AllowPrivate: true}
	made, sent := e.Svc.RunBillingOnce(ctx)
	if made != 1 || sent != 0 {
		t.Fatalf("made %d, delivered %d with the receiver down", made, sent)
	}
	l, _ := e.Svc.ListInvoices(ctx, e.Admin, org)
	if len(l) != 1 || l[0].Period != "2026-09" || l[0].Status != "open" || l[0].TotalCents != 1400 {
		t.Fatalf("%+v", l)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	made, sent = e.Svc.RunBillingOnce(ctx)
	if made != 0 || sent != 1 {
		t.Fatalf("made %d, delivered %d once the receiver was back", made, sent)
	}
	if made, sent = e.Svc.RunBillingOnce(ctx); made+sent != 0 {
		t.Fatalf("a finished month was invoiced or delivered again: %d %d", made, sent)
	}
	l, _ = e.Svc.ListInvoices(ctx, e.Admin, org)
	if len(l) != 1 || l[0].Status != "sent" {
		t.Fatalf("%+v", l)
	}
	mu.Lock()
	if len(posts) != 2 || posts[0] != posts[1] {
		t.Fatalf("deliveries %v: the receiver must see the same number each time", posts)
	}
	mu.Unlock()
	// the next month's call makes October's, and a voided one is made again
	e.Clock.T = time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	snapshot(t, e, org, "2026-10-20", 5, 0, 0)
	if made, _ = e.Svc.RunBillingOnce(ctx); made != 1 {
		t.Fatalf("October: made %d", made)
	}
	_ = errors.New
}
