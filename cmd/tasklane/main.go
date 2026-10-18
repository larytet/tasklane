// Command tasklane runs the server and its maintenance subcommands.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/doctor"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/store"
)

var version = "0.1.0-dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg := config.Load()
	ctx := context.Background()
	switch cmd {
	case "serve":
		serve(ctx, cfg)
	case "migrate":
		st := mustOpen(ctx, cfg)
		defer st.Close()
		applied, err := st.Migrate(ctx)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("applied %d migration(s) %v\n", len(applied), applied)
	case "doctor":
		cs := doctor.Run(ctx, cfg)
		for _, c := range cs {
			mark := "ok  "
			if !c.OK && c.Warn {
				mark = "warn"
			} else if !c.OK {
				mark = "FAIL"
			} else if c.Warn {
				mark = "warn"
			}
			fmt.Printf("[%s] %-14s %s\n", mark, c.Name, c.Detail)
		}
		if doctor.Failed(cs) {
			os.Exit(1)
		}
	case "create-admin":
		if len(os.Args) < 4 {
			log.Fatal("usage: tasklane create-admin EMAIL NAME   (password from TASKLANE_ADMIN_PASSWORD)")
		}
		st := mustOpen(ctx, cfg)
		defer st.Close()
		if cfg.AdminPassword == "" {
			log.Fatal("set TASKLANE_ADMIN_PASSWORD")
		}
		svc := domain.New(st.Pool)
		if _, err := svc.CreateUser(ctx, os.Args[2], strings.Join(os.Args[3:], " "), cfg.AdminPassword, true); err != nil {
			log.Fatal(err)
		}
		fmt.Println("administrator created")
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; commands: serve migrate doctor create-admin version\n", cmd)
		os.Exit(2)
	}
}

func mustOpen(ctx context.Context, cfg config.Config) *store.Store {
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	return st
}

func serve(ctx context.Context, cfg config.Config) {
	st := mustOpen(ctx, cfg)
	defer st.Close()
	// refuse to run against a schema this binary does not match; migrations are an explicit step
	// except for a brand-new database, which is initialized here.
	cur, err := st.SchemaVersion(ctx)
	if err != nil || cur == 0 {
		if _, err := st.Migrate(ctx); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	}
	svc := domain.New(st.Pool)
	if err := svc.EnsureDefaults(ctx); err != nil {
		log.Fatalf("defaults: %v", err)
	}
	if cfg.AdminEmail != "" && cfg.AdminPassword != "" {
		if _, err := svc.CreateUser(ctx, cfg.AdminEmail, "Administrator", cfg.AdminPassword, true); err == nil {
			log.Printf("created administrator %s", cfg.AdminEmail)
		}
	}
	srv := &api.Server{Svc: svc, SessionTTL: cfg.SessionTTL, Secure: strings.HasPrefix(cfg.BaseURL, "https://"),
		Ready: func(ctx context.Context) error { return st.Pool.Ping(ctx) }}
	hs := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("tasklane %s listening on %s", version, cfg.Listen)
		if err := hs.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	shutdown, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hs.Shutdown(shutdown)
}
