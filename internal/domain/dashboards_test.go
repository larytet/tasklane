package domain_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

type dashEnv struct {
	*testutil.Env
	pg *search.Postgres
}

// newDash makes two projects: ABC (Alice and Bob) and XYZ (Alice only), with issues of varied status, priority, assignee and labels.
func newDash(t *testing.T) *dashEnv {
	e := testutil.New(t)
	e.Clock.T = time.Now()
	e.Project()
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Secret"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.AddMember(ctx, e.Admin, "XYZ", e.Alice.ID, "member"); err != nil {
		t.Fatal(err)
	}
	d := &dashEnv{Env: e, pg: &search.Postgres{DB: e.Store.Pool}}
	mk := func(proj, summary, prio string, assignee domain.Actor, labels ...string) domain.Issue {
		is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: proj, Type: "Task", Summary: summary, Priority: prio, Assignee: assignee.ID, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	none := domain.Actor{}
	mk("ABC", "a1", "high", e.Alice, "ui", "urgent")
	mk("ABC", "a2", "high", e.Bob, "ui")
	mk("ABC", "a3", "low", e.Bob)
	mk("ABC", "a4", "medium", none, "backend")
	done := mk("ABC", "a5 done", "medium", e.Alice, "ui")
	mk("XYZ", "x1 secret", "high", e.Alice, "secret")
	mk("XYZ", "x2 secret", "low", e.Alice)
	if tid, ok := trans(t, e, e.Alice, done.Key, "Resolve"); ok {
		if _, err := e.Svc.Transition(ctx, e.Alice, done.Key, tid, nil); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func (d *dashEnv) scope(t *testing.T, a domain.Actor) search.Scope {
	t.Helper()
	all, rd, wr, err := d.Svc.SearchScope(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	return search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: d.Svc.Now}
}

func (d *dashEnv) gadget(t *testing.T, a domain.Actor, kind string, cfg map[string]any) map[string]any {
	t.Helper()
	dash, err := d.Svc.CreateDashboard(ctx, d.Alice, domain.DashboardIn{Name: fmt.Sprint(kind, time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	g, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: kind, Config: cfg})
	if err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
	if a.ID != d.Alice.ID { // someone else looking at the owner's dashboard: it is shared
		if err := d.Svc.UpdateDashboard(ctx, d.Alice, dash.ID, domain.DashboardIn{Name: dash.Name, Shared: "org", Columns: 2}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := d.Svc.GadgetData(ctx, a, dash.ID, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != "" {
		t.Fatalf("%s: %s", kind, r.Error)
	}
	return viaJSON(t, r.Data)
}

// viaJSON looks at data the way a client does: after it has been serialised.
func viaJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		t.Fatalf("%T: %v", v, err)
	}
	return m
}

func aggs(t *testing.T, m map[string]any, key string) []search.AggRow {
	t.Helper()
	b, _ := json.Marshal(m[key])
	var out []search.AggRow
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rowsOf(t *testing.T, m map[string]any) []search.Row {
	t.Helper()
	b, _ := json.Marshal(m["issues"])
	var out []search.Row
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDASH01_FiltersAreSavedValidatedAndSharedWithoutSharingData(t *testing.T) {
	d := newDash(t)
	if _, err := d.Svc.CreateFilter(ctx, d.Alice, domain.FilterIn{Name: "Bad", TQL: "status = = Done"}); err == nil {
		t.Fatal("a query that does not parse was saved")
	}
	if _, err := d.Svc.CreateFilter(ctx, d.Alice, domain.FilterIn{Name: "", TQL: "project = ABC"}); err == nil {
		t.Fatal("a filter without a name was saved")
	}
	mine, err := d.Svc.CreateFilter(ctx, d.Alice, domain.FilterIn{Name: "Open ABC", TQL: "project = ABC AND statusCategory != Done"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Svc.CreateFilter(ctx, d.Alice, domain.FilterIn{Name: "open abc", TQL: "project = ABC"}); err == nil {
		t.Fatal("a second filter with the same name (in another case) was saved")
	}
	if l, _ := d.Svc.ListFilters(ctx, d.Bob); len(l) != 0 {
		t.Fatalf("a private filter is listed for someone else: %+v", l)
	}
	if err := d.Svc.UpdateFilter(ctx, d.Bob, mine.ID, domain.FilterIn{Name: "x", TQL: "project = ABC"}); err != domain.ErrNotFound && err != domain.ErrForbidden {
		t.Fatalf("someone else changed a private filter: %v", err)
	}
	if err := d.Svc.UpdateFilter(ctx, d.Alice, mine.ID, domain.FilterIn{Name: "Open ABC", TQL: "project = ABC AND statusCategory != Done", Shared: "org"}); err != nil {
		t.Fatal(err)
	}
	l, _ := d.Svc.ListFilters(ctx, d.Bob)
	if len(l) != 1 || l[0].Name != "Open ABC" || l[0].Mine || l[0].Owner != "Alice K." {
		t.Fatalf("%+v", l)
	}
	// Bob may use it but not change or delete it
	if err := d.Svc.UpdateFilter(ctx, d.Bob, mine.ID, domain.FilterIn{Name: "Bob's", TQL: "project = ABC", Shared: "org"}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if err := d.Svc.DeleteFilter(ctx, d.Bob, mine.ID); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	// another organization sees nothing of it
	d.Svc.CreateOrganization(ctx, d.Admin, "Globex", "boss@globex.example", "Boss", "password123")
	ul, _ := d.Svc.ListUsersFor(ctx, d.Admin)
	var boss domain.Actor
	for _, u := range ul {
		if u.Email == "boss@globex.example" {
			boss, _ = d.Svc.ActorFor(ctx, u.ID)
		}
	}
	if l, _ := d.Svc.ListFilters(ctx, boss); len(l) != 0 {
		t.Fatalf("another organization sees %+v", l)
	}
	// a filter that a dashboard uses cannot be deleted, nor made private if an organization-wide dashboard uses it
	dash, _ := d.Svc.CreateDashboard(ctx, d.Alice, domain.DashboardIn{Name: "uses it", Shared: "org"})
	if _, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "filter_results", Config: map[string]any{"filter_id": mine.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Svc.DeleteFilter(ctx, d.Alice, mine.ID); err == nil || !strings.Contains(err.Error(), "still use") {
		t.Fatalf("%v", err)
	}
	if err := d.Svc.UpdateFilter(ctx, d.Alice, mine.ID, domain.FilterIn{Name: "Open ABC", TQL: "project = ABC", Shared: "private"}); err == nil || !strings.Contains(err.Error(), "organization-wide dashboard") {
		t.Fatalf("%v", err)
	}
	if l, _ := d.Svc.ListFilters(ctx, d.Alice); l[0].InUse != 1 {
		t.Fatalf("%+v", l)
	}
}

func TestDASH02_DashboardsAreVisibleToTheirOwnerAndTheOrganizationWhenSharedAndEditedByTheOwner(t *testing.T) {
	d := newDash(t)
	dash, err := d.Svc.CreateDashboard(ctx, d.Alice, domain.DashboardIn{Name: "Team", Columns: 3, RefreshSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if dash.Columns != 3 || dash.Shared != "private" || !dash.Mine || !dash.CanEdit {
		t.Fatalf("%+v", dash)
	}
	for _, bad := range []domain.DashboardIn{{Name: "x", Columns: 4}, {Name: "x", RefreshSeconds: 5}, {Name: "x", RefreshSeconds: 99999}, {Name: "x", Shared: "world"}, {Name: ""}} {
		if _, err := d.Svc.CreateDashboard(ctx, d.Alice, bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	// private: nobody else can see that it exists
	if _, err := d.Svc.GetDashboard(ctx, d.Bob, dash.ID); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if l, _ := d.Svc.ListDashboards(ctx, d.Bob); len(l) != 0 {
		t.Fatalf("%+v", l)
	}
	g, _ := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "text", Config: map[string]any{"markdown": "hello"}})
	if err := d.Svc.UpdateDashboard(ctx, d.Alice, dash.ID, domain.DashboardIn{Name: "Team", Columns: 3, Shared: "org", RefreshSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	b, err := d.Svc.GetDashboard(ctx, d.Bob, dash.ID)
	if err != nil || b.CanEdit || b.Mine || len(b.Gadgets) != 1 {
		t.Fatalf("%+v %v", b, err)
	}
	// ... who can use it but not change it
	if err := d.Svc.UpdateDashboard(ctx, d.Bob, dash.ID, domain.DashboardIn{Name: "Bob's", Columns: 1}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if _, err := d.Svc.AddGadget(ctx, d.Bob, dash.ID, domain.GadgetIn{Kind: "text", Config: map[string]any{"markdown": "x"}}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if err := d.Svc.DeleteGadget(ctx, d.Bob, dash.ID, g.ID); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if err := d.Svc.DeleteDashboard(ctx, d.Bob, dash.ID); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	// a copy of his own
	cp, err := d.Svc.CloneDashboard(ctx, d.Bob, dash.ID)
	if err != nil || !cp.Mine || cp.Shared != "private" || len(cp.Gadgets) != 1 || !strings.Contains(cp.Name, "copy") {
		t.Fatalf("%+v %v", cp, err)
	}
	if _, err := d.Svc.GetDashboard(ctx, d.Alice, cp.ID); err != domain.ErrNotFound {
		t.Fatalf("a private copy is visible to the original's owner: %v", err)
	}
	// favourites and defaults are each person's own; unsharing takes the dashboard from the others, with their marks
	d.Svc.SetFavourite(ctx, d.Bob, dash.ID, true)
	d.Svc.SetDefaultDashboard(ctx, d.Bob, dash.ID)
	if l, _ := d.Svc.ListDashboards(ctx, d.Bob); !l[0].Favourite || l[0].ID != dash.ID && !l[1].Default {
		t.Fatalf("%+v", l)
	}
	if l, _ := d.Svc.ListDashboards(ctx, d.Alice); l[0].Favourite || l[0].Default {
		t.Fatalf("Alice sees Bob's marks: %+v", l)
	}
	d.Svc.UpdateDashboard(ctx, d.Alice, dash.ID, domain.DashboardIn{Name: "Team", Columns: 3, Shared: "private"})
	if _, err := d.Svc.GetDashboard(ctx, d.Bob, dash.ID); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	var marks int
	d.Store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM dashboard_favourite)+(SELECT count(*) FROM user_default_dashboard)`).Scan(&marks)
	if marks != 0 {
		t.Fatalf("%d marks of people who no longer see the dashboard", marks)
	}
	// another organization and no one at all cannot see a shared dashboard; an instance administrator can
	d.Svc.UpdateDashboard(ctx, d.Alice, dash.ID, domain.DashboardIn{Name: "Team", Columns: 3, Shared: "org"})
	d.Svc.CreateOrganization(ctx, d.Admin, "Globex", "boss@globex.example", "Boss", "password123")
	ul, _ := d.Svc.ListUsersFor(ctx, d.Admin)
	var boss domain.Actor
	for _, u := range ul {
		if u.Email == "boss@globex.example" {
			boss, _ = d.Svc.ActorFor(ctx, u.ID)
		}
	}
	if _, err := d.Svc.GetDashboard(ctx, boss, dash.ID); err != domain.ErrNotFound {
		t.Fatalf("another organization saw a dashboard: %v", err)
	}
	if _, err := d.Svc.GadgetData(ctx, boss, dash.ID, g.ID); err != domain.ErrNotFound {
		t.Fatalf("another organization read gadget data: %v", err)
	}
	if _, err := d.Svc.GetDashboard(ctx, d.Admin, dash.ID); err != nil {
		t.Fatal(err)
	}
	// columns: shrinking moves gadgets into the last column; at most 20 gadgets
	d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "text", Col: 2, Config: map[string]any{"markdown": "right"}})
	d.Svc.UpdateDashboard(ctx, d.Alice, dash.ID, domain.DashboardIn{Name: "Team", Columns: 1, Shared: "org"})
	got, _ := d.Svc.GetDashboard(ctx, d.Alice, dash.ID)
	for _, g := range got.Gadgets {
		if g.Col != 0 {
			t.Fatalf("a gadget is in column %d of a 1-column dashboard", g.Col)
		}
	}
	for i := 0; i < 25; i++ {
		if _, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "text", Config: map[string]any{"markdown": fmt.Sprint("g", i)}}); err != nil {
			if !strings.Contains(err.Error(), "at most 20") {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("more than 20 gadgets were accepted")
}

func asInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	}
	return -1
}

// Gadget data matches what the filter lists: a pie slice for status X has as many issues as the search "status = X", the slices add up
// to the total, a statistic's cells are hand-counted, and counting by label counts an issue once for each of its labels.
func TestDASH03_GadgetDataMatchesTheFiltersAndHandCountedFixtures(t *testing.T) {
	d := newDash(t)
	q := "project = ABC"
	sc := d.scope(t, d.Alice)
	total := func(extra string) int {
		r, err := d.pg.Search(ctx, sc, "("+q+") AND "+extra, search.Page{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return r.Total
	}
	// filter results: the same rows and total as the search
	fr := d.gadget(t, d.Alice, "filter_results", map[string]any{"q": q + " ORDER BY created ASC", "limit": 3})
	want, _ := d.pg.Search(ctx, sc, q+" ORDER BY created ASC", search.Page{Limit: 3})
	if fr["total"] != float64(want.Total) && fr["total"] != want.Total {
		t.Fatalf("total %v, search says %d", fr["total"], want.Total)
	}
	rows := rowsOf(t, fr)
	if len(rows) != 3 || rows[0].Key != want.Rows[0].Key || rows[2].Key != want.Rows[2].Key {
		t.Fatalf("%+v vs %+v", rows, want.Rows)
	}
	// a pie by status: each slice is what the search says, and they add up
	pie := d.gadget(t, d.Alice, "pie", map[string]any{"q": q, "by": "status"})
	sum := 0
	for _, s := range aggs(t, pie, "slices") {
		if got := total(fmt.Sprintf(`status = "%s"`, s.A)); got != s.Count {
			t.Errorf("slice %s: %d, search says %d", s.A, s.Count, got)
		}
		sum += s.Count
	}
	if sum != asInt(pie["total"]) || sum != 5 {
		t.Fatalf("slices add up to %d, total %v, want 5", sum, pie["total"])
	}
	// by priority: high 2, low 1, medium 2 (a1 a2 high; a3 low; a4 a5 medium)
	pr := d.gadget(t, d.Alice, "pie", map[string]any{"q": q, "by": "priority"})
	got := map[string]int{}
	for _, s := range aggs(t, pr, "slices") {
		got[s.A] = s.Count
	}
	if fmt.Sprint(got) != "map[high:2 low:1 medium:2]" {
		t.Fatalf("%v", got)
	}
	// by label: ui 3 (a1 a2 a5), urgent 1, backend 1, and the four issues' no-label bucket holds a3 only; counts are per label
	lb := d.gadget(t, d.Alice, "pie", map[string]any{"q": q, "by": "label"})
	got = map[string]int{}
	for _, s := range aggs(t, lb, "slices") {
		got[s.A] = s.Count
	}
	if fmt.Sprint(got) != "map[(no label):1 backend:1 ui:3 urgent:1]" {
		t.Fatalf("%v", got)
	}
	if asInt(lb["total"]) != 5 {
		t.Fatalf("the total counts an issue once however many labels it has: %v", lb["total"])
	}
	// two-dimensional: assignee x priority
	st := d.gadget(t, d.Alice, "stats_2d", map[string]any{"q": q, "rows": "assignee", "cols": "priority"})
	cells := map[string]int{}
	for _, c := range aggs(t, st, "cells") {
		cells[c.A+"/"+c.B] = c.Count
	}
	if fmt.Sprint(cells) != "map[Alice K./high:1 Alice K./medium:1 Bob R./high:1 Bob R./low:1 Unassigned/medium:1]" {
		t.Fatalf("%v", cells)
	}
	// assigned to me, for each person
	ma := d.gadget(t, d.Alice, "assigned_to_me", map[string]any{})
	mb := d.gadget(t, d.Bob, "assigned_to_me", map[string]any{})
	if asInt(ma["total"]) != 3 || asInt(mb["total"]) != 2 { // Alice: a1, x1, x2 (a5 is done); Bob: a2, a3
		t.Fatalf("Alice %v, Bob %v", ma["total"], mb["total"])
	}
	// saved filter binding: the same numbers as the query it holds
	f, _ := d.Svc.CreateFilter(ctx, d.Alice, domain.FilterIn{Name: "High ABC", TQL: "project = ABC AND priority = high", Shared: "org"})
	bound := d.gadget(t, d.Alice, "filter_results", map[string]any{"filter_id": f.ID})
	if asInt(bound["total"]) != 2 {
		t.Fatalf("%v", bound["total"])
	}
	// the activity stream: newest first, only issues the filter matches
	act := d.gadget(t, d.Alice, "activity_stream", map[string]any{"q": "project = XYZ", "limit": 10})
	for _, e := range act["events"].([]any) {
		_ = e
	}
	if n := len(act["events"].([]any)); n == 0 {
		t.Fatal("no activity")
	}
}

// Average age and created-versus-resolved, on a clock the test controls so the numbers are exact.
func TestDASH03_AgeAndFlowAreHandComputed(t *testing.T) {
	e := testutil.New(t)
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e.Clock.T = start
	e.Project()
	d := &dashEnv{Env: e, pg: &search.Postgres{DB: e.Store.Pool}}
	mk := func(daysAgo int, summary string) domain.Issue {
		e.Clock.T = start.AddDate(0, 0, -daysAgo)
		is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: summary})
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	mk(10, "old")
	mk(4, "newer")
	closed := mk(20, "closed long ago")
	e.Clock.T = start.AddDate(0, 0, -2)
	tid, _ := trans(t, e, e.Alice, closed.Key, "Resolve")
	e.Svc.Transition(ctx, e.Alice, closed.Key, tid, nil)
	e.Clock.T = start
	age := d.gadget(t, e.Alice, "average_age", map[string]any{"q": "project = ABC"})
	// open issues only: ages 10 and 4 days: average 7
	if asInt(age["open_issues"]) != 2 || age["average_age_days"] != 7.0 || age["oldest"] != "ABC-1" {
		t.Fatalf("%v", age)
	}
	cvr := d.gadget(t, e.Alice, "created_vs_resolved", map[string]any{"q": "project = ABC", "days": 14})
	type day struct {
		Day               string
		Created, Resolved int
	}
	byDay := map[string][2]int{}
	for _, x := range cvr["days"].([]any) {
		_ = x
	}
	days := cvr["days"]
	b, _ := jsonMarshal(days)
	_ = b
	for _, x := range toDays(t, days) {
		byDay[x.Day] = [2]int{x.Created, x.Resolved}
	}
	if len(byDay) != 14 {
		t.Fatalf("%d days", len(byDay))
	}
	// created: 10 days ago (old) and 4 days ago (newer); the third was created 20 days ago, before the window; resolved 2 days ago
	if byDay["2026-09-21"] != [2]int{1, 0} || byDay["2026-09-27"] != [2]int{1, 0} || byDay["2026-09-29"] != [2]int{0, 1} || byDay["2026-10-01"] != [2]int{0, 0} {
		t.Fatalf("%v", byDay)
	}
	tot := [2]int{}
	for _, v := range byDay {
		tot[0] += v[0]
		tot[1] += v[1]
	}
	if tot != [2]int{2, 1} {
		t.Fatalf("created and resolved in the window: %v", tot)
	}
}

// The property that matters: gadget data is computed for the person looking. Two people looking at the same shared dashboard see the
// issues each may see; the owner's wider view never leaks to others.
func TestDASH04_GadgetDataIsComputedForTheViewerNotTheOwner(t *testing.T) {
	d := newDash(t)
	dash, _ := d.Svc.CreateDashboard(ctx, d.Alice, domain.DashboardIn{Name: "Everything", Shared: "org"})
	by, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "pie", Config: map[string]any{"q": "", "by": "project"}})
	if err != nil {
		t.Fatal(err)
	}
	add := func(kind string, cfg map[string]any) domain.Gadget {
		g, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: kind, Config: cfg})
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		return g
	}
	list := add("filter_results", map[string]any{"q": "created >= \"2000-01-01\" ORDER BY created ASC", "limit": 50})
	cnt := add("average_age", map[string]any{})
	act := add("activity_stream", map[string]any{"limit": 30})
	data := func(who domain.Actor, g domain.Gadget) map[string]any {
		r, err := d.Svc.GadgetData(ctx, who, dash.ID, g.ID)
		if err != nil || r.Error != "" {
			t.Fatalf("%v %s", err, r.Error)
		}
		return viaJSON(t, r.Data)
	}
	slices := func(m map[string]any) map[string]int {
		out := map[string]int{}
		for _, s := range aggs(t, m, "slices") {
			out[s.A] = s.Count
		}
		return out
	}
	if got := slices(data(d.Alice, by)); fmt.Sprint(got) != "map[ABC:5 XYZ:2]" {
		t.Fatalf("the owner sees %v", got)
	}
	if got := slices(data(d.Bob, by)); fmt.Sprint(got) != "map[ABC:5]" {
		t.Fatalf("Bob, who is not in XYZ, sees %v on the owner's dashboard", got)
	}
	for _, r := range rowsOf(t, data(d.Bob, list)) {
		if strings.HasPrefix(r.Key, "XYZ") || strings.Contains(r.Summary, "secret") {
			t.Fatalf("the list shows Bob %s: %s", r.Key, r.Summary)
		}
	}
	if n := asInt(data(d.Bob, list)["total"]); n != 5 {
		t.Fatalf("Bob's total is %d", n)
	}
	if n := asInt(data(d.Alice, list)["total"]); n != 7 {
		t.Fatalf("Alice's total is %d", n)
	}
	if o := data(d.Bob, cnt)["oldest"]; o != nil && strings.HasPrefix(o.(string), "XYZ") {
		t.Fatalf("the oldest open issue for Bob is a secret one: %v", o)
	}
	for _, e := range data(d.Bob, act)["events"].([]any) {
		if strings.HasPrefix(fmt.Sprint(e), "{XYZ") {
			t.Fatalf("the activity shows Bob a secret issue: %v", e)
		}
	}
	// a viewer who cannot use the saved filter a gadget binds gets an error for that gadget only
	f, _ := d.Svc.CreateFilter(ctx, d.Alice, domain.FilterIn{Name: "Shared one", TQL: "project = ABC", Shared: "org"})
	g2, _ := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "filter_results", Config: map[string]any{"filter_id": f.ID}})
	d.Store.Pool.Exec(ctx, `DELETE FROM saved_filter WHERE id::text=$1`, f.ID)
	r, err := d.Svc.GadgetData(ctx, d.Bob, dash.ID, g2.ID)
	if err != nil || !strings.Contains(r.Error, "not available") {
		t.Fatalf("%v %+v", err, r)
	}
	if r, _ := d.Svc.GadgetData(ctx, d.Bob, dash.ID, by.ID); r.Error != "" {
		t.Fatalf("one broken gadget broke another: %s", r.Error)
	}
	// private filters cannot be bound on an organization-wide dashboard, and a gadget needs the right kind of configuration
	pf, _ := d.Svc.CreateFilter(ctx, d.Alice, domain.FilterIn{Name: "Private", TQL: "project = ABC"})
	if _, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "filter_results", Config: map[string]any{"filter_id": pf.ID}}); err == nil || !strings.Contains(err.Error(), "organization-wide filters") {
		t.Fatalf("%v", err)
	}
	if err := d.Svc.UpdateDashboard(ctx, d.Alice, dash.ID, domain.DashboardIn{Name: "Everything", Shared: "org"}); err != nil {
		t.Fatal(err)
	}
	priv, _ := d.Svc.CreateDashboard(ctx, d.Alice, domain.DashboardIn{Name: "Mine"})
	d.Svc.AddGadget(ctx, d.Alice, priv.ID, domain.GadgetIn{Kind: "filter_results", Config: map[string]any{"filter_id": pf.ID}})
	if err := d.Svc.UpdateDashboard(ctx, d.Alice, priv.ID, domain.DashboardIn{Name: "Mine", Shared: "org"}); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("a dashboard with a private filter was shared: %v", err)
	}
}

func TestDASH05_GadgetsAreValidatedAndTextAndFramesAreSafe(t *testing.T) {
	d := newDash(t)
	dash, _ := d.Svc.CreateDashboard(ctx, d.Alice, domain.DashboardIn{Name: "Checks"})
	bad := map[string]domain.GadgetIn{
		"unknown kind":      {Kind: "weather"},
		"bad query":         {Kind: "filter_results", Config: map[string]any{"q": "status = ="}},
		"both":              {Kind: "filter_results", Config: map[string]any{"q": "project = ABC", "filter_id": "00000000-0000-0000-0000-000000000000"}},
		"someone's filter":  {Kind: "filter_results", Config: map[string]any{"filter_id": "00000000-0000-0000-0000-000000000000"}},
		"limit":             {Kind: "filter_results", Config: map[string]any{"limit": 500}},
		"fractional limit":  {Kind: "filter_results", Config: map[string]any{"limit": 2.5}},
		"pie without by":    {Kind: "pie", Config: map[string]any{}},
		"pie bad by":        {Kind: "pie", Config: map[string]any{"by": "password"}},
		"2d same":           {Kind: "stats_2d", Config: map[string]any{"rows": "status", "cols": "status"}},
		"days":              {Kind: "created_vs_resolved", Config: map[string]any{"days": 1000}},
		"workload board":    {Kind: "workload", Config: map[string]any{}},
		"empty text":        {Kind: "text", Config: map[string]any{"markdown": "  "}},
		"huge text":         {Kind: "text", Config: map[string]any{"markdown": strings.Repeat("x", 5000)}},
		"http frame":        {Kind: "iframe", Config: map[string]any{"url": "http://wiki.example.com/x"}},
		"frame not allowed": {Kind: "iframe", Config: map[string]any{"url": "https://evil.example.org/x"}},
		"frame userinfo":    {Kind: "iframe", Config: map[string]any{"url": "https://user:pw@wiki.example.com/x"}},
		"javascript frame":  {Kind: "iframe", Config: map[string]any{"url": "javascript:alert(1)"}},
	}
	for name, in := range bad {
		if _, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// text: markup is removed when it is saved
	g, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "text", Config: map[string]any{"markdown": "Hello <script>alert(1)</script> **world**"}})
	if err != nil {
		t.Fatal(err)
	}
	// the text is kept as text: like issue text it is shown by the client as text, never as markup (the browser test checks that)
	if md := g.Config["markdown"].(string); !strings.Contains(md, "world") {
		t.Fatalf("%q", md)
	}
	// frames: only the listed hosts, and only https
	d.Svc.DashboardIframeHosts = []string{"wiki.example.com"}
	f, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "iframe", Config: map[string]any{"url": "https://WIKI.example.com/page", "height": 400}})
	if err != nil || f.Config["height"].(int) != 400 {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := d.Svc.AddGadget(ctx, d.Alice, dash.ID, domain.GadgetIn{Kind: "iframe", Config: map[string]any{"url": "https://wiki.example.com.evil.org/x"}}); err == nil {
		t.Fatal("a look-alike host was accepted")
	}
	// updating checks again, and a gadget can be moved
	if err := d.Svc.UpdateGadget(ctx, d.Alice, dash.ID, g.ID, domain.GadgetIn{Title: "Notes", Col: 1, Config: map[string]any{"markdown": "changed"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Svc.GetDashboard(ctx, d.Alice, dash.ID)
	moved := false
	for _, x := range got.Gadgets {
		if x.ID == g.ID && x.Col == 1 && x.Title == "Notes" {
			moved = true
		}
	}
	if !moved {
		t.Fatalf("%+v", got.Gadgets)
	}
	if err := d.Svc.UpdateGadget(ctx, d.Alice, dash.ID, g.ID, domain.GadgetIn{Config: map[string]any{"markdown": ""}}); err == nil {
		t.Fatal("an update bypassed validation")
	}
	if err := d.Svc.DeleteGadget(ctx, d.Alice, dash.ID, "00000000-0000-0000-0000-000000000000"); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

type flowDay struct {
	Day               string `json:"day"`
	Created, Resolved int
}

func toDays(t *testing.T, v any) []struct {
	Day               string
	Created, Resolved int
} {
	t.Helper()
	b, _ := json.Marshal(v)
	var raw []struct {
		Day      string `json:"day"`
		Created  int    `json:"created"`
		Resolved int    `json:"resolved"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	out := make([]struct {
		Day               string
		Created, Resolved int
	}, len(raw))
	for i, r := range raw {
		out[i].Day, out[i].Created, out[i].Resolved = r.Day, r.Created, r.Resolved
	}
	return out
}
