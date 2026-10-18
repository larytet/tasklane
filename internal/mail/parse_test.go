package mail_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/larytet/tasklane/internal/mail"
)

func inmsg(ct, cte, body string, extra ...string) []byte {
	h := []string{"From: a@example.com", "To: b@example.com", "Subject: s", "Message-ID: <1@x>", "MIME-Version: 1.0", "Content-Type: " + ct}
	if cte != "" {
		h = append(h, "Content-Transfer-Encoding: "+cte)
	}
	h = append(h, extra...)
	return []byte(strings.Join(h, "\r\n") + "\r\n\r\n" + body)
}

func TestMAIL15_CharsetsAndTransferEncodings(t *testing.T) {
	sjis, _ := japanese.ShiftJIS.NewEncoder().String("こんにちは世界")
	gbk, _ := simplifiedchinese.GBK.NewEncoder().String("你好，世界")
	latin, _ := charmap.ISO8859_1.NewEncoder().String("Grüße aus Köln")
	latin15, _ := charmap.ISO8859_15.NewEncoder().String("20 € für Zoë")
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"utf-8", inmsg("text/plain; charset=utf-8", "8bit", "héllo wörld ✓"), "héllo wörld ✓"},
		{"iso-8859-1", inmsg("text/plain; charset=iso-8859-1", "8bit", latin), "Grüße aus Köln"},
		{"iso-8859-15", inmsg("text/plain; charset=ISO-8859-15", "8bit", latin15), "20 € für Zoë"},
		{"shift-jis", inmsg("text/plain; charset=shift_jis", "8bit", sjis), "こんにちは世界"},
		{"gbk", inmsg("text/plain; charset=gbk", "8bit", gbk), "你好，世界"},
		{"quoted-printable", inmsg("text/plain; charset=utf-8", "quoted-printable", "caf=C3=A9 =E2=9C=93 soft=\r\nbreak"), "café ✓ softbreak"},
		{"quoted-printable latin1", inmsg("text/plain; charset=iso-8859-1", "quoted-printable", "Gr=FC=DFe"), "Grüße"},
		{"base64", inmsg("text/plain; charset=utf-8", "base64", base64.StdEncoding.EncodeToString([]byte("encoded body ✓"))), "encoded body ✓"},
		{"base64 with line breaks", inmsg("text/plain; charset=utf-8", "base64", "ZW5jb2Rl\r\nZCBib2R5\r\n"), "encoded body"},
		{"base64 without padding", inmsg("text/plain; charset=utf-8", "base64", "aGVsbG8"), "hello"},
		{"unknown charset keeps the text", inmsg("text/plain; charset=x-nonsense", "8bit", "plain ascii"), "plain ascii"},
		{"invalid utf-8 is repaired", inmsg("text/plain; charset=utf-8", "8bit", "ok \xff\xfe still ok"), "ok \uFFFD still ok"},
		{"no content type at all", []byte("From: a@example.com\r\nMessage-ID: <1@x>\r\n\r\njust text"), "just text"},
		{"bare LF line endings", []byte("From: a@example.com\nMessage-ID: <1@x>\nContent-Type: text/plain\n\nline one\nline two"), "line one\nline two"},
	}
	for _, c := range cases {
		p, err := mail.Parse(c.raw)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if strings.TrimSpace(p.Text) != c.want {
			t.Errorf("%s: %q, want %q", c.name, p.Text, c.want)
		}
	}
}

func TestMAIL15_HeadersAreDecodedByTheHeaderReader(t *testing.T) {
	p, err := mail.Parse(inmsg("text/plain", "", "x", "X-Long: "+strings.Repeat("a", 5000)))
	if err != nil || len(p.Header.Get("X-Long")) != 5000 {
		t.Fatalf("%v", err)
	}
	// a header section over the limit is refused, not read
	if _, err := mail.Parse(inmsg("text/plain", "", "x", "X-Pad: "+strings.Repeat("b", mail.MaxHeaderBytes+10))); err == nil {
		t.Fatal("a huge header section was accepted")
	}
	for _, bad := range [][]byte{nil, []byte(""), []byte("\x00\x01\x02"), []byte("no colon here\r\n\r\nbody"), []byte("Subject: only a header")} {
		if p, err := mail.Parse(bad); err == nil && p == nil {
			t.Errorf("%q", bad)
		}
	}
}

