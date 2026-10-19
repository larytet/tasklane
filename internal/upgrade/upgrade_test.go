//go:build upgrade

// Package upgrade is the rolling-upgrade test (TESTPLAN UPG-02): two nodes of the previous release behind a load balancer, a crowd of simulated
// people working through it, and the nodes replaced one at a time by the current release while they work. It runs the real binaries from
// deploy/compose/docker-compose.upgrade.yml, started by `make upgrade`.
package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

func need(t testing.TB, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is not set: run this under `make upgrade`", k)
	}
	return v
}

func compose(t testing.TB, args ...string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", need(t, "UPGRADE_COMPOSE")+" "+strings.Join(args, " ")).CombinedOutput()
	if err != nil {
		t.Fatalf("compose %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func port(t testing.TB, svc string, p int) string {
	t.Helper()
	out := strings.TrimSpace(compose(t, "port", svc, fmt.Sprint(p)))
	return out[strings.LastIndex(out, ":")+1:]
}

func waitReady(t testing.TB, base string, within time.Duration) {
	t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if r, err := http.Get(base + "/readyz"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s never became ready", base)
}

// ---- the load balancer: round robin over the nodes that pass /readyz, as a real one does; a node that begins to stop fails it at once

type lb struct {
	mu       sync.Mutex
	backends map[string]*backend
	rr       int
}

type backend struct {
	u     *url.URL
	proxy *httputil.ReverseProxy
	ok    bool
}

func newLB() *lb { return &lb{backends: map[string]*backend{}} }

func (l *lb) add(base string) {
	u, _ := url.Parse(base)
	p := httputil.NewSingleHostReverseProxy(u)
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) { w.WriteHeader(http.StatusBadGateway) }
	l.mu.Lock()
	l.backends[base] = &backend{u: u, proxy: p}
	l.mu.Unlock()
}

func (l *lb) remove(base string) { l.mu.Lock(); delete(l.backends, base); l.mu.Unlock() }

func (l *lb) check(ctx context.Context) {
	hc := &http.Client{Timeout: 500 * time.Millisecond}
	for ctx.Err() == nil {
		l.mu.Lock()
		names := make([]string, 0, len(l.backends))
		for n := range l.backends {
			names = append(names, n)
		}
		l.mu.Unlock()
		for _, n := range names {
			ok := false
			if r, err := hc.Get(n + "/readyz"); err == nil {
				ok = r.StatusCode == 200
				r.Body.Close()
			}
			l.mu.Lock()
			if b := l.backends[n]; b != nil {
				b.ok = ok
			}
			l.mu.Unlock()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (l *lb) pick() *backend {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ok []string
	for n, b := range l.backends {
		if b.ok {
			ok = append(ok, n)
		}
	}
	if len(ok) == 0 {
		return nil
	}
	sort.Strings(ok)
	l.rr++
	return l.backends[ok[l.rr%len(ok)]]
}

func (l *lb) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := l.pick()
	if b == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	b.proxy.ServeHTTP(w, r)
}

// ---- the crowd

type stats struct {
	total, ok, conflicts, failed atomic.Int64
	mu                           sync.Mutex
	lat                          []time.Duration
	failures                     []string
}

func (s *stats) record(d time.Duration, status int, err error, what string) {
	s.total.Add(1)
	switch {
	case err == nil && status >= 200 && status < 300:
		s.ok.Add(1)
		s.mu.Lock()
		s.lat = append(s.lat, d)
		s.mu.Unlock()
	case err == nil && status == 409:
		s.conflicts.Add(1)
	default:
		s.failed.Add(1)
		s.mu.Lock()
		if len(s.failures) < 20 {
			s.failures = append(s.failures, fmt.Sprintf("%s: %d %v", what, status, err))
		}
		s.mu.Unlock()
	}
}

type user struct {
	email, key string
	c          *http.Client
	base       string
	seq        int64
	acked      string
	pending    string
	comments   []string
	st         *stats
}

func newClient(base string) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func (u *user) do(method, path string, body any, retry bool) (int, []byte, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, u.base+path, rd)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "tasklane")
		start := time.Now()
		resp, err := u.c.Do(req)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if retry && attempt == 0 && (resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504) {
				time.Sleep(400 * time.Millisecond)
				continue
			}
			u.st.record(time.Since(start), resp.StatusCode, nil, method+" "+path)
			return resp.StatusCode, b, nil
		}
		last = err
		if retry && attempt == 0 {
			time.Sleep(400 * time.Millisecond)
			continue
		}
		break
	}
	u.st.record(0, 0, last, method+" "+path)
	return 0, nil, last
}

