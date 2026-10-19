package domain_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
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

// An email reply for an issue in a paused project is given back to the sender or the mailbox reader (a temporary failure), not quarantined; the
// same message is accepted when it comes again after the pause.
func TestPAUSE13_InboundMailIsDeferredNotQuarantined(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	raw := e.eml("Bob R. <bob@example.com>", addr, "Re: [ABC-1] Printer is on fire", []string{authOK}, "Thanks, I will look today."+quoted)
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "restore:test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Svc.IngestMail(ctx, e.trusted.ID, true, raw)
	if !isPaused(err) {
		t.Fatalf("a message for a paused project was %v, want the pause refusal", err)
	}
	var rows int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE message_id='<m1@mail.example.com>'`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("the refused message left %d row(s) behind: a repeat would be taken for a duplicate", rows)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if r := e.ingest(true, raw); r.Outcome != "accepted" || len(e.comments()) != 1 {
		t.Fatalf("after the pause: %+v, %d comment(s)", r, len(e.comments()))
	}
}

// A code event (a push with a smart commit) for a repository mapped to a paused project stays queued, and its commands run after the pause
// instead of being recorded as skipped for good.
func TestPAUSE13_GitEventsWaitForThePause(t *testing.T) {
	r := newCodeRig(t)
	e := r.e
	is := e.Issue("redirect", "")
	start, _ := trans(t, e, e.Alice, is.Key, "Start progress")
	e.Svc.Transition(ctx, e.Alice, is.Key, start, nil)
	r.link(t, "octo", e.Alice)
	r.smart(t, "default_branch")
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil {
		t.Fatal(err)
	}
	msg := is.Key + " #comment fixed the redirect"
	r.push(t, "main", "e1e1e1e1", gitmock.GHCommitBy("e1e1e1e1", msg, "octo", "octo@example.com", t0))
	if len(r.comments(is.Key)) != 0 || len(r.smartRuns(is.Key)) != 0 {
		t.Fatalf("a smart commit ran in a paused project: %q", r.smartRuns(is.Key))
	}
	var queued int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_inbox WHERE processed_at IS NULL AND attempts=0`).Scan(&queued)
	if queued != 1 {
		t.Fatalf("%d delivery(ies) queued with no attempt spent, want 1", queued)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := e.Svc.ProcessGitInbox(ctx, 10); err != nil || n != 1 {
		t.Fatalf("after the pause: %d %v", n, err)
	}
	if got := r.smartRuns(is.Key); len(got) != 1 || !strings.HasPrefix(got[0], "#comment applied") {
		t.Fatalf("%q", got)
	}
}

// Auto-archive leaves a paused project alone without waiting out the admission time for every issue, and archives after the pause.
func TestPAUSE13_AutoArchiveWaitsForThePause(t *testing.T) {
	r := newFields(t)
	e := r.e
	old := mk(t, e, e.Alice, "Task", "resolved long ago", "")
	resolveIssue(t, r, old)
	e.Store.Pool.Exec(ctx, `UPDATE issue SET resolved_at=$2 WHERE key=$1`, old, e.Svc.Now().AddDate(0, 0, -40))
	days := 30
	if err := e.Svc.SetArchivePolicy(ctx, e.Admin, "ABC", &days); err != nil {
		t.Fatal(err)
	}
	domain.PauseAdmitWait = 2 * time.Second
	defer func() { domain.PauseAdmitWait = 150 * time.Millisecond }()
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "restore:test"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if n, err := e.Svc.AutoArchive(ctx, 100); err != nil || n != 0 {
		t.Fatalf("archived %d issue(s) in a paused project: %v", n, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("waited %s for the pause instead of standing still", d)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := e.Svc.AutoArchive(ctx, 100); err != nil || n != 1 {
		t.Fatalf("after the pause: %d %v", n, err)
	}
}

// The thumbnailer leaves the attachments of a paused project alone and makes their thumbnails after the pause.
func TestPAUSE13_ThumbnailsWaitForThePause(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	is := e.Issue("shots", "")
	attach(t, e, e.Alice, is.Key, "shot.png", "image/png", pngBytes(300, 200))
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "restore:test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.Svc.ThumbnailsOnce(ctx); err != nil || n != 0 {
		t.Fatalf("%d thumbnail(s) made in a paused project: %v", n, err)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := e.Svc.ThumbnailsOnce(ctx); err != nil || n != 1 {
		t.Fatalf("after the pause: %d %v", n, err)
	}
}

// Tiering moves nothing while a pause covers the instance or a project that refers to the object, and moves it afterwards.
func TestPAUSE13_TieringWaitsForThePause(t *testing.T) {
	for _, scope := range []string{"project:ABC", "instance"} {
		t.Run(scope, func(t *testing.T) {
			e := newTierEnv(t, nil)
			sha, _ := e.attach("old.log", 2000, 90)
			e.rules(domain.TierRule{Name: "logs", Class: cls("log"), MinAgeDays: 30, Enabled: true})
			lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: scope, Owner: "restore:test"})
			if err != nil {
				t.Fatal(err)
			}
			if run, err := e.Svc.RunTiering(ctx, 10); err != nil || run.Moved != 0 || e.tier(sha) != "hot" {
				t.Fatalf("moved %d under a %s pause: %+v %v", run.Moved, scope, run, err)
			}
			if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond)
			if run, err := e.Svc.RunTiering(ctx, 10); err != nil || run.Moved != 1 || e.tier(sha) != "cold" {
				t.Fatalf("after the pause: %+v %v", run, err)
			}
		})
	}
}
