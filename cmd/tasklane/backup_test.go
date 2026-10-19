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

// The commands, on the real binary: back up a database that has data, verify the backup without a database, restore it into a new
// empty database, and be refused when restoring over data or from a damaged backup.
func TestPAUSE02_TheCommandsBackUpAndRestoreARealDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	suffix := time.Now().UnixNano() % 1e9
	dbA, dbB := fmt.Sprintf("tl_bk_a_%d", suffix), fmt.Sprintf("tl_bk_b_%d", suffix)
	for _, d := range []string{dbA, dbB} {
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+d); err != nil {
			t.Skip("cannot create a database: " + err.Error())
		}
		defer admin.Exec(ctx, `DROP DATABASE `+d+` WITH (FORCE)`)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "tasklane")
	if out, err := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	dsn := func(db string) string { return strings.Replace(testutil.DSN(), "/tasklane", "/"+db, 1) }
	key := strings.Repeat("ab", 32)
	run := func(db string, args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "TASKLANE_DATABASE_URL="+dsn(db), "TASKLANE_BLOB_DIR="+filepath.Join(dir, "blobs-"+db), "TASKLANE_SECRET_KEY="+key, "TASKLANE_ADMIN_PASSWORD=adminpass1")
		var b bytes.Buffer
		c.Stdout, c.Stderr = &b, &b
		err := c.Run()
		return b.String(), err
	}
	if out, err := run(dbA, "migrate"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run(dbA, "create-admin", "boss@example.com", "The", "Boss"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	bk := filepath.Join(dir, "backup")
	out, err := run(dbA, "backup", bk)
	if err != nil || !strings.Contains(out, "backup written to") || !strings.Contains(out, "keep it as carefully as the database") {
		t.Fatalf("backup: %v\n%s", err, out)
	}
	if out, err := run(dbA, "backup", bk); err == nil || !strings.Contains(out, "already holds a backup") {
		t.Fatalf("a second backup into the same place: %v\n%s", err, out)
	}
	// verified with no database at all
	if out, err := run("nonexistent_db", "verify-backup", bk); err != nil || !strings.HasPrefix(out, "ok: format tasklane-backup version 1") {
		t.Fatalf("verify-backup: %v\n%s", err, out)
	}
	out, err = run(dbB, "restore", bk)
	if err != nil || !strings.Contains(out, "audit chain verifies: true") || !strings.Contains(out, "people must sign in again") {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	pool, _ := pgxpool.New(ctx, dsn(dbB))
	defer pool.Close()
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE email='boss@example.com' AND is_admin`).Scan(&n)
	if n != 1 {
		t.Fatalf("the administrator is not in the restored database (%d)", n)
	}
	// not over data
	if out, err := run(dbB, "restore", bk); err == nil || !strings.Contains(out, "not empty") {
		t.Fatalf("a restore over data: %v\n%s", err, out)
	}
	// over a running instance, on purpose: the current state is written next to the backup first, and the data becomes the backup's
	pool.Exec(ctx, `INSERT INTO app_user(email,name) VALUES('later@example.com','Later')`)
	out, err = run(dbB, "restore", "--replace", bk)
	if err != nil || !strings.Contains(out, "writing a backup of the current state to") || !strings.Contains(out, "restart the servers") {
		t.Fatalf("restore --replace: %v\n%s", err, out)
	}
	pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE email='later@example.com'`).Scan(&n)
	if n != 0 {
		t.Fatal("a person made after the backup survived --replace")
	}
	before, _ := filepath.Glob(bk + ".before-restore-*")
	if len(before) != 1 {
		t.Fatalf("the backup of the replaced state: %v", before)
	}
	if out, err := run("nonexistent_db", "verify-backup", before[0]); err != nil || !strings.HasPrefix(out, "ok:") {
		t.Fatalf("the backup of the replaced state does not verify: %v\n%s", err, out)
	}
	if out, err := run(dbB, "restore", "--no-safety-backup", bk); err == nil || !strings.Contains(out, "usage") {
		t.Fatalf("--no-safety-backup without --replace: %v\n%s", err, out)
	}
	// a damaged backup is refused before a database is touched, with a nonzero exit
	f, _ := os.OpenFile(filepath.Join(bk, "data", "app_user.copy"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("x\n")
	f.Close()
	if out, err := run("nonexistent_db", "verify-backup", bk); err == nil || !strings.Contains(out, "does not match its checksum") {
		t.Fatalf("a damaged backup: %v\n%s", err, out)
	}
	if out, err := run(dbB, "restore", bk); err == nil || !strings.Contains(out, "does not match its checksum") {
		t.Fatalf("restoring a damaged backup: %v\n%s", err, out)
	}
}
