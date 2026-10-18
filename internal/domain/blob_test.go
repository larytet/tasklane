package domain_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func attach(t *testing.T, e *testutil.Env, a domain.Actor, key, name, mime string, data []byte) domain.Attachment {
	t.Helper()
	att, err := e.Svc.AttachStream(ctx, a, key, name, mime, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return att
}

func TestBLOB01_PasteScreenshotAndAsyncThumbnail(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	fs := e.WithBlobs()
	is := e.Issue("s", "")
	orig := pngBytes(600, 400)
	att := attach(t, e, e.Alice, is.Key, "../../etc/shot.png", "image/png", orig)
	if att.Name != "shot.png" || att.Class != "image" || att.MIME != "image/png" || att.Thumb != "pending" || att.ScanState != "clean" {
		t.Fatalf("%+v", att) // the name never carries a path
	}
	// the upload did not wait for the thumbnail
	if n, err := e.Svc.ThumbnailsOnce(ctx); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	di, err := e.Svc.FileInfo(ctx, e.Alice, att.ID, true)
	if err != nil || di.MIME != "image/png" || di.Size == 0 {
		t.Fatalf("%v %+v", err, di)
	}
	f, _ := fs.Open(di.SHA256)
	img, err := png.Decode(f)
	f.Close()
	if err != nil || img.Bounds().Dx() != 256 || img.Bounds().Dy() != 170 {
		t.Fatalf("thumbnail %v %v", err, img.Bounds())
	}
	// original intact
	o, _ := e.Svc.FileInfo(ctx, e.Alice, att.ID, false)
	f, _ = fs.Open(o.SHA256)
	got, _ := io.ReadAll(f)
	f.Close()
	if !bytes.Equal(got, orig) {
		t.Fatal("original changed")
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if len(v.Attachments) != 1 || v.Attachments[0].Thumb != "ready" {
		t.Fatalf("%+v", v.Attachments)
	}
}

func TestBLOB09_ThumbnailFailureFallsBackAndRetries(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	is := e.Issue("s", "")
	// valid PNG signature and header, truncated body: sniffed as an image but undecodable
	bad := pngBytes(50, 50)[:80]
	att := attach(t, e, e.Alice, is.Key, "broken.png", "image/png", bad)
	e.Svc.ThumbnailsOnce(ctx)
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if v.Attachments[0].Thumb != "failed" {
		t.Fatalf("%+v", v.Attachments[0])
	}
	if _, err := e.Svc.FileInfo(ctx, e.Alice, att.ID, true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("thumbnail of a failed image is served")
	}
	if err := e.Svc.RetryThumbnail(ctx, e.Alice, att.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = e.Svc.View(ctx, e.Alice, is.Key)
	if v.Attachments[0].Thumb != "pending" {
		t.Fatal("retry did not requeue")
	}
	// the attachment itself stays downloadable throughout
	if _, err := e.Svc.FileInfo(ctx, e.Alice, att.ID, false); err != nil {
		t.Fatal(err)
	}
}

func TestBLOB02_ResumableUploadAfterDroppedConnection(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	fs := e.WithBlobs()
	is := e.Issue("s", "")
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB
	sum := sha256.Sum256(data)
	us, err := e.Svc.StartUpload(ctx, e.Alice, is.Key, "big.bin", "application/octet-stream", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	// first chunk, then a connection that dies mid-chunk
	if _, err := e.Svc.AppendChunk(ctx, e.Alice, us.ID, 0, bytes.NewReader(data[:1<<20])); err != nil {
		t.Fatal(err)
	}
	broken := io.MultiReader(bytes.NewReader(data[1<<20:3<<19]), errReader{})
	got, err := e.Svc.AppendChunk(ctx, e.Alice, us.ID, 1<<20, broken)
	if err == nil {
		t.Fatal("expected the broken connection to be reported")
	}
	st, _ := e.Svc.UploadStatus(ctx, e.Alice, us.ID)
	if st.Offset != got.Offset || st.Offset != 3<<19 { // the bytes that arrived are kept
		t.Fatalf("offset %d", st.Offset)
	}
	// a client with a stale offset is told where to resume
	var om *domain.OffsetMismatch
	if _, err := e.Svc.AppendChunk(ctx, e.Alice, us.ID, 0, bytes.NewReader(data)); !errors.As(err, &om) || om.Offset != 3<<19 {
		t.Fatalf("%v", err)
	}
	// committing an incomplete upload is refused
	if _, err := e.Svc.CommitUpload(ctx, e.Alice, us.ID); !errors.As(err, &om) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.AppendChunk(ctx, e.Alice, us.ID, st.Offset, bytes.NewReader(data[st.Offset:])); err != nil {
		t.Fatal(err)
	}
	att, err := e.Svc.CommitUpload(ctx, e.Alice, us.ID)
	if err != nil || att.SHA256 != hex.EncodeToString(sum[:]) || att.Size != int64(len(data)) {
		t.Fatalf("%v %+v", err, att)
	}
	if f, err := fs.Open(att.SHA256); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	// another user cannot touch the session
	if _, err := e.Svc.UploadStatus(ctx, e.Bob, us.ID); err == nil {
		t.Fatal("session visible to another user")
	}
	// more bytes than declared are refused and not kept
	us2, _ := e.Svc.StartUpload(ctx, e.Alice, is.Key, "x", "", 10)
	if _, err := e.Svc.AppendChunk(ctx, e.Alice, us2.ID, 0, strings.NewReader("12345678901234")); err == nil {
		t.Fatal("oversize chunk accepted")
	}
	if st, _ := e.Svc.UploadStatus(ctx, e.Alice, us2.ID); st.Offset != 0 {
		t.Fatal("oversize bytes kept")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

func TestBLOB06_DeduplicationAndRefcount(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	fs := e.WithBlobs()
	e.Svc.Blobs.Retention = time.Hour
	is := e.Issue("s", "")
	data := []byte("same content in two attachments")
	a1 := attach(t, e, e.Alice, is.Key, "a.txt", "text/plain", data)
	a2 := attach(t, e, e.Bob, is.Key, "b.txt", "text/plain", data)
	if a1.SHA256 != a2.SHA256 {
		t.Fatal("hash differs")
	}
	count := func() int {
		n := 0
		fs.Walk(func(string, int64) error { n++; return nil })
		return n
	}
	if count() != 1 {
		t.Fatalf("stored %d objects", count())
	}
	var rc int
	e.Store.Pool.QueryRow(ctx, `SELECT refcount FROM blob WHERE sha256=$1`, a1.SHA256).Scan(&rc)
	if rc != 2 {
		t.Fatalf("refcount %d", rc)
	}
	e.Svc.DeleteAttachment(ctx, e.Alice, is.Key, a1.ID)
	if n, _ := e.Svc.GC(ctx); n != 0 || count() != 1 {
		t.Fatal("bytes removed while still referenced")
	}
	if _, err := e.Svc.FileInfo(ctx, e.Bob, a2.ID, false); err != nil { // the other attachment is unaffected
		t.Fatal(err)
	}
	e.Svc.DeleteAttachment(ctx, e.Bob, is.Key, a2.ID)
	if n, _ := e.Svc.GC(ctx); n != 0 || count() != 1 { // zero references, but inside the retention window
		t.Fatal("bytes removed inside the retention window")
	}
	e.Clock.Advance(2 * time.Hour)
	if n, err := e.Svc.GC(ctx); err != nil || n != 1 || count() != 0 {
		t.Fatalf("gc %d %v, %d left", n, err, count())
	}
}

func TestBLOB07_AbandonedUploadsAreRemoved(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	fs := e.WithBlobs()
	is := e.Issue("s", "")
	us, _ := e.Svc.StartUpload(ctx, e.Alice, is.Key, "x.bin", "", 100)
	e.Svc.AppendChunk(ctx, e.Alice, us.ID, 0, strings.NewReader("partial"))
	staging := filepath.Join(fs.Root, "staging")
	ents, _ := os.ReadDir(staging)
	if len(ents) != 1 {
		t.Fatalf("%d staging files", len(ents))
	}
	e.Clock.T = time.Now()                                // staging files carry real mtimes; measure against real time
	if n, _ := e.Svc.Janitor(ctx, 24*time.Hour); n != 0 { // too young
		t.Fatal("fresh upload removed")
	}
	old := time.Now().Add(-25 * time.Hour)
	os.Chtimes(filepath.Join(staging, ents[0].Name()), old, old)
	e.Store.Pool.Exec(ctx, `UPDATE upload_session SET updated_at=now()-interval '25 hours'`)
	e.Clock.T = time.Now() // the janitor measures against the service clock
	if n, err := e.Svc.Janitor(ctx, 24*time.Hour); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM upload_session`).Scan(&n)
	if n != 0 {
		t.Fatal("session left behind")
	}
	if _, err := e.Svc.UploadStatus(ctx, e.Alice, us.ID); err == nil {
		t.Fatal("abandoned session still usable")
	}
}

func TestBLOB08_SpoofedTypesBombsAndEICAR(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	is := e.Issue("s", "")
	// declared image, content is a script: flagged, and never previewed inline
	sp := attach(t, e, e.Alice, is.Key, "photo.png", "image/png", []byte("#!/bin/sh\necho pwned\n"))
	if sp.TypeFlag == "" || sp.Class == "image" {
		t.Fatalf("spoof not flagged: %+v", sp)
	}
	// html is not on the inline list
	h := attach(t, e, e.Alice, is.Key, "x.html", "text/html", []byte("<html><script>alert(1)</script></html>"))
	if di, _ := e.Svc.FileInfo(ctx, e.Alice, h.ID, false); di.Inline {
		t.Fatal("html would render inline")
	}
	// EICAR is blocked and not downloadable, even when hidden inside a larger file
	eicar := `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`
	for _, body := range []string{eicar, strings.Repeat("a", 3<<20) + eicar + strings.Repeat("b", 100)} {
		b := attach(t, e, e.Alice, is.Key, "eicar.com", "application/octet-stream", []byte(body))
		if b.ScanState != "blocked" {
			t.Fatalf("not blocked: %+v", b)
		}
		var nd *domain.NotDownloadable
		if _, err := e.Svc.FileInfo(ctx, e.Alice, b.ID, false); !errors.As(err, &nd) {
			t.Fatalf("blocked file downloadable: %v", err)
		}
		if _, _, err := e.Svc.MintLink(ctx, e.Alice, b.ID, false); err == nil {
			t.Fatal("link minted for a blocked file")
		}
	}
	// the signature split across read chunks is still found
	split := strings.Repeat("x", 1<<20-20) + eicar
	if b := attach(t, e, e.Alice, is.Key, "split.bin", "", []byte(split)); b.ScanState != "blocked" {
		t.Fatal("signature across a chunk boundary missed")
	}
	// zip bomb: tiny on disk, enormous when extracted
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for i := 0; i < 3; i++ {
		w, _ := zw.Create("zeros" + string(rune('a'+i)))
		chunk := make([]byte, 1<<20)
		for j := 0; j < 400; j++ {
			w.Write(chunk)
		}
	}
	zw.Close()
	_, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "bomb.zip", "application/zip", bytes.NewReader(zb.Bytes()))
	var inv *domain.InvalidError
	if !errors.As(err, &inv) || !strings.Contains(err.Error(), "bomb") {
		t.Fatalf("zip bomb accepted: %v", err)
	}
	// a normal zip is fine
	var ok bytes.Buffer
	zw = zip.NewWriter(&ok)
	w, _ := zw.Create("a.txt")
	w.Write([]byte("hello"))
	zw.Close()
	if a := attach(t, e, e.Alice, is.Key, "ok.zip", "application/zip", ok.Bytes()); a.Class != "archive" {
		t.Fatalf("%+v", a)
	}
	// class limits
	e.Svc.Blobs.Limits = map[blob.Class]int64{blob.ClassImage: 100, blob.ClassDocument: 1 << 20, blob.ClassArchive: 1 << 20}
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "huge.png", "image/png", bytes.NewReader(pngBytes(200, 200))); !errors.As(err, &inv) || !strings.Contains(err.Error(), "image files are limited") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "empty", "", bytes.NewReader(nil)); err == nil {
		t.Fatal("empty file accepted")
	}
	// no staging debris after any refusal
	ents, _ := os.ReadDir(filepath.Join(e.Svc.Blobs.Store.(*blob.FS).Root, "staging"))
	if len(ents) != 0 {
		t.Fatalf("%d staging files left after refusals", len(ents))
	}
}

func TestBLOB05_DownloadAuthorization(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	is := e.Issue("s", "")
	att := attach(t, e, e.Alice, is.Key, "notes.txt", "text/plain", []byte("hello"))
	path, exp, err := e.Svc.MintLink(ctx, e.Alice, att.ID, false)
	if err != nil || !exp.After(e.Clock.Now()) || exp.After(e.Clock.Now().Add(10*time.Minute)) {
		t.Fatalf("%v %v", err, exp)
	}
	parse := func(p string) (id, expS, sig, v string) {
		u := strings.SplitN(p, "?", 2)
		id = strings.TrimPrefix(u[0], "/files/")
		for _, kv := range strings.Split(u[1], "&") {
			x := strings.SplitN(kv, "=", 2)
			switch x[0] {
			case "exp":
				expS = x[1]
			case "sig":
				sig = x[1]
			case "v":
				v = x[1]
			}
		}
		return
	}
	id, expS, sig, v := parse(path)
	if _, err := e.Svc.VerifyLink(ctx, id, expS, sig, v); err != nil {
		t.Fatal(err)
	}
	// tampering: another attachment id, a longer expiry, a flipped signature, the thumbnail variant
	other := attach(t, e, e.Alice, is.Key, "other.txt", "text/plain", []byte("other"))
	for name, args := range map[string][4]string{
		"other id":  {other.ID, expS, sig, v},
		"longer":    {id, "99999999999", sig, v},
		"bad sig":   {id, expS, strings.Repeat("0", 64), v},
		"variant":   {id, expS, sig, "t"},
		"no params": {id, "", "", ""},
	} {
		if _, err := e.Svc.VerifyLink(ctx, args[0], args[1], args[2], args[3]); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.Clock.Advance(6 * time.Minute) // expiry
	if _, err := e.Svc.VerifyLink(ctx, id, expS, sig, v); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expired link works: %v", err)
	}
	// revoked or never-granted users cannot mint
	u, _ := e.Svc.CreateUser(ctx, "eve@example.com", "Eve", "password123", false)
	eve := domain.Actor{ID: u.ID, Label: "Eve"}
	if _, _, err := e.Svc.MintLink(ctx, eve, att.ID, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider minted a link: %v", err)
	}
	if _, err := e.Svc.FileInfo(ctx, eve, att.ID, false); err == nil {
		t.Fatal("outsider read file info")
	}
	// an attachment of a deleted issue is not reachable
	e.Store.Pool.Exec(ctx, `UPDATE issue SET deleted_at=now()`)
	if _, err := e.Svc.FileInfo(ctx, e.Alice, att.ID, false); err == nil {
		t.Fatal("file of a trashed issue reachable")
	}
}

func TestBLOB12_23_FsckAndReferenceIntegrity(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	fs := e.WithBlobs()
	is := e.Issue("s", "")
	a1 := attach(t, e, e.Alice, is.Key, "one.txt", "text/plain", []byte("one"))
	a2 := attach(t, e, e.Alice, is.Key, "two.txt", "text/plain", []byte("two"))
	a3 := attach(t, e, e.Alice, is.Key, "three.txt", "text/plain", []byte("three"))
	r, err := e.Svc.Fsck(ctx, false)
	if err != nil || !r.Clean() || r.Checked != 3 {
		t.Fatalf("%v %+v", err, r)
	}
	// bit-rot, a deleted object and an orphan
	p := func(sha string) string { return filepath.Join(fs.Root, sha[:2], sha[2:4], sha) }
	os.Chmod(p(a1.SHA256), 0o640)
	os.WriteFile(p(a1.SHA256), []byte("rotten"), 0o640)
	os.Remove(p(a2.SHA256))
	orphan, _ := fs.Put([]byte("nobody references me"))
	r, _ = e.Svc.Fsck(ctx, false)
	if len(r.Corrupt) != 1 || r.Corrupt[0] != a1.SHA256 || len(r.Missing) != 1 || r.Missing[0] != a2.SHA256 || len(r.Orphans) != 1 || r.Orphans[0] != orphan {
		t.Fatalf("%+v", r)
	}
	// repair removes the corrupt and orphaned bytes; the damaged rows stay visible as missing
	e.Svc.Fsck(ctx, true)
	r, _ = e.Svc.Fsck(ctx, false)
	if len(r.Corrupt) != 0 || len(r.Orphans) != 0 || len(r.Missing) != 2 {
		t.Fatalf("after repair: %+v", r)
	}
	// a damaged file is reported on download, not served as garbage
	if _, err := e.Svc.OpenBlob(a1.SHA256); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	_ = a3
	// locators cannot be built from user input: only 64 hex characters name an object
	for _, bad := range []string{"../../etc/passwd", "..", "ab/cd/" + strings.Repeat("a", 64), strings.Repeat("A", 64), strings.Repeat("a", 63), "", strings.Repeat("a", 64) + "/x", "%2e%2e"} {
		if _, err := fs.Open(bad); err == nil || errors.Is(err, blob.ErrNotFound) {
			t.Errorf("locator %q was accepted", bad)
		}
		if err := fs.Remove(bad); err == nil {
			t.Errorf("remove accepted %q", bad)
		}
	}
	if _, err := fs.StagingPath("../x"); err == nil {
		t.Fatal("staging traversal accepted")
	}
	// no table holds file bytes: every column of the blob tables is metadata
	rows, _ := e.Store.Pool.Query(ctx, `SELECT table_name,column_name,data_type FROM information_schema.columns WHERE table_schema=current_schema() AND table_name IN ('blob','attachment','upload_session') AND data_type='bytea'`)
	defer rows.Close()
	if rows.Next() {
		t.Fatal("a blob table has a bytea column")
	}
}

func TestBLOB20_UploadCapacityIsBounded(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	e.Svc.Blobs.MaxUploads = 1
	e.Svc.ConfigureBlobs(ctx, e.Svc.Blobs)
	is := e.Issue("s", "")
	pr, pw := io.Pipe()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "slow.bin", "", pr)
		done <- err
	}()
	<-started
	var busy bool
	for i := 0; i < 200 && !busy; i++ {
		_, err := e.Svc.AttachStream(ctx, e.Bob, is.Key, "second.bin", "", strings.NewReader("x"))
		busy = errors.Is(err, domain.ErrBusy)
		if !busy {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !busy {
		t.Fatal("second concurrent upload was not limited")
	}
	// interactive calls are unaffected while an upload is in flight
	if _, err := e.Svc.View(ctx, e.Alice, is.Key); err != nil {
		t.Fatal(err)
	}
	pw.Write([]byte("done"))
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AttachStream(ctx, e.Bob, is.Key, "after.bin", "", strings.NewReader("x")); err != nil {
		t.Fatalf("capacity not released: %v", err)
	}
}
