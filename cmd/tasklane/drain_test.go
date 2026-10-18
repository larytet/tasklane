package main

import (
	"bufio"
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
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/testutil"
)

// CHAOS-07: stopping the real server gracefully. Readiness fails first (a load balancer moves new work elsewhere) while the process
// keeps serving; open pages are told to reconnect; a request that is in flight when the signal arrives still gets its answer; then the
// process exits cleanly.
func TestCHAOS07_TheRealServerDrainsOnSIGTERM(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_drain_%d", time.Now().UnixNano()%1e9)
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
	dsn := strings.Replace(testutil.DSN(), "/tasklane", "/"+db, 1)
	c := exec.Command(bin, "serve")
	c.Env = append(os.Environ(), "TASKLANE_DATABASE_URL="+dsn, "TASKLANE_LISTEN="+addr, "TASKLANE_BLOB_DIR="+filepath.Join(dir, "blobs"),
		"TASKLANE_ADMIN_EMAIL=admin@example.com", "TASKLANE_ADMIN_PASSWORD=adminpass1", "TASKLANE_SHUTDOWN_DRAIN_DELAY=2s")
	var logs bytes.Buffer
	c.Stdout, c.Stderr = &logs, &logs
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- c.Wait() }()
	defer c.Process.Kill()
	base := "http://" + addr
	status := func(path string) int {
		r, err := http.Get(base + path)
		if err != nil {
			return -1
		}
		r.Body.Close()
		return r.StatusCode
	}
	for i := 0; ; i++ {
		if status("/readyz") == 200 {
			break
		}
		if i > 100 {
			t.Fatalf("the server did not start:\n%s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	// an open page's socket
	lb, _ := json.Marshal(map[string]string{"email": "admin@example.com", "password": "adminpass1"})
	req, _ := http.NewRequest("POST", base+"/api/v1/login", bytes.NewReader(lb))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "tasklane")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: %v\n%s", err, logs.String())
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, ck := range resp.Cookies() {
		cookie = ck
	}
	treq, _ := http.NewRequest("POST", base+"/api/v1/ws/ticket", nil)
	treq.Header.Set("X-Requested-With", "tasklane")
	treq.AddCookie(cookie)
	tresp, err := http.DefaultClient.Do(treq)
	if err != nil {
		t.Fatal(err)
	}
	var tk struct{ Ticket string }
	json.NewDecoder(tresp.Body).Decode(&tk)
	tresp.Body.Close()
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/api/v1/ws?ticket="+tk.Ticket, nil)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer ws.CloseNow()
	hint := make(chan map[string]any, 4)
	go func() {
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				close(hint)
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			hint <- m
		}
	}()
	// a request that is in flight when the signal arrives: its headers and half of its body are sent, the rest comes later
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"email":"nobody@example.com","password":"not the password"}`
	fmt.Fprintf(conn, "POST /api/v1/login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nX-Requested-With: tasklane\r\nContent-Length: %d\r\n\r\n%s", addr, len(body), body[:20])
	time.Sleep(200 * time.Millisecond)

	signalled := time.Now()
	c.Process.Signal(syscall.SIGTERM)
	// readiness fails promptly while the process keeps serving: that is the point of the delay
	failed := false
	for time.Since(signalled) < time.Second {
		if status("/readyz") == 503 {
			failed = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !failed {
		t.Fatalf("readiness did not fail within a second of the signal:\n%s", logs.String())
	}
	if got := status("/healthz"); got != 200 {
		t.Fatalf("liveness during the drain: %d (the process is alive and must say so)", got)
	}
	select {
	case m, ok := <-hint:
		if !ok || m["type"] != "reconnect" {
			t.Fatalf("the open page was told %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the open page was not told to reconnect")
	}
	// the drain delay passes and the listener closes, but the request in flight is still answered
	time.Sleep(2500 * time.Millisecond)
	fmt.Fprint(conn, body[20:])
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && err != io.EOF || !strings.HasPrefix(line, "HTTP/1.1 4") { // a wrong password: 401, answered, not reset
		t.Fatalf("the request in flight got %q, %v", line, err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the server exited with %v:\n%s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the server did not exit:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "stopping: readiness now fails") || !strings.Contains(logs.String(), "stopped") {
		t.Fatalf("the log does not tell the story:\n%s", logs.String())
	}
	if status("/healthz") != -1 {
		t.Fatal("the server is still answering after it exited")
	}
}
