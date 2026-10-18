package domain_test

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

var prague, _ = time.LoadLocation("Europe/Prague")

func pg(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, prague) }

// slaRig: a portal project with a request type carrying a policy, and helpers to move the clock and read a request's clocks.
type slaRig struct {
	*portal
	rt domain.RequestType
}

func newSLA(t *testing.T, start time.Time, policy domain.SLAPolicy) *slaRig {
	p := newPortal(t)
	p.Clock.T = start // before anyone signs in: sessions and codes are in the same time
	n := "Support"
	rt, err := p.Svc.CreateRequestType(ctx, p.Admin, "ABC", domain.RequestTypeIn{Name: &n, SLA: &policy})
	if err != nil {
		t.Fatal(err)
	}
	return &slaRig{portal: p, rt: rt}
}

func (r *slaRig) request(t *testing.T) (key string, c domain.Customer) {
	t.Helper()
	c = r.signIn(t, "cora@example.com")
	pr, err := r.Svc.PortalCreateRequest(ctx, c, "ABC", domain.PortalRequestIn{Type: r.rt.ID, Summary: "Help"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	return pr.Key, c
}

func (r *slaRig) clocks(t *testing.T, key string) map[string]domain.SLAClock {
	t.Helper()
	if err := r.Svc.RefreshSLA(ctx, key); err != nil {
		t.Fatal(err)
	}
	v, err := r.Svc.View(ctx, r.Alice, key)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]domain.SLAClock{}
	for _, c := range v.SLA {
		out[c.Kind] = c
	}
	return out
}

// reply: a public reply by staff at the given time (the comment's own timestamp is the database's, so it is set to the test's time).
func (r *slaRig) reply(t *testing.T, key string, who domain.Actor, internal bool, when time.Time) {
	t.Helper()
	r.Clock.T = when
	c, err := r.Svc.AddComment(ctx, who, key, "a reply", internal)
	if err != nil {
		t.Fatal(err)
	}
	r.Store.Pool.Exec(ctx, `UPDATE comment SET created_at=$2 WHERE id::text=$1`, c.ID, when)
	if err := r.Svc.RefreshSLA(ctx, key); err != nil {
		t.Fatal(err)
	}
}

func (r *slaRig) move(t *testing.T, key, name string, when time.Time) {
	t.Helper()
	r.Clock.T = when
	id, ok := trans(t, r.Env, r.Alice, key, name)
	if !ok {
		t.Fatalf("no transition %q", name)
	}
	if _, err := r.Svc.Transition(ctx, r.Alice, key, id, nil); err != nil {
		t.Fatal(err)
	}
}

func within(a *time.Time, b time.Time) bool {
	return a != nil && a.Sub(b) < time.Second && b.Sub(*a) < time.Second
}

// PORT-08 / SLA01: the policy is checked: goals, calendar, stopping statuses; only project administrators set it.
func TestSLA01_PolicyValidation(t *testing.T) {
	p := newPortal(t)
	mk := func(a domain.Actor, pol domain.SLAPolicy) error {
		n := "Form " + strings.Repeat("x", len(pol.PauseStatuses)) + time.Now().String()
		_, err := p.Svc.CreateRequestType(ctx, a, "ABC", domain.RequestTypeIn{Name: &n, SLA: &pol})
		return err
	}
	if err := mk(p.Admin, domain.SLAPolicy{FirstResponse: 30, Resolution: 480, Calendar: &domain.SLACalendar{TimeZone: "Europe/Prague"}, PauseStatuses: []string{"in progress"}}); err != nil {
		t.Fatalf("a good policy: %v", err)
	}
	for name, pol := range map[string]domain.SLAPolicy{
		"negative":      {FirstResponse: -1},
		"too long":      {Resolution: 60*24*365 + 1},
		"first > final": {FirstResponse: 600, Resolution: 60},
		"zone":          {FirstResponse: 30, Calendar: &domain.SLACalendar{TimeZone: "Mars/Olympus"}},
		"preset":        {FirstResponse: 30, Calendar: &domain.SLACalendar{TimeZone: "UTC", Preset: "mon-sun"}},
		"start":         {FirstResponse: 30, Calendar: &domain.SLACalendar{TimeZone: "UTC", Start: "9am"}},
		"past midnight": {FirstResponse: 30, Calendar: &domain.SLACalendar{TimeZone: "UTC", Start: "20:00"}},
		"holiday":       {FirstResponse: 30, Calendar: &domain.SLACalendar{TimeZone: "UTC", Holidays: []string{"tomorrow"}}},
		"status":        {FirstResponse: 30, PauseStatuses: []string{"Waiting for customer"}},
	} {
		if err := mk(p.Admin, pol); !isInvalid(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := mk(p.Alice, domain.SLAPolicy{FirstResponse: 30}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member set a policy: %v", err)
	}
}

// SLA02: a working calendar, a weekend, the first response met by a public reply (never by a note or by the customer), one notice per breach.
func TestSLA02_CalendarFirstResponseAndBreach(t *testing.T) {
	r := newSLA(t, pg(2026, 3, 6, 16, 0), domain.SLAPolicy{FirstResponse: 120, Resolution: 480, Calendar: &domain.SLACalendar{TimeZone: "Europe/Prague", Start: "09:00"}})
	key, c := r.request(t)
	if _, err := r.Svc.UpdateFields(ctx, r.Alice, key, nil, false, domain.Changes{Assignee: &r.Alice.ID}); err != nil {
		t.Fatal(err)
	}
	cl := r.clocks(t, key)
	if cl["first_response"].State != "running" || !within(cl["first_response"].Due, pg(2026, 3, 9, 10, 0)) {
		t.Fatalf("first response: %+v", cl["first_response"])
	}
	if !within(cl["resolution"].Due, pg(2026, 3, 9, 16, 0)) {
		t.Fatalf("resolution due: %+v", cl["resolution"].Due)
	}
	// over the weekend nothing is counted
	r.Clock.T = pg(2026, 3, 8, 12, 0)
	if cl = r.clocks(t, key); cl["first_response"].State != "running" || cl["first_response"].Elapsed != 60 {
		t.Fatalf("on Sunday: %+v", cl["first_response"])
	}
	// a note, the customer's own comment and the hidden portal account do not answer the customer
	r.reply(t, key, r.Alice, true, pg(2026, 3, 9, 9, 10))
	if _, err := r.Svc.PortalComment(ctx, c, "ABC", key, "hello?"); err != nil {
		t.Fatal(err)
	}
	if cl = r.clocks(t, key); cl["first_response"].State != "running" {
		t.Fatalf("a note or a customer comment met the goal: %+v", cl["first_response"])
	}
	// a public reply by staff does
	r.reply(t, key, r.Alice, false, pg(2026, 3, 9, 9, 30))
	cl = r.clocks(t, key)
	if cl["first_response"].State != "met" || cl["first_response"].Late || !within(cl["first_response"].Met, pg(2026, 3, 9, 9, 30)) {
		t.Fatalf("first response: %+v", cl["first_response"])
	}
	// the resolution goal passes (16:00): breached once, noticed once
	r.Clock.T = pg(2026, 3, 9, 16, 1)
	if n, err := r.Svc.SLASweep(ctx); err != nil || n == 0 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if cl = r.clocks(t, key); cl["resolution"].State != "breached" || cl["resolution"].BreachedAt == nil || !within(cl["resolution"].BreachedAt, pg(2026, 3, 9, 16, 0)) {
		t.Fatalf("resolution: %+v", cl["resolution"])
	}
	r.Svc.SLASweep(ctx)
	notify(t, r.Env)
	n := 0
	for _, x := range inboxOf(t, r.Env, r.Alice, "") {
		if x.Kind == "sla" && strings.Contains(x.What, "resolution goal was missed") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("breach notices for the assignee: %d", n)
	}
	// resolved late: met, marked late, no second notice
	r.move(t, key, "Resolve", pg(2026, 3, 9, 17, 30))
	cl = r.clocks(t, key)
	if cl["resolution"].State != "met" || !cl["resolution"].Late {
		t.Fatalf("resolved late: %+v", cl["resolution"])
	}
	r.Svc.SLASweep(ctx)
	notify(t, r.Env)
	n = 0
	for _, x := range inboxOf(t, r.Env, r.Alice, "") {
		if x.Kind == "sla" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("notices after resolving: %d", n)
	}
}

// SLA03: a stopping status stops the clock and starting again moves the due time; resolving meets the goal; a reopened request is not
// charged for the time it sat resolved.
func TestSLA03_PauseResumeAndReopen(t *testing.T) {
	r := newSLA(t, pg(2026, 4, 7, 10, 0), domain.SLAPolicy{Resolution: 240, PauseStatuses: []string{"In Progress"}}) // every minute counts
	key, _ := r.request(t)
	cl := r.clocks(t, key)
	if cl["resolution"].State != "running" || !within(cl["resolution"].Due, pg(2026, 4, 7, 14, 0)) {
		t.Fatalf("started: %+v", cl["resolution"])
	}
	if _, ok := cl["first_response"]; ok {
		t.Fatal("a clock for a goal the policy does not have")
	}
	r.move(t, key, "Start progress", pg(2026, 4, 7, 11, 0)) // stops after 60 minutes
	r.Clock.T = pg(2026, 4, 7, 13, 0)
	if cl = r.clocks(t, key); cl["resolution"].State != "paused" || cl["resolution"].Elapsed != 60 || cl["resolution"].Due != nil {
		t.Fatalf("paused: %+v", cl["resolution"])
	}
	// a paused clock does not breach, however long
	r.Clock.T = pg(2026, 4, 9, 9, 0)
	if cl = r.clocks(t, key); cl["resolution"].State != "paused" {
		t.Fatalf("a paused clock moved: %+v", cl["resolution"])
	}
	r.move(t, key, "Back to To Do", pg(2026, 4, 9, 14, 0)) // runs again, 180 minutes left
	r.Clock.T = pg(2026, 4, 9, 15, 0)
	if cl = r.clocks(t, key); cl["resolution"].State != "running" || cl["resolution"].Elapsed != 120 || !within(cl["resolution"].Due, pg(2026, 4, 9, 17, 0)) {
		t.Fatalf("running again: %+v", cl["resolution"])
	}
	// resolved in time: met; the first response is not a goal here
	r.move(t, key, "Resolve", pg(2026, 4, 9, 16, 0))
	cl = r.clocks(t, key)
	if cl["resolution"].State != "met" || cl["resolution"].Late {
		t.Fatalf("resolved: %+v", cl["resolution"])
	}
	// reopened a day later: the day it sat resolved is not counted
	r.move(t, key, "Reopen", pg(2026, 4, 10, 16, 0))
	r.Clock.T = pg(2026, 4, 10, 16, 30)
	cl = r.clocks(t, key)
	if cl["resolution"].State != "running" || cl["resolution"].Elapsed != 210 || !within(cl["resolution"].Due, pg(2026, 4, 10, 17, 0)) {
		t.Fatalf("reopened: %+v", cl["resolution"])
	}
	r.Clock.T = pg(2026, 4, 10, 17, 5)
	if cl = r.clocks(t, key); cl["resolution"].State != "breached" {
		t.Fatalf("breached after reopening: %+v", cl["resolution"])
	}
}

// SLA04: resolving without a reply answers the customer; a policy change leaves running clocks alone; clocks are in search.
func TestSLA04_ResolveAnswersSnapshotAndSearch(t *testing.T) {
	r := newSLA(t, pg(2026, 5, 4, 10, 0), domain.SLAPolicy{FirstResponse: 30, Resolution: 600})
	key, _ := r.request(t)
	other, _ := r.request(t)
	r.move(t, key, "Resolve", pg(2026, 5, 4, 10, 20))
	if cl := r.clocks(t, key); cl["first_response"].State != "met" || cl["first_response"].Late {
		t.Fatalf("resolving without a reply: %+v", cl["first_response"])
	}
	// the policy is changed: the request that is running keeps the goals it began under
	pol := domain.SLAPolicy{FirstResponse: 5, Resolution: 10}
	if _, err := r.Svc.UpdateRequestType(ctx, r.Admin, r.rt.ID, domain.RequestTypeIn{SLA: &pol}); err != nil {
		t.Fatal(err)
	}
	if cl := r.clocks(t, other); cl["first_response"].Goal != 30 || cl["resolution"].Goal != 600 {
		t.Fatalf("a running clock followed a new policy: %+v", cl)
	}
	// search sees the stored state
	r.Clock.T = pg(2026, 5, 4, 10, 40)
	r.Svc.SLASweep(ctx)
	for q, want := range map[string][]string{
		`sla.firstResponse.state = breached`: {other},
		`sla.firstResponse.state = met`:      {key},
		`sla.state = running`:                {other},
		`sla.state in (breached, met)`:       {other, key},
		`sla.resolution.state = met`:         {key},
	} {
		got := r.keysViaSearch(t, q)
		if strings.Join(got, ",") != strings.Join(sortedCopy(want), ",") {
			t.Fatalf("%s: got %v, want %v", q, got, sortedCopy(want))
		}
	}
	if _, err := r.keysErr(`sla.state = late`); err == nil {
		t.Fatal("an unknown state was accepted")
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func (r *slaRig) keysErr(q string) ([]string, error) {
	pg := &search.Postgres{DB: r.Store.Pool}
	all, rd, wr, err := r.Svc.SearchScope(ctx, r.Admin)
	if err != nil {
		return nil, err
	}
	ks, err := pg.Keys(ctx, search.Scope{UserID: r.Admin.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: r.Svc.Now}, q, 100)
	sort.Strings(ks)
	return ks, err
}

func (r *slaRig) keysViaSearch(t *testing.T, q string) []string {
	t.Helper()
	ks, err := r.keysErr(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return ks
}
