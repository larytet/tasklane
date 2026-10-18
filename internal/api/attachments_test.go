package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/testutil"
)

func blobSetup(t *testing.T) (*testutil.Env, *client, *client, string) {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: 3600e9}).Handler())
	t.Cleanup(ts.Close)
	mk := func(email string) *client {
		c := &client{t: t, base: ts.URL}
		us, _ := e.Svc.ListUsers(testutil.Ctx())
		for _, u := range us {
			if u.Email == email {
				c.token, _ = e.Svc.CreateAPIToken(testutil.Ctx(), u.ID, "t")
			}
		}
		return c
	}
	alice, bob := mk("alice@example.com"), mk("bob@example.com")
	_, out, _ := alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "att"})
	return e, alice, bob, out["key"].(string)
}

func upload(t *testing.T, c *client, key, name, ctype string, data []byte) (int, map[string]any) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := map[string][]string{"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename="%s"`, name)}, "Content-Type": {ctype}}
	pw, _ := mw.CreatePart(hdr)
	pw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+"/api/v1/issues/"+key+"/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func get(t *testing.T, url, token string, hdr ...string) (*http.Response, []byte) {
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestHTTP_BLOB04_RangeETagAndHeaders(t *testing.T) {
	_, alice, _, key := blobSetup(t)
	data := []byte(strings.Repeat("line of a log file\n", 1000))
	code, att := upload(t, alice, key, "build.log", "text/plain", data)
	if code != 201 {
		t.Fatalf("%d %v", code, att)
	}
	id := att["id"].(string)
	url := alice.base + "/api/v1/attachments/" + id + "/download"
	resp, body := get(t, url, alice.token)
	if resp.StatusCode != 200 || !bytes.Equal(body, data) || resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("no range support")
	}
	// log tail: the last 19 bytes, and a middle range
	resp, body = get(t, url, alice.token, "Range", "bytes=-19")
	if resp.StatusCode != 206 || string(body) != "line of a log file\n" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	resp, body = get(t, url, alice.token, "Range", "bytes=19-37")
	if resp.StatusCode != 206 || len(body) != 19 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 19-37/%d", len(data)) {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get(t, url, alice.token, "Range", "bytes=999999999-"); resp.StatusCode != 416 {
		t.Fatalf("unsatisfiable range: %d", resp.StatusCode)
	}
	// conditional GET
	etag := resp.Header.Get("ETag")
	resp, _ = get(t, url, alice.token)
	etag = resp.Header.Get("ETag")
	if resp, _ := get(t, url, alice.token, "If-None-Match", etag); resp.StatusCode != 304 {
		t.Fatalf("revalidation: %d", resp.StatusCode)
	}
	// active content is forced to download and never typed as html
	code, h := upload(t, alice, key, "evil.html", "text/html", []byte("<script>alert(1)</script>"))
	if code != 201 {
		t.Fatal(code)
	}
	resp, _ = get(t, alice.base+"/api/v1/attachments/"+h["id"].(string)+"/download", alice.token)
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("unsafe serving: %v", resp.Header)
	}
	// a cookie-less request is refused
	if resp, _ := get(t, url, ""); resp.StatusCode != 401 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestHTTP_BLOB05_SignedLinksAndBlockedFiles(t *testing.T) {
	e, alice, bob, key := blobSetup(t)
	_, att := upload(t, alice, key, "n.txt", "text/plain", []byte("secret notes"))
	id := att["id"].(string)
	code, out, _ := bob.do("POST", "/api/v1/attachments/"+id+"/link", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	link := out["url"].(string)
	// the signed link needs no credentials (it can live on a separate origin)
	resp, body := get(t, alice.base+link, "")
	if resp.StatusCode != 200 || string(body) != "secret notes" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	resp, _ = get(t, alice.base+strings.Replace(link, "sig=", "sig=0", 1), "")
	if resp.StatusCode != 403 {
		t.Fatalf("tampered link: %d", resp.StatusCode)
	}
	e.Clock.Advance(10 * 60e9)
	if resp, _ := get(t, alice.base+link, ""); resp.StatusCode != 403 {
		t.Fatalf("expired link: %d", resp.StatusCode)
	}
	// EICAR: blocked, 423, no link
	eicar := `X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`
	_, bad := upload(t, alice, key, "e.com", "application/octet-stream", []byte(eicar))
	bid := bad["id"].(string)
	if bad["scan_state"] != "blocked" {
		t.Fatalf("%v", bad)
	}
	if resp, _ := get(t, alice.base+"/api/v1/attachments/"+bid+"/download", alice.token); resp.StatusCode != 423 {
		t.Fatalf("blocked download: %d", resp.StatusCode)
	}
	if code, _, _ := alice.do("POST", "/api/v1/attachments/"+bid+"/link", nil); code != 423 {
		t.Fatalf("blocked link: %d", code)
	}
}

func TestHTTP_BLOB02_ResumableOverHTTP(t *testing.T) {
	_, alice, bob, key := blobSetup(t)
	data := bytes.Repeat([]byte("abcdefghijklmnop"), 1<<17) // 2 MiB
	sum := sha256.Sum256(data)
	code, us, _ := alice.do("POST", "/api/v1/issues/"+key+"/uploads", map[string]any{"name": "video.bin", "size": len(data), "type": "application/octet-stream"})
	if code != 201 {
		t.Fatalf("%d %v", code, us)
	}
	id := us["id"].(string)
	patch := func(c *client, off int, b []byte) (int, http.Header) {
		req, _ := http.NewRequest("PATCH", c.base+"/api/v1/uploads/"+id, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Upload-Offset", fmt.Sprint(off))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	if c, _ := patch(alice, 0, data[:700000]); c != 200 {
		t.Fatal(c)
	}
	// "connection dropped": the client asks where to resume
	req, _ := http.NewRequest("HEAD", alice.base+"/api/v1/uploads/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+alice.token)
	resp, _ := http.DefaultClient.Do(req)
	if resp.Header.Get("Upload-Offset") != "700000" {
		t.Fatalf("offset %q", resp.Header.Get("Upload-Offset"))
	}
	if c, h := patch(alice, 0, data[:10]); c != 409 || h.Get("Upload-Offset") != "700000" {
		t.Fatalf("stale chunk: %d %v", c, h)
	}
	if c, _ := patch(bob, 700000, data[700000:]); c != 404 {
		t.Fatalf("another user's chunk: %d", c)
	}
	if c, _ := patch(alice, 700000, data[700000:]); c != 200 {
		t.Fatal(c)
	}
	code, att, _ := alice.do("POST", "/api/v1/uploads/"+id+"/commit", nil)
	if code != 201 || att["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("%d %v", code, att)
	}
}

// BLOB-02 at full size: 2 GiB, dropped and resumed, with the heap staying flat. `make perf`.
func TestHTTP_BLOB02_TwoGiBConstantMemory(t *testing.T) {
	if os.Getenv("TASKLANE_PERF") == "" {
		t.Skip("set TASKLANE_PERF=1 (make perf) for the 2 GiB upload")
	}
	_, alice, _, key := blobSetup(t)
	const total = int64(2) << 30
	gen := func(from, to int64) io.Reader { // deterministic bytes without holding them in memory
		return io.LimitReader(&patternReader{pos: from}, to-from)
	}
	h := sha256.New()
	io.Copy(h, gen(0, total))
	want := hex.EncodeToString(h.Sum(nil))
	_, us, _ := alice.do("POST", "/api/v1/issues/"+key+"/uploads", map[string]any{"name": "rec.mp4", "size": total})
	id := us["id"].(string)
	send := func(from, to int64, dropAfter int64) {
		var r io.Reader = gen(from, to)
		if dropAfter > 0 {
			r = io.LimitReader(r, dropAfter) // the connection dies after dropAfter bytes
		}
		req, _ := http.NewRequest("PATCH", alice.base+"/api/v1/uploads/"+id, r)
		req.ContentLength = -1
		req.Header.Set("Authorization", "Bearer "+alice.token)
		req.Header.Set("Upload-Offset", fmt.Sprint(from))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}
	var peak uint64
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > peak {
			peak = m.HeapAlloc
		}
	}
	chunk := int64(256 << 20)
	var off int64
	send(0, chunk, chunk/2) // dropped halfway through the first chunk
	for off < total {
		req, _ := http.NewRequest("HEAD", alice.base+"/api/v1/uploads/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+alice.token)
		resp, _ := http.DefaultClient.Do(req)
		fmt.Sscan(resp.Header.Get("Upload-Offset"), &off)
		if off >= total {
			break
		}
		end := off + chunk
		if end > total {
			end = total
		}
		send(off, end, 0)
		sample()
	}
	code, att, _ := alice.do("POST", "/api/v1/uploads/"+id+"/commit", nil)
	if code != 201 || att["sha256"] != want {
		t.Fatalf("%d %v want %s", code, att, want)
	}
	sample()
	t.Logf("2 GiB committed; peak heap %d MiB", peak>>20)
	if peak > 128<<20 {
		t.Fatalf("heap grew to %d MiB during a 2 GiB upload", peak>>20)
	}
}

type patternReader struct{ pos int64 }

func (p *patternReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte((p.pos + int64(i)) * 2654435761 >> 13)
	}
	mp4 := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'm', 'p', '4', '2', 'i', 's', 'o', 'm'}
	for i := range b { // an MP4 header, so the sniffer classifies the stream as video (2 GiB class limit)
		if q := p.pos + int64(i); q < int64(len(mp4)) {
			b[i] = mp4[q]
		}
	}
	p.pos += int64(len(b))
	return len(b), nil
}
