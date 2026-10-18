package blob_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/config"
)

func key() []byte { k := make([]byte, 32); rand.Read(k); return k }

func objFile(root, sha string) string { return filepath.Join(root, sha[:2], sha[2:4], sha) }

func newEnc(t *testing.T, cur byte, keys map[byte][]byte) (*blob.Encrypted, string) {
	dir := t.TempDir()
	fs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := blob.NewEncrypted(fs, cur, keys)
	if err != nil {
		t.Fatal(err)
	}
	return e, dir
}

func put(t *testing.T, s blob.Store, b []byte) string {
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
	return sha
}

func readAll(t *testing.T, s blob.Store, sha string) ([]byte, error) {
	o, err := s.Open(sha)
	if err != nil {
		return nil, err
	}
	defer o.Close()
	return io.ReadAll(o)
}

func TestBLOB16_RoundTripAtEveryChunkBoundaryAndNothingReadableAtRest(t *testing.T) {
	e, dir := newEnc(t, 1, map[byte][]byte{1: key()})
	for _, n := range []int{0, 1, 65535, 65536, 65537, 131072, 200000} {
		b := make([]byte, n)
		rand.Read(b)
		copy(b, "PLAINTEXT-MARKER")
		sha := put(t, e, b)
		got, err := readAll(t, e, sha)
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("size %d: %v equal=%v", n, err, bytes.Equal(got, b))
		}
		raw, _ := os.ReadFile(objFile(dir, sha))
		if n >= 16 && bytes.Contains(raw, []byte("PLAINTEXT-MARKER")) {
			t.Fatalf("size %d: the stored file contains the plaintext", n)
		}
		if len(raw) <= n {
			t.Fatalf("size %d: stored file is not larger than the content (%d)", n, len(raw))
		}
		// ranges: seek into the middle, across a chunk boundary, and from the end
		if n > 70000 {
			o, _ := e.Open(sha)
			o.Seek(65530, io.SeekStart)
			part := make([]byte, 20)
			io.ReadFull(o, part)
			if !bytes.Equal(part, b[65530:65550]) {
				t.Fatal("a range across a chunk boundary is wrong")
			}
			end, _ := o.Seek(-5, io.SeekEnd)
			tail, _ := io.ReadAll(o)
			if end != int64(n-5) || !bytes.Equal(tail, b[n-5:]) {
				t.Fatal("a range from the end is wrong")
			}
			o.Close()
		}
	}
}

func TestBLOB16_AlteredCutOffSwappedOrForeignObjectsAreRefused(t *testing.T) {
	e, dir := newEnc(t, 1, map[byte][]byte{1: key()})
	b := make([]byte, 200000)
	rand.Read(b)
	sha := put(t, e, b)
	path := objFile(dir, sha)
	orig, _ := os.ReadFile(path)

	mutate := func(name string, f func([]byte) []byte) {
		os.Chmod(path, 0o600)
		os.WriteFile(path, f(append([]byte{}, orig...)), 0o600)
		if _, err := readAll(t, e, sha); err == nil {
			t.Errorf("%s: read succeeded", name)
		}
	}
	mutate("flipped byte in a chunk", func(x []byte) []byte { x[len(x)/2] ^= 1; return x })
	mutate("flipped byte in the wrapped key", func(x []byte) []byte { x[30] ^= 1; return x })
	full := 65536 + 16
	mutate("cut at a chunk boundary", func(x []byte) []byte { return x[:65+2*full] })
	mutate("chunks reordered", func(x []byte) []byte {
		c := append([]byte{}, x[65:65+full]...)
		copy(x[55:], x[55+full:65+2*full])
		copy(x[65+full:], c)
		return x
	})
	os.Chmod(path, 0o600)
	os.WriteFile(path, orig, 0o600)
	if got, err := readAll(t, e, sha); err != nil || !bytes.Equal(got, b) {
		t.Fatal("restoring the file did not restore the object")
	}
	// an object copied under another name does not decrypt
	other := strings.Repeat("ab", 32)
	os.MkdirAll(filepath.Dir(objFile(dir, other)), 0o700)
	os.WriteFile(objFile(dir, other), orig, 0o600)
	if _, err := readAll(t, e, other); err == nil {
		t.Fatal("an object decrypted under a different name")
	}
	// a wrong or missing master key
	fs, _ := blob.NewFS(dir)
	wrong, _ := blob.NewEncrypted(fs, 1, map[byte][]byte{1: key()})
	if _, err := wrong.Open(sha); err == nil {
		t.Fatal("opened with the wrong key")
	}
	missing, _ := blob.NewEncrypted(fs, 2, map[byte][]byte{2: key()})
	if _, err := missing.Open(sha); err == nil || !strings.Contains(err.Error(), "master key 1") {
		t.Fatalf("opened without its key, or the error does not name it: %v", err)
	}
}

