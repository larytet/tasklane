package domain_test

import (
	"bytes"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func keyOf(sha string) string { return sha[:2] + "/" + sha[2:4] + "/" + sha }

// The attachment features run unchanged on the S3 backend (BLOB-01, 06, 08, 11, 12, 21 against the in-process double).
func TestS3Backend_ScreenshotThumbnailAndDownload(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	st, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	orig := pngBytes(600, 400)
	att := attach(t, e, e.Alice, is.Key, "shot.png", "image/png", orig)
	if got, ok := m.Get(keyOf(att.SHA256)); !ok || !bytes.Equal(got, orig) {
		t.Fatal("the original is not in the bucket under its hash")
	}
	if n, err := e.Svc.ThumbnailsOnce(ctx); err != nil || n != 1 { // the thumbnailer reads the object back through ranged requests
		t.Fatalf("%d %v", n, err)
	}
	di, _ := e.Svc.FileInfo(ctx, e.Alice, att.ID, true)
	f, err := st.Open(di.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(f)
	f.Close()
	if err != nil || img.Bounds().Dx() != 256 {
		t.Fatalf("%v %v", err, img.Bounds())
	}
	o, _ := e.Svc.FileInfo(ctx, e.Alice, att.ID, false)
	rd, err := e.Svc.OpenBlob(o.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rd)
	rd.Close()
	if !bytes.Equal(got, orig) {
		t.Fatal("download differs")
	}
}

func TestS3Backend_DedupeRefcountAndGC(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	a1 := attach(t, e, e.Alice, is.Key, "a.txt", "text/plain", []byte("same bytes"))
	a2 := attach(t, e, e.Alice, is.Key, "b.txt", "text/plain", []byte("same bytes"))
	if a1.SHA256 != a2.SHA256 || len(m.Keys()) != 1 || m.Count("PUT", keyOf(a1.SHA256)) != 1 {
		t.Fatalf("keys %v puts %d", m.Keys(), m.Count("PUT", keyOf(a1.SHA256)))
	}
	e.Svc.DeleteAttachment(ctx, e.Alice, is.Key, a1.ID)
	if n, _ := e.Svc.GC(ctx); n != 0 || len(m.Keys()) != 1 {
		t.Fatalf("collected a blob that is still referenced: %d", n)
	}
	e.Svc.DeleteAttachment(ctx, e.Alice, is.Key, a2.ID)
	if n, err := e.Svc.GC(ctx); err != nil || n != 1 || len(m.Keys()) != 0 {
		t.Fatalf("%d %v %v", n, err, m.Keys())
	}
}

func TestS3Backend_LargeUploadUsesMultipart(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	big := make([]byte, 12<<20)
	for i := range big {
		big[i] = byte(i * 13)
	}
	att := attach(t, e, e.Alice, is.Key, "big.bin", "application/octet-stream", big)
	if m.Count("PUT", "partNumber") != 3 {
		t.Fatalf("%d parts", m.Count("PUT", "partNumber"))
	}
	if got, _ := m.Get(keyOf(att.SHA256)); !bytes.Equal(got, big) {
		t.Fatal("multipart content differs")
	}
}

func TestS3Backend_OutageLeavesNothingVisibleAndRetrySucceeds(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	_, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	m.Fail(100, 503, "SlowDown", nil) // the store is throttling everything
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "x.txt", "text/plain", strings.NewReader("hello")); err == nil {
		t.Fatal("an upload succeeded during an outage")
	}
	if len(m.Keys()) != 0 {
		t.Fatalf("a partial blob became visible: %v", m.Keys())
	}
	if v, _ := e.Svc.View(ctx, e.Alice, is.Key); len(v.Attachments) != 0 {
		t.Fatal("an attachment row exists for a blob that was never stored")
	}
	var rows int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM blob`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d blob rows for nothing stored", rows)
	}
	m.Reset()
	if _, err := e.Svc.AttachStream(ctx, e.Alice, is.Key, "x.txt", "text/plain", strings.NewReader("hello")); err != nil {
		t.Fatalf("after the outage: %v", err)
	}
}

func TestS3Backend_FsckFindsRotMissingAndOrphans(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	st, m := e.WithS3Blobs()
	is := e.Issue("s", "")
	a1 := attach(t, e, e.Alice, is.Key, "one.txt", "text/plain", []byte("one"))
	a2 := attach(t, e, e.Alice, is.Key, "two.txt", "text/plain", []byte("two"))
	if r, err := e.Svc.Fsck(ctx, false); err != nil || !r.Clean() || r.Checked != 2 {
		t.Fatalf("%v %+v", err, r)
	}
	m.Tamper(keyOf(a1.SHA256), []byte("rotten"))
	st.Remove(a2.SHA256)
	orphan, _ := st.Put([]byte("nobody references me"))
	r, _ := e.Svc.Fsck(ctx, false)
	if len(r.Corrupt) != 1 || r.Corrupt[0] != a1.SHA256 || len(r.Missing) != 1 || r.Missing[0] != a2.SHA256 || len(r.Orphans) != 1 || r.Orphans[0] != orphan {
		t.Fatalf("%+v", r)
	}
	e.Svc.Fsck(ctx, true)
	if r, _ = e.Svc.Fsck(ctx, false); len(r.Corrupt) != 0 || len(r.Orphans) != 0 {
		t.Fatalf("after repair: %+v", r)
	}
	if _, err := e.Svc.OpenBlob(a1.SHA256); err == nil || err != blob.ErrNotFound {
		t.Fatalf("%v", err)
	}
}

func TestS3Backend_EICARIsBlockedAndBundlesRoundTrip(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.WithS3Blobs()
	is := e.Issue("s", "")
	eicar := `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`
	bad := attach(t, e, e.Alice, is.Key, "eicar.txt", "text/plain", []byte(eicar))
	if bad.ScanState != "blocked" {
		t.Fatalf("%+v", bad)
	}
	good := attach(t, e, e.Alice, is.Key, "notes.txt", "text/plain", []byte("keep these notes"))
	// export from S3, import into a second instance on its own S3
	o := domain.DefaultExport()
	b, _ := exportBytes(t, e, e.Admin, o)
	dst := testutil.New(t)
	dst.WithS3Blobs()
	importBytes(t, dst, b, domain.ImportOpts{})
	v, err := dst.Svc.View(ctx, dst.Admin, "ABC-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, at := range v.Attachments {
		if at.SHA256 == good.SHA256 {
			f, err := dst.Svc.OpenBlob(at.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(f)
			f.Close()
			found = string(data) == "keep these notes"
		}
	}
	if !found {
		t.Fatal("the attachment did not survive an S3 to S3 round trip")
	}
}
