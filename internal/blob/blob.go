// Package blob is the blob store: immutable, content-addressed objects behind a small interface,
// with a local filesystem backend (SPEC 17.6). Locators are derived from the SHA-256 only, so
// no user input ever becomes a path.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ErrNotFound = errors.New("blob not found")

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA reports whether s is a lowercase hex SHA-256; the only way to name an object.
func ValidSHA(s string) bool { return shaRe.MatchString(s) }

// Store is the backend interface: the filesystem backend (FS) and the S3 backend (S3).
type Store interface {
	// NewStaging creates an empty staging file and returns its id and path.
	NewStaging() (id string, f *os.File, err error)
	StagingPath(id string) (string, error)
	// Commit moves a verified staging file into the store under its hash. If the object exists the
	// staging file is discarded (deduplication). The caller supplies the hash it computed.
	Commit(stagingID, sha string) (existed bool, err error)
	Open(sha string) (Object, error)
	Remove(sha string) error
	Walk(fn func(sha string, size int64) error) error
	// CleanStaging removes staging files not touched since the cutoff and returns how many.
	CleanStaging(olderThan time.Time) (int, error)
	// WriteCheck proves the store is writable (used by doctor and readiness).
	WriteCheck() error
	// Put stores a small in-memory object (derived data such as thumbnails).
	Put(data []byte) (sha string, err error)
}

// FS is the local filesystem backend: root/ab/cd/<sha> for objects, root/staging/<id> for uploads in flight.
type FS struct{ Root string }

func NewFS(root string) (*FS, error) {
	for _, d := range []string{root, filepath.Join(root, "staging")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	return &FS{Root: root}, nil
}

func (s *FS) objPath(sha string) (string, error) {
	if !ValidSHA(sha) {
		return "", fmt.Errorf("invalid blob locator")
	}
	return filepath.Join(s.Root, sha[:2], sha[2:4], sha), nil
}

var idRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *FS) NewStaging() (string, *os.File, error) {
	b := make([]byte, 16)
	if _, err := randRead(b); err != nil {
		return "", nil, err
	}
	id := hex.EncodeToString(b)
	f, err := os.OpenFile(filepath.Join(s.Root, "staging", id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	return id, f, err
}

func (s *FS) StagingPath(id string) (string, error) {
	if !idRe.MatchString(id) {
		return "", fmt.Errorf("invalid staging id")
	}
	return filepath.Join(s.Root, "staging", id), nil
}

func (s *FS) Commit(stagingID, sha string) (bool, error) {
	src, err := s.StagingPath(stagingID)
	if err != nil {
		return false, err
	}
	dst, err := s.objPath(sha)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dst); err == nil {
		os.Remove(src)
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, err
	}
	if f, err := os.Open(src); err == nil { // make the bytes durable before the rename makes them visible
		f.Sync()
		f.Close()
	}
	if err := os.Rename(src, dst); err != nil {
		return false, err
	}
	os.Chmod(dst, 0o440)
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		d.Sync()
		d.Close()
	}
	return false, nil
}

// Raw is implemented by stores that can hold bytes under a name that is not their hash: an encrypted object is named by the
// hash of its plaintext, a tiered one is moved byte for byte. With replace the object under the name is replaced in one atomic
// step (a rewrap never leaves a moment with no object).
type Raw interface {
	CommitRaw(stagingID, name string, replace bool) (existed bool, err error)
}

// CommitRaw stores a staging file under name through the store's raw path, or through Commit when it has none.
func CommitRaw(st Store, stagingID, name string, replace bool) (bool, error) {
	if r, ok := st.(Raw); ok {
		return r.CommitRaw(stagingID, name, replace)
	}
	if replace {
		if err := st.Remove(name); err != nil && err != ErrNotFound {
			return false, err
		}
	}
	return st.Commit(stagingID, name)
}

func (s *FS) CommitRaw(stagingID, name string, replace bool) (bool, error) {
	if !replace {
		return s.Commit(stagingID, name)
	}
	src, err := s.StagingPath(stagingID)
	if err != nil {
		return false, err
	}
	dst, err := s.objPath(name)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, err
	}
	if f, err := os.Open(src); err == nil {
		f.Sync()
		f.Close()
	}
	if err := os.Rename(src, dst); err != nil { // rename replaces the old object atomically
		return false, err
	}
	os.Chmod(dst, 0o440)
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		d.Sync()
		d.Close()
	}
	return true, nil
}

func (s *FS) Open(sha string) (Object, error) {
	p, err := s.objPath(sha)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *FS) Remove(sha string) error {
	p, err := s.objPath(sha)
	if err != nil {
		return err
	}
	os.Chmod(p, 0o640)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *FS) Walk(fn func(string, int64) error) error {
	return filepath.WalkDir(s.Root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "staging" && filepath.Dir(p) == s.Root {
				return filepath.SkipDir
			}
			return nil
		}
		if !ValidSHA(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(d.Name(), info.Size())
	})
}

func (s *FS) CleanStaging(olderThan time.Time) (int, error) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "staging"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || info.ModTime().After(olderThan) {
			continue
		}
		if os.Remove(filepath.Join(s.Root, "staging", e.Name())) == nil {
			n++
		}
	}
	return n, nil
}

func (s *FS) WriteCheck() error {
	f := filepath.Join(s.Root, ".writecheck")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		return err
	}
	return os.Remove(f)
}

func (s *FS) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	id, f, err := s.NewStaging()
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	_, err = s.Commit(id, sha)
	return sha, err
}

// HashFile streams a file through SHA-256 with constant memory.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// SafeName reduces an uploaded file name to something that is only ever stored as metadata and shown:
// no path components, no control characters, bounded length.
func SafeName(n string) string {
	n = strings.ReplaceAll(n, "\\", "/")
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	var b strings.Builder
	for _, r := range n {
		if r >= 32 && r != 127 {
			b.WriteRune(r)
		}
	}
	n = strings.TrimSpace(b.String())
	if len([]rune(n)) > 200 {
		n = string([]rune(n)[:200])
	}
	if n == "" || n == "." || n == ".." {
		n = "file"
	}
	return n
}
