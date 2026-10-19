package mcp_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/mcp"
	"github.com/larytet/tasklane/internal/search"
)

// node starts another server on the same database, as a second node of the cluster is, with the session key every node derives alike.
func (e *env) node(t *testing.T, withKey bool, now func() time.Time) *httptest.Server {
	m := mcp.New(e.Svc, &search.Postgres{DB: e.Store.Pool})
	if withKey {
		k, err := e.Svc.DerivedKey(ctx, "mcp-session")
		if err != nil {
			t.Fatal(err)
		}
		m.SessionKey = k
	}
	if now != nil {
		m.Now = now
	}
	srv := httptest.NewServer((&api.Server{Svc: e.Svc, MCP: m, SessionTTL: time.Hour}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func (a *agent) on(url string) { a.e.srv = &httptest.Server{URL: url} }

// UPG-03 (MCP sessions): a session started on one node continues on any other, so a client behind a load balancer, or one whose node was replaced
// in a rolling upgrade, does not have to start over. It is the same person's session only; a forged, altered or expired id is not acknowledged.
func TestMCP_SessionsContinueOnAnotherNode(t *testing.T) {
	e := newEnv(t)
	clock := time.Now()
	a := e.node(t, true, func() time.Time { return clock })
	b := e.node(t, true, func() time.Time { return clock })
	ag := e.newAgent("roamer", "member", []string{"read", "comment"})
	orig := e.srv
	defer func() { e.srv = orig }()

	ag.on(a.URL)
	ag.init()
	if !strings.HasPrefix(ag.sid, "tls1.") {
		t.Fatalf("the session id is not self-contained: %q", ag.sid)
	}
	ag.ok("get_issue", map[string]any{"key": e.key})
	sid := ag.sid
	ag.on(b.URL) // the next call lands on the other node
	if r := ag.ok("get_issue", map[string]any{"key": e.key}); r.Text == "" {
		t.Fatal("no answer on the other node")
	}
	// the other node can use it for everything a session is for
	ag.ok("search_issues", map[string]any{"query": "project = ABC"})

	// another person cannot use it
	other := e.newAgent("intruder", "member", []string{"read"})
	other.on(b.URL)
	other.sid = sid
	if code, _, _ := other.post(other.token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil); code != 404 {
		t.Fatalf("another agent used the session: %d", code)
	}
	// a tampered id is refused, on a node that never saw it
	c := e.node(t, true, func() time.Time { return clock })
	other.on(c.URL) // nor on a node that has never seen the id, where only the signature and the owner inside it can say
	if code, _, _ := other.post(other.token, map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/list"}, nil); code != 404 {
		t.Fatalf("another agent used the session on a fresh node: %d", code)
	}
	ag.on(c.URL)
	ag.sid = sid[:len(sid)-3] + "AAA"
	if code, _, _ := ag.post(ag.token, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, nil); code != 404 {
		t.Fatalf("a tampered session id: %d", code)
	}
	ag.sid = sid
	if code, _, _ := ag.post(ag.token, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}, nil); code != 200 {
		t.Fatalf("the genuine id on a third node: %d", code)
	}
	// it expires
	clock = clock.Add(25 * time.Hour)
	d := e.node(t, true, func() time.Time { return clock })
	ag.on(d.URL)
	if code, _, _ := ag.post(ag.token, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/list"}, nil); code != 404 {
		t.Fatalf("an expired session: %d", code)
	}
	// without the key (a node of the previous release, or no secret shared) the old behaviour holds: ids are the issuing node's alone
	old := e.node(t, false, nil)
	ag2 := e.newAgent("local", "member", []string{"read"})
	ag2.on(old.URL)
	ag2.init()
	if strings.HasPrefix(ag2.sid, "tls1.") {
		t.Fatal("a node without the key issued a signed id")
	}
	ag2.on(b.URL)
	if code, _, _ := ag2.post(ag2.token, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/list"}, nil); code != 404 {
		t.Fatalf("a random id from another node: %d", code)
	}
}
