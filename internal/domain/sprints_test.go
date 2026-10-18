package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/calendar"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func scrum(t *testing.T, e *testutil.Env) domain.Board {
	b, err := e.Svc.CreateBoard(ctx, e.Admin, "ABC", "Scrum", "scrum")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func backlog(t *testing.T, e *testutil.Env, a domain.Actor, b domain.Board) domain.BacklogView {
	v, err := e.Svc.Backlog(ctx, a, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func cardList(cs []domain.Card) string {
	var k []string
	for _, c := range cs {
		k = append(k, c.Key)
	}
	return strings.Join(k, " ")
}

func est(t *testing.T, e *testutil.Env, key string, v float64) {
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, key, nil, false, domain.Changes{Estimate: &v}); err != nil {
		t.Fatal(err)
	}
}

func TestSPR_AutoSprintsNamesAndCalendarChange(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	kb := newBoard(t, e, "kanban")
	if _, err := e.Svc.CreateSprint(ctx, e.Admin, kb.ID, domain.SprintIn{}); err == nil {
		t.Fatal("a kanban board got a sprint")
	}
	b := scrum(t, e)
	n, err := e.Svc.EnsureSprints(ctx, b.ID)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := e.Svc.EnsureSprints(ctx, b.ID); n != 0 {
		t.Fatalf("not idempotent: %d", n)
	}
	v := backlog(t, e, e.Alice, b)
	s1, s2 := v.Sprints[0], v.Sprints[1]
	if s1.Name != "Week 2026-W43" || s2.Name != "Week 2026-W44" || s1.State != "future" { // the spec's own example, from the clock Monday 19 Oct 2026
		t.Fatalf("%q %q %s", s1.Name, s2.Name, s1.State)
	}
	if !s1.Start.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)) || !s1.End.Equal(s2.Start) || s1.WorkingDays != 5 {
		t.Fatalf("%v %v %d", s1.Start, s1.End, s1.WorkingDays)
	}
	// a Sunday-start, Sunday to Thursday board in Jerusalem: the future sprints follow from the next sprint on
	il, _ := calendar.Preset("sun-thu", "Asia/Jerusalem")
	if _, err := e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Calendar: &il}); err != nil {
		t.Fatal(err)
	}
	v = backlog(t, e, e.Alice, b)
	if v.Sprints[0].Name != "Week of 2026-10-18" || v.Sprints[0].Start.UTC().Format("2006-01-02 15:04") != "2026-10-17 21:00" { // Sunday 00:00 at UTC+3 (daylight time)
		t.Fatalf("%q %v", v.Sprints[0].Name, v.Sprints[0].Start)
	}
	if v.Sprints[0].Calendar.TimeZone != "Asia/Jerusalem" || !v.Sprints[0].End.Equal(v.Sprints[1].Start) {
		t.Fatalf("%+v", v.Sprints[0].Calendar)
	}
	// a bad calendar is refused, and a member cannot change it
	bad := calendar.Calendar{TimeZone: "Nowhere/City"}
	if _, err := e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Calendar: &bad}); err == nil {
		t.Fatal("a bad calendar was accepted")
	}
	two := 2
	if _, err := e.Svc.UpdateBoard(ctx, e.Alice, b.ID, domain.BoardPatch{AutoSprints: &two}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	// two-week sprints and a custom name pattern
	w, pat := 2, "Sprint {n} ({start} to {end})"
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{SprintWeeks: &w, NamePattern: &pat})
	v = backlog(t, e, e.Alice, b)
	if !strings.HasPrefix(v.Sprints[0].Name, "Sprint 1 (2026-10-18 to 2026-10-31)") {
		t.Fatalf("%q", v.Sprints[0].Name)
	}
	// a manually renamed sprint keeps its name through a recompute
	nm := "Launch week"
	e.Svc.UpdateSprint(ctx, e.Admin, v.Sprints[0].ID, &nm, nil)
	w = 1
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{SprintWeeks: &w})
	if v = backlog(t, e, e.Alice, b); v.Sprints[0].Name != "Launch week" || v.Sprints[1].Name == "Launch week" {
		t.Fatalf("%q %q", v.Sprints[0].Name, v.Sprints[1].Name)
	}
}

