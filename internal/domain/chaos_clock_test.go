package domain_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// CHAOS-08: two nodes whose clocks are an hour apart, one ahead and one behind, write to the same issues. The order of what people
// see is the order the database accepted it in (versions, sequence numbers, identifiers), never the order of the nodes' clocks.
func TestCHAOS08_NodesWithSkewedClocksStillAgreeOnOrder(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	ahead := domain.New(e.Store.Pool)
	behind := domain.New(e.Store.Pool)
	// the machines' own clocks are an hour apart; Now is each node's clock corrected to the database's, which a node does by itself
	// every few seconds (SyncClock). Ordering never depended on the clocks; leases and expiries do, so they are checked below.
	ahead.Clock = func() time.Time { return time.Now().Add(time.Hour) }
	behind.Clock = func() time.Time { return time.Now().Add(-time.Hour) }
	nodes := []*domain.Service{ahead, behind}

	is, err := ahead.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "skew", Description: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	// comments alternate between the nodes: the thread must come back in the order they were written
	for i := 0; i < 12; i++ {
		if _, err := nodes[i%2].AddComment(ctx, e.Alice, is.Key, fmt.Sprintf("comment %02d", i), false); err != nil {
			t.Fatal(err)
		}
	}
	v, err := e.Svc.View(ctx, e.Alice, is.Key)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range v.Comments {
		if want := fmt.Sprintf("comment %02d", i); c.Body != want {
			t.Fatalf("the thread is out of order at %d: %q, want %q (a node's clock decided)", i, c.Body, want)
		}
	}
	// text saves alternate with the version each side last saw: every save is accepted once, in order, with no gaps
	seq := v.Issue.DescSeq
	for i := 0; i < 8; i++ {
		body := fmt.Sprintf("v%d", i+2)
		r, err := nodes[i%2].SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: body, BaseSeq: seq, Mode: "save", SaveID: fmt.Sprintf("skew-%d", i)})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		if r.Seq != seq+1 {
			t.Fatalf("save %d got version %d, want %d", i, r.Seq, seq+1)
		}
		seq = r.Seq
	}
	// a node with a stale view conflicts however its clock reads
	if _, err := behind.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: "stale", BaseSeq: seq - 3, Mode: "save", SaveID: "stale-1"}); err == nil {
		t.Fatal("a save based on an old version was accepted by the node whose clock is behind")
	}
	// history lists the changes in the order they happened, not the order of the clocks
	prio := []string{"highest", "high", "medium", "low", "lowest", "high", "medium", "low", "highest", "lowest"}
	for i, p := range prio {
		p := p
		if _, err := nodes[i%2].UpdateFields(ctx, e.Alice, is.Key, nil, false, domain.Changes{Priority: &p}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := e.Svc.History(ctx, e.Alice, is.Key)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range h {
		if x.Field == "priority" && x.After != nil {
			got = append(got, *x.After)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(prio) {
		t.Fatalf("history is in the order of the clocks, not of the changes:\n got  %v\n want %v", got, prio)
	}
	// leases: a claim made by one node must be believed by the other. Before a node has compared its clock with the database's, a
	// node that is behind writes a lease that already looks expired to everyone else (a control, so that the check below means something)
	key2 := e.Issue("lease skew", "x").Key
	if _, err := behind.ClaimIssue(ctx, e.Alice, key2, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := ahead.ClaimIssue(ctx, e.Bob, key2, 10*time.Minute); err != nil {
		t.Fatalf("control: with uncorrected clocks the lease should have looked expired (%v); the check below would prove nothing", err)
	}
	// with the clocks corrected the claim holds, whichever node asks
	offA, err := ahead.SyncClock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	offB, _ := behind.SyncClock(ctx)
	if offA > -59*time.Minute || offA < -61*time.Minute || offB < 59*time.Minute || offB > 61*time.Minute {
		t.Fatalf("the offsets were measured as %s and %s, want about -1h and +1h", offA, offB)
	}
	key3 := e.Issue("lease skew two", "x").Key
	if _, err := behind.ClaimIssue(ctx, e.Alice, key3, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := ahead.ClaimIssue(ctx, e.Bob, key3, 10*time.Minute); err == nil {
		t.Fatal("a node whose clock is an hour ahead took a lease another node had just made")
	} else if _, ok := err.(*domain.LeaseHeld); !ok {
		t.Fatalf("%v", err)
	}
	if _, err := ahead.ClaimIssue(ctx, e.Alice, key3, 10*time.Minute); err != nil {
		t.Fatalf("the holder could not renew through the other node: %v", err)
	}
	// stored times now agree with the database's, from either node
	var skewMS int64
	e.Store.Pool.QueryRow(ctx, `SELECT abs(extract(epoch FROM (expires_at - clock_timestamp())) * 1000 - 600000)::bigint FROM issue_lease l JOIN issue i ON i.id=l.issue_id WHERE i.number=(SELECT number FROM issue WHERE id=l.issue_id) AND l.holder_id::text=$1`, e.Alice.ID).Scan(&skewMS)
	if skewMS > 5000 {
		t.Fatalf("the stored lease expiry is %d ms away from ten minutes after the database's now", skewMS)
	}
	// the operator is told when a node's clock is far off
	ahead.NodeID, ahead.NodeVersion = "ahead-node", "1.0.0"
	if err := ahead.RegisterNode(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := e.Svc.ClusterStatus(ctx, e.Admin)
	if err != nil || len(st.Nodes) != 1 || st.Nodes[0].ClockOffsetMS < 3500000 || st.Nodes[0].ClockWarning == "" {
		t.Fatalf("%+v %v", st.Nodes, err)
	}
}
