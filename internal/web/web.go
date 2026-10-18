// Package web embeds the single-page app and serves it with cache validators and gzip.
package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed static
var assets embed.FS

type file struct {
	raw, gz []byte
	etag    string
	ctype   string
}

var files = map[string]*file{}

func init() {
	sub, _ := fs.Sub(assets, "static")
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := fs.ReadFile(sub, p)
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		zw.Write(raw)
		zw.Close()
		sum := sha256.Sum256(raw)
		ct := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml"}[path.Ext(p)]
		if ct == "" {
			ct = "application/octet-stream"
		}
		files[p] = &file{raw: raw, gz: b.Bytes(), etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: ct}
		return nil
	})
}

// Sizes reports the gzipped size of every asset, for the bundle budget test.
func Sizes() map[string]int {
	out := map[string]int{}
	for k, f := range files {
		out[k] = len(f.gz)
	}
	return out
}

// Handler serves the app shell for every non-API path (client-side routing) and assets by name.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", 405)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		f := files[name]
		if f == nil {
			if strings.Contains(name, ".") { // a missing asset is a 404, not the app shell
				http.NotFound(w, r)
				return
			}
			f = files["index.html"]
			if name == "portal" || strings.HasPrefix(name, "portal/") { // the customer portal is its own small page: no staff app code
				f = files["portal.html"]
			}
		}
		h := w.Header()
		h.Set("Content-Type", f.ctype)
		h.Set("ETag", f.etag)
		h.Set("Cache-Control", "no-cache") // always revalidate: one tiny 304 on repeat visits
		h.Set("Vary", "Accept-Encoding")
		if r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := f.raw
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			body = f.gz
		}
		w.Write(body)
	})
}
