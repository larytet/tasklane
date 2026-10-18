package doctor_test

import (
	"context"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/doctor"
	"github.com/larytet/tasklane/internal/testutil"
)

func find(cs []doctor.Check, name string) doctor.Check {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return doctor.Check{}
}

func TestCFG_DoctorReportsBrokenEnvironmentWithoutPanicking(t *testing.T) {
	cfg := config.Config{DatabaseURL: "postgres://tl:tl@127.0.0.1:1/none", BlobDir: t.TempDir(), SMTPHost: "127.0.0.1", SMTPPort: 1}
	cs := doctor.Run(context.Background(), cfg)
	if c := find(cs, "database"); c.OK || !strings.Contains(c.Detail, "cannot connect") {
		t.Fatalf("%+v", c)
	}
	if c := find(cs, "mail"); c.OK {
		t.Fatalf("%+v", c)
	}
	if !doctor.Failed(cs) {
		t.Fatal("must fail")
	}
}

func TestCFG_DoctorHealthyInstall(t *testing.T) {
	e := testutil.New(t) // schema is migrated, admin exists
	_ = e
	cfg := config.Config{DatabaseURL: testutil.DSN() + "?search_path=" + schemaOf(e), BlobDir: t.TempDir()}
	cs := doctor.Run(context.Background(), cfg)
	if c := find(cs, "schema"); !c.OK {
		t.Fatalf("%+v", c)
	}
	if c := find(cs, "admin account"); !c.OK {
		t.Fatalf("%+v", c)
	}
	if c := find(cs, "blob store"); !c.OK {
		t.Fatalf("%+v", c)
	}
	if doctor.Failed(cs) {
		t.Fatalf("%+v", cs)
	}
}

func schemaOf(e *testutil.Env) string {
	var s string
	e.Store.Pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&s)
	return s
}
