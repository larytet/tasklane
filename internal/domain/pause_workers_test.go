package domain_test

import (
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

// PAUSE-13 (SPEC 18.2): background workers in a paused scope stand still. An automation rule whose trigger happened before the pause does
// not act while the pause lasts (and its event is not lost); it acts once, after the pause.
func TestPAUSE13_AutomationStandsStillInAPausedScope(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	rule := r.rule(t, "note", domain.Spec{"type": "issue.transitioned", "to": "Done"}, nil,
		domain.Spec{"kind": "comment", "text": "closed by rule"})
	k := mk(t, e, e.Alice, "Task", "work", "")
	tid, _ := trans(t, e, e.Alice, k, "Resolve")
	if _, err := e.Svc.Transition(ctx, e.Alice, k, tid, nil); err != nil {
		t.Fatal(err)
	}
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil {
		t.Fatal(err)
	}
	r.Svc.ProcessAutomationEvents(ctx, 0)
	if v, _ := e.Svc.View(ctx, e.Alice, k); len(v.Comments) != 0 {
		t.Fatalf("the rule wrote %d comment(s) into a paused project", len(v.Comments))
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		r.run()
		time.Sleep(1100 * time.Millisecond) // the lease cache of a second
	}
	v, _ := e.Svc.View(ctx, e.Alice, k)
	if len(v.Comments) != 1 || v.Comments[0].Body != "closed by rule" {
		t.Fatalf("after the pause the rule must act once: %+v; runs %s", v.Comments, states(r.runs(t, rule.ID)))
	}
}

// A scheduled rule does not act in a paused project; it acts on the tick after the pause.
func TestPAUSE13_ScheduledRulesStandStillInAPausedScope(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	r.rule(t, "nag", domain.Spec{"type": "schedule", "every_minutes": 60, "query": "project = ABC"}, nil, domain.Spec{"kind": "comment", "text": "still open"})
	k := mk(t, e, e.Alice, "Task", "work", "")
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := e.Svc.ProcessAutomationSchedules(ctx); n != 0 {
		t.Fatalf("the scheduled rule ran %d time(s) in a paused project", n)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	e.Clock.T = e.Clock.T.Add(61 * time.Minute)
	if n, _ := e.Svc.ProcessAutomationSchedules(ctx); n != 1 {
		t.Fatalf("the next tick after the pause ran %d time(s)", n)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, k); len(v.Comments) != 1 {
		t.Fatalf("%d comments", len(v.Comments))
	}
}

// The sprint rollover job does not touch a paused project and does it on the next pass after the pause; nothing is skipped.
func TestPAUSE13_SprintRolloverWaitsForThePause(t *testing.T) {
	e, _ := pauseEnv(t)
	b := scrum(t, e)
	e.Svc.EnsureSprints(ctx, b.ID)
	v := backlog(t, e, e.Alice, b)
	e.Svc.StartSprint(ctx, e.Admin, v.Sprints[0].ID)
	e.Clock.T = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) // the sprint's end
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil {
		t.Fatal(err)
	}
	if r, c, err := e.Svc.RunSprintJobs(ctx); r != 0 || c != 0 {
		t.Fatalf("the job changed a paused project: rolled %d, created %d, %v", r, c, err)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if r, _, err := e.Svc.RunSprintJobs(ctx); err != nil || r != 1 {
		t.Fatalf("after the pause: rolled %d, %v", r, err)
	}
}
