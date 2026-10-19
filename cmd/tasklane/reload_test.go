package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/testutil"
)

// CFG-03: the real server re-reads its configuration file on SIGHUP. A safe setting (the shortest password) applies without a restart, a
// setting that needs a restart is reported as such, and an invalid file leaves the running settings alone.
func TestCFG03_TheRealServerReloadsTheSafeSubsetOnSIGHUP(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_hup_%d", time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+db); err != nil {
		t.Skip("cannot create a database: " + err.Error())
	}
	defer admin.Exec(ctx, `DROP DATABASE `+db+` WITH (FORCE)`)
	dir := t.TempDir()
	bin := filepath.Join(dir, "tasklane")
	if out, err := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	conf := filepath.Join(dir, "tasklane.conf")
	write := func(s string) {
		if err := os.WriteFile(conf, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("password_min_length = 8\n")
	dsn := strings.Replace(testutil.DSN(), "/tasklane", "/"+db, 1)
	c := exec.Command(bin, "serve")
	c.Env = append(os.Environ(), "TASKLANE_CONFIG="+conf, "TASKLANE_DATABASE_URL="+dsn, "TASKLANE_LISTEN="+addr, "TASKLANE_BLOB_DIR="+filepath.Join(dir, "blobs"),
		"TASKLANE_ADMIN_EMAIL=admin@example.com", "TASKLANE_ADMIN_PASSWORD=adminpass1", "TASKLANE_SHUTDOWN_DRAIN_DELAY=0s")
	var logs syncBuf
	c.Stdout, c.Stderr = &logs, &logs
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	base := "http://" + addr
	for i := 0; ; i++ {
		if r, err := http.Get(base + "/readyz"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				break
			}
		}
		if i > 100 {
			t.Fatalf("the server did not start:\n%s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	lb, _ := json.Marshal(map[string]string{"email": "admin@example.com", "password": "adminpass1"})
	req, _ := http.NewRequest("POST", base+"/api/v1/login", bytes.NewReader(lb))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "tasklane")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: %v\n%s", err, logs.String())
	}
	resp.Body.Close()
	cookies := resp.Cookies()
	n := 0
	newUser := func(pw string) (int, string) {
		n++
		b, _ := json.Marshal(map[string]any{"email": fmt.Sprintf("u%d@example.com", n), "name": "U", "password": pw})
		r, _ := http.NewRequest("POST", base+"/api/v1/users", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Requested-With", "tasklane")
		for _, ck := range cookies {
			r.AddCookie(ck)
		}
		rs, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Body.Close()
		body, _ := io.ReadAll(rs.Body)
		return rs.StatusCode, string(body)
	}
	hup := func(want string) {
		t.Helper()
		before := strings.Count(logs.String(), want)
		c.Process.Signal(syscall.SIGHUP)
		for i := 0; i < 100; i++ {
			if strings.Count(logs.String(), want) > before {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("no %q in the log after SIGHUP:\n%s", want, logs.String())
	}
	if code, body := newUser("tenchars10"); code != 201 {
		t.Fatalf("a ten character password with the minimum at eight: %d %s", code, body)
	}
	// a safe key and a restart-only key change
	write("password_min_length = 20\nmax_upload_mb = 7\n")
	hup("applied: password_min_length; changed but needing a restart: max_upload_mb")
	if code, body := newUser("tenchars10x"); code != 400 || !strings.Contains(body, "at least 20") {
		t.Fatalf("a short password after the reload: %d %s", code, body)
	}
	// an invalid file: refused as a whole, the running minimum stays at twenty
	write("password_min_length = soon\n")
	hup("reload: the new configuration is not valid")
	if code, _ := newUser("short-but-ten"); code != 400 {
		t.Fatalf("after an invalid reload the old settings must stay: %d", code)
	}
	// and the server still answers on its own address
	if r, err := http.Get(base + "/readyz"); err != nil || r.StatusCode != 200 {
		t.Fatalf("readiness after the reloads: %v", err)
	}
}

// syncBuf is a log buffer the test reads while the server's output is still being copied into it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
