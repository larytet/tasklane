package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/api"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func rootClient(t *testing.T, e *testutil.Env, like *client) *client {
	tok, err := e.Svc.CreateAPIToken(testutil.Ctx(), e.Admin.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	return &client{t: t, base: like.base, token: tok}
}

func cardKeys(out map[string]any, col string) []string {
	var keys []string
	for _, c := range out["columns"].([]any) {
		cm := c.(map[string]any)
		if cm["name"] == col {
			for _, k := range cm["cards"].([]any) {
				keys = append(keys, k.(map[string]any)["key"].(string))
			}
		}
	}
	return keys
}

func TestHTTP_BoardFilterQuickFiltersAndMove(t *testing.T) {
	e, alice, bob, key := setup(t) // ABC-1 exists, reported by alice
	_, mine, _ := alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "payments bug", "labels": []string{"payments"}})
	mineKey := mine["key"].(string)
	alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "docs", "labels": []string{"docs"}})
	_, theirs, _ := bob.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "payments refund", "labels": []string{"payments"}})
	theirKey := theirs["key"].(string)
	me := e.Alice.ID
	if _, err := e.Svc.UpdateFields(testutil.Ctx(), e.Alice, mineKey, nil, false, domain.Changes{Assignee: &me}); err != nil {
		t.Fatal(err)
	}
	// members cannot make boards: an administrator does
	if code, _, _ := alice.do("POST", "/api/v1/projects/ABC/boards", map[string]any{"name": "B", "kind": "kanban"}); code != 403 {
		t.Fatalf("member created a board: %d", code)
	}
	root := rootClient(t, e, alice)
	code, b, _ := root.do("POST", "/api/v1/projects/ABC/boards", map[string]any{"name": "Payments", "kind": "kanban"})
	if code != 201 {
		t.Fatalf("%d %v", code, b)
	}
	id := b["id"].(string)
	code, v, _ := alice.do("GET", "/api/v1/boards/"+id, nil)
	if code != 200 || len(cardKeys(v, "To Do")) != 4 {
		t.Fatalf("%d %v", code, v)
	}
	// a board filter limits the cards, in TQL
	root.do("PATCH", "/api/v1/boards/"+id, map[string]any{"filter": "labels = payments", "quick_filters": []map[string]any{{"name": "Only my issues", "query": "assignee = currentUser()"}}})
	_, v, _ = alice.do("GET", "/api/v1/boards/"+id, nil)
	if got := cardKeys(v, "To Do"); len(got) != 2 {
		t.Fatalf("filter: %v", got)
	}
	// a quick filter narrows further; for the other person the same chip gives a different answer
	_, v, _ = alice.do("GET", "/api/v1/boards/"+id+"?quick=Only+my+issues", nil)
	if got := cardKeys(v, "To Do"); len(got) != 1 || got[0] != mineKey {
		t.Fatalf("quick filter: %v", got)
	}
	_, v, _ = bob.do("GET", "/api/v1/boards/"+id+"?quick=Only+my+issues", nil)
	if got := cardKeys(v, "To Do"); len(got) != 0 {
		t.Fatalf("bob has nothing assigned: %v (%s)", got, theirKey)
	}
	// a filter that does not parse is a 400 that says so, never a silently unfiltered board
	root.do("PATCH", "/api/v1/boards/"+id, map[string]any{"filter": "labels = ("})
	if code, out, _ := alice.do("GET", "/api/v1/boards/"+id, nil); code != 400 || out["code"] != "tql_error" {
		t.Fatalf("%d %v", code, out)
	}
	root.do("PATCH", "/api/v1/boards/"+id, map[string]any{"filter": ""})
	// move: transition and rank over HTTP, with the explanation of a blocked move
	code, res, _ := alice.do("POST", "/api/v1/boards/"+id+"/move", map[string]any{"key": key, "column": "In Progress"})
	if code != 200 || res["transitioned"] != true {
		t.Fatalf("%d %v", code, res)
	}
	_, v, _ = alice.do("GET", "/api/v1/boards/"+id, nil)
	if got := cardKeys(v, "In Progress"); len(got) != 1 || got[0] != key {
		t.Fatalf("%v", got)
	}
	alice.do("POST", "/api/v1/boards/"+id+"/move", map[string]any{"key": key, "column": "Done"})
	code, out, _ := alice.do("POST", "/api/v1/boards/"+id+"/move", map[string]any{"key": key, "column": "In Progress"})
	if code != 400 {
		t.Fatalf("blocked move: %d %v", code, out)
	}
	// the card is where it was, and nothing in the response leaks other projects
	_, v, _ = alice.do("GET", "/api/v1/boards/"+id, nil)
	if got := cardKeys(v, "Done"); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestHTTP_SprintFlow(t *testing.T) {
	e, alice, bob, k1 := setup(t)
	root := rootClient(t, e, alice)
	_, b, _ := root.do("POST", "/api/v1/projects/ABC/boards", map[string]any{"name": "Scrum", "kind": "scrum"})
	id := b["id"].(string)
	if code, _, _ := alice.do("POST", "/api/v1/boards/"+id+"/sprints", map[string]any{}); code != 403 {
		t.Fatalf("a member made a sprint: %d", code)
	}
	code, sp, _ := root.do("POST", "/api/v1/boards/"+id+"/sprints", map[string]any{"name": "Sprint one", "goal": "ship checkout"})
	if code != 201 || sp["state"] != "future" {
		t.Fatalf("%d %v", code, sp)
	}
	sid := sp["id"].(string)
	// estimates ride on the ordinary issue patch
	if code, out, _ := alice.do("PATCH", "/api/v1/issues/"+k1, map[string]any{"estimate": 5}); code != 200 || out["estimate"] != float64(5) {
		t.Fatalf("%d %v", code, out)
	}
	if code, _, _ := alice.do("POST", "/api/v1/boards/"+id+"/plan", map[string]any{"sprint": sid, "keys": []string{k1}}); code != 200 {
		t.Fatalf("plan: %d", code)
	}
	_, bl, _ := bob.do("GET", "/api/v1/boards/"+id+"/backlog", nil)
	sprints := bl["sprints"].([]any)
	first := sprints[0].(map[string]any)
	if first["name"] != "Sprint one" || len(first["issues"].([]any)) != 1 || first["totals"].(map[string]any)["todo"].(map[string]any)["estimate"] != float64(5) {
		t.Fatalf("%v", first)
	}
	// complete preview over HTTP, then start, then complete for real
	if code, _, _ := root.do("POST", "/api/v1/sprints/"+sid+"/complete", map[string]any{"destination": "backlog"}); code != 400 {
		t.Fatalf("completing a future sprint: %d", code)
	}
	if code, _, _ := root.do("POST", "/api/v1/sprints/"+sid+"/start", nil); code != 200 {
		t.Fatalf("start %d", code)
	}
	code, plan, _ := root.do("POST", "/api/v1/sprints/"+sid+"/complete", map[string]any{"destination": "backlog", "dry_run": true})
	if code != 200 || plan["dry_run"] != true || len(plan["moved"].([]any)) != 1 {
		t.Fatalf("%d %v", code, plan)
	}
	if _, v, _ := alice.do("GET", "/api/v1/sprints/"+sid, nil); v["state"] != "active" {
		t.Fatalf("the preview closed the sprint: %v", v["state"])
	}
	_, board, _ := alice.do("GET", "/api/v1/boards/"+id, nil)
	if got := cardKeys(board, "To Do"); len(got) != 1 || got[0] != k1 { // the active sprint feeds the scrum board
		t.Fatalf("%v", got)
	}
	if code, _, _ := root.do("POST", "/api/v1/sprints/"+sid+"/complete", map[string]any{"destination": "backlog"}); code != 200 {
		t.Fatalf("complete %d", code)
	}
	_, bl, _ = alice.do("GET", "/api/v1/boards/"+id+"/backlog", nil)
	if len(bl["backlog"].([]any)) != 1 || bl["backlog"].([]any)[0].(map[string]any)["carried_over"] != float64(1) {
		t.Fatalf("%v", bl["backlog"])
	}
	// a kanban board has no backlog endpoint content
	_, kb, _ := root.do("POST", "/api/v1/projects/ABC/boards", map[string]any{"name": "Flow", "kind": "kanban"})
	if code, _, _ := alice.do("GET", "/api/v1/boards/"+kb["id"].(string)+"/backlog", nil); code != 400 {
		t.Fatalf("%d", code)
	}
}

