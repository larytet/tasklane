package domain

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/blob"
)

type Attachment struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	MIME      string    `json:"mime"`
	Class     string    `json:"class"`
	Uploader  string    `json:"uploader"`
	Created   time.Time `json:"created"`
	ScanState string    `json:"scan_state"`
	TypeFlag  string    `json:"type_flag,omitempty"`
	Thumb     string    `json:"thumb_state"`
	SHA256    string    `json:"sha256"`
}

// Blobs configures the attachment subsystem.
type Blobs struct {
	Store      blob.Store
	Limits     map[blob.Class]int64
	Retention  time.Duration // how long an unreferenced blob is kept before GC removes it
	LinkTTL    time.Duration
	MaxUploads int // concurrent upload limit (bounded capacity, so uploads cannot starve interactive requests)
	secret     []byte
	slots      chan struct{}
}

// ErrBusy is returned when the concurrent upload limit is reached (HTTP 503 with Retry-After).
var ErrBusy = errors.New("too many uploads in progress")

func (s *Service) listAttachments(ctx context.Context, issueID string) ([]Attachment, error) {
	rows, err := s.DB.Query(ctx, `SELECT a.id,a.name,b.size,b.mime,b.class,COALESCE(u.name,''),a.created_at,b.scan_state,a.type_flag,a.thumb_state,b.sha256
		FROM attachment a JOIN blob b ON b.id=a.blob_id LEFT JOIN app_user u ON u.id=a.uploader_id WHERE a.issue_id=$1 ORDER BY a.created_at,a.id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		var a Attachment
		rows.Scan(&a.ID, &a.Name, &a.Size, &a.MIME, &a.Class, &a.Uploader, &a.Created, &a.ScanState, &a.TypeFlag, &a.Thumb, &a.SHA256)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) blobsReady() error {
	if s.Blobs == nil || s.Blobs.Store == nil {
		return invalid("attachments are not configured")
	}
	return nil
}

// ConfigureBlobs sets the store and loads (or creates) the link-signing secret, kept in the database so
// every node and every restart agrees on it.
func (s *Service) ConfigureBlobs(ctx context.Context, b *Blobs) error {
	if b.Limits == nil {
		b.Limits = blob.Limits
	}
	if b.LinkTTL == 0 {
		b.LinkTTL = 5 * time.Minute
	}
	if b.MaxUploads == 0 {
		b.MaxUploads = 8
	}
	b.slots = make(chan struct{}, b.MaxUploads)
	var sec string
	err := s.DB.QueryRow(ctx, `SELECT value FROM setting WHERE key='link_secret'`).Scan(&sec)
	if err == pgx.ErrNoRows {
		raw := make([]byte, 32)
		rand.Read(raw)
		sec = hex.EncodeToString(raw)
		if _, err := s.DB.Exec(ctx, `INSERT INTO setting(key,value) VALUES('link_secret',$1) ON CONFLICT (key) DO NOTHING`, sec); err != nil {
			return err
		}
		err = s.DB.QueryRow(ctx, `SELECT value FROM setting WHERE key='link_secret'`).Scan(&sec)
	}
	if err != nil {
		return err
	}
	b.secret = []byte(sec)
	s.Blobs = b
	return nil
}

// ---- upload sessions (resumable) and the single-request fast path share one pipeline ----

type UploadSession struct {
	ID       string `json:"id"`
	Offset   int64  `json:"offset"`
	Size     int64  `json:"size"`
	Name     string `json:"name"`
	IssueKey string `json:"issue"`
}

func (s *Service) acquire() (func(), error) {
	select {
	case s.Blobs.slots <- struct{}{}:
		return func() { <-s.Blobs.slots }, nil
	default:
		return nil, ErrBusy
	}
}

// StartUpload opens a resumable upload session for an issue. The size is declared up front so an
// oversized file is refused before any bytes move.
func (s *Service) StartUpload(ctx context.Context, a Actor, key, name, declaredMIME string, size int64) (UploadSession, error) {
	if err := s.blobsReady(); err != nil {
		return UploadSession{}, err
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return UploadSession{}, err
	}
	if !canWrite(role) {
		return UploadSession{}, ErrForbidden
	}
	if size <= 0 {
		return UploadSession{}, invalid("file size is required")
	}
	if max := s.maxAnyClass(); size > max {
		return UploadSession{}, invalid("file is too large (limit %d MiB)", max>>20)
	}
	name = blob.SafeName(name)
	us := UploadSession{Size: size, Name: name, IssueKey: is.Key}
	err = s.DB.QueryRow(ctx, `INSERT INTO upload_session(user_id,issue_id,name,declared_mime,total_size) VALUES($1,$2,$3,$4,$5) RETURNING id`,
		a.ID, is.ID, name, declaredMIME, size).Scan(&us.ID)
	if err != nil {
		return us, err
	}
	// the staging file shares the session id (hex without dashes)
	_, f, err := s.stagingFor(us.ID, true)
	if err != nil {
		return us, err
	}
	f.Close()
	return us, nil
}

func (s *Service) maxAnyClass() int64 {
	var m int64
	for _, v := range s.Blobs.Limits {
		if v > m {
			m = v
		}
	}
	return m
}

func sessionStagingID(id string) string {
	b := make([]byte, 0, 32)
	for i := 0; i < len(id); i++ {
		if id[i] != '-' {
			b = append(b, id[i])
		}
	}
	return string(b)
}

// stagingFor opens the session's staging file (creating it when create is true).
func (s *Service) stagingFor(sessionID string, create bool) (string, *os.File, error) {
	p, err := s.Blobs.Store.StagingPath(sessionStagingID(sessionID))
	if err != nil {
		return "", nil, err
	}
	flags := os.O_WRONLY | os.O_APPEND
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(p, flags, 0o640)
	return p, f, err
}

func (s *Service) loadSession(ctx context.Context, a Actor, id string) (UploadSession, string, string, error) {
	var us UploadSession
	var issueID, mime string
	err := s.DB.QueryRow(ctx, `SELECT u.id,u.received,u.total_size,u.name,i.key,u.issue_id,u.declared_mime FROM upload_session u JOIN issue i ON i.id=u.issue_id
		WHERE u.id=$1 AND u.user_id=$2`, id, a.ID).Scan(&us.ID, &us.Offset, &us.Size, &us.Name, &us.IssueKey, &issueID, &mime)
	if err == pgx.ErrNoRows || (err != nil && len(id) != 36) {
		return us, "", "", ErrNotFound
	}
	return us, issueID, mime, err
}

// UploadStatus returns the current offset so a client can resume after a dropped connection.
func (s *Service) UploadStatus(ctx context.Context, a Actor, id string) (UploadSession, error) {
	if err := s.blobsReady(); err != nil {
		return UploadSession{}, err
	}
	us, _, _, err := s.loadSession(ctx, a, id)
	return us, err
}

// OffsetMismatch is returned when a chunk does not start where the server stopped (HTTP 409).
type OffsetMismatch struct{ Offset int64 }

func (e *OffsetMismatch) Error() string { return "chunk offset does not match the server offset" }

// AppendChunk streams r into the staging file at the expected offset with constant memory.
// A dropped connection leaves the bytes received so far; the client asks for the offset and continues.
func (s *Service) AppendChunk(ctx context.Context, a Actor, id string, offset int64, r io.Reader) (UploadSession, error) {
	if err := s.blobsReady(); err != nil {
		return UploadSession{}, err
	}
	us, _, _, err := s.loadSession(ctx, a, id)
	if err != nil {
		return us, err
	}
	release, err := s.acquire()
	if err != nil {
		return us, err
	}
	defer release()
	if offset != us.Offset {
		return us, &OffsetMismatch{Offset: us.Offset}
	}
	path, f, err := s.stagingFor(id, false)
	if err != nil {
		return us, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() != us.Offset { // the file and the row must agree; trust the row, trim the file
		if err := os.Truncate(path, us.Offset); err != nil {
			return us, err
		}
	}
	remaining := us.Size - us.Offset
	n, copyErr := io.Copy(f, io.LimitReader(r, remaining+1))
	if n > remaining { // more bytes than declared
		os.Truncate(path, us.Offset)
		return us, invalid("upload is larger than the declared size")
	}
	f.Sync()
	if n > 0 {
		if _, err := s.DB.Exec(ctx, `UPDATE upload_session SET received=received+$2, updated_at=now() WHERE id=$1`, id, n); err != nil {
			return us, err
		}
		us.Offset += n
	}
	return us, copyErr // a broken connection reports its error, but the received bytes are kept
}

// CommitUpload validates the finished file and attaches it atomically.
func (s *Service) CommitUpload(ctx context.Context, a Actor, id string) (Attachment, error) {
	if err := s.blobsReady(); err != nil {
		return Attachment{}, err
	}
	us, issueID, mime, err := s.loadSession(ctx, a, id)
	if err != nil {
		return Attachment{}, err
	}
	if us.Offset != us.Size {
		return Attachment{}, &OffsetMismatch{Offset: us.Offset}
	}
	is, role, err := s.access(ctx, a, us.IssueKey)
	if err != nil {
		return Attachment{}, err
	}
	if !canWrite(role) {
		return Attachment{}, ErrForbidden
	}
	path, err := s.Blobs.Store.StagingPath(sessionStagingID(id))
	if err != nil {
		return Attachment{}, err
	}
	att, err := s.commitStaged(ctx, a, is, issueID, sessionStagingID(id), path, us.Name, mime)
	if err != nil {
		return att, err
	}
	s.DB.Exec(ctx, `DELETE FROM upload_session WHERE id=$1`, id)
	return att, nil
}

// AttachStream is the single-request fast path (a pasted screenshot): same pipeline, one call.
func (s *Service) AttachStream(ctx context.Context, a Actor, key, name, declaredMIME string, r io.Reader) (Attachment, error) {
	if err := s.blobsReady(); err != nil {
		return Attachment{}, err
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return Attachment{}, err
	}
	if !canWrite(role) {
		return Attachment{}, ErrForbidden
	}
	release, err := s.acquire()
	if err != nil {
		return Attachment{}, err
	}
	defer release()
	id, f, err := s.Blobs.Store.NewStaging()
	if err != nil {
		return Attachment{}, err
	}
	max := s.maxAnyClass()
	n, err := io.Copy(f, io.LimitReader(r, max+1))
	f.Close()
	path, _ := s.Blobs.Store.StagingPath(id)
	if err != nil || n > max || n == 0 {
		os.Remove(path)
		if err == nil && n == 0 {
			return Attachment{}, invalid("file is empty")
		}
		if err == nil {
			return Attachment{}, invalid("file is too large (limit %d MiB)", max>>20)
		}
		return Attachment{}, err
	}
	return s.commitStaged(ctx, a, is, is.ID, id, path, blob.SafeName(name), declaredMIME)
}

// commitStaged hashes, sniffs, enforces class limits, refuses bombs, scans, then commits the object and
// the attachment row together. Every refusal removes the staging file, so nothing orphaned stays visible.
func (s *Service) commitStaged(ctx context.Context, a Actor, is Issue, issueID, stagingID, path, name, declaredMIME string) (Attachment, error) {
	fail := func(err error) (Attachment, error) { os.Remove(path); return Attachment{}, err }
	sha, size, err := blob.HashFile(path)
	if err != nil {
		return fail(err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fail(err)
	}
	head := make([]byte, 512)
	hn, _ := io.ReadFull(f, head)
	head = head[:hn]
	mime, class := blob.Sniff(head, name)
	// scan the whole stream in chunks with constant memory
	f.Seek(0, io.SeekStart)
	sc := &blob.Scanner{}
	buf := make([]byte, 1<<20)
	sig := ""
	for sig == "" {
		n, rerr := f.Read(buf)
		if n > 0 {
			sig = sc.Feed(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	f.Close()
	if lim := s.Blobs.Limits[class]; lim > 0 && size > lim {
		return fail(invalid("%s files are limited to %d MiB (this one is %d MiB)", class, lim>>20, size>>20))
	}
	if mime == "application/zip" {
		if err := blob.CheckZip(path, size); err != nil {
			return fail(invalid("%v", err))
		}
	}
	scanState, detail := "clean", ""
	if sig != "" {
		scanState, detail = "blocked", sig
	}
	flag := blob.TypeFlag(declaredMIME, mime)
	var att Attachment
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var blobID string
		err := tx.QueryRow(ctx, `SELECT id FROM blob WHERE sha256=$1 FOR UPDATE`, sha).Scan(&blobID)
		if err == pgx.ErrNoRows {
			if _, err := s.Blobs.Store.Commit(stagingID, sha); err != nil {
				return err
			}
			err = tx.QueryRow(ctx, `INSERT INTO blob(sha256,size,mime,class,scan_state,scan_detail) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`,
				sha, size, mime, string(class), scanState, detail).Scan(&blobID)
		} else if err == nil {
			os.Remove(path) // deduplicated: the bytes are already stored
			// a hit on an existing blob never weakens its scan state
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE blob SET refcount=refcount+1, zero_since=NULL WHERE id=$1`, blobID); err != nil {
			return err
		}
		thumb := "none"
		if class == blob.ClassImage && scanState == "clean" {
			thumb = "pending"
		}
		err = tx.QueryRow(ctx, `INSERT INTO attachment(issue_id,blob_id,name,declared_mime,type_flag,uploader_id,thumb_state) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id,created_at`,
			issueID, blobID, name, declaredMIME, flag, nilIfEmpty(a.ID), thumb).Scan(&att.ID, &att.Created)
		if err != nil {
			return err
		}
		if err := history(ctx, tx, issueID, a, "attachment", nil, sp(name)); err != nil {
			return err
		}
		tx.Exec(ctx, `UPDATE issue SET updated_at=now() WHERE id=$1`, issueID)
		return outbox(ctx, tx, "issue:"+is.Key, "attachment.added", map[string]any{"key": is.Key, "attachment": att.ID, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		os.Remove(path)
		return Attachment{}, err
	}
	var scanned string
	s.DB.QueryRow(ctx, `SELECT scan_state FROM blob WHERE sha256=$1`, sha).Scan(&scanned)
	att.Name, att.Size, att.MIME, att.Class, att.ScanState, att.TypeFlag, att.SHA256, att.Uploader = name, size, mime, string(class), scanned, flag, sha, a.Label
	att.Thumb = "none"
	if class == blob.ClassImage && scanned == "clean" {
		att.Thumb = "pending"
	}
	return att, nil
}

// DeleteAttachment drops one reference; the bytes go only when the GC finds zero references past retention.
func (s *Service) DeleteAttachment(ctx context.Context, a Actor, key, attID string) error {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var blobID, name string
		err := tx.QueryRow(ctx, `DELETE FROM attachment WHERE id=$1 AND issue_id=$2 RETURNING blob_id,name`, attID, is.ID).Scan(&blobID, &name)
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE blob SET refcount=refcount-1, zero_since=CASE WHEN refcount-1<=0 THEN $2::timestamptz ELSE zero_since END WHERE id=$1`, blobID, s.Now()); err != nil {
			return err
		}
		if err := history(ctx, tx, is.ID, a, "attachment", sp(name), nil); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+is.Key, "attachment.removed", map[string]any{"key": is.Key, "attachment": attID, "actor": a.Label, "actor_id": a.ID})
	})
}

// ---- download: authorization at issue time, short-lived signed links, safe serving ----

type DownloadInfo struct {
	SHA256  string
	Name    string
	MIME    string
	Size    int64
	Inline  bool
	Created time.Time
	Thumb   bool
}

func (s *Service) sign(id string, exp int64, variant string) string {
	m := hmac.New(sha256.New, s.Blobs.secret)
	fmt.Fprintf(m, "%s|%d|%s", id, exp, variant)
	return hex.EncodeToString(m.Sum(nil))
}

// FileInfo checks the actor may read the attachment's issue and that its blob is downloadable.
func (s *Service) FileInfo(ctx context.Context, a Actor, attID string, thumb bool) (DownloadInfo, error) {
	var key string
	err := s.DB.QueryRow(ctx, `SELECT i.key FROM attachment a JOIN issue i ON i.id=a.issue_id WHERE a.id=$1 AND i.deleted_at IS NULL`, attID).Scan(&key)
	if err != nil {
		return DownloadInfo{}, ErrNotFound
	}
	if _, _, err := s.access(ctx, a, key); err != nil {
		return DownloadInfo{}, err
	}
	return s.fileInfo(ctx, attID, thumb)
}

func (s *Service) fileInfo(ctx context.Context, attID string, thumb bool) (DownloadInfo, error) {
	var d DownloadInfo
	var scan string
	var thumbID *string
	var thumbState string
	err := s.DB.QueryRow(ctx, `SELECT b.sha256,a.name,b.mime,b.size,b.scan_state,a.created_at,a.thumb_blob_id,a.thumb_state FROM attachment a JOIN blob b ON b.id=a.blob_id WHERE a.id=$1`, attID).
		Scan(&d.SHA256, &d.Name, &d.MIME, &d.Size, &scan, &d.Created, &thumbID, &thumbState)
	if err == pgx.ErrNoRows {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	if scan != "clean" { // blocked and still-scanning files are not downloadable
		return d, &NotDownloadable{State: scan}
	}
	d.Inline = blob.SafeInline(d.MIME)
	if thumb {
		if thumbID == nil || thumbState != "ready" {
			return d, ErrNotFound
		}
		err = s.DB.QueryRow(ctx, `SELECT sha256,size FROM blob WHERE id=$1`, *thumbID).Scan(&d.SHA256, &d.Size)
		d.MIME, d.Inline, d.Thumb = "image/png", true, true
		d.Name = "thumb-" + d.Name + ".png"
	}
	return d, err
}

// NotDownloadable is returned for blocked or still-scanning files (HTTP 423).
type NotDownloadable struct{ State string }

func (e *NotDownloadable) Error() string { return "file is " + e.State }

// MintLink returns a short-lived signed path for the attachment, only after the permission checks.
func (s *Service) MintLink(ctx context.Context, a Actor, attID string, thumb bool) (string, time.Time, error) {
	if err := s.blobsReady(); err != nil {
		return "", time.Time{}, err
	}
	if _, err := s.FileInfo(ctx, a, attID, thumb); err != nil {
		return "", time.Time{}, err
	}
	exp := s.Now().Add(s.Blobs.LinkTTL)
	variant := "f"
	if thumb {
		variant = "t"
	}
	q := url.Values{"exp": {strconv.FormatInt(exp.Unix(), 10)}, "sig": {s.sign(attID, exp.Unix(), variant)}, "v": {variant}}
	return "/files/" + attID + "?" + q.Encode(), exp, nil
}

// VerifyLink validates a signed link's signature and expiry and returns the file to serve.
func (s *Service) VerifyLink(ctx context.Context, attID, expStr, sig, variant string) (DownloadInfo, error) {
	if err := s.blobsReady(); err != nil {
		return DownloadInfo{}, err
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || (variant != "f" && variant != "t") {
		return DownloadInfo{}, ErrForbidden
	}
	want := s.sign(attID, exp, variant)
	if !hmac.Equal([]byte(want), []byte(sig)) || s.Now().Unix() > exp {
		return DownloadInfo{}, ErrForbidden
	}
	return s.fileInfo(ctx, attID, variant == "t")
}

// OpenBlob opens the stored bytes by hash.
func (s *Service) OpenBlob(sha string) (*os.File, error) { return s.Blobs.Store.Open(sha) }

// ---- operations: GC, janitor, fsck ----

// GC removes blobs that have had zero references for longer than the retention window.
func (s *Service) GC(ctx context.Context) (int, error) {
	if err := s.blobsReady(); err != nil {
		return 0, err
	}
	cut := s.Now().Add(-s.Blobs.Retention)
	rows, err := s.DB.Query(ctx, `SELECT id,sha256 FROM blob WHERE refcount<=0 AND zero_since IS NOT NULL AND zero_since<=$1
		AND id NOT IN (SELECT thumb_blob_id FROM attachment WHERE thumb_blob_id IS NOT NULL)`, cut)
	if err != nil {
		return 0, err
	}
	type bl struct{ id, sha string }
	var dead []bl
	for rows.Next() {
		var b bl
		rows.Scan(&b.id, &b.sha)
		dead = append(dead, b)
	}
	rows.Close()
	n := 0
	for _, b := range dead {
		// delete the row first (guarded against a concurrent re-reference), then the bytes
		tag, err := s.DB.Exec(ctx, `DELETE FROM blob WHERE id=$1 AND refcount<=0`, b.id)
		if err != nil || tag.RowsAffected() == 0 {
			continue
		}
		if err := s.Blobs.Store.Remove(b.sha); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Janitor removes abandoned uploads (staging files and sessions) older than the given age.
func (s *Service) Janitor(ctx context.Context, maxAge time.Duration) (int, error) {
	if err := s.blobsReady(); err != nil {
		return 0, err
	}
	cut := s.Now().Add(-maxAge)
	s.DB.Exec(ctx, `DELETE FROM upload_session WHERE updated_at<=$1`, cut)
	return s.Blobs.Store.CleanStaging(cut)
}

type FsckReport struct {
	Missing []string `json:"missing"` // referenced by the database, absent from the store
	Orphans []string `json:"orphans"` // in the store, referenced by nothing
	Corrupt []string `json:"corrupt"` // content hash differs from the name
	Checked int      `json:"checked"`
}

func (r FsckReport) Clean() bool { return len(r.Missing)+len(r.Orphans)+len(r.Corrupt) == 0 }

// Fsck compares the database with the store. With repair, orphans and corrupt objects are removed
// (a corrupt object's row is kept so the damage stays visible as "missing").
func (s *Service) Fsck(ctx context.Context, repair bool) (FsckReport, error) {
	var r FsckReport
	if err := s.blobsReady(); err != nil {
		return r, err
	}
	known := map[string]bool{}
	rows, err := s.DB.Query(ctx, `SELECT sha256 FROM blob`)
	if err != nil {
		return r, err
	}
	for rows.Next() {
		var sha string
		rows.Scan(&sha)
		known[sha] = true
	}
	rows.Close()
	present := map[string]bool{}
	err = s.Blobs.Store.Walk(func(sha string, size int64) error {
		r.Checked++
		present[sha] = true
		f, err := s.Blobs.Store.Open(sha)
		if err != nil {
			return nil
		}
		h := sha256.New()
		_, cerr := io.Copy(h, f)
		f.Close()
		if cerr != nil || hex.EncodeToString(h.Sum(nil)) != sha {
			r.Corrupt = append(r.Corrupt, sha)
			if repair {
				s.Blobs.Store.Remove(sha)
			}
		} else if !known[sha] {
			r.Orphans = append(r.Orphans, sha)
			if repair {
				s.Blobs.Store.Remove(sha)
			}
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	for sha := range known {
		if !present[sha] {
			r.Missing = append(r.Missing, sha)
		}
	}
	return r, nil
}

// ThumbnailsOnce generates pending thumbnails (derived data: asynchronous, regenerable, never blocking an
// upload). A failure marks the attachment "failed" so the UI shows a fallback and offers a retry.
func (s *Service) ThumbnailsOnce(ctx context.Context) (int, error) {
	if err := s.blobsReady(); err != nil {
		return 0, err
	}
	rows, err := s.DB.Query(ctx, `SELECT a.id,b.sha256 FROM attachment a JOIN blob b ON b.id=a.blob_id WHERE a.thumb_state='pending' AND b.scan_state='clean' ORDER BY a.created_at LIMIT 50`)
	if err != nil {
		return 0, err
	}
	type job struct{ id, sha string }
	var jobs []job
	for rows.Next() {
		var j job
		rows.Scan(&j.id, &j.sha)
		jobs = append(jobs, j)
	}
	rows.Close()
	done := 0
	for _, j := range jobs {
		var png []byte
		f, err := s.Blobs.Store.Open(j.sha)
		if err == nil {
			png, err = blob.Thumbnail(f, 256)
			f.Close()
		}
		if err != nil {
			s.DB.Exec(ctx, `UPDATE attachment SET thumb_state='failed' WHERE id=$1`, j.id)
			continue
		}
		sha, err := s.Blobs.Store.Put(png)
		if err != nil {
			return done, err
		}
		err = s.tx(ctx, func(tx pgx.Tx) error {
			var bid string
			err := tx.QueryRow(ctx, `INSERT INTO blob(sha256,size,mime,class,scan_state,refcount) VALUES($1,$2,'image/png','image','clean',1)
				ON CONFLICT (sha256) DO UPDATE SET refcount=blob.refcount+1, zero_since=NULL RETURNING id`, sha, len(png)).Scan(&bid)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE attachment SET thumb_blob_id=$2, thumb_state='ready' WHERE id=$1`, j.id, bid)
			return err
		})
		if err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

