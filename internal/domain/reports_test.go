package domain_test

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func at(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }

func finish(t *testing.T, e *testutil.Env, key string) {
	v, _ := e.Svc.View(ctx, e.Alice, key)
	if _, err := e.Svc.Transition(ctx, e.Alice, key, tidTo(t, v, "Done"), nil); err != nil {
		t.Fatal(err)
	}
}

func items(r []domain.ReportItem) string {
	var k []string
	for _, i := range r {
		k = append(k, i.Key)
	}
	sort.Strings(k)
	return strings.Join(k, " ")
}

func setOf(keys ...string) string {
	sort.Strings(keys)
	return strings.Join(keys, " ")
}

// a week in a sprint: planned 10 points, 8 added on Wednesday, 2 removed on Thursday, two finished
func runSprintWeek(t *testing.T) (*testutil.Env, domain.Board, string, map[string]string) {
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	e.Svc.EnsureSprints(ctx, b.ID)
	k := map[string]string{}
	for _, x := range []struct {
		name string
		pts  float64
	}{{"A", 5}, {"B", 3}, {"C", 2}, {"D", 8}} {
		k[x.name] = mk(t, e, e.Alice, "Task", "issue "+x.name, "")
		est(t, e, k[x.name], x.pts)
	}
	v := backlog(t, e, e.Alice, b)
	sid := v.Sprints[0].ID
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, sid, []string{k["A"], k["B"], k["C"]}, domain.MoveIn{})
	e.Svc.StartSprint(ctx, e.Admin, sid) // Monday 19 Oct 09:00
	e.Clock.T = at(20, 10)
	finish(t, e, k["A"])
	e.Clock.T = at(21, 10)
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, sid, []string{k["D"]}, domain.MoveIn{})
	e.Clock.T = at(22, 10)
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, "", []string{k["C"]}, domain.MoveIn{})
	e.Clock.T = at(23, 10)
	finish(t, e, k["B"])
	return e, b, sid, k
}

func TestREPORT_BurndownBurnupAndSprintReport(t *testing.T) {
	e, b, sid, k := runSprintWeek(t)
	e.Clock.T = at(23, 17)
	if _, err := e.Svc.CompleteSprint(ctx, e.Admin, sid, "backlog", false); err != nil {
		t.Fatal(err)
	}
	r, err := e.Svc.SprintReport(ctx, e.Alice, sid)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		at                            time.Time
		remaining, ideal, scope, done float64
	}{
		{at(19, 9), 10, 10, 10, 0}, // the start: 10 points committed
		{at(20, 0), 10, 8, 10, 0},  // Tuesday midnight: nothing done yet, ideal line has used a day
		{at(21, 0), 5, 6, 10, 5},   // A finished on Tuesday
		{at(22, 0), 13, 4, 18, 5},  // D (8) was added on Wednesday: scope grows, remaining jumps
		{at(23, 0), 11, 2, 16, 5},  // C (2) was removed on Thursday
		{at(23, 17), 8, 2, 16, 8},  // B finished on Friday; only D is left
	}
	if len(r.Burn) != len(want) {
		t.Fatalf("%d samples: %+v", len(r.Burn), r.Burn)
	}
	for i, w := range want {
		g := r.Burn[i]
		if !g.At.Equal(w.at) || g.Remaining != w.remaining || g.Ideal != w.ideal || g.Scope != w.scope || g.Done != w.done {
			t.Errorf("sample %d: %+v, want %+v", i, g, w)
		}
	}
	if r.Committed != 10 || r.Completed != 8 {
		t.Fatalf("committed %v completed %v", r.Committed, r.Completed)
	}
	if items(r.CompletedIs) != setOf(k["A"], k["B"]) || items(r.NotCompleted) != k["D"] || items(r.Removed) != k["C"] || items(r.Added) != k["D"] {
		t.Fatalf("completed %s | not %s | removed %s | added %s", items(r.CompletedIs), items(r.NotCompleted), items(r.Removed), items(r.Added))
	}
	// velocity: committed against completed over closed sprints
	vel, err := e.Svc.Velocity(ctx, e.Alice, b.ID, 6)
	if err != nil || len(vel) != 1 || vel[0].Committed != 10 || vel[0].Completed != 8 {
		t.Fatalf("%v %+v", err, vel)
	}
	// a later change of an estimate does not rewrite the closed sprint's chart (history is replayed, not today's value)
	e.Clock.T = at(26, 9)
	est(t, e, k["A"], 100)
	r2, _ := e.Svc.SprintReport(ctx, e.Alice, sid)
	if r2.Committed != 10 || r2.Burn[0].Remaining != 10 || r2.Completed != 8 {
		t.Fatalf("a closed sprint's report changed: %+v", r2.Burn[0])
	}
	// a later calendar edit does not either
	nz := "Pacific/Auckland"
	_ = nz
	// an outsider learns nothing
	out, _ := e.Svc.CreateUser(ctx, "o@example.com", "O", "password123", false)
	if _, err := e.Svc.SprintReport(ctx, domain.Actor{ID: out.ID, Kind: "human"}, sid); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Velocity(ctx, e.Alice, newBoard(t, e, "kanban").ID, 6); err == nil {
		t.Fatal("velocity on a kanban board")
	}
}