func TestSPR_LifecycleScopeAndCarryOver(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	e.Svc.EnsureSprints(ctx, b.ID)
	k1, k2, k3, k4 := mk(t, e, e.Alice, "Task", "a", ""), mk(t, e, e.Alice, "Task", "b", ""), mk(t, e, e.Alice, "Task", "c", ""), mk(t, e, e.Alice, "Task", "d", "")
	est(t, e, k1, 3)
	est(t, e, k2, 5)
	v := backlog(t, e, e.Alice, b)
	if cardList(v.Backlog) != strings.Join([]string{k1, k2, k3, k4}, " ") {
		t.Fatalf("backlog %s", cardList(v.Backlog))
	}
	s1, s2 := v.Sprints[0].ID, v.Sprints[1].ID
	if err := e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1, []string{k1, k2, k3}, domain.MoveIn{}); err != nil {
		t.Fatal(err)
	}
	// a scrum board shows the active sprint only: nothing before the sprint starts
	if bv, _ := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil); len(bv.Columns[0].Cards) != 0 {
		t.Fatalf("a future sprint's issues are on the board: %d", len(bv.Columns[0].Cards))
	}
	if _, err := e.Svc.StartSprint(ctx, e.Alice, s1); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member started a sprint: %v", err)
	}
	if _, err := e.Svc.StartSprint(ctx, e.Admin, s1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.StartSprint(ctx, e.Admin, s1); err == nil || !strings.Contains(err.Error(), "only a future sprint") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.StartSprint(ctx, e.Admin, s2); err == nil || !strings.Contains(err.Error(), "still active") {
		t.Fatalf("two active sprints without the parallel setting: %v", err)
	}
	if bv, _ := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil); len(bv.Columns[0].Cards) != 3 {
		t.Fatalf("%d", len(bv.Columns[0].Cards))
	}
	// scope change after the start is recorded: k4 added, k3 removed
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1, []string{k4}, domain.MoveIn{})
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, "", []string{k3}, domain.MoveIn{})
	sv, _ := e.Svc.GetSprint(ctx, e.Alice, s1)
	if sv.ScopeAdded != 1 || sv.ScopeRemoved != 1 || len(sv.Issues) != 3 {
		t.Fatalf("added %d removed %d issues %d", sv.ScopeAdded, sv.ScopeRemoved, len(sv.Issues))
	}
	if sv.Totals.Todo.Count != 3 || sv.Totals.Todo.Estimate != 8 {
		t.Fatalf("%+v", sv.Totals)
	}
	// k1 finishes; k2 is in progress; k4 untouched
	v1, _ := e.Svc.View(ctx, e.Alice, k1)
	e.Svc.Transition(ctx, e.Alice, k1, tidTo(t, v1, "Done"), nil)
	v2, _ := e.Svc.View(ctx, e.Alice, k2)
	e.Svc.Transition(ctx, e.Alice, k2, tidTo(t, v2, "In Progress"), nil)
	sv, _ = e.Svc.GetSprint(ctx, e.Alice, s1)
	if sv.Totals.Done.Count != 1 || sv.Totals.Done.Estimate != 3 || sv.Totals.InProgress.Estimate != 5 {
		t.Fatalf("%+v", sv.Totals)
	}
	// the preview is the result and commits nothing
	plan, err := e.Svc.CompleteSprint(ctx, e.Admin, s1, "next", true)
	if err != nil || !plan.DryRun || strings.Join(plan.Moved, " ") != k2+" "+k4 || strings.Join(plan.Done, " ") != k1 || plan.Destination != v.Sprints[1].Name {
		t.Fatalf("%v %+v", err, plan)
	}
	if sv, _ = e.Svc.GetSprint(ctx, e.Alice, s1); sv.State != "active" {
		t.Fatalf("a dry run closed the sprint: %s", sv.State)
	}
	if sv2, _ := e.Svc.GetSprint(ctx, e.Alice, s2); len(sv2.Issues) != 0 {
		t.Fatal("a dry run moved issues")
	}
	res, err := e.Svc.CompleteSprint(ctx, e.Admin, s1, "next", false)
	if err != nil || strings.Join(res.Moved, " ") != strings.Join(plan.Moved, " ") || res.Destination != plan.Destination {
		t.Fatalf("plan and result differ: %v %+v vs %+v", err, res, plan)
	}
	if sv, _ = e.Svc.GetSprint(ctx, e.Alice, s1); sv.State != "closed" || sv.ClosedAt == nil {
		t.Fatalf("%+v", sv.State)
	}
	if sv2, _ := e.Svc.GetSprint(ctx, e.Alice, s2); cardList(sv2.Issues) != k2+" "+k4 {
		t.Fatalf("%s", cardList(sv2.Issues))
	}
	// carried over once, and the done issue stays with the closed sprint
	bl := backlog(t, e, e.Alice, b)
	for _, c := range bl.Sprints[0].Issues {
		if c.CarriedOver != 1 {
			t.Fatalf("%s carried over %d times", c.Key, c.CarriedOver)
		}
	}
	if err := e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1, []string{k3}, domain.MoveIn{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.CompleteSprint(ctx, e.Admin, s1, "next", false); err == nil {
		t.Fatal("a closed sprint was completed twice")
	}
	if cardList(bl.Backlog) != k3 { // k1 is done and k2, k4 are in the next sprint
		t.Fatalf("backlog %s", cardList(bl.Backlog))
	}
	// the move left a history trail
	h, _ := e.Svc.History(ctx, e.Alice, k2)
	trail := false
	for _, x := range h {
		trail = trail || (x.Field == "sprint" && x.After != nil && *x.After == v.Sprints[1].Name)
	}
	if !trail {
		t.Fatalf("no sprint history: %+v", h)
	}
	// retro notes on the closed sprint
	if err := e.Svc.SetRetro(ctx, e.Alice, s1, "What went well: <script>x</script> shipping"); err != nil {
		t.Fatal(err)
	}
	if sv, _ = e.Svc.GetSprint(ctx, e.Alice, s1); !strings.Contains(sv.Retro, "shipping") {
		t.Fatalf("%q", sv.Retro)
	}
}

