package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/blob"
)

// Direct uploads (BLOB-03): with a backend that supports it, the browser sends the bytes straight to the store using
// presigned URLs, and the application only handles control messages. The bytes sit under a staging key nobody can see;
// at completion they are read back and verified exactly as a proxied upload is, then adopted under their hash.

const directTTL = 2 * time.Hour

type DirectPart struct {
	Number int    `json:"n"`
	URL    string `json:"url"`
}

type DirectUpload struct {
	ID        string       `json:"id"`
	Mode      string       `json:"mode"` // "single" (one PUT to url) or "multipart" (one PUT per part)
	URL       string       `json:"url,omitempty"`
	PartSize  int64        `json:"part_size,omitempty"`
	Parts     []DirectPart `json:"parts,omitempty"`
	ExpiresAt time.Time    `json:"expires_at"`
}

type DirectPartDone struct {
	Number int    `json:"n"`
	ETag   string `json:"etag"`
}

func (s *Service) direct() (blob.DirectStore, error) {
	if err := s.blobsReady(); err != nil {
		return nil, err
	}
	ds, ok := s.Blobs.Store.(blob.DirectStore)
	if !ok {
		return nil, invalid("this installation keeps files on disk, so direct uploads are not available: use the ordinary upload")
	}
	return ds, nil
}

