// Package s3mock is an in-process S3 test double (path-style): objects, ranges, copy, listing, multipart, presigned
// URLs, and signature verification, with fault injection. It stands in for AWS S3 and MinIO in the unit and integration
// tiers (TESTPLAN 3.2); the same backend tests run against real MinIO in the compose environment.
package s3mock

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/larytet/tasklane/internal/blob"
)

const (
	AccessKey = "AKIATESTACCESSKEY"
	SecretKey = "test-secret-key/with+symbols"
	Region    = "eu-test-1"
	Bucket    = "tasklane-test"
)

type object struct {
	data []byte
	etag string
	mod  time.Time
}

type upload struct {
	key   string
	parts map[int][]byte
}

type fault struct {
	remaining int
	status    int
	code      string
	match     func(*http.Request) bool
}

type Server struct {
	*httptest.Server
	mu      sync.Mutex
	objects map[string]*object
	uploads map[string]*upload
	faults  []*fault
	reqs    []string
	MinPart int // smallest non-final part, like S3's 5 MiB
	Now     func() time.Time
	nextUp  int
	// SawSSE records the last x-amz-server-side-encryption header seen on a write.
	SawSSE string
}

func New() *Server {
	s := &Server{objects: map[string]*object{}, uploads: map[string]*upload{}, MinPart: 5 << 20, Now: time.Now}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Fail makes the next n requests that match (nil matches all) answer with the given status and S3 error code.
func (s *Server) Fail(n, status int, code string, match func(*http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{n, status, code, match})
}

// Reset clears injected faults (and nothing else).
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// Requests returns "METHOD path" for every request served so far.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func (s *Server) Count(method, contains string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, contains) {
			n++
		}
	}
	return n
}

// Keys lists the stored keys, sorted.
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var k []string
	for key := range s.objects {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func (s *Server) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), o.data...), true
}

// Tamper changes a stored object's bytes behind the application's back (for corruption and integrity tests).
func (s *Server) Tamper(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objects[key]; ok {
		o.data = data
	}
}

func (s *Server) OpenUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

func xmlErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.RequestURI())
	for _, f := range s.faults {
		if f.remaining > 0 && (f.match == nil || f.match(r)) {
			f.remaining--
			s.mu.Unlock()
			io.Copy(io.Discard, r.Body)
			xmlErr(w, f.status, f.code, "injected failure")
			return
		}
	}
	s.mu.Unlock()
	if err := blob.VerifySigV4(r, AccessKey, SecretKey, Region, s.Now()); err != nil {
		io.Copy(io.Discard, r.Body)
		xmlErr(w, 403, "SignatureDoesNotMatch", err.Error())
		return
	}
	path := r.URL.Path
	prefix := "/" + Bucket
	if path != prefix && !strings.HasPrefix(path, prefix+"/") {
		xmlErr(w, 404, "NoSuchBucket", "the bucket does not exist")
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(path, prefix), "/")
	q := r.URL.Query()
	switch {
	case key == "" && r.Method == "HEAD":
		w.WriteHeader(200)
	case key == "" && r.Method == "GET":
		s.list(w, q)
	case r.Method == "POST" && q.Has("uploads"):
		s.createUpload(w, key)
	case r.Method == "PUT" && q.Has("partNumber"):
		s.putPart(w, r, q)
	case r.Method == "POST" && q.Has("uploadId"):
		s.complete(w, r, key, q.Get("uploadId"))
	case r.Method == "DELETE" && q.Has("uploadId"):
		s.mu.Lock()
		delete(s.uploads, q.Get("uploadId"))
		s.mu.Unlock()
		w.WriteHeader(204)
	case r.Method == "PUT" && r.Header.Get("x-amz-copy-source") != "":
		s.copy(w, r, key)
	case r.Method == "PUT":
		s.put(w, r, key)
	case r.Method == "GET" || r.Method == "HEAD":
		s.get(w, r, key)
	case r.Method == "DELETE":
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(204)
	default:
		xmlErr(w, 405, "MethodNotAllowed", "not supported by the test double")
	}
}

