package mcp_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

func (e *env) issues(t *testing.T, n int, prefix string) []string {
	var keys []string
	for i := 0; i < n; i++ {
		is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: fmt.Sprintf("%s %d", prefix, i)})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, is.Key)
	}
	return keys
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func TestMCP12_IdempotencyKeysMakeRetriesSafe(t *testing.T) {
	e := newEnv(t)
	ag := e.newAgent("retrier", "member", nil)
	ag.init()
	args := map[string]any{"key": e.key, "body": "posted once", "idempotency_key": "retry-key-0001"}
	first := ag.ok("add_comment", args)
	second := ag.ok("add_comment", args)
	if second.Data["idempotent_replay"] != true || first.Data["idempotent_replay"] != nil {
		t.Fatalf("first %v second %v", first.Data, second.Data)
	}
	v, _ := e.Svc.View(ctx, e.Admin, e.key)
	n := 0
	for _, c := range v.Comments {
		if c.Body == "posted once" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the comment was posted %d times", n)
	}
	// the same key with other arguments is refused, not silently replayed
	if r := ag.call("add_comment", map[string]any{"key": e.key, "body": "something else", "idempotency_key": "retry-key-0001"}); !r.Err || !strings.Contains(r.Text, "different arguments") {
		t.Fatalf("%+v", r)
	}
	// keys belong to the caller: another agent using the same key does its own call
	other := e.newAgent("other", "member", nil)
	other.init()
	if r := other.ok("add_comment", args); r.Data["idempotent_replay"] != nil {
		t.Fatal("a key was shared between two identities")
	}
	// too short a key is refused
	if r := ag.call("add_comment", map[string]any{"key": e.key, "body": "x", "idempotency_key": "short"}); !r.Err || !strings.Contains(r.Text, "8 to 128") {
		t.Fatalf("%+v", r)
	}
	// a failure does not pin the key: the retry runs
	bad := map[string]any{"key": "ABC-9999", "body": "to nowhere", "idempotency_key": "retry-key-0002"}
	if r := ag.call("add_comment", bad); !r.Err {
		t.Fatal("a comment on a missing issue succeeded")
	}
	var held int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_key WHERE key='retry-key-0002'`).Scan(&held)
	if held != 0 {
		t.Fatal("a failed call kept its key")
	}
	// replays do not spend the write budget
	e.m.WritesPerHour = 2
	budget := e.newAgent("budgeted", "member", nil)
	budget.init()
	one := map[string]any{"key": e.key, "body": "budgeted", "idempotency_key": "retry-key-0003"}
	for i := 0; i < 5; i++ {
		if r := budget.call("add_comment", one); r.Err {
			t.Fatalf("attempt %d: %s", i, r.Text)
		}
	}
	// ten retries at once do the work once
	e.m.WritesPerHour = 100
	racer := e.newAgent("racer", "member", nil)
	racer.init()
	var wg sync.WaitGroup
	results := make([]result, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = racer.call("add_comment", map[string]any{"key": e.key, "body": "raced", "idempotency_key": "retry-key-0004"})
		}(i)
	}
	wg.Wait()
	v, _ = e.Svc.View(ctx, e.Admin, e.key)
	posted := 0
	for _, c := range v.Comments {
		if c.Body == "raced" {
			posted++
		}
	}
	if posted != 1 {
		t.Fatalf("ten concurrent retries posted %d comments", posted)
	}
}

func TestMCP12_BulkUpdateDryRunIsExactAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	keys := e.issues(t, 6, "bulk")
	ag := e.newAgent("bulker", "member", nil)
	ag.init()
	hi := "high"
	e.Svc.UpdateFields(ctx, e.Alice, keys[0], nil, false, domain.Changes{Priority: &hi}) // already high: unchanged
	before := map[string]int64{}
	for _, k := range keys {
		v, _ := e.Svc.View(ctx, e.Admin, k)
		before[k] = v.Issue.Version
	}
	args := map[string]any{"keys": anySlice(keys), "priority": "high", "labels_add": []any{"batch", "audited"}, "labels_remove": []any{"nothing-here"}, "assignee": "me"}
	dry := ag.ok("bulk_update_fields", args) // dry_run is not given: it defaults to true
	if dry.Data["dry_run"] != true {
		t.Fatalf("%v", dry.Data)
	}
	sum := dry.Data["summary"].(map[string]any)
	if sum["would_change"] != float64(6) || sum["errors"] != float64(0) {
		t.Fatalf("%v", sum)
	}
	for _, k := range keys {
		v, _ := e.Svc.View(ctx, e.Admin, k)
		if v.Issue.Version != before[k] || len(v.Issue.Labels) != 0 {
			t.Fatalf("%s: a dry run changed the issue", k)
		}
	}
	// the preview names each change with before and after
	first := dry.Data["items"].([]any)[1].(map[string]any)
	ch := first["changes"].([]any)
	got := map[string]string{}
	for _, c := range ch {
		m := c.(map[string]any)
		got[m["field"].(string)] = fmt.Sprint(m["after"])
	}
	if got["priority"] != "high" || got["labels"] != "[audited batch]" || got["assignee"] != "bulker" {
		t.Fatalf("%v", got)
	}
	// the first issue is already high: its plan has no priority change, the rest of the plan stands
	for _, c := range dry.Data["items"].([]any)[0].(map[string]any)["changes"].([]any) {
		if c.(map[string]any)["field"] == "priority" {
			t.Fatal("an unchanged field was planned")
		}
	}
	// the real run does exactly that
	args["dry_run"] = false
	args["idempotency_key"] = "bulk-update-0001"
	real := ag.ok("bulk_update_fields", args)
	rs := real.Data["summary"].(map[string]any)
	if rs["changed"] != float64(6) || rs["failed"] != float64(0) {
		t.Fatalf("%v", rs)
	}
	for _, k := range keys {
		v, _ := e.Svc.View(ctx, e.Admin, k)
		if v.Issue.Priority != "high" || strings.Join(v.Issue.Labels, ",") != "audited,batch" || v.Issue.Assignee == nil || v.Issue.Assignee.Name != "bulker" {
			t.Fatalf("%s: %+v", k, v.Issue)
		}
	}
	// a retry of the real run does nothing again and says so
	again := ag.ok("bulk_update_fields", args)
	if again.Data["idempotent_replay"] != true {
		t.Fatalf("%v", again.Data)
	}
	// run once more without a key: now there is nothing to change
	args["idempotency_key"] = nil
	delete(args, "idempotency_key")
	noop := ag.ok("bulk_update_fields", args)
	ns := noop.Data["summary"].(map[string]any)
	if ns["unchanged"] != float64(6) || ns["changed"] != float64(0) {
		t.Fatalf("%v", ns)
	}
}

func TestMCP12_BulkReportsEachIssueAndRespectsLimitsAndGates(t *testing.T) {
	e := newEnv(t)
	keys := e.issues(t, 3, "mixed")
	ag := e.newAgent("bulker", "member", nil)
	ag.init()
	// an issue in a project the agent cannot see, a missing key and good ones: each is reported on its own
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Other"); err != nil {
		t.Fatal(err)
	}
	hidden, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Type: "Task", Summary: "not yours"})
	if err != nil {
		t.Fatal(err)
	}
	r := ag.ok("bulk_update_fields", map[string]any{"keys": anySlice(append(keys, hidden.Key, "ABC-9999")), "priority": "low", "dry_run": false})
	sum := r.Data["summary"].(map[string]any)
	if sum["changed"] != float64(3) || sum["errors"] != float64(2) {
		t.Fatalf("%v", sum)
	}
	for _, it := range r.Data["items"].([]any) {
		m := it.(map[string]any)
		if (m["key"] == hidden.Key || m["key"] == "ABC-9999") && (m["ok"] != false || !strings.Contains(fmt.Sprint(m["error"]), "not found")) {
			t.Fatalf("%v", m)
		}
	}
	v, _ := e.Svc.View(ctx, e.Admin, hidden.Key)
	if v.Issue.Priority == "low" {
		t.Fatal("an issue the agent cannot see was changed")
	}
	// a viewer can plan nothing it may not do
	viewer := e.newAgent("viewer", "viewer", nil)
	viewer.init()
	vr := viewer.ok("bulk_update_fields", map[string]any{"keys": anySlice(keys), "priority": "high"})
	if vr.Data["summary"].(map[string]any)["errors"] != float64(3) {
		t.Fatalf("%v", vr.Data["summary"])
	}
	// too many issues, no target, nothing to change, bad values
	if r := ag.call("bulk_update_fields", map[string]any{"keys": anySlice(e.issues(t, 51, "many")), "priority": "high"}); !r.Err || !strings.Contains(r.Text, "at most 50") {
		t.Fatalf("%+v", r)
	}
	for name, a := range map[string]map[string]any{
		"no target":     {"priority": "high"},
		"nothing to do": {"keys": anySlice(keys)},
		"bad priority":  {"keys": anySlice(keys), "priority": "urgent"},
		"bad query":     {"query": "project = (", "priority": "high"},
		"too broad":     {"query": "project = ABC", "priority": "high"},
	} {
		if r := ag.call("bulk_update_fields", a); !r.Err {
			t.Errorf("%s was accepted: %s", name, r.Text)
		}
	}
	// targets picked by a query
	e.Svc.AddComment(ctx, e.Alice, keys[0], "x", false)
	q := ag.ok("bulk_update_fields", map[string]any{"query": "key = " + keys[1], "priority": "highest"})
	if q.Data["summary"].(map[string]any)["would_change"] != float64(1) {
		t.Fatalf("%v", q.Data)
	}
	// the write budget is spent per change, whole or not at all
	e.m.WritesPerHour = 2
	tight := e.newAgent("tight", "member", nil)
	tight.init()
	if r := tight.call("bulk_update_fields", map[string]any{"keys": anySlice(keys), "priority": "medium", "dry_run": false}); !r.Err || !strings.Contains(r.Text, "write budget") {
		t.Fatalf("%+v", r)
	}
	for _, k := range keys {
		v, _ := e.Svc.View(ctx, e.Admin, k)
		if v.Issue.Priority == "medium" {
			t.Fatal("part of a bulk change was applied although the budget could not cover it")
		}
	}
	if r := tight.ok("bulk_update_fields", map[string]any{"keys": anySlice(keys), "priority": "medium"}); r.Data["dry_run"] != true {
		t.Fatal("a dry run was refused by the write budget")
	}
}

func TestMCP12_BulkTransitionHonoursTheWorkflowAndHumanGates(t *testing.T) {
	e := newEnv(t)
	keys := e.issues(t, 3, "move")
	ag := e.newAgent("mover", "member", nil)
	ag.init()
	// one issue is already In Progress, so "Start progress" is not available from there
	tid, _ := trans2(t, e, keys[0], "Start progress")
	e.Svc.Transition(ctx, e.Alice, keys[0], tid, nil)
	dry := ag.ok("bulk_transition", map[string]any{"keys": anySlice(keys), "transition": "Start progress"})
	items := dry.Data["items"].([]any)
	if items[0].(map[string]any)["ok"] != false || !strings.Contains(fmt.Sprint(items[0].(map[string]any)["error"]), "not available from status") || items[1].(map[string]any)["ok"] != true {
		t.Fatalf("%v", items)
	}
	if dry.Data["summary"].(map[string]any)["would_change"] != float64(2) {
		t.Fatalf("%v", dry.Data["summary"])
	}
	for _, k := range keys[1:] {
		if v, _ := e.Svc.View(ctx, e.Admin, k); v.Issue.Status != "To Do" {
			t.Fatalf("%s: a dry run moved an issue", k)
		}
	}
	real := ag.ok("bulk_transition", map[string]any{"keys": anySlice(keys), "transition": "Start progress", "dry_run": false, "idempotency_key": "bulk-move-0001"})
	if real.Data["summary"].(map[string]any)["changed"] != float64(2) {
		t.Fatalf("%v", real.Data["summary"])
	}
	for _, k := range keys {
		if v, _ := e.Svc.View(ctx, e.Admin, k); v.Issue.Status != "In Progress" {
			t.Fatalf("%s: %s", k, v.Issue.Status)
		}
	}
	// a transition that needs a person is refused for an agent, issue by issue, in the plan already
	done := transitionIDByName(t, e, "Done")
	if err := e.Svc.SetTransitionHuman(ctx, e.Admin, done, true); err != nil {
		t.Fatal(err)
	}
	gate := ag.ok("bulk_transition", map[string]any{"keys": anySlice(keys), "transition": "Done"})
	if gate.Data["summary"].(map[string]any)["errors"] != float64(3) || !strings.Contains(fmt.Sprint(gate.Data["items"]), "needs a person") {
		t.Fatalf("%v", gate.Data)
	}
	g2 := ag.ok("bulk_transition", map[string]any{"keys": anySlice(keys), "transition": "Done", "dry_run": false})
	if g2.Data["summary"].(map[string]any)["changed"] != float64(0) {
		t.Fatalf("%v", g2.Data)
	}
}

func trans2(t *testing.T, e *env, key, name string) (string, bool) {
	v, err := e.Svc.View(ctx, e.Admin, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v.Transitions {
		if x.Name == name {
			return x.ID, true
		}
	}
	return "", false
}

func transitionIDByName(t *testing.T, e *env, name string) string {
	var id string
	if err := e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM transition WHERE name=$1`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