func TestSPR_CompleteDestinations(t *testing.T) {
	for _, dest := range []string{"backlog", "new", "specific", "next-when-none"} {
		t.Run(dest, func(t *testing.T) {
			e := testutil.New(t)
			e.Project()
			b := scrum(t, e)
			only := 1
			e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{AutoSprints: &only})
			e.Svc.EnsureSprints(ctx, b.ID)
			k := mk(t, e, e.Alice, "Task", "x", "")
			v := backlog(t, e, e.Alice, b)
			e.Svc.AssignSprint(ctx, e.Alice, b.ID, v.Sprints[0].ID, []string{k}, domain.MoveIn{})
			e.Svc.StartSprint(ctx, e.Admin, v.Sprints[0].ID)
			d := dest
			switch dest {
			case "specific":
				extra, _ := e.Svc.CreateSprint(ctx, e.Admin, b.ID, domain.SprintIn{Name: "Extra"})
				d = extra.ID
			case "next-when-none":
				d = "next"
			}
			res, err := e.Svc.CompleteSprint(ctx, e.Admin, v.Sprints[0].ID, d, false)
			if err != nil {
				t.Fatal(err)
			}
			after := backlog(t, e, e.Alice, b)
			switch dest {
			case "backlog":
				if res.Destination != "Backlog" || cardList(after.Backlog) != k {
					t.Fatalf("%+v %s", res, cardList(after.Backlog))
				}
			case "specific":
				if res.Destination != "Extra" {
					t.Fatalf("%+v", res)
				}
			default: // a sprint was created for them
				if len(after.Sprints) != 1 || cardList(after.Sprints[0].Issues) != k {
					t.Fatalf("%+v", after.Sprints)
				}
			}
		})
	}
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	e.Svc.EnsureSprints(ctx, b.ID)
	v := backlog(t, e, e.Alice, b)
	e.Svc.StartSprint(ctx, e.Admin, v.Sprints[0].ID)
	if _, err := e.Svc.CompleteSprint(ctx, e.Admin, v.Sprints[0].ID, "00000000-0000-0000-0000-000000000000", false); err == nil {
		t.Fatal("an unknown destination was accepted")
	}
	other := scrumNamed(t, e, "Other")
	e.Svc.EnsureSprints(ctx, other.ID)
	ov := backlog(t, e, e.Alice, other)
	if _, err := e.Svc.CompleteSprint(ctx, e.Admin, v.Sprints[0].ID, ov.Sprints[0].ID, false); err == nil {
		t.Fatal("a sprint of another board was accepted as the destination")
	}
}