func (u *user) sync() {
	st, b, err := u.do("GET", "/api/v1/issues/"+u.key+"/view", nil, true)
	if err != nil || st != 200 {
		return
	}
	var v struct {
		Issue struct {
			DescSeq int64 `json:"description_seq"`
		} `json:"issue"`
	}
	json.Unmarshal(b, &v)
	u.seq = v.Issue.DescSeq
}

func (u *user) run(stop <-chan struct{}, rng *rand.Rand) {
	n := 0
	needSync := true
	for {
		select {
		case <-stop:
			return
		default:
		}
		if needSync {
			u.sync()
			needSync = false
		}
		switch r := rng.Intn(100); {
		case r < 45: // a save of the description, idempotent by its save id so a retry cannot apply it twice
			n++
			body := fmt.Sprintf("%s revision %d", u.email, n)
			u.pending = body
			st, b, err := u.do("PUT", "/api/v1/issues/"+u.key+"/text/description", map[string]any{"body": body, "base_seq": u.seq, "save_id": fmt.Sprintf("%s-%d", u.email, n)}, true)
			switch {
			case err == nil && st == 200:
				var res struct {
					Seq int64 `json:"seq"`
				}
				json.Unmarshal(b, &res)
				u.seq, u.acked, u.pending = res.Seq, body, ""
			default:
				needSync = true
			}
		case r < 65:
			u.do("GET", "/api/v1/issues/"+u.key+"/view", nil, true)
		case r < 85:
			u.do("GET", "/api/v1/issues?limit=20&q="+url.QueryEscape("project = UPG AND text ~ \"revision\""), nil, true)
		default:
			body := fmt.Sprintf("%s comment %d", u.email, n)
			st, _, err := u.do("POST", "/api/v1/issues/"+u.key+"/comments", map[string]any{"body": body}, false)
			if err == nil && st == 201 {
				u.comments = append(u.comments, body)
			}
		}
		time.Sleep(time.Duration(40+rng.Intn(80)) * time.Millisecond)
	}
}

// ---- sockets that reconnect with jitter, as the page does

type watcher struct {
	base, email string
	key         string
	connected   atomic.Bool
	reconnects  atomic.Int64
	maxOutage   atomic.Int64 // nanoseconds
}

func (w *watcher) run(ctx context.Context) {
	c := newClient(w.base)
	u := &user{base: w.base, c: c, st: &stats{}}
	u.do("POST", "/api/v1/login", map[string]string{"email": w.email, "password": "crowdpass1"}, true)
	down := time.Time{}
	for ctx.Err() == nil {
		st, b, err := u.do("POST", "/api/v1/ws/ticket", nil, false)
		var tk struct {
			Ticket string `json:"ticket"`
		}
		json.Unmarshal(b, &tk)
		if err == nil && st == 200 && tk.Ticket != "" {
			wsURL := "ws" + strings.TrimPrefix(w.base, "http") + "/api/v1/ws?ticket=" + tk.Ticket
			ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {w.base}}})
			if err == nil {
				b, _ := json.Marshal(map[string]any{"op": "sub", "channel": "issue:" + w.key})
				ws.Write(ctx, websocket.MessageText, b)
				if !down.IsZero() {
					if d := int64(time.Since(down)); d > w.maxOutage.Load() {
						w.maxOutage.Store(d)
					}
					w.reconnects.Add(1)
					down = time.Time{}
				}
				w.connected.Store(true)
				for {
					if _, _, err := ws.Read(ctx); err != nil {
						break
					}
				}
				ws.CloseNow()
				w.connected.Store(false)
				if ctx.Err() == nil {
					down = time.Now()
				}
			}
		}
		if down.IsZero() && ctx.Err() == nil {
			down = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(100+rand.Intn(900)) * time.Millisecond): // jittered
		}
	}
}

func adminClient(t testing.TB, base string) *user {
	u := &user{base: base, c: newClient(base), st: &stats{}}
	if st, b, err := u.do("POST", "/api/v1/login", map[string]string{"email": "admin@example.com", "password": "adminpass1"}, true); err != nil || st != 200 {
		t.Fatalf("admin login on %s: %d %v %s", base, st, err, b)
	}
	return u
}

