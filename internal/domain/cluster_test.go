package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestUPG04_VersionsAreOrderedAsSemanticVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0}, {"1.2.3", "1.2.4", -1}, {"1.10.0", "1.9.9", 1}, {"v2.0.0", "1.99.99", 1},
		{"1.0.0-rc1", "1.0.0", -1}, {"1.0.0", "1.0.0-rc1", 1}, {"1.0.0-rc1", "1.0.0-rc2", -1},
		{"0.1.0-dev", "0.1.0", 0}, // a development build counts as the release it leads up to
		{"1.2.3+build5", "1.2.3", 0}, {"1.2", "1.2.0", 0},
	} {
		if got := domain.CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("%s vs %s: %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func clusterEnv(t *testing.T, gates ...domain.Gate) *testutil.Env {
	e := testutil.New(t)
	e.Clock.T = time.Now()
	e.Svc.Gates = gates
	return e
}

func (e *clusterNode) beat() {
	e.T.Helper()
	if err := e.Svc.RegisterNode(ctx); err != nil {
		e.T.Fatal(err)
	}
}

type clusterNode struct{ *testutil.Env }

func node(e *testutil.Env, id, version string) *clusterNode {
	e.Svc.NodeID, e.Svc.NodeVersion = id, version
	return &clusterNode{e}
}

func TestUPG04_AGateStaysOffUntilEveryLiveNodeQualifiesThenTurnsOnByItself(t *testing.T) {
	e := clusterEnv(t, domain.Gate{Name: "newthing", MinVersion: "1.1.0", Doc: "needs 1.1"})
	n1 := node(e, "node-a", "1.0.0")
	n1.beat()
	if e.Svc.GateOn(ctx, "newthing") {
		t.Fatal("a gate was on before it was evaluated")
	}
	e.Svc.EvaluateGates(ctx)
	if e.Svc.GateOn(ctx, "newthing") {
		t.Fatal("on while the only node is older")
	}
	// the rollout: node b arrives on the new version while a still runs the old one
	node(e, "node-b", "1.1.0").beat()
	e.Svc.EvaluateGates(ctx)
	if e.Svc.GateOn(ctx, "newthing") {
		t.Fatal("on while a live node is older")
	}
	st, err := e.Svc.ClusterStatus(ctx, e.Admin)
	if err != nil || st.MinVersion != "1.0.0" || len(st.Nodes) != 2 || st.Gates[0].Qualifies || len(st.Gates[0].Blocking) != 1 || !strings.Contains(st.Gates[0].Blocking[0], "node-a") {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := e.Svc.ClusterStatus(ctx, e.Alice); err == nil {
		t.Fatal("a member read the cluster")
	}
	// node a is replaced by the new version: no node is older, the gate turns on without anyone doing anything
	node(e, "node-a", "1.1.0").beat()
	e.Svc.EvaluateGates(ctx)
	if !e.Svc.GateOn(ctx, "newthing") {
		t.Fatal("not on although every node qualifies")
	}
	// a node that stopped reporting no longer counts: the old one that crashed does not hold the gate back for ever
	e2 := clusterEnv(t, domain.Gate{Name: "g2", MinVersion: "2.0.0"})
	node(e2, "old", "1.0.0").beat()
	e2.Clock.Advance(time.Minute)
	node(e2, "new", "2.0.0").beat()
	e2.Svc.EvaluateGates(ctx)
	if !e2.Svc.GateOn(ctx, "g2") {
		t.Fatal("a node that stopped half a minute ago held the gate back")
	}
	// leaving gracefully removes the node at once
	e3 := clusterEnv(t, domain.Gate{Name: "g3", MinVersion: "2.0.0"})
	node(e3, "old", "1.0.0").beat()
	node(e3, "new", "2.0.0").beat()
	e3.Svc.EvaluateGates(ctx)
	if e3.Svc.GateOn(ctx, "g3") {
		t.Fatal("on with an old node live")
	}
	node(e3, "old", "1.0.0").Svc.LeaveCluster(ctx)
	e3.Svc.EvaluateGates(ctx)
	if !e3.Svc.GateOn(ctx, "g3") {
		t.Fatal("did not turn on after the old node left")
	}
	// with nobody registered nothing is known, so nothing turns on
	e4 := clusterEnv(t, domain.Gate{Name: "g4", MinVersion: "0.0.1"})
	e4.Svc.EvaluateGates(ctx)
	if e4.Svc.GateOn(ctx, "g4") {
		t.Fatal("a gate turned on with no node registered")
	}
}

func TestUPG04_ARiskyGateWaitsForAnAdministratorAndTheRollbackIsRefused(t *testing.T) {
	e := clusterEnv(t, domain.Gate{Name: "risky", MinVersion: "2.0.0", Risky: true})
	node(e, "a", "1.9.0").beat()
	e.Svc.EvaluateGates(ctx)
	if err := e.Svc.ConfirmGate(ctx, e.Admin, "risky"); err == nil || !strings.Contains(err.Error(), "1.9.0") {
		t.Fatalf("confirmed a gate no node qualifies for: %v", err)
	}
	node(e, "a", "2.0.0").beat()
	e.Svc.EvaluateGates(ctx)
	if e.Svc.GateOn(ctx, "risky") {
		t.Fatal("a risky gate turned on by itself")
	}
	st, _ := e.Svc.ClusterStatus(ctx, e.Admin)
	if st.Gates[0].State != "pending" || !st.Gates[0].Qualifies {
		t.Fatalf("%+v", st.Gates[0])
	}
	if err := e.Svc.ConfirmGate(ctx, e.Alice, "risky"); err == nil {
		t.Fatal("a member confirmed a gate")
	}
	if err := e.Svc.ConfirmGate(ctx, e.Admin, "nothing"); err == nil {
		t.Fatal("confirmed a gate that does not exist")
	}
	// a node of the old version joins while it waits: it no longer qualifies
	node(e, "b", "1.9.0").beat()
	e.Svc.EvaluateGates(ctx)
	if st, _ := e.Svc.ClusterStatus(ctx, e.Admin); st.Gates[0].State != "off" {
		t.Fatalf("a pending gate stayed pending with an old node live: %+v", st.Gates[0])
	}
	if err := e.Svc.ConfirmGate(ctx, e.Admin, "risky"); err == nil {
		t.Fatal("confirmed with an old node live")
	}
	e.Svc.LeaveCluster(ctx) // b (the current identity) leaves
	e.Svc.EvaluateGates(ctx)
	if err := e.Svc.ConfirmGate(ctx, e.Admin, "risky"); err != nil {
		t.Fatal(err)
	}
	if !e.Svc.GateOn(ctx, "risky") {
		t.Fatal("not on after the confirmation")
	}
	var audits int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='cluster.gate.activated'`).Scan(&audits)
	if audits != 1 {
		t.Fatalf("%d audit rows", audits)
	}
	// it stays on, and a node older than it is refused with the reason, whatever it reports
	if err := e.Svc.CheckNodeAgainstGates(ctx, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.CheckNodeAgainstGates(ctx, "2.1.0"); err != nil {
		t.Fatal(err)
	}
	err := e.Svc.CheckNodeAgainstGates(ctx, "1.9.0")
	if err == nil || !strings.Contains(err.Error(), `"risky"`) || !strings.Contains(err.Error(), "2.0.0") || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("a rollback past an active gate was not refused with the reason: %v", err)
	}
	// and a gate that is on does not turn off when an old node is started anyway
	node(e, "c", "1.9.0").beat()
	e.Svc.EvaluateGates(ctx)
	if !e.Svc.GateOn(ctx, "risky") {
		t.Fatal("an active gate was switched off")
	}
}

func TestUPG04_TheBuiltInGateQualifiesForADevelopmentBuildOfTheCurrentVersion(t *testing.T) {
	e := newInbound(t)
	if e.Svc.GateOn(ctx, domain.MailRetentionG) {
		t.Fatal("the gate was on before any node registered")
	}
	node(e.Env, "only", "0.1.0-dev").beat()
	e.Svc.EvaluateGates(ctx)
	if !e.Svc.GateOn(ctx, domain.MailRetentionG) {
		t.Fatal("a development build of the current version does not qualify for the current gate")
	}
}
