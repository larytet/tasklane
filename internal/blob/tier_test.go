package blob_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/testutil/s3mock"
)

func newTier(t *testing.T) (*blob.Tiered, *blob.FS, *rig, string) {
	dir := t.TempDir()
	hot, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, nil)
	return blob.NewTiered(hot, r.s), hot, r, dir
}

func cold(r *rig, sha string) ([]byte, bool) { return r.m.Get(sha[:2] + "/" + sha[2:4] + "/" + sha) }

func TestBLOB15_NewObjectsAreHotAndMoveToColdWithTransparentReads(t *testing.T) {
	tr, hot, r, _ := newTier(t)
	var shas []string
	want := map[string][]byte{}
	for _, n := range []int{0, 10, 70000, 40 << 20} { // the last one is a multipart upload to the cold tier
		b := make([]byte, n)
		rand.Read(b)
		sha := put(t, tr, b)
		shas = append(shas, sha)
		want[sha] = b
		if tr.Where(sha) != "hot" {
			t.Fatalf("a new object is %s", tr.Where(sha))
		}
	}
	for _, sha := range shas {
		if err := tr.MoveCold(sha); err != nil {
			t.Fatalf("%s: %v", sha[:8], err)
		}
		if _, err := hot.Open(sha); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("the hot copy of %s is still there", sha[:8])
		}
		if _, ok := cold(r, sha); !ok {
			t.Fatalf("%s is not in the cold tier", sha[:8])
		}
		if tr.Where(sha) != "cold" {
			t.Fatalf("%s", tr.Where(sha))
		}
		got, err := readAll(t, tr, sha)
		if err != nil || !bytes.Equal(got, want[sha]) {
			t.Fatalf("%s read through the cold tier: %v", sha[:8], err)
		}
	}
	// ranges work against the cold tier too
	big := shas[3]
	o, _ := tr.Open(big)
	o.Seek(20<<20+5, io.SeekStart)
	part := make([]byte, 100)
	io.ReadFull(o, part)
	o.Close()
	if !bytes.Equal(part, want[big][20<<20+5:20<<20+105]) {
		t.Fatal("a range of a cold object is wrong")
	}
	// moving again is a no-op, an unknown object is an error
	if err := tr.MoveCold(shas[1]); err != nil {
		t.Fatal(err)
	}
	if err := tr.MoveCold(sha64('f')); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// the listing has each object once, from either tier
	seen := map[string]int{}
	tr.Walk(func(sha string, size int64) error { seen[sha]++; return nil })
	for _, sha := range shas {
		if seen[sha] != 1 {
			t.Fatalf("%s listed %d times", sha[:8], seen[sha])
		}
	}
	st := tr.Stats()
	if st.ColdReads < 4 || st.ColdAvg <= 0 {
		t.Fatalf("%+v", st)
	}
}

func sha64(c byte) string { return string(bytes.Repeat([]byte{c}, 64)) }

func TestBLOB15_AMoveThatIsInterruptedOrWrongNeverLosesTheObject(t *testing.T) {
	tr, hot, r, _ := newTier(t)
	b := make([]byte, 200000)
	rand.Read(b)
	sha := put(t, tr, b)
	// the cold tier refuses: the object stays hot and readable, and the retry works
	r.m.Fail(1, 503, "SlowDown", nil)
	r.m.Fail(1, 500, "InternalError", nil)
	var err error
	for i := 0; i < 8 && err == nil; i++ { // enough failures to exhaust the client's retries
		r.m.Fail(1, 500, "InternalError", nil)
	}
	if err = tr.MoveCold(sha); err == nil {
		t.Fatal("a failing cold tier did not fail the move")
	}
	if tr.Where(sha) != "hot" {
		t.Fatalf("after a failed move the object is %q", tr.Where(sha))
	}
	if got, err := readAll(t, tr, sha); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("%v", err)
	}
	r.m.Reset()
	// interrupted after the cold copy was written and before the hot one was removed: in both, readable, and the next move finishes
	o, _ := hot.Open(sha)
	id, f, _ := r.s.NewStaging()
	io.Copy(f, o)
	f.Close()
	o.Close()
	if _, err := r.s.Commit(id, sha); err != nil {
		t.Fatal(err)
	}
	if tr.Where(sha) != "both" {
		t.Fatalf("%q", tr.Where(sha))
	}
	if got, err := readAll(t, tr, sha); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("%v", err)
	}
	if err := tr.MoveCold(sha); err != nil || tr.Where(sha) != "cold" {
		t.Fatalf("%v %q", err, tr.Where(sha))
	}
	// a cold copy that differs from the hot one (damaged in transit or by a bad earlier run) is detected and the hot copy kept
	c := make([]byte, 5000)
	rand.Read(c)
	sha2 := put(t, tr, c)
	other := append([]byte{}, c...)
	other[100] ^= 0xff
	id, f, _ = r.s.NewStaging()
	f.Write(other)
	f.Close()
	if _, err := blob.CommitRaw(r.s, id, sha2, false); err != nil {
		t.Fatal(err)
	}
	err = tr.MoveCold(sha2)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("differs")) || !bytes.Contains([]byte(err.Error()), []byte("hot copy is kept")) {
		t.Fatalf("%v", err)
	}
	if _, herr := hot.Open(sha2); herr != nil {
		t.Fatal("the hot copy was removed after a mismatch")
	}
}

