package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

type autoRig struct{ *testutil.Env }

func newAuto(t *testing.T) *autoRig {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	e.Svc.Matcher = search.Matcher{Engine: &search.Postgres{DB: e.Store.Pool}, Now: e.Svc.Now}
	r := &autoRig{e}
	r.run() // the automation cursor starts at the end of history: rules act on what happens from now on
	return r
}

func (r *autoRig) run() int {
	r.T.Helper()
	total := 0
	for i := 0; i < 20; i++ { // rule actions make events of their own: run until quiet
		n, err := r.Svc.ProcessAutomationEvents(ctx, 0)
		if err != nil {
			r.T.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	return total
}

func (r *autoRig) rule(t *testing.T, name string, trigger domain.Spec, conds []domain.Spec, actions ...domain.Spec) domain.AutoRule {
	t.Helper()
	ru, err := r.Svc.SaveAutoRule(ctx, r.Admin, 0, domain.AutoRule{Name: name, Project: "ABC", Enabled: true, Trigger: trigger, Conditions: conds, Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	return ru
}

func (r *autoRig) runs(t *testing.T, id int64) []domain.AutoRun {
	rs, err := r.Svc.AutoRuns(ctx, r.Admin, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func states(rs []domain.AutoRun) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.State)
	}
	return strings.Join(s, ",")
}

func TestCORE10_AutomationRunsActionsAsAutomationAndOnlyOnce(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	rule := r.rule(t, "close out", domain.Spec{"type": "issue.transitioned", "to": "Done"},
		[]domain.Spec{{"kind": "query", "query": "priority = high"}},
		domain.Spec{"kind": "comment", "text": "{{issue.key}} ({{issue.summary}}) went {{trigger.from}} to {{trigger.to}} by {{actor}} [{{rule.name}}] {{unknown.thing}}"},
		domain.Spec{"kind": "assign", "to": "reporter"},
		domain.Spec{"kind": "set_field", "field": "labels", "add": []string{"closed"}},
		domain.Spec{"kind": "notify", "users": []string{"bob@example.com"}, "text": "{{issue.key}} is done"})
	hi := "high"
	a := mk(t, e, e.Alice, "Task", "matching", "")
	b := mk(t, e, e.Alice, "Task", "not matching", "")
	bob := e.Bob.ID
	e.Svc.UpdateFields(ctx, e.Alice, a, nil, false, domain.Changes{Priority: &hi, Assignee: &bob})
	for _, k := range []string{a, b} {
		tid, _ := trans(t, e, e.Alice, k, "Resolve")
		e.Svc.Transition(ctx, e.Alice, k, tid, nil)
	}
	if n := r.run(); n != 2 {
		t.Fatalf("%d rule runs", n)
	}
	v, _ := e.Svc.View(ctx, e.Admin, a)
	last := v.Comments[len(v.Comments)-1]
	if last.Body != a+" (matching) went To Do to Done by Alice K. [close out] {{unknown.thing}}" {
		t.Fatalf("comment %q", last.Body)
	}
	if v.Issue.Assignee == nil || v.Issue.Assignee.ID != e.Alice.ID || strings.Join(v.Issue.Labels, ",") != "closed" {
		t.Fatalf("%+v", v.Issue)
	}
	h, _ := e.Svc.History(ctx, e.Admin, a)
	var by []string
	for _, x := range h {
		if x.Field == "assignee" || x.Field == "labels" {
			by = append(by, x.Actor)
		}
	}
	if len(by) == 0 || by[len(by)-1] != "Automation: close out" {
		t.Fatalf("history actors %v", by)
	}
	vb, _ := e.Svc.View(ctx, e.Admin, b)
	if len(vb.Comments) != 0 || len(vb.Issue.Labels) != 0 {
		t.Fatal("the rule ran on an issue that does not match its condition")
	}
	if got := states(r.runs(t, rule.ID)); got != "skipped,done" {
		t.Fatalf("run states %q", got)
	}
	if ns := inboxOf(t, e, e.Bob, ""); len(ns) == 0 || ns[0].Kind != "automation" || ns[0].What != a+" is done" {
		t.Fatalf("%+v", ns)
	}
	// replaying every event does nothing twice
	e.Store.Pool.Exec(ctx, `UPDATE outbox_cursor SET last_id=0 WHERE consumer='automation'`)
	r.run()
	v2, _ := e.Svc.View(ctx, e.Admin, a)
	if len(v2.Comments) != len(v.Comments) || len(r.runs(t, rule.ID)) != 2 {
		t.Fatalf("a replay repeated work: %d comments, %d runs", len(v2.Comments), len(r.runs(t, rule.ID)))
	}
}

func TestCORE10_AutomationLoopsAreCut(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	// a rule that comments whenever there is a comment would answer itself for ever
	echo := r.rule(t, "echo", domain.Spec{"type": "comment.added"}, nil, domain.Spec{"kind": "comment", "text": "echo"})
	k := mk(t, e, e.Alice, "Task", "loop", "")
	e.Svc.AddComment(ctx, e.Alice, k, "hello", false)
	r.run()
	v, _ := e.Svc.View(ctx, e.Admin, k)
	if len(v.Comments) != 2 { // the person's comment and one echo
		t.Fatalf("%d comments: the rule answered itself", len(v.Comments))
	}
	if got := states(r.runs(t, echo.ID)); got != "loop,done" {
		t.Fatalf("run states %q", got)
	}
	// two rules that undo each other: Done reopens, reopening resolves
	a := r.rule(t, "reopen when done", domain.Spec{"type": "issue.transitioned", "to": "Done"}, nil, domain.Spec{"kind": "transition", "transition": "Reopen"})
	b := r.rule(t, "resolve when reopened", domain.Spec{"type": "issue.transitioned", "to": "To Do"}, nil, domain.Spec{"kind": "transition", "transition": "Resolve"})
	k2 := mk(t, e, e.Alice, "Task", "ping pong", "")
	tid, _ := trans(t, e, e.Alice, k2, "Resolve")
	e.Svc.Transition(ctx, e.Alice, k2, tid, nil)
	r.run()
	all := states(r.runs(t, a.ID)) + "|" + states(r.runs(t, b.ID))
	if !strings.Contains(all, "loop") {
		t.Fatalf("the cycle was not recognised: %s", all)
	}
	h, _ := e.Svc.History(ctx, e.Admin, k2)
	if len(h) > 12 {
		t.Fatalf("the cycle ran away: %d history entries", len(h))
	}
}

func TestCORE10_AutomationFailureStopsTheRuleAndIsRecorded(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	rule := r.rule(t, "bad second step", domain.Spec{"type": "issue.created"}, nil,
		domain.Spec{"kind": "comment", "text": "first"},
		domain.Spec{"kind": "transition", "transition": "No such transition"},
		domain.Spec{"kind": "comment", "text": "never"})
	k := mk(t, e, e.Alice, "Task", "fails", "")
	r.run()
	rs := r.runs(t, rule.ID)
	if len(rs) != 1 || rs[0].State != "failed" || rs[0].Detail["action"] != float64(1) || !strings.Contains(rs[0].Detail["error"].(string), "not available") {
		t.Fatalf("%+v", rs)
	}
	v, _ := e.Svc.View(ctx, e.Admin, k)
	if len(v.Comments) != 1 || v.Comments[0].Body != "first" {
		t.Fatalf("%+v", v.Comments)
	}
	// automation goes through the workflow: a validator that refuses stops it like a person
	tid := transitionID(t, e, "Resolve")
	e.Svc.SetTransitionRules(ctx, e.Admin, tid, domain.TRules{Validators: []domain.Spec{{"kind": "field_required", "field": "labels", "message": "labels first"}}})
	rule2 := r.rule(t, "resolve", domain.Spec{"type": "issue.updated", "fields": []string{"priority"}}, nil, domain.Spec{"kind": "transition", "transition": "Resolve"})
	k2 := mk(t, e, e.Alice, "Task", "needs labels", "")
	hi := "high"
	e.Svc.UpdateFields(ctx, e.Alice, k2, nil, false, domain.Changes{Priority: &hi})
	r.run()
	rs = r.runs(t, rule2.ID)
	if len(rs) != 1 || rs[0].State != "failed" || !strings.Contains(rs[0].Detail["error"].(string), "labels first") {
		t.Fatalf("%+v", rs)
	}
	if got := statusOf(t, e, k2); got != "To Do" {
		t.Fatalf("automation got past a validator: %q", got)
	}
}

func TestCORE10_AutomationDryRunChangesNothing(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	rule := r.rule(t, "plan", domain.Spec{"type": "issue.created"}, []domain.Spec{{"kind": "field", "field": "priority", "value": "high"}},
		domain.Spec{"kind": "comment", "text": "hello {{issue.key}}"}, domain.Spec{"kind": "transition", "transition": "Done"}, domain.Spec{"kind": "transition", "transition": "Resolve"},
		domain.Spec{"kind": "assign", "to": "bob@example.com"})
	k := mk(t, e, e.Alice, "Task", "dry", "")
	before, _ := e.Svc.View(ctx, e.Admin, k)
	d, err := e.Svc.DryRunAutoRule(ctx, e.Admin, rule.ID, k)
	if err != nil {
		t.Fatal(err)
	}
	if d.WouldRun || len(d.Conditions) != 1 || d.Conditions[0].Passed {
		t.Fatalf("priority is medium, the condition must fail: %+v", d)
	}
	if len(d.Actions) != 4 || d.Actions[0].Would != "comment: hello "+k || d.Actions[1].Problem == "" || !strings.Contains(d.Actions[2].Would, "To Do to Done") || !strings.Contains(d.Actions[3].Would, "Bob") {
		t.Fatalf("%+v", d.Actions)
	}
	hi := "high"
	e.Svc.UpdateFields(ctx, e.Alice, k, nil, false, domain.Changes{Priority: &hi})
	if d, _ = e.Svc.DryRunAutoRule(ctx, e.Admin, rule.ID, k); !d.WouldRun {
		t.Fatalf("%+v", d)
	}
	after, _ := e.Svc.View(ctx, e.Admin, k)
	if len(after.Comments) != len(before.Comments) || after.Issue.Status != before.Issue.Status || after.Issue.Assignee != nil {
		t.Fatal("a dry run changed the issue")
	}
	if len(r.runs(t, rule.ID)) != 0 {
		t.Fatal("a dry run was recorded as a run")
	}
	if _, err := e.Svc.DryRunAutoRule(ctx, e.Admin, rule.ID, "XYZ-1"); err == nil {
		t.Fatal("a dry run on an issue of another project")
	}
}

func TestCORE10_AutomationScheduledRulesRunOncePerTick(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	rule := r.rule(t, "nag", domain.Spec{"type": "schedule", "every_minutes": 60, "query": "project = ABC AND priority = high"}, nil, domain.Spec{"kind": "comment", "text": "still open"})
	hi := "high"
	a := mk(t, e, e.Alice, "Task", "high one", "")
	e.Svc.UpdateFields(ctx, e.Alice, a, nil, false, domain.Changes{Priority: &hi})
	mk(t, e, e.Alice, "Task", "medium one", "")
	n, err := e.Svc.ProcessAutomationSchedules(ctx)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ = e.Svc.ProcessAutomationSchedules(ctx); n != 0 {
		t.Fatalf("the same tick ran twice: %d", n)
	}
	e.Clock.T = e.Clock.T.Add(61 * time.Minute)
	if n, _ = e.Svc.ProcessAutomationSchedules(ctx); n != 1 {
		t.Fatalf("the next tick: %d", n)
	}
	v, _ := e.Svc.View(ctx, e.Admin, a)
	if len(v.Comments) != 2 {
		t.Fatalf("%d comments", len(v.Comments))
	}
	if got := states(r.runs(t, rule.ID)); got != "done,done" {
		t.Fatalf("%q", got)
	}
	// daily at a time, in a time zone: not before that time, then once for the day
	e.Svc.DeleteAutoRule(ctx, e.Admin, rule.ID)
	daily := r.rule(t, "morning", domain.Spec{"type": "schedule", "daily_at": "09:00", "timezone": "Europe/Berlin", "query": "project = ABC AND priority = high"}, nil, domain.Spec{"kind": "comment", "text": "good morning"})
	berlin, _ := time.LoadLocation("Europe/Berlin")
	e.Clock.T = time.Date(2026, 10, 21, 8, 59, 0, 0, berlin)
	if n, _ = e.Svc.ProcessAutomationSchedules(ctx); n != 0 {
		t.Fatalf("before 09:00: %d", n)
	}
	e.Clock.T = time.Date(2026, 10, 21, 9, 1, 0, 0, berlin)
	if n, _ = e.Svc.ProcessAutomationSchedules(ctx); n != 1 {
		t.Fatalf("at 09:01: %d", n)
	}
	e.Clock.T = time.Date(2026, 10, 21, 17, 0, 0, 0, berlin)
	if n, _ = e.Svc.ProcessAutomationSchedules(ctx); n != 0 {
		t.Fatalf("the same day again: %d", n)
	}
	e.Clock.T = time.Date(2026, 10, 22, 9, 5, 0, 0, berlin)
	if n, _ = e.Svc.ProcessAutomationSchedules(ctx); n != 1 {
		t.Fatalf("the next day: %d", n)
	}
	_ = daily
}

func TestCORE10_AutomationRulesPermissionsAndValidation(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	base := domain.AutoRule{Name: "x", Project: "ABC", Enabled: true, Trigger: domain.Spec{"type": "issue.created"}, Actions: []domain.Spec{{"kind": "comment", "text": "hi"}}}
	if _, err := e.Svc.SaveAutoRule(ctx, e.Alice, 0, base); err != domain.ErrForbidden {
		t.Fatalf("a member made a rule: %v", err)
	}
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", e.Bob.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	mine, err := e.Svc.SaveAutoRule(ctx, e.Bob, 0, base)
	if err != nil {
		t.Fatalf("a project administrator could not make a rule: %v", err)
	}
	global := base
	global.Project = ""
	if _, err := e.Svc.SaveAutoRule(ctx, e.Bob, 0, global); err != domain.ErrForbidden {
		t.Fatalf("a project administrator made a rule for every project: %v", err)
	}
	other := base
	other.Project = "XYZ"
	if _, err := e.Svc.SaveAutoRule(ctx, e.Bob, 0, other); err == nil {
		t.Fatal("a rule in a project the person does not administer")
	}
	if _, err := e.Svc.SaveAutoRule(ctx, e.Admin, 0, global); err != nil {
		t.Fatal(err)
	}
	// a rule stays in its project
	moved := base
	moved.Project = "XYZ"
	if _, err := e.Svc.SaveAutoRule(ctx, e.Admin, mine.ID, moved); err == nil {
		t.Fatal("a rule moved to another project")
	}
	if _, err := e.Svc.GetAutoRule(ctx, e.Alice, mine.ID); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	// validation
	bad := map[string]domain.AutoRule{
		"no name":        {Project: "ABC", Trigger: base.Trigger, Actions: base.Actions},
		"bad trigger":    {Name: "x", Project: "ABC", Trigger: domain.Spec{"type": "nonsense"}, Actions: base.Actions},
		"no actions":     {Name: "x", Project: "ABC", Trigger: base.Trigger},
		"bad action":     {Name: "x", Project: "ABC", Trigger: base.Trigger, Actions: []domain.Spec{{"kind": "format_disk"}}},
		"bad condition":  {Name: "x", Project: "ABC", Trigger: base.Trigger, Conditions: []domain.Spec{{"kind": "query", "query": "project = ("}}, Actions: base.Actions},
		"schedule no q":  {Name: "x", Project: "ABC", Trigger: domain.Spec{"type": "schedule", "every_minutes": 60}, Actions: base.Actions},
		"too frequent":   {Name: "x", Project: "ABC", Trigger: domain.Spec{"type": "schedule", "every_minutes": 1, "query": "project = ABC"}, Actions: base.Actions},
		"both schedules": {Name: "x", Project: "ABC", Trigger: domain.Spec{"type": "schedule", "every_minutes": 60, "daily_at": "09:00", "query": "project = ABC"}, Actions: base.Actions},
		"webhook no url": {Name: "x", Project: "ABC", Trigger: base.Trigger, Actions: []domain.Spec{{"kind": "webhook", "url": "ftp://x"}}},
		"unknown field":  {Name: "x", Project: "ABC", Trigger: base.Trigger, Actions: []domain.Spec{{"kind": "set_field", "field": "customfield_9999", "value": 1}}},
	}
	for name, rule := range bad {
		rule.Enabled = true
		if _, err := e.Svc.SaveAutoRule(ctx, e.Admin, 0, rule); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	many := base
	for i := 0; i < 11; i++ {
		many.Actions = append(many.Actions, domain.Spec{"kind": "comment", "text": "hi"})
	}
	if _, err := e.Svc.SaveAutoRule(ctx, e.Admin, 0, many); err == nil {
		t.Fatal("too many actions")
	}
	// a webhook secret is stored encrypted and never shown
	wh := base
	wh.Actions = []domain.Spec{{"kind": "webhook", "url": "https://example.com/hook", "secret": "s3cret"}}
	saved, err := e.Svc.SaveAutoRule(ctx, e.Admin, 0, wh)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	e.Store.Pool.QueryRow(ctx, `SELECT actions::text FROM automation_rule WHERE id=$1`, saved.ID).Scan(&stored)
	if strings.Contains(stored, "s3cret") || saved.Actions[0]["secret_set"] != true || saved.Actions[0]["secret"] != nil {
		t.Fatalf("stored %s, returned %+v", stored, saved.Actions[0])
	}
	// editing without the secret keeps it
	saved.Actions[0]["url"] = "https://example.com/hook"
	again, err := e.Svc.SaveAutoRule(ctx, e.Admin, saved.ID, saved)
	if err != nil || again.Actions[0]["secret_set"] != true {
		t.Fatalf("%+v %v", again.Actions, err)
	}
	if err := e.Svc.DeleteAutoRule(ctx, e.Alice, saved.ID); err == nil {
		t.Fatal("a member deleted a rule")
	}
	if err := e.Svc.DeleteAutoRule(ctx, e.Admin, saved.ID); err != nil {
		t.Fatal(err)
	}
}

// MCP-20: a cycle that runs through something outside Tasklane (here, an agent that comments whenever a rule comments)
// carries no chain, so the per-issue limit is what stops it.
func TestMCP20_ACycleThroughAnAgentIsStoppedByThePerIssueLimit(t *testing.T) {
	r := newAuto(t)
	e := r.Env
	rule := r.rule(t, "ack", domain.Spec{"type": "comment.added", "contains": "please ack"}, nil, domain.Spec{"kind": "comment", "text": "acknowledged"})
	k := mk(t, e, e.Alice, "Task", "ping", "")
	agent, _ := e.Svc.CreateAgent(ctx, e.Admin, "echo agent")
	e.Svc.AddMember(ctx, e.Admin, "ABC", agent.ID, "member")
	ag := domain.Actor{ID: agent.ID, Label: "Agent: echo", Kind: "agent"}
	for i := 0; i < 14; i++ { // the agent answers every acknowledgement with another request, as a webhook-driven agent could
		if _, err := e.Svc.AddComment(ctx, ag, k, "please ack", false); err != nil {
			t.Fatal(err)
		}
		r.run()
	}
	v, _ := e.Svc.View(ctx, e.Admin, k)
	acks := 0
	for _, c := range v.Comments {
		if c.Body == "acknowledged" {
			acks++
		}
	}
	if acks != 10 {
		t.Fatalf("the rule acknowledged %d times; the limit is 10 an hour", acks)
	}
	if got := states(r.runs(t, rule.ID)); !strings.HasPrefix(got, "throttled,throttled,throttled,throttled,done") {
		t.Fatalf("%q", got)
	}
	// an hour later the rule works again
	e.Clock.T = e.Clock.T.Add(61 * time.Minute)
	e.Svc.AddComment(ctx, ag, k, "please ack", false)
	r.run()
	v, _ = e.Svc.View(ctx, e.Admin, k)
	after := 0
	for _, c := range v.Comments {
		if c.Body == "acknowledged" {
			after++
		}
	}
	if after != 11 {
		t.Fatalf("%d", after)
	}
}
