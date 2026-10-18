package api_test

import (
	"fmt"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/blob"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// faultStore is a blob store whose disk can fail: "full" gives every new upload a staging file that answers ENOSPC (/dev/full), and
// "stuck" one whose writes block until the test lets them go (a disk that has stopped answering).
type faultStore struct {
	blob.Store
	mu     sync.Mutex
	mode   string
	stuckR []*os.File
}

func (f *faultStore) set(m string) { f.mu.Lock(); f.mode = m; f.mu.Unlock() }

func (f *faultStore) NewStaging() (string, *os.File, error) {
	f.mu.Lock()
	mode := f.mode
	f.mu.Unlock()
	switch mode {
	case "full":
		id, real, err := f.Store.NewStaging()
		if err != nil {
			return "", nil, err
		}
		real.Close()
		d, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
		return id, d, err
	case "stuck":
		id, real, err := f.Store.NewStaging()
		if err != nil {
			return "", nil, err
		}
		real.Close()
		r, w, err := os.Pipe() // nobody reads: a write of more than the pipe's buffer blocks
		f.mu.Lock()
		f.stuckR = append(f.stuckR, r)
		f.mu.Unlock()
		return id, w, err
	}
	return f.Store.NewStaging()
}

func (f *faultStore) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.stuckR {
		r.Close() // the blocked writers fail with a broken pipe, as a disk that comes back with an error would
	}
	f.stuckR = nil
}

func diskSetup(t *testing.T) (*testutil.Env, *faultStore, *client, string) {
	e := testutil.New(t)
	e.Project()
	fs := e.WithBlobs()
	fault := &faultStore{Store: fs}
	if err := e.Svc.ConfigureBlobs(testutil.Ctx(), &domain.Blobs{Store: fault, MaxUploads: 1}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: time.Hour}).Handler())
	t.Cleanup(ts.Close)
	c := &client{t: t, base: ts.URL}
	c.token, _ = e.Svc.CreateAPIToken(testutil.Ctx(), e.Alice.ID, "t")
	_, out, _ := c.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "disk"})
	return e, fault, c, out["key"].(string)
}

func attachmentRows(e *testutil.Env) (att, blobs int) {
	e.Store.Pool.QueryRow(testutil.Ctx(), `SELECT (SELECT count(*) FROM attachment),(SELECT count(*) FROM blob)`).Scan(&att, &blobs)
	return
}

func stagingFiles(t *testing.T, fs blob.Store) int {
	n := 0
	if f, ok := fs.(*faultStore); ok {
		fs = f.Store
	}
	if r, ok := fs.(*blob.FS); ok {
		ents, _ := os.ReadDir(r.Root + "/staging")
		n = len(ents)
	}
	return n
}

// CHAOS-05: a full disk. The upload is refused with a plain message and a Retry-After, nothing half-written is left (no attachment,
// no blob row, no staging file), everything that does not need the disk keeps working, and the same upload succeeds once there is room.
func TestCHAOS05_AFullDiskRefusesUploadsPlainlyAndLeavesNothingBehind(t *testing.T) {
	e, fault, c, key := diskSetup(t)
	payload := make([]byte, 200_000)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if code, _ := upload(t, c, key, "ok.bin", "application/octet-stream", payload); code != 201 {
		t.Fatalf("an upload on a healthy disk: %d", code)
	}
	a0, b0 := attachmentRows(e)
	fault.set("full")
	code, body := upload(t, c, key, "full.bin", "application/octet-stream", payload)
	if code != 507 || body["code"] != "storage_full" {
		t.Fatalf("a full disk: %d %v", code, body)
	}
	if msg, _ := body["error"].(string); msg == "" || len(msg) < 20 {
		t.Fatalf("the message is not plain: %q", msg)
	}
	if a, b := attachmentRows(e); a != a0 || b != b0 {
		t.Fatalf("a failed upload left rows behind: attachments %d->%d, blobs %d->%d", a0, a, b0, b)
	}
	if n := stagingFiles(t, fault); n != 0 {
		t.Fatalf("%d staging file(s) left behind", n)
	}
	// what does not need the blob disk goes on: reading and writing issues, comments, text saves
	if code, _, _ := c.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "while the disk is full"}); code != 201 {
		t.Fatalf("creating an issue while the blob disk is full: %d", code)
	}
	if code, _, _ := c.do("POST", "/api/v1/issues/"+key+"/comments", map[string]any{"body": "still talking"}); code != 201 && code != 200 {
		t.Fatalf("commenting while the blob disk is full: %d", code)
	}
	// and the earlier attachment is intact and downloadable
	if code, out, _ := c.do("GET", "/api/v1/issues/"+key+"/view", nil); code != 200 || len(out["attachments"].([]any)) != 1 {
		t.Fatalf("the earlier attachment: %d", code)
	}
	// room comes back: the same upload now works, with nothing stale in the way
	fault.set("")
	if code, body := upload(t, c, key, "full.bin", "application/octet-stream", payload); code != 201 {
		t.Fatalf("after the disk recovered: %d %v", code, body)
	}
}

// CHAOS-05: a disk that has stopped answering. Uploads that wait on it hold their slots, so the next one is turned away at once with
// a retry hint instead of queueing without limit; interactive work is not slowed; when the disk fails, the stuck upload fails
// cleanly and its slot is free again.
func TestCHAOS05_ASlowDiskHoldsUploadsBackWithoutSlowingEverythingElse(t *testing.T) {
	e, fault, c, key := diskSetup(t)
	fault.set("stuck")
	payload := make([]byte, 400_000)
	done := make(chan int, 1)
	go func() { code, _ := upload(t, c, key, "stuck.bin", "application/octet-stream", payload); done <- code }()
	time.Sleep(500 * time.Millisecond) // it is now blocked on the disk, holding the only upload slot
	start := time.Now()
	code, body := upload(t, c, key, "second.bin", "application/octet-stream", payload)
	if code != 503 || body["code"] != "busy" {
		t.Fatalf("a second upload while the disk is stuck: %d %v", code, body)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the refusal took %s: it should not queue behind a stuck disk", d)
	}
	var lat []time.Duration
	for i := 0; i < 20; i++ {
		s := time.Now()
		if code, _, _ := c.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": fmt.Sprint("busy ", i)}); code != 201 {
			t.Fatalf("%d", code)
		}
		lat = append(lat, time.Since(s))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	if p := lat[len(lat)*95/100]; p > 300*time.Millisecond {
		t.Fatalf("interactive requests slowed to p95 %s by a stuck disk", p)
	}
	select {
	case code := <-done:
		t.Fatalf("the stuck upload finished by itself with %d", code)
	default:
	}
	fault.release() // the disk answers with an error
	select {
	case code := <-done:
		if code < 400 {
			t.Fatalf("the stuck upload reported success: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck upload never returned after the disk failed")
	}
	if a, b := attachmentRows(e); a != 0 || b != 0 {
		t.Fatalf("rows left behind: attachments %d, blobs %d", a, b)
	}
	// the slot is free again
	fault.set("")
	if code, _ := upload(t, c, key, "after.bin", "application/octet-stream", payload); code != 201 {
		t.Fatalf("after the disk recovered: %d", code)
	}
}
