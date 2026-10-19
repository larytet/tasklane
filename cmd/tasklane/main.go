// Command tasklane runs the server and its maintenance subcommands.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"github.com/larytet/tasklane/internal/egress"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/doctor"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/mcp"
	"github.com/larytet/tasklane/internal/redis"
	"github.com/larytet/tasklane/internal/rt"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/store"
)

var version = "0.1.0-dev"

// versionMarker is set by the release build with the version between a fixed label and a semicolon, so that `tasklane
// verify-release` can read the version out of a binary without running it (Go does not record linker flags in the build
// information). `tasklane version` prints it, which also keeps the linker from discarding it.
var versionMarker = "tasklane-release-version=<none>;" // a build outside the release script: no marker a verifier would accept

func releaseVersion() string {
	v := strings.TrimSuffix(strings.TrimPrefix(versionMarker, "tasklane-release-version="), ";")
	if strings.HasPrefix(v, "<") {
		return version
	}
	return v
}

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
	if cmd == "verify-backup" { // checks files against a manifest: no configuration, no database
		verifyBackupCmd()
		return
	}
	if cmd == "verify-release" { // check a downloaded release before installing it; it needs no configuration and no database
		if len(os.Args) != 4 {
			fmt.Fprintln(os.Stderr, "usage: tasklane verify-release PUBLIC_KEY_FILE RELEASE_DIR")
			os.Exit(2)
		}
		os.Exit(verifyRelease(os.Args[2], os.Args[3]))
	}
	cfgArgs := os.Args[min(2, len(os.Args)):]
	if cmd == "blobs" || cmd == "migrate" || cmd == "backup" || cmd == "restore" || cmd == "create-admin" || cmd == "upgrade" {
		cfgArgs = nil // their words (migrate, verify, rewrap, plan, finalize, --verify) are the command's own; settings come from the environment and the file
	}
	cfg, err := config.Load(config.Options{Args: cfgArgs})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	switch cmd {
	case "backup":
		backupCmd(ctx, cfg)
	case "restore":
		restoreCmd(ctx, cfg)
	case "serve":
		serve(ctx, cfg)
	case "upgrade":
		switch {
		case len(os.Args) >= 3 && os.Args[2] == "preflight":
			upgradePreflightCmd(ctx, cfg)
		case len(os.Args) >= 3 && os.Args[2] == "wait":
			upgradeWaitCmd(ctx, cfg)
		default:
			log.Fatal("usage: tasklane upgrade preflight [--backup DIR] | tasklane upgrade wait [--timeout DURATION]")
		}
	case "migrate":
		st := mustOpen(ctx, cfg)
		defer st.Close()
		if len(os.Args) > 2 && os.Args[2] == "finalize" {
			if err := finalizeGuard(ctx, st, len(os.Args) > 3 && os.Args[3] == "--force"); err != nil {
				log.Fatal(err)
			}
			applied, err := st.Finalize(ctx)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("applied %d migration(s) %v; the previous release can no longer run on this database\n", len(applied), applied)
			return
		}
		if len(os.Args) > 2 && os.Args[2] == "plan" {
			plan, err := st.Plan(ctx)
			if err != nil {
				log.Fatal(err)
			}
			if len(plan) == 0 {
				fmt.Println("the schema is up to date; nothing to apply")
			}
			for _, p := range plan {
				fmt.Printf("would apply %04d %s  [%s%s%s]\n", p.Version, p.Name, p.Phase, map[bool]string{true: ", no transaction", false: ""}[p.NoTx], map[bool]string{true: ", held until `migrate finalize`", false: ""}[p.Held])
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
		fs, err := blob.NewFromConfig(cfg)
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
	case "blobs":
		blobsCmd(ctx, cfg)
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
		fmt.Println(releaseVersion())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q; commands: serve migrate backup restore verify-backup doctor reindex fsck blobs config create-admin verify-release version\n", cmd)
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
	verdict, err := st.CheckCompatWaiting(ctx, store.DefaultPolicy(), 5*time.Minute) // nodes started together: the others wait for the one that migrates
	if err != nil {
		log.Fatalf("refusing to start: %v", err)
	}
	if verdict.Fresh {
		if _, err := st.Migrate(ctx); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	}
	if verdict.Window {
		log.Printf("warning: %s", verdict.Note)
	}
	svc := domain.New(st.Pool)
	svc.WebhookAllowPrivate = cfg.WebhookAllowPrivate
	svc.PasswordMinLength = int(cfg.PasswordMinLength)
	svc.BaseURL, svc.SSOAllowPrivate = cfg.BaseURL, cfg.SSOAllowPrivate
	if cfg.SecretKey != "" {
		k, err := hex.DecodeString(cfg.SecretKey)
		if err != nil || len(k) != 32 {
			log.Fatalf("secret_key must be 64 hexadecimal characters (32 bytes)")
		}
		svc.SecretKey = k
	}
	if err := svc.EnsureDefaults(ctx); err != nil {
		log.Fatalf("defaults: %v", err)
	}
	fs, err := blob.NewFromConfig(cfg)
	if err != nil {
		log.Fatalf("blob store: %v", err)
	}
	if err := svc.ConfigureBlobs(ctx, &domain.Blobs{Store: fs, Retention: 7 * 24 * time.Hour}); err != nil {
		log.Fatalf("blobs: %v", err)
	}
	go svc.RunBlobWorkers(ctx, log.Printf)
	go svc.RunLifecycle(ctx, log.Printf)
	go svc.RunTierWorker(ctx, 10*time.Minute, log.Printf)
	go svc.RunGitWorkers(ctx, log.Printf)
	go svc.RunJiraWorkers(ctx, log.Printf)
	go svc.RunWorkflowJobs(ctx, log.Printf)
	go svc.RunNotifier(ctx, log.Printf)
	go svc.RunApprovals(ctx, log.Printf)
	go svc.RunSLA(ctx, log.Printf)
	go svc.RunMetering(ctx, log.Printf)
	go svc.RunMaintenance(ctx, log.Printf)
	go svc.RunBilling(ctx, log.Printf)
	go svc.RunWorkflowMigrations(ctx, log.Printf)
	dom := "tasklane.local"
	if u, err := url.Parse(cfg.BaseURL); err == nil && u.Hostname() != "" {
		dom = u.Hostname()
	}
	host, _ := os.Hostname()
	nb := make([]byte, 4)
	rand.Read(nb)
	if cfg.ClockSkew != "" { // fault injection: the node's own clock is wrong on purpose; SyncClock right after corrects it as it would a real one
		d, err := time.ParseDuration(cfg.ClockSkew)
		if err != nil {
			log.Fatalf("fault_clock_skew: %v", err)
		}
		svc.Clock = func() time.Time { return time.Now().Add(d) }
		log.Printf("warning: fault injection is on: this node's clock reads %s off the truth (fault_clock_skew)", d)
	}
	if off, err := svc.SyncClock(ctx); err != nil {
		log.Printf("warning: could not compare this machine's clock with the database's: %v", err)
	} else if off > domain.ClockWarnAt || off < -domain.ClockWarnAt {
		log.Printf("warning: this machine's clock is %s from the database's; times are corrected, but fix the machine's time synchronisation", off.Round(time.Second))
	}
	svc.WebAuthnRPID, svc.WebAuthnOrigins, svc.WebAuthnRequireUV = cfg.WebAuthnRPID, cfg.WebAuthnOrigins, cfg.WebAuthnRequireUV
	svc.Hosted, svc.HostedRegion = cfg.Hosted, cfg.HostedRegion
	if cfg.Hosted && cfg.BillingURL != "" {
		svc.Billing = domain.WebhookBilling{URL: cfg.BillingURL, Secret: cfg.BillingSecret, AllowPrivate: cfg.WebhookAllowPrivate}
	}
	for _, c := range cfg.TrustedProxies {
		if _, n, err := net.ParseCIDR(c); err == nil {
			svc.TrustedProxies = append(svc.TrustedProxies, n)
		} else if ip := net.ParseIP(c); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			svc.TrustedProxies = append(svc.TrustedProxies, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		} else {
			log.Fatalf("trusted_proxies: %q is not an address or a network", c)
		}
	}
	svc.DashboardIframeHosts = cfg.DashboardIframeHosts
	svc.NodeID, svc.NodeVersion = fmt.Sprintf("%s-%d-%x", host, os.Getpid(), nb), releaseVersion()
	if err := svc.CheckNodeAgainstGates(ctx, svc.NodeVersion); err != nil { // rolling back past an activated behaviour is refused
		log.Fatalf("refusing to start: %v", err)
	}
	go svc.RunCluster(ctx, log.Printf)
	go svc.RunPauseAcks(ctx)
	svc.IMAPAllowPrivate = cfg.IMAPAllowPrivate
	svc.MailRawDays, svc.MailBounceDays, svc.MailRetentionSet = cfg.MailRawDays, cfg.MailBounceDays, true
	go svc.RunMailRetentionWorker(ctx, time.Hour, log.Printf)
	go svc.RunIMAP(ctx, log.Printf)
	if cfg.SMTPListen != "" {
		var tc *tls.Config
		if cfg.SMTPListenCert != "" || cfg.SMTPListenKey != "" {
			cert, err := tls.LoadX509KeyPair(cfg.SMTPListenCert, cfg.SMTPListenKey)
			if err != nil {
				log.Fatalf("smtp_listen_tls_cert and smtp_listen_tls_key: %v", err)
			}
			tc = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		}
		go func() {
			if err := svc.RunSMTPListener(ctx, cfg.SMTPListen, tc, log.Printf); err != nil {
				log.Printf("smtp listener: %v", err)
			}
		}()
	}
	svc.DNS = net.DefaultResolver // DKIM keys are looked up in DNS
	svc.InboundDomain = dom       // mail can be received (a webhook source) without this server sending any
	if cfg.SMTPHost != "" {
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
	pgEngine := &search.Postgres{DB: st.Pool}
	var searcher search.Searcher = pgEngine
	var searchAdmin *search.Admin
	if cfg.SearchEngine == "opensearch" {
		if cfg.OSURL == "" {
			log.Fatalf("search_engine=opensearch needs opensearch_url")
		}
		oc := &search.OSClient{BaseURL: cfg.OSURL, User: cfg.OSUser, Password: cfg.OSPassword, APIKey: cfg.OSAPIKey,
			HTTP: egress.Policy{AllowPrivate: cfg.OSAllowPrivate}.Client()}
		oix := &search.OSIndex{DB: st.Pool, C: oc, Alias: cfg.OSIndex, Shards: int(cfg.OSShards), Replicas: int(cfg.OSReplicas)}
		router := &search.Router{PG: pgEngine, OS: &search.OpenSearch{C: oc, PG: pgEngine, Alias: cfg.OSIndex}, Index: oix}
		searcher, searchAdmin = router, &search.Admin{Index: oix, Router: router}
		go func() { // the cluster may come up after us: keep trying, searches fall back to PostgreSQL meanwhile
			for ixCtx.Err() == nil {
				if err := oix.Ensure(ixCtx); err != nil {
					log.Printf("search index: %v (retrying; searches use PostgreSQL meanwhile)", err)
					time.Sleep(10 * time.Second)
					continue
				}
				oix.Run(ixCtx, log.Printf)
			}
		}()
		go searchAdmin.RunMaintenance(ixCtx, 6*time.Hour, log.Printf)
	}
	svc.Matcher = search.Matcher{Engine: pgEngine, Now: svc.Now} // automation reads PostgreSQL, never a lagging copy
	go svc.RunAutomation(ctx, log.Printf)
	go sprintJobs(ctx, svc)
	var bus *redis.Bus
	if cfg.RedisURL != "" { // live events and presence between nodes; see internal/domain/bus.go
		o, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			log.Fatal(err)
		}
		bus = redis.New(o)
		svc.AttachBus(domain.RedisBus(bus))
		go bus.Run(ctx)
	}
	var hub *rt.Hub
	if cfg.Realtime {
		hub = rt.New(svc)
		svc.OnRemotePresence = hub.BroadcastPresence
		go hub.SweepLoop(ctx)
	} else { // no socket to tell anyone: presence that missed its heartbeats is still removed
		log.Printf("realtime is switched off: pages work without live updates")
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					svc.Presence.Sweep()
				}
			}
		}()
	}
	degraded := func() string {
		if st, ok := svc.BusLink(); ok && !st.Connected {
			return "the cluster event bus (redis_url) is unreachable: live updates and presence from other nodes are missing until it returns"
		}
		return ""
	}
	srv := &api.Server{Degraded: degraded, Version: svc.NodeVersion, MinClient: cfg.MinClient, Hub: hub, Svc: svc, Search: searcher, SearchAdmin: searchAdmin, SessionTTL: cfg.SessionTTL, Secure: strings.HasPrefix(cfg.BaseURL, "https://"),
		Ready: func(ctx context.Context) error {
			if err := st.Pool.Ping(ctx); err != nil {
				return err
			}
			return blobReady(fs)
		}}
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
	// a graceful stop (SPEC 17.8): readiness fails first so a load balancer moves new requests elsewhere, open pages are told to
	// reconnect (spread out), the node is marked as draining, and only after the delay does the listener close and wait for what is in
	// flight
	log.Printf("stopping: readiness now fails; draining for %s", cfg.DrainDelay)
	srv.Drain()
	svc.Drain(ctx)
	if hub != nil {
		go hub.Drain(time.Second)
	}
	time.Sleep(cfg.DrainDelay)
	shutdown, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hs.Shutdown(shutdown)
	svc.LeaveCluster(context.Background())
	log.Printf("stopped")
}

// sprintJobs is the scheduler for auto-sprints and auto-rollover. Every node runs it: each step re-checks state under a lock.
func sprintJobs(ctx context.Context, svc *domain.Service) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if rolled, created, err := svc.RunSprintJobs(ctx); err != nil {
			log.Printf("sprint jobs: %v", err)
		} else if rolled+created > 0 {
			log.Printf("sprint jobs: rolled over %d, created %d", rolled, created)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// blobReady reports the blob store's health for /readyz. An S3 check is a network round trip, so the answer is kept
// for fifteen seconds; the filesystem check is cheap and always fresh.
var blobHealth struct {
	sync.Mutex
	at  time.Time
	err error
}

func blobReady(st blob.Store) error {
	if _, ok := st.(*blob.S3); !ok {
		return st.WriteCheck()
	}
	blobHealth.Lock()
	defer blobHealth.Unlock()
	if time.Since(blobHealth.at) > 15*time.Second {
		blobHealth.err, blobHealth.at = st.WriteCheck(), time.Now()
	}
	return blobHealth.err
}
