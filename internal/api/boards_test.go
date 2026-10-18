package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
	"github.com/larytet/tasklane/internal/testutil/osmock"
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

func TestHTTP_GitWebhookToPanel(t *testing.T) {
	e, alice, bob, key := setup(t)
	root := rootClient(t, e, alice)
	code, out, _ := root.do("POST", "/api/v1/git/connections", map[string]any{"name": "gh", "provider": "github", "token": "ghp_secret_token"})
	if code != 201 || out["webhook_secret"] == nil || out["webhook_url"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	secret := out["webhook_secret"].(string)
	connID := out["connection"].(map[string]any)["id"].(string)
	if s, _ := json.Marshal(out); strings.Contains(string(s), "ghp_secret_token") {
		t.Fatal("the token came back in a response")
	}
	// the secret is shown once: the list has only fingerprints
	_, raw, _ := root.do("GET", "/api/v1/git/connections", nil)
	if s, _ := json.Marshal(raw); strings.Contains(string(s), secret) || strings.Contains(string(s), "ghp_secret_token") {
		t.Fatalf("a credential was listed: %s", s)
	}
	if code, _, _ := root.do("POST", "/api/v1/git/connections/"+connID+"/repos", map[string]any{"full_name": "acme/shop", "projects": []string{"ABC"}}); code != 201 {
		t.Fatalf("%d", code)
	}
	send := func(h gitmock.Hook, withToken string) int {
		req, _ := http.NewRequest("POST", alice.base+"/api/v1/git/webhooks/"+connID, bytes.NewReader(h.Body))
		for k, v := range h.Header {
			req.Header[k] = v
		}
		if withToken != "" {
			req.Header.Set("Authorization", "Bearer "+withToken) // a session or token does not stand in for a signature
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	pr := gitmock.GHPR("acme/shop", 3, key+" webhook", "", "feat", "1a2b3c4d", "open", false, time.Now())
	if c := send(gitmock.GitHubHook("wrong", "pull_request", "w1", pr), alice.token); c != 403 {
		t.Fatalf("a bad signature with a valid token: %d", c)
	}
	if c := send(gitmock.GitHubHook(secret, "pull_request", "w1", pr), ""); c != 200 { // anonymous, but signed
		t.Fatalf("%d", c)
	}
	if n, err := e.Svc.ProcessGitInbox(testutil.Ctx(), 10); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	_, panel, _ := bob.do("GET", "/api/v1/issues/"+key+"/development", nil)
	prs := panel["pull_requests"].([]any)
	if len(prs) != 1 || prs[0].(map[string]any)["state"] != "open" || panel["summary"] != "1 PR open" {
		t.Fatalf("%v", panel)
	}
	// members cannot manage connections
	if code, _, _ := alice.do("GET", "/api/v1/git/connections", nil); code != 403 {
		t.Fatalf("%d", code)
	}
}

// The index behind the API: search answers come from OpenSearch with PostgreSQL's authorization, the operator endpoints
// work, and an outage degrades to PostgreSQL with a warning instead of an error.
func TestHTTP_OpenSearchEngineAndAdminEndpoints(t *testing.T) {
	e, alice, bob, key := setup(t)
	m := osmock.New()
	defer m.Close()
	c := &search.OSClient{BaseURL: m.URL, HTTP: egress.Policy{AllowPrivate: true}.Client()}
	ix := &search.OSIndex{DB: e.Store.Pool, C: c, Sleep: func(time.Duration) {}}
	if err := ix.Ensure(testutil.Ctx()); err != nil {
		t.Fatal(err)
	}
	pg := &search.Postgres{DB: e.Store.Pool}
	router := &search.Router{PG: pg, OS: &search.OpenSearch{C: c, PG: pg}, Index: ix}
	admin := &search.Admin{Index: ix, Router: router}
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: 3600e9, Search: router, SearchAdmin: admin}).Handler())
	defer ts.Close()
	alice.base, bob.base = ts.URL, ts.URL
	root := rootClient(t, e, alice)
	root.base = ts.URL
	sync := func() {
		for {
			if n, err := ix.RunOnce(testutil.Ctx()); err != nil || n == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	sync()
	code, out, _ := alice.do("GET", "/api/v1/issues?q="+url.QueryEscape(`text ~ "checkout"`), nil)
	if code != 200 || out["engine"] != "opensearch" || len(out["issues"].([]any)) != 1 || out["issues"].([]any)[0].(map[string]any)["key"] != key {
		t.Fatalf("%d %v", code, out)
	}
	// operator views are for administrators
	if code, _, _ := alice.do("GET", "/api/v1/admin/search", nil); code != 403 {
		t.Fatalf("%d", code)
	}
	_, st, _ := root.do("GET", "/api/v1/admin/search", nil)
	if st["engine"] != "opensearch" || st["health"] != "green" || st["backlog_events"] != float64(0) || st["dead_letters"] != float64(0) {
		t.Fatalf("%v", st)
	}
	code, rep, _ := root.do("POST", "/api/v1/admin/search/reconcile", map[string]any{})
	if code != 200 || rep["checked"] != float64(1) {
		t.Fatalf("%d %v", code, rep)
	}
	if code, _, _ := root.do("POST", "/api/v1/admin/search/reindex", map[string]any{"project": "ABC"}); code != 200 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := root.do("POST", "/api/v1/admin/search/rebuild", nil); code != 202 {
		t.Fatalf("%d", code)
	}
	for i := 0; i < 100; i++ { // the rebuild runs in the background
		if _, st, _ := root.do("GET", "/api/v1/admin/search", nil); st["running"] == nil || st["running"] == "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, st, _ = root.do("GET", "/api/v1/admin/search", nil)
	if st["previous_index"] == nil || st["previous_index"] == "" || st["last_error"] != nil {
		t.Fatalf("the rebuild did not finish: %v", st)
	}
	if code, _, _ := root.do("POST", "/api/v1/admin/search/rollback", nil); code != 200 {
		t.Fatalf("%d", code)
	}
	// an outage is a warning, never an error
	m.Fail(100000, 503, nil)
	code, out, _ = bob.do("GET", "/api/v1/issues?q="+url.QueryEscape(`text ~ "checkout"`), nil)
	if code != 200 || out["engine"] != nil || !strings.Contains(out["warning"].(string), "search is limited") {
		t.Fatalf("%d %v", code, out)
	}
}
