//go:build chaos

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// chaosNet is the bad network between people and the cluster (CHAOS-03): every request and every answer is delayed, some requests are lost before they
// reach a node, some answers are lost after the node has committed the work, and some requests arrive twice. WebSockets pass through untouched.
type chaosNet struct {
	next                          http.Handler
	delay                         time.Duration
	pDropReq, pDropResp, pDup     float64
	mu                            sync.Mutex
	rng                           *rand.Rand
	droppedReq, droppedResp, dups atomic.Int64
}

func (c *chaosNet) roll(p float64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rng.Float64() < p
}

func (c *chaosNet) wait() {
	c.mu.Lock()
	d := c.delay/2 + time.Duration(c.rng.Int63n(int64(c.delay)))/2 // 0.5 to 1.0 of the delay, so answers overtake each other
	c.mu.Unlock()
	time.Sleep(d)
}

func kill(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

func (c *chaosNet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		c.next.ServeHTTP(w, r)
		return
	}
	c.wait()
	if c.roll(c.pDropReq) {
		c.droppedReq.Add(1)
		kill(w)
		return
	}
	body, _ := readAll(r)
	send := func() *httptest.ResponseRecorder {
		rr := r.Clone(r.Context())
		rr.Body = newBody(body)
		rec := httptest.NewRecorder()
		c.next.ServeHTTP(rec, rr)
		return rec
	}
	if r.Method != "GET" && c.roll(c.pDup) { // the same request twice
		c.dups.Add(1)
		send()
	}
	rec := send()
	c.wait()
	if c.roll(c.pDropResp) {
		c.droppedResp.Add(1)
		kill(w)
		return
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}

// TestCHAOS03_NetworkFaultsAndNodeFailures: the LIVE-09 campaign through a network with 300 ms of latency, lost requests, lost answers and
// duplicated requests, while a node is killed, Redis stops, and PostgreSQL restarts. The reconciler judges every shared text at the end.
func TestCHAOS03_NetworkFaultsAndNodeFailures(t *testing.T) { campaign(t, false) }

// TestCHAOS05_PacketLevelFaults: the same campaign with the faults made by toxiproxy between the people and each node: TCP connections reset as they open,
// answers cut off after a few bytes, every packet delayed with jitter, and the stream sliced into small pieces.
func TestCHAOS05_PacketLevelFaults(t *testing.T) { campaign(t, true) }

func campaign(t *testing.T, tcp bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	compose(t, "up -d --wait postgres redis")
	compose(t, "up -d new1")
	new1 := "http://127.0.0.1:" + port(t, "new1", 8080)
	waitReady(t, new1, 90*time.Second)
	compose(t, "up -d new2")
	new2 := "http://127.0.0.1:" + port(t, "new2", 8080)
	waitReady(t, new2, 90*time.Second)
	dbURL := func() string {
		return "postgres://tl:tl@127.0.0.1:" + port(t, "postgres", 5432) + "/tasklane?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dbURL())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { pool.Close() }()

	l := newLB()
	var workURL1, workURL2 string
	noKeepAlive = tcp
	defer func() { noKeepAlive = false }()
	if tcp {
		compose(t, "up -d toxiproxy")
		api := "http://127.0.0.1:" + port(t, "toxiproxy", 8474)
		eventually(t, "toxiproxy", 30*time.Second, func() bool {
			r, err := http.Get(api + "/version")
			if err == nil {
				r.Body.Close()
			}
			return err == nil
		})
		post := func(path string, body map[string]any) {
			b, _ := json.Marshal(body)
			r, err := http.Post(api+path, "application/json", strings.NewReader(string(b)))
			if err != nil || r.StatusCode/100 != 2 {
				t.Fatalf("toxiproxy %s: %v %v", path, err, r)
			}
			r.Body.Close()
		}
		for i, up := range []string{"new1:8080", "new2:8080"} {
			name := fmt.Sprintf("n%d", i+1)
			post("/proxies", map[string]any{"name": name, "listen": fmt.Sprintf("0.0.0.0:808%d", i+1), "upstream": up})
			post("/proxies/"+name+"/toxics", map[string]any{"name": "delay-down", "type": "latency", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"latency": 120, "jitter": 100}})
			post("/proxies/"+name+"/toxics", map[string]any{"name": "delay-up", "type": "latency", "stream": "upstream", "toxicity": 1.0, "attributes": map[string]any{"latency": 120, "jitter": 100}})
			post("/proxies/"+name+"/toxics", map[string]any{"name": "slice", "type": "slicer", "stream": "downstream", "toxicity": 1.0, "attributes": map[string]any{"average_size": 64, "size_variation": 32, "delay": 200}})
			post("/proxies/"+name+"/toxics", map[string]any{"name": "reset", "type": "reset_peer", "stream": "upstream", "toxicity": 0.05, "attributes": map[string]any{"timeout": 0}})
			post("/proxies/"+name+"/toxics", map[string]any{"name": "cut", "type": "limit_data", "stream": "downstream", "toxicity": 0.05, "attributes": map[string]any{"bytes": 40}})
		}
		workURL1 = "http://127.0.0.1:" + port(t, "toxiproxy", 8081)
		workURL2 = "http://127.0.0.1:" + port(t, "toxiproxy", 8082)
		l.addVia(workURL1, new1)
		l.addVia(workURL2, new2)
	} else {
		l.add(new1)
		l.add(new2)
	}
	go l.check(ctx)
	lnr, err := newListener()
	if err != nil {
		t.Fatal(err)
	}
	net_ := &chaosNet{next: l, delay: 300 * time.Millisecond, pDropReq: 0.05, pDropResp: 0.05, pDup: 0.03, rng: rand.New(rand.NewSource(11))}
	if tcp { // the faults are in the proxies
		net_ = &chaosNet{next: l, delay: time.Millisecond, rng: rand.New(rand.NewSource(11))}
	}
	srv := &http.Server{Handler: net_}
	go srv.Serve(lnr)
	defer srv.Close()
	front := "http://" + lnr.Addr().String()
	direct := adminClient(t, new1) // setup is made directly: the chaos is for the people
	b1, b2 := new1, new2
	if tcp {
		b1, b2 = workURL1, workURL2
	}
	eventually(t, "the balancer to see both nodes", 20*time.Second, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.backends[b1].ok && l.backends[b2].ok })
	if tcp { // the faults are real: ordinary requests through the proxies fail some of the time
		failed := 0
		for i := 0; i < 200; i++ {
			c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
			if r, err := c.Get(workURL1 + "/api/v1/system/status"); err != nil {
				failed++
			} else {
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
			}
		}
		t.Logf("%d of 200 plain requests through the proxy failed", failed)
		if failed == 0 {
			t.Fatal("toxiproxy injected no failures")
		}
	}

	direct.mustJSON(t, "POST", "/api/v1/projects", map[string]string{"key": "CHS", "name": "Chaos"}, 201)
	const people, texts = 12, 3
	keys := make([]string, texts)
	for i := range keys {
		is := direct.mustJSON(t, "POST", "/api/v1/issues", map[string]any{"project": "CHS", "type": "Task", "summary": fmt.Sprintf("Shared text %d", i), "description": "start"}, 201)
		keys[i] = fmt.Sprint(is["key"])
	}
	var writers []*appender
	for i := 0; i < people; i++ {
		email := fmt.Sprintf("chaos%02d@example.com", i)
		made := direct.mustJSON(t, "POST", "/api/v1/users", map[string]any{"email": email, "name": fmt.Sprintf("Chaos %02d", i), "password": "chaospass1"}, 201)
		direct.mustJSON(t, "POST", "/api/v1/projects/CHS/members", map[string]string{"user": fmt.Sprint(made["id"]), "role": "member"}, 200)
		u := &user{email: email, c: newClient(front), base: front, st: &stats{}}
		if st, b, err := (&user{email: email, c: newClient(new1), base: new1, st: &stats{}}).do("POST", "/api/v1/login", map[string]string{"email": email, "password": "chaospass1"}, true); err != nil || st != 200 {
			t.Fatalf("login: %d %v %s", st, err, b)
		}
		// the session is in the database: log in through the bad network too, as a person does
		for try := 0; try < 20; try++ {
			if st, _, err := u.do("POST", "/api/v1/login", map[string]string{"email": email, "password": "chaospass1"}, true); err == nil && st == 200 {
				break
			}
		}
		writers = append(writers, &appender{u: u, key: keys[i%texts], n: i})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func(w *appender, seed int64) { defer wg.Done(); w.run(stop, rand.New(rand.NewSource(seed))) }(w, int64(300+i))
	}
	phase := func(name string, d time.Duration) { t.Logf("%s (%s)", name, d); time.Sleep(d) }

	phase("a bad network", 15*time.Second)
	// a node dies with saves in flight, and comes back
	compose(t, "kill new1")
	if !tcp {
		l.remove(new1)
	}
	phase("one node down", 10*time.Second)
	compose(t, "up -d new1")
	new1 = "http://127.0.0.1:" + port(t, "new1", 8080)
	waitReady(t, new1, 90*time.Second)
	if tcp {
		l.mu.Lock()
		l.backends[workURL1].health = new1 // the work still goes by the proxy, whose upstream is the service name
		l.mu.Unlock()
	} else {
		l.add(new1)
	}
	phase("both nodes again", 8*time.Second)
	compose(t, "stop redis")
	phase("Redis down", 6*time.Second)
	compose(t, "start redis")
	phase("Redis back", 6*time.Second)
	restartAt := time.Now()
	compose(t, "restart postgres")
	pool.Close()
	pool, _ = pgxpool.New(ctx, dbURL())
	for _, n := range []string{new1, new2} {
		waitReady(t, n, 120*time.Second)
	}
	t.Logf("PostgreSQL was away for %s; the balancer had nowhere to send work for %s in all", time.Since(restartAt).Round(time.Second), l.noneTotal.Round(time.Second))
	phase("after the database restart", 12*time.Second)

	close(stop)
	wg.Wait()
	t.Logf("the network: %d requests lost, %d answers lost after the work was done, %d requests delivered twice", net_.droppedReq.Load(), net_.droppedResp.Load(), net_.dups.Load())

	for _, w := range writers {
		if w.gaveUp > 0 {
			t.Errorf("writer %d: %d appends were never acknowledged (last answer: %s)", w.n, w.gaveUp, w.last)
		}
	}
	total := 0
	for i, key := range keys {
		var tokens []string
		for _, w := range writers {
			if w.key == key {
				tokens = append(tokens, w.acked...)
			}
		}
		var vs []domain.TextVersion
		eventually(t, "the versions of "+key, 60*time.Second, func() bool {
			dc := adminClient(t, new2)
			st, b, err := dc.do("GET", "/api/v1/issues/"+key+"/text/description/versions", nil, true)
			vs = nil
			return err == nil && st == 200 && json.Unmarshal(b, &vs) == nil && len(vs) > 0
		})
		e1, e2, e4, gap := testutil.Reconcile(tokens, vs)
		total += len(tokens)
		t.Logf("text %d (%s): %d acknowledged appends: E1=%d E2=%d E4=%d gap=%v", i, key, len(tokens), e1, e2, e4, gap)
		if e1+e2+e4 != 0 || gap {
			t.Errorf("%s: E1=%d (lost) E2=%d (stale replaced newer) E4=%d (duplicated) gap=%v", key, e1, e2, e4, gap)
		}
	}
	if total < 200 {
		t.Errorf("only %d appends were acknowledged in the whole run", total)
	}
	if !tcp && (net_.droppedResp.Load() == 0 || net_.droppedReq.Load() == 0 || net_.dups.Load() == 0) {
		t.Errorf("the network did not inject every fault it was asked to")
	}
}
