package main

import (
	"context"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/testutil"
)

// MAIL-09: the real binary, configured with smtp_listen, receives mail for its own domain, refuses to relay, and records what it took.
func TestMAIL09_TheServerListensForMailWhenConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_smtpd_%d", time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+db); err != nil {
		t.Skip("cannot create a database: " + err.Error())
	}
	defer admin.Exec(ctx, `DROP DATABASE `+db+` WITH (FORCE)`)
	dir := t.TempDir()
	bin := filepath.Join(dir, "tasklane")
	if out, err := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dsn := strings.Replace(testutil.DSN(), "/tasklane", "/"+db, 1)
	smtpAddr, httpAddr := freePort(t), freePort(t)
	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "TASKLANE_DATABASE_URL="+dsn, "TASKLANE_LISTEN="+httpAddr, "TASKLANE_BLOB_DIR="+filepath.Join(dir, "blobs"),
		"TASKLANE_BASE_URL=https://mail.test.example", "TASKLANE_SMTP_LISTEN="+smtpAddr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var conn net.Conn
	for i := 0; i < 100; i++ {
		if conn, err = net.Dial("tcp", smtpAddr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the SMTP listener did not come up: %v", err)
	}
	conn.Close()
	// it is not a relay
	if err := smtp.SendMail(smtpAddr, nil, "a@example.org", []string{"victim@elsewhere.example"}, []byte("Subject: x\r\n\r\nbody\r\n")); err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("a message for another domain was not refused: %v", err)
	}
	// mail for its own domain is taken and recorded (from nobody it knows, so it is held)
	rcpt := "reply+" + strings.Repeat("A", 39) + "@mail.test.example"
	msg := "From: stranger@example.org\r\nTo: " + rcpt + "\r\nSubject: hello\r\nMessage-ID: <wired@example.org>\r\n\r\nhello\r\n"
	if err := smtp.SendMail(smtpAddr, nil, "stranger@example.org", []string{rcpt}, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	var outcome, reason string
	if err := pool.QueryRow(ctx, `SELECT outcome,reason FROM mail_inbound WHERE message_id='<wired@example.org>'`).Scan(&outcome, &reason); err != nil || outcome != "quarantined" {
		t.Fatalf("%v %q %q", err, outcome, reason)
	}
}
