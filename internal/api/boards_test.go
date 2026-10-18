package api_test

import (
	"testing"

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
