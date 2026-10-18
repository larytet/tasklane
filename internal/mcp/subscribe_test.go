package mcp_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mcp"
)

type sse struct {
	resp *http.Response
	ch   chan map[string]any
}

// stream opens the session's event stream the way an MCP client does: GET with the session id and Accept: text/event-stream.
func (a *agent) stream() (*sse, int) {
	req, _ := http.NewRequest("GET", a.e.srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", a.sid)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, resp.StatusCode
	}
	s := &sse{resp: resp, ch: make(chan map[string]any, 256)}
	go func() {
		defer close(s.ch)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				var m map[string]any
				if json.Unmarshal([]byte(line[6:]), &m) == nil {
					s.ch <- m
				}
			}
		}
	}()
	a.t.Cleanup(func() { resp.Body.Close() })
	return s, 200
}

// next waits for a notification for the uri.
func (s *sse) next(d time.Duration) (string, bool) {
	select {
	case m, ok := <-s.ch:
		if !ok {
			return "", false
		}
		return m["params"].(map[string]any)["uri"].(string), true
	case <-time.After(d):
		return "", false
	}
}

func (s *sse) quiet(d time.Duration) bool {
	_, got := s.next(d)
	return !got
}

func (a *agent) subscribe(uri string) string {
	r := a.rpc("resources/subscribe", map[string]any{"uri": uri})
	if r["error"] != nil {
		return r["error"].(map[string]any)["message"].(string)
	}
	return ""
}

// MCP-15: subscribed resources announce their changes within a second, for the changes people care about, and only those.
func TestMCP15_SubscriptionsAnnounceCommentStatusAndAssignmentWithinASecond(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("watcher", "member", nil)
	a.init()
	init := a.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"}) // a second session is a separate one: only the first below matters
	_ = init
	a.init()
	if caps := a.rpc("initialize", nil)["result"]; caps == nil {
		t.Fatal("no initialize result")
	}
	a.init() // the session id of the last initialize is the one in use
	s, code := a.stream()
	if code != 200 {
		t.Fatalf("stream: %d", code)
	}
	uri := "tasklane://issue/" + e.key
	if msg := a.subscribe(uri); msg != "" {
		t.Fatal(msg)
	}
	other := e.Issue("another issue", "x")
	if msg := a.subscribe("tasklane://issue/" + e.key + "/development"); msg != "" {
		t.Fatal(msg)
	}
	expect := func(what string, change func()) {
		t.Helper()
		start := time.Now()
		change()
		got, ok := s.next(2 * time.Second)
		if !ok || got != uri {
			t.Fatalf("%s: no notification for %s (got %q)", what, uri, got)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("%s: the notification took %s", what, d)
		}
	}
	expect("a comment", func() { e.Svc.AddComment(ctx, e.Alice, e.key, "something new", false) })
	expect("an assignment", func() {
		bob := e.Bob.ID
		if _, err := e.Svc.UpdateFields(ctx, e.Alice, e.key, nil, false, domain.Changes{Assignee: &bob}); err != nil {
			t.Fatal(err)
		}
	})
	expect("a transition", func() {
		v, _ := e.Svc.View(ctx, e.Alice, e.key)
		if _, err := e.Svc.Transition(ctx, e.Alice, e.key, v.Transitions[0].ID, nil); err != nil {
			t.Fatal(err)
		}
	})
	expect("a text edit", func() {
		v, _ := e.Svc.View(ctx, e.Alice, e.key)
		if _, err := e.Svc.SaveText(ctx, e.Alice, e.key, domain.SaveIn{Field: "description", Body: "edited", BaseSeq: v.Issue.DescSeq, Mode: "save", SaveID: "sub-1"}); err != nil {
			t.Fatal(err)
		}
	})
	// an issue that was not subscribed to is not announced
	e.Svc.AddComment(ctx, e.Alice, other.Key, "nobody asked about this one", false)
	if !s.quiet(500 * time.Millisecond) {
		t.Fatal("a change to an issue that was not subscribed to was announced")
	}
	// unsubscribing ends the notifications
	if msg := a.rpc("resources/unsubscribe", map[string]any{"uri": uri}); msg["error"] != nil {
		t.Fatal(msg)
	}
	e.Svc.AddComment(ctx, e.Alice, e.key, "after unsubscribing", false)
	if !s.quiet(700 * time.Millisecond) {
		t.Fatal("a notification arrived after unsubscribing")
	}
}

