//go:build chaos

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// TestCHAOS08_ClockSkewBetweenRealNodes: two real nodes whose own clocks are an hour apart in opposite directions (fault_clock_skew +1h and -1h) serve the
// same people. Each measures its clock against the database and corrects for it, so every stored time and every expiry must mean the same on both:
// sessions made on one node work on the other, a pause taken on one holds the other and ends on time for both, nothing is stamped an hour off, and the
// reconciler is clean after a campaign through both.
func TestCHAOS08_ClockSkewBetweenRealNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	os.Setenv("UPGRADE_SKEW1", "1h")
	os.Setenv("UPGRADE_SKEW2", "-1h")
	defer os.Unsetenv("UPGRADE_SKEW1")
	defer os.Unsetenv("UPGRADE_SKEW2")
	compose(t, "up -d --wait postgres redis")
	compose(t, "up -d new1")
	new1 := "http://127.0.0.1:" + port(t, "new1", 8080)
	waitReady(t, new1, 90*time.Second)
	compose(t, "up -d new2")
	new2 := "http://127.0.0.1:" + port(t, "new2", 8080)
	waitReady(t, new2, 90*time.Second)
	pool, err := pgxpool.New(ctx, "postgres://tl:tl@127.0.0.1:"+port(t, "postgres", 5432)+"/tasklane?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	a1, a2 := adminClient(t, new1), adminClient(t, new2)

	// 1. the nodes know their clocks are wrong, say so, and have corrected for it
	var cl map[string]any
	eventually(t, "both nodes to report their clock offsets", 60*time.Second, func() bool {
		cl = a1.mustJSON(t, "GET", "/api/v1/admin/cluster", nil, 200)
		n := 0
		for _, x := range cl["nodes"].([]any) {
			if m := x.(map[string]any); m["clock_warning"] != nil && m["clock_warning"] != "" {
				n++
			}
		}
		return n == 2
	})
	offs := []float64{}
	for _, x := range cl["nodes"].([]any) {
		offs = append(offs, x.(map[string]any)["clock_offset_ms"].(float64))
	}
	if len(offs) != 2 || math.Abs(math.Abs(offs[0])-3.6e6) > 60e3 || math.Abs(math.Abs(offs[1])-3.6e6) > 60e3 || offs[0]*offs[1] >= 0 {
		t.Fatalf("clock offsets reported: %v (want about +1h and -1h)", offs)
	}
	t.Logf("offsets the nodes report: %.0f s and %.0f s", offs[0]/1000, offs[1]/1000)

	// 2. a person, a project, a token and a session made through one node work on the other
	adminMade := a1.mustJSON(t, "POST", "/api/v1/projects", map[string]string{"key": "SKW", "name": "Skew"}, 201)
	_ = adminMade
	a1.mustJSON(t, "POST", "/api/v1/projects", map[string]string{"key": "SKP", "name": "Skew pause"}, 201)
	const people = 6
	var writers []*appender
	keys := []string{}
	for i := 0; i < 2; i++ {
		is := a1.mustJSON(t, "POST", "/api/v1/issues", map[string]any{"project": "SKW", "type": "Task", "summary": fmt.Sprintf("Shared %d", i), "description": "start"}, 201)
		keys = append(keys, fmt.Sprint(is["key"]))
	}
	for i := 0; i < people; i++ {
		email := fmt.Sprintf("skew%02d@example.com", i)
		made := a1.mustJSON(t, "POST", "/api/v1/users", map[string]any{"email": email, "name": fmt.Sprintf("Skew %02d", i), "password": "skewpass1"}, 201)
		a1.mustJSON(t, "POST", "/api/v1/projects/SKW/members", map[string]string{"user": fmt.Sprint(made["id"]), "role": "member"}, 200)
		node := []string{new1, new2}[i%2] // signs in on one node and then works through both
		other := []string{new2, new1}[i%2]
		first := &user{email: email, c: newClient(node), base: node, st: &stats{}}
		if st, b, err := first.do("POST", "/api/v1/login", map[string]string{"email": email, "password": "skewpass1"}, true); err != nil || st != 200 {
			t.Fatalf("login on %s: %d %v %s", node, st, err, b)
		}
		second := &user{email: email, c: first.c, base: other, st: &stats{}} // the same cookies on the other node
		if st, b, _ := second.do("GET", "/api/v1/me", nil, true); st != 200 {
			t.Fatalf("a session made on %s was refused by %s: %d %s", node, other, st, b)
		}
		tok := first.mustTokenJSON(t, "/api/v1/me/tokens", map[string]any{"name": "skew", "expires_in_days": 1})
		for _, base := range []string{new1, new2} {
			req, _ := http.NewRequest("GET", base+"/api/v1/me", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
				t.Fatalf("a token made on %s was refused by %s: %v", node, base, err)
			}
		}
		writers = append(writers, &appender{u: &user{email: email, c: first.c, base: node, st: &stats{}}, key: keys[i%2], n: i})
	}

	// 3. a pause taken on one node holds the other and ends at the same moment for both
	lease := a1.mustJSON(t, "POST", "/api/v1/admin/pause", map[string]any{"scope": "project:SKP", "owner": "skew", "drain_wait_ms": 5000, "hold_ms": 12000}, 200)
	if lease["state"] != "paused" {
		t.Fatalf("%v", lease)
	}
	started := time.Now()
	for _, base := range []string{new1, new2} {
		st, _, _ := a1.do("POST", "/api/v1/issues", map[string]any{"project": "SKP", "type": "Task", "summary": "during"}, false)
		_ = st
		c := adminClient(t, base)
		if code, _, _ := c.do("POST", "/api/v1/issues", map[string]any{"project": "SKP", "type": "Task", "summary": "during the pause"}, false); code != 503 {
			t.Fatalf("a write on %s during the pause: %d", base, code)
		}
	}
	// the writes resume on both nodes when the pause ends: about 17 s after it began (5 s of drain wait is part of its deadline), not an hour early or late
	for _, base := range []string{new1, new2} {
		c := adminClient(t, base)
		eventually(t, "writes to resume on "+base, 40*time.Second, func() bool {
			code, _, _ := c.do("POST", "/api/v1/issues", map[string]any{"project": "SKP", "type": "Task", "summary": "after the pause"}, false)
			return code == 201
		})
	}
	if took := time.Since(started); took < 5*time.Second || took > 35*time.Second {
		t.Fatalf("the pause ended after %s: its deadline was read through a skewed clock", took)
	}

	for i, c := range []*user{a1, a2, a1, a2} { // comments written through both nodes
		c.mustJSON(t, "POST", "/api/v1/issues/"+keys[0]+"/comments", map[string]any{"body": fmt.Sprintf("comment %d", i)}, 201)
	}
	// 4. a campaign through both nodes with the nodes' clocks an hour apart
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func(w *appender, seed int64) { defer wg.Done(); w.run(stop, rand.New(rand.NewSource(seed))) }(w, int64(500+i))
	}
	time.Sleep(25 * time.Second)
	close(stop)
	wg.Wait()
	for i, key := range keys {
		var tokens []string
		for _, w := range writers {
			if w.key == key {
				tokens = append(tokens, w.acked...)
			}
		}
		var vs []domain.TextVersion
		_, b, _ := a2.do("GET", "/api/v1/issues/"+key+"/text/description/versions", nil, true)
		json.Unmarshal(b, &vs)
		e1, e2, e4, gap := testutil.Reconcile(tokens, vs)
		t.Logf("text %d: %d acknowledged appends: E1=%d E2=%d E4=%d gap=%v", i, len(tokens), e1, e2, e4, gap)
		if len(tokens) < 20 || e1+e2+e4 != 0 || gap {
			t.Errorf("text %d: %d appends, E1=%d E2=%d E4=%d gap=%v", i, len(tokens), e1, e2, e4, gap)
		}
	}

	// 5. nothing is stamped an hour off: every time the nodes wrote is within minutes of the database's own clock
	for _, q := range []struct{ what, sql string }{
		{"history", `SELECT count(*), COALESCE(max(abs(extract(epoch from (at - now())))),0) FROM issue_history`},
		{"text versions", `SELECT count(*), COALESCE(max(abs(extract(epoch from (at - now())))),0) FROM text_version`},
		{"comments", `SELECT count(*), COALESCE(max(abs(extract(epoch from (created_at - now())))),0) FROM comment`},
		{"audit log", `SELECT count(*), COALESCE(max(abs(extract(epoch from (at - now())))),0) FROM audit_event`},
		{"cluster heartbeats", `SELECT count(*), COALESCE(max(abs(extract(epoch from (seen_at - now())))),0) FROM cluster_node`},
		{"sessions made", `SELECT count(*), COALESCE(max(abs(extract(epoch from (created_at - now())))),0) FROM session`},
	} {
		var n int
		var off float64
		if err := pool.QueryRow(ctx, q.sql).Scan(&n, &off); err != nil {
			t.Fatalf("%s: %v", q.what, err)
		}
		t.Logf("%-18s %5d rows, the farthest %.0f s from the database's clock", q.what, n, off)
		if n == 0 {
			t.Errorf("%s: nothing was written", q.what)
		}
		if off > 600 {
			t.Errorf("%s: a row is stamped %.0f s away from the database's clock (a skewed node's clock leaked into a stored time)", q.what, off)
		}
	}
	var expiresIn float64
	pool.QueryRow(ctx, `SELECT min(extract(epoch from (expires_at - now()))) FROM session`).Scan(&expiresIn)
	if expiresIn < 13*24*3600 { // the default session lasts 14 days
		t.Errorf("a session expires in %.0f hours, far from the 14 days it was given", expiresIn/3600)
	}
}
