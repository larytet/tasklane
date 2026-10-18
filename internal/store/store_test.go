package store_test

import (
	"context"
	"sync"
	"testing"

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