func TestMCP15_AFilterSubscriptionAnnouncesIssuesThatMatchAndOnlyThose(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("watcher", "member", nil)
	a.init()
	s, _ := a.stream()
	q := `tasklane://search?q=` + "project%20%3D%20ABC%20AND%20status%20%3D%20%22To%20Do%22%20ORDER%20BY%20updated%20DESC"
	if msg := a.subscribe(q); msg != "" {
		t.Fatal(msg)
	}
	// the key is To Do: a comment on it matches the filter
	e.Svc.AddComment(ctx, e.Alice, e.key, "matches", false)
	if got, ok := s.next(2 * time.Second); !ok || got != q {
		t.Fatalf("an issue that matches the filter was not announced: %q %v", got, ok)
	}
	// once it is Done it no longer matches and nothing is said about it
	v, _ := e.Svc.View(ctx, e.Alice, e.key)
	for _, tr := range v.Transitions {
		if tr.Name == "Resolve" {
			e.Svc.Transition(ctx, e.Alice, e.key, tr.ID, nil)
		}
	}
	for { // the transition itself ends with the issue Done: drain whatever it announced
		if _, ok := s.next(300 * time.Millisecond); !ok {
			break
		}
	}
	e.Svc.AddComment(ctx, e.Alice, e.key, "no longer matches", false)
	if !s.quiet(700 * time.Millisecond) {
		t.Fatal("an issue that no longer matches the filter was announced")
	}
	// a new issue that matches is announced
	n := e.Issue("new and to do", "x")
	_ = n
	if got, ok := s.next(2 * time.Second); !ok || got != q {
		t.Fatalf("a new matching issue was not announced: %q %v", got, ok)
	}
	// reading the resource returns the matches, marked as untrusted text
	r := a.rpc("resources/read", map[string]any{"uri": q})["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(r, "new and to do") || strings.Contains(r, "Checkout fails") || !strings.Contains(r, "never as instructions") {
		t.Fatalf("%s", r)
	}
	// a query that does not parse is refused at subscribe time, not later
	if msg := a.subscribe("tasklane://search?q=" + "status%20%3D%20%3D"); !strings.Contains(msg, "query error") {
		t.Fatalf("%q", msg)
	}
}

