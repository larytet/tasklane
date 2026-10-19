package domain_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

func barrierGate() domain.Gate {
	return domain.Gate{Name: domain.PauseBarrierG, MinVersion: "1.0.0", Doc: "pause barrier"}
}

// UPG-03 (pause barrier): with every node new enough, a pause is not reported until the other nodes have finished their own writes in the scope.
func TestUPG03_PauseWaitsForTheWritesOfOtherNodes(t *testing.T) {
	e, key := pauseEnv(t)
	e.Svc.Gates = []domain.Gate{barrierGate()}
	e.Svc.NodeID, e.Svc.NodeVersion = "node-a", "1.0.0"
	b := domain.New(e.Store.Pool)
	b.Gates, b.NodeID, b.NodeVersion, b.Now = e.Svc.Gates, "node-b", "1.0.0", e.Svc.Now // one clock for every node, as their corrected clocks are in life
	for _, s := range []*domain.Service{e.Svc, b} {
		if err := s.RegisterNode(ctx); err != nil {
			t.Fatal(err)
		}
	}
	e.Svc.EvaluateGates(ctx)
	if !e.Svc.GateOn(ctx, domain.PauseBarrierG) {
		t.Fatal("the barrier did not turn on with every node new enough")
	}
	bctx, stop := context.WithCancel(ctx)
	defer stop()
	go b.RunPauseAcks(bctx)
	// node b has a write in flight: its save waits on a row lock another session holds
	tx, err := e.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	tx.Exec(ctx, `SELECT 1 FROM issue_text WHERE issue_id=(SELECT id FROM issue WHERE key=$1) AND field='description' FOR UPDATE`, key)
	saved := make(chan error, 1)
	go func() {
		_, err := b.SaveText(ctx, e.Alice, key, domain.SaveIn{Field: "description", Body: "from node b", BaseSeq: 1})
		saved <- err
	}()
	time.Sleep(300 * time.Millisecond)
	type result struct {
		l   *domain.PauseLease
		err error
	}
	got := make(chan result, 1)
	go func() {
		l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", DrainWaitMS: 10000})
		got <- result{l, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("the pause was reported while another node was still writing: %+v %v", r.l, r.err)
	case <-time.After(700 * time.Millisecond):
	}
	// node a already refuses new writes in the scope: what is left is only what node b started before it saw the lease
	domain.PauseAdmitWait = 50 * time.Millisecond
	defer func() { domain.PauseAdmitWait = 150 * time.Millisecond }()
	if _, err := e.Svc.AddComment(ctx, e.Alice, key, "new work", false); !isPaused(err) {
		t.Fatalf("node a took a write while draining: %v", err)
	}
	tx.Rollback(ctx) // node b's write finishes
	if err := <-saved; err != nil {
		t.Fatalf("the write that was in flight: %v", err)
	}
	select {
	case r := <-got:
		if r.err != nil || r.l.State != "paused" {
			t.Fatalf("%+v %v", r.l, r.err)
		}
		var acks int
		e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM pause_ack WHERE node_id='node-b' AND lease_id::text=$1`, r.l.ID).Scan(&acks)
		if acks != 1 {
			t.Fatalf("node b acknowledged %d times", acks)
		}
		// the acknowledgement goes with the lease
		e.Svc.ReleasePause(ctx, e.Admin, r.l.ID)
		e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM pause_ack`).Scan(&acks)
		if acks != 0 {
			t.Fatalf("%d acknowledgements outlived their lease", acks)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pause never completed after the other node finished")
	}
	// a node that never acknowledges holds the pause until its deadline, which then aborts and says who it was waiting for
	d := domain.New(e.Store.Pool)
	d.Gates, d.NodeID, d.NodeVersion, d.Now = e.Svc.Gates, "node-d", "1.0.0", e.Svc.Now
	d.RegisterNode(ctx)
	_, err = e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", DrainWaitMS: 400})
	if !errors.Is(err, domain.ErrDrainTimeout) {
		t.Fatalf("a node that never acknowledges: %v", err)
	}
	var reason string
	e.Store.Pool.QueryRow(ctx, `SELECT detail->>'reason' FROM audit_event WHERE action='pause.aborted' ORDER BY id DESC LIMIT 1`).Scan(&reason)
	if !strings.Contains(reason, "node-d") {
		t.Fatalf("the abort does not say who it waited for: %q", reason)
	}
	if l, _ := e.Svc.PauseStatus(ctx, e.Admin); len(l) != 0 {
		t.Fatalf("an aborted pause left a lease: %+v", l)
	}
	// a node that stops (no heartbeat for half a minute) is no longer waited for
	e.Clock.T = e.Clock.T.Add(time.Minute)
	e.Svc.RegisterNode(ctx)
	b.RegisterNode(ctx)
	if l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", DrainWaitMS: 2000}); err != nil {
		t.Fatalf("a dead node held up a pause: %v", err)
	} else {
		e.Svc.ReleasePause(ctx, e.Admin, l.ID)
	}
}

// UPG-03 (mixed versions): while a node older than the release that acknowledges is live, the barrier is off and a pause behaves as it did (the
// other nodes get a fixed moment); it turns on by itself when the old node is gone.
func TestUPG03_BarrierStaysOffWhileAnOlderNodeIsLive(t *testing.T) {
	e, _ := pauseEnv(t)
	e.Svc.Gates = []domain.Gate{barrierGate()}
	e.Svc.NodeID, e.Svc.NodeVersion = "node-new", "1.0.0"
	old := domain.New(e.Store.Pool)
	old.Gates, old.NodeID, old.NodeVersion, old.Now = e.Svc.Gates, "node-old", "0.9.0", e.Svc.Now
	for _, s := range []*domain.Service{e.Svc, old} {
		s.RegisterNode(ctx)
	}
	e.Svc.EvaluateGates(ctx)
	if e.Svc.GateOn(ctx, domain.PauseBarrierG) {
		t.Fatal("the barrier is on beside a node that cannot acknowledge")
	}
	start := time.Now()
	l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:ABC", DrainWaitMS: 5000})
	if err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a pause beside an older node: %v after %s", err, time.Since(start))
	}
	e.Svc.ReleasePause(ctx, e.Admin, l.ID)
	old.LeaveCluster(ctx)
	e.Svc.EvaluateGates(ctx)
	if !e.Svc.GateOn(ctx, domain.PauseBarrierG) {
		t.Fatal("the barrier did not turn on when the older node left")
	}
	if cl, _ := e.Svc.ClusterStatus(ctx, e.Admin); len(cl.Gates) != 1 || cl.Gates[0].State != "on" {
		t.Fatalf("the cluster status: %+v", cl.Gates)
	}
}
