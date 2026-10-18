package doctor_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"testing"

	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/doctor"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/smtpmock"
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

// INST-02: each fault is named, with a fix.
func faultCfg(t *testing.T) config.Config {
	return config.Config{DatabaseURL: testutil.DSN(), BlobDir: t.TempDir()}
}

func TestINST02_MissingExtensionsOnAFreshDatabase(t *testing.T) {
	admin, err := pgxpool.New(context.Background(), testutil.DSN())
	if err != nil || admin.Ping(context.Background()) != nil {
		t.Skip("PostgreSQL not available")
	}
	defer admin.Close()
	name := "tl_noext_" + fmt.Sprint(time.Now().UnixNano()%1e9)
	if _, err := admin.Exec(context.Background(), `CREATE DATABASE `+name); err != nil {
		t.Skip("cannot create a database: " + err.Error())
	}
	defer admin.Exec(context.Background(), `DROP DATABASE `+name+` WITH (FORCE)`)
	cfg := faultCfg(t)
	cfg.DatabaseURL = strings.Replace(testutil.DSN(), "/tasklane", "/"+name, 1)
	cs := doctor.Run(context.Background(), cfg)
	for _, ext := range []string{"pg_trgm", "unaccent"} {
		c := find(cs, "extension "+ext)
		if c.OK || !strings.Contains(c.Detail, "CREATE EXTENSION "+ext) {
			t.Fatalf("%s: %+v", ext, c)
		}
	}
	if c := find(cs, "schema"); c.OK || !strings.Contains(c.Detail, "tasklane migrate") {
		t.Fatalf("%+v", c)
	}
	if !doctor.Failed(cs) {
		t.Fatal("must fail")
	}
	if c := find(cs, "postgresql"); !c.OK {
		t.Fatalf("%+v", c)
	}
}

func TestINST02_SMTPFaultsAreNamedWithAFix(t *testing.T) {
	// healthy relay
	m := smtpmock.New(t, smtpmock.Options{TLS: "starttls", User: "u", Pass: "p"})
	cfg := faultCfg(t)
	cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPTLS, cfg.SMTPUser, cfg.SMTPPass = "localhost", m.Port(), "starttls", "u", "wrong"
	// the mock's certificate is not trusted by the system roots, so the first thing doctor must say is exactly that
	c := find(doctor.Run(context.Background(), cfg), "mail")
	if c.OK || !strings.Contains(c.Detail, "certificate") || !strings.Contains(c.Detail, "install its CA") {
		t.Fatalf("%+v", c)
	}
	// plaintext relay with wrong credentials: login failure names the settings to check
	m2 := smtpmock.New(t, smtpmock.Options{TLS: "none", User: "u", Pass: "p"})
	cfg.SMTPPort, cfg.SMTPTLS = m2.Port(), "none"
	c = find(doctor.Run(context.Background(), cfg), "mail")
	if c.OK || !strings.Contains(c.Detail, "TASKLANE_SMTP_PASS") {
		t.Fatalf("%+v", c)
	}
	cfg.SMTPPass = "p"
	if c = find(doctor.Run(context.Background(), cfg), "mail"); !c.OK {
		t.Fatalf("%+v", c)
	}
	if len(m2.Messages()) != 0 { // a probe sends no mail
		t.Fatal("doctor sent a message")
	}
	// wrong mode for the port, and a relay that is not there
	cfg.SMTPTLS = "starttls"
	if c = find(doctor.Run(context.Background(), cfg), "mail"); c.OK || !strings.Contains(c.Detail, "TASKLANE_SMTP_TLS") {
		t.Fatalf("%+v", c)
	}
	cfg.SMTPPort = 1
	if c = find(doctor.Run(context.Background(), cfg), "mail"); c.OK || !strings.Contains(c.Detail, "TASKLANE_SMTP_HOST") {
		t.Fatalf("%+v", c)
	}
}

func TestINST02_UnwritableBlobDirectoryAndHealthyReport(t *testing.T) {
	cfg := faultCfg(t)
	dir := t.TempDir()
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if os.Geteuid() != 0 {
		cfg.BlobDir = dir + "/blobs"
		if c := find(doctor.Run(context.Background(), cfg), "blob store"); c.OK {
			t.Fatalf("%+v", c)
		}
	}
	cs := doctor.Run(context.Background(), faultCfg(t))
	for _, name := range []string{"database", "postgresql", "clock", "file descriptors"} {
		if c := find(cs, name); !c.OK {
			t.Fatalf("healthy system reports %s: %+v", name, c)
		}
	}
}