func scrumNamed(t *testing.T, e *testutil.Env, name string) domain.Board {
	b, err := e.Svc.CreateBoard(ctx, e.Admin, "ABC", name, "scrum")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSPR_RolloverJobWithTheClock(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	e.Svc.EnsureSprints(ctx, b.ID)
	k1, k2 := mk(t, e, e.Alice, "Task", "done soon", ""), mk(t, e, e.Alice, "Task", "slips", "")
	v := backlog(t, e, e.Alice, b)
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, v.Sprints[0].ID, []string{k1, k2}, domain.MoveIn{})
	e.Svc.StartSprint(ctx, e.Admin, v.Sprints[0].ID)
	v1, _ := e.Svc.View(ctx, e.Alice, k1)
	e.Svc.Transition(ctx, e.Alice, k1, tidTo(t, v1, "Done"), nil)
	// nothing is due on Friday; at Monday 00:00 (the sprint's exclusive end) the job rolls over
	e.Clock.T = time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC)
	if r, c, err := e.Svc.RunSprintJobs(ctx); err != nil || r != 0 || c != 1 { // nothing is due, but the sprint that started left one future sprint to top up
		t.Fatalf("%d %d %v", r, c, err)
	}
	e.Clock.T = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	r, c, err := e.Svc.RunSprintJobs(ctx)
	if err != nil || r != 1 || c != 1 {
		t.Fatalf("rolled %d created %d %v", r, c, err)
	}
	after := backlog(t, e, e.Alice, b)
	if after.Sprints[0].State != "active" || after.Sprints[0].Name != "Week 2026-W44" || cardList(after.Sprints[0].Issues) != k2 { // the unfinished issue is in the new active sprint
		t.Fatalf("%+v", after.Sprints[0])
	}
	if len(after.Sprints) != 3 || after.Sprints[1].State != "future" { // and two future sprints are ready again
		t.Fatalf("%d sprints", len(after.Sprints))
	}
	if r, c, _ := e.Svc.RunSprintJobs(ctx); r != 0 || c != 0 { // a second run (another node) does nothing
		t.Fatalf("%d %d", r, c)
	}
	// with rollover to the backlog, unfinished work returns to the backlog and the next sprint still starts
	bl := "backlog"
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Rollover: &bl})
	e.Clock.T = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	if r, _, err := e.Svc.RunSprintJobs(ctx); err != nil || r != 1 {
		t.Fatalf("%d %v", r, err)
	}
	after = backlog(t, e, e.Alice, b)
	if cardList(after.Backlog) != k2 || after.Sprints[0].State != "active" || len(after.Sprints[0].Issues) != 0 {
		t.Fatalf("backlog %s active %+v", cardList(after.Backlog), after.Sprints[0].Issues)
	}
	// carried over twice by now
	if after.Backlog[0].CarriedOver != 2 {
		t.Fatalf("carried over %d", after.Backlog[0].CarriedOver)
	}
	// with auto-rollover off nothing happens by itself
	off := false
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{AutoRollover: &off})
	e.Clock.T = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	if r, _, _ := e.Svc.RunSprintJobs(ctx); r != 0 {
		t.Fatal("rolled over with auto-rollover off")
	}
}

