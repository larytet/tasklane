package domain_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// LIFE-16: a bulk job is done once per issue, however many workers there are and wherever they stop.
func TestLIFE16_BulkCloneSurvivesWorkersThatStopAndRunTogether(t *testing.T) {
	e, _ := pauseEnv(t)
	var keys []string
	for i := 0; i < 60; i++ {
		keys = append(keys, mk(t, e, e.Alice, "Task", fmt.Sprintf("item %d", i), ""))
	}
	before := countIssues(t, e)
	j, err := e.Svc.StartBulk(ctx, e.Alice, domain.BulkIn{Keys: keys, Op: domain.LifecycleIn{Op: "clone"}})
	if err != nil || j.Total != 60 || j.State != "pending" {
		t.Fatalf("%v %+v", err, j)
	}
	// a worker that does ten items and "dies" (its lease is left behind)
	if n, err := e.Svc.ProcessBulk(ctx, 10); err != nil || n != 10 {
		t.Fatalf("%d %v", n, err)
	}
	// nobody takes the job while the lease of the dead worker lasts... it was released by the clean stop above, so two live workers race for the rest
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := e.Svc.ProcessBulk(ctx, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	st, _ := e.Svc.BulkStatus(ctx, e.Alice, j.ID)
	if st.State != "done" || st.Done != 60 || st.Failed != 0 {
		t.Fatalf("%+v", st)
	}
	if got := countIssues(t, e) - before; got != 60 {
		t.Fatalf("%d clones made for 60 issues: a clone is made once", got)
	}
}

// A worker that dies with the lease still held: the job waits for the lease to run out, and then another worker carries on from the first
// item that is not finished.
func TestLIFE16_ADeadWorkersJobIsTakenOverAfterItsLease(t *testing.T) {
	e, _ := pauseEnv(t)
	var keys []string
	for i := 0; i < 20; i++ {
		keys = append(keys, mk(t, e, e.Alice, "Task", fmt.Sprintf("item %d", i), ""))
	}
	j, _ := e.Svc.StartBulk(ctx, e.Alice, domain.BulkIn{Keys: keys, Op: domain.LifecycleIn{Op: "delete"}})
	// the first worker does five items and is killed: its lease is left in place, as at the moment of a crash
	if n, err := e.Svc.ProcessBulk(ctx, 5); err != nil || n != 5 {
		t.Fatalf("%d %v", n, err)
	}
	e.Store.Pool.Exec(ctx, `UPDATE bulk_job SET leased_until=$2 WHERE id=$1`, j.ID, e.Svc.Now().Add(time.Minute))
	if n, _ := e.Svc.ProcessBulk(ctx, 100); n != 0 {
		t.Fatalf("another worker took a job whose lease had not run out: %d", n)
	}
	e.Clock.Advance(2 * time.Minute)
	if n, err := e.Svc.ProcessBulk(ctx, 100); err != nil || n != 15 {
		t.Fatalf("the takeover did %d items (%v), want the 15 that were left", n, err)
	}
	st, _ := e.Svc.BulkStatus(ctx, e.Alice, j.ID)
	if st.State != "done" || st.Done != 20 {
		t.Fatalf("%+v", st)
	}
	var trashed int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE deleted_at IS NOT NULL`).Scan(&trashed)
	if trashed != 20 {
		t.Fatalf("%d deleted", trashed)
	}
}

// Items that cannot be done fail one by one with the reason; the rest are done; progress and an estimate are shown while it runs; only the person
// who started it (or an administrator) can see or cancel it.
func TestLIFE16_FailuresProgressAndWhoMaySee(t *testing.T) {
	e, _ := pauseEnv(t)
	good := mk(t, e, e.Alice, "Task", "movable", "")
	other := mk(t, e, e.Alice, "Task", "movable too", "")
	// in XYZ Alice may read but not change: that one fails with the reason, the others are done
	e.Svc.AddMember(ctx, e.Admin, "XYZ", e.Alice.ID, "viewer")
	is, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Summary: "read only for Alice"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Svc.StartBulk(ctx, e.Alice, domain.BulkIn{Keys: []string{good, is.Key, other, good}, Op: domain.LifecycleIn{Op: "delete"}})
	if err != nil || j.Total != 3 { // the repeated key counts once
		t.Fatalf("%v %+v", err, j)
	}
	// an issue nobody told Alice about is refused up front, the same as one that does not exist
	for _, k := range []string{"ABC-9999", "NOPE-1"} {
		if _, err := e.Svc.StartBulk(ctx, e.Alice, domain.BulkIn{Keys: []string{k}, Op: domain.LifecycleIn{Op: "delete"}}); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("%s: %v", k, err)
		}
	}
	if _, err := e.Svc.BulkStatus(ctx, e.Bob, j.ID); err == nil {
		t.Fatal("another person saw the job")
	}
	if err := e.Svc.CancelBulk(ctx, e.Bob, j.ID); err == nil {
		t.Fatal("another person cancelled the job")
	}
	if _, err := e.Svc.BulkStatus(ctx, e.Admin, j.ID); err != nil {
		t.Fatalf("an administrator cannot see it: %v", err)
	}
	e.Svc.ProcessBulk(ctx, 1) // one item
	e.Clock.Advance(time.Minute)
	st, _ := e.Svc.BulkStatus(ctx, e.Alice, j.ID)
	if st.State != "running" || st.Done+st.Failed != 1 || st.ETA == nil {
		t.Fatalf("progress: %+v", st)
	}
	e.Svc.ProcessBulk(ctx, 100)
	st, _ = e.Svc.BulkStatus(ctx, e.Alice, j.ID)
	if st.State != "done" || st.Done != 2 || st.Failed != 1 || len(st.Errors) != 1 || st.Errors[0].Key != is.Key {
		t.Fatalf("%+v", st)
	}
	// purge stays one at a time; unknown operations and empty jobs are refused
	for _, bad := range []domain.BulkIn{{Keys: []string{good}, Op: domain.LifecycleIn{Op: "purge"}}, {Keys: nil, Op: domain.LifecycleIn{Op: "delete"}}, {Keys: []string{good}, Op: domain.LifecycleIn{Op: "explode"}}} {
		if _, err := e.Svc.StartBulk(ctx, e.Alice, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	// a cancelled job stops after the item in flight
	more := []string{mk(t, e, e.Alice, "Task", "x", ""), mk(t, e, e.Alice, "Task", "y", ""), mk(t, e, e.Alice, "Task", "z", "")}
	j2, _ := e.Svc.StartBulk(ctx, e.Alice, domain.BulkIn{Keys: more, Op: domain.LifecycleIn{Op: "delete"}})
	e.Svc.ProcessBulk(ctx, 1)
	if err := e.Svc.CancelBulk(ctx, e.Alice, j2.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.Svc.ProcessBulk(ctx, 100); n != 0 {
		t.Fatalf("a cancelled job went on: %d", n)
	}
	if st, _ = e.Svc.BulkStatus(ctx, e.Alice, j2.ID); st.State != "cancelled" || st.Done != 1 {
		t.Fatalf("%+v", st)
	}
}

// A pause on the project makes the worker stop where it is; the job goes on after the pause and nothing is lost or done twice.
func TestLIFE16_BulkJobWaitsForAPause(t *testing.T) {
	e, _ := pauseEnv(t)
	var keys []string
	for i := 0; i < 6; i++ {
		keys = append(keys, mk(t, e, e.Alice, "Task", fmt.Sprintf("p%d", i), ""))
	}
	j, _ := e.Svc.StartBulk(ctx, e.Alice, domain.BulkIn{Keys: keys, Op: domain.LifecycleIn{Op: "delete"}})
	e.Svc.ProcessBulk(ctx, 2)
	lease, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "import:test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := e.Svc.ProcessBulk(ctx, 100); n != 0 {
		t.Fatalf("%d items done in a paused project", n)
	}
	st, _ := e.Svc.BulkStatus(ctx, e.Alice, j.ID)
	if st.Failed != 0 || st.Done != 2 {
		t.Fatalf("the pause failed items: %+v", st)
	}
	if err := e.Svc.ReleasePause(ctx, e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, _ := e.Svc.ProcessBulk(ctx, 100); n != 4 {
		t.Fatalf("after the pause: %d", n)
	}
	if st, _ = e.Svc.BulkStatus(ctx, e.Alice, j.ID); st.State != "done" || st.Done != 6 {
		t.Fatalf("%+v", st)
	}
}

func countIssues(t *testing.T, e *testutil.Env) (n int) {
	t.Helper()
	if err := e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return
}
