//go:build interop

// Package interop holds the real-service interoperability smoke (TESTPLAN 3.3): the shipped binary, two nodes, a real PostgreSQL, a real Redis and
// a real OpenSearch from deploy/compose/docker-compose.interop.yml, started by `make interop`. Basic happy paths only. These tests fail, never skip,
// when a service is missing, and every scenario asserts which engine and which bus actually answered: a silent fallback to PostgreSQL or to
// local-only presence is a failure.
package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

func env(t testing.TB, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is not set: the interop suite runs under `make interop` against the compose profile (it does not skip)", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type client struct {
	base string
	http *http.Client
}

func login(t testing.TB, base, email, password string) *client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &client{base: base, http: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
	st, body := c.do(t, "POST", "/api/v1/login", map[string]string{"email": email, "password": password})
	if st != 200 {
		t.Fatalf("login %s on %s: %d %s", email, base, st, body)
	}
	return c
}

func (c *client) do(t testing.TB, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "tasklane")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c *client) json(t testing.TB, method, path string, body any, want int) map[string]any {
	t.Helper()
	st, b := c.do(t, method, path, body)
	if st != want {
		t.Fatalf("%s %s: %d (want %d) %s", method, path, st, want, b)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func eventually(t testing.TB, what string, within time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

func compose(t testing.TB, args ...string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", env(t, "INTEROP_COMPOSE")+" "+strings.Join(args, " "))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("compose %v: %v\n%s", args, err, out)
	}
	return string(out)
}

var (
	once       sync.Once
	n1, n2     *client
	ivy1, ivy2 *client
	ivyID      string
	adminID    string
	pool       *pgxpool.Pool
	svc        *domain.Service
)

// setup logs in on both nodes once, and creates the data the search scenarios share.
func setup(t *testing.T) {
	t.Helper()
	once.Do(func() {
		n1 = login(t, env(t, "INTEROP_APP1"), "admin@example.com", "adminpass1")
		n2 = login(t, env(t, "INTEROP_APP2"), "admin@example.com", "adminpass1")
		var err error
		if pool, err = pgxpool.New(context.Background(), env(t, "INTEROP_DB")); err != nil {
			t.Fatal(err)
		}
		svc = domain.New(pool)
		n1.json(t, "POST", "/api/v1/projects", map[string]string{"key": "IOP", "name": "Interop"}, 201)
		n1.json(t, "POST", "/api/v1/projects", map[string]string{"key": "IOQ", "name": "Hidden"}, 201)
		u := n1.json(t, "POST", "/api/v1/users", map[string]any{"email": "ivy@example.com", "name": "Ivy", "password": "ivypassword1"}, 201)
		ivyID, _ = u["id"].(string)
		n1.json(t, "POST", "/api/v1/projects/IOP/members", map[string]string{"user": ivyID, "role": "member"}, 200)
		pool.QueryRow(context.Background(), `SELECT id::text FROM app_user WHERE email='admin@example.com'`).Scan(&adminID)
		colors := []string{"red", "green", "blue"}
		words := []string{"alpha", "bravo", "charlie", "delta"}
		prios := []string{"lowest", "low", "medium", "high", "highest"}
		for i := 1; i <= 40; i++ {
			is := n1.json(t, "POST", "/api/v1/issues", map[string]any{"project": "IOP", "type": "Task", "summary": fmt.Sprintf("Widget %d %s", i, colors[i%3]),
				"description": fmt.Sprintf("The %s part of widget number %d", words[i%4], i), "priority": prios[i%5], "labels": []string{colors[i%3]}}, 201)
			if i%5 == 0 {
				n1.json(t, "POST", "/api/v1/issues/"+is["key"].(string)+"/comments", map[string]any{"body": "a needle comment on it"}, 201)
			}
		}
		for i := 1; i <= 5; i++ {
			n1.json(t, "POST", "/api/v1/issues", map[string]any{"project": "IOQ", "type": "Task", "summary": fmt.Sprintf("Secret plan %d", i), "description": "the alpha of the hidden plan"}, 201)
		}
		ivy1 = login(t, env(t, "INTEROP_APP1"), "ivy@example.com", "ivypassword1")
		ivy2 = login(t, env(t, "INTEROP_APP2"), "ivy@example.com", "ivypassword1")
	})
}

type result struct {
	Issues []struct {
		Key string `json:"key"`
	} `json:"issues"`
	Total   int    `json:"total"`
	Engine  string `json:"engine"`
	Warning string `json:"warning"`
}

func (c *client) search(t testing.TB, q string) result {
	t.Helper()
	st, b := c.do(t, "GET", "/api/v1/issues?limit=200&q="+urlEscape(q), nil)
	if st != 200 {
		t.Fatalf("search %q: %d %s", q, st, b)
	}
	var r result
	json.Unmarshal(b, &r)
	return r
}

func urlEscape(s string) string {
	r := strings.NewReplacer(" ", "%20", "\"", "%22", "(", "%28", ")", "%29", ",", "%2C", "~", "%7E", "=", "%3D", "!", "%21", "<", "%3C", ">", "%3E")
	return r.Replace(s)
}

func keys(r result, ordered bool) string {
	ks := make([]string, 0, len(r.Issues))
	for _, i := range r.Issues {
		ks = append(ks, i.Key)
	}
	if !ordered {
		sort.Strings(ks)
	}
	return strings.Join(ks, ",")
}

// pgAnswer asks the PostgreSQL engine directly, on the same data, as the person with the given id.
func pgAnswer(t testing.TB, userID, q string) result {
	t.Helper()
	ctx := context.Background()
	a, err := svc.ActorFor(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	all, rd, wr, err := svc.SearchScope(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	res, err := (&search.Postgres{DB: pool}).Search(ctx, search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: time.Now}, q, search.Page{Limit: 200})
	if err != nil {
		t.Fatalf("postgres %q: %v", q, err)
	}
	b, _ := json.Marshal(res)
	var r result
	json.Unmarshal(b, &r)
	return r
}

// waitIndexed waits until the index has caught up with everything the setup wrote.
func waitIndexed(t *testing.T, c *client, q string, total int) {
	t.Helper()
	eventually(t, fmt.Sprintf("OpenSearch to answer %q with %d issues", q, total), 120*time.Second, func() bool {
		r := c.search(t, q)
		return r.Engine == "opensearch" && r.Total == total
	})
}

// INTEROP-01: the stack is up on both nodes, and the services are the real ones, reachable, and in use.
func TestINTEROP01_StackIsUpAndTheServicesAreInUse(t *testing.T) {
	setup(t)
	for _, base := range []string{env(t, "INTEROP_APP1"), env(t, "INTEROP_APP2")} {
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "ready" {
			t.Fatalf("%s/readyz: %d %q (a degraded node, or a bus that is down, is a failure here)", base, resp.StatusCode, b)
		}
	}
	// the cluster: two live nodes, and each says its bus link is up
	eventually(t, "two live nodes with the event bus connected", 60*time.Second, func() bool {
		for _, c := range []*client{n1, n2} {
			cl := c.json(t, "GET", "/api/v1/admin/cluster", nil, 200)
			ev, _ := cl["events"].(map[string]any)
			nodes, _ := cl["nodes"].([]any)
			live := 0
			for _, n := range nodes {
				if m, _ := n.(map[string]any); m["live"] == true {
					live++
				}
			}
			if ev == nil || ev["connected"] != true || live < 2 {
				return false
			}
		}
		return true
	})
	// doctor, run inside the first node's container, names both services with their versions
	out := compose(t, "exec -T app1 /usr/local/bin/tasklane doctor")
	for _, want := range []string{"redis", envOr("INTEROP_REDIS_EXPECT", "Redis 7"), "search", envOr("INTEROP_OS_EXPECT", "OpenSearch 2.")} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FAIL") {
		t.Fatalf("doctor reports a failure:\n%s", out)
	}
	// the index and its alias exist, with the strict mapping
	os_ := env(t, "INTEROP_OS")
	eventually(t, "the OpenSearch alias", 60*time.Second, func() bool {
		resp, err := http.Get(os_ + "/_alias/tasklane-issues")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	})
	resp, err := http.Get(os_ + "/tasklane-issues/_mapping")
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(mb), `"dynamic":"strict"`) || !strings.Contains(string(mb), `"summary"`) {
		t.Fatalf("the index mapping is not the expected one: %.400s", mb)
	}
	st := n1.json(t, "GET", "/api/v1/admin/search", nil, 200)
	if st["engine"] != "opensearch" || st["active_index"] == "" || st["active_index"] == nil {
		t.Fatalf("the admin search status: %v", st)
	}
}

var parityQueries = []struct {
	q       string
	ordered bool
}{
	{`project = IOP`, false},
	{`project = IOP AND priority = high`, false},
	{`labels = red AND project = IOP`, false},
	{`text ~ "bravo" AND project = IOP`, false},
	{`summary ~ "widget"`, false},
	{`text ~ "needle"`, false},
	{`project = IOP AND status = "To Do" ORDER BY created ASC`, true},
	{`labels in (red, blue) AND priority in (low, lowest) AND project = IOP`, false},
	{`NOT labels = red AND project = IOP`, false},
	{`text ~ "alpha"`, false},
}

func checkParity(t *testing.T, label string) {
	t.Helper()
	for _, who := range []struct {
		name string
		id   string
		node []*client
	}{{"admin", adminID, []*client{n1, n2}}, {"restricted user", ivyID, []*client{ivy1, ivy2}}} {
		for _, p := range parityQueries {
			want := pgAnswer(t, who.id, p.q)
			for i, c := range who.node {
				got := c.search(t, p.q)
				if got.Engine != "opensearch" || got.Warning != "" {
					t.Errorf("%s: %s on node %d, %q: answered by %q with warning %q (a fallback is a failure)", label, who.name, i+1, p.q, got.Engine, got.Warning)
					continue
				}
				if keys(got, p.ordered) != keys(want, p.ordered) {
					t.Errorf("%s: %s on node %d, %q:\n  opensearch %s\n  postgres   %s", label, who.name, i+1, p.q, keys(got, p.ordered), keys(want, p.ordered))
				}
			}
		}
	}
}

// INTEROP-02: ordinary searches on the real OpenSearch agree with the PostgreSQL engine on the same data, and a restricted person sees only what they may.
func TestINTEROP02_SearchesAgreeWithPostgres(t *testing.T) {
	setup(t)
	waitIndexed(t, n1, "project = IOP", 40)
	waitIndexed(t, n1, "project = IOQ", 5)
	waitIndexed(t, n2, "project = IOP", 40)
	checkParity(t, "initial")
	// the restricted user cannot see the other project, on either engine
	r := ivy1.search(t, `text ~ "alpha"`)
	for _, i := range r.Issues {
		if strings.HasPrefix(i.Key, "IOQ-") {
			t.Fatalf("a restricted user found %s", i.Key)
		}
	}
	if len(r.Issues) == 0 {
		t.Fatal("the restricted user found nothing in their own project")
	}
}

// INTEROP-03: the index follows updates, transitions, comments, deletes and moves, and a rebuild of the index leaves the answers the same.
func TestINTEROP03_TheIndexFollowsChangesAndRebuilds(t *testing.T) {
	setup(t)
	waitIndexed(t, n1, "project = IOP", 40)
	// new text on IOP-1, a comment on IOP-2
	v := n1.json(t, "GET", "/api/v1/issues/IOP-1/view", nil, 200)
	seq := int64(0)
	if is, _ := v["issue"].(map[string]any); is != nil {
		if f, ok := is["description_seq"].(float64); ok {
			seq = int64(f)
		}
	}
	n1.json(t, "PUT", "/api/v1/issues/IOP-1/text/description", map[string]any{"body": "now it mentions zephyr", "base_seq": seq, "save_id": "interop-1"}, 200)
	n1.json(t, "POST", "/api/v1/issues/IOP-2/comments", map[string]any{"body": "a quokka was here"}, 201)
	eventually(t, "the new text to be found", 90*time.Second, func() bool {
		r := n2.search(t, `text ~ "zephyr"`)
		return r.Engine == "opensearch" && keys(r, false) == "IOP-1"
	})
	eventually(t, "the new comment to be found", 90*time.Second, func() bool {
		r := n1.search(t, `text ~ "quokka"`)
		return r.Engine == "opensearch" && keys(r, false) == "IOP-2"
	})
	// a transition: IOP-3 to its Done status
	view := n1.json(t, "GET", "/api/v1/issues/IOP-3/view", nil, 200)
	tid := ""
	if ts, ok := view["transitions"].([]any); ok {
		for _, x := range ts {
			m, _ := x.(map[string]any)
			if m["to"] == "Done" || m["name"] == "Resolve" || m["name"] == "Done" {
				tid, _ = m["id"].(string)
			}
		}
	}
	if tid == "" {
		t.Fatalf("no transition to Done in %v", view["transitions"])
	}
	n1.json(t, "POST", "/api/v1/issues/IOP-3/transitions", map[string]any{"transition": tid}, 200)
	eventually(t, "the transition to be indexed", 90*time.Second, func() bool {
		r := n1.search(t, `project = IOP AND statusCategory = Done`)
		return r.Engine == "opensearch" && keys(r, false) == "IOP-3"
	})
	// a delete and a move
	n1.json(t, "POST", "/api/v1/issues/IOP-4/lifecycle", map[string]any{"op": "delete"}, 200)
	mv := n1.json(t, "POST", "/api/v1/issues/IOP-5/lifecycle", map[string]any{"op": "move", "project": "IOQ"}, 200)
	newKey, _ := mv["result_key"].(string)
	if !strings.HasPrefix(newKey, "IOQ-") {
		t.Fatalf("the move answered %v", mv)
	}
	eventually(t, "the delete and the move to be indexed", 90*time.Second, func() bool {
		a := n1.search(t, `project = IOP`)
		b := n1.search(t, `key = `+newKey)
		return a.Engine == "opensearch" && a.Total == 38 && b.Engine == "opensearch" && keys(b, false) == newKey
	})
	checkParity(t, "after the changes")
	// rebuild the index and ask again
	before := n1.json(t, "GET", "/api/v1/admin/search", nil, 200)["active_index"]
	n1.json(t, "POST", "/api/v1/admin/search/rebuild", map[string]any{}, 202)
	eventually(t, "the rebuilt index to become the active one", 180*time.Second, func() bool {
		st := n1.json(t, "GET", "/api/v1/admin/search", nil, 200)
		return st["active_index"] != before && (st["running"] == nil || st["running"] == "")
	})
	waitIndexed(t, n1, "project = IOP", 38)
	checkParity(t, "after the rebuild")
}

type socket struct {
	c    *websocket.Conn
	mu   sync.Mutex
	msgs []map[string]any
}

func (s *socket) has(f func(map[string]any) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if f(m) {
			return true
		}
	}
	return false
}

