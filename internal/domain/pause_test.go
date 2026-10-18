package domain_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func init() { domain.PauseAdmitWait = 150 * time.Millisecond }

func pauseEnv(t *testing.T) (*testutil.Env, string) {
	e := testutil.New(t)
	e.Project()
	xyz(t, e)
	return e, mk(t, e, e.Alice, "Task", "pausable", "")
}

func isPaused(err error) bool { var p *domain.ErrPaused; return errors.As(err, &p) }

func TestPAUSE04_WritersAreHeldThenRefusedReadsContinue(t *testing.T) {
	e, key := pauseEnv(t)
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil || lease.State != "paused" {
		t.Fatalf("%v %+v", err, lease)
	}
	hi := "high"
	for name, err := range map[string]error{
		"update": func() error {
			_, e2 := e.Svc.UpdateFields(ctx, e.Alice, key, nil, false, domain.Changes{Priority: &hi})
			return e2
		}(),
		"save": func() error {
			_, e2 := e.Svc.SaveText(ctx, e.Alice, key, domain.SaveIn{Field: "description", Body: "x", BaseSeq: 1})
			return e2
		}(),
		"comment": func() error { _, e2 := e.Svc.AddComment(ctx, e.Alice, key, "c", false); return e2 }(),
		"create": func() error {
			_, e2 := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "x"})
			return e2
		}(),
		"lifecycle": func() error { _, e2 := e.Svc.Lifecycle(ctx, e.Alice, key, domain.LifecycleIn{Op: "clone"}); return e2 }(),
		"link":      func() error { return e.Svc.AddLink(ctx, e.Alice, key, "relates", key) }(),
	} {
		if !isPaused(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the refusal carries what the client needs
	_, perr := e.Svc.AddComment(ctx, e.Alice, key, "c", false)
	var ep *domain.ErrPaused
	if !errors.As(perr, &ep) || ep.Scope != "project:ABC" || ep.RetryAfter < time.Second || ep.Owner != "import:test" {
		t.Fatalf("%+v", ep)
	}
	// reads, presence and drafts are untouched; another project is untouched
	if _, err := e.Svc.View(ctx, e.Alice, key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Heartbeat(ctx, e.Alice, key, "description"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.SaveDraft(ctx, e.Alice, key, "description", "kept while paused", 1); err != nil {
		t.Fatalf("drafts must work during a pause: %v", err)
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "XYZ", Summary: "other project"}); err != nil {
		t.Fatalf("a project pause blocked another project: %v", err)
	}
	// release: writes go through the normal path, and a retry of a save sent before the pause is idempotent
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, e.Alice, key, "after", false); err != nil {
		t.Fatal(err)
	}
	// a write that arrives during the pause and waits is carried out after release, once
	l2, _ := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC"})
	domain.PauseAdmitWait = 3 * time.Second
	defer func() { domain.PauseAdmitWait = 150 * time.Millisecond }()
	done := make(chan error, 1)
	go func() {
		_, err := e.Svc.SaveText(ctx, e.Alice, key, domain.SaveIn{Field: "description", Body: "held then applied", BaseSeq: 1, SaveID: "held-1"})
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the write was not held: %v", err)
	default:
	}
	e.Svc.ReleasePause(ctx, e.Admin, l2.ID)
	if err := <-done; err != nil {
		t.Fatalf("held write failed after release: %v", err)
	}
	if r, err := e.Svc.SaveText(ctx, e.Alice, key, domain.SaveIn{Field: "description", Body: "held then applied", BaseSeq: 1, SaveID: "held-1"}); err != nil || !r.Retried {
		t.Fatalf("retry of the held save: %v %+v", err, r)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, key); v.Issue.DescSeq != 2 {
		t.Fatalf("applied %d times", v.Issue.DescSeq-1)
	}
}

func TestPAUSE06_DrainDeadlineAbortsAndTheScopeRunsAgain(t *testing.T) {
	e, key := pauseEnv(t)
	// a slow writer: its save waits on a row lock held by another session, so it is in flight when the pause is requested
	tx, err := e.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tx.Exec(ctx, `SELECT 1 FROM issue_text WHERE issue_id=(SELECT id FROM issue WHERE key=$1) AND field='description' FOR UPDATE`, key)
	slow := make(chan error, 1)
	go func() {
		_, err := e.Svc.SaveText(ctx, e.Alice, key, domain.SaveIn{Field: "description", Body: "slow", BaseSeq: 1})
		slow <- err
	}()
	time.Sleep(200 * time.Millisecond)
	_, err = e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", DrainWaitMS: 300})
	if !errors.Is(err, domain.ErrDrainTimeout) {
		t.Fatalf("%v", err)
	}
	st, _ := e.Svc.PauseStatus(ctx, e.Admin)
	if len(st) != 0 {
		t.Fatalf("an aborted pause left a lease: %+v", st)
	}
	var aborted int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='pause.aborted'`).Scan(&aborted)
	if aborted != 1 {
		t.Fatal("the abort was not audited")
	}
	// the scope runs again, no data changed by the abort, and the slow writer completes once its lock frees
	if _, err := e.Svc.AddComment(ctx, e.Alice, key, "runs again", false); err != nil {
		t.Fatalf("scope did not resume: %v", err)
	}
	tx.Rollback(ctx)
	if err := <-slow; err != nil {
		t.Fatalf("slow writer: %v", err)
	}
	// and a pause with no in-flight writes succeeds at once
	l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", DrainWaitMS: 300})
	if err != nil {
		t.Fatal(err)
	}
	e.Svc.ReleasePause(ctx, e.Admin, l.ID)
}

func TestPAUSE08_OverlappingScopesAreRejected(t *testing.T) {
	e, _ := pauseEnv(t)
	a, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC"})
	if err != nil {
		t.Fatal(err)
	}
	// the same scope, and the instance over a project, are refused; a different project is fine
	var ce *domain.ConflictError
	for _, sc := range []string{"project:ABC", "instance"} {
		if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: sc}); !errors.As(err, &ce) {
			t.Errorf("%s: %v", sc, err)
		}
	}
	b, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:XYZ"})
	if err != nil {
		t.Fatalf("disjoint projects should coexist: %v", err)
	}
	e.Svc.ReleasePause(ctx, e.Admin, a.ID)
	e.Svc.ReleasePause(ctx, e.Admin, b.ID)
	// instance first, then a project inside it: refused
	c, _ := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "instance"})
	if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC"}); !errors.As(err, &ce) {
		t.Fatalf("%v", err)
	}
	// concurrent requests for the same scope: exactly one is granted
	e.Svc.ReleasePause(ctx, e.Admin, c.ID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "racer"}); err == nil {
				mu.Lock()
				granted++
				mu.Unlock()
				_ = l
			}
		}()
	}
	wg.Wait()
	if granted != 1 {
		t.Fatalf("%d pauses granted for one scope", granted)
	}
	// only administrators pause anything
	if _, err := e.Svc.RequestPause(ctx, e.Alice, domain.PauseIn{Scope: "instance"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:NOPE"}); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestPAUSE07_ALeaseExpiresSoACrashedJobCannotHoldTheSystem(t *testing.T) {
	e, key := pauseEnv(t)
	if _, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "instance", Owner: "crashed job", HoldMS: 5000}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, e.Alice, key, "blocked", false); !isPaused(err) {
		t.Fatalf("%v", err)
	}
	e.Clock.Advance(time.Hour) // the owner never comes back
	if st, _ := e.Svc.PauseStatus(ctx, e.Admin); len(st) != 0 {
		t.Fatalf("expired lease still listed: %+v", st)
	}
	if _, err := e.Svc.AddComment(ctx, e.Alice, key, "free again", false); err != nil {
		t.Fatalf("writes did not resume after the lease expired: %v", err)
	}
}

func TestPAUSE10_12_SnapshotAndNoneHoldNothingAndRecordAMarker(t *testing.T) {
	e, key := pauseEnv(t)
	for _, mode := range []string{"snapshot", "none"} {
		l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "instance", Mode: mode})
		if err != nil || l.State != "none" || l.ID != "" {
			t.Fatalf("%s: %v %+v", mode, err, l)
		}
		var m domain.Marker
		if err := json.Unmarshal(l.Marker, &m); err != nil || m.Mode != mode || m.LSN == "" || m.OutboxID == 0 || m.At.IsZero() {
			t.Fatalf("%s marker: %v %+v", mode, err, m)
		}
		if _, err := e.Svc.AddComment(ctx, e.Alice, key, "not held "+mode, false); err != nil {
			t.Fatalf("%s held a write: %v", mode, err)
		}
		if st, _ := e.Svc.PauseStatus(ctx, e.Admin); len(st) != 0 {
			t.Fatalf("%s left a lease", mode)
		}
	}
	// the marker moves forward with the outbox
	l1, _ := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "instance", Mode: "snapshot"})
	e.Svc.AddComment(ctx, e.Alice, key, "more", false)
	l2, _ := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "instance", Mode: "snapshot"})
	var m1, m2 domain.Marker
	json.Unmarshal(l1.Marker, &m1)
	json.Unmarshal(l2.Marker, &m2)
	if m2.OutboxID <= m1.OutboxID {
		t.Fatalf("marker did not advance: %d -> %d", m1.OutboxID, m2.OutboxID)
	}
	// a real pause is audited at both ends
	p, _ := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC"})
	e.Svc.ReleasePause(ctx, e.Admin, p.ID)
	var started, ended int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='pause.started'`).Scan(&started)
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='pause.ended'`).Scan(&ended)
	if started != 1 || ended != 1 {
		t.Fatalf("audit: started %d ended %d", started, ended)
	}
	_ = context.Background
}
