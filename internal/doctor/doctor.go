// Package doctor checks an installation: database, schema version, blob directory, mail, admin account.
package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/larytet/tasklane/internal/config"
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
		add("database", false, "cannot connect: "+err.Error())
	} else {
		defer st.Close()
		add("database", true, "connected")
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
		add("mail", true, "outbound email is not configured (account mail and notifications are off)", true)
	} else {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.SMTPHost, fmt.Sprint(cfg.SMTPPort)), 3*time.Second)
		if err != nil {
			add("mail", false, "cannot reach SMTP server: "+err.Error())
		} else {
			c.Close()
			add("mail", true, "SMTP server reachable")
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