func openSocket(t *testing.T, c *client, key string) *socket {
	t.Helper()
	tk := c.json(t, "POST", "/api/v1/ws/ticket", nil, 200)
	ticket, _ := tk["ticket"].(string)
	url := "ws" + strings.TrimPrefix(c.base, "http") + "/api/v1/ws?ticket=" + ticket
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {c.base}}})
	if err != nil {
		t.Fatalf("websocket on %s: %v", c.base, err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	s := &socket{c: ws}
	go func() {
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				s.mu.Lock()
				s.msgs = append(s.msgs, m)
				s.mu.Unlock()
			}
		}
	}()
	send := func(m map[string]any) {
		b, _ := json.Marshal(m)
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	send(map[string]any{"op": "sub", "channel": "issue:" + key})
	eventually(t, "the subscription", 10*time.Second, func() bool {
		return s.has(func(m map[string]any) bool { return m["type"] == "subscribed" })
	})
	// the page's presence heartbeat
	send(map[string]any{"op": "presence", "channel": "issue:" + key, "field": ""})
	return s
}

// INTEROP-04: two people on different nodes: each sees the other, a save on one node reaches the other node's socket, and a save from the old version conflicts.
func TestINTEROP04_PresenceEventsAndConflictsAcrossNodes(t *testing.T) {
	setup(t)
	key := "IOP-10"
	adminSock := openSocket(t, n1, key)
	ivySock := openSocket(t, ivy2, key)
	eventually(t, "each person to see the other, across nodes, through Redis", 30*time.Second, func() bool {
		sees := func(s *socket, userID string) bool {
			return s.has(func(m map[string]any) bool {
				if m["type"] != "presence" {
					return false
				}
				ps, _ := m["presence"].([]any)
				for _, p := range ps {
					if pm, _ := p.(map[string]any); pm["user_id"] == userID {
						return true
					}
				}
				return false
			})
		}
		return sees(adminSock, ivyID) && sees(ivySock, adminID)
	})
	// both read the same version of the description; the admin on node 1 saves
	v := ivy2.json(t, "GET", "/api/v1/issues/"+key+"/view", nil, 200)
	seq := int64(0)
	if is, _ := v["issue"].(map[string]any); is != nil {
		if f, ok := is["description_seq"].(float64); ok {
			seq = int64(f)
		}
	}
	n1.json(t, "PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": "saved on node one", "base_seq": seq, "save_id": "x-1"}, 200)
	eventually(t, "the save event on the other node's socket", 15*time.Second, func() bool {
		return ivySock.has(func(m map[string]any) bool {
			ev, _ := m["event"].(map[string]any)
			return m["type"] == "event" && ev != nil && ev["issue"] == key && ev["type"] == "issue.text_saved"
		})
	})
	// the second person saves from the old version: the normal conflict, not data loss
	st, body := ivy2.do(t, "PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": "saved on node two", "base_seq": seq, "save_id": "x-2"})
	if st != 409 || !strings.Contains(string(body), "text_conflict") {
		t.Fatalf("a save from the old version: %d %s", st, body)
	}
	d := n2.json(t, "GET", "/api/v1/issues/"+key+"/view", nil, 200)
	if is, _ := d["issue"].(map[string]any); is == nil || !strings.Contains(fmt.Sprint(is["description"]), "saved on node one") && !strings.Contains(fmt.Sprint(d), "saved on node one") {
		t.Fatalf("the first save is not what node two reads: %v", d["issue"])
	}
}

