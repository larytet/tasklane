package rt_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/rt"
	"github.com/larytet/tasklane/internal/testutil"
)

type env struct {
	*testutil.Env
	hub *rt.Hub
	srv *httptest.Server
	key string
}

func newEnv(t *testing.T) *env {
	e := testutil.New(t)
	e.Project()
	hub := rt.New(e.Svc)
	srv := httptest.NewServer((&api.Server{Svc: e.Svc, Hub: hub, SessionTTL: time.Hour}).Handler())
	t.Cleanup(srv.Close)
	return &env{Env: e, hub: hub, srv: srv, key: e.Issue("realtime", "text").Key}
}

func (e *env) wsURL(ticket string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/v1/ws?ticket=" + ticket
}

type sock struct {
	t  *testing.T
	c  *websocket.Conn
	in chan map[string]any
}

func (e *env) dial(a domain.Actor) *sock {
	e.T.Helper()
	c, _, err := websocket.Dial(context.Background(), e.wsURL(e.hub.Mint(a)), nil)
	if err != nil {
		e.T.Fatal(err)
	}
	s := &sock{t: e.T, c: c, in: make(chan map[string]any, 256)}
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				close(s.in)
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			s.in <- m
		}
	}()
	e.T.Cleanup(func() { c.CloseNow() })
	return s
}

func (s *sock) send(v any) {
	b, _ := json.Marshal(v)
	if err := s.c.Write(context.Background(), websocket.MessageText, b); err != nil {
		s.t.Fatal(err)
	}
}

// next returns the next message matching the predicate within the timeout.
func (s *sock) next(timeout time.Duration, match func(map[string]any) bool) (map[string]any, bool) {
	deadline := time.After(timeout)
	for {
		select {
		case m, ok := <-s.in:
			if !ok {
				return nil, false
			}
			if match == nil || match(m) {
				return m, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

func typ(t string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["type"] == t }
}

func (s *sock) sub(key string) {
	s.send(map[string]any{"op": "sub", "channel": "issue:" + key})
	if _, ok := s.next(2*time.Second, typ("subscribed")); !ok {
		s.t.Fatal("not subscribed to " + key)
	}
}

func presenceOf(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, p := range m["presence"].([]any) {
		out = append(out, p.(map[string]any))
	}
	return out
}

func p95(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)*95/100]
}

func TestPRES01_PERF12_PresenceReachesOthersQuickly(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.dial(e.Alice), e.dial(e.Bob)
	alice.sub(e.key)
	bob.sub(e.key)
	// Alice learns Bob arrived (he subscribed), then that he is editing the description
	if m, ok := alice.next(2*time.Second, func(m map[string]any) bool {
		return m["type"] == "presence" && len(presenceOf(m)) == 0
	}); !ok {
		t.Fatalf("initial presence %v", m)
	}
	var ds []time.Duration
	for i := 0; i < 40; i++ {
		field := []string{"description", ""}[i%2]
		start := time.Now()
		bob.send(map[string]any{"op": "presence", "channel": "issue:" + e.key, "field": field})
		m, ok := alice.next(2*time.Second, typ("presence"))
		if !ok {
			t.Fatalf("no presence update for %q", field)
		}
		ds = append(ds, time.Since(start))
		ps := presenceOf(m)
		if len(ps) != 1 || ps[0]["name"] != "Bob R." || ps[0]["editing"] != field && !(field == "" && ps[0]["editing"] == nil) {
			t.Fatalf("%v", ps)
		}
	}
	if p := p95(ds); p > time.Second {
		t.Fatalf("presence p95 %v exceeds 1 s", p)
	} else {
		t.Logf("presence change reaches another person: p95 %v", p)
	}
}

func TestPRES01_ExpiryAndLeaveAreBroadcast(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.dial(e.Alice), e.dial(e.Bob)
	alice.sub(e.key)
	bob.sub(e.key)
	bob.send(map[string]any{"op": "presence", "channel": "issue:" + e.key, "field": "description"})
	alice.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "presence" && len(presenceOf(m)) == 1 })
	// closing the tab removes Bob at once
	bob.c.Close(websocket.StatusNormalClosure, "")
	if _, ok := alice.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "presence" && len(presenceOf(m)) == 0 }); !ok {
		t.Fatal("leave not broadcast")
	}
	// a missed heartbeat expires after 30 s (injected clock) and the sweep tells Alice
	e.Svc.Presence.Heartbeat(e.key, e.Bob, "description", false)
	e.hub.BroadcastPresence(e.key)
	alice.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "presence" && len(presenceOf(m)) == 1 })
	e.Clock.Advance(31 * time.Second)
	for _, k := range e.Svc.Presence.Sweep() {
		e.hub.BroadcastPresence(k)
	}
	if _, ok := alice.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "presence" && len(presenceOf(m)) == 0 }); !ok {
		t.Fatal("expiry not broadcast")
	}
}

