package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/store"
)

func src(ms ...store.Migration) func() ([]store.Migration, error) {
	return func() ([]store.Migration, error) { return ms, nil }
}

func mig(v int, name, phase, sql string) store.Migration {
	return store.Migration{Version: v, Name: name, SQL: "-- phase: " + phase + "\n" + sql}
}

var (
	m1 = mig(1, "0001_base", "expand", "CREATE TABLE widget (id int PRIMARY KEY, label text);")
	m2 = mig(2, "0002_color", "expand", "ALTER TABLE widget ADD COLUMN color text;")
	m3 = mig(3, "0003_size", "expand", "ALTER TABLE widget ADD COLUMN size int;")
	m4 = mig(4, "0004_drop_label", "contract", "ALTER TABLE widget DROP COLUMN label;")
)

func tableHas(t *testing.T, st *store.Store, col string) bool {
	var ok bool
	if err := st.Pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='widget' AND column_name=$1)`, col).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// UPG-06: a node outside the schema range refuses to start, and says why and what to do.
func TestUPG06_ANodeOutsideItsSchemaRangeRefusesToStart(t *testing.T) {
	st := freshSchema(t, "upg06")
	ctx := context.Background()
	old := store.Policy{Latest: 2}
	// an empty database is for the caller to initialize
	if v, err := st.CheckCompat(ctx, old); err != nil || !v.Fresh {
		t.Fatalf("%+v %v", v, err)
	}
	st.Source = src(m1, m2)
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := st.CheckCompat(ctx, store.Policy{Latest: 2}); err != nil || v.Fresh || v.Window {
		t.Fatalf("in step: %+v %v", v, err)
	}
	// behind: a newer binary on an older schema does not migrate by itself
	_, err := st.CheckCompat(ctx, store.Policy{Latest: 3})
	var inc *store.IncompatibleError
	if !errors.As(err, &inc) || !strings.Contains(inc.Reason, "behind") || !strings.Contains(inc.Fix, "tasklane migrate") {
		t.Fatalf("%v", err)
	}
	// a newer release applies an expand migration: the older binary runs beside it, with a note
	st.Source = src(m1, m2, m3)
	st.Migrate(ctx)
	v, err := st.CheckCompat(ctx, store.Policy{Latest: 2})
	if err != nil || !v.Window || !strings.Contains(v.Note, "expand-only") || v.Applied != 3 {
		t.Fatalf("inside the window: %+v %v", v, err)
	}
	// the newer release finalizes: a contract migration removed a column the older one reads, so the older one refuses
	st.Source = src(m1, m2, m3, m4)
	if n, err := st.Migrate(ctx); err != nil || len(n) != 0 {
		t.Fatalf("migrate applied a contract migration: %v %v", n, err)
	}
	if _, err := st.CheckCompat(ctx, store.Policy{Latest: 2}); err != nil {
		t.Fatalf("a held contract migration closed the window: %v", err)
	}
	if n, err := st.Finalize(ctx); err != nil || len(n) != 1 {
		t.Fatalf("%v %v", n, err)
	}
	_, err = st.CheckCompat(ctx, store.Policy{Latest: 2})
	if !errors.As(err, &inc) || !strings.Contains(inc.Reason, "finalized") || !strings.Contains(inc.Reason, "4") || !strings.Contains(inc.Fix, "restore a backup") {
		t.Fatalf("after finalize: %v", err)
	}
	// even a binary that knows version 3 but not 4 is outside the window now
	if _, err := st.CheckCompat(ctx, store.Policy{Latest: 3}); err == nil {
		t.Fatal("a binary that lacks the contract migration was allowed to run")
	}
	if v, err := st.CheckCompat(ctx, store.Policy{Latest: 4}); err != nil || v.Window {
		t.Fatalf("the current binary: %+v %v", v, err)
	}
	// a migration whose phase was never recorded cannot be classified: refused rather than guessed
	st.Pool.Exec(ctx, `UPDATE schema_migration SET phase=NULL WHERE version=3`)
	if _, err := st.CheckCompat(ctx, store.Policy{Latest: 2}); err == nil {
		t.Fatal("an unclassified migration was treated as safe")
	}
}

// UPG-05: a database older than the oldest this release upgrades from is refused with the stepping path.
func TestUPG05_ADatabaseOlderThanTheFloorIsRefusedWithTheSteppingPath(t *testing.T) {
	st := freshSchema(t, "upg05")
	ctx := context.Background()
	st.Source = src(m1, m2)
	st.Migrate(ctx)
	p := store.Policy{Latest: 5, Floor: 3, FloorRelease: "1.2.0"}
	_, err := st.CheckCompat(ctx, p)
	var inc *store.IncompatibleError
	if !errors.As(err, &inc) || !strings.Contains(inc.Reason, "older than the oldest") || !strings.Contains(inc.Reason, "1.2.0") || !strings.Contains(inc.Fix, "Upgrade to 1.2.0 first") {
		t.Fatalf("%v", err)
	}
	p.Stepping = "Upgrade to 1.2.0, run it once, then upgrade to this release."
	if _, err := st.CheckCompat(ctx, p); err == nil || !strings.Contains(err.Error(), "run it once") {
		t.Fatalf("%v", err)
	}
	// at the floor it is only behind (and told to migrate), not too old
	p2 := store.Policy{Latest: 5, Floor: 2, FloorRelease: "1.2.0"}
	if _, err := st.CheckCompat(ctx, p2); err == nil || strings.Contains(err.Error(), "older than the oldest") {
		t.Fatalf("%v", err)
	}
	// the real policy of this build has a latest equal to its newest migration
	ms, _ := store.Migrations()
	if p := store.DefaultPolicy(); p.Latest != ms[len(ms)-1].Version {
		t.Fatalf("%+v", p)
	}
}

// Contract migrations apply only with finalize, and nothing after a held one is applied in between.
func TestUPG05_ContractMigrationsWaitForFinalizeAndKeepTheOrder(t *testing.T) {
	st := freshSchema(t, "upg05b")
	ctx := context.Background()
	m5 := mig(5, "0005_late", "expand", "ALTER TABLE widget ADD COLUMN late int;")
	st.Source = src(m1, m2, m3, m4, m5)
	plan, err := st.Plan(ctx)
	if err != nil || len(plan) != 5 || !plan[3].Held || plan[0].Held || plan[3].Phase != "contract" {
		t.Fatalf("%+v %v", plan, err)
	}
	applied, err := st.Migrate(ctx)
	if err != nil || len(applied) != 3 {
		t.Fatalf("%v %v", applied, err)
	}
	if !tableHas(t, st, "label") || tableHas(t, st, "late") {
		t.Fatal("the schema moved past a held contract migration")
	}
	var phase string
	st.Pool.QueryRow(ctx, `SELECT phase FROM schema_migration WHERE version=2`).Scan(&phase)
	if phase != "expand" {
		t.Fatalf("the phase was not recorded: %q", phase)
	}
	if again, _ := st.Migrate(ctx); len(again) != 0 {
		t.Fatalf("a second migrate applied %v", again)
	}
	applied, err = st.Finalize(ctx)
	if err != nil || len(applied) != 2 || tableHas(t, st, "label") || !tableHas(t, st, "late") {
		t.Fatalf("%v %v", applied, err)
	}
	st.Pool.QueryRow(ctx, `SELECT phase FROM schema_migration WHERE version=4`).Scan(&phase)
	if phase != "contract" {
		t.Fatalf("%q", phase)
	}
}

// UPG-12: a migration that fails midway leaves no partial schema, and the node recovers by running the fixed one.
func TestUPG12_AMigrationThatFailsMidwayLeavesNothingBehind(t *testing.T) {
	st := freshSchema(t, "upg12")
	ctx := context.Background()
	bad := mig(2, "0002_half", "expand", "CREATE TABLE half_a (id int);\nCREATE TABLE half_b (id int);\nINSERT INTO no_such_table VALUES (1);")
	st.Source = src(m1, bad)
	applied, err := st.Migrate(ctx)
	if err == nil || !strings.Contains(err.Error(), "0002_half") || len(applied) != 1 {
		t.Fatalf("%v %v", applied, err)
	}
	exists := func(tbl string) (ok bool) {
		st.Pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, tbl).Scan(&ok)
		return
	}
	if exists("half_a") || exists("half_b") {
		t.Fatal("a failed migration left part of its work behind")
	}
	if v, _ := st.SchemaVersion(ctx); v != 1 {
		t.Fatalf("the failed migration was recorded: version %d", v)
	}
	// the node still starts on what is applied (nothing is half done), and the fixed migration then applies
	if v, err := st.CheckCompat(ctx, store.Policy{Latest: 1}); err != nil || v.Fresh {
		t.Fatalf("%+v %v", v, err)
	}
	fixed := mig(2, "0002_half", "expand", "CREATE TABLE half_a (id int);\nCREATE TABLE half_b (id int);")
	st.Source = src(m1, fixed)
	if applied, err := st.Migrate(ctx); err != nil || len(applied) != 1 || !exists("half_a") || !exists("half_b") {
		t.Fatalf("%v %v", applied, err)
	}
	// a database that is not there is an error with a reason, not a crash or a guess
	st.Pool.Close()
	if _, err := st.CheckCompat(ctx, store.Policy{Latest: 2}); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("%v", err)
	}
}
