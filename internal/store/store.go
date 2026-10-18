// Package store owns the PostgreSQL connection pool and the migration framework.
package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	Pool *pgxpool.Pool
	// Source supplies the migrations; nil means the ones embedded in the binary. Tests set it to inject failures and releases.
	Source func() ([]Migration, error)
}

func (s *Store) migrations() ([]Migration, error) {
	if s.Source != nil {
		return s.Source()
	}
	return Migrations()
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migration is one numbered, forward-only schema step.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

func Migrations() ([]Migration, error) {
	ents, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var ms []Migration
	for _, e := range ents {
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%d_", &v); err != nil {
			return nil, fmt.Errorf("bad migration name %q", e.Name())
		}
		b, _ := migrationFS.ReadFile("migrations/" + e.Name())
		ms = append(ms, Migration{v, strings.TrimSuffix(e.Name(), ".sql"), string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	return ms, nil
}

func checksum(sql string) string { h := sha256.Sum256([]byte(sql)); return hex.EncodeToString(h[:]) }

// LintAll checks every embedded migration in order (later ones know which tables earlier ones created).
func LintAll() []string {
	ms, err := Migrations()
	if err != nil {
		return []string{err.Error()}
	}
	return lintSet(ms)
}

func lintSet(ms []Migration) []string {
	var out []string
	earlier := map[string]bool{}
	for _, m := range ms {
		out = append(out, Lint(m.Name, m.SQL, earlier)...)
		for _, t := range tablesIn(m.SQL) {
			earlier[t] = true
		}
	}
	return out
}

// PlanStep is one pending migration, as `tasklane migrate plan` reports it.
type PlanStep struct {
	Version int
	Name    string
	Phase   Phase
	NoTx    bool
	// Held: a contract migration removes what the previous release still uses, so `migrate` does not apply it; only
	// `migrate finalize` does, once no node of the previous release is left and a rollback is no longer wanted.
	Held bool
}

// ErrChecksum is returned when an applied migration's file changed afterwards (history must not be rewritten).
type ErrChecksum struct {
	Version int
	Name    string
}

func (e *ErrChecksum) Error() string {
	return fmt.Sprintf("migration %s was modified after it was applied; applied migrations are immutable (add a new migration instead)", e.Name)
}

// Plan lists what Migrate would apply, in order, without changing anything. It also verifies the checksums of
// everything already applied, so a rewritten migration is caught before it can cause drift.
func (s *Store) Plan(ctx context.Context) ([]PlanStep, error) {
	ms, err := s.migrations()
	if err != nil {
		return nil, err
	}
	applied := map[int]string{}
	var have bool
	if err := s.Pool.QueryRow(ctx, `SELECT to_regclass('schema_migration') IS NOT NULL`).Scan(&have); err != nil {
		return nil, err
	}
	if have {
		rows, err := s.Pool.Query(ctx, `SELECT version, COALESCE(checksum,'') FROM schema_migration`)
		if err != nil {
			// the checksum column does not exist yet on an older database: treat every applied row as unchecked
			rows, err = s.Pool.Query(ctx, `SELECT version, '' FROM schema_migration`)
			if err != nil {
				return nil, err
			}
		}
		for rows.Next() {
			var v int
			var c string
			rows.Scan(&v, &c)
			applied[v] = c
		}
		rows.Close()
	}
	var plan []PlanStep
	for _, m := range ms {
		if c, ok := applied[m.Version]; ok {
			if c != "" && c != checksum(m.SQL) {
				return nil, &ErrChecksum{m.Version, m.Name}
			}
			continue
		}
		ph, _ := phaseOf(m.SQL)
		plan = append(plan, PlanStep{m.Version, m.Name, ph, hasNoTx(m.SQL), ph == Contract})
	}
	return plan, nil
}

// Migrate applies pending migrations under an advisory lock so concurrent nodes are safe. Each migration runs in its own
// transaction (unless marked no-transaction) with a one second lock timeout: a migration that cannot get its lock fails
// fast and leaves the schema untouched, instead of queueing behind traffic and blocking it.
func (s *Store) Migrate(ctx context.Context) (applied []int, err error) { return s.migrate(ctx, false) }

// Finalize applies the pending contract migrations as well. After it, the previous release can no longer run on this
// database: going back means restoring a backup.
func (s *Store) Finalize(ctx context.Context) (applied []int, err error) { return s.migrate(ctx, true) }

func (s *Store) migrate(ctx context.Context, finalize bool) (applied []int, err error) {
	all, err := s.migrations()
	if err != nil {
		return nil, err
	}
	if problems := lintSet(all); len(problems) > 0 {
		return nil, fmt.Errorf("migration lint failed:\n  %s", strings.Join(problems, "\n  "))
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	// Poll for the runner lock instead of blocking in pg_advisory_lock: a blocked lock call is an open statement, and
	// CREATE INDEX CONCURRENTLY in the running node waits for every other open statement, so two nodes would deadlock.
	// A polling node is idle between attempts and never in the way.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var got bool
		if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(727274)`).Scan(&got); err != nil {
			return nil, err
		}
		if got {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another node has been migrating the schema for more than two minutes")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(727274)`)
	// every statement of the runner, including its bookkeeping, gives up after a second instead of queueing behind traffic
	if _, err = conn.Exec(ctx, `SET lock_timeout = '1s'`); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), `RESET lock_timeout`)
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now(), checksum text)`); err != nil {
		return nil, err
	}
	if _, err = conn.Exec(ctx, `ALTER TABLE schema_migration ADD COLUMN IF NOT EXISTS checksum text`); err != nil {
		return nil, err
	}
	// the phase of each applied migration is what tells a node of an older release whether the schema is still one it can run on
	if _, err = conn.Exec(ctx, `ALTER TABLE schema_migration ADD COLUMN IF NOT EXISTS phase text`); err != nil {
		return nil, err
	}
	done := map[int]string{}
	rows, err := conn.Query(ctx, `SELECT version, COALESCE(checksum,'') FROM schema_migration`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int
		var c string
		rows.Scan(&v, &c)
		done[v] = c
	}
	rows.Close()
	ms := all
	for _, m := range ms {
		if c, ok := done[m.Version]; ok {
			switch {
			case c == "": // a row from before checksums existed: adopt the current file
				conn.Exec(ctx, `UPDATE schema_migration SET checksum=$2 WHERE version=$1`, m.Version, checksum(m.SQL))
			case c != checksum(m.SQL):
				return applied, &ErrChecksum{m.Version, m.Name}
			}
			continue
		}
		ph, _ := phaseOf(m.SQL)
		if ph == Contract && !finalize {
			return applied, nil // nothing after a held migration is applied either: the order of the schema is the order of the files
		}
		if hasNoTx(m.SQL) {
			if _, err = conn.Exec(ctx, `SET lock_timeout = '1s'`); err == nil {
				_, err = conn.Exec(ctx, m.SQL)
			}
			conn.Exec(ctx, `RESET lock_timeout`)
			if err == nil {
				_, err = conn.Exec(ctx, `INSERT INTO schema_migration(version,name,checksum,phase) VALUES($1,$2,$3,$4)`, m.Version, m.Name, checksum(m.SQL), string(ph))
			}
			if err != nil {
				return applied, fmt.Errorf("migration %s: %w", m.Name, err)
			}
			applied = append(applied, m.Version)
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err == nil {
			_, err = tx.Exec(ctx, m.SQL)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migration(version,name,checksum,phase) VALUES($1,$2,$3,$4)`, m.Version, m.Name, checksum(m.SQL), string(ph))
		}
		if err != nil {
			tx.Rollback(ctx)
			return applied, fmt.Errorf("migration %s: %w", m.Name, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

// SchemaVersion returns the highest applied migration (0 if none).
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM schema_migration`).Scan(&v)
	return v, err
}

// OpenSchema opens a pool whose search_path is pinned to one schema; used by tests for isolation.
func OpenSchema(ctx context.Context, dsn, schema string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{Pool: pool}, nil
}
