package web_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/web"
)

func get(h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestShellRoutingAndCaching(t *testing.T) {
	h := web.Handler()
	for _, p := range []string{"/", "/issue/ABC-1", "/anything/deep"} { // client-side routes return the shell
		w := get(h, p)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `id="app"`) || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	if w := get(h, "/missing.js"); w.Code != 404 {
		t.Fatalf("missing asset: %d", w.Code)
	}
	w := get(h, "/app.js")
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("%v", w.Header())
	}
	if w := get(h, "/app.js", "If-None-Match", etag); w.Code != 304 || w.Body.Len() != 0 { // repeat visits cost one tiny 304
		t.Fatalf("revalidation: %d", w.Code)
	}
	w = get(h, "/app.js", "Accept-Encoding", "gzip")
	zr, err := gzip.NewReader(w.Body)
	if err != nil || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip: %v", err)
	}
	raw, _ := io.ReadAll(zr)
	if !strings.Contains(string(raw), "use strict") {
		t.Fatal("gzip body is not the script")
	}
	if w := httptest.NewRecorder(); true {
		h.ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
		if w.Code != 405 {
			t.Fatalf("POST: %d", w.Code)
		}
	}
}

// SPEC 21: first-ever visit loads under 200 KB of gzipped JavaScript.
func TestBundleBudget(t *testing.T) {
	total := 0
	for name, n := range web.Sizes() {
		if strings.HasSuffix(name, ".js") {
			total += n
		}
	}
	t.Logf("gzipped JavaScript: %d bytes", total)
	if total > 200*1024 {
		t.Fatalf("JavaScript is %d bytes gzipped, budget 204800", total)
	}
}

// The app never builds markup from data: no innerHTML, outerHTML, insertAdjacentHTML, document.write or eval.
func TestNoUnsafeDOMSinks(t *testing.T) {
	out, err := exec.Command("grep", "-nE", `innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|new Function`, "static/app.js", "static/lib.js").CombinedOutput()
	if err == nil {
		t.Fatalf("unsafe DOM sink found:\n%s", out)
	}
}

func TestJavaScriptUnitTests(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command("node", "--test", "jstest").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