func TestPERF13_ChangesReachOpenPagesQuickly(t *testing.T) {
	e := newEnv(t)
	bob := e.dial(e.Bob)
	bob.sub(e.key)
	var ds []time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		var want string
		switch i % 3 {
		case 0:
			p := []string{"low", "high"}[i%2]
			e.Svc.UpdateFields(context.Background(), e.Alice, e.key, nil, false, domain.Changes{Priority: &p})
			want = "issue.updated"
		case 1:
			e.Svc.AddComment(context.Background(), e.Alice, e.key, fmt.Sprint("c", i), false)
			want = "comment.added"
		default:
			v, _ := e.Svc.View(context.Background(), e.Alice, e.key)
			e.Svc.SaveText(context.Background(), e.Alice, e.key, domain.SaveIn{Field: "description", Body: fmt.Sprint("t", i), BaseSeq: v.Issue.DescSeq})
			want = "issue.text_saved"
		}
		m, ok := bob.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "event" })
		if !ok {
			t.Fatal("no event for " + want)
		}
		ev := m["event"].(map[string]any)
		if ev["type"] != want || ev["issue"] != e.key || ev["actor"] != "Alice K." {
			t.Fatalf("%v, want %s", ev, want)
		}
		if want == "issue.text_saved" && ev["text_seq"] == nil {
			t.Fatal("text event without text_seq")
		}
		ds = append(ds, time.Since(start))
	}
	t.Logf("saved change visible on another open page: p95 %v", p95(ds))
	if p95(ds) > time.Second {
		t.Fatal("event latency above 1 s")
	}
}