func TestMAIL13_ContentIsReducedToSafeText(t *testing.T) {
	html := `<html><head><style>p{color:red}</style><script>alert(1)</script></head><body onload="x()">
<p>Hello <b>there</b> &amp; welcome</p><img src="http://tracker.example/pixel.gif"><a href="javascript:evil()">click</a>
<div>second<br>line</div><script>document.cookie</script></body></html>`
	p, err := mail.Parse(inmsg("text/html; charset=utf-8", "8bit", html))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"alert", "<", "tracker", "javascript", "onload", "document.cookie", "color:red"} {
		if strings.Contains(p.Text, bad) {
			t.Errorf("%q survived: %q", bad, p.Text)
		}
	}
	if !strings.Contains(p.Text, "Hello there & welcome") || !strings.Contains(p.Text, "second\nline") {
		t.Errorf("text lost: %q", p.Text)
	}
	// the plain part wins over the HTML part of the same message
	alt := "--b\r\nContent-Type: text/plain\r\n\r\nplain wins\r\n--b\r\nContent-Type: text/html\r\n\r\n<p>html loses</p>\r\n--b--\r\n"
	if p, _ := mail.Parse(inmsg(`multipart/alternative; boundary=b`, "", alt)); strings.TrimSpace(p.Text) != "plain wins" {
		t.Errorf("%q", p.Text)
	}
}

func TestMAIL13_14_HostileStructureStaysBounded(t *testing.T) {
	// nesting far beyond the limit: parsed without recursion blowing up, and the text above the limit is not read
	body := "deep text"
	ct := "text/plain"
	for i := 0; i < 40; i++ {
		b := fmt.Sprintf("b%d", i)
		body = "--" + b + "\r\nContent-Type: " + ct + "\r\n\r\n" + body + "\r\n--" + b + "--\r\n"
		ct = "multipart/mixed; boundary=" + b
	}
	start := time.Now()
	p, err := mail.Parse(inmsg(ct, "", body))
	if err != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("%v %v", err, time.Since(start))
	}
	if !p.Truncated || strings.Contains(p.Text, "deep text") {
		t.Errorf("a 40-level nest was followed: truncated=%v %q", p.Truncated, p.Text)
	}
	// thousands of tiny parts
	var parts strings.Builder
	for i := 0; i < 5000; i++ {
		parts.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	parts.WriteString("--b--\r\n")
	if p, err := mail.Parse(inmsg("multipart/mixed; boundary=b", "", parts.String())); err != nil || !p.Truncated {
		t.Errorf("part count not bounded: %v", err)
	}
	// a body far larger than the text limit is cut, not held
	p, _ = mail.Parse(inmsg("text/plain", "", strings.Repeat("z", 3<<20)))
	if len(p.Text) > 1<<20+1 || !p.Truncated {
		t.Errorf("text %d truncated=%v", len(p.Text), p.Truncated)
	}
	// attachments are counted and their bytes never become text; a type that lies about itself is still an attachment
	att := "--b\r\nContent-Type: text/plain\r\n\r\nreal text\r\n--b\r\nContent-Type: image/png; name=x.png\r\nContent-Disposition: attachment\r\n\r\nPNGDATA\r\n--b\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nSECRET FILE TEXT\r\n--b--\r\n"
	p, _ = mail.Parse(inmsg("multipart/mixed; boundary=b", "", att))
	if p.Attachments != 2 || strings.Contains(p.Text, "PNGDATA") || strings.Contains(p.Text, "SECRET") || strings.TrimSpace(p.Text) != "real text" {
		t.Errorf("%d %q", p.Attachments, p.Text)
	}
	// a multipart without a boundary or with a broken one yields no text and no crash
	for _, ct := range []string{"multipart/mixed", "multipart/mixed; boundary=\"\"", "multipart/alternative; boundary=zz"} {
		if _, err := mail.Parse(inmsg(ct, "", "--b\r\nContent-Type: text/plain\r\n\r\nx\r\n--b--")); err != nil {
			t.Errorf("%s: %v", ct, err)
		}
	}
}