// StartDirectUpload hands out presigned URLs for a file of the declared size. sha256, when given, is the browser's own
// hash of the file and is checked at completion.
func (s *Service) StartDirectUpload(ctx context.Context, a Actor, key, name, declaredMIME string, size int64, sha string) (DirectUpload, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return DirectUpload{}, err
	}
	if !canWrite(role) {
		return DirectUpload{}, ErrForbidden
	}
	ds, err := s.direct()
	if err != nil {
		return DirectUpload{}, err
	}
	if size <= 0 {
		return DirectUpload{}, invalid("file size is required")
	}
	if max := s.maxAnyClass(); size > max {
		return DirectUpload{}, invalid("file is too large (limit %d MiB)", max>>20)
	}
	if err := s.checkQuota(ctx, is.Project, size); err != nil {
		return DirectUpload{}, err
	}
	if sha != "" && !blob.ValidSHA(sha) {
		return DirectUpload{}, invalid("sha256 must be 64 lowercase hex characters")
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return DirectUpload{}, perr
	}
	defer endWrite()
	du := DirectUpload{ExpiresAt: s.Now().Add(directTTL)}
	parts := 0
	if size > ds.PartSize() {
		parts = int((size + ds.PartSize() - 1) / ds.PartSize())
		if parts > 10000 {
			return du, invalid("too many parts")
		}
	}
	if err := s.DB.QueryRow(ctx, `INSERT INTO direct_upload(user_id,issue_id,name,declared_mime,size,sha256,parts,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
		a.ID, is.ID, blob.SafeName(name), declaredMIME, size, sha, parts, du.ExpiresAt).Scan(&du.ID); err != nil {
		return du, err
	}
	skey := ds.StagingKey(du.ID)
	if parts == 0 {
		du.Mode, du.URL = "single", ds.PresignPut(skey, directTTL)
		return du, nil
	}
	id, err := ds.BeginMultipart(ctx, skey)
	if err != nil {
		s.DB.Exec(ctx, `DELETE FROM direct_upload WHERE id::text=$1`, du.ID)
		return du, err
	}
	s.DB.Exec(ctx, `UPDATE direct_upload SET multipart_id=$2 WHERE id::text=$1`, du.ID, id)
	du.Mode, du.PartSize = "multipart", ds.PartSize()
	for n := 1; n <= parts; n++ {
		du.Parts = append(du.Parts, DirectPart{Number: n, URL: ds.PresignPart(skey, id, n, directTTL)})
	}
	return du, nil
}

type directRow struct {
	id, issueID, name, mime, sha, multipart string
	size                                    int64
	parts                                   int
	expires                                 time.Time
}

func (s *Service) loadDirect(ctx context.Context, a Actor, id string) (directRow, error) {
	var d directRow
	err := s.DB.QueryRow(ctx, `SELECT id::text,issue_id::text,name,declared_mime,sha256,multipart_id,size,parts,expires_at FROM direct_upload WHERE id::text=$1 AND user_id=$2`, id, a.ID).
		Scan(&d.id, &d.issueID, &d.name, &d.mime, &d.sha, &d.multipart, &d.size, &d.parts, &d.expires)
	if err == pgx.ErrNoRows || (err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")) {
		return d, ErrNotFound // somebody else's upload is indistinguishable from none
	}
	return d, err
}

// AbortDirectUpload cancels an upload and removes whatever was staged.
func (s *Service) AbortDirectUpload(ctx context.Context, a Actor, id string) error {
	ds, err := s.direct()
	if err != nil {
		return err
	}
	d, err := s.loadDirect(ctx, a, id)
	if err != nil {
		return err
	}
	s.dropDirect(ctx, ds, d)
	return nil
}

func (s *Service) dropDirect(ctx context.Context, ds blob.DirectStore, d directRow) {
	skey := ds.StagingKey(d.id)
	if d.multipart != "" {
		ds.AbortMultipart(ctx, skey, d.multipart)
	}
	ds.DeleteKey(ctx, skey)
	s.DB.Exec(ctx, `DELETE FROM direct_upload WHERE id::text=$1`, d.id)
}

// CompleteDirectUpload verifies the staged bytes and makes them an attachment. Every refusal removes the staged object.
func (s *Service) CompleteDirectUpload(ctx context.Context, a Actor, id string, done []DirectPartDone) (Attachment, error) {
	ds, err := s.direct()
	if err != nil {
		return Attachment{}, err
	}
	d, err := s.loadDirect(ctx, a, id)
	if err != nil {
		return Attachment{}, err
	}
	var key string
	if err := s.DB.QueryRow(ctx, `SELECT key FROM issue WHERE id::text=$1 AND deleted_at IS NULL`, d.issueID).Scan(&key); err != nil {
		s.dropDirect(ctx, ds, d)
		return Attachment{}, ErrGone
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return Attachment{}, err
	}
	if !canWrite(role) {
		return Attachment{}, ErrForbidden
	}
	if s.Now().After(d.expires) {
		s.dropDirect(ctx, ds, d)
		return Attachment{}, invalid("this upload expired; start it again")
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return Attachment{}, perr
	}
	defer endWrite()
	release, err := s.acquire()
	if err != nil {
		return Attachment{}, err
	}
	defer release()
	skey := ds.StagingKey(d.id)
	reject := func(msg string) (Attachment, error) {
		s.dropDirect(ctx, ds, d)
		return Attachment{}, invalid("%s", msg)
	}
	if d.multipart != "" {
		if len(done) != d.parts {
			return Attachment{}, invalid("expected %d parts, got %d", d.parts, len(done))
		}
		parts := make([]blob.Part, 0, len(done))
		for _, p := range done {
			parts = append(parts, blob.Part{Number: p.Number, ETag: p.ETag})
		}
		if err := ds.CompleteMultipart(ctx, skey, d.multipart, parts); err != nil {
			return reject("the store could not assemble the parts (a part is missing, out of order or was changed): " + err.Error())
		}
	}
	size, err := ds.StatKey(ctx, skey)
	if errors.Is(err, blob.ErrNotFound) {
		return Attachment{}, invalid("nothing was uploaded to the URL yet")
	}
	if err != nil {
		return Attachment{}, err
	}
	if size != d.size {
		return reject("the uploaded size differs from the declared size (the upload was truncated or changed)")
	}
	// read the staged bytes back once: hash, sniff the type, scan
	obj, err := ds.OpenKey(ctx, skey)
	if err != nil {
		return Attachment{}, err
	}
	h := sha256.New()
	sc := &blob.Scanner{}
	buf := make([]byte, 1<<20)
	var head []byte
	sig := ""
	for {
		n, rerr := obj.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if len(head) < 512 {
				head = append(head, buf[:min(n, 512-len(head))]...)
			}
			if sig == "" {
				sig = sc.Feed(buf[:n])
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				obj.Close()
				return Attachment{}, rerr
			}
			break
		}
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if d.sha != "" && d.sha != sha {
		obj.Close()
		return reject("the uploaded bytes do not match the checksum the browser declared")
	}
	name := blob.SafeName(d.name)
	mime, class := blob.Sniff(head, name)
	if lim := s.Blobs.Limits[class]; lim > 0 && size > lim {
		obj.Close()
		return reject(string(class) + " files are limited to " + itoa(lim>>20) + " MiB")
	}
	if mime == "application/zip" { // the archive check needs random access: stage a local copy for it
		tmp, f, err := s.Blobs.Store.NewStaging()
		if err != nil {
			obj.Close()
			return Attachment{}, err
		}
		obj.Seek(0, io.SeekStart)
		_, cerr := io.Copy(f, obj)
		f.Close()
		path, _ := s.Blobs.Store.StagingPath(tmp)
		var zerr error
		if cerr == nil {
			zerr = blob.CheckZip(path, size)
		}
		os.Remove(path)
		if cerr != nil {
			obj.Close()
			return Attachment{}, cerr
		}
		if zerr != nil {
			obj.Close()
			return reject(zerr.Error())
		}
	}
	obj.Close()
	scanState, detail := "clean", ""
	if sig != "" {
		scanState, detail = "blocked", sig
	}
	flag := blob.TypeFlag(d.mime, mime)
	att, err := s.registerAttachment(ctx, a, is, is.ID, name, d.mime, sha, size, mime, class, scanState, detail, flag,
		func() error { _, err := ds.Adopt(ctx, skey, sha); return err }, func() { ds.DeleteKey(ctx, skey) })
	if err != nil {
		return Attachment{}, err
	}
	s.DB.Exec(ctx, `DELETE FROM direct_upload WHERE id::text=$1`, d.id)
	// a blob that already existed took the dedupe branch inside registerAttachment, which dropped the staged copy;
	// a new one was adopted. Either way no staging object remains.
	ds.DeleteKey(ctx, skey)
	return att, nil
}

// CleanDirectUploads aborts uploads past their expiry and removes their staged objects (run by the janitor).
func (s *Service) CleanDirectUploads(ctx context.Context) (int, error) {
	ds, ok := s.Blobs.Store.(blob.DirectStore)
	if !ok {
		return 0, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,issue_id::text,name,declared_mime,sha256,multipart_id,size,parts,expires_at FROM direct_upload WHERE expires_at<=$1`, s.Now())
	if err != nil {
		return 0, err
	}
	var dead []directRow
	for rows.Next() {
		var d directRow
		rows.Scan(&d.id, &d.issueID, &d.name, &d.mime, &d.sha, &d.multipart, &d.size, &d.parts, &d.expires)
		dead = append(dead, d)
	}
	rows.Close()
	for _, d := range dead {
		s.dropDirect(ctx, ds, d)
	}
	return len(dead), nil
}