func TestSPR_BacklogRankingAndPlanningRules(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	e.Svc.EnsureSprints(ctx, b.ID)
	a, bb, c, d := mk(t, e, e.Alice, "Task", "a", ""), mk(t, e, e.Alice, "Task", "b", ""), mk(t, e, e.Alice, "Task", "c", ""), mk(t, e, e.Alice, "Task", "d", "")
	epic := mk(t, e, e.Alice, "Epic", "epic", "")
	story := mk(t, e, e.Alice, "Story", "story", epic)
	sub := mk(t, e, e.Alice, "Subtask", "sub", story)
	v := backlog(t, e, e.Alice, b)
	s1, s2 := v.Sprints[0].ID, v.Sprints[1].ID
	if strings.Contains(cardList(v.Backlog), epic+" ") && false {
		t.Fatal()
	}
	// top, bottom, before and after in the backlog
	e.Svc.RankInContainer(ctx, e.Alice, b.ID, d, domain.MoveIn{Top: true})
	e.Svc.RankInContainer(ctx, e.Alice, b.ID, a, domain.MoveIn{After: bb})
	v = backlog(t, e, e.Alice, b)
	got := strings.Fields(cardList(v.Backlog))
	if got[0] != d || indexOf(got, bb) > indexOf(got, a) || indexOf(got, a) != indexOf(got, bb)+1 {
		t.Fatalf("%v", got)
	}
	e.Svc.RankInContainer(ctx, e.Alice, b.ID, d, domain.MoveIn{}) // to the bottom
	got = strings.Fields(cardList(backlog(t, e, e.Alice, b).Backlog))
	if got[len(got)-1] != d {
		t.Fatalf("%v", got)
	}
	// planning several at once keeps their order at the requested spot
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1, []string{c, d}, domain.MoveIn{})
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1, []string{a, bb}, domain.MoveIn{Top: true})
	v = backlog(t, e, e.Alice, b)
	if cardList(v.Sprints[0].Issues) != strings.Join([]string{a, bb, c, d}, " ") {
		t.Fatalf("%s", cardList(v.Sprints[0].Issues))
	}
	// an issue is in one open sprint at a time
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, s2, []string{c}, domain.MoveIn{})
	v = backlog(t, e, e.Alice, b)
	if cardList(v.Sprints[0].Issues) != strings.Join([]string{a, bb, d}, " ") || cardList(v.Sprints[1].Issues) != c {
		t.Fatalf("%s | %s", cardList(v.Sprints[0].Issues), cardList(v.Sprints[1].Issues))
	}
	// neighbours must be in the same list
	if err := e.Svc.RankInContainer(ctx, e.Alice, b.ID, a, domain.MoveIn{After: c}); err == nil || !strings.Contains(err.Error(), "not in that list") {
		t.Fatalf("%v", err)
	}
	// epics and subtasks are not planned on their own
	if err := e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1, []string{epic}, domain.MoveIn{}); err == nil || !strings.Contains(err.Error(), "epic") {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1, []string{sub}, domain.MoveIn{}); err == nil || !strings.Contains(err.Error(), "subtask") {
		t.Fatalf("%v", err)
	}
	// trashed issues vanish from every list
	life(t, e, e.Admin, d, domain.LifecycleIn{Op: "delete"})
	v = backlog(t, e, e.Alice, b)
	if strings.Contains(cardList(v.Sprints[0].Issues), d) {
		t.Fatalf("a trashed issue is still planned: %s", cardList(v.Sprints[0].Issues))
	}
	// roles
	viewer, _ := e.Svc.CreateUser(ctx, "v@example.com", "V", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	va := domain.Actor{ID: viewer.ID, Label: "V", Kind: "human"}
	if err := e.Svc.AssignSprint(ctx, va, b.ID, s1, []string{story}, domain.MoveIn{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Backlog(ctx, va, b.ID); err != nil {
		t.Fatalf("a viewer reads the backlog: %v", err)
	}
	out, _ := e.Svc.CreateUser(ctx, "o@example.com", "O", "password123", false)
	if _, err := e.Svc.Backlog(ctx, domain.Actor{ID: out.ID, Kind: "human"}, b.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.GetSprint(ctx, domain.Actor{ID: out.ID, Kind: "human"}, s1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func indexOf(l []string, s string) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return -1
}

func TestSPR_CapacityAndCommitment(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	tm := "time"
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Estimation: &tm})
	e.Svc.EnsureSprints(ctx, b.ID)
	v := backlog(t, e, e.Alice, b)
	s1 := v.Sprints[0]
	// everyone works Mon-Fri 8h (the board calendar): 40 hours each in the sprint week
	if s1.Capacity != 120 || len(s1.People) != 3 { // Alice, Bob and the administrator who created the project
		t.Fatalf("%v %+v", s1.Capacity, s1.People)
	}
	// Bob lives in Israel and works Sun-Thu with a holiday on Monday: his own calendar applies
	il, _ := calendar.Preset("sun-thu", "Asia/Jerusalem")
	il.Holidays = []string{"2026-10-19"}
	if err := e.Svc.SetUserCalendar(ctx, e.Alice, e.Bob.ID, &il); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.SetUserCalendar(ctx, e.Bob, e.Bob.ID, &il); err != nil {
		t.Fatal(err)
	}
	v = backlog(t, e, e.Alice, b)
	// the sprint is Mon 19 - Sun 25 UTC: Bob works Tue, Wed, Thu (Mon is a holiday) and Sun 25 = 4 days
	if v.Sprints[0].Capacity != 40+40+32 {
		t.Fatalf("%v %+v", v.Sprints[0].Capacity, v.Sprints[0].People)
	}
	// time off reduces capacity; negative or silly values are refused; only oneself or an administrator
	if err := e.Svc.SetCapacity(ctx, e.Alice, s1.ID, e.Alice.ID, 8); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.SetCapacity(ctx, e.Alice, s1.ID, e.Bob.ID, 8); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.SetCapacity(ctx, e.Alice, s1.ID, e.Alice.ID, -1); err == nil {
		t.Fatal("negative hours accepted")
	}
	if err := e.Svc.SetCapacity(ctx, e.Admin, s1.ID, e.Bob.ID, 4); err != nil {
		t.Fatal(err)
	}
	v = backlog(t, e, e.Alice, b)
	if v.Sprints[0].Capacity != 32+28+40 {
		t.Fatalf("%v", v.Sprints[0].Capacity)
	}
	// commitment indicator for hour estimates
	k1, k2 := mk(t, e, e.Alice, "Task", "big", ""), mk(t, e, e.Alice, "Task", "small", "")
	est(t, e, k1, 110)
	est(t, e, k2, 2)
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1.ID, []string{k2}, domain.MoveIn{})
	if v = backlog(t, e, e.Alice, b); v.Sprints[0].Commit != "under" {
		t.Fatalf("%q", v.Sprints[0].Commit)
	}
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, s1.ID, []string{k1}, domain.MoveIn{})
	if v = backlog(t, e, e.Alice, b); v.Sprints[0].Commit != "over" || v.Sprints[0].Committed != 112 {
		t.Fatalf("%q %v", v.Sprints[0].Commit, v.Sprints[0].Committed)
	}
	// estimates validate
	neg := -1.0
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, k1, nil, false, domain.Changes{Estimate: &neg}); err == nil {
		t.Fatal("a negative estimate was accepted")
	}
	if is, err := e.Svc.UpdateFields(ctx, e.Alice, k1, nil, false, domain.Changes{ClearEstimate: true}); err != nil || is.Estimate != nil {
		t.Fatalf("%v %+v", err, is.Estimate)
	}
}

