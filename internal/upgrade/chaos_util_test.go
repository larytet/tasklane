//go:build chaos

package upgrade

import (
	"bytes"
	"io"
	"net/http"
)

func readAll(r *http.Request) ([]byte, error) { return io.ReadAll(r.Body) }
func newBody(b []byte) io.ReadCloser          { return io.NopCloser(bytes.NewReader(b)) }
