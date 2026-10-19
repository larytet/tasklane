package domain_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
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

// The trash purge does not delete from a paused project; it does it on the next pass.
func TestPAUSE13_TrashPurgeWaitsForThePause(t *testing.T) {
	e, key := pauseEnv(t)
	life(t, e, e.Alice, key, domain.LifecycleIn{Op: "delete"})
	e.Clock.Advance(31 * 24 * time.Hour)
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "restore:test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.Svc.PurgeExpired(ctx); err != nil || n != 0 {
		t.Fatalf("purged %d issue(s) from a paused project: %v", n, err)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := e.Svc.PurgeExpired(ctx); err != nil || n != 1 {
		t.Fatalf("after the pause: %d %v", n, err)
	}
}

// A workflow job (an outbound webhook of a transition rule) for an issue in a paused project is not sent and spends no attempt; it is sent after the pause.
func TestPAUSE13_WorkflowJobsWaitForThePause(t *testing.T) {
	e, key := pauseEnv(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	params, _ := json.Marshal(map[string]any{"url": srv.URL, "allow_private": true})
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO workflow_job(kind,issue_id,params,next_at) SELECT 'webhook',id,$1,$2 FROM issue WHERE key=$3`, params, e.Clock.Now(), key); err != nil {
		t.Fatal(err)
	}
	e.Svc.HookEgress = func(p egress.Policy) egress.Policy { p.AllowPrivate = true; return p }
	e.Svc.WebhookAllowPrivate = true
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.Svc.ProcessWorkflowJobs(ctx, 10); err != nil || n != 0 || hits.Load() != 0 {
		t.Fatalf("a job ran in a paused project: %d %v, %d request(s)", n, err, hits.Load())
	}
	var attempts int
	e.Store.Pool.QueryRow(ctx, `SELECT attempts FROM workflow_job`).Scan(&attempts)
	if attempts != 0 {
		t.Fatalf("the pause spent %d attempt(s)", attempts)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	e.Clock.Advance(10 * time.Second)
	if n, err := e.Svc.ProcessWorkflowJobs(ctx, 10); err != nil || n != 1 || hits.Load() != 1 {
		t.Fatalf("after the pause: %d %v, %d request(s)", n, err, hits.Load())
	}
}

// The service level sweep leaves the clocks of a paused project alone and catches up after.
func TestPAUSE13_SLASweepWaitsForThePause(t *testing.T) {
	e, key := pauseEnv(t)
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO sla_clock(issue_id,kind,goal_minutes,started_at) SELECT id,'resolution',1,$1 FROM issue WHERE key=$2`, e.Clock.Now().Add(-time.Hour), key); err != nil {
		t.Fatal(err)
	}
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.Svc.SLASweep(ctx); err != nil || n != 0 {
		t.Fatalf("the sweep looked at %d clock(s) in a paused project: %v", n, err)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := e.Svc.SLASweep(ctx); err != nil || n != 1 {
		t.Fatalf("after the pause: %d %v", n, err)
	}
}