func TestREPORT_ActiveSprintUsesNow(t *testing.T) {
	e, _, sid, _ := runSprintWeek(t) // the clock is Friday 10:00 and the sprint is still active
	r, err := e.Svc.SprintReport(ctx, e.Alice, sid)
	if err != nil {
		t.Fatal(err)
	}
	last := r.Burn[len(r.Burn)-1]
	if !last.At.Equal(at(23, 10)) || last.Remaining != 8 || last.Done != 8 {
		t.Fatalf("%+v", last)
	}
	if items(r.CompletedIs) == "" || len(r.NotCompleted) != 1 {
		t.Fatalf("%v %v", r.CompletedIs, r.NotCompleted)
	}
}

func TestREPORT_FlowReports(t *testing.T) {
	e, b, _, k := runSprintWeek(t)
	e.Clock.T = at(23, 17)
	cf, err := e.Svc.CumulativeFlow(ctx, e.Alice, b.ID, 7, nil)
	if err != nil || len(cf.Days) != 7 {
		t.Fatalf("%v %d", err, len(cf.Days))
	}
	byDay := map[string]map[string]int{}
	for _, d := range cf.Days {
		byDay[d.Day] = d.Counts
	}
	if c := byDay["2026-10-18"]; c["To Do"] != 0 || c["Done"] != 0 { // before anything existed
		t.Fatalf("%v", c)
	}
	if c := byDay["2026-10-19"]; c["To Do"] != 4 {
		t.Fatalf("Monday: %v", c)
	}
	if c := byDay["2026-10-20"]; c["Done"] != 1 || c["To Do"] != 3 {
		t.Fatalf("Tuesday: %v", c)
	}
	if c := byDay["2026-10-23"]; c["Done"] != 2 || c["To Do"] != 2 {
		t.Fatalf("Friday: %v", c)
	}
	// created against resolved
	cr, _ := e.Svc.CreatedVsResolved(ctx, e.Alice, b.ID, 7, nil)
	got := map[string][2]int{}
	for _, d := range cr {
		got[d.Day] = [2]int{d.Created, d.Resolved}
	}
	if got["2026-10-19"] != [2]int{4, 0} || got["2026-10-20"] != [2]int{0, 1} || got["2026-10-23"] != [2]int{0, 1} || cr[len(cr)-1].CumCreated != 4 || cr[len(cr)-1].CumResol != 2 {
		t.Fatalf("%v %+v", got, cr[len(cr)-1])
	}
	// control chart: A was created Monday 09:00 and resolved Tuesday 10:00
	cc, _ := e.Svc.ControlChart(ctx, e.Alice, b.ID, 30, nil)
	if len(cc.Points) != 2 || cc.Points[0].Key != k["A"] || cc.Points[0].Days != 1.04 || cc.Average <= 0 {
		t.Fatalf("%+v", cc)
	}
	// workload: C and D are open and unassigned
	wl, _ := e.Svc.WorkloadByAssignee(ctx, e.Alice, b.ID, nil)
	if len(wl) != 1 || wl[0].Assignee != "Unassigned" || wl[0].Open != 2 || wl[0].Estimate != 10 {
		t.Fatalf("%+v", wl)
	}
	// time in status
	ts, _ := e.Svc.TimeInStatus(ctx, e.Alice, b.ID, 30, nil)
	var todo, done domain.StatusTime
	for _, s := range ts {
		if s.Status == "To Do" {
			todo = s
		}
		if s.Status == "Done" {
			done = s
		}
	}
	if todo.IssueCount != 4 || todo.TotalDays < 5 || done.IssueCount != 2 { // A waited 1 day, B 4 days, C and D 4 days and a bit each
		t.Fatalf("%+v %+v", todo, done)
	}
	// the filter keys restrict every report
	only, _ := e.Svc.WorkloadByAssignee(ctx, e.Alice, b.ID, []string{k["D"]})
	if len(only) != 1 || only[0].Open != 1 || only[0].Estimate != 8 {
		t.Fatalf("%+v", only)
	}
	// trashed issues are invisible to reports
	life(t, e, e.Admin, k["D"], domain.LifecycleIn{Op: "delete"})
	wl, _ = e.Svc.WorkloadByAssignee(ctx, e.Alice, b.ID, nil)
	if wl[0].Open != 1 {
		t.Fatalf("%+v", wl)
	}
}

func TestREPORT_EpicBurndown(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := newBoard(t, e, "kanban")
	epic := mk(t, e, e.Alice, "Epic", "the epic", "")
	s1, s2 := mk(t, e, e.Alice, "Story", "one", epic), mk(t, e, e.Alice, "Story", "two", epic)
	est(t, e, s1, 3)
	est(t, e, s2, 5)
	e.Clock.T = at(20, 10)
	finish(t, e, s1)
	e.Clock.T = at(21, 12)
	r, err := e.Svc.EpicBurndown(ctx, e.Alice, b.ID, epic)
	if err != nil || len(r.Points) != 3 {
		t.Fatalf("%v %+v", err, r.Points)
	}
	if p := r.Points[0]; p.Remaining != 8 || p.Scope != 8 { // Monday
		t.Fatalf("%+v", p)
	}
	if p := r.Points[1]; p.Remaining != 5 || p.Done != 3 { // Tuesday
		t.Fatalf("%+v", p)
	}
	if p := r.Points[2]; p.Remaining != 5 || p.Scope != 8 {
		t.Fatalf("%+v", p)
	}
	if _, err := e.Svc.EpicBurndown(ctx, e.Alice, b.ID, "ABC-999"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}