func TestSPR_CalendarChangeAndClosedSprintsStayPut(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	e.Svc.EnsureSprints(ctx, b.ID)
	v := backlog(t, e, e.Alice, b)
	active := v.Sprints[0]
	e.Svc.StartSprint(ctx, e.Admin, active.ID)
	il, _ := calendar.Preset("sun-thu", "Asia/Jerusalem")
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Calendar: &il})
	v = backlog(t, e, e.Alice, b)
	if !v.Sprints[0].Start.Equal(active.Start) || v.Sprints[0].Calendar.TimeZone != "UTC" { // the running sprint keeps its boundaries and snapshot
		t.Fatalf("%v %+v", v.Sprints[0].Start, v.Sprints[0].Calendar)
	}
	if v.Sprints[1].Calendar.TimeZone != "Asia/Jerusalem" || v.Sprints[1].Start.Before(v.Sprints[0].End) { // the next one follows the new calendar and never overlaps
		t.Fatalf("%v %v", v.Sprints[1].Start, v.Sprints[0].End)
	}
	// an administrator may recompute the active sprint; a closed one is never recomputed
	rs, err := e.Svc.RecomputeSprint(ctx, e.Admin, active.ID)
	if err != nil || rs.Calendar.TimeZone != "Asia/Jerusalem" || !rs.Start.Before(active.Start) {
		t.Fatalf("%v %+v", err, rs)
	}
	if _, err := e.Svc.RecomputeSprint(ctx, e.Alice, active.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	e.Svc.CompleteSprint(ctx, e.Admin, active.ID, "backlog", false)
	if _, err := e.Svc.RecomputeSprint(ctx, e.Admin, active.ID); err == nil {
		t.Fatal("a closed sprint was recomputed")
	}
	closed, _ := e.Svc.GetSprint(ctx, e.Alice, active.ID)
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Calendar: &calendar.Calendar{TimeZone: "Pacific/Auckland", WeekStart: time.Monday, WorkingDays: il.WorkingDays, Hours: 6, Numbering: "none"}})
	again, _ := e.Svc.GetSprint(ctx, e.Alice, active.ID)
	if !again.Start.Equal(closed.Start) || !again.End.Equal(closed.End) || again.Calendar.TimeZone != closed.Calendar.TimeZone || again.WorkingDays != closed.WorkingDays {
		t.Fatal("a later calendar edit changed a closed sprint")
	}
}