func TestSEC02_HandshakeAndAbuse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// forged, missing, expired and reused tickets are refused before any upgrade
	for name, tk := range map[string]string{"forged": "wt_deadbeef", "missing": ""} {
		_, resp, err := websocket.Dial(ctx, e.wsURL(tk), nil)
		if err == nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("%s ticket: %v %v", name, err, resp)
		}
	}
	tk := e.hub.Mint(e.Alice)
	c, _, err := websocket.Dial(ctx, e.wsURL(tk), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.CloseNow()
	if _, resp, err := websocket.Dial(ctx, e.wsURL(tk), nil); err == nil || resp.StatusCode != 401 {
		t.Fatal("a ticket was used twice")
	}
	e.hub.Now = func() time.Time { return time.Now().Add(time.Minute) }
	old := e.hub.Mint(e.Alice)
	e.hub.Now = time.Now
	e.Clock.Advance(0)
	e.hub.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, resp, err := websocket.Dial(ctx, e.wsURL(old), nil); err == nil || resp.StatusCode != 401 {
		t.Fatal("an expired ticket worked")
	}
	e.hub.Now = time.Now
	// a ticket is bound to the person who minted it: Bob cannot use Alice's channel rights
	// bad origin
	_, resp, err := websocket.Dial(ctx, e.wsURL(e.hub.Mint(e.Alice)), &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("cross-origin handshake: %v %v", err, resp)
	}
	// oversized frame: the connection is closed, the server stays up
	s := e.dial(e.Alice)
	s.c.SetReadLimit(1 << 20)
	s.c.Write(ctx, websocket.MessageText, []byte(`{"op":"ping","pad":"`+strings.Repeat("x", 100000)+`"}`))
	if _, ok := s.next(2*time.Second, nil); ok {
		t.Fatal("oversized frame was processed")
	}
	// malformed JSON and binary frames
	s = e.dial(e.Alice)
	s.c.Write(ctx, websocket.MessageText, []byte(`{not json`))
	if _, ok := s.next(2*time.Second, nil); ok {
		t.Fatal("malformed JSON accepted")
	}
	s = e.dial(e.Alice)
	s.c.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3})
	if _, ok := s.next(2*time.Second, nil); ok {
		t.Fatal("binary frame accepted")
	}
	// flooding is cut off by the rate limit
	s = e.dial(e.Alice)
	closed := false
	for i := 0; i < 400 && !closed; i++ {
		if err := s.c.Write(ctx, websocket.MessageText, []byte(`{"op":"ping"}`)); err != nil {
			closed = true
		}
	}
	for {
		m, ok := s.next(2*time.Second, nil)
		if !ok {
			closed = true
			break
		}
		_ = m
	}
	if !closed {
		t.Fatal("flood not cut off")
	}
	// the server is still healthy
	s = e.dial(e.Alice)
	s.send(map[string]any{"op": "ping"})
	if _, ok := s.next(2*time.Second, typ("pong")); !ok {
		t.Fatal("server unhealthy after abuse")
	}
	// bad channel names and too many subscriptions
	s.send(map[string]any{"op": "sub", "channel": "issue:../../x"})
	if m, ok := s.next(2*time.Second, typ("error")); !ok || m["code"] != "bad_channel" {
		t.Fatalf("%v", m)
	}
	s.send(map[string]any{"op": "frobnicate"})
	if m, ok := s.next(2*time.Second, typ("error")); !ok || m["code"] != "unknown_op" {
		t.Fatalf("%v", m)
	}
}

func TestPRES05_SEC02_ChannelAuthorization(t *testing.T) {
	e := newEnv(t)
	outsider, _ := e.Svc.CreateUser(context.Background(), "eve@example.com", "Eve", "password123", false)
	eve := domain.Actor{ID: outsider.ID, Label: "Eve"}
	s := e.dial(eve)
	// existing-but-forbidden and nonexistent answer identically: no existence leak
	s.send(map[string]any{"op": "sub", "channel": "issue:" + e.key})
	m1, _ := s.next(2*time.Second, typ("error"))
	s.send(map[string]any{"op": "sub", "channel": "issue:ABC-9999"})
	m2, _ := s.next(2*time.Second, typ("error"))
	if m1["code"] != "denied" || m2["code"] != "denied" {
		t.Fatalf("%v %v", m1, m2)
	}
	delete(m1, "channel")
	delete(m2, "channel")
	b1, _ := json.Marshal(m1)
	b2, _ := json.Marshal(m2)
	if string(b1) != string(b2) {
		t.Fatalf("responses differ: %s vs %s", b1, b2)
	}
	// presence without a subscription is refused too
	s.send(map[string]any{"op": "presence", "channel": "issue:" + e.key, "field": "description"})
	if m, _ := s.next(2*time.Second, typ("error")); m["code"] != "denied" {
		t.Fatalf("%v", m)
	}
	if len(e.Svc.Presence.List(e.key, "")) != 0 {
		t.Fatal("an unauthorized user appeared in presence")
	}
	// events stop when access is revoked (checked on the next event after the cache window)
	bob := e.dial(e.Bob)
	bob.sub(e.key)
	e.Svc.AddComment(context.Background(), e.Alice, e.key, "before revoke", false)
	if _, ok := bob.next(2*time.Second, typ("event")); !ok {
		t.Fatal("no event before revocation")
	}
	e.Store.Pool.Exec(context.Background(), `DELETE FROM project_member WHERE user_id=$1`, e.Bob.ID)
	e.hub.Now = func() time.Time { return time.Now().Add(10 * time.Second) } // past the authorization cache
	e.Svc.AddComment(context.Background(), e.Alice, e.key, "after revoke: secret", false)
	m, ok := bob.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "event" || m["type"] == "error" })
	if !ok || m["type"] != "error" || m["code"] != "denied" {
		t.Fatalf("revoked user got %v", m)
	}
	if m, ok := bob.next(500*time.Millisecond, typ("event")); ok {
		t.Fatalf("revoked user still receives events: %v", m)
	}
	e.hub.Now = time.Now
}

