// Package testutil gives every test its own PostgreSQL schema, a seeded service and users.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/larytet/tasklane/internal/testutil/s3mock"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/store"
)

func DSN() string {
	if v := os.Getenv("TASKLANE_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://tl:tl@localhost:55432/tasklane"
}

type Env struct {
	T     *testing.T
	Svc   *domain.Service
	Store *store.Store
	Admin domain.Actor
	Alice domain.Actor
	Bob   domain.Actor
	Clock *Clock
}

// Clock is the injectable clock (TESTPLAN: no sleeps in time-dependent tests).
type Clock struct{ T time.Time }

func (c *Clock) Now() time.Time          { return c.T }
func (c *Clock) Advance(d time.Duration) { c.T = c.T.Add(d) }

func New(t *testing.T) *Env {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, DSN())
	if err != nil || admin.Ping(ctx) != nil {
		t.Skip("PostgreSQL not available: " + DSN())
	}
	b := make([]byte, 6)
	rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSchema(ctx, DSN(), schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := domain.New(st.Pool)
	clk := &Clock{T: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
	svc.Now = clk.Now
	if err := svc.EnsureDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	e := &Env{T: t, Svc: svc, Store: st, Clock: clk}
	e.Admin = e.user("admin@example.com", "Admin", true)
	e.Alice = e.user("alice@example.com", "Alice K.", false)
	e.Bob = e.user("bob@example.com", "Bob R.", false)
	return e
}

// NewUser creates another account for tests that need a role of their own.
func (e *Env) NewUser(email, name string, admin bool) domain.Actor { return e.user(email, name, admin) }

func (e *Env) user(email, name string, admin bool) domain.Actor {
	u, err := e.Svc.CreateUser(context.Background(), email, name, "password123", admin)
	if err != nil {
		e.T.Fatal(err)
	}
	return domain.Actor{ID: u.ID, Label: u.Name, Admin: u.Admin, Kind: "human"}
}

// Project creates project ABC with Alice and Bob as members.
func (e *Env) Project() {
	ctx := context.Background()
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "ABC", "Alpha"); err != nil {
		e.T.Fatal(err)
	}
	for _, a := range []domain.Actor{e.Alice, e.Bob} {
		if err := e.Svc.AddMember(ctx, e.Admin, "ABC", a.ID, "member"); err != nil {
			e.T.Fatal(err)
		}
	}
}

func (e *Env) Issue(summary, desc string) domain.Issue {
	is, err := e.Svc.CreateIssue(context.Background(), e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: summary, Description: desc})
	if err != nil {
		e.T.Fatal(err)
	}
	return is
}

// Ctx is a convenience for tests that need a context value.
func Ctx() context.Context { return context.Background() }

// WithBlobs gives the service a filesystem blob store in a temp dir and zero GC retention.
func (e *Env) WithBlobs() *blob.FS {
	fs, err := blob.NewFS(e.T.TempDir())
	if err != nil {
		e.T.Fatal(err)
	}
	if err := e.Svc.ConfigureBlobs(context.Background(), &domain.Blobs{Store: fs}); err != nil {
		e.T.Fatal(err)
	}
	return fs
}

// WithS3Blobs backs the service with the in-process S3 double and returns both, with 5 MiB parts so multipart is
// exercised by modest files and zero GC retention.
func (e *Env) WithS3Blobs() (*blob.S3, *s3mock.Server) {
	m := s3mock.New()
	e.T.Cleanup(m.Close)
	c, err := blob.NewS3Client(blob.S3Config{Endpoint: m.URL, Region: s3mock.Region, Bucket: s3mock.Bucket, AccessKey: s3mock.AccessKey, SecretKey: s3mock.SecretKey,
		PartSize: 5 << 20, Sleep: func(time.Duration) {}})
	if err != nil {
		e.T.Fatal(err)
	}
	st, err := blob.NewS3(c, e.T.TempDir())
	if err != nil {
		e.T.Fatal(err)
	}
	if err := e.Svc.ConfigureBlobs(context.Background(), &domain.Blobs{Store: st}); err != nil {
		e.T.Fatal(err)
	}
	return st, m
}
