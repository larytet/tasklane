package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func bulkItems(out map[string]any) map[string]map[string]any {
	m := map[string]map[string]any{}
	for _, x := range out["items"].([]any) {
		it := x.(map[string]any)
		m[it["key"].(string)] = it
	}
	return m
}

func statusOfKey(t *testing.T, e *testutil.Env, key string) string {
	v, err := e.Svc.View(testutil.Ctx(), e.Admin, key)
	if err != nil {
		t.Fatal(err)
	}
	return v.Issue.Status
}

// BULK-03, BULK-04: fields, transition, move, archive and delete, by keys and by query; a dry run (the default) changes nothing and
// plans exactly what a run does; a retry with the same idempotency key after a lost answer applies nothing twice.
func TestBULK03_04_EveryOperationPlannedThenApplied(t *testing.T) {
	e, alice, _, first := setup(t)
	var keys []string
	keys = append(keys, first)
	for i := 0; i < 3; i++ {
		_, out, _ := alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "type": "Task", "summary": fmt.Sprint("bulk ", i), "labels": []string{"old"}})
		keys = append(keys, out["key"].(string))
	}
	// a dry run changes nothing and says what would change
	code, out, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys, "op": "fields", "priority": "highest", "labels_add": []string{"bulk"}, "labels_remove": []string{"old"}, "assignee": "me"})
	if code != 200 || out["dry_run"] != true || out["summary"].(map[string]any)["would_change"].(float64) != 4 {
		t.Fatalf("dry run: %d %v", code, out)
	}
	if v, _ := e.Svc.View(testutil.Ctx(), e.Alice, keys[1]); v.Issue.Priority == "highest" {
		t.Fatal("the dry run changed something")
	}
	plan := bulkItems(out)
	// the run does exactly that
	code, out, hdr := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys, "op": "fields", "priority": "highest", "labels_add": []string{"bulk"}, "labels_remove": []string{"old"}, "assignee": "me", "dry_run": false, "idempotency_key": "bulk-fields-1"})
	if code != 200 || out["summary"].(map[string]any)["changed"].(float64) != 4 || hdr.Get("Tasklane-Undo") != "" {
		t.Fatalf("run: %d %v %v", code, out, hdr)
	}
	for k, it := range bulkItems(out) {
		if fmt.Sprint(it["changes"]) != fmt.Sprint(plan[k]["changes"]) {
			t.Fatalf("%s: the plan %v differs from the result %v", k, plan[k]["changes"], it["changes"])
		}
	}
	v, _ := e.Svc.View(testutil.Ctx(), e.Alice, keys[1])
	if v.Issue.Priority != "highest" || strings.Join(v.Issue.Labels, ",") != "bulk" || v.Issue.Assignee == nil || v.Issue.Assignee.ID != e.Alice.ID {
		t.Fatalf("applied: %+v", v.Issue)
	}
	ver := v.Issue.Version
	code, out, _ = alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys, "op": "fields", "priority": "highest", "labels_add": []string{"bulk"}, "labels_remove": []string{"old"}, "assignee": "me", "dry_run": false, "idempotency_key": "bulk-fields-1"})
	if code != 200 || out["idempotent_replay"] != true {
		t.Fatalf("retry: %d %v", code, out)
	}
	if v, _ := e.Svc.View(testutil.Ctx(), e.Alice, keys[1]); v.Issue.Version != ver {
		t.Fatal("the retry applied again")
	}
	if code, _, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys[:1], "op": "fields", "priority": "low", "dry_run": false, "idempotency_key": "bulk-fields-1"}); code != 409 {
		t.Fatalf("the same key for another request: %d", code)
	}
	// by query, a transition, with one undo for the batch
	code, out, hdr = alice.do("POST", "/api/v1/bulk", map[string]any{"query": "labels = bulk", "op": "transition", "transition": "Resolve", "dry_run": false})
	if code != 200 || out["summary"].(map[string]any)["changed"].(float64) != 4 || !strings.HasPrefix(hdr.Get("Tasklane-Undo"), "un_") {
		t.Fatalf("transition by query: %d %v %v", code, out, hdr)
	}
	tok := hdr.Get("Tasklane-Undo")
	if statusOfKey(t, e, keys[2]) != "Done" {
		t.Fatal("not resolved")
	}
	if code, out, _ := alice.do("POST", "/api/v1/undo/"+tok, nil); code != 200 || len(out["undone"].([]any)) != 4 {
		t.Fatalf("undo transition: %d %v", code, out)
	}
	if statusOfKey(t, e, keys[3]) != "To Do" {
		t.Fatal("the batch undo did not put the status back")
	}
	// resolve again, archive the four, then undo the archive: one token puts every issue back
	alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys, "op": "transition", "transition": "Resolve", "dry_run": false})
	code, out, hdr = alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys, "op": "archive", "dry_run": false})
	if code != 200 || out["summary"].(map[string]any)["changed"].(float64) != 4 {
		t.Fatalf("archive: %d %v", code, out)
	}
	if code, out, _ := alice.do("POST", "/api/v1/undo/"+hdr.Get("Tasklane-Undo"), nil); code != 200 || len(out["undone"].([]any)) != 4 {
		t.Fatalf("undo archive: %d %v", code, out)
	}
	// move: planned per issue, applied with new keys
	if _, err := e.Svc.CreateProject(testutil.Ctx(), e.Admin, "XYZ", "Xray"); err != nil {
		t.Fatal(err)
	}
	e.Svc.AddMember(testutil.Ctx(), e.Admin, "XYZ", e.Alice.ID, "member")
	code, out, _ = alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys[2:], "op": "move", "project": "XYZ"})
	if code != 200 || out["summary"].(map[string]any)["would_change"].(float64) != 2 {
		t.Fatalf("move plan: %d %v", code, out)
	}
	if code, out, _ = alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys[2:], "op": "move", "project": "XYZ", "dry_run": false}); code != 200 || out["summary"].(map[string]any)["changed"].(float64) != 2 {
		t.Fatalf("move: %d %v", code, out)
	}
	if v, err := e.Svc.View(testutil.Ctx(), e.Alice, keys[2]); err != nil || !strings.HasPrefix(v.Issue.Key, "XYZ-") {
		t.Fatalf("not moved: %v %v", err, v.Issue.Key)
	}
	// delete, undone as a batch: back with their keys
	code, _, hdr = alice.do("POST", "/api/v1/bulk", map[string]any{"keys": keys[:2], "op": "delete", "dry_run": false})
	if code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, out, _ := alice.do("POST", "/api/v1/undo/"+hdr.Get("Tasklane-Undo"), nil); code != 200 || len(out["undone"].([]any)) != 2 {
		t.Fatalf("undo delete: %d %v", code, out)
	}
	if _, err := e.Svc.View(testutil.Ctx(), e.Alice, keys[0]); err != nil {
		t.Fatalf("not restored: %v", err)
	}
}

