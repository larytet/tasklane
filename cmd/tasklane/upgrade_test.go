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

// UPG-07: `upgrade preflight` says what an upgrade would do and exits nonzero on a blocker; `migrate finalize` refuses while a live node
// still runs an older release.
func TestUPG07_PreflightAndTheFinalizeGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_pre_%d", time.Now().UnixNano()%1e9)
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
	run := func(args ...string) (string, int) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "TASKLANE_DATABASE_URL="+dsn, "TASKLANE_LISTEN=127.0.0.1:0", "TASKLANE_BLOB_DIR="+filepath.Join(dir, "blobs"))
		var b bytes.Buffer
		c.Stdout, c.Stderr = &b, &b
		err := c.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return b.String(), code
	}
	// a fresh database: migrations to apply, nothing blocks, the procedure starts with a backup
	out, code := run("upgrade", "preflight", "--backup", "/safe/place")
	if code != 0 || !strings.Contains(out, "would apply") || !strings.Contains(out, "tasklane backup /safe/place") || !strings.Contains(out, "no live node") {
		t.Fatalf("fresh database: exit %d\n%s", code, out)
	}
	if out, code := run("migrate"); code != 0 {
		t.Fatalf("migrate: %s", out)
	}
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	out, code = run("upgrade", "preflight")
	if code != 0 || !strings.Contains(out, "0 migration(s) to apply now") {
		t.Fatalf("migrated database: exit %d\n%s", code, out)
	}
	// a live node of an older release: preflight names it, and finalize refuses until it is gone or --force
	pool.Exec(ctx, `INSERT INTO cluster_node(id,version) VALUES('old-1','0.0.1')`)
	out, code = run("upgrade", "preflight")
	if code != 0 || !strings.Contains(out, "node old-1 runs 0.0.1 (older, to replace)") || !strings.Contains(out, "1 to replace") {
		t.Fatalf("older node: exit %d\n%s", code, out)
	}
	out, code = run("migrate", "finalize")
	if code == 0 || !strings.Contains(out, "refusing to finalize") || !strings.Contains(out, "old-1 (0.0.1)") {
		t.Fatalf("finalize beside an older node must refuse: exit %d\n%s", code, out)
	}
	if out, code = run("migrate", "finalize", "--force"); code != 0 {
		t.Fatalf("--force: exit %d\n%s", code, out)
	}
	// a node that is no longer live does not hold finalize back
	pool.Exec(ctx, `UPDATE cluster_node SET seen_at = now() - interval '1 hour'`)
	if out, code = run("migrate", "finalize"); code != 0 {
		t.Fatalf("a node that stopped reporting must not block: exit %d\n%s", code, out)
	}
	// a node of a newer release than this binary blocks the preflight
	pool.Exec(ctx, `UPDATE cluster_node SET seen_at = now(), version='99.0.0'`)
	out, code = run("upgrade", "preflight")
	if code != 1 || !strings.Contains(out, "NEWER than this binary") {
		t.Fatalf("newer node: exit %d\n%s", code, out)
	}
}