func TestBLOB16_RotationRewritesTheHeaderOnlyAndTheOldKeyCanGo(t *testing.T) {
	k1, k2 := key(), key()
	e1, dir := newEnc(t, 1, map[byte][]byte{1: k1})
	var shas []string
	want := map[string][]byte{}
	for _, n := range []int{10, 70000, 300000} {
		b := make([]byte, n)
		rand.Read(b)
		sha := put(t, e1, b)
		shas = append(shas, sha)
		want[sha] = b
	}
	before, _ := os.ReadFile(objFile(dir, shas[2]))

	fs, _ := blob.NewFS(dir)
	e2, _ := blob.NewEncrypted(fs, 2, map[byte][]byte{1: k1, 2: k2})
	if rep := e2.RewrapAll(shas); rep.Changed != 3 || len(rep.Failed) != 0 {
		t.Fatalf("%+v", rep)
	}
	if rep := e2.RewrapAll(shas); rep.Changed != 0 {
		t.Fatalf("a second pass changed %d objects", rep.Changed)
	}
	after, _ := os.ReadFile(objFile(dir, shas[2]))
	if after[4] != 2 || before[4] != 1 {
		t.Fatalf("key ids %d -> %d", before[4], after[4])
	}
	if !bytes.Equal(before[65:], after[65:]) {
		t.Fatal("rotation re-encrypted the content; it must only rewrap the key")
	}
	// now the old key is retired: everything still reads
	only2, _ := blob.NewEncrypted(fs, 2, map[byte][]byte{2: k2})
	for sha, b := range want {
		if got, err := readAll(t, only2, sha); err != nil || !bytes.Equal(got, b) {
			t.Fatalf("%s: %v", sha[:8], err)
		}
	}
	if _, err := readAll(t, e1, shas[0]); err == nil {
		t.Fatal("the retired key still opens a rewrapped object")
	}
}

func TestBLOB16_ObjectsStoredBeforeEncryptionAreServedAndThenEncrypted(t *testing.T) {
	dir := t.TempDir()
	plain, _ := blob.NewFS(dir)
	b := make([]byte, 90000)
	rand.Read(b)
	copy(b, "OLD-PLAINTEXT-MARKER")
	sha := put(t, plain, b)
	e, _ := blob.NewEncrypted(plain, 7, map[byte][]byte{7: key()})
	if got, err := readAll(t, e, sha); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("an old object is not served as it is: %v", err)
	}
	if ch, err := e.Rewrap(sha); err != nil || !ch {
		t.Fatalf("%v %v", ch, err)
	}
	raw, _ := os.ReadFile(objFile(dir, sha))
	if bytes.Contains(raw, []byte("OLD-PLAINTEXT-MARKER")) {
		t.Fatal("the old object is still readable at rest after rewrap")
	}
	if got, err := readAll(t, e, sha); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("after rewrap: %v", err)
	}
}

func TestBLOB16_DeduplicationAndThumbnailsGoThroughEncryption(t *testing.T) {
	e, dir := newEnc(t, 1, map[byte][]byte{1: key()})
	b := []byte("same bytes twice")
	a := put(t, e, b)
	if c := put(t, e, b); c != a {
		t.Fatal("the same content got two names")
	}
	sha, err := e.Put([]byte("derived thumbnail bytes"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(objFile(dir, sha))
	if bytes.Contains(raw, []byte("derived thumbnail")) {
		t.Fatal("derived data is stored in the clear")
	}
	if got, _ := readAll(t, e, sha); string(got) != "derived thumbnail bytes" {
		t.Fatalf("%q", got)
	}
}

func TestBLOB16_ConfigurationOfTheKeys(t *testing.T) {
	good := strings.Repeat("ab", 32)
	dir := t.TempDir()
	base := config.Config{BlobBackend: "fs", BlobDir: dir}
	ok := base
	ok.BlobEncKeys, ok.BlobEncCurrent = "1:"+good+",2:"+strings.Repeat("cd", 32), "2"
	s, err := blob.NewFromConfig(ok)
	if err != nil {
		t.Fatal(err)
	}
	if _, isEnc := s.(*blob.Encrypted); !ok2(isEnc) {
		t.Fatalf("%T", s)
	}
	bad := map[string]config.Config{
		"no current":   {BlobBackend: "fs", BlobDir: dir, BlobEncKeys: "1:" + good},
		"unknown cur":  {BlobBackend: "fs", BlobDir: dir, BlobEncKeys: "1:" + good, BlobEncCurrent: "9"},
		"short key":    {BlobBackend: "fs", BlobDir: dir, BlobEncKeys: "1:abcd", BlobEncCurrent: "1"},
		"not hex":      {BlobBackend: "fs", BlobDir: dir, BlobEncKeys: "1:" + strings.Repeat("zz", 32), BlobEncCurrent: "1"},
		"bad id":       {BlobBackend: "fs", BlobDir: dir, BlobEncKeys: "300:" + good, BlobEncCurrent: "1"},
		"duplicate id": {BlobBackend: "fs", BlobDir: dir, BlobEncKeys: "1:" + good + ",1:" + good, BlobEncCurrent: "1"},
	}
	for name, c := range bad {
		if _, err := blob.NewFromConfig(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if s, _ := blob.NewFromConfig(base); func() bool { _, e := s.(*blob.Encrypted); return e }() {
		t.Fatal("encryption is on without keys")
	}
}

func ok2(b bool) bool { return b }