// BULK-05, BULK-06: failures per issue with their reason and the rest applied; the cap of one page, an empty selection, duplicates
// and unknown keys.
func TestBULK05_06_PartialFailuresAndCaps(t *testing.T) {
	e, alice, _, first := setup(t)
	ctx := testutil.Ctx()
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Xray"); err != nil {
		t.Fatal(err)
	}
	e.Svc.AddMember(ctx, e.Admin, "XYZ", e.Alice.ID, "viewer")
	ro, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Type: "Task", Summary: "read only for alice"})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := e.Svc.CreateProject(ctx, e.Admin, "QQQ", "Hidden")
	if err != nil {
		t.Fatal(err)
	}
	_ = hidden
	hid, _ := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "QQQ", Type: "Task", Summary: "not alice's"})
	_, out, _ := alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "type": "Task", "summary": "second"})
	second := out["key"].(string)
	// the second issue is already Done: Resolve is not available from there
	if _, err := e.Svc.Transition(ctx, e.Alice, second, transitionOf(t, e, second, "Resolve"), nil); err != nil {
		t.Fatal(err)
	}
	code, out, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": []string{first, first, strings.ToLower(first), second, ro.Key, hid.Key, "ABC-99999"}, "op": "transition", "transition": "Resolve", "dry_run": false})
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	items := bulkItems(out)
	if len(items) != 5 || items[first]["ok"] != true || statusOfKey(t, e, first) != "Done" {
		t.Fatalf("duplicates once, the good one applied: %v", items)
	}
	for k, want := range map[string]string{second: "not available", ro.Key: "forbidden", hid.Key: "not found", "ABC-99999": "not found"} {
		if items[k]["ok"] != false || !strings.Contains(fmt.Sprint(items[k]["error"]), want) {
			t.Errorf("%s: want %q, got %v", k, want, items[k])
		}
	}
	if out["summary"].(map[string]any)["failed"].(float64) != 0 || out["summary"].(map[string]any)["errors"].(float64) != 4 {
		t.Fatalf("summary: %v", out["summary"])
	}
	// caps
	var many []string
	for i := 1; i <= 101; i++ {
		many = append(many, fmt.Sprintf("ABC-%d", i))
	}
	if code, out, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": many, "op": "archive"}); code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "at most 100") {
		t.Fatalf("over the cap: %d %v", code, out)
	}
	if code, _, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": []string{}, "op": "archive"}); code != 400 {
		t.Fatalf("empty selection: %d", code)
	}
	if code, _, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": []string{first}, "op": "explode"}); code != 400 {
		t.Fatalf("unknown op: %d", code)
	}
	if code, _, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": []string{first}, "op": "fields"}); code != 400 {
		t.Fatalf("nothing to change: %d", code)
	}
	outsider, err := e.Svc.CreateUser(ctx, "outsider@example.com", "Out Sider", "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	if code, out, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": []string{first}, "op": "fields", "assignee": outsider.ID}); code != 200 || bulkItems(out)[first]["ok"] != false {
		t.Fatalf("a person outside the project: %d %v", code, out)
	}
	// the transitions the selection can take
	code, tr, _ := alice.doList("POST", "/api/v1/bulk/transitions", map[string]any{"keys": []string{first, second, ro.Key}})
	if code != 200 || len(tr) == 0 {
		t.Fatalf("transitions: %d %v", code, tr)
	}
	for _, x := range tr {
		if x["name"] == "Resolve" {
			t.Fatalf("Resolve is offered although no selected issue can take it: %v", tr)
		}
	}
}

