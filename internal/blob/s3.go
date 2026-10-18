package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Config configures the S3 backend (AWS S3 or any S3-compatible service such as MinIO).
type S3Config struct {
	Endpoint  string // empty: AWS (https://s3.<region>.amazonaws.com); otherwise a full URL such as http://minio:9000
	Region    string
	Bucket    string
	Prefix    string // optional key prefix, so a bucket can be shared
	AccessKey string
	SecretKey string
	PathStyle bool   // bucket in the path rather than the host; implied by a custom endpoint
	SSE       string // "", "AES256" or "aws:kms"
	KMSKeyID  string
	// PartSize is the multipart part size (minimum 5 MiB); objects up to PartSize are sent with one request.
	PartSize int64
	// HTTP and the retry knobs are for tests; zero values mean the defaults.
	HTTP     *http.Client
	Attempts int
	Sleep    func(time.Duration)
	Now      func() time.Time
}

// S3Error is an error response from the service.
type S3Error struct {
	Status  int
	Code    string
	Message string
	Op      string
}

func (e *S3Error) Error() string {
	return fmt.Sprintf("s3 %s: %d %s: %s", e.Op, e.Status, e.Code, e.Message)
}

// S3Client is a small S3 client: object operations, multipart uploads and presigned URLs.
type S3Client struct {
	cfg  S3Config
	sg   signer
	base *url.URL
}

func NewS3Client(cfg S3Config) (*S3Client, error) {
	if cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("s3: bucket, access key and secret key are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.PartSize == 0 {
		cfg.PartSize = 16 << 20
	}
	if cfg.PartSize < 5<<20 {
		return nil, errors.New("s3: the part size is at least 5 MiB")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 0, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second, MaxIdleConnsPerHost: 16}}
	}
	if cfg.Attempts == 0 {
		cfg.Attempts = 5
	}
	if cfg.Sleep == nil {
		cfg.Sleep = time.Sleep
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ep := cfg.Endpoint
	if ep == "" {
		ep = "https://s3." + cfg.Region + ".amazonaws.com"
	} else {
		cfg.PathStyle = true
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("s3: bad endpoint %q", ep)
	}
	if cfg.SSE != "" && cfg.SSE != "AES256" && cfg.SSE != "aws:kms" {
		return nil, errors.New("s3: sse is AES256 or aws:kms")
	}
	return &S3Client{cfg: cfg, sg: signer{AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey, Region: cfg.Region, Service: "s3"}, base: u}, nil
}

// objURL builds the URL of a key (already including any configured prefix).
func (c *S3Client) objURL(key string, q url.Values) *url.URL {
	u := *c.base
	host := c.base.Host
	path := "/" + uriEncode(c.cfg.Bucket, false) + "/" + uriEncode(key, true)
	if !c.cfg.PathStyle {
		host = c.cfg.Bucket + "." + c.base.Host
		path = "/" + uriEncode(key, true)
	}
	if key == "" {
		path = strings.TrimSuffix(path, "/")
		if path == "" {
			path = "/"
		}
	}
	u.Host = host
	u.Path, _ = url.PathUnescape(path)
	u.RawPath = path
	u.RawQuery = canonicalQuery(q)
	return &u
}

func retryable(status int, code string) bool {
	switch status {
	case 500, 502, 503, 504, 429:
		return true
	}
	return code == "SlowDown" || code == "RequestTimeout" || code == "InternalError" || code == "ServiceUnavailable"
}

// bodyFn yields a fresh body for each attempt (a retry must resend the bytes).
type bodyFn func() (io.ReadCloser, error)