func TestSPR_ParallelSprints(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := scrum(t, e)
	yes := true
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Parallel: &yes})
	e.Svc.EnsureSprints(ctx, b.ID)
	v := backlog(t, e, e.Alice, b)
	for _, sp := range v.Sprints {
		if _, err := e.Svc.StartSprint(ctx, e.Admin, sp.ID); err != nil {
			t.Fatal(err)
		}
	}
	k := mk(t, e, e.Alice, "Task", "x", "")
	e.Svc.AssignSprint(ctx, e.Alice, b.ID, v.Sprints[1].ID, []string{k}, domain.MoveIn{})
	if bv, _ := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil); len(bv.Columns[0].Cards) != 1 { // both active sprints feed the board
		t.Fatalf("%d", len(bv.Columns[0].Cards))
	}
}

// Estimates travel with the issue through both bundle formats.
func TestEXP_EstimateRoundTrips(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			e := testutil.New(t)
			e.Project()
			k := mk(t, e, e.Alice, "Task", "estimated", "")
			est(t, e, k, 3.5)
			o := domain.DefaultExport()
			o.Format = format
			b, _ := exportBytes(t, e, e.Admin, o)
			dst := testutil.New(t)
			dst.WithBlobs()
			importBytes(t, dst, b, domain.ImportOpts{})
			v, err := dst.Svc.View(ctx, dst.Admin, "ABC-1")
			if err != nil || v.Issue.Estimate == nil || *v.Issue.Estimate != 3.5 {
				t.Fatalf("%v %+v", err, v.Issue.Estimate)
			}
		})
	}
}
