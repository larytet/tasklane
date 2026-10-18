package mail

import (
	"bytes"
	"errors"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// Limits keep the parser's work and memory bounded whatever a sender puts in the message (MAIL-13, MAIL-15).
const (
	MaxHeaderBytes = 256 << 10
	maxDepth       = 6
	maxParts       = 100
	maxTextBytes   = 1 << 20
)

// Parsed is what the tracker needs from an inbound message.
type Parsed struct {
	Header      mail.Header
	Text        string // the readable body: the text part, or the HTML part reduced to text
	Attachments int    // parts that are files; they are not kept by the reply path
	Truncated   bool
}

var ErrMalformed = errors.New("the message is malformed")

// Parse reads headers and the readable body. It never fails on a bad charset or encoding of the body (the text is
// kept as far as it decodes) and refuses a message whose header section is huge or that has no header at all.
func Parse(raw []byte) (*Parsed, error) {
	end := bytes.Index(raw, []byte("\r\n\r\n"))
	if e2 := bytes.Index(raw, []byte("\n\n")); end < 0 || (e2 >= 0 && e2 < end) {
		end = e2
	}
	if end < 0 {
		end = len(raw)
	}
	if end > MaxHeaderBytes {
		return nil, errors.New("the message headers are too large")
	}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrMalformed
	}
	p := &Parsed{Header: m.Header}
	parts := 0
	var plain, htm string
	var walk func(h map[string][]string, body io.Reader, depth int)
	walk = func(h map[string][]string, body io.Reader, depth int) {
		if parts++; parts > maxParts || depth > maxDepth {
			p.Truncated = true
			return
		}
		get := func(k string) string {
			for kk, v := range h {
				if strings.EqualFold(kk, k) && len(v) > 0 {
					return v[0]
				}
			}
			return ""
		}
		mt, params, err := mime.ParseMediaType(get("Content-Type"))
		if err != nil || mt == "" {
			mt, params = "text/plain", map[string]string{}
		}
		disp, _, _ := mime.ParseMediaType(get("Content-Disposition"))
		if strings.HasPrefix(mt, "multipart/") {
			if params["boundary"] == "" {
				return
			}
			mr := multipart.NewReader(io.LimitReader(body, 32<<20), params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					return
				}
				walk(part.Header, part, depth+1)
			}
		}
		if disp == "attachment" || (mt != "text/plain" && mt != "text/html") {
			p.Attachments++
			io.Copy(io.Discard, io.LimitReader(body, 32<<20))
			return
		}
		txt, trunc := decodeText(body, get("Content-Transfer-Encoding"), params["charset"])
		p.Truncated = p.Truncated || trunc
		if mt == "text/html" {
			if htm == "" {
				htm = txt
			}
		} else if plain == "" {
			plain = txt
		}
	}
	hdr := map[string][]string(m.Header)
	walk(hdr, m.Body, 0)
	if strings.TrimSpace(plain) != "" {
		p.Text = plain
	} else {
		p.Text = HTMLToText(htm)
	}
	p.Text = strings.ReplaceAll(strings.ReplaceAll(p.Text, "\r\n", "\n"), "\r", "\n")
	return p, nil
}

func decodeText(body io.Reader, cte, charset string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	case "base64":
		body = newLenientBase64(body)
	}
	b, _ := io.ReadAll(io.LimitReader(body, maxTextBytes+1))
	trunc := len(b) > maxTextBytes
	if trunc {
		b = b[:maxTextBytes]
	}
	if charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "us-ascii") {
		if enc, err := htmlindex.Get(charset); err == nil {
			if d, err := enc.NewDecoder().Bytes(b); err == nil {
				b = d
			}
		}
	}
	if !utf8.Valid(b) {
		b = []byte(strings.ToValidUTF8(string(b), "�"))
	}
	return strings.ToValidUTF8(string(b), "�"), trunc
}

var (
	reScript = regexp.MustCompile(`(?is)<(script|style|head)[^>]*>.*?</(script|style|head)>`)
	reBreak  = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/tr|/h[1-6])\s*/?>`)
	reTag    = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText reduces HTML to its text: no script, no style, no remote content, no markup survives.
func HTMLToText(h string) string {
	h = reScript.ReplaceAllString(h, "")
	h = reBreak.ReplaceAllString(h, "\n")
	h = reTag.ReplaceAllString(h, "")
	h = html.UnescapeString(h)
	lines := strings.Split(h, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t ")
	}
	return strings.TrimSpace(reBlank.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
