package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/testutil/s3mock"
)

type rig struct {
	t     *testing.T
	m     *s3mock.Server
	c     *blob.S3Client
	s     *blob.S3
	sleep []time.Duration
	now   time.Time
}

func newRig(t *testing.T, mod func(*blob.S3Config)) *rig {
	m := s3mock.New()
	t.Cleanup(m.Close)
	r := &rig{t: t, m: m, now: time.Now()}
	cfg := blob.S3Config{Endpoint: m.URL, Region: s3mock.Region, Bucket: s3mock.Bucket, AccessKey: s3mock.AccessKey, SecretKey: s3mock.SecretKey,
		Sleep: func(d time.Duration) { r.sleep = append(r.sleep, d) }, Now: func() time.Time { return r.now }}
	if mod != nil {
		mod(&cfg)
	}
	c, err := blob.NewS3Client(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := blob.NewS3(c, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.c, r.s = c, s
	m.Now = func() time.Time { return r.now }
	return r
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (r *rig) commit(data []byte) (string, bool) {
	r.t.Helper()
	id, f, err := r.s.NewStaging()
	if err != nil {
		r.t.Fatal(err)
	}
	f.Write(data)
	f.Close()
	existed, err := r.s.Commit(id, sha(data))
	if err != nil {
		r.t.Fatal(err)
	}
	return sha(data), existed
}

func TestS3_StoreRoundTripRangeDedupeAndDelete(t *testing.T) {
	r := newRig(t, nil)
	data := bytes.Repeat([]byte("tasklane blob "), 1000)
	h, existed := r.commit(data)
	if existed {
		t.Fatal("first commit reported existing")
	}
	key := h[:2] + "/" + h[2:4] + "/" + h
	if got, ok := r.m.Get(key); !ok || !bytes.Equal(got, data) {
		t.Fatalf("the object is not under its hash key %s: %v", key, r.m.Keys())
	}
	f, err := r.s.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := io.ReadAll(f)
	if !bytes.Equal(all, data) {
		t.Fatal("read back differs")
	}
	f.Seek(-14, io.SeekEnd) // a ranged read, as a download resume or the thumbnailer would do
	tail, _ := io.ReadAll(f)
	f.Seek(14, io.SeekStart)
	mid := make([]byte, 14)
	io.ReadFull(f, mid)
	f.Close()
	if string(tail) != "tasklane blob " || string(mid) != "tasklane blob " {
		t.Fatalf("%q %q", tail, mid)
	}
	// the same bytes again: deduplicated, nothing uploaded a second time, staging cleaned
	puts := r.m.Count("PUT", "")
	if _, existed := r.commit(data); !existed || r.m.Count("PUT", "") != puts {
		t.Fatalf("existed=%v puts %d -> %d", existed, puts, r.m.Count("PUT", ""))
	}
	if err := r.s.Remove(h); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Open(h); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := r.s.Remove(h); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := r.s.Open("../etc/passwd"); err == nil {
		t.Fatal("a path was accepted as a locator")
	}
	// small derived objects
	png := []byte("\x89PNG small")
	ph, err := r.s.Put(png)
	if err != nil || ph != sha(png) {
		t.Fatal(err)
	}
	if _, err := r.s.Put(png); err != nil {
		t.Fatal(err)
	}
}

func TestS3_MultipartForLargeObjectsAndAbortOnFailure(t *testing.T) {
	r := newRig(t, func(c *blob.S3Config) { c.PartSize = 5 << 20 })
	big := make([]byte, 11<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	h, _ := r.commit(big)
	key := h[:2] + "/" + h[2:4] + "/" + h
	if got, _ := r.m.Get(key); !bytes.Equal(got, big) {
		t.Fatal("multipart object differs")
	}
	if n := r.m.Count("PUT", "partNumber"); n != 3 { // 5 + 5 + 1 MiB
		t.Fatalf("%d parts", n)
	}
	if r.m.OpenUploads() != 0 {
		t.Fatal("an upload was left open")
	}
	// a failure in the middle aborts the upload and leaves nothing visible
	r.m.Fail(1, 403, "AccessDenied", func(req *http.Request) bool { return strings.Contains(req.URL.RawQuery, "partNumber=2") })
	other := make([]byte, 11<<20)
	other[0] = 1
	id, f, _ := r.s.NewStaging()
	f.Write(other)
	f.Close()
	if _, err := r.s.Commit(id, sha(other)); err == nil {
		t.Fatal("a failed upload reported success")
	}
	if r.m.OpenUploads() != 0 {
		t.Fatal("the failed upload was not aborted")
	}
	oh := sha(other)
	if _, ok := r.m.Get(oh[:2] + "/" + oh[2:4] + "/" + oh); ok {
		t.Fatal("a partial object became visible")
	}
	// the staging file survives a failed commit so the caller can retry
	p, _ := r.s.StagingPath(id)
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the staging file was lost")
	}
	if _, err := r.s.Commit(id, oh); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
}

func TestS3_RetriesBackoffAndPermanentErrors(t *testing.T) {
	r := newRig(t, nil)
	r.m.Fail(2, 503, "SlowDown", nil) // throttled twice, then fine
	if _, existed := r.commit([]byte("throttled")); existed {
		t.Fatal()
	}
	if len(r.sleep) < 2 || r.sleep[1] < r.sleep[0] { // exponential backoff
		t.Fatalf("backoff %v", r.sleep)
	}
	// 403 is not retried: the first answer is final, with the service's reason
	r.sleep = nil
	before := len(r.m.Requests())
	r.m.Fail(1, 403, "AccessDenied", nil)
	err := r.c.PutBytes(testCtx(), "k", []byte("x"), "")
	var se *blob.S3Error
	if !errors.As(err, &se) || se.Status != 403 || se.Code != "AccessDenied" || len(r.m.Requests())-before != 1 || len(r.sleep) != 0 {
		t.Fatalf("%v requests %d sleeps %d", err, len(r.m.Requests())-before, len(r.sleep))
	}
	// an outage: five attempts, then a clear error
	r.sleep = nil
	r.m.Fail(10, 500, "InternalError", nil)
	if err := r.s.WriteCheck(); err == nil {
		t.Fatal("an outage looked healthy")
	}
	if len(r.sleep) != 4 {
		t.Fatalf("%d backoffs for 5 attempts", len(r.sleep))
	}
}

func TestS3_SignaturesAreVerifiedAndIntegrityChecked(t *testing.T) {
	r := newRig(t, nil)
	bad, err := blob.NewS3Client(blob.S3Config{Endpoint: r.m.URL, Region: s3mock.Region, Bucket: s3mock.Bucket, AccessKey: s3mock.AccessKey, SecretKey: "wrong", Now: func() time.Time { return r.now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.HeadBucket(testCtx()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a wrong secret was accepted: %v", err)
	}
	if err := r.c.HeadBucket(testCtx()); err != nil {
		t.Fatal(err)
	}
	// clock skew beyond 15 minutes is refused, as AWS does
	r.now = r.now.Add(-time.Hour)
	r.m.Now = time.Now
	if err := r.c.HeadBucket(testCtx()); err == nil {
		t.Fatal("a request an hour off was accepted")
	}
	r.now = time.Now()
	// the checksum the client sends is verified by the service: bytes that do not match their hash are refused
	id, f, _ := r.s.NewStaging()
	f.Write([]byte("real bytes"))
	f.Close()
	if _, err := r.s.Commit(id, sha([]byte("other bytes"))); err == nil || !strings.Contains(err.Error(), "BadDigest") {
		t.Fatalf("%v", err)
	}
	if len(r.m.Keys()) != 0 {
		t.Fatalf("a corrupt object was stored: %v", r.m.Keys())
	}
}

func TestS3_WalkPagesAndIgnoresForeignKeysAndHonoursPrefix(t *testing.T) {
	r := newRig(t, func(c *blob.S3Config) { c.Prefix = "tl/" })
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		h, _ := r.commit([]byte{byte(i), 1, 2, 3})
		want[h] = true
	}
	r.c.PutBytes(testCtx(), "notes/readme.txt", []byte("not a blob"), "") // foreign objects under the same prefix
	n := 0
	err := r.s.Walk(func(h string, size int64) error {
		if !want[h] || size != 4 {
			t.Errorf("unexpected %s %d", h, size)
		}
		n++
		return nil
	})
	if err != nil || n != 5 {
		t.Fatalf("%v walked %d", err, n)
	}
	for _, k := range r.m.Keys() {
		if !strings.HasPrefix(k, "tl/") {
			t.Fatalf("a key outside the prefix: %s", k)
		}
	}
}

func TestS3_PresignedURLsWorkTamperingAndExpiryDoNot(t *testing.T) {
	r := newRig(t, nil)
	h, _ := r.commit([]byte("download me"))
	key := h[:2] + "/" + h[2:4] + "/" + h
	url := r.c.Presign("GET", key, time.Minute, 0, "")
	get := func(u string) (int, string) {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(url); code != 200 || body != "download me" {
		t.Fatalf("%d %q", code, body)
	}
	if code, _ := get(strings.Replace(url, "X-Amz-Expires=60", "X-Amz-Expires=6000", 1)); code != 403 {
		t.Fatalf("a changed expiry was accepted: %d", code)
	}
	other := r.c.Presign("GET", "ab/cd/"+strings.Repeat("0", 64), time.Minute, 0, "")
	if code, _ := get(strings.Replace(other, "ab/cd/"+strings.Repeat("0", 64), key, 1)); code != 403 { // a signature for one key does not open another
		t.Fatalf("a signature for another key worked: %d", code)
	}
	r.now = r.now.Add(2 * time.Minute)
	if code, _ := get(url); code != 403 {
		t.Fatalf("an expired URL worked: %d", code)
	}
	// a presigned PUT stores an object a browser could upload
	r.now = time.Now()
	put := r.c.Presign("PUT", "staging/abc", time.Minute, 0, "")
	req, _ := http.NewRequest("PUT", put, strings.NewReader("direct"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	if b, _ := r.m.Get("staging/abc"); string(b) != "direct" {
		t.Fatal("the presigned upload did not land")
	}
}

func TestS3_ServerSideEncryptionHeaderAndConfigValidation(t *testing.T) {
	r := newRig(t, func(c *blob.S3Config) { c.SSE = "aws:kms"; c.KMSKeyID = "alias/tasklane" })
	r.commit([]byte("secret"))
	if r.m.SawSSE != "aws:kms" {
		t.Fatalf("%q", r.m.SawSSE)
	}
	for name, c := range map[string]blob.S3Config{
		"no bucket":    {AccessKey: "a", SecretKey: "b"},
		"no secret":    {Bucket: "b", AccessKey: "a"},
		"tiny parts":   {Bucket: "b", AccessKey: "a", SecretKey: "s", PartSize: 1 << 20},
		"bad sse":      {Bucket: "b", AccessKey: "a", SecretKey: "s", SSE: "rot13"},
		"bad endpoint": {Bucket: "b", AccessKey: "a", SecretKey: "s", Endpoint: "::nope"},
	} {
		if _, err := blob.NewS3Client(c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// virtual-hosted style for AWS itself, path style for custom endpoints
	aws, _ := blob.NewS3Client(blob.S3Config{Bucket: "bkt", Region: "eu-west-1", AccessKey: "a", SecretKey: "s"})
	if u := aws.Presign("GET", "k", time.Minute, 0, ""); !strings.HasPrefix(u, "https://bkt.s3.eu-west-1.amazonaws.com/k?") {
		t.Fatal(u)
	}
	pathStyle, _ := blob.NewS3Client(blob.S3Config{Bucket: "bkt", Endpoint: "http://minio:9000", AccessKey: "a", SecretKey: "s"})
	if u := pathStyle.Presign("GET", "k", time.Minute, 0, ""); !strings.HasPrefix(u, "http://minio:9000/bkt/k?") {
		t.Fatal(u)
	}
}

func TestS3_WriteCheckReflectsHealth(t *testing.T) {
	r := newRig(t, nil)
	if err := r.s.WriteCheck(); err != nil {
		t.Fatal(err)
	}
	missing, _ := blob.NewS3Client(blob.S3Config{Endpoint: r.m.URL, Region: s3mock.Region, Bucket: "nope", AccessKey: s3mock.AccessKey, SecretKey: s3mock.SecretKey, Now: func() time.Time { return r.now }})
	ms, _ := blob.NewS3(missing, t.TempDir())
	if err := ms.WriteCheck(); err == nil {
		t.Fatal("a missing bucket looked healthy")
	}
}

func testCtx() context.Context { return context.Background() }