func (s *Server) put(w http.ResponseWriter, r *http.Request, key string) {
	if r.ContentLength < 0 {
		xmlErr(w, 411, "MissingContentLength", "a content length is required")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil || int64(len(data)) != r.ContentLength {
		xmlErr(w, 400, "IncompleteBody", "the body was shorter than its content length")
		return
	}
	if want := r.Header.Get("x-amz-checksum-sha256"); want != "" {
		sum := sha256.Sum256(data)
		if base64.StdEncoding.EncodeToString(sum[:]) != want {
			xmlErr(w, 400, "BadDigest", "the checksum does not match the body")
			return
		}
	}
	sum := md5.Sum(data)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	s.mu.Lock()
	s.objects[key] = &object{data: data, etag: etag, mod: s.Now()}
	s.SawSSE = r.Header.Get("x-amz-server-side-encryption")
	s.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.WriteHeader(200)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	o, ok := s.objects[key]
	var data []byte
	if ok {
		data = o.data
	}
	s.mu.Unlock()
	if !ok {
		if r.Method == "HEAD" {
			w.WriteHeader(404)
			return
		}
		xmlErr(w, 404, "NoSuchKey", "the key does not exist")
		return
	}
	w.Header().Set("ETag", o.etag)
	w.Header().Set("Accept-Ranges", "bytes")
	status := 200
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") && r.Method == "GET" {
		parts := strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)
		start, err := strconv.ParseInt(parts[0], 10, 64)
		end := int64(len(data)) - 1
		if len(parts) == 2 && parts[1] != "" {
			end, _ = strconv.ParseInt(parts[1], 10, 64)
		}
		if err != nil || start >= int64(len(data)) {
			xmlErr(w, 416, "InvalidRange", "the requested range is not satisfiable")
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data, status = data[start:end+1], 206
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method == "GET" {
		w.Write(data)
	}
}

func (s *Server) copy(w http.ResponseWriter, r *http.Request, key string) {
	src := strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "/"+Bucket+"/")
	s.mu.Lock()
	o, ok := s.objects[src]
	if ok {
		s.objects[key] = &object{data: append([]byte(nil), o.data...), etag: o.etag, mod: s.Now()}
	}
	s.mu.Unlock()
	if !ok {
		xmlErr(w, 404, "NoSuchKey", "the copy source does not exist")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<CopyObjectResult><ETag>%s</ETag></CopyObjectResult>`, o.etag)
}

func (s *Server) list(w http.ResponseWriter, q map[string][]string) {
	prefix := ""
	if v := q["prefix"]; len(v) > 0 {
		prefix = v[0]
	}
	after := ""
	if v := q["continuation-token"]; len(v) > 0 {
		after = v[0]
	}
	keys := []string{}
	for _, k := range s.Keys() {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	const page = 2 // small pages so paging is exercised
	truncated := len(keys) > page
	if truncated {
		keys = keys[:page]
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, k := range keys {
		s.mu.Lock()
		n := len(s.objects[k].data)
		s.mu.Unlock()
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, k, n)
	}
	fmt.Fprintf(&b, `<IsTruncated>%v</IsTruncated>`, truncated)
	if truncated {
		fmt.Fprintf(&b, `<NextContinuationToken>%s</NextContinuationToken>`, keys[len(keys)-1])
	}
	b.WriteString(`</ListBucketResult>`)
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, b.String())
}

func (s *Server) createUpload(w http.ResponseWriter, key string) {
	s.mu.Lock()
	s.nextUp++
	id := fmt.Sprintf("upload-%d", s.nextUp)
	s.uploads[id] = &upload{key: key, parts: map[int][]byte{}}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, Bucket, key, id)
}

func (s *Server) putPart(w http.ResponseWriter, r *http.Request, q map[string][]string) {
	data, _ := io.ReadAll(r.Body)
	n, _ := strconv.Atoi(q["partNumber"][0])
	s.mu.Lock()
	up, ok := s.uploads[q["uploadId"][0]]
	if ok {
		up.parts[n] = data
	}
	s.mu.Unlock()
	if !ok {
		xmlErr(w, 404, "NoSuchUpload", "the upload does not exist")
		return
	}
	sum := md5.Sum(data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(200)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request, key, id string) {
	var req struct {
		Parts []struct {
			Number int    `xml:"PartNumber"`
			ETag   string `xml:"ETag"`
		} `xml:"Part"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		xmlErr(w, 400, "MalformedXML", "the completion document is not valid")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	up, ok := s.uploads[id]
	if !ok {
		xmlErr(w, 404, "NoSuchUpload", "the upload does not exist")
		return
	}
	var all []byte
	last := 0
	for i, p := range req.Parts {
		data, have := up.parts[p.Number]
		sum := md5.Sum(data)
		if !have || p.Number <= last || `"`+hex.EncodeToString(sum[:])+`"` != p.ETag {
			xmlErr(w, 400, "InvalidPart", "a part is missing, out of order, or its ETag does not match")
			return
		}
		if i < len(req.Parts)-1 && len(data) < s.MinPart {
			xmlErr(w, 400, "EntityTooSmall", "a part other than the last is below the minimum size")
			return
		}
		last = p.Number
		all = append(all, data...)
	}
	sum := md5.Sum(all)
	etag := fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(sum[:]), len(req.Parts))
	s.objects[key] = &object{data: all, etag: etag, mod: s.Now()}
	delete(s.uploads, id)
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>%s</ETag></CompleteMultipartUploadResult>`, Bucket, key, etag)
}