func TestBLOB15_DeduplicationRemovalAndWritesAcrossTiers(t *testing.T) {
	tr, _, r, _ := newTier(t)
	b := []byte("cold content")
	sha := put(t, tr, b)
	tr.MoveCold(sha)
	// storing the same content again does not create a hot copy
	if again := put(t, tr, b); again != sha || tr.Where(sha) != "cold" {
		t.Fatalf("%q", tr.Where(sha))
	}
	if sha2, _ := tr.Put(b); sha2 != sha || tr.Where(sha) != "cold" {
		t.Fatalf("Put made a hot copy: %q", tr.Where(sha))
	}
	// removing removes from wherever it is, and from both when a move was interrupted
	if err := tr.Remove(sha); err != nil || tr.Where(sha) != "" {
		t.Fatalf("%v %q", err, tr.Where(sha))
	}
	if _, ok := cold(r, sha); ok {
		t.Fatal("the cold copy survived a removal")
	}
	if err := tr.Remove(sha); err != nil {
		t.Fatalf("removing what is gone: %v", err)
	}
	if err := tr.WriteCheck(); err != nil {
		t.Fatal(err)
	}
	// a broken cold tier is reported by name
	r.m.Fail(20, 500, "InternalError", nil)
	if err := tr.WriteCheck(); err == nil || !bytes.Contains([]byte(err.Error()), []byte("cold tier")) {
		t.Fatalf("%v", err)
	}
}

func TestBLOB15_EncryptedObjectsMoveAsCiphertextAndRewrapStaysInTheirTier(t *testing.T) {
	tr, hot, r, _ := newTier(t)
	k1, k2 := key(), key()
	e1, _ := blob.NewEncrypted(tr, 1, map[byte][]byte{1: k1})
	b := make([]byte, 150000)
	rand.Read(b)
	copy(b, "TIER-PLAINTEXT-MARKER")
	sha := put(t, e1, b)
	if err := tr.MoveCold(sha); err != nil { // no key involved
		t.Fatal(err)
	}
	stored, _ := cold(r, sha)
	if bytes.Contains(stored, []byte("TIER-PLAINTEXT-MARKER")) || len(stored) <= len(b) {
		t.Fatal("the cold tier holds plaintext")
	}
	if got, err := readAll(t, e1, sha); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("%v", err)
	}
	// rotation rewrites the header of the cold object in the cold tier, and nothing appears in the hot one
	e2, _ := blob.NewEncrypted(tr, 2, map[byte][]byte{1: k1, 2: k2})
	if rep := e2.RewrapAll([]string{sha}); rep.Changed != 1 || len(rep.Failed) != 0 {
		t.Fatalf("%+v", rep)
	}
	if _, err := hot.Open(sha); !errors.Is(err, blob.ErrNotFound) || tr.Where(sha) != "cold" {
		t.Fatalf("the rewrap moved the object: %q", tr.Where(sha))
	}
	after, _ := cold(r, sha)
	if after[4] != 2 || !bytes.Equal(stored[65:], after[65:]) {
		t.Fatal("rotation changed more than the header")
	}
	only2, _ := blob.NewEncrypted(tr, 2, map[byte][]byte{2: k2})
	if got, err := readAll(t, only2, sha); err != nil || !bytes.Equal(got, b) {
		t.Fatalf("%v", err)
	}
	if blob.AsTiered(e2) != tr || blob.AsTiered(blob.NewFallback(e2, hot)) != tr || blob.AsTiered(hot) != nil {
		t.Fatal("AsTiered does not see through the wrappers")
	}
}

func TestBLOB15_ReadersNeverMissAnObjectWhileItMoves(t *testing.T) {
	tr, _, _, _ := newTier(t)
	var shas []string
	want := map[string][]byte{}
	for i := 0; i < 12; i++ {
		b := make([]byte, 30000+i)
		rand.Read(b)
		sha := put(t, tr, b)
		shas = append(shas, sha)
		want[sha] = b
	}
	var misses, wrong atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				sha := shas[i%len(shas)]
				o, err := tr.Open(sha)
				if err != nil {
					misses.Add(1)
					continue
				}
				got, err := io.ReadAll(o)
				o.Close()
				if err != nil || !bytes.Equal(got, want[sha]) {
					wrong.Add(1)
				}
			}
		}()
	}
	for _, sha := range shas {
		if err := tr.MoveCold(sha); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if misses.Load() != 0 || wrong.Load() != 0 {
		t.Fatalf("%d reads missed an object and %d read the wrong bytes while objects moved", misses.Load(), wrong.Load())
	}
}

// The latency the documentation quotes: the same object read from the hot tier (a local file) and from the cold one (an S3
// service; here an in-process mock on loopback, so the figure is the cost of the client's protocol work and not of a network).
func TestBLOB15_ColdReadLatencyIsMeasured(t *testing.T) {
	tr, _, _, _ := newTier(t)
	b := make([]byte, 1<<20)
	rand.Read(b)
	sha := put(t, tr, b)
	read := func() time.Duration {
		start := time.Now()
		o, err := tr.Open(sha)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, o)
		o.Close()
		return time.Since(start)
	}
	median := func(n int) time.Duration {
		var ds []time.Duration
		for i := 0; i < n; i++ {
			ds = append(ds, read())
		}
		for i := range ds {
			for j := i + 1; j < len(ds); j++ {
				if ds[j] < ds[i] {
					ds[i], ds[j] = ds[j], ds[i]
				}
			}
		}
		return ds[len(ds)/2]
	}
	hotMed := median(21)
	tr.MoveCold(sha)
	coldMed := median(21)
	t.Logf("1 MiB read: hot (local file) %s, cold (S3 client against an in-process mock) %s", hotMed, coldMed)
	if coldMed > 2*time.Second {
		t.Fatalf("a cold read of 1 MiB took %s", coldMed)
	}
	_ = s3mock.Bucket
}
