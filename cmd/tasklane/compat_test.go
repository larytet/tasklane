package main

import (
	"bytes"
	"context"
	"fmt"
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

// UPG-05, UPG-06: the real binary refuses to start on a schema outside its range, and says why; doctor says the same.
func TestUPG06_TheServerRefusesToStartOutsideItsSchemaRange(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_compat_%d", time.Now().UnixNano()%1e9)
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
	env := []string{"TASKLANE_DATABASE_URL=" + dsn, "TASKLANE_LISTEN=127.0.0.1:0", "TASKLANE_BLOB_DIR=" + filepath.Join(dir, "blobs")}
	run := func(args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), env...)
		var b bytes.Buffer
		c.Stdout, c.Stderr = &b, &b
		done := make(chan error, 1)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { done <- c.Wait() }()
		select {
		case err := <-done:
			return b.String(), err
		case <-time.After(20 * time.Second):
			c.Process.Kill()
			return b.String() + "\n(still running after 20 s)", nil
		}
	}
	if out, err := run("migrate"); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	var latest int
	pool.QueryRow(ctx, `SELECT max(version) FROM schema_migration`).Scan(&latest)
	// 1. a database behind the binary: the newest migration looks not yet applied (its row is parked under another number)
	pool.Exec(ctx, `UPDATE schema_migration SET version = version - 1000 WHERE version = $1`, latest)
	out, err := run("serve")
	if err == nil || !strings.Contains(out, "refusing to start") || !strings.Contains(out, "behind") || !strings.Contains(out, "tasklane migrate") {
		t.Fatalf("a schema behind the binary: err=%v\n%s", err, out)
	}
	if out, err := run("doctor"); err == nil || !strings.Contains(out, "behind") {
		t.Fatalf("doctor on a schema behind the binary: err=%v\n%s", err, out)
	}
	pool.Exec(ctx, `UPDATE schema_migration SET version = version + 1000 WHERE version = $1`, latest-1000)
	// 2. a database finalized by a newer release: serve refuses and says to restore
	pool.Exec(ctx, `INSERT INTO schema_migration(version,name,checksum,phase) VALUES(9001,'9001_newer_contract','x','contract')`)
	out, err = run("serve")
	if err == nil || !strings.Contains(out, "refusing to start") || !strings.Contains(out, "finalized") || !strings.Contains(out, "restore a backup") {
		t.Fatalf("a finalized newer schema: err=%v\n%s", err, out)
	}
	if out, err := run("doctor"); err == nil || !strings.Contains(out, "finalized") {
		t.Fatalf("doctor on a finalized schema: err=%v\n%s", err, out)
	}
	// 3. a newer schema that only expanded: inside the window the binary runs, with a warning
	pool.Exec(ctx, `UPDATE schema_migration SET phase='expand', name='9001_newer_expand' WHERE version=9001`)
	out, _ = run("serve")
	if !strings.Contains(out, "warning: running beside a newer schema") || strings.Contains(out, "refusing to start") {
		t.Fatalf("inside the window:\n%s", out)
	}
	if out, err := run("doctor"); err != nil || !strings.Contains(out, "expand-only") {
		t.Fatalf("doctor inside the window: err=%v\n%s", err, out)
	}
	// 4. nothing to finalize today, and the command says so
	if out, err := run("migrate", "finalize"); err != nil || !strings.Contains(out, "applied 0 migration") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run("migrate", "plan"); err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("%v\n%s", err, out)
	}
	// 5. UPG-04: a behaviour already on for the cluster needs a newer binary: starting this one is a rollback that is refused
	pool.Exec(ctx, `INSERT INTO feature_gate(name,min_version,state,activated_at) VALUES('future_thing','99.0.0','on',now())`)
	out, err = run("serve")
	if err == nil || !strings.Contains(out, "refusing to start") || !strings.Contains(out, "future_thing") || !strings.Contains(out, "99.0.0") {
		t.Fatalf("a rollback past an active gate: err=%v\n%s", err, out)
	}
	pool.Exec(ctx, `DELETE FROM feature_gate WHERE name='future_thing'`)
}

// UPG-01 (nodes started together): a node that starts while another is applying migrations waits for it instead of refusing a schema that is
// only behind because the migration is not finished. Found by the real-service interoperability profile, which starts two nodes at once.
func TestUPG01_ANodeStartedDuringAMigrationWaitsForIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_wait_%d", time.Now().UnixNano()%1e9)
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
	env := append(os.Environ(), "TASKLANE_DATABASE_URL="+dsn, "TASKLANE_LISTEN=127.0.0.1:0", "TASKLANE_BLOB_DIR="+filepath.Join(dir, "blobs"))
	mig := exec.Command(bin, "migrate")
	mig.Env = env
	if out, err := mig.CombinedOutput(); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	var latest int
	pool.QueryRow(ctx, `SELECT max(version) FROM schema_migration`).Scan(&latest)
	// a migration is "in progress": its lock is held by another session and the newest one is not yet recorded
	hold, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	hold.Exec(ctx, `SELECT pg_advisory_lock(727274)`)
	pool.Exec(ctx, `UPDATE schema_migration SET version = version - 1000 WHERE version = $1`, latest)
	c := exec.Command(bin, "serve")
	c.Env = env
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	time.Sleep(2 * time.Second)
	if strings.Contains(out.String(), "refusing to start") {
		t.Fatalf("the node refused to start while a migration was running:\n%s", out.String())
	}
	// the migration finishes
	pool.Exec(ctx, `UPDATE schema_migration SET version = $1 WHERE version = $2`, latest, latest-1000)
	hold.Exec(ctx, `SELECT pg_advisory_unlock(727274)`)
	for i := 0; i < 60; i++ {
		if strings.Contains(out.String(), "listening on") {
			return
		}
		if strings.Contains(out.String(), "refusing to start") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the node did not come up after the migration finished:\n%s", out.String())
}