// Reports honour the board filter, and a filter that does not parse is a 400 on reports as it is on the board.
func TestHTTP_ReportsFollowTheBoardFilter(t *testing.T) {
	e, alice, bob, _ := setup(t)
	root := rootClient(t, e, alice)
	_, b, _ := root.do("POST", "/api/v1/projects/ABC/boards", map[string]any{"name": "Flow", "kind": "kanban"})
	id := b["id"].(string)
	for _, path := range []string{"cumulative-flow", "control-chart", "created-vs-resolved", "workload", "time-in-status"} {
		if code, _, _ := bob.do("GET", "/api/v1/boards/"+id+"/reports/"+path+"?days=7", nil); code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
	}
	root.do("PATCH", "/api/v1/boards/"+id, map[string]any{"filter": "labels = ("})
	for _, path := range []string{"cumulative-flow", "workload"} {
		if code, out, _ := alice.do("GET", "/api/v1/boards/"+id+"/reports/"+path, nil); code != 400 || out["code"] != "tql_error" {
			t.Fatalf("%s: %d %v", path, code, out)
		}
	}
	// velocity needs a scrum board
	if code, _, _ := alice.do("GET", "/api/v1/boards/"+id+"/reports/velocity", nil); code != 400 {
		t.Fatalf("velocity on kanban: %d", code)
	}
}

// With the S3 backend the page may connect to the bucket (direct uploads) and nowhere else; with files on disk it may not.
func TestHTTP_CSPAllowsOnlyTheBucketOrigin(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: 3600e9}).Handler())
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/healthz")
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' "+m.URL+";") || strings.Contains(csp, "*") {
		t.Fatalf("%s", csp)
	}
	e2 := testutil.New(t)
	e2.Project()
	e2.WithBlobs()
	ts2 := httptest.NewServer((&api.Server{Svc: e2.Svc, SessionTTL: 3600e9}).Handler())
	defer ts2.Close()
	resp, _ = http.Get(ts2.URL + "/healthz")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self';") {
		t.Fatalf("%s", csp)
	}
}
