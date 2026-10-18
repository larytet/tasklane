// Command tasklane runs the server and its maintenance subcommands.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/doctor"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/mcp"
	"github.com/larytet/tasklane/internal/rt"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/store"
)

var version = "0.1.0-dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx := context.Background()
	if cmd == "config" { // config print | config docs
		sub := ""
		if len(os.Args) > 2 {
			sub = os.Args[2]
		}
		switch sub {
		case "docs":
			fmt.Print(config.Docs())
		case "print":
			es, err := config.Resolve(config.Options{Args: os.Args[3:]})
			fmt.Print(config.Print(es)) // secrets are masked
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		default:
			fmt.Fprintln(os.Stderr, "usage: tasklane config print | tasklane config docs")
			os.Exit(2)
		}
		return
	}
	cfg, err := config.Load(config.Options{Args: os.Args[min(2, len(os.Args)):]})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch cmd {
	case "serve":
		serve(ctx, cfg)
	case "migrate":
		st := mustOpen(ctx, cfg)
		defer st.Close()
		if len(os.Args) > 2 && os.Args[2] == "plan" {
			plan, err := st.Plan(ctx)
			if err != nil {
				log.Fatal(err)
			}
			if len(plan) == 0 {
				fmt.Println("the schema is up to date; nothing to apply")
			}
			for _, p := range plan {
				fmt.Printf("would apply %04d %s  [%s%s]\n", p.Version, p.Name, p.Phase, map[bool]string{true: ", no transaction", false: ""}[p.NoTx])
			}
			return
		}
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
	case "mcp": // stdio transport: proxies JSON-RPC lines to the server with the user's token
		url := os.Getenv("TASKLANE_URL")
		if url == "" {
			url = "http://localhost:8080"
		}
		tok := os.Getenv("TASKLANE_TOKEN")
		if tok == "" {
			fmt.Fprintln(os.Stderr, "set TASKLANE_TOKEN to an agent token (and TASKLANE_URL to the server)")
			os.Exit(2)
		}
		if err := mcp.ServeStdio(os.Stdin, os.Stdout, strings.TrimRight(url, "/")+"/mcp", tok); err != nil {
			log.Fatal(err)
		}
	case "reindex":
		st := mustOpen(ctx, cfg)
		defer st.Close()
		if err := search.ReindexAll(ctx, st.Pool); err != nil {
			log.Fatal(err)
		}
		fmt.Println("search index rebuilt")
	case "fsck":
		st := mustOpen(ctx, cfg)
		defer st.Close()
		svc := domain.New(st.Pool)
		fs, err := blob.NewFS(cfg.BlobDir)
		if err != nil {
			log.Fatal(err)
		}
		if err := svc.ConfigureBlobs(ctx, &domain.Blobs{Store: fs}); err != nil {
			log.Fatal(err)
		}
		rep, err := svc.Fsck(ctx, len(os.Args) > 2 && os.Args[2] == "--repair")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("checked %d objects: %d missing, %d orphaned, %d corrupt\n", rep.Checked, len(rep.Missing), len(rep.Orphans), len(rep.Corrupt))
		if !rep.Clean() {
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
		fmt.Fprintf(os.Stderr, "unknown command %q; commands: serve migrate doctor reindex fsck config create-admin version\n", cmd)
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
	fs, err := blob.NewFS(cfg.BlobDir)
	if err != nil {
		log.Fatalf("blob store: %v", err)
	}
	if err := svc.ConfigureBlobs(ctx, &domain.Blobs{Store: fs, Retention: 7 * 24 * time.Hour}); err != nil {
		log.Fatalf("blobs: %v", err)
	}
	go svc.RunBlobWorkers(ctx, log.Printf)
	go svc.RunLifecycle(ctx, log.Printf)
	if cfg.SMTPHost != "" {
		dom := "tasklane.local"
		if u, err := url.Parse(cfg.BaseURL); err == nil && u.Hostname() != "" {
			dom = u.Hostname()
		}
		svc.Mail = &domain.MailConfig{
			Mailer: &mail.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, TLS: cfg.SMTPTLS, User: cfg.SMTPUser, Pass: cfg.SMTPPass, From: cfg.SMTPFrom},
			From:   cfg.SMTPFrom, Domain: dom, BaseURL: cfg.BaseURL,
		}
		go svc.RunMail(ctx, log.Printf)
	} else {
		log.Printf("outbound email is off (set TASKLANE_SMTP_HOST to enable notifications and account mail)")
	}
	if cfg.AdminEmail != "" && cfg.AdminPassword != "" {
		if _, err := svc.CreateUser(ctx, cfg.AdminEmail, "Administrator", cfg.AdminPassword, true); err == nil {
			log.Printf("created administrator %s", cfg.AdminEmail)
		}
	}
	ixCtx, stopIx := context.WithCancel(ctx)
	defer stopIx()
	go (&search.Indexer{DB: st.Pool}).Run(ixCtx)
	hub := rt.New(svc)
	go hub.SweepLoop(ctx)
	srv := &api.Server{Hub: hub, Svc: svc, SessionTTL: cfg.SessionTTL, Secure: strings.HasPrefix(cfg.BaseURL, "https://"),
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
