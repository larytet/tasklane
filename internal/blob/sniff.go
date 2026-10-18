package blob

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

// Class is the blob class that decides size limits and preview rules.
type Class string

const (
	ClassImage    Class = "image"
	ClassVideo    Class = "video"
	ClassLog      Class = "log"
	ClassDocument Class = "document"
	ClassArchive  Class = "archive"
)

// Limits are the default per-class maximum sizes (SPEC 17.6).
var Limits = map[Class]int64{
	ClassImage: 25 << 20, ClassVideo: 2 << 30, ClassLog: 1 << 30, ClassDocument: 100 << 20, ClassArchive: 100 << 20,
}

// Sniff detects the real type from the first bytes and classifies it.
func Sniff(head []byte, name string) (mime string, class Class) {
	mime = http.DetectContentType(head)
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = mime[:i]
	}
	lname := strings.ToLower(name)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return mime, ClassImage
	case strings.HasPrefix(mime, "video/"), mime == "application/ogg":
		return mime, ClassVideo
	case mime == "application/zip", mime == "application/x-gzip", mime == "application/gzip":
		return mime, ClassArchive
	case mime == "text/plain":
		if strings.HasSuffix(lname, ".log") || strings.HasSuffix(lname, ".out") || strings.HasSuffix(lname, ".trace") {
			return "text/plain", ClassLog
		}
		return mime, ClassDocument
	}
	return mime, ClassDocument
}

// TypeFlag reports when the declared MIME type's family disagrees with the sniffed one (type spoofing).
func TypeFlag(declared, sniffed string) string {
	declared = strings.ToLower(strings.TrimSpace(declared))
	if i := strings.Index(declared, ";"); i >= 0 {
		declared = declared[:i]
	}
	if declared == "" || declared == "application/octet-stream" || declared == sniffed {
		return ""
	}
	fam := func(m string) string {
		if i := strings.Index(m, "/"); i >= 0 {
			return m[:i]
		}
		return m
	}
	if fam(declared) == fam(sniffed) && fam(declared) != "application" {
		return ""
	}
	if sniffed == "application/octet-stream" || sniffed == "text/plain" && fam(declared) == "text" {
		return ""
	}
	return fmt.Sprintf("declared %s but content looks like %s", declared, sniffed)
}

// Bomb limits for archives.
const (
	MaxUncompressed = int64(1) << 30
	MaxRatio        = 200
	MaxEntries      = 20000
)

// CheckZip refuses archives that expand beyond the limits, reading only the central directory.
func CheckZip(path string, compressed int64) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil // not a readable zip: stored as an opaque file, never extracted
	}
	defer zr.Close()
	if len(zr.File) > MaxEntries {
		return fmt.Errorf("archive has too many entries (%d, limit %d)", len(zr.File), MaxEntries)
	}
	var total uint64
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if int64(total) > MaxUncompressed || total > 1<<62 {
			return fmt.Errorf("archive expands to more than %d MiB; refused as a decompression bomb", MaxUncompressed>>20)
		}
	}
	if compressed > 0 && int64(total)/compressed > MaxRatio && int64(total) > 100<<20 {
		return fmt.Errorf("archive compression ratio is above %d:1; refused as a decompression bomb", MaxRatio)
	}
	return nil
}

// eicar is the standard antivirus test string (not malware).
var eicar = []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)

// Scan inspects a stream in chunks for the built-in signature set (EICAR). It is the stand-in for an
// external scanner such as ClamAV, behind the same call.
type Scanner struct{ tail []byte }

// Feed scans the next chunk; it returns a signature name when one is found.
func (s *Scanner) Feed(chunk []byte) string {
	buf := append(s.tail, chunk...)
	if bytes.Contains(buf, eicar) {
		return "EICAR-Test-File"
	}
	if len(buf) > len(eicar) {
		s.tail = append([]byte(nil), buf[len(buf)-len(eicar)+1:]...)
	} else {
		s.tail = buf
	}
	return ""
}

// SafeInline lists the types that may preview inline; everything else is forced to download.
func SafeInline(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "application/pdf", "text/plain":
		return true
	}
	return strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/")
}
