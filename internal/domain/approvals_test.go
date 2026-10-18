package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

type apr struct {
	*testutil.Env
	carol, dave domain.Actor
}

// newApr: ABC with Alice, Bob, Carol and Dave as members; Eve (a person with no project) for the refusals.
func newApr(t *testing.T) *apr {
	e := testutil.New(t)
	e.Project()
	a := &apr{Env: e}
	a.carol = e.NewUser("carol@example.com", "Carol", false)
	a.dave = e.NewUser("dave@example.com", "Dave", false)
	for _, u := range []domain.Actor{a.carol, a.dave} {
		if err := e.Svc.AddMember(ctx, e.Admin, "ABC", u.ID, "member"); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (a *apr) ask(t *testing.T, key string, in domain.ApprovalIn) domain.Approval {
	t.Helper()
	ap, err := a.Svc.RequestApproval(ctx, a.Alice, key, in)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return ap
}

func (a *apr) decide(t *testing.T, who domain.Actor, id, d string) domain.Approval {
	t.Helper()
	ap, err := a.Svc.DecideApproval(ctx, who, id, d, "")
	if err != nil {
		t.Fatalf("%s decides %s: %v", who.Label, d, err)
	}
	return ap
}

// APR01: the rule decides: all need everyone and one refusal ends it; any needs one and everyone's refusal ends it; n needs n and ends
// as soon as n can no longer be reached.
func TestAPR01_RulesDecideTheOutcome(t *testing.T) {
	a := newApr(t)
	users := []string{a.Bob.ID, a.carol.ID, a.dave.ID}
	type step struct {
		who  domain.Actor
		d    string
		want string
	}
	cases := []struct {
		name  string
		in    domain.ApprovalIn
		steps []step
	}{
		{"all granted", domain.ApprovalIn{Users: users}, []step{{a.Bob, "approve", "pending"}, {a.carol, "approve", "pending"}, {a.dave, "approve", "granted"}}},
		{"all rejected by one", domain.ApprovalIn{Users: users}, []step{{a.Bob, "approve", "pending"}, {a.carol, "reject", "rejected"}}},
		{"any granted by one", domain.ApprovalIn{Users: users, Rule: "any"}, []step{{a.Bob, "reject", "pending"}, {a.carol, "approve", "granted"}}},
		{"any rejected by all", domain.ApprovalIn{Users: users, Rule: "any"}, []step{{a.Bob, "reject", "pending"}, {a.carol, "reject", "pending"}, {a.dave, "reject", "rejected"}}},
		{"2 of 3 granted", domain.ApprovalIn{Users: users, Rule: "n", N: 2}, []step{{a.Bob, "approve", "pending"}, {a.carol, "reject", "pending"}, {a.dave, "approve", "granted"}}},
		{"2 of 3 cannot be reached", domain.ApprovalIn{Users: users, Rule: "n", N: 2}, []step{{a.Bob, "reject", "pending"}, {a.carol, "reject", "rejected"}}},
	}
	for _, c := range cases {
		key := mk(t, a.Env, a.Alice, "Task", c.name, "")
		ap := a.ask(t, key, c.in)
		for _, s := range c.steps {
			if ap = a.decide(t, s.who, ap.ID, s.d); ap.Status != s.want {
				t.Fatalf("%s: after %s %s the approval is %s, want %s", c.name, s.who.Label, s.d, ap.Status, s.want)
			}
		}
	}
	key := mk(t, a.Env, a.Alice, "Task", "validation", "")
	for _, bad := range []domain.ApprovalIn{{}, {Users: users, Rule: "most"}, {Users: users, Rule: "n"}, {Users: users, Rule: "n", N: 4}, {Users: users, Deadline: func() *time.Time { x := a.Svc.Now().Add(-time.Hour); return &x }()}} {
		if _, err := a.Svc.RequestApproval(ctx, a.Alice, key, bad); !isInvalid(err) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
}

// APR02: one approval at a time; a decision is final; only approvers decide, and only people (never an agent) and only while pending.
func TestAPR02_DecisionsAreFinalAndRestricted(t *testing.T) {
	a := newApr(t)
	key := mk(t, a.Env, a.Alice, "Task", "restricted", "")
	ap := a.ask(t, key, domain.ApprovalIn{Users: []string{a.Bob.ID, a.carol.ID}})
	if _, err := a.Svc.RequestApproval(ctx, a.Alice, key, domain.ApprovalIn{Users: []string{a.Bob.ID}}); !isInvalid(err) {
		t.Fatalf("a second open approval: %v", err)
	}
	if _, err := a.Svc.DecideApproval(ctx, a.dave, ap.ID, "approve", ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a person who is not an approver decided: %v", err)
	}
	if _, err := a.Svc.DecideApproval(ctx, a.Alice, ap.ID, "approve", ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the requester decided without being an approver: %v", err)
	}
	agent := a.Bob
	agent.Kind = "agent"
	if _, err := a.Svc.DecideApproval(ctx, agent, ap.ID, "approve", ""); !isInvalid(err) {
		t.Fatalf("an agent decided: %v", err)
	}
	if _, err := a.Svc.DecideApproval(ctx, a.Bob, ap.ID, "maybe", ""); !isInvalid(err) {
		t.Fatalf("a made-up decision: %v", err)
	}
	a.decide(t, a.Bob, ap.ID, "approve")
	if _, err := a.Svc.DecideApproval(ctx, a.Bob, ap.ID, "reject", ""); !isInvalid(err) {
		t.Fatalf("changed a decision: %v", err)
	}
	if got := a.decide(t, a.carol, ap.ID, "approve"); got.Status != "granted" {
		t.Fatalf("status %s", got.Status)
	}
	if _, err := a.Svc.DecideApproval(ctx, a.carol, ap.ID, "approve", ""); !isInvalid(err) {
		t.Fatalf("decided a finished approval: %v", err)
	}
	// now a new request can be made
	a.ask(t, key, domain.ApprovalIn{Users: []string{a.dave.ID}})
	// an unknown id is not found
	if _, err := a.Svc.DecideApproval(ctx, a.Bob, "not-an-id", "approve", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a bad id: %v", err)
	}
}

// APR03: the approver set can come from groups, the reporter, the assignee; approvers must be able to see the project.
func TestAPR03_ApproverSets(t *testing.T) {
	a := newApr(t)
	g, err := a.Svc.CreateGroup(ctx, a.Admin, "Release managers")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []domain.Actor{a.carol, a.dave} {
		if err := a.Svc.AddGroupMember(ctx, a.Admin, g.ID, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	key := mk(t, a.Env, a.Alice, "Task", "sets", "")
	ap := a.ask(t, key, domain.ApprovalIn{Groups: []string{"release managers"}, Derived: []string{"reporter"}, Users: []string{"BOB@example.com"}})
	if len(ap.Approvers) != 4 {
		t.Fatalf("approvers: %+v", ap.Approvers)
	}
	if ap.Needed != 4 {
		t.Fatalf("all needs everyone: %d", ap.Needed)
	}
	// no assignee: derived assignee is refused
	k2 := mk(t, a.Env, a.Alice, "Task", "no assignee", "")
	if _, err := a.Svc.RequestApproval(ctx, a.Alice, k2, domain.ApprovalIn{Derived: []string{"assignee"}}); !isInvalid(err) {
		t.Fatalf("derived assignee of nobody: %v", err)
	}
	// someone with no access to the project cannot be asked
	eve := a.NewUser("eve2@example.com", "Eve Two", false)
	if _, err := a.Svc.RequestApproval(ctx, a.Alice, k2, domain.ApprovalIn{Users: []string{eve.ID}}); !isInvalid(err) || !strings.Contains(err.Error(), "cannot see this project") {
		t.Fatalf("an approver outside the project: %v", err)
	}
	if _, err := a.Svc.RequestApproval(ctx, a.Alice, k2, domain.ApprovalIn{Users: []string{"nobody@example.com"}}); !isInvalid(err) {
		t.Fatalf("an unknown approver: %v", err)
	}
	if _, err := a.Svc.RequestApproval(ctx, a.Alice, k2, domain.ApprovalIn{Groups: []string{"no such group"}}); !isInvalid(err) {
		t.Fatalf("an unknown group: %v", err)
	}
	// a viewer cannot ask
	if err := a.Svc.AddMember(ctx, a.Admin, "ABC", eve.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Svc.RequestApproval(ctx, eve, k2, domain.ApprovalIn{Users: []string{a.Bob.ID}}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer asked: %v", err)
	}
	// someone outside the project cannot even tell the issue exists
	out := a.NewUser("out@example.com", "Outsider", false)
	if _, err := a.Svc.RequestApproval(ctx, out, k2, domain.ApprovalIn{Users: []string{a.Bob.ID}}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider asked: %v", err)
	}
}

// APR04: a decision's comment is in the thread and the history; Mine marks what is waiting for me; cancel is for the asker or an administrator.
func TestAPR04_ThreadHistoryAndMyApprovals(t *testing.T) {
	a := newApr(t)
	key := mk(t, a.Env, a.Alice, "Task", "thread", "")
	ap := a.ask(t, key, domain.ApprovalIn{Users: []string{a.Bob.ID, a.carol.ID}, Rule: "any", Comment: "please look"})
	mine, err := a.Svc.MyApprovals(ctx, a.Bob)
	if err != nil || len(mine) != 1 || !mine[0].Mine || mine[0].Issue != key {
		t.Fatalf("my approvals: %+v %v", mine, err)
	}
	if other, _ := a.Svc.MyApprovals(ctx, a.dave); len(other) != 0 {
		t.Fatalf("someone else's: %+v", other)
	}
	if _, err := a.Svc.DecideApproval(ctx, a.Bob, ap.ID, "approve", "looks <b>good</b> to me"); err != nil {
		t.Fatal(err)
	}
	v, _ := a.Svc.View(ctx, a.Alice, key)
	found := false
	for _, c := range v.Comments {
		if c.Author.ID == a.Bob.ID && strings.HasPrefix(c.Body, "Approved: looks") && !c.Internal {
			found = true
		}
	}
	if !found {
		t.Fatalf("the decision is not in the thread: %+v", v.Comments)
	}
	if len(v.Approvals) != 1 || v.Approvals[0].Status != "granted" || v.Approvals[0].Approvers[0].Comment == "" && v.Approvals[0].Approvers[1].Comment == "" {
		t.Fatalf("view approvals: %+v", v.Approvals)
	}
	var h int
	a.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE i.key=$1 AND h.field='approval'`, key).Scan(&h)
	if h != 2 {
		t.Fatalf("history: %d", h) // requested, and granted
	}
	if mine, _ = a.Svc.MyApprovals(ctx, a.Bob); len(mine) != 0 {
		t.Fatalf("a decided approval still waits: %+v", mine)
	}
	// cancel
	k2 := mk(t, a.Env, a.Alice, "Task", "cancel", "")
	ap2 := a.ask(t, k2, domain.ApprovalIn{Users: []string{a.Bob.ID}})
	if _, err := a.Svc.CancelApproval(ctx, a.carol, ap2.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("someone else cancelled: %v", err)
	}
	if got, err := a.Svc.CancelApproval(ctx, a.Alice, ap2.ID); err != nil || got.Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	if _, err := a.Svc.DecideApproval(ctx, a.Bob, ap2.ID, "approve", ""); !isInvalid(err) {
		t.Fatalf("decided a cancelled approval: %v", err)
	}
	// an administrator may cancel another person's request
	ap3 := a.ask(t, k2, domain.ApprovalIn{Users: []string{a.Bob.ID}})
	if _, err := a.Svc.CancelApproval(ctx, a.Admin, ap3.ID); err != nil {
		t.Fatalf("admin cancel: %v", err)
	}
}

// APR05: the workflow condition approval_granted holds a transition until the latest approval is granted.
func TestAPR05_WorkflowCondition(t *testing.T) {
	a := newApr(t)
	key := mk(t, a.Env, a.Alice, "Task", "gated", "")
	tid := transitionID(t, a.Env, "Resolve")
	if err := a.Svc.SetTransitionRules(ctx, a.Admin, tid, domain.TRules{Conditions: []domain.Spec{{"kind": "approval_granted"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := trans(t, a.Env, a.Alice, key, "Resolve"); ok {
		t.Fatal("offered with no approval")
	}
	ap := a.ask(t, key, domain.ApprovalIn{Users: []string{a.Bob.ID}})
	if _, ok := trans(t, a.Env, a.Alice, key, "Resolve"); ok {
		t.Fatal("offered while the approval is pending")
	}
	a.decide(t, a.Bob, ap.ID, "reject")
	if _, ok := trans(t, a.Env, a.Alice, key, "Resolve"); ok {
		t.Fatal("offered after a rejection")
	}
	ap = a.ask(t, key, domain.ApprovalIn{Users: []string{a.Bob.ID}})
	a.decide(t, a.Bob, ap.ID, "approve")
	id, ok := trans(t, a.Env, a.Alice, key, "Resolve")
	if !ok {
		t.Fatal("not offered after the approval was granted")
	}
	if _, err := a.Svc.Transition(ctx, a.Alice, key, id, nil); err != nil {
		t.Fatal(err)
	}
	// a newer request that is still open closes the gate again
	k2 := mk(t, a.Env, a.Alice, "Task", "gated again", "")
	ap2 := a.ask(t, k2, domain.ApprovalIn{Users: []string{a.Bob.ID}})
	a.decide(t, a.Bob, ap2.ID, "approve")
	a.ask(t, k2, domain.ApprovalIn{Users: []string{a.Bob.ID}})
	if _, ok := trans(t, a.Env, a.Alice, k2, "Resolve"); ok {
		t.Fatal("an older grant opened the gate over a newer open request")
	}
}

// APR06: approvers are told when asked and the asker when decided, once, respecting preferences; a deadline brings one reminder.
func TestAPR06_NotificationsAndReminders(t *testing.T) {
	a := newApr(t)
	key := mk(t, a.Env, a.Alice, "Task", "notified", "")
	soon := a.Svc.Now().Add(2 * time.Hour)
	ap := a.ask(t, key, domain.ApprovalIn{Users: []string{a.Bob.ID, a.carol.ID}, Rule: "any", Deadline: &soon})
	// Carol does not want approval notices
	p := a.Svc.MyNotifyPrefs(ctx, a.carol)
	p.Muted = []string{"approval"}
	if _, err := a.Svc.SetNotifyPrefs(ctx, a.carol, p); err != nil {
		t.Fatal(err)
	}
	notify(t, a.Env)
	bob := inboxOf(t, a.Env, a.Bob, "")
	if len(bob) == 0 || bob[0].Kind != "approval" || !strings.Contains(bob[0].What, "asked you to approve") {
		t.Fatalf("Bob's inbox: %+v", bob)
	}
	for _, n := range inboxOf(t, a.Env, a.carol, "") {
		if n.Kind == "approval" {
			t.Fatalf("a muted kind arrived: %+v", n)
		}
	}
	for _, n := range inboxOf(t, a.Env, a.Alice, "") {
		if n.Kind == "approval" {
			t.Fatalf("the asker was told of their own request: %+v", n)
		}
	}
	// the deadline is within a day: one reminder, to those who have not decided, and only once
	n, err := a.Svc.ApprovalSweep(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if n, _ = a.Svc.ApprovalSweep(ctx); n != 0 {
		t.Fatalf("a second reminder: %d", n)
	}
	notify(t, a.Env)
	got := 0
	for _, x := range inboxOf(t, a.Env, a.Bob, "") {
		if strings.Contains(x.What, "decision") && strings.Contains(x.What, "due") {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("reminders for Bob: %d", got)
	}
	// the decision tells the asker
	a.decide(t, a.Bob, ap.ID, "approve")
	notify(t, a.Env)
	found := false
	for _, x := range inboxOf(t, a.Env, a.Alice, "") {
		if x.Kind == "approval" && strings.Contains(x.What, "Bob") && strings.Contains(x.What, "granted") {
			found = true
		}
	}
	if !found {
		t.Fatalf("Alice was not told: %+v", inboxOf(t, a.Env, a.Alice, ""))
	}
	// a decided approval is not reminded about
	k2 := mk(t, a.Env, a.Alice, "Task", "decided before the sweep", "")
	d2 := a.Svc.Now().Add(time.Hour)
	ap2 := a.ask(t, k2, domain.ApprovalIn{Users: []string{a.Bob.ID}, Deadline: &d2})
	a.decide(t, a.Bob, ap2.ID, "approve")
	if n, _ = a.Svc.ApprovalSweep(ctx); n != 0 {
		t.Fatalf("a reminder for a decided approval: %d", n)
	}
}