// RetryThumbnail re-queues a failed or missing thumbnail.
func (s *Service) RetryThumbnail(ctx context.Context, a Actor, attID string) error {
	var key string
	if err := s.DB.QueryRow(ctx, `SELECT i.key FROM attachment a JOIN issue i ON i.id=a.issue_id WHERE a.id=$1`, attID).Scan(&key); err != nil {
		return ErrNotFound
	}
	if _, role, err := s.access(ctx, a, key); err != nil {
		return err
	} else if !canWrite(role) {
		return ErrForbidden
	}
	_, err := s.DB.Exec(ctx, `UPDATE attachment a SET thumb_state='pending' FROM blob b WHERE a.id=$1 AND b.id=a.blob_id AND b.class='image' AND a.thumb_state IN ('failed','none')`, attID)
	return err
}

// RunBlobWorkers runs thumbnails, the staging janitor and the GC until ctx ends.
func (s *Service) RunBlobWorkers(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	last := time.Time{}
	for {
		if _, err := s.ThumbnailsOnce(ctx); err != nil && ctx.Err() == nil {
			logf("thumbnails: %v", err)
		}
		if time.Since(last) > time.Hour {
			last = time.Now()
			if _, err := s.Janitor(ctx, 24*time.Hour); err != nil && ctx.Err() == nil {
				logf("janitor: %v", err)
			}
			if _, err := s.GC(ctx); err != nil && ctx.Err() == nil {
				logf("gc: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
