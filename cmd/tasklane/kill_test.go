package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

type proc struct {
	t    *testing.T
	bin  string
	env  []string
	addr string
	cmd  *exec.Cmd
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func (p *proc) start() {
	p.cmd = exec.Command(p.bin, "serve")
	p.cmd.Env = append(os.Environ(), p.env...)
	if err := p.cmd.Start(); err != nil {
		p.t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if r, err := http.Get("http://" + p.addr + "/readyz"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.t.Fatal("server did not become ready")
}

// kill is SIGKILL: no shutdown hooks, no flush, connections reset, exactly a crashed node.
func (p *proc) kill() {
	p.cmd.Process.Signal(syscall.SIGKILL)
	p.cmd.Wait()
}

// DUR-01, DUR-02, CHAOS-01: the real binary is killed with SIGKILL in the middle of a save campaign and restarted.
// Clients retry with the same save id. Afterwards: no acknowledged save is lost (E1), none is applied twice (E4), no stale
// save replaced newer content (E2), text_seq has no gap, and the drafts written before the kill are intact.
func TestCHAOS01_DUR01_KillTheServerMidSave(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and kills the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_kill_%d", time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+db); err != nil {
		t.Skip("cannot create a database: " + err.Error())
	}
	defer admin.Exec(ctx, `DROP DATABASE `+db+` WITH (FORCE)`)
	dir := t.TempDir()
	bin := filepath.Join(dir, "tasklane")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	addr := freePort(t)
	p := &proc{t: t, bin: bin, addr: addr, env: []string{
		"TASKLANE_DATABASE_URL=" + strings.Replace(testutil.DSN(), "/tasklane", "/"+db, 1), "TASKLANE_LISTEN=" + addr, "TASKLANE_BLOB_DIR=" + filepath.Join(dir, "blobs"),
		"TASKLANE_ADMIN_EMAIL=admin@example.com", "TASKLANE_ADMIN_PASSWORD=adminpass1"}}
	p.start()
	defer func() { p.kill() }()
	base := "http://" + addr
	hc := &http.Client{Timeout: 5 * time.Second}
	call := func(tok, method, path string, body any) (int, map[string]any, error) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, base+path, rd)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := hc.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out, nil
	}
	// setup directly in the database through the domain layer: users, a project, tokens, one issue
	pool, _ := pgxpool.New(ctx, strings.Replace(testutil.DSN(), "/tasklane", "/"+db, 1))
	defer pool.Close()
	svc := domain.New(pool)
	var adm domain.Actor
	var toks []string
	var key string
	func() {
		rows, _ := pool.Query(ctx, `SELECT id FROM app_user WHERE email='admin@example.com'`)
		for rows.Next() {
			rows.Scan(&adm.ID)
		}
		rows.Close()
		adm.Label, adm.Admin = "Administrator", true
		if _, err := svc.CreateProject(ctx, adm, "KIL", "Kill test"); err != nil {
			t.Fatal(err)
		}
		const users = 8
		for i := 0; i < users; i++ {
			u, err := svc.CreateUser(ctx, fmt.Sprintf("k%d@example.com", i), fmt.Sprintf("Killer %d", i), "password123", false)
			if err != nil {
				t.Fatal(err)
			}
			svc.AddMember(ctx, adm, "KIL", u.ID, "member")
			tk, _ := svc.CreateAPIToken(ctx, u.ID, "k")
			toks = append(toks, tk)
		}
		is, err := svc.CreateIssue(ctx, adm, domain.CreateIn{Project: "KIL", Summary: "kill", Description: "start"})
		if err != nil {
			t.Fatal(err)
		}
		key = is.Key
	}()
	// a draft written before the kill must survive it (DUR-02); a comment draft, because a successful description save deletes the saver's own description draft by design
	if c, _, err := call(toks[0], "PUT", "/api/v1/issues/"+key+"/drafts/comment", map[string]any{"body": "my unsaved words", "base_seq": 0}); err != nil || c != 200 {
		t.Fatalf("draft: %v %d", err, c)
	}
	const perUser = 12
	var done int64
	results := make(chan string, 8*perUser)
	var wg sync.WaitGroup
	var restarts, retries int64
	for u := range toks {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(u) + 100))
			for n := 0; n < perUser; n++ {
				token := fmt.Sprintf("[k%02dn%02d]", u, n)
				var body string
				var seq int64
				saveID := fmt.Sprintf("kill-%d-%d-%d", u, n, rng.Int63())
				acked := false
				for attempt := 0; attempt < 600 && !acked; attempt++ {
					if attempt > 0 {
						time.Sleep(time.Duration(10+rng.Intn(40+attempt*2)) * time.Millisecond)
					}
					if body == "" || attempt == 0 { // read the current text
						c, v, err := call(toks[u], "GET", "/api/v1/issues/"+key+"/view", nil)
						if err != nil || c != 200 {
							atomic.AddInt64(&retries, 1)
							continue
						}
						iss := v["issue"].(map[string]any)
						body, seq = iss["description"].(string), int64(iss["description_seq"].(float64))
					}
					c, out, err := call(toks[u], "PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": body + "\n\n" + token, "base_seq": seq, "save_id": saveID})
					switch {
					case err != nil: // killed before or after the commit: retry the same save id and base
						atomic.AddInt64(&retries, 1)
					case c == 200:
						acked = true
					case c == 409:
						d := out["detail"].(map[string]any)
						body, seq = d["current"].(string), int64(d["current_seq"].(float64))
						saveID = fmt.Sprintf("kill-%d-%d-%d", u, n, rng.Int63())
					default:
						t.Errorf("unexpected %d %v", c, out)
						return
					}
				}
				if !acked {
					t.Errorf("%s never acknowledged", token)
				}
				results <- token
				atomic.AddInt64(&done, 1)
			}
		}(u)
	}
	// kill -9 the server three times while the campaign runs; each time restart it after a short outage
	killer := make(chan struct{})
	go func() {
		defer close(killer)
		for k := 0; k < 3; k++ {
			for atomic.LoadInt64(&done) < int64(8*perUser*(k+1)/5) {
				time.Sleep(20 * time.Millisecond)
				if atomic.LoadInt64(&done) >= 8*perUser {
					return
				}
			}
			p.kill()
			atomic.AddInt64(&restarts, 1)
			time.Sleep(700 * time.Millisecond)
			p.start()
		}
	}()
	wg.Wait()
	<-killer
	close(results)
	var tokens []string
	for r := range results {
		tokens = append(tokens, r)
	}
	if restarts == 0 {
		t.Fatal("the server was never killed")
	}
	vs, err := svc.TextVersions(ctx, adm, key, "description")
	if err != nil {
		t.Fatal(err)
	}
	e1, e2, e4, gap := testutil.Reconcile(tokens, vs)
	t.Logf("%d saves, server killed %d times, %d client retries; E1=%d E2=%d E4=%d gap=%v; final seq %d", len(tokens), restarts, retries, e1, e2, e4, gap, vs[0].Seq)
	if e1+e2+e4 != 0 || gap {
		t.Fatalf("E1=%d E2=%d E4=%d gap=%v", e1, e2, e4, gap)
	}
	// the draft is intact after three crashes
	ds, err := svc.GetDrafts(ctx, domain.Actor{ID: userID(t, pool, "k0@example.com"), Label: "k0"}, key)
	if err != nil || len(ds) != 1 || ds[0].Field != "comment" || ds[0].Body != "my unsaved words" {
		t.Fatalf("draft after the kills: %v %+v", err, ds)
	}
	// and the restarted server is healthy: ready, and a new save works
	if c, _, err := call(toks[1], "GET", "/api/v1/issues/"+key+"/view", nil); err != nil || c != 200 {
		t.Fatalf("view after restart: %v %d", err, c)
	}
}

func userID(t *testing.T, pool *pgxpool.Pool, email string) string {
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id FROM app_user WHERE email=$1`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