func TestPRES06_TrafficOnlyOnStateChange(t *testing.T) {
	e := newEnv(t)
	const viewers = 50
	var socks []*sock
	for i := 0; i < viewers; i++ {
		a := e.Alice
		if i%2 == 1 {
			a = e.Bob
		}
		s := e.dial(a)
		s.sub(e.key)
		socks = append(socks, s)
	}
	heartbeats := func() {
		var wg sync.WaitGroup
		for _, s := range socks {
			wg.Add(1)
			go func(s *sock) {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					s.send(map[string]any{"op": "presence", "channel": "issue:" + e.key, "field": ""})
				}
			}(s)
		}
		wg.Wait()
		time.Sleep(500 * time.Millisecond)
	}
	// round one: two people become present, which is two state changes, each sent to the 50 subscribers
	_, before := e.hub.Stats()
	heartbeats()
	_, mid := e.hub.Stats()
	if got := mid - before; got > 2*viewers {
		t.Fatalf("250 heartbeats caused %d messages; two joins should cause at most %d", got, 2*viewers)
	}
	// round two: nothing changes, so nothing is sent, however many heartbeats arrive
	heartbeats()
	_, after := e.hub.Stats()
	if after != mid {
		t.Fatalf("250 unchanged heartbeats caused %d messages", after-mid)
	}
	t.Logf("joins: %d messages for 2 state changes x %d subscribers; 250 further heartbeats: %d messages", mid-before, viewers, after-mid)
	// the stack is 50 connections but two people
	conns, _ := e.hub.Stats()
	if conns != viewers {
		t.Fatalf("%d connections", conns)
	}
	if got := e.Svc.Presence.List(e.key, ""); len(got) != 2 {
		t.Fatalf("%d people on the issue", len(got))
	}
}

func TestLIVE17_EventsAreBestEffort_SavesNeverDependOnThem(t *testing.T) {
	e := newEnv(t)
	slow := e.dial(e.Bob)
	slow.sub(e.key)
	// the subscriber never reads again; the publisher must not block, and saves must keep working
	slow.c.CloseNow()
	start := time.Now()
	for i := 0; i < 300; i++ {
		if _, err := e.Svc.AddComment(context.Background(), e.Alice, e.key, fmt.Sprint("c", i), false); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("publishing blocked the writers: %v", d)
	}
	v, _ := e.Svc.View(context.Background(), e.Alice, e.key)
	if len(v.Comments) != 300 {
		t.Fatalf("%d comments", len(v.Comments))
	}
	// with the hub removed entirely, everything still works (no hub means no events, nothing else changes)
	e.Svc.Publish = nil
	if _, err := e.Svc.SaveText(context.Background(), e.Alice, e.key, domain.SaveIn{Field: "description", Body: "still works", BaseSeq: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestPRES_HTTPFallbackPresenceReachesSocketClients(t *testing.T) {
	e := newEnv(t)
	alice := e.dial(e.Alice)
	alice.sub(e.key)
	tok, _ := e.Svc.CreateAPIToken(context.Background(), e.Bob.ID, "b")
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/issues/"+e.key+"/presence", strings.NewReader(`{"field":"description"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	m, ok := alice.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "presence" && len(presenceOf(m)) == 1 })
	if !ok || presenceOf(m)[0]["editing"] != "description" {
		t.Fatalf("%v", m)
	}
	req, _ = http.NewRequest("DELETE", e.srv.URL+"/api/v1/issues/"+e.key+"/presence", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	http.DefaultClient.Do(req)
	if _, ok := alice.next(2*time.Second, func(m map[string]any) bool { return m["type"] == "presence" && len(presenceOf(m)) == 0 }); !ok {
		t.Fatal("leave over HTTP not broadcast")
	}
}
