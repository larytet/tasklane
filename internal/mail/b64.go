package mail

import (
	"bytes"
	"encoding/base64"
	"io"
)

// newLenientBase64 decodes base64 that may contain line breaks or spaces or lack padding. It stops at the first byte that
// is not base64 and keeps whatever decoded before it.
func newLenientBase64(r io.Reader) io.Reader {
	b, _ := io.ReadAll(io.LimitReader(r, 48<<20))
	clean := make([]byte, 0, len(b))
scan:
	for _, c := range b {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			clean = append(clean, c)
		case c == '=' || c == '\r' || c == '\n' || c == ' ' || c == '\t':
		default:
			break scan
		}
	}
	if len(clean)%4 == 1 { // a lone leftover character carries no byte
		clean = clean[:len(clean)-1]
	}
	d, _ := base64.RawStdEncoding.DecodeString(string(clean))
	return bytes.NewReader(d)
}
