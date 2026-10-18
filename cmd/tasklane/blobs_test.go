package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/testutil"
)

// BLOB-14, BLOB-16: the real binary moves attachments from a plain directory into an encrypted one, verifies them, rotates
// the master key and verifies again, and fsck still passes on the encrypted store.
func TestBLOB14_16_CLIMigrateEncryptRewrapVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_blobs_%d", time.Now().UnixNano()%1e9)
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
	oldDir, newDir := filepath.Join(dir, "old"), filepath.Join(dir, "new")
	k1, k2 := strings.Repeat("11", 32), strings.Repeat("22", 32)
	run := func(env []string, args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), append([]string{"TASKLANE_DATABASE_URL=" + dsn}, env...)...)
		var b bytes.Buffer
		c.Stdout, c.Stderr = &b, &b
		err := c.Run()
		return b.String(), err
	}
	if out, err := run(nil, "migrate"); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	// the old directory holds three attachments the database knows about
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	os.MkdirAll(oldDir, 0o700)
	var shas []string
	for i, body := range []string{"first SECRET-MARKER attachment", "", strings.Repeat("big SECRET-MARKER ", 9000)} {
		h := sha256.Sum256([]byte(body))
		sha := hex.EncodeToString(h[:])
		shas = append(shas, sha)
		p := filepath.Join(oldDir, sha[:2], sha[2:4], sha)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(body), 0o400)
		if _, err := pool.Exec(ctx, `INSERT INTO blob(sha256,size,mime,class,scan_state,refcount) VALUES($1,$2,'text/plain','other','clean',1)`, sha, len(body)); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	env := []string{"TASKLANE_BLOB_DIR=" + newDir, "TASKLANE_BLOB_FALLBACK=fs", "TASKLANE_BLOB_FALLBACK_DIR=" + oldDir,
		"TASKLANE_BLOB_ENCRYPTION_KEYS=1:" + k1, "TASKLANE_BLOB_ENCRYPTION_CURRENT=1"}
	if out, err := run(env, "blobs", "migrate", "--verify"); err != nil || !strings.Contains(out, "3 objects: 3 copied") {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if out, err := run(env, "blobs", "migrate"); err != nil || !strings.Contains(out, "3 already there") {
		t.Fatalf("a second migrate is not a no-op: %v\n%s", err, out)
	}
	for _, sha := range shas {
		raw, err := os.ReadFile(filepath.Join(newDir, sha[:2], sha[2:4], sha))
		if err != nil || bytes.Contains(raw, []byte("SECRET-MARKER")) {
			t.Fatalf("%s is missing or readable at rest in the new store: %v", sha[:8], err)
		}
	}
	// the new store alone (no fallback) verifies and passes fsck
	only := []string{"TASKLANE_BLOB_DIR=" + newDir, "TASKLANE_BLOB_ENCRYPTION_KEYS=1:" + k1, "TASKLANE_BLOB_ENCRYPTION_CURRENT=1"}
	if out, err := run(only, "blobs", "verify"); err != nil || !strings.Contains(out, "3 objects checked, 0 bad") {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	if out, err := run(only, "fsck"); err != nil || !strings.Contains(out, "0 missing, 0 orphaned, 0 corrupt") {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
	// without the key the store cannot be read
	if out, err := run([]string{"TASKLANE_BLOB_DIR=" + newDir}, "blobs", "verify"); err == nil {
		t.Fatalf("verify passed without the keys\n%s", out)
	}
	// rotate: key 2 becomes current, rewrap, then retire key 1
	rot := []string{"TASKLANE_BLOB_DIR=" + newDir, "TASKLANE_BLOB_ENCRYPTION_KEYS=1:" + k1 + ",2:" + k2, "TASKLANE_BLOB_ENCRYPTION_CURRENT=2"}
	if out, err := run(rot, "blobs", "rewrap"); err != nil || !strings.Contains(out, "3 objects: 3 moved to key 2") {
		t.Fatalf("rewrap: %v\n%s", err, out)
	}
	if out, err := run(rot, "blobs", "rewrap"); err != nil || !strings.Contains(out, "0 moved") {
		t.Fatalf("a second rewrap is not a no-op: %v\n%s", err, out)
	}
	retired := []string{"TASKLANE_BLOB_DIR=" + newDir, "TASKLANE_BLOB_ENCRYPTION_KEYS=2:" + k2, "TASKLANE_BLOB_ENCRYPTION_CURRENT=2"}
	if out, err := run(retired, "blobs", "verify"); err != nil || !strings.Contains(out, "0 bad") {
		t.Fatalf("after retiring key 1: %v\n%s", err, out)
	}
	if out, err := run(retired, "fsck"); err != nil {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
	// usage errors are loud
	if out, err := run(only, "blobs", "rewrap"); err == nil || !strings.Contains(out, "master key 2, which is not configured") {
		t.Fatalf("rewrapping without the key the objects use must fail and say why: %v\n%s", err, out)
	}
	if _, err := run([]string{"TASKLANE_BLOB_DIR=" + newDir}, "blobs", "rewrap"); err == nil {
		t.Fatal("rewrap without keys succeeded")
	}
	if _, err := run([]string{"TASKLANE_BLOB_DIR=" + newDir}, "blobs", "migrate"); err == nil {
		t.Fatal("migrate without a fallback succeeded")
	}
}

// BLOB-15: the real binary, configured with a cold tier, moves objects by rule with `blobs tier`, and `fsck` and `blobs verify`
// see both tiers.
func TestBLOB15_CLITierMovesObjectsByRuleAndVerifySeesBothTiers(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	db := fmt.Sprintf("tl_tier_%d", time.Now().UnixNano()%1e9)
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
	hotDir, coldDir := filepath.Join(dir, "hot"), filepath.Join(dir, "cold")
	run := func(env []string, args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), append([]string{"TASKLANE_DATABASE_URL=" + dsn}, env...)...)
		var b bytes.Buffer
		c.Stdout, c.Stderr = &b, &b
		err := c.Run()
		return b.String(), err
	}
	if out, err := run(nil, "migrate"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	pool, _ := pgxpool.New(ctx, dsn)
	defer pool.Close()
	os.MkdirAll(hotDir, 0o700)
	hot, _ := blob.NewFS(hotDir)
	var shas []string
	for i, body := range []string{"old log " + strings.Repeat("x", 100), "young log " + strings.Repeat("y", 100)} {
		h := sha256.Sum256([]byte(body))
		sha := hex.EncodeToString(h[:])
		id, f, _ := hot.NewStaging()
		f.WriteString(body)
		f.Close()
		hot.Commit(id, sha)
		age := []int{90, 1}[i]
		if _, err := pool.Exec(ctx, `INSERT INTO blob(sha256,size,mime,class,scan_state,refcount,created_at) VALUES($1,$2,'text/plain','log','clean',1,now() - make_interval(days => $3))`, sha, len(body), age); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	pool.Exec(ctx, `INSERT INTO tier_rule(name,class,min_age_days) VALUES('old logs','log',30)`)
	env := []string{"TASKLANE_BLOB_DIR=" + hotDir, "TASKLANE_BLOB_COLD_BACKEND=fs", "TASKLANE_BLOB_COLD_DIR=" + coldDir}
	if out, err := run(env, "blobs", "tier"); err != nil || !strings.Contains(out, "1 objects moved to the cold tier") {
		t.Fatalf("tier: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(coldDir, shas[0][:2], shas[0][2:4], shas[0])); err != nil {
		t.Fatalf("the old log is not in the cold tier: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hotDir, shas[0][:2], shas[0][2:4], shas[0])); err == nil {
		t.Fatal("the old log is still in the hot tier")
	}
	if _, err := os.Stat(filepath.Join(hotDir, shas[1][:2], shas[1][2:4], shas[1])); err != nil {
		t.Fatalf("the young log left the hot tier: %v", err)
	}
	var tier string
	pool.QueryRow(ctx, `SELECT tier FROM blob WHERE sha256=$1`, shas[0]).Scan(&tier)
	if tier != "cold" {
		t.Fatalf("recorded as %s", tier)
	}
	if out, err := run(env, "blobs", "verify"); err != nil || !strings.Contains(out, "2 objects checked, 0 bad") {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	if out, err := run(env, "fsck"); err != nil || !strings.Contains(out, "0 missing, 0 orphaned, 0 corrupt") {
		t.Fatalf("fsck: %v\n%s", err, out)
	}
	// the settings are validated: the same directory, a missing directory, S3 on both sides
	for name, e := range map[string][]string{
		"same directory": {"TASKLANE_BLOB_DIR=" + hotDir, "TASKLANE_BLOB_COLD_BACKEND=fs", "TASKLANE_BLOB_COLD_DIR=" + hotDir},
		"no directory":   {"TASKLANE_BLOB_DIR=" + hotDir, "TASKLANE_BLOB_COLD_BACKEND=fs"},
	} {
		if out, err := run(e, "blobs", "verify"); err == nil {
			t.Errorf("%s was accepted: %s", name, out)
		}
	}
	if out, err := run([]string{"TASKLANE_BLOB_DIR=" + hotDir}, "blobs", "tier"); err == nil || !strings.Contains(out, "blob_cold_backend") {
		t.Errorf("tier without a cold tier: %v %s", err, out)
	}
}
