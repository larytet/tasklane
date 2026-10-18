// Package store owns the PostgreSQL connection pool and the migration framework.
package store

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct{ Pool *pgxpool.Pool }

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

// Migrate applies pending migrations under an advisory lock so concurrent nodes are safe.
// Each migration runs in its own transaction and is recorded with it.
func (s *Store) Migrate(ctx context.Context) (applied []int, err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, `SELECT pg_advisory_lock(727274)`); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(727274)`)
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (version int PRIMARY KEY, name text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	done := map[int]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migration`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v int
		rows.Scan(&v)
		done[v] = true
	}
	rows.Close()
	ms, err := Migrations()
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if done[m.Version] {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err = tx.Exec(ctx, m.SQL); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migration(version,name) VALUES($1,$2)`, m.Version, m.Name)
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
