package domain_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/redis"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/redismock"
)

// busNode is one server process of a cluster: its own service on the shared database, its own link to the shared Redis, its own pages
// (modelled by the events and presence changes its listeners see).
type busNode struct {
	svc    *domain.Service
	bus    *redis.Bus
	mu     sync.Mutex
	events []domain.Event
	remote atomic.Int64 // presence changes announced by other nodes
}

func (n *busNode) seen(typ string) (c int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, e := range n.events {
		if e.Type == typ {
			c++
		}
	}
	return
}

func newBusNode(t *testing.T, e *testutil.Env, srv *redismock.Server, name string, first bool) *busNode {
	n := &busNode{}
	if first {
		n.svc = e.Svc
	} else {
		n.svc = domain.New(e.Store.Pool)
		n.svc.Presence = domain.NewPresence(e.Svc.Now)
	}
	n.svc.NodeID, n.svc.NodeVersion = name, "1.0.0"
	o, err := redis.ParseURL("redis://" + name + ":x@" + srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	n.bus = redis.New(o)
	n.svc.AttachBus(domain.RedisBus(n.bus))
	n.svc.OnEvent(func(ev domain.Event) { n.mu.Lock(); n.events = append(n.events, ev); n.mu.Unlock() })
	n.svc.OnRemotePresence = func(string) { n.remote.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	go n.bus.Run(ctx)
	t.Cleanup(cancel)
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func busCluster(t *testing.T) (*busNode, *busNode, *redismock.Server, *testutil.Env, string) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	srv := redismock.New(t)
	a := newBusNode(t, e, srv, "node-a", true)
	b := newBusNode(t, e, srv, "node-b", false)
	is := e.Issue("shared issue", "v1")
	waitFor(t, "both nodes subscribed", func() bool {
		return srv.Subscribers("tasklane:cluster") == 2 && a.bus.Status().Connected && b.bus.Status().Connected
	})
	return a, b, srv, e, is.Key
}

func comment(t *testing.T, n *busNode, e *testutil.Env, key, body string) {
	t.Helper()
	if _, err := n.svc.AddComment(ctx, e.Alice, key, body, false); err != nil {
		t.Fatalf("a save on %s: %v", n.svc.NodeID, err)
	}
}

// Events and presence reach the other node's pages, once, and not back to the node that made them.
func TestCLUSTER_EventsAndPresenceReachOtherNodes(t *testing.T) {
	a, b, _, e, key := busCluster(t)
	comment(t, a, e, key, "from a")
	waitFor(t, "b hears a's change", func() bool { return b.seen("comment.added") == 1 })
	time.Sleep(100 * time.Millisecond)
	if a.seen("comment.added") != 1 || b.seen("comment.added") != 1 {
		t.Fatalf("a saw %d, b saw %d: one change is one event on each node", a.seen("comment.added"), b.seen("comment.added"))
	}
	comment(t, b, e, key, "from b")
	waitFor(t, "a hears b's change", func() bool { return a.seen("comment.added") == 2 })
	// presence
	if _, err := a.svc.Heartbeat(ctx, e.Alice, key, "description"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "b sees alice editing", func() bool {
		for _, p := range b.svc.Presence.List(key, e.Bob.ID) {
			if p.UserID == e.Alice.ID && p.Editing == "description" {
				return true
			}
		}
		return false
	})
	if b.remote.Load() == 0 {
		t.Fatal("b's pages were not told that presence changed")
	}
	a.svc.LeavePresence(key, e.Alice.ID)
	waitFor(t, "b sees alice leave", func() bool { return len(b.svc.Presence.List(key, e.Bob.ID)) == 0 })
	if st, ok := a.svc.BusLink(); !ok || !st.Connected || st.Published == 0 {
		t.Fatalf("the bus link: %+v %v", st, ok)
	}
	cl, _ := a.svc.ClusterStatus(ctx, e.Admin)
	if cl.Events == nil || !cl.Events.Connected {
		t.Fatalf("the cluster status does not show the bus: %+v", cl.Events)
	}
}

// CHAOS-02: Redis goes away and comes back. Saves are never affected; each node's own pages stay live, the other node's changes and presence are
// missing while it is down and resume when it returns; a message sent into the outage does not arrive late.
func TestCHAOS02_RedisDownThenRestored(t *testing.T) {
	a, b, srv, e, key := busCluster(t)
	comment(t, a, e, key, "before")
	waitFor(t, "before reaches b", func() bool { return b.seen("comment.added") == 1 })
	srv.Stop()
	waitFor(t, "both nodes notice", func() bool { return !a.bus.Status().Connected && !b.bus.Status().Connected })
	if st, _ := a.svc.BusLink(); st.Connected {
		t.Fatal("the service reports a bus that is down as connected")
	}
	// saves on both nodes still work, and fast
	for i := 0; i < 5; i++ {
		start := time.Now()
		comment(t, a, e, key, fmt.Sprintf("during a %d", i))
		comment(t, b, e, key, fmt.Sprintf("during b %d", i))
		if time.Since(start) > 2*time.Second {
			t.Fatalf("saves slowed to %s while Redis was down", time.Since(start))
		}
	}
	// each node's own pages are still live; the other node's are not told
	if a.seen("comment.added") != 6 || b.seen("comment.added") != 6 {
		t.Fatalf("own-node events: a %d, b %d (want 1 before + 5 of its own, and the other's 5 missing)", a.seen("comment.added"), b.seen("comment.added"))
	}
	// presence is local only
	a.svc.Heartbeat(ctx, e.Alice, key, "description")
	time.Sleep(200 * time.Millisecond)
	if len(a.svc.Presence.List(key, e.Bob.ID)) != 1 || len(b.svc.Presence.List(key, e.Admin.ID)) != 0 {
		t.Fatal("presence crossed a dead link, or was lost locally")
	}
	srv.Start()
	waitFor(t, "both reconnect", func() bool {
		return a.bus.Status().Connected && b.bus.Status().Connected && srv.Subscribers("tasklane:cluster") == 2
	})
	comment(t, a, e, key, "after")
	waitFor(t, "after reaches b", func() bool { return b.seen("comment.added") == 7 })
	if a.seen("comment.added") != 7 {
		t.Fatalf("a saw %d", a.seen("comment.added"))
	}
	a.svc.Heartbeat(ctx, e.Alice, key, "description")
	waitFor(t, "presence resumes", func() bool { return len(b.svc.Presence.List(key, e.Admin.ID)) == 1 })
	// nothing was lost where it matters: every comment is in the database
	v, _ := e.Svc.View(ctx, e.Alice, key)
	if len(v.Comments) != 12 {
		t.Fatalf("%d comments in the database, want 12", len(v.Comments))
	}
	if st, _ := a.svc.BusLink(); st.Dropped == 0 && st.Published == 0 {
		t.Fatalf("no counters moved: %+v", st)
	}
}

// CHAOS-04: a partition between nodes (one node cannot reach the other's events). Saves on either side stay correct because the database decides:
// two edits from the same base cannot both win. Presence is local to each side; events resume on heal, and the cut-off side catches up from the
// database.
func TestCHAOS04_PartitionBetweenNodes(t *testing.T) {
	a, b, srv, e, key := busCluster(t)
	srv.Cut("node-b")
	waitFor(t, "b cut off", func() bool { return !b.bus.Status().Connected })
	if !a.bus.Status().Connected {
		t.Fatal("the node that was not cut lost its link")
	}
	// two people edit the same text from the same version, one on each side of the partition
	var wg sync.WaitGroup
	res := make([]error, 2)
	for i, n := range []*busNode{a, b} {
		wg.Add(1)
		go func(i int, n *busNode) {
			defer wg.Done()
			_, res[i] = n.svc.SaveText(ctx, e.Alice, key, domain.SaveIn{Field: "description", Body: fmt.Sprintf("edit from %s", n.svc.NodeID), BaseSeq: 1})
		}(i, n)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range res {
		var tc *domain.TextConflict
		switch {
		case err == nil:
			wins++
		case errors.As(err, &tc):
			conflicts++
		default:
			t.Fatalf("a save failed with something other than a conflict: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("%d saves won and %d conflicted from the same base: the database must decide exactly one", wins, conflicts)
	}
	// each side's own work shows on its own side only
	comment(t, a, e, key, "a's side")
	comment(t, b, e, key, "b's side")
	time.Sleep(200 * time.Millisecond)
	if b.seen("comment.added") != 1 || a.seen("comment.added") != 1 {
		t.Fatalf("events crossed the partition: a %d, b %d", a.seen("comment.added"), b.seen("comment.added"))
	}
	a.svc.Heartbeat(ctx, e.Alice, key, "")
	time.Sleep(150 * time.Millisecond)
	if len(b.svc.Presence.List(key, e.Admin.ID)) != 0 {
		t.Fatal("presence crossed the partition")
	}
	// the cut-off side still reads the truth from the database
	vb, err := b.svc.View(ctx, e.Alice, key)
	if err != nil || len(vb.Comments) != 2 {
		t.Fatalf("the cut-off node's view: %d comments, %v", len(vb.Comments), err)
	}
	srv.Heal("node-b")
	waitFor(t, "b reconnects", func() bool { return b.bus.Status().Connected && srv.Subscribers("tasklane:cluster") == 2 })
	comment(t, a, e, key, "after the heal")
	waitFor(t, "events resume", func() bool { return b.seen("comment.added") == 2 })
	a.svc.Heartbeat(ctx, e.Alice, key, "description")
	waitFor(t, "presence resumes", func() bool { return len(b.svc.Presence.List(key, e.Admin.ID)) == 1 })
}
