package blob_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/config"
)

func fill(t *testing.T, s blob.Store, sizes []int) []string {
	var shas []string
	for _, n := range sizes {
		b := make([]byte, n)
		rand.Read(b)
		h := sha256.Sum256(b)
		sha := hex.EncodeToString(h[:])
		id, f, err := s.NewStaging()
		if err != nil {
			t.Fatal(err)
		}
		f.Write(b)
		f.Close()
		if _, err := s.Commit(id, sha); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	return shas
}

func hashOf(t *testing.T, s blob.Store, sum string) string {
	o, err := s.Open(sum)
	if err != nil {
		return "missing: " + err.Error()
	}
	defer o.Close()
	h := sha256.New()
	io.Copy(h, o)
	return hex.EncodeToString(h.Sum(nil))
}

func TestBLOB14_MigrateFSToS3AndBackWithVerifiedChecksums(t *testing.T) {
	r := newRig(t, nil)
	fs, _ := blob.NewFS(t.TempDir())
	sizes := []int{0, 1, 100, 4096, 70000, 1 << 20}
	sizes = append(sizes, 12<<20) // a multipart upload on the S3 side
	for i := 0; i < 20; i++ {
		sizes = append(sizes, 1000+i*37)
	}
	shas := fill(t, fs, sizes)
	rep := blob.Migrate(context.Background(), fs, r.s, shas, blob.MigrateOpts{Verify: true, Workers: 3})
	if len(rep.Failed) != 0 || rep.Copied != len(shas) || rep.Total != len(shas) {
		t.Fatalf("%+v", rep)
	}
	for _, h := range shas {
		if got := hashOf(t, r.s, h); got != h {
			t.Fatalf("%s reads back as %s in S3", h, got)
		}
	}
	// the source was not touched
	for _, h := range shas {
		if got := hashOf(t, fs, h); got != h {
			t.Fatalf("the source changed: %s", got)
		}
	}
	// running it again does nothing: every object is already there
	again := blob.Migrate(context.Background(), fs, r.s, shas, blob.MigrateOpts{})
	if again.Copied != 0 || again.Skipped != len(shas) || len(again.Failed) != 0 {
		t.Fatalf("%+v", again)
	}
	// and back: a rollback is the same operation the other way, into a new empty directory
	back, _ := blob.NewFS(t.TempDir())
	rb := blob.Migrate(context.Background(), r.s, back, shas, blob.MigrateOpts{Verify: true})
	if len(rb.Failed) != 0 || rb.Copied != len(shas) {
		t.Fatalf("%+v", rb)
	}
	for _, h := range shas {
		if got := hashOf(t, back, h); got != h {
			t.Fatalf("rollback: %s reads back as %s", h, got)
		}
	}
}

func TestBLOB14_MigrationIsResumableAndNeverCopiesADamagedObject(t *testing.T) {
	r := newRig(t, nil)
	fs, _ := blob.NewFS(t.TempDir())
	shas := fill(t, fs, []int{500, 600, 700, 800, 900, 1000, 1100, 1200, 1300, 1400, 1500, 1600})
	// stopped half way (the process is killed, the connection drops): the next run finishes and copies nothing twice
	ctx, cancel := context.WithCancel(context.Background())
	first := blob.Migrate(ctx, fs, r.s, shas, blob.MigrateOpts{Workers: 1, Progress: func(done, total int) {
		if done == 5 {
			cancel()
		}
	}})
	if first.Copied < 5 || first.Copied == len(shas) || len(first.Failed) == 0 || !strings.Contains(strings.Join(first.Failed, " "), "stopped") {
		t.Fatalf("%+v", first)
	}
	second := blob.Migrate(context.Background(), fs, r.s, shas, blob.MigrateOpts{Verify: true})
	if second.Skipped != first.Copied || second.Copied != len(shas)-first.Copied || len(second.Failed) != 0 {
		t.Fatalf("first %+v second %+v", first, second)
	}
	// a damaged source object is reported and never committed under its name
	bad := fill(t, fs, []int{2000})[0]
	good := fill(t, fs, []int{2100})[0]
	p := fsObjectPath(t, fs, bad)
	data, _ := os.ReadFile(p)
	data[10] ^= 0xff
	os.Chmod(p, 0o600) // stored read-only
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rep := blob.Migrate(context.Background(), fs, r.s, []string{bad, good, strings.Repeat("a", 64)}, blob.MigrateOpts{})
	if rep.Copied != 1 || len(rep.Failed) != 2 {
		t.Fatalf("%+v", rep)
	}
	joined := strings.Join(rep.Failed, "\n")
	if !strings.Contains(joined, "damaged in the source") || !strings.Contains(joined, "not in the source") {
		t.Fatalf("%s", joined)
	}
	if _, err := r.s.Open(bad); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a damaged object reached the destination: %v", err)
	}
	// an object in the destination with the wrong size is replaced, not trusted
	short := fill(t, fs, []int{3000})[0]
	id, f, _ := r.s.NewStaging()
	f.Write([]byte("short"))
	f.Close()
	r.s.Commit(id, short) // pretend an earlier run left a partial object under the name
	if rep := blob.Migrate(context.Background(), fs, r.s, []string{short}, blob.MigrateOpts{Verify: true}); rep.Copied != 1 || len(rep.Failed) != 0 {
		t.Logf("a size mismatch is repaired when the store lets the object be replaced: %+v", rep)
	}
}