func (u *user) mustJSON(t testing.TB, method, path string, body any, want int) map[string]any {
	t.Helper()
	st, b, err := u.do(method, path, body, true)
	if err != nil || st != want {
		t.Fatalf("%s %s: %d %v %s", method, path, st, err, b)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func clusterVersions(t testing.TB, a *user) (versions map[string]int, gate string) {
	t.Helper()
	cl := a.mustJSON(t, "GET", "/api/v1/admin/cluster", nil, 200)
	versions = map[string]int{}
	for _, n := range cl["nodes"].([]any) {
		m := n.(map[string]any)
		if m["live"] == true {
			versions[fmt.Sprint(m["version"])]++
		}
	}
	for _, g := range cl["gates"].([]any) {
		m := g.(map[string]any)
		if m["name"] == "pause_barrier" {
			gate = fmt.Sprint(m["state"])
		}
	}
	return
}

func TestUPG02_RollingUpgradeUnderLoad(t *testing.T) {
	const users = 12
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// the old release, one node first (it makes the schema), then the second
	compose(t, "up -d --wait postgres")
	compose(t, "up -d old1")
	old1 := "http://127.0.0.1:" + port(t, "old1", 8080)
	waitReady(t, old1, 90*time.Second)
	compose(t, "up -d old2")
	old2 := "http://127.0.0.1:" + port(t, "old2", 8080)
	waitReady(t, old2, 90*time.Second)
	pool, err := pgxpool.New(ctx, "postgres://tl:tl@127.0.0.1:"+port(t, "postgres", 5432)+"/tasklane?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// the balancer
	l := newLB()
	l.add(old1)
	l.add(old2)
	go l.check(ctx)
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: l}
	ln, err := newListener()
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	front := "http://" + ln.Addr().String()
	eventually(t, "the balancer to see both nodes", 20*time.Second, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.backends[old1].ok && l.backends[old2].ok })

	// the people and their work: an issue each, in a project of their own
	adm := adminClient(t, front)
	adm.mustJSON(t, "POST", "/api/v1/projects", map[string]string{"key": "UPG", "name": "Upgrade crowd"}, 201)
	adm.mustJSON(t, "POST", "/api/v1/projects", map[string]string{"key": "UPP", "name": "Pause probe"}, 201)
	st := &stats{}
	crowd := make([]*user, 0, users)
	for i := 0; i < users; i++ {
		email := fmt.Sprintf("crowd%02d@example.com", i)
		made := adm.mustJSON(t, "POST", "/api/v1/users", map[string]any{"email": email, "name": fmt.Sprintf("Crowd %02d", i), "password": "crowdpass1"}, 201)
		adm.mustJSON(t, "POST", "/api/v1/projects/UPG/members", map[string]string{"user": fmt.Sprint(made["id"]), "role": "member"}, 200)
		is := adm.mustJSON(t, "POST", "/api/v1/issues", map[string]any{"project": "UPG", "type": "Task", "summary": fmt.Sprintf("Work of %s", email), "description": "start"}, 201)
		u := &user{email: email, key: fmt.Sprint(is["key"]), c: newClient(front), base: front, st: st}
		if code, b, err := u.do("POST", "/api/v1/login", map[string]string{"email": email, "password": "crowdpass1"}, true); err != nil || code != 200 {
			t.Fatalf("login %s: %d %v %s", email, code, err, b)
		}
		crowd = append(crowd, u)
	}
	st.total.Store(0)
	st.ok.Store(0)
	st.lat = nil

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, u := range crowd {
		wg.Add(1)
		go func(u *user, seed int64) { defer wg.Done(); u.run(stop, rand.New(rand.NewSource(seed))) }(u, int64(i+1))
	}
	watchers := make([]*watcher, 0, 4)
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	for i := 0; i < 4; i++ {
		w := &watcher{base: front, email: crowd[i].email, key: crowd[i].key}
		watchers = append(watchers, w)
		go w.run(wctx)
	}
	phase := func(name string, d time.Duration) { t.Logf("%s (%s)", name, d); time.Sleep(d) }

	phase("steady state on the old release", 8*time.Second)

	// 1. the schema moves first, with the old nodes still serving (the expand step)
	compose(t, "run --rm migrate-new")
	phase("old nodes on the migrated schema", 6*time.Second)

	// 2. node one is replaced: the new node joins, the old one is told to stop, drains and leaves
	compose(t, "up -d new1")
	new1 := "http://127.0.0.1:" + port(t, "new1", 8080)
	waitReady(t, new1, 90*time.Second)
	l.add(new1)
	eventually(t, "the balancer to see the new node", 10*time.Second, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.backends[new1].ok })
	compose(t, "stop -t 30 old1")
	l.remove(old1)
	phase("one old and one new node", 6*time.Second)

	// the mixed cluster: two versions live, the behaviour that needs the new release is still off, and a pause still works
	a1 := adminClient(t, new1)
	eventually(t, "the cluster to show both releases", 40*time.Second, func() bool { v, _ := clusterVersions(t, a1); return v["0.0.9"] == 1 && v["0.1.0"] == 1 })
	if _, gate := clusterVersions(t, a1); gate == "on" {
		t.Fatal("the pause barrier is on beside a node of the old release that cannot acknowledge it")
	}
	startPause := time.Now()
	lease := a1.mustJSON(t, "POST", "/api/v1/admin/pause", map[string]any{"scope": "project:UPP", "owner": "upgrade test", "drain_wait_ms": 20000}, 200)
	if lease["state"] != "paused" || time.Since(startPause) > 10*time.Second {
		t.Fatalf("a pause in the mixed cluster: %v after %s", lease["state"], time.Since(startPause))
	}
	a1.mustJSON(t, "DELETE", "/api/v1/admin/pause/"+fmt.Sprint(lease["id"]), nil, 200)

	// 3. node two
	compose(t, "up -d new2")
	new2 := "http://127.0.0.1:" + port(t, "new2", 8080)
	waitReady(t, new2, 90*time.Second)
	l.add(new2)
	eventually(t, "the balancer to see the second new node", 10*time.Second, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.backends[new2].ok })
	compose(t, "stop -t 30 old2")
	l.remove(old2)
	phase("the new release alone", 8*time.Second)

	// the behaviour that waited for the whole cluster turns on, by itself
	eventually(t, "the pause barrier to turn on once every node is new", 60*time.Second, func() bool {
		v, gate := clusterVersions(t, a1)
		return len(v) == 1 && v["0.1.0"] == 2 && gate == "on"
	})
	lease = a1.mustJSON(t, "POST", "/api/v1/admin/pause", map[string]any{"scope": "project:UPP", "owner": "upgrade test", "drain_wait_ms": 20000}, 200)
	var acks int
	pool.QueryRow(ctx, `SELECT count(*) FROM pause_ack WHERE lease_id::text=$1`, fmt.Sprint(lease["id"])).Scan(&acks)
	a1.mustJSON(t, "DELETE", "/api/v1/admin/pause/"+fmt.Sprint(lease["id"]), nil, 200)
	if lease["state"] != "paused" || acks < 1 {
		t.Fatalf("a pause across the two new nodes: %v with %d acknowledgements", lease["state"], acks)
	}

	close(stop)
	wg.Wait()

	// ---- what the rolling upgrade must not have cost
	total, failed := st.total.Load(), st.failed.Load()
	rate := 100 * float64(st.ok.Load()+st.conflicts.Load()) / float64(total)
	sort.Slice(st.lat, func(i, j int) bool { return st.lat[i] < st.lat[j] })
	p95 := st.lat[len(st.lat)*95/100]
	t.Logf("%d requests: %d ok, %d conflicts, %d failed after a retry (%.3f%% succeeded), p95 %s", total, st.ok.Load(), st.conflicts.Load(), failed, rate, p95)
	for _, f := range st.failures {
		t.Logf("  failure: %s", f)
	}
	if total < 1500 {
		t.Fatalf("only %d requests were made: the crowd did not run long enough to mean anything", total)
	}
	if rate < 99.5 {
		t.Errorf("reliability %.3f%% is below 99.5%% (%d failed of %d)", rate, failed, total)
	}
	if p95 > time.Second {
		t.Errorf("p95 latency %s is over the 1 s budget", p95)
	}
	// no lost edit: what each person was told was saved is what is saved, and every comment they were told was added is there
	lost := 0
	for _, u := range crowd {
		v := adm.mustJSON(t, "GET", "/api/v1/issues/"+u.key+"/view", nil, 200)
		desc := fmt.Sprint(v["issue"].(map[string]any)["description"])
		if desc != u.acked && !(u.pending != "" && desc == u.pending) {
			lost++
			t.Errorf("%s: the description is %q, the last acknowledged save was %q (pending %q)", u.key, desc, u.acked, u.pending)
		}
		have := map[string]bool{}
		for _, c := range v["comments"].([]any) {
			have[fmt.Sprint(c.(map[string]any)["body"])] = true
		}
		for _, c := range u.comments {
			if !have[c] {
				lost++
				t.Errorf("%s: the comment %q was acknowledged and is missing", u.key, c)
			}
		}
	}
	if lost != 0 {
		t.Fatalf("%d acknowledged changes were lost", lost)
	}
	// the sockets came back, each within a bounded time
	eventually(t, "every socket to be connected", 20*time.Second, func() bool {
		for _, w := range watchers {
			if !w.connected.Load() {
				return false
			}
		}
		return true
	})
	var reconnects int64
	for _, w := range watchers {
		reconnects += w.reconnects.Load()
		if d := time.Duration(w.maxOutage.Load()); d > 15*time.Second {
			t.Errorf("a socket was down for %s", d)
		}
	}
	if reconnects < 4 {
		t.Errorf("only %d socket reconnections across two node replacements: the sockets were not on the nodes that were replaced", reconnects)
	}
	t.Logf("%d socket reconnections, longest outage %s", reconnects, longestOutage(watchers))
}

func longestOutage(ws []*watcher) time.Duration {
	var m int64
	for _, w := range ws {
		if v := w.maxOutage.Load(); v > m {
			m = v
		}
	}
	return time.Duration(m).Round(time.Millisecond)
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