// UNDO-08: a batch undo with one issue changed by someone else in between puts every other issue back and reports that one.
func TestUNDO08_BatchUndoWithOneChangedIssue(t *testing.T) {
	e, alice, bob, first := setup(t)
	_, out, _ := alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "type": "Task", "summary": "two"})
	second := out["key"].(string)
	_, _, hdr := alice.do("POST", "/api/v1/bulk", map[string]any{"keys": []string{first, second}, "op": "transition", "transition": "Resolve", "dry_run": false})
	tok := hdr.Get("Tasklane-Undo")
	if code, _, _ := bob.do("POST", "/api/v1/issues/"+second+"/comments", map[string]any{"body": "meanwhile"}); code != 201 {
		t.Fatal("comment")
	}
	code, res, _ := alice.do("POST", "/api/v1/undo/"+tok, nil)
	if code != 200 || len(res["undone"].([]any)) != 1 || res["conflicts"].(map[string]any)[second] == nil {
		t.Fatalf("%d %v", code, res)
	}
	if statusOfKey(t, e, first) != "To Do" || statusOfKey(t, e, second) != "Done" {
		t.Fatal("wrong issues reverted")
	}
}

// transitionOf is the id of the transition named name that the issue can take now (each project may have its own workflow).
func transitionOf(t *testing.T, e *testutil.Env, key, name string) string {
	v, err := e.Svc.View(testutil.Ctx(), e.Admin, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range v.Transitions {
		if tr.Name == name {
			return tr.ID
		}
	}
	t.Fatalf("%s cannot take %q", key, name)
	return ""
}

// doList is do for routes that answer with a JSON array.
func (c *client) doList(method, path string, body any) (int, []map[string]any, http.Header) {
	var rd *bytes.Reader
	b, _ := json.Marshal(body)
	rd = bytes.NewReader(b)
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func bulkJobDone(t *testing.T, e *testutil.Env, c *client, id string, step int) map[string]any {
	t.Helper()
	for i := 0; i < 20; i++ {
		if n, err := e.Svc.ProcessBulk(testutil.Ctx(), step); err != nil {
			t.Fatal(err)
		} else if n == 0 {
			break
		}
	}
	_, out, _ := c.do("GET", "/api/v1/bulk/"+id, nil)
	return out
}

// UX-BULK-ALL, BULK-02 (later part): "Select all N matching" runs the bar's actions on every issue a query matches, past the page of 100,
// as a job in the person's own scope: a field change, a transition and an archive; an issue that cannot take it fails with its reason and
// the rest go on; the page limit of the interactive endpoint is unchanged.
func TestUXBULKALL_SelectAllMatchingAsJob(t *testing.T) {
	e, alice, bob, _ := setup(t)
	ctx := testutil.Ctx()
	var keys []string
	for i := 0; i < 130; i++ {
		_, out, _ := alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "type": "Task", "summary": fmt.Sprint("all ", i), "labels": []string{"sel"}})
		keys = append(keys, out["key"].(string))
	}
	// one is already Done, so the transition cannot be taken there
	done := keys[5]
	if _, err := e.Svc.Transition(ctx, e.Alice, done, transitionOf(t, e, done, "Resolve"), nil); err != nil {
		t.Fatal(err)
	}
	// the interactive endpoint still refuses more than a page
	if code, out, _ := alice.do("POST", "/api/v1/bulk", map[string]any{"query": "labels = sel", "op": "fields", "priority": "high"}); code != 400 || !strings.Contains(fmt.Sprint(out["error"]), "more than 100") {
		t.Fatalf("the page cap: %d %v", code, out)
	}
	// fields over the whole match, as a job: 130 issues, two worker rounds of 100
	code, out, _ := alice.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = sel", "do": map[string]any{"kind": "fields", "priority": "highest", "labels_add": []string{"all"}}})
	if code != 202 || out["total"] != float64(130) || out["op"] != "fields" {
		t.Fatalf("start: %d %v", code, out)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, keys[7]); v.Issue.Priority == "highest" {
		t.Fatal("the job changed something before a worker took it")
	}
	st := bulkJobDone(t, e, alice, fmt.Sprint(int64(out["id"].(float64))), 100)
	if st["state"] != "done" || st["done"] != float64(130) || st["failed"] != float64(0) {
		t.Fatalf("fields job: %v", st)
	}
	for _, k := range []string{keys[0], keys[7], keys[129]} {
		if v, _ := e.Svc.View(ctx, e.Alice, k); v.Issue.Priority != "highest" || !strings.Contains(strings.Join(v.Issue.Labels, ","), "all") {
			t.Fatalf("%s not changed: %v %v", k, v.Issue.Priority, v.Issue.Labels)
		}
	}
	// a transition: the one already Done fails with the reason, the other 129 go on
	code, out, _ = alice.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = sel", "do": map[string]any{"kind": "transition", "transition": "Resolve"}})
	if code != 202 {
		t.Fatalf("start transition: %d %v", code, out)
	}
	st = bulkJobDone(t, e, alice, fmt.Sprint(int64(out["id"].(float64))), 100)
	errs, _ := st["errors"].([]any)
	if st["state"] != "done" || st["done"] != float64(129) || st["failed"] != float64(1) || len(errs) != 1 ||
		errs[0].(map[string]any)["key"] != done || !strings.Contains(fmt.Sprint(errs[0]), "not available") {
		t.Fatalf("transition job: %v", st)
	}
	if statusOfKey(t, e, keys[8]) != "Done" {
		t.Fatal("not resolved")
	}
	// an archive of finished work
	code, out, _ = alice.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = sel", "do": map[string]any{"kind": "archive"}})
	if code != 202 {
		t.Fatalf("start archive: %d %v", code, out)
	}
	st = bulkJobDone(t, e, alice, fmt.Sprint(int64(out["id"].(float64))), 100)
	if st["state"] != "done" || st["done"] != float64(130) {
		t.Fatalf("archive job: %v", st)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, keys[9]); v.Issue.Archived == nil {
		t.Fatal("not archived")
	}
	// only the owner sees the job
	if code, _, _ := bob.do("GET", fmt.Sprintf("/api/v1/bulk/%d", int64(out["id"].(float64))), nil); code != 404 {
		t.Fatalf("another person sees the job: %d", code)
	}
	// a wrong query and an empty match are refused before any job exists; so is an action that says nothing
	if code, out, _ := alice.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = = =", "do": map[string]any{"kind": "archive"}}); code != 400 || out["error"] == nil {
		t.Fatalf("bad query: %d %v", code, out)
	}
	if code, _, _ := alice.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = nothing-has-this", "do": map[string]any{"kind": "archive"}}); code != 400 {
		t.Fatalf("empty match: %d", code)
	}
	if code, _, _ := alice.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = sel", "do": map[string]any{"kind": "fields"}}); code != 400 {
		t.Fatalf("nothing to change: %d", code)
	}
	if code, _, _ := alice.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = sel", "do": map[string]any{"kind": "purge"}}); code != 400 {
		t.Fatalf("unknown kind: %d", code)
	}
	// the query runs in the person's own scope: a person who cannot read the project matches nothing, rather than learning what is there
	outsider, err := e.Svc.CreateUser(ctx, "scope@example.com", "Scope Less", "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.Svc.CreateAPIToken(ctx, outsider.ID, "t")
	oc := &client{t: t, base: alice.base, token: tok}
	if code, _, _ := oc.do("POST", "/api/v1/bulk/lifecycle", map[string]any{"query": "labels = sel", "do": map[string]any{"kind": "fields", "priority": "low"}}); code != 400 {
		t.Fatalf("a person outside the project: %d", code)
	}
}
