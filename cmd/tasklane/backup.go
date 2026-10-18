package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/store"
)

// cliService is the service as a command sees it: the secret key and the blob store as the server would have them.
func cliService(ctx context.Context, cfg config.Config, migrate bool) (*store.Store, *domain.Service) {
	st := mustOpen(ctx, cfg)
	if migrate { // a restore target is a new database: the schema first, then the service that reads settings from it
		if _, err := st.Migrate(ctx); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	}
	svc := domain.New(st.Pool)
	if cfg.SecretKey != "" {
		k, err := hex.DecodeString(cfg.SecretKey)
		if err != nil || len(k) != 32 {
			log.Fatalf("secret_key must be 64 hexadecimal characters (32 bytes)")
		}
		svc.SecretKey = k
	}
	fs, err := blob.NewFromConfig(cfg)
	if err != nil {
		log.Fatalf("blob store: %v", err)
	}
	if err := svc.ConfigureBlobs(ctx, &domain.Blobs{Store: fs}); err != nil {
		log.Fatalf("blobs: %v", err)
	}
	return st, svc
}

// backupCmd is `tasklane backup DIR`: a consistent backup of the database and the attachments into a new directory. The server does
// not need to stop and writers are not held up.
func backupCmd(ctx context.Context, cfg config.Config) {
	if len(os.Args) != 3 {
		log.Fatal("usage: tasklane backup DIR   (DIR must be new or empty; settings come from the environment and the config file)")
	}
	st, svc := cliService(ctx, cfg, false)
	defer st.Close()
	man, err := svc.Backup(ctx, os.Args[2], domain.BackupOpts{Version: releaseVersion(), BlobsEncryptedAtSource: cfg.BlobEncKeys != ""})
	if err != nil {
		log.Fatalf("backup: %v", err)
	}
	var rows int64
	for _, t := range man.Tables {
		rows += t.Rows
	}
	fmt.Printf("backup written to %s\n  schema version %d, taken at %s (log position %s, outbox %d)\n  %d tables, %d rows; %d attachments (%d bytes)\n",
		os.Args[2], man.SchemaVersion, man.Marker.At.Format("2006-01-02 15:04:05Z07:00"), man.Marker.LSN, man.Marker.OutboxID, len(man.Tables), rows, man.Blobs, man.BlobBytes)
	for _, n := range man.Notes {
		fmt.Println("  note: " + n)
	}
	fmt.Println("  the backup holds password hashes, sealed credentials and the attachments: keep it as carefully as the database; restoring needs the same secret_key")
	if len(man.BlobsMissing) > 0 {
		fmt.Printf("  WARNING: %d attachment(s) the database refers to were missing or damaged in the blob store and are not in the backup (run `tasklane fsck`)\n", len(man.BlobsMissing))
		os.Exit(1)
	}
}

// verifyBackupCmd is `tasklane verify-backup DIR`: every file against the manifest, without touching a database.
func verifyBackupCmd() {
	if len(os.Args) != 3 {
		log.Fatal("usage: tasklane verify-backup DIR")
	}
	man, err := domain.VerifyBackup(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("ok: format %s version %d, schema %d, %d tables, %d attachments, made %s by %s\n", man.Format, man.Version, man.SchemaVersion, len(man.Tables), man.Blobs, man.CreatedAt.Format("2006-01-02 15:04:05Z07:00"), man.TasklaneVersion)
}

// restoreCmd is `tasklane restore DIR`: into an empty database (it is migrated first). To replace a running instance, restore into a
// new database and point the servers at it.
func restoreCmd(ctx context.Context, cfg config.Config) {
	if len(os.Args) != 3 {
		log.Fatal("usage: tasklane restore DIR   (into the empty database named by database_url; stop the servers that use it)")
	}
	if _, err := domain.VerifyBackup(os.Args[2]); err != nil { // before the database is touched
		log.Fatalf("restore: %v", err)
	}
	st, svc := cliService(ctx, cfg, true)
	defer st.Close()
	rep, err := svc.Restore(ctx, os.Args[2])
	if err != nil {
		log.Fatalf("restore: %v", err)
	}
	fmt.Printf("restored %d tables (%d rows) and %d attachments; the backup was made %s by %s at outbox position %d\n",
		rep.Tables, rep.Rows, rep.Blobs, rep.BackupMade.Format("2006-01-02 15:04:05Z07:00"), rep.FromVersion, rep.Marker.OutboxID)
	fmt.Printf("  audit chain verifies: %v; attachments complete: %v\n", rep.AuditOK, rep.FsckClean)
	for _, w := range rep.Warnings {
		fmt.Println("  note: " + w)
	}
	if !rep.AuditOK || !rep.FsckClean {
		os.Exit(3)
	}
}