// INTEROP-05: a pause taken on node 1 holds a writer on node 2 (which acknowledges it), and resuming clears it on both.
func TestINTEROP05_PauseAcrossNodes(t *testing.T) {
	setup(t)
	lease := n1.json(t, "POST", "/api/v1/admin/pause", map[string]any{"scope": "project:IOP", "owner": "interop", "drain_wait_ms": 20000}, 200)
	id, _ := lease["id"].(string)
	if lease["state"] != "paused" || id == "" {
		t.Fatalf("the pause: %v", lease)
	}
	var acks int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM pause_ack WHERE lease_id::text=$1`, id).Scan(&acks)
	if acks < 1 {
		t.Fatalf("node 2 did not acknowledge the pause (%d acknowledgements): the barrier did not run across the real nodes", acks)
	}
	st, body := n2.do(t, "POST", "/api/v1/issues/IOP-11/comments", map[string]any{"body": "written during the pause"})
	if st != 503 || !strings.Contains(string(body), "paused") {
		t.Fatalf("a write on node 2 during the pause: %d %s", st, body)
	}
	other := n2.json(t, "POST", "/api/v1/issues", map[string]any{"project": "IOQ", "type": "Task", "summary": "another project is not paused"}, 201)
	if other["key"] == nil {
		t.Fatal("a write to another project was refused")
	}
	n1.json(t, "DELETE", "/api/v1/admin/pause/"+id, nil, 200)
	eventually(t, "node 2 to accept writes again", 15*time.Second, func() bool {
		st, _ := n2.do(t, "POST", "/api/v1/issues/IOP-11/comments", map[string]any{"body": "after the resume"})
		return st == 201
	})
}
