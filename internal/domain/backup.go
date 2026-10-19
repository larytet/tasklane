package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Backup and restore (SPEC 17.4, 18.2). A backup is a directory:
//
//	manifest.json      what it is: format version, the schema version it was taken at, the marker, every table with its row count and checksum
//	data/<table>.copy  each table, as PostgreSQL's COPY text format, all taken from ONE consistent database snapshot
//	blobs/ab/cd/<sha>  the attachments the snapshot refers to, copied AFTER the snapshot (so the restored database never refers to a
//	                   file the backup lacks; extra files are harmless)
//
// The database part is read in a single repeatable-read transaction: it sees the database as it was at one instant (the marker), and
// writers are not held up. While a backup runs, the blob garbage collector is held off (an advisory lock), so a file the snapshot
// refers to cannot be deleted before it is copied. A restore goes into an empty database, brings the blobs in first and the rows
// after, keeps every identifier and the audit chain exactly as they were, resets the sequences, and checks the result against the
// manifest.
//
// What is not in it, on purpose: sessions and sign-in state (everyone signs in again), cluster nodes, pauses, upload sessions. What it
// holds that you must protect: credentials sealed with the instance secret key (restore needs the same key), password hashes, and
// attachments in clear (when the source encrypts them at rest, the backup does not): keep a backup as carefully as the database.

const (
	BackupFormat  = "tasklane-backup"
	BackupVersion = 1
	// backupLockKey is the advisory lock a backup holds while it copies files, and the blob garbage collector tries.
	backupLockKey int64 = 0x7461736b6c616e65
)

// backupSkip are the tables a backup leaves out: they describe the moment of the backup, not the data.
var backupSkip = map[string]bool{
	"schema_migration": true, "session": true, "mfa_pending": true, "cluster_node": true, "webauthn_challenge": true,
	"login_attempt": true, "upload_session": true, "direct_upload": true, "oauth_code": true, "sso_state": true, "pause_lease": true,
}

type BackupMarker struct {
	OutboxID int64     `json:"outbox_id"`
	LSN      string    `json:"lsn"`
	At       time.Time `json:"at"`
	Mode     string    `json:"mode"`
}

type BackupTable struct {
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
	File    string   `json:"file"`
	SHA256  string   `json:"sha256"`
	Bytes   int64    `json:"bytes"`
}

type BackupSequence struct {
	Last *int64 `json:"last_value"` // null: never used
}

type BackupManifest struct {
	Format          string                    `json:"format"`
	Version         int                       `json:"version"`
	CreatedAt       time.Time                 `json:"created_at"`
	TasklaneVersion string                    `json:"tasklane_version"`
	SchemaVersion   int                       `json:"schema_version"`
	Marker          BackupMarker              `json:"marker"`
	Tables          map[string]BackupTable    `json:"tables"`
	Sequences       map[string]BackupSequence `json:"sequences"`
	Blobs           int                       `json:"blobs"`
	BlobBytes       int64                     `json:"blob_bytes"`
	BlobsMissing    []string                  `json:"blobs_missing,omitempty"` // the database refers to them; the source store did not have them
	SecretCheck     string                    `json:"secret_check"`            // a value sealed with the instance secret key: restore says if the key differs
	Notes           []string                  `json:"notes,omitempty"`
}

// BackupOpts: Version is the running release; BlobsEncryptedAtSource makes the manifest say that the attachments are in clear here.
type BackupOpts struct {
	Version                string
	BlobsEncryptedAtSource bool
}

