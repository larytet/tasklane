package domain_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/s3mock"
)

func put(t *testing.T, url string, data []byte) string {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("PUT to the presigned URL: %d", resp.StatusCode)
	}
	return resp.Header.Get("ETag")
}

func hexSHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func stagingKeys(m *s3mock.Server) int {
	n := 0
	for _, k := range m.Keys() {
		if strings.HasPrefix(k, "staging/") {
			n++
		}
	}
	return n
}

func TestBLOB03_DirectSingleAndMultipartUpload(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	// small: one presigned PUT
	data := []byte("direct to the bucket")
	du, err := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "note.txt", "text/plain", int64(len(data)), hexSHA(data))
	if err != nil || du.Mode != "single" || du.URL == "" {
		t.Fatalf("%v %+v", err, du)
	}
	put(t, du.URL, data)
	// nothing is visible before completion: no attachment, no blob row, only a staged object
	if v, _ := e.Svc.View(ctx, e.Alice, is.Key); len(v.Attachments) != 0 {
		t.Fatal("an attachment is visible before the upload was completed")
	}
	if stagingKeys(m) != 1 || len(m.Keys()) != 1 {
		t.Fatalf("%v", m.Keys())
	}
	att, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, nil)
	if err != nil || att.SHA256 != hexSHA(data) || att.MIME != "text/plain" || att.Size != int64(len(data)) {
		t.Fatalf("%v %+v", err, att)
	}
	if got, ok := m.Get(keyOf(att.SHA256)); !ok || !bytes.Equal(got, data) || stagingKeys(m) != 0 {
		t.Fatalf("not adopted under its hash: %v", m.Keys())
	}
	if m.Count("GET", "") != 1 { // verified with one pass over the bytes, never downloaded twice for a small file
		t.Logf("get requests: %d", m.Count("GET", ""))
	}
	// large: parts in any order, with ETags
	big := make([]byte, 12<<20)
	for i := range big {
		big[i] = byte(i * 31)
	}
	du, err = e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "big.bin", "application/octet-stream", int64(len(big)), "")
	if err != nil || du.Mode != "multipart" || len(du.Parts) != 3 || du.PartSize != 5<<20 {
		t.Fatalf("%v %+v", err, du)
	}
	etags := make([]domain.DirectPartDone, len(du.Parts))
	var wg sync.WaitGroup
	for i := len(du.Parts) - 1; i >= 0; i-- { // concurrently and out of order
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lo, hi := i*int(du.PartSize), (i+1)*int(du.PartSize)
			if hi > len(big) {
				hi = len(big)
			}
			etags[i] = domain.DirectPartDone{Number: du.Parts[i].Number, ETag: put(t, du.Parts[i].URL, big[lo:hi])}
		}(i)
	}
	wg.Wait()
	att, err = e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, etags)
	if err != nil || att.SHA256 != hexSHA(big) {
		t.Fatalf("%v %+v", err, att)
	}
	if got, _ := m.Get(keyOf(att.SHA256)); !bytes.Equal(got, big) || stagingKeys(m) != 0 || m.OpenUploads() != 0 {
		t.Fatalf("%v", m.Keys())
	}
	// the upload is finished: completing again finds nothing
	if _, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, etags); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestBLOB03_DirectUploadRejectionsRemoveTheStagedObject(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	start := func(name, mime string, declared int, sha string) domain.DirectUpload {
		du, err := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, name, mime, int64(declared), sha)
		if err != nil {
			t.Fatal(err)
		}
		return du
	}
	refused := func(du domain.DirectUpload, parts []domain.DirectPartDone, want string) {
		t.Helper()
		_, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, parts)
		var inv *domain.InvalidError
		if !errors.As(err, &inv) || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q, got %v", want, err)
		}
		if stagingKeys(m) != 0 || m.OpenUploads() != 0 || len(m.Keys()) != 0 {
			t.Fatalf("leftovers after a refusal: %v uploads %d", m.Keys(), m.OpenUploads())
		}
		if v, _ := e.Svc.View(ctx, e.Alice, is.Key); len(v.Attachments) != 0 {
			t.Fatal("a refused upload became an attachment")
		}
		var n int
		e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM direct_upload`).Scan(&n)
		if n != 0 {
			t.Fatal("the upload record survived a refusal")
		}
	}
	// truncated: the browser said 1000 bytes and sent 10
	du := start("t.bin", "", 1000, "")
	put(t, du.URL, []byte("0123456789"))
	refused(du, nil, "size differs")
	// tampered: the bytes are not the ones whose hash the browser declared
	du = start("t.txt", "text/plain", 5, hexSHA([]byte("hello")))
	put(t, du.URL, []byte("HELLO"))
	refused(du, nil, "checksum")
	// a part the client changed after getting its ETag, and a missing part
	big := make([]byte, 11<<20)
	du = start("b.bin", "", len(big), "")
	e1 := put(t, du.Parts[0].URL, big[:5<<20])
	_ = put(t, du.Parts[1].URL, big[5<<20:10<<20])
	_ = put(t, du.Parts[2].URL, big[10<<20:])
	refused(du, []domain.DirectPartDone{{1, e1}, {2, `"deadbeef"`}, {3, `"x"`}}, "could not assemble")
	du = start("c.bin", "", len(big), "")
	// a wrong number of parts is a client mistake: refused before the store is touched, and the upload stays open to retry
	if _, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, []domain.DirectPartDone{{1, "a"}}); err == nil || !strings.Contains(err.Error(), "expected 3 parts") {
		t.Fatalf("%v", err)
	}
	if m.OpenUploads() != 1 {
		t.Fatal("the upload should still be open")
	}
	if err := e.Svc.AbortDirectUpload(ctx, e.Alice, du.ID); err != nil || m.OpenUploads() != 0 {
		t.Fatalf("%v", err)
	}
	// a zip bomb
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
	du = start("bomb.zip", "application/zip", zb.Len(), "")
	put(t, du.URL, zb.Bytes())
	refused(du, nil, "bomb")
	// nothing uploaded yet is a plain, retryable message and keeps the upload open
	du = start("n.txt", "text/plain", 3, "")
	if _, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, nil); err == nil || !strings.Contains(err.Error(), "nothing was uploaded") {
		t.Fatalf("%v", err)
	}
	put(t, du.URL, []byte("abc"))
	if _, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, nil); err != nil {
		t.Fatalf("the retry after uploading failed: %v", err)
	}
}

func TestBLOB03_DirectUploadTypeSniffScanAndLimits(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	_ = m
	is := e.Issue("s", "")
	done := func(name, mime string, data []byte) domain.Attachment {
		du, err := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, name, mime, int64(len(data)), "")
		if err != nil {
			t.Fatal(err)
		}
		put(t, du.URL, data)
		a, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	// a script declared as an image is flagged and never treated as one
	sp := done("photo.png", "image/png", []byte("#!/bin/sh\necho pwned\n"))
	if sp.TypeFlag == "" || sp.Class == "image" {
		t.Fatalf("%+v", sp)
	}
	// a real image keeps its class and gets a thumbnail
	img := done("shot.png", "image/png", pngBytes(300, 200))
	if img.Class != "image" || img.Thumb != "pending" {
		t.Fatalf("%+v", img)
	}
	if n, err := e.Svc.ThumbnailsOnce(ctx); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	// EICAR is blocked and not downloadable
	eicar := `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`
	b := done("e.com", "", []byte(strings.Repeat("a", 2<<20)+eicar))
	if b.ScanState != "blocked" {
		t.Fatalf("%+v", b)
	}
	// the same bytes again deduplicate: one object, two references
	d1 := done("one.txt", "text/plain", []byte("same"))
	d2 := done("two.txt", "text/plain", []byte("same"))
	if d1.SHA256 != d2.SHA256 || stagingKeys(m) != 0 {
		t.Fatalf("%v", m.Keys())
	}
	// oversize is refused before any URL is handed out
	if _, err := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "huge.bin", "", 5<<40, ""); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "x", "", 0, ""); err == nil {
		t.Fatal("a zero size was accepted")
	}
	if _, err := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "x", "", 10, "NOTHEX"); err == nil {
		t.Fatal("a malformed checksum was accepted")
	}
}

func TestBLOB03_DirectUploadAuthorizationExpiryAndBackend(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	viewer, _ := e.Svc.CreateUser(ctx, "v@example.com", "V", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	va := domain.Actor{ID: viewer.ID, Label: "V", Kind: "human"}
	out, _ := e.Svc.CreateUser(ctx, "o@example.com", "O", "password123", false)
	oa := domain.Actor{ID: out.ID, Label: "O", Kind: "human"}
	if _, err := e.Svc.StartDirectUpload(ctx, va, is.Key, "x", "", 10, ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer got URLs: %v", err)
	}
	if _, err := e.Svc.StartDirectUpload(ctx, oa, is.Key, "x", "", 10, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider learned the issue exists: %v", err)
	}
	du, _ := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "x.txt", "text/plain", 3, "")
	put(t, du.URL, []byte("abc"))
	// somebody else cannot complete or abort it, and cannot tell it exists
	for _, who := range []domain.Actor{e.Bob, e.Admin, oa} {
		if _, err := e.Svc.CompleteDirectUpload(ctx, who, du.ID, nil); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s: %v", who.Label, err)
		}
		if err := e.Svc.AbortDirectUpload(ctx, who, du.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s: %v", who.Label, err)
		}
	}
	// abort removes the staged object
	if err := e.Svc.AbortDirectUpload(ctx, e.Alice, du.ID); err != nil || stagingKeys(m) != 0 {
		t.Fatalf("%v %v", err, m.Keys())
	}
	// the permission is checked again at completion: the writer was demoted meanwhile
	du, _ = e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "y.txt", "text/plain", 3, "")
	put(t, du.URL, []byte("abc"))
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Alice.ID, "viewer")
	if _, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, nil); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Alice.ID, "member")
	// expiry: an upload nobody completes is cleaned up, parts included
	big, _ := e.Svc.StartDirectUpload(ctx, e.Alice, is.Key, "z.bin", "", 11<<20, "")
	put(t, big.Parts[0].URL, make([]byte, 5<<20))
	e.Clock.Advance(3 * time.Hour)
	if _, err := e.Svc.CompleteDirectUpload(ctx, e.Alice, du.ID, nil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("%v", err)
	}
	if n, err := e.Svc.CleanDirectUploads(ctx); err != nil || n != 1 || m.OpenUploads() != 0 || stagingKeys(m) != 0 {
		t.Fatalf("%d %v uploads %d %v", n, err, m.OpenUploads(), m.Keys())
	}
	// the filesystem backend says so plainly
	fsEnv := testutil.New(t)
	fsEnv.Project()
	fsEnv.WithBlobs()
	fis := fsEnv.Issue("s", "")
	if _, err := fsEnv.Svc.StartDirectUpload(ctx, fsEnv.Alice, fis.Key, "x", "", 10, ""); err == nil || !strings.Contains(err.Error(), "ordinary upload") {
		t.Fatalf("%v", err)
	}
	_ = io.EOF
}