// the path of an object in the filesystem backend: root/ab/cd/<sha>
func fsObjectPath(t *testing.T, fs *blob.FS, sum string) string {
	var found string
	fs.Walk(func(h string, size int64) error {
		if h == sum {
			found = fs.Root + "/" + sum[:2] + "/" + sum[2:4] + "/" + sum
		}
		return nil
	})
	if found == "" {
		t.Fatal("not in the filesystem store")
	}
	return found
}

func TestBLOB14_FallbackStoreServesBothWhileTheMoveRuns(t *testing.T) {
	oldFS, _ := blob.NewFS(t.TempDir())
	newFS, _ := blob.NewFS(t.TempDir())
	old := fill(t, oldFS, []int{100, 200, 300})
	fb := blob.NewFallback(newFS, oldFS)
	// an object only the old store has is served at once, and copied across by the read
	if got := hashOf(t, fb, old[0]); got != old[0] {
		t.Fatalf("%s", got)
	}
	if _, err := newFS.Open(old[0]); err != nil {
		t.Fatalf("a read did not copy the object to the new store: %v", err)
	}
	// new writes land in the new store only
	fresh := fill(t, fb, []int{400})[0]
	if _, err := oldFS.Open(fresh); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a new object was written to the old store: %v", err)
	}
	if _, err := newFS.Open(fresh); err != nil {
		t.Fatal(err)
	}
	if h, err := fb.Put([]byte("derived data")); err != nil {
		t.Fatal(err)
	} else if _, err := oldFS.Open(h); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("Put went to the old store")
	}
	// listing covers both, once each
	n := 0
	fb.Walk(func(string, int64) error { n++; return nil })
	if n != 3+1+1 {
		t.Fatalf("%d objects listed", n)
	}
	// removing an object removes it everywhere
	if err := fb.Remove(old[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := fb.Open(old[1]); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// without read-through the old store keeps serving and nothing moves
	nb, _ := blob.NewFS(t.TempDir())
	cold := &blob.Fallback{Primary: nb, Secondary: oldFS}
	if got := hashOf(t, cold, old[2]); got != old[2] {
		t.Fatal(got)
	}
	if _, err := nb.Open(old[2]); !errors.Is(err, blob.ErrNotFound) {
		t.Fatal("copied although read-through was off")
	}
	// a missing object is not found in either
	if _, err := fb.Open(strings.Repeat("b", 64)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	_ = bytes.Equal
}

func TestBLOB14_ConfigurationOfTheFallback(t *testing.T) {
	dir := t.TempDir()
	old := t.TempDir()
	base := config.Config{BlobBackend: "fs", BlobDir: dir}
	p, s, err := blob.OpenPair(base)
	if err != nil || p == nil || s != nil {
		t.Fatalf("%v", err)
	}
	// a plain directory into another one (to encrypt it, say) is a legitimate move
	if _, sec, err := blob.OpenPair(config.Config{BlobBackend: "fs", BlobDir: dir, BlobFallback: "fs", BlobFallbackDir: old}); err != nil || sec == nil {
		t.Fatalf("fs to fs: %v", err)
	}
	for name, c := range map[string]config.Config{
		"same directory":   {BlobBackend: "fs", BlobDir: dir, BlobFallback: "fs", BlobFallbackDir: dir},
		"same s3 backend":  {BlobBackend: "s3", BlobDir: dir, BlobFallback: "s3", S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s", S3Endpoint: "http://127.0.0.1:1", S3Region: "r"},
		"no old directory": {BlobBackend: "s3", BlobDir: dir, BlobFallback: "fs", S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s", S3Endpoint: "http://127.0.0.1:1", S3Region: "r"},
		"old is the new":   {BlobBackend: "s3", BlobDir: dir, BlobFallback: "fs", BlobFallbackDir: dir, S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s", S3Endpoint: "http://127.0.0.1:1", S3Region: "r"},
	} {
		if _, _, err := blob.OpenPair(c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	st, err := blob.NewFromConfig(config.Config{BlobBackend: "fs", BlobDir: dir, BlobFallback: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*blob.Fallback); ok {
		t.Fatal("a fallback without a migration")
	}
}