func (s *Service) backupTables(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		if !backupSkip[n] {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}

func columnsOf(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, table string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT a.attname FROM pg_attribute a WHERE a.attrelid = (quote_ident(current_schema())||'.'||quote_ident($1))::regclass AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = '' ORDER BY a.attnum`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func quoteIdents(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = pgx.Identifier{n}.Sanitize()
	}
	return strings.Join(q, ", ")
}

type hashWriter struct {
	w io.Writer
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
	n int64
}

func (h *hashWriter) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}

func blobPath(dir, sha string) string { return filepath.Join(dir, "blobs", sha[:2], sha[2:4], sha) }

// Backup writes a backup into dir (which must not hold one already). It never holds writers up.
func (s *Service) Backup(ctx context.Context, dir string, o BackupOpts) (*BackupManifest, error) {
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
		return nil, invalid("%s already holds a backup; choose an empty directory", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o750); err != nil {
		return nil, err
	}
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	// held until the files are copied: the blob garbage collector does not delete while a backup is reading
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, backupLockKey); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, backupLockKey)

	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	man := &BackupManifest{Format: BackupFormat, Version: BackupVersion, CreatedAt: s.Now(), TasklaneVersion: o.Version, Tables: map[string]BackupTable{}, Sequences: map[string]BackupSequence{}}
	// the first statement fixes the snapshot: everything below sees the database as it is here
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT max(id) FROM outbox),0), (CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END)::text, clock_timestamp(), COALESCE((SELECT max(version) FROM schema_migration),0)`).
		Scan(&man.Marker.OutboxID, &man.Marker.LSN, &man.Marker.At, &man.SchemaVersion); err != nil {
		return nil, err
	}
	man.Marker.Mode = "snapshot"
	if s.BackupHook != nil {
		s.BackupHook("snapshot")
	}
	tables, err := s.backupTables(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, t := range tables {
		cols, err := columnsOf(ctx, tx, t)
		if err != nil {
			return nil, err
		}
		file := "data/" + t + ".copy"
		f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, err
		}
		hw := &hashWriter{w: f, h: sha256.New()}
		tag, err := tx.Conn().PgConn().CopyTo(ctx, hw, fmt.Sprintf(`COPY (SELECT %s FROM %s) TO STDOUT`, quoteIdents(cols), pgx.Identifier{t}.Sanitize()))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", t, err)
		}
		man.Tables[t] = BackupTable{Columns: cols, Rows: tag.RowsAffected(), File: file, SHA256: hex.EncodeToString(hw.h.Sum(nil)), Bytes: hw.n}
	}
	seqRows, err := tx.Query(ctx, `SELECT sequencename, last_value FROM pg_sequences WHERE schemaname=current_schema() ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	for seqRows.Next() {
		var n string
		var last *int64
		seqRows.Scan(&n, &last)
		man.Sequences[n] = BackupSequence{Last: last}
	}
	seqRows.Close()
	type blobRef struct {
		sha  string
		size int64
	}
	var refs []blobRef
	if s.Blobs != nil {
		br, err := tx.Query(ctx, `SELECT sha256,size FROM blob ORDER BY sha256`)
		if err != nil {
			return nil, err
		}
		for br.Next() {
			var r blobRef
			br.Scan(&r.sha, &r.size)
			refs = append(refs, r)
		}
		br.Close()
	}
	// the secret-key check: a value sealed now; a restore that cannot open it was given a different key
	if sealed, err := s.seal(ctx, "tasklane-backup-check"); err == nil {
		man.SecretCheck = hex.EncodeToString(sealed)
	}
	tx.Rollback(ctx) // the snapshot is done; the files below are copied after it, with the garbage collector held off

	// ---- the files, after the snapshot
	if s.Blobs == nil {
		if len(refs) == 0 {
			man.Notes = append(man.Notes, "no blob store was configured: attachments are not in this backup")
		}
	} else {
		for _, r := range refs {
			o, err := s.Blobs.Store.Open(r.sha)
			if err != nil {
				man.BlobsMissing = append(man.BlobsMissing, r.sha)
				continue
			}
			p := blobPath(dir, r.sha)
			os.MkdirAll(filepath.Dir(p), 0o750)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
			if err != nil {
				o.Close()
				return nil, err
			}
			h := sha256.New()
			n, cerr := io.Copy(io.MultiWriter(f, h), o)
			o.Close()
			if f.Close() != nil || cerr != nil || hex.EncodeToString(h.Sum(nil)) != r.sha {
				os.Remove(p)
				man.BlobsMissing = append(man.BlobsMissing, r.sha) // damaged at the source: not copied, and said so
				continue
			}
			man.Blobs++
			man.BlobBytes += n
		}
		if o.BlobsEncryptedAtSource {
			man.Notes = append(man.Notes, "the attachments are encrypted at rest in the source but are stored here in clear: protect this backup accordingly")
		}
		sort.Strings(man.BlobsMissing)
	}
	b, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o640); err != nil { // last: a directory without it is not a backup
		return nil, err
	}
	return man, nil
}

type RestoreReport struct {
	Tables      int          `json:"tables"`
	Rows        int64        `json:"rows"`
	Blobs       int          `json:"blobs"`
	Marker      BackupMarker `json:"marker"`
	AuditOK     bool         `json:"audit_chain_ok"`
	FsckClean   bool         `json:"blobs_clean"`
	Warnings    []string     `json:"warnings"`
	BackupMade  time.Time    `json:"backup_made"`
	FromVersion string       `json:"from_version"`
}

func readBackupManifest(dir string) (*BackupManifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, invalid("%s is not a backup: manifest.json is missing (a backup is written manifest last, so an interrupted one has none)", dir)
	}
	var m BackupManifest
	if err := json.Unmarshal(b, &m); err != nil || m.Format != BackupFormat {
		return nil, invalid("%s is not a Tasklane backup: the manifest is unreadable", dir)
	}
	if m.Version > BackupVersion {
		return nil, invalid("this backup is format version %d, newer than this release understands (%d): restore it with a newer release", m.Version, BackupVersion)
	}
	return &m, nil
}

// VerifyBackup checks a backup without restoring it: the manifest, every table's checksum and every attachment's.
func VerifyBackup(dir string) (*BackupManifest, error) {
	m, err := readBackupManifest(dir)
	if err != nil {
		return nil, err
	}
	for name, t := range m.Tables {
		f, err := os.Open(filepath.Join(dir, t.File))
		if err != nil {
			return m, invalid("the backup is incomplete: %s is missing", t.File)
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil || hex.EncodeToString(h.Sum(nil)) != t.SHA256 || n != t.Bytes {
			return m, invalid("the backup is damaged: table %s does not match its checksum", name)
		}
	}
	missing := map[string]bool{}
	for _, s := range m.BlobsMissing {
		missing[s] = true
	}
	found := 0
	err = filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		h := sha256.New()
		io.Copy(h, f)
		f.Close()
		if hex.EncodeToString(h.Sum(nil)) != d.Name() {
			return invalid("the backup is damaged: attachment %s does not match its name", d.Name())
		}
		found++
		return nil
	})
	if err != nil {
		return m, err
	}
	if found != m.Blobs {
		return m, invalid("the backup lists %d attachments and holds %d", m.Blobs, found)
	}
	return m, nil
}

// fkOrder sorts tables so that a table comes after the tables it refers to.
func fkOrder(ctx context.Context, db *pgxpool.Pool, tables []string) ([]string, error) {
	rows, err := db.Query(ctx, `SELECT c.conrelid::regclass::text, c.confrelid::regclass::text FROM pg_constraint c WHERE c.contype='f' AND c.connamespace=current_schema()::regnamespace AND c.conrelid<>c.confrelid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	in := map[string]bool{}
	for _, t := range tables {
		in[t] = true
	}
	parents := map[string]map[string]bool{}
	clean := func(n string) string { return strings.Trim(n[strings.LastIndex(n, ".")+1:], `"`) }
	for rows.Next() {
		var child, parent string
		rows.Scan(&child, &parent)
		child, parent = clean(child), clean(parent)
		if in[child] && in[parent] {
			if parents[child] == nil {
				parents[child] = map[string]bool{}
			}
			parents[child][parent] = true
		}
	}
	var out []string
	done := map[string]bool{}
	var visit func(t string, stack map[string]bool) error
	visit = func(t string, stack map[string]bool) error {
		if done[t] {
			return nil
		}
		if stack[t] {
			return fmt.Errorf("tables refer to each other in a cycle through %s", t)
		}
		stack[t] = true
		ps := make([]string, 0, len(parents[t]))
		for p := range parents[t] {
			ps = append(ps, p)
		}
		sort.Strings(ps)
		for _, p := range ps {
			if err := visit(p, stack); err != nil {
				return err
			}
		}
		delete(stack, t)
		done[t] = true
		out = append(out, t)
		return nil
	}
	sorted := append([]string(nil), tables...)
	sort.Strings(sorted)
	for _, t := range sorted {
		if err := visit(t, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

// Restore loads a backup into an EMPTY database (the schema must be migrated: the command does that first). The backup's schema may be
// older than this release's (added columns take their defaults) but not newer.
func (s *Service) Restore(ctx context.Context, dir string) (*RestoreReport, error) {
	return s.restore(ctx, dir, false)
}

func (s *Service) restore(ctx context.Context, dir string, replace bool) (*RestoreReport, error) {
	man, err := VerifyBackup(dir)
	if err != nil {
		return nil, err
	}
	rep := &RestoreReport{Marker: man.Marker, BackupMade: man.CreatedAt, FromVersion: man.TasklaneVersion, Warnings: []string{}}
	var schema int
	s.DB.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM schema_migration`).Scan(&schema)
	if man.SchemaVersion > schema {
		return nil, invalid("this backup was taken at schema version %d and this database is at %d: restore with the release that made it, or a newer one", man.SchemaVersion, schema)
	}
	var users, issues int
	s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM issue)`).Scan(&users, &issues)
	if !replace && (users > 0 || issues > 0) {
		return nil, invalid("this database is not empty (%d people, %d issues): restore into a new, empty database and switch to it", users, issues)
	}
	have, err := s.backupTables(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	haveSet := map[string]bool{}
	for _, t := range have {
		haveSet[t] = true
	}
	var names []string
	for t := range man.Tables {
		if !haveSet[t] {
			return nil, invalid("the backup has a table, %s, that this release no longer has: restore with the release that made it", t)
		}
		names = append(names, t)
	}
	order, err := fkOrder(ctx, s.DB, names)
	if err != nil {
		return nil, err
	}
	// ---- the attachments first: the rows that refer to them come after
	if man.Blobs > 0 {
		if s.Blobs == nil {
			return nil, invalid("this backup holds %d attachments: configure the blob store before restoring", man.Blobs)
		}
		err = filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			id, w, err := s.Blobs.Store.NewStaging()
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, f); err != nil {
				w.Close()
				return err
			}
			w.Close()
			if _, err := s.Blobs.Store.Commit(id, d.Name()); err != nil {
				return err
			}
			rep.Blobs++
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("attachments: %w", err)
		}
	}
	// ---- the rows
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// the triggers that guard live data (the audit chain computing hashes, same-organization checks, the email domain rule) must not
	// rewrite or refuse what is being put back exactly as it was: switched off for the load, on again before the commit
	trg, err := tx.Query(ctx, `SELECT DISTINCT c.relname FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid WHERE NOT t.tgisinternal AND c.relnamespace=current_schema()::regnamespace`)
	if err != nil {
		return nil, err
	}
	var guarded []string
	for trg.Next() {
		var n string
		trg.Scan(&n)
		guarded = append(guarded, n)
	}
	trg.Close()
	for _, t := range guarded {
		if _, err := tx.Exec(ctx, `ALTER TABLE `+pgx.Identifier{t}.Sanitize()+` DISABLE TRIGGER USER`); err != nil {
			return nil, err
		}
	}
	all := make([]string, len(have))
	copy(all, have)
	if _, err := tx.Exec(ctx, `TRUNCATE `+quoteIdents(all)+` CASCADE`); err != nil { // what migrations seeded (the default organization) is replaced by the backup's
		return nil, err
	}
	for _, t := range order {
		bt := man.Tables[t]
		f, err := os.Open(filepath.Join(dir, bt.File))
		if err != nil {
			return nil, err
		}
		tag, err := tx.Conn().PgConn().CopyFrom(ctx, f, fmt.Sprintf(`COPY %s (%s) FROM STDIN`, pgx.Identifier{t}.Sanitize(), quoteIdents(bt.Columns)))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", t, err)
		}
		if tag.RowsAffected() != bt.Rows {
			return nil, fmt.Errorf("table %s: restored %d rows, the backup says %d", t, tag.RowsAffected(), bt.Rows)
		}
		rep.Tables++
		rep.Rows += tag.RowsAffected()
	}
	for name, sq := range man.Sequences {
		if sq.Last == nil {
			continue
		}
		if _, err := tx.Exec(ctx, `SELECT setval($1::regclass, $2, true)`, pgx.Identifier{name}.Sanitize(), *sq.Last); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("sequence %s: %v", name, err))
		}
	}
	for _, t := range guarded {
		if _, err := tx.Exec(ctx, `ALTER TABLE `+pgx.Identifier{t}.Sanitize()+` ENABLE TRIGGER USER`); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// ---- checks against what the manifest promised
	var outbox int64
	s.DB.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM outbox`).Scan(&outbox)
	if outbox != man.Marker.OutboxID {
		return rep, fmt.Errorf("the restored outbox ends at %d, the marker says %d", outbox, man.Marker.OutboxID)
	}
	if c, err := s.VerifyAudit(ctx, Actor{Admin: true}); err == nil {
		rep.AuditOK = c.OK
		if !c.OK {
			rep.Warnings = append(rep.Warnings, "the audit chain does not verify after the restore: "+c.Problem)
		}
	}
	rep.FsckClean = true
	if s.Blobs != nil {
		fr, err := s.Fsck(ctx, false)
		if err != nil {
			rep.Warnings = append(rep.Warnings, "could not check the attachments: "+err.Error())
			rep.FsckClean = false
		} else if len(fr.Missing) > 0 || len(fr.Corrupt) > 0 {
			rep.FsckClean = false
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d attachment(s) the database refers to are missing and %d are corrupt", len(fr.Missing), len(fr.Corrupt)))
		}
	}
	if len(man.BlobsMissing) > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("the backup was made while %d attachment(s) were already missing or damaged at the source", len(man.BlobsMissing)))
	}
	if man.SecretCheck != "" {
		raw, _ := hex.DecodeString(man.SecretCheck)
		if p, err := s.unseal(ctx, raw); err != nil || p != "tasklane-backup-check" {
			rep.Warnings = append(rep.Warnings, "the instance secret key differs from the one that made this backup: credentials stored in the database (Git, Jira, mailbox and single sign-on secrets) cannot be read until secret_key is set to the original")
		}
	}
	// outbox consumers continue from their stored cursors, so derived data (notifications, search) catches up from the log; the search
	// index is rebuilt with `tasklane reindex`
	rep.Warnings = append(rep.Warnings, "people must sign in again, and `tasklane reindex` rebuilds the search index if you use OpenSearch")
	return rep, nil
}

// RestoreOverOpts says how a restore over a live instance protects what it replaces.
type RestoreOverOpts struct {
	SafetyDir string        // a new directory that receives a backup of the current state first, so the restore can itself be undone; empty: none
	Hold      time.Duration // how long writers may be held for the whole job (default 30 minutes)
	Drain     time.Duration // how long in-flight writes may take to finish (default 60 seconds)
}

// RestoreOver replaces the data of a running instance with a backup (SPEC 17.4: "a restore runs in pause mode"). The instance is paused first
// (writers wait, then get 503 with Retry-After; reads and drafts continue), a backup of the current state is taken when asked, the backup is
// loaded in one transaction, so a failure leaves the data exactly as it was, and the pause ends whatever happens. Everyone is signed out
// (sessions are not part of a backup), and the audit log records that the restore started, ended, and what it replaced.
func (s *Service) RestoreOver(ctx context.Context, a Actor, dir string, o RestoreOverOpts) (*RestoreReport, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	if _, err := VerifyBackup(dir); err != nil { // a damaged backup is refused before anything is paused or touched
		return nil, err
	}
	if o.Hold <= 0 {
		o.Hold = 30 * time.Minute
	}
	if o.Drain <= 0 {
		o.Drain = 60 * time.Second
	}
	lease, err := s.RequestPause(ctx, a, PauseIn{Scope: "instance", Owner: "restore", Mode: "pause", DrainWaitMS: int(o.Drain / time.Millisecond), HoldMS: int(o.Hold / time.Millisecond)})
	if err != nil {
		return nil, err
	}
	defer func() {
		s.DB.Exec(context.WithoutCancel(ctx), `DELETE FROM pause_lease WHERE id=$1`, lease.ID)
		s.leases(context.WithoutCancel(ctx), true)
	}()
	// other nodes learn of the pause from the database within a moment; give them that moment before the data moves
	select {
	case <-time.After(2 * pauseCacheTTL):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	audit := func(action string, detail map[string]any) {
		s.DB.Exec(context.WithoutCancel(ctx), `INSERT INTO audit_event(actor_label,action,target,detail) VALUES($1,$2,'instance',$3)`, a.Label, action, jsonMarshal(detail))
	}
	var issues, users int
	s.DB.QueryRow(ctx, `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM issue)`).Scan(&users, &issues)
	audit("restore.started", map[string]any{"backup": filepath.Base(dir), "replacing_people": users, "replacing_issues": issues, "safety_backup": o.SafetyDir != ""})
	if o.SafetyDir != "" {
		if _, err := s.Backup(ctx, o.SafetyDir, BackupOpts{}); err != nil {
			audit("restore.aborted", map[string]any{"reason": "the safety backup failed: " + err.Error()})
			return nil, fmt.Errorf("the backup of the current state failed, so nothing was replaced: %w", err)
		}
	}
	rep, err := s.restore(ctx, dir, true)
	if err != nil {
		audit("restore.aborted", map[string]any{"reason": err.Error()})
		return rep, err
	}
	audit("restore.completed", map[string]any{"backup": filepath.Base(dir), "tables": rep.Tables, "rows": rep.Rows, "attachments": rep.Blobs})
	rep.Warnings = append(rep.Warnings, "restart the servers that use this database: they may hold settings and caches from the data that was replaced")
	return rep, nil
}

var _ = errors.New
