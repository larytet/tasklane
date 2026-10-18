// Package doctor checks an installation: database, schema version, blob directory, mail, admin account.
package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/store"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"`
	Detail string `json:"detail"`
}

// Run performs every check and returns them; it never panics on a broken environment.
func Run(ctx context.Context, cfg config.Config) []Check {
	var out []Check
	add := func(name string, ok bool, detail string, warn ...bool) {
		out = append(out, Check{Name: name, OK: ok, Warn: len(warn) > 0 && warn[0], Detail: detail})
	}
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		add("database", false, "cannot connect: "+err.Error()+". Check TASKLANE_DATABASE_URL, that PostgreSQL is running and that the network allows the connection")
	} else {
		defer st.Close()
		add("database", true, "connected")
		var ver int
		st.Pool.QueryRow(ctx, `SHOW server_version_num`).Scan(&ver)
		var vs string
		st.Pool.QueryRow(ctx, `SHOW server_version`).Scan(&vs)
		if ver != 0 && ver < 150000 {
			add("postgresql", false, fmt.Sprintf("PostgreSQL %s is too old; Tasklane supports PostgreSQL 15 and newer. Upgrade the server", vs))
		} else {
			add("postgresql", true, "version "+vs)
		}
		have := map[string]bool{}
		if rows, err := st.Pool.Query(ctx, `SELECT extname FROM pg_extension`); err == nil {
			for rows.Next() {
				var n string
				rows.Scan(&n)
				have[n] = true
			}
			rows.Close()
		}
		for _, ext := range []string{"pg_trgm", "unaccent"} {
			if have[ext] {
				add("extension "+ext, true, "installed")
			} else {
				add("extension "+ext, false, "missing: run `CREATE EXTENSION "+ext+" SCHEMA public;` as a user who may (the database owner can on PostgreSQL 13 and later), or install the postgresql-contrib package")
			}
		}
		var dbNow time.Time
		if err := st.Pool.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err == nil {
			skew := time.Since(dbNow)
			if skew < 0 {
				skew = -skew
			}
			switch {
			case skew > 30*time.Second:
				add("clock", false, fmt.Sprintf("this host and the database differ by %v; fix NTP on one of them (sessions, links and schedules depend on it)", skew.Round(time.Second)))
			case skew > 3*time.Second:
				add("clock", true, fmt.Sprintf("this host and the database differ by %v; check NTP", skew.Round(time.Second)), true)
			default:
				add("clock", true, "in step with the database")
			}
		}
		ms, _ := store.Migrations()
		want := 0
		if len(ms) > 0 {
			want = ms[len(ms)-1].Version
		}
		got, err := st.SchemaVersion(ctx)
		switch {
		case err != nil:
			add("schema", false, "schema is not initialized: run `tasklane migrate`")
		case got < want:
			add("schema", false, fmt.Sprintf("schema version %d is behind the binary (%d): run `tasklane migrate`", got, want))
		case got > want:
			add("schema", false, fmt.Sprintf("schema version %d is newer than this binary (%d): upgrade the binary", got, want))
		default:
			add("schema", true, fmt.Sprintf("version %d", got))
			var n int
			st.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE is_admin AND active`).Scan(&n)
			if n == 0 {
				add("admin account", false, "no administrator exists: run `tasklane create-admin`", true)
			} else {
				add("admin account", true, fmt.Sprintf("%d administrator(s)", n))
			}
		}
	}
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil {
		if rl.Cur < 1024 {
			add("file descriptors", false, fmt.Sprintf("the open-file limit is %d; raise it to at least 4096 (ulimit -n, LimitNOFILE= in systemd)", rl.Cur))
		} else {
			add("file descriptors", true, fmt.Sprintf("limit %d", rl.Cur))
		}
	}
	if err := os.MkdirAll(cfg.BlobDir, 0o750); err != nil {
		add("blob store", false, "cannot create "+cfg.BlobDir+": "+err.Error())
	} else {
		f := filepath.Join(cfg.BlobDir, ".doctor")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			add("blob store", false, cfg.BlobDir+" is not writable: "+err.Error())
		} else {
			os.Remove(f)
			add("blob store", true, "local filesystem at "+cfg.BlobDir+" is writable")
		}
	}
	if cfg.SMTPHost == "" {
		add("mail", true, "outbound email is not configured (account mail and notifications are off); set TASKLANE_SMTP_HOST to turn it on", true)
	} else {
		p := &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, TLS: cfg.SMTPTLS, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.SMTPFrom, Timeout: 5 * time.Second}
		if err := p.Probe(ctx); err != nil {
			hint := ""
			switch {
			case strings.Contains(err.Error(), "authentication"):
				hint = ". Check TASKLANE_SMTP_USER and TASKLANE_SMTP_PASS"
			case strings.Contains(err.Error(), "certificate"):
				hint = ". The relay's certificate is not trusted; install its CA on this host (verification cannot be turned off)"
			case strings.Contains(err.Error(), "STARTTLS"):
				hint = ". Set TASKLANE_SMTP_TLS to the mode the relay supports (tls for port 465)"
			case strings.Contains(err.Error(), "connect"):
				hint = ". Check TASKLANE_SMTP_HOST and TASKLANE_SMTP_PORT and the firewall"
			}
			add("mail", false, "cannot use the SMTP relay: "+err.Error()+hint)
		} else {
			add("mail", true, "SMTP relay reachable, TLS negotiated and login accepted")
		}
	}
	return out
}

// Failed reports whether any non-warning check failed.
func Failed(cs []Check) bool {
	for _, c := range cs {
		if !c.OK && !c.Warn {
			return true
		}
	}
	return false
}