func bytesBody(b []byte) bodyFn {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

// do sends a signed request with retries and backoff for throttling and server errors. It returns the response with
// its body open; the caller closes it. Non-2xx responses become *S3Error (and the body is consumed).
func (c *S3Client) do(ctx context.Context, op, method string, u *url.URL, hdr map[string]string, size int64, body bodyFn) (*http.Response, error) {
	var last error
	for attempt := 0; attempt < c.cfg.Attempts; attempt++ {
		if attempt > 0 {
			d := time.Duration(100<<uint(attempt-1))*time.Millisecond + time.Duration(rand.Intn(50))*time.Millisecond
			c.cfg.Sleep(d)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		var rc io.ReadCloser
		if body != nil {
			var err error
			if rc, err = body(); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), rc)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.ContentLength = size
			if size == 0 { // a non-nil body with length 0 is sent chunked, which S3 refuses: an empty object has no body at all
				req.Body = http.NoBody
				rc.Close()
			}
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		c.sg.signRequest(req, unsignedPayload, c.cfg.Now())
		resp, err := c.cfg.HTTP.Do(req)
		if err != nil {
			last = fmt.Errorf("s3 %s: %w", op, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue // a dropped connection is retried
		}
		if resp.StatusCode/100 == 2 {
			return resp, nil
		}
		serr := readS3Error(resp, op)
		if !retryable(serr.Status, serr.Code) {
			return nil, serr
		}
		last = serr
	}
	return nil, last
}

func readS3Error(resp *http.Response, op string) *S3Error {
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	xml.Unmarshal(b, &x)
	if x.Code == "" {
		x.Code = http.StatusText(resp.StatusCode)
	}
	return &S3Error{Status: resp.StatusCode, Code: x.Code, Message: x.Message, Op: op}
}

func (c *S3Client) key(k string) string { return c.cfg.Prefix + k }

func (c *S3Client) sseHeaders(h map[string]string) map[string]string {
	if c.cfg.SSE != "" {
		h["x-amz-server-side-encryption"] = c.cfg.SSE
		if c.cfg.SSE == "aws:kms" && c.cfg.KMSKeyID != "" {
			h["x-amz-server-side-encryption-aws-kms-key-id"] = c.cfg.KMSKeyID
		}
	}
	return h
}

func b64sha(hexSum string) string {
	b, _ := hex.DecodeString(hexSum)
	return base64.StdEncoding.EncodeToString(b)
}

// HeadBucket proves the credentials work and the bucket exists.
func (c *S3Client) HeadBucket(ctx context.Context) error {
	resp, err := c.do(ctx, "head bucket", "HEAD", c.objURL("", nil), nil, 0, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Stat returns the size of an object, or ErrNotFound.
func (c *S3Client) Stat(ctx context.Context, key string) (int64, error) {
	resp, err := c.do(ctx, "head "+key, "HEAD", c.objURL(c.key(key), nil), nil, 0, nil)
	if err != nil {
		var se *S3Error
		if errors.As(err, &se) && se.Status == 404 {
			return 0, ErrNotFound
		}
		return 0, err
	}
	resp.Body.Close()
	return resp.ContentLength, nil
}

// PutBytes stores a small object with an integrity checksum the service verifies.
func (c *S3Client) PutBytes(ctx context.Context, key string, data []byte, contentType string) error {
	sum := sha256.Sum256(data)
	h := c.sseHeaders(map[string]string{"x-amz-checksum-sha256": b64sha(hex.EncodeToString(sum[:]))})
	if contentType != "" {
		h["Content-Type"] = contentType
	}
	resp, err := c.do(ctx, "put "+key, "PUT", c.objURL(c.key(key), nil), h, int64(len(data)), bytesBody(data))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// PutFile uploads a local file: one request up to the part size, multipart above it. sha is the file's SHA-256 (hex),
// which a single-request upload sends as a checksum the service verifies. A failed multipart upload is aborted.
func (c *S3Client) PutFile(ctx context.Context, key, path, sha string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	open := func() (io.ReadCloser, error) { return os.Open(path) }
	if info.Size() <= c.cfg.PartSize {
		h := c.sseHeaders(map[string]string{})
		if sha != "" {
			h["x-amz-checksum-sha256"] = b64sha(sha)
		}
		resp, err := c.do(ctx, "put "+key, "PUT", c.objURL(c.key(key), nil), h, info.Size(), open)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	id, err := c.CreateMultipart(ctx, key)
	if err != nil {
		return err
	}
	var parts []Part
	for n, off := 1, int64(0); off < info.Size(); n, off = n+1, off+c.cfg.PartSize {
		length := c.cfg.PartSize
		if off+length > info.Size() {
			length = info.Size() - off
		}
		off, length := off, length
		sect := func() (io.ReadCloser, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			if _, err := f.Seek(off, io.SeekStart); err != nil {
				f.Close()
				return nil, err
			}
			return struct {
				io.Reader
				io.Closer
			}{io.LimitReader(f, length), f}, nil
		}
		resp, err := c.do(ctx, "upload part", "PUT", c.objURL(c.key(key), url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {id}}), nil, length, sect)
		if err != nil {
			c.AbortMultipart(context.Background(), key, id)
			return err
		}
		resp.Body.Close()
		parts = append(parts, Part{Number: n, ETag: resp.Header.Get("ETag")})
	}
	if err := c.CompleteMultipart(ctx, key, id, parts); err != nil {
		c.AbortMultipart(context.Background(), key, id)
		return err
	}
	return nil
}

type Part struct {
	Number int
	ETag   string
}

func (c *S3Client) CreateMultipart(ctx context.Context, key string) (string, error) {
	resp, err := c.do(ctx, "create multipart", "POST", c.objURL(c.key(key), url.Values{"uploads": {""}}), c.sseHeaders(map[string]string{}), 0, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var x struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&x); err != nil || x.UploadID == "" {
		return "", errors.New("s3: no upload id in the response")
	}
	return x.UploadID, nil
}

func (c *S3Client) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	var b bytes.Buffer
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		fmt.Fprintf(&b, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", p.Number, xmlEscape(p.ETag))
	}
	b.WriteString("</CompleteMultipartUpload>")
	resp, err := c.do(ctx, "complete multipart", "POST", c.objURL(c.key(key), url.Values{"uploadId": {uploadID}}), map[string]string{"Content-Type": "application/xml"}, int64(b.Len()), bytesBody(b.Bytes()))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// S3 may answer 200 with an error document in the body
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if bytes.Contains(body, []byte("<Error>")) {
		var x struct {
			Code    string `xml:"Code"`
			Message string `xml:"Message"`
		}
		xml.Unmarshal(body, &x)
		return &S3Error{Status: 200, Code: x.Code, Message: x.Message, Op: "complete multipart"}
	}
	return nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (c *S3Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	resp, err := c.do(ctx, "abort multipart", "DELETE", c.objURL(c.key(key), url.Values{"uploadId": {uploadID}}), nil, 0, nil)
	if err != nil {
		var se *S3Error
		if errors.As(err, &se) && se.Status == 404 {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// Copy copies an object inside the bucket on the server side (no bytes pass through us).
func (c *S3Client) Copy(ctx context.Context, from, to string) error {
	h := c.sseHeaders(map[string]string{"x-amz-copy-source": "/" + uriEncode(c.cfg.Bucket, false) + "/" + uriEncode(c.key(from), true)})
	resp, err := c.do(ctx, "copy", "PUT", c.objURL(c.key(to), nil), h, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if bytes.Contains(body, []byte("<Error>")) {
		return &S3Error{Status: 200, Code: "CopyFailed", Message: "the service reported an error while copying", Op: "copy"}
	}
	return nil
}

func (c *S3Client) Delete(ctx context.Context, key string) error {
	resp, err := c.do(ctx, "delete "+key, "DELETE", c.objURL(c.key(key), nil), nil, 0, nil)
	if err != nil {
		var se *S3Error
		if errors.As(err, &se) && se.Status == 404 {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// GetRange opens an object from offset to the end (or to end inclusive when end >= 0).
func (c *S3Client) GetRange(ctx context.Context, key string, off, end int64) (io.ReadCloser, error) {
	rng := fmt.Sprintf("bytes=%d-", off)
	if end >= 0 {
		rng = fmt.Sprintf("bytes=%d-%d", off, end)
	}
	resp, err := c.do(ctx, "get "+key, "GET", c.objURL(c.key(key), nil), map[string]string{"Range": rng}, 0, nil)
	if err != nil {
		var se *S3Error
		if errors.As(err, &se) && (se.Status == 404 || se.Code == "NoSuchKey") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return resp.Body, nil
}

type ListItem struct {
	Key  string
	Size int64
}

// List walks every key under prefix (relative to the configured prefix), page by page.
func (c *S3Client) List(ctx context.Context, prefix string, fn func(ListItem) error) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {c.key(prefix)}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, "list", "GET", c.objURL("", q), nil, 0, nil)
		if err != nil {
			return err
		}
		var x struct {
			Contents []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&x)
		resp.Body.Close()
		if err != nil {
			return err
		}
		for _, it := range x.Contents {
			if err := fn(ListItem{Key: strings.TrimPrefix(it.Key, c.cfg.Prefix), Size: it.Size}); err != nil {
				return err
			}
		}
		if !x.Truncated || x.Next == "" {
			return nil
		}
		token = x.Next
	}
}

// Origin is the scheme and host presigned URLs point at; a browser may connect there only if the page's CSP allows it.
func (c *S3Client) Origin() string {
	u := c.objURL("x", nil)
	return u.Scheme + "://" + u.Host
}

// Presign returns a time-limited URL for one method on one key. For a part upload pass partNumber and uploadID.
func (c *S3Client) Presign(method, key string, expires time.Duration, partNumber int, uploadID string) string {
	q := url.Values{}
	if uploadID != "" {
		q.Set("uploadId", uploadID)
		q.Set("partNumber", strconv.Itoa(partNumber))
	}
	return c.sg.presign(method, c.objURL(c.key(key), q), expires, c.cfg.Now())
}

// ---------- the Store ----------

// Object is what Open returns: the stored bytes, readable from any offset.
type Object interface {
	io.Reader
	io.Seeker
	io.Closer
}

// S3 is the object-store backend. Uploads are staged on local disk (so hashing, sniffing and scanning work the same as
// on the filesystem backend) and become visible only when the verified object is committed to its hash key.
type S3 struct {
	C       *S3Client
	staging *FS
}

// NewS3 uses stagingDir for local staging files; it never holds committed objects.
func NewS3(c *S3Client, stagingDir string) (*S3, error) {
	fs, err := NewFS(stagingDir)
	if err != nil {
		return nil, err
	}
	return &S3{C: c, staging: fs}, nil
}

func objKey(sha string) (string, error) {
	if !ValidSHA(sha) {
		return "", errors.New("invalid blob locator")
	}
	return sha[:2] + "/" + sha[2:4] + "/" + sha, nil
}

// Origin is where browsers send direct uploads.
func (s *S3) Origin() string { return s.C.Origin() }

func (s *S3) NewStaging() (string, *os.File, error) { return s.staging.NewStaging() }
func (s *S3) StagingPath(id string) (string, error) { return s.staging.StagingPath(id) }
func (s *S3) CleanStaging(t time.Time) (int, error) { return s.staging.CleanStaging(t) }

func (s *S3) Commit(stagingID, sha string) (bool, error) {
	key, err := objKey(sha)
	if err != nil {
		return false, err
	}
	path, err := s.staging.StagingPath(stagingID)
	if err != nil {
		return false, err
	}
	ctx := context.Background()
	if _, err := s.C.Stat(ctx, key); err == nil {
		os.Remove(path)
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if err := s.C.PutFile(ctx, key, path, sha); err != nil {
		return false, err // the staging file stays: the caller can retry or clean it
	}
	os.Remove(path)
	return false, nil
}

func (s *S3) Open(sha string) (Object, error) {
	key, err := objKey(sha)
	if err != nil {
		return nil, err
	}
	size, err := s.C.Stat(context.Background(), key)
	if err != nil {
		return nil, err
	}
	return &s3Reader{c: s.C, key: key, size: size}, nil
}

// s3Reader reads an object with ranged requests; a forward read streams one response, a seek starts a new one.
type s3Reader struct {
	c    *S3Client
	key  string
	size int64
	off  int64
	body io.ReadCloser
}

func (r *s3Reader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		b, err := r.c.GetRange(context.Background(), r.key, r.off, -1)
		if err != nil {
			return 0, err
		}
		r.body = b
	}
	n, err := r.body.Read(p)
	r.off += int64(n)
	if err == io.EOF && r.off < r.size { // the connection ended early: the next read resumes from where we are
		r.body.Close()
		r.body = nil
		err = nil
	}
	return n, err
}

func (r *s3Reader) Seek(off int64, whence int) (int64, error) {
	var n int64
	switch whence {
	case io.SeekStart:
		n = off
	case io.SeekCurrent:
		n = r.off + off
	case io.SeekEnd:
		n = r.size + off
	}
	if n < 0 {
		return 0, errors.New("negative seek")
	}
	if n != r.off && r.body != nil {
		r.body.Close()
		r.body = nil
	}
	r.off = n
	return n, nil
}

func (r *s3Reader) Close() error {
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}

func (s *S3) Remove(sha string) error {
	key, err := objKey(sha)
	if err != nil {
		return err
	}
	return s.C.Delete(context.Background(), key)
}

func (s *S3) Walk(fn func(string, int64) error) error {
	return s.C.List(context.Background(), "", func(it ListItem) error {
		parts := strings.Split(it.Key, "/")
		if len(parts) != 3 || !ValidSHA(parts[2]) || parts[2][:2] != parts[0] || parts[2][2:4] != parts[1] {
			return nil // staging objects and anything foreign are not blobs
		}
		return fn(parts[2], it.Size)
	})
}

func (s *S3) WriteCheck() error {
	ctx := context.Background()
	if err := s.C.HeadBucket(ctx); err != nil {
		return err
	}
	if err := s.C.PutBytes(ctx, ".writecheck", []byte("x"), ""); err != nil {
		return err
	}
	return s.C.Delete(ctx, ".writecheck")
}

func (s *S3) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	key, _ := objKey(sha)
	if _, err := s.C.Stat(context.Background(), key); err == nil {
		return sha, nil
	}
	return sha, s.C.PutBytes(context.Background(), key, data, "")
}
