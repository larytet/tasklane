package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/store"
	"github.com/larytet/tasklane/internal/testutil"
)

func freshSchema(t *testing.T, name string) *store.Store {
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available")
	}
	admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+name+` CASCADE`)
	admin.Exec(ctx, `CREATE SCHEMA `+name)
	st, err := store.OpenSchema(ctx, testutil.DSN(), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); admin.Exec(ctx, `DROP SCHEMA `+name+` CASCADE`); admin.Close() })
	return st
}

func TestUPG01_MigrateIsIdempotentAndRecorded(t *testing.T) {
	st := freshSchema(t, "mig_idem")
	ctx := context.Background()
	a, err := st.Migrate(ctx)
	if err != nil || len(a) == 0 {
		t.Fatalf("%v %v", a, err)
	}
	b, err := st.Migrate(ctx)
	if err != nil || len(b) != 0 {
		t.Fatalf("second run applied %v (%v)", b, err)
	}
	v, _ := st.SchemaVersion(ctx)
	ms, _ := store.Migrations()
	if v != ms[len(ms)-1].Version {
		t.Fatalf("version %d", v)
	}
}

func TestUPG01_ConcurrentNodesMigrateOnce(t *testing.T) {
	st := freshSchema(t, "mig_conc")
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := st.Migrate(ctx)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += len(a)
			mu.Unlock()
		}()
	}
	wg.Wait()
	ms, _ := store.Migrations()
	if total != len(ms) { // each migration applied by exactly one node
		t.Fatalf("applied %d times, want %d", total, len(ms))
	}
}

func TestMigrationsAreNumberedWithoutGaps(t *testing.T) {
	ms, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range ms {
		if m.Version != i+1 {
			t.Fatalf("migration %q has version %d at position %d", m.Name, m.Version, i+1)
		}
	}
}

func TestUPG01_RealMigrationsPassTheLint(t *testing.T) {
	if p := store.LintAll(); len(p) > 0 {
		t.Fatalf("%v", p)
	}
}

func TestUPG01_LintRejectsDangerousExpandMigrations(t *testing.T) {
	earlier := map[string]bool{"issue": true}
	cases := map[string]string{
		"no header":                      "ALTER TABLE issue ADD COLUMN x int;",
		"drop column":                    "-- phase: expand\nALTER TABLE issue DROP COLUMN summary;",
		"drop table":                     "-- phase: expand\nDROP TABLE comment;",
		"rename":                         "-- phase: expand\nALTER TABLE issue RENAME COLUMN summary TO title;",
		"type change":                    "-- phase: expand\nALTER TABLE issue ALTER COLUMN version TYPE int;",
		"set not null":                   "-- phase: expand\nALTER TABLE issue ALTER COLUMN assignee_id SET NOT NULL;",
		"truncate":                       "-- phase: expand\nTRUNCATE comment;",
		"plain index on an old table":    "-- phase: expand\nCREATE INDEX i_x ON issue (priority);",
		"concurrent index inside a tx":   "-- phase: expand\nCREATE INDEX CONCURRENTLY i_x ON issue (priority);",
		"multi-statement no-transaction": "-- phase: expand\n-- no-transaction\nCREATE INDEX CONCURRENTLY a ON issue (priority);\nCREATE INDEX CONCURRENTLY b ON issue (status_id);",
	}
	for name, sql := range cases {
		if len(store.Lint("m", sql, earlier)) == 0 {
			t.Errorf("%s was accepted", name)
		}
	}
	ok := map[string]string{
		"add nullable column":             "-- phase: expand\nALTER TABLE issue ADD COLUMN x int;",
		"add column with default":         "-- phase: expand\nALTER TABLE issue ADD COLUMN y text NOT NULL DEFAULT '';",
		"new table and its index":         "-- phase: expand\nCREATE TABLE t (a int);\nCREATE INDEX t_a ON t (a);",
		"concurrent index, no-tx":         "-- phase: expand\n-- no-transaction\nCREATE INDEX CONCURRENTLY i_x ON issue (priority);",
		"a trigger that refuses truncate": "-- phase: expand\nCREATE TRIGGER t BEFORE TRUNCATE ON issue FOR EACH STATEMENT EXECUTE FUNCTION f();\nCREATE TRIGGER u BEFORE UPDATE OR DELETE OR TRUNCATE ON issue FOR EACH STATEMENT EXECUTE FUNCTION f();",
		"contract may drop":               "-- phase: contract\nALTER TABLE issue DROP COLUMN old;",
		"commented-out drop ignored":      "-- phase: expand\n-- DROP TABLE comment;\nALTER TABLE issue ADD COLUMN z int;",
	}
	for name, sql := range ok {
		if p := store.Lint("m", sql, earlier); len(p) > 0 {
			t.Errorf("%s was rejected: %v", name, p)
		}
	}
}