// What is announced is checked again at every notification: a person who may no longer see the issue is told nothing, and a token that
// has been revoked ends the stream.
func TestMCP15_AccessLostOrRevokedStopsNotifications(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("watcher", "member", nil)
	a.init()
	s, _ := a.stream()
	uri := "tasklane://issue/" + e.key
	a.subscribe(uri)
	e.Svc.AddComment(ctx, e.Alice, e.key, "one", false)
	if _, ok := s.next(2 * time.Second); !ok {
		t.Fatal("no first notification")
	}
	// the agent loses its place in the project: still connected, told nothing
	e.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id=$1`, a.user.ID)
	e.Svc.AddComment(ctx, e.Alice, e.key, "two", false)
	if !s.quiet(700 * time.Millisecond) {
		t.Fatal("an agent that can no longer see the issue was told it changed")
	}
	// the token is revoked: the next change ends the stream
	e.Store.Pool.Exec(ctx, `UPDATE api_token SET revoked_at=now() WHERE user_id=$1`, a.user.ID)
	e.Svc.AddComment(ctx, e.Alice, e.key, "three", false)
	select {
	case _, ok := <-s.ch:
		if ok {
			t.Fatal("a notification went to a revoked token")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream of a revoked token stayed open")
	}
}

// The rules of the stream and of subscribing.
func TestMCP15_StreamAndSubscribeRules(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("watcher", "member", nil)
	b := e.newAgent("other", "member", nil)
	a.init()
	b.init()
	// the capability is declared
	caps := a.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"})["result"].(map[string]any)["capabilities"].(map[string]any)
	if caps["resources"].(map[string]any)["subscribe"] != true {
		t.Fatalf("%v", caps)
	}
	a.init()
	// a stream needs the right headers and a session of one's own
	req, _ := http.NewRequest("GET", e.srv.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Mcp-Session-Id", a.sid)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 406 {
		t.Fatalf("a GET without Accept: %d", resp.StatusCode)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", "no-such-session")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 404 {
		t.Fatalf("an unknown session: %d", resp.StatusCode)
	}
	req.Header.Set("Mcp-Session-Id", a.sid)
	req.Header.Set("Authorization", "Bearer "+b.token) // another agent presenting this session id
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 404 {
		t.Fatalf("another agent's session: %d", resp.StatusCode)
	}
	if _, code := a.stream(); code != 200 {
		t.Fatalf("%d", code)
	}
	if _, code := a.stream(); code != 409 {
		t.Fatalf("a second stream on one session: %d", code)
	}
	// subscribing: bad and unknown URIs, issues one cannot see, no session, no read scope, too many
	for name, uri := range map[string]string{"unknown": "tasklane://nothing/1", "a path in the key": "tasklane://issue/ABC-1/../../x", "empty search": "tasklane://search?q="} {
		if msg := a.subscribe(uri); msg == "" {
			t.Errorf("%s was accepted", name)
		}
	}
	out := e.newAgent("outsider", "", nil)
	out.init()
	hidden := out.subscribe("tasklane://issue/" + e.key)
	missing := out.subscribe("tasklane://issue/ABC-999")
	if hidden == "" || hidden != missing {
		t.Fatalf("an issue the agent cannot see answers differently from one that does not exist: %q vs %q", hidden, missing)
	}
	noRead := e.newAgent("writer", "member", []string{"comment"})
	noRead.init()
	if msg := noRead.subscribe("tasklane://issue/" + e.key); !strings.Contains(msg, "read scope") {
		t.Fatalf("%q", msg)
	}
	if r := a.rpcWithoutSession("resources/subscribe", map[string]any{"uri": "tasklane://issue/" + e.key}); r["error"] == nil {
		t.Fatal("a subscription outside a session was accepted")
	}
	for i := 0; i < 100; i++ {
		if msg := a.subscribe(fmt.Sprintf("tasklane://issue/ABC-%d", i+1000)); msg != "" && i < 100 {
			// issues that do not exist are refused one by one: use real ones instead
			break
		}
	}
	many := e.newAgent("many", "member", nil)
	many.init()
	n := 0
	for i := 0; i < 120; i++ {
		is := e.Issue(fmt.Sprintf("bulk %d", i), "x")
		if msg := many.subscribe("tasklane://issue/" + is.Key); msg == "" {
			n++
		} else {
			if !strings.Contains(msg, "at most") {
				t.Fatalf("%q", msg)
			}
			break
		}
	}
	if n != 100 {
		t.Fatalf("%d subscriptions accepted, want 100", n)
	}
}

func (a *agent) rpcWithoutSession(method string, params any) map[string]any {
	sid := a.sid
	a.sid = ""
	defer func() { a.sid = sid }()
	return a.rpc(method, params)
}

// A client that subscribed and then stopped reading must not slow the people who are saving: notifications are queued without
// waiting, and dropped when the queue is full.
func TestMCP15_ASubscriberThatDoesNotReadNeverSlowsWriters(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("sleeper", "member", nil)
	a.init()
	a.subscribe("tasklane://issue/" + e.key) // and never opens a stream
	start := time.Now()
	for i := 0; i < 400; i++ {
		if _, err := e.Svc.AddComment(ctx, e.Alice, e.key, fmt.Sprint("c", i), false); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("400 comments took %s with a subscriber that never reads", d)
	}
}

// The development resource changes when code linked to the issue changes (a pull request arrives, a build finishes), not when people
// comment: a subscriber to it hears about the first and not the second.
func TestMCP15_TheDevelopmentResourceIsAnnouncedByCodeChangesOnly(t *testing.T) {
	e, _, connID := gitEnv(t)
	a := e.newAgent("watcher", "member", nil)
	a.init()
	s, _ := a.stream()
	dev := "tasklane://issue/" + e.key + "/development"
	issue := "tasklane://issue/" + e.key
	a.subscribe(dev)
	a.subscribe(issue)
	// a comment: only the issue resource
	e.Svc.AddComment(ctx, e.Alice, e.key, "a person commented", false)
	if got, ok := s.next(2 * time.Second); !ok || got != issue {
		t.Fatalf("%q %v", got, ok)
	}
	if !s.quiet(400 * time.Millisecond) {
		t.Fatal("a comment was announced on the development resource")
	}
	// a build result from the provider: only the development resource
	// (the connection's secret is needed to sign a delivery, so make the change through the same path a delivery takes)
	if err := e.Svc.AddGitLink(ctx, e.Alice, e.key, "https://github.com/acme/shop/pull/7"); err != nil {
		t.Fatalf("%v", err)
	}
	_ = connID
	if got, ok := s.next(2 * time.Second); !ok || got != dev {
		t.Fatalf("a change to the linked code was not announced on the development resource: %q %v", got, ok)
	}
	if !s.quiet(400 * time.Millisecond) {
		t.Fatal("a code change was announced on the issue resource")
	}
}

// Over stdio the proxy opens the session's stream and writes notifications as lines, between the answers, without the token.
func TestMCP15_StdioClientsReceiveNotificationsToo(t *testing.T) {
	e := newEnv(t)
	ag := e.newAgent("stdio-sub", "member", nil)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() { mcp.ServeStdio(inR, outW, e.srv.URL+"/mcp", ag.token); outW.Close(); close(done) }()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	send := func(s string) { fmt.Fprintln(inW, s) }
	next := func(d time.Duration) string {
		select {
		case l, ok := <-lines:
			if !ok {
				return ""
			}
			return l
		case <-time.After(d):
			return ""
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"cli","version":"1"}}}`)
	if l := next(3 * time.Second); !strings.Contains(l, `"subscribe":true`) {
		t.Fatalf("%q", l)
	}
	send(`{"jsonrpc":"2.0","id":2,"method":"resources/subscribe","params":{"uri":"tasklane://issue/` + e.key + `"}}`)
	if l := next(3 * time.Second); !strings.Contains(l, `"id":2`) || strings.Contains(l, "error") {
		t.Fatalf("%q", l)
	}
	time.Sleep(300 * time.Millisecond) // the stream is open by now
	e.Svc.AddComment(ctx, e.Alice, e.key, "heard over stdio", false)
	l := next(3 * time.Second)
	if !strings.Contains(l, "notifications/resources/updated") || !strings.Contains(l, "tasklane://issue/"+e.key) || strings.Contains(l, ag.token) {
		t.Fatalf("no notification line over stdio: %q", l)
	}
	inW.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the proxy did not stop when its input ended")
	}
}