func TestUPG01_PlanMatchesWhatMigrateDoes(t *testing.T) {
	st := freshSchema(t, "mig_plan")
	ctx := context.Background()
	plan, err := st.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := store.Migrations()
	if len(plan) != len(ms) || plan[0].Phase != store.Expand {
		t.Fatalf("fresh database plan: %+v", plan)
	}
	if v, _ := st.SchemaVersion(ctx); v != 0 {
		t.Fatal("plan changed the database")
	}
	applied, err := st.Migrate(ctx)
	if err != nil || len(applied) != len(plan) {
		t.Fatalf("%v %v", applied, err)
	}
	for i, p := range plan {
		if applied[i] != p.Version {
			t.Fatalf("plan said %d, migrate applied %d", p.Version, applied[i])
		}
	}
	if again, _ := st.Plan(ctx); len(again) != 0 {
		t.Fatalf("plan after migrate: %+v", again)
	}
}

func TestUPG01_ModifiedAppliedMigrationIsDetected(t *testing.T) {
	st := freshSchema(t, "mig_sum")
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// someone edited an applied migration file: simulate by changing the recorded checksum
	st.Pool.Exec(ctx, `UPDATE schema_migration SET checksum='deadbeef' WHERE version=2`)
	var ec *store.ErrChecksum
	if _, err := st.Migrate(ctx); !errors.As(err, &ec) || ec.Version != 2 || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("%v", err)
	}
	if _, err := st.Plan(ctx); !errors.As(err, &ec) {
		t.Fatalf("plan did not detect it: %v", err)
	}
	// rows from before checksums existed are adopted, not rejected
	st.Pool.Exec(ctx, `UPDATE schema_migration SET checksum=NULL`)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("legacy rows: %v", err)
	}
	var n int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM schema_migration WHERE checksum IS NOT NULL`).Scan(&n)
	if n == 0 {
		t.Fatal("checksums were not adopted")
	}
}

// A migration that cannot get its lock fails within about a second with a lock-timeout error and changes nothing,
// instead of queueing behind traffic and blocking it.
func TestUPG01_MigrationFailsFastWhenTheLockIsHeld(t *testing.T) {
	st := freshSchema(t, "mig_lock")
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil { // everything applied once
		t.Fatal(err)
	}
	v0, _ := st.SchemaVersion(ctx)
	// another session holds an exclusive lock on the bookkeeping table (a long-running transaction, a stuck deploy)
	blocker, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE schema_migration IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan error, 1)
	go func() { _, err := st.Migrate(ctx); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "lock timeout") {
			t.Fatalf("want a lock timeout error, got %v", err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("took %v to give up", d)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("migration blocked behind a held lock for 15 s")
	}
	blocker.Rollback(ctx)
	if v, _ := st.SchemaVersion(ctx); v != v0 {
		t.Fatal("a failed migration changed the schema")
	}
}

// A no-transaction migration (CREATE INDEX CONCURRENTLY) really builds the index and is recorded.
func TestUPG01_ConcurrentIndexMigrationRuns(t *testing.T) {
	st := freshSchema(t, "mig_conc_idx")
	ctx := context.Background()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='issue_history_field_idx'`).Scan(&n)
	if n != 1 {
		t.Fatal("the concurrent index was not built")
	}
	var valid bool
	st.Pool.QueryRow(ctx, `SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relname='issue_history_field_idx' AND c.relnamespace=current_schema()::regnamespace`).Scan(&valid)
	if !valid {
		t.Fatal("the index is not valid")
	}
}

// UPG-01: a concurrent index build that gives up on a busy database (it waits for every other transaction, and the migration's lock time-out is
// one second) must not leave an invalid index that the next run takes for the real one.
func TestUPG01_AFailedConcurrentIndexBuildIsNotLeftBehindAsValid(t *testing.T) {
	st := freshSchema(t, "mig_invalid_idx")
	ctx := context.Background()
	defer func(d time.Duration) { store.ConcurrentIndexLockTimeout = d }(store.ConcurrentIndexLockTimeout)
	store.ConcurrentIndexLockTimeout = 700 * time.Millisecond
	other, err := pgxpool.New(ctx, testutil.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	blocker, err := other.Begin(ctx) // an open transaction with a snapshot: the index build cannot finish while it lasts
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
		t.Fatal(err)
	}
	blocker.Exec(ctx, `SELECT count(*) FROM pg_class`)
	start := time.Now()
	if _, err := st.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("migrating beside an open transaction: %v", err)
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Fatalf("gave up after %s: the build is attempted three times", d)
	}
	invalid := func() (n int) {
		st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_index x JOIN pg_class c ON c.oid=x.indexrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE NOT x.indisvalid AND n.nspname='mig_invalid_idx'`).Scan(&n)
		return
	}
	if n := invalid(); n != 0 {
		t.Fatalf("%d invalid index(es) left behind", n)
	}
	blocker.Rollback(ctx)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrating again: %v", err)
	}
	if n := invalid(); n != 0 {
		t.Fatalf("%d invalid index(es) after the second run", n)
	}
	var have int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='mig_invalid_idx' AND indexname='mail_inbound_received_idx'`).Scan(&have)
	if have != 1 {
		t.Fatalf("the index of migration 0049 exists %d time(s)", have)
	}
}
