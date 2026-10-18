package blob

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Encryption at rest (BLOB-16). Every object is stored as: magic, the id of the master key that wrapped its data key,
// the wrapped data key, then the content in 64 KiB chunks each sealed with AES-256-GCM under that data key. A chunk's nonce
// is its number and its additional data names the object and says whether it is the last, so chunks cannot be reordered,
// swapped between objects or cut off. Reading supports seeking (ranges). Rotating the master key rewrites only the
// header (Rewrap): the content is never re-encrypted. Objects stored before encryption was switched on are served as they are
// until Rewrap encrypts them.
const (
	encMagic   = "TLE1"
	encChunk   = 64 << 10
	encHdrKey  = 12 + 32 + 16 // nonce, key, tag
	encHdrSize = len(encMagic) + 1 + encHdrKey
)

type Encrypted struct {
	Inner   Store
	Current byte            // id of the key new objects are wrapped with
	Keys    map[byte][]byte // master keys by id, 32 bytes each; old ones stay here until nothing uses them
}

func NewEncrypted(inner Store, current byte, keys map[byte][]byte) (*Encrypted, error) {
	for id, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("master key %d must be 32 bytes", id)
		}
	}
	if _, ok := keys[current]; !ok {
		return nil, errors.New("the current master key is not among the keys")
	}
	return &Encrypted{Inner: inner, Current: current, Keys: keys}, nil
}

func gcm(key []byte) cipher.AEAD {
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	return g
}

func chunkNonce(i uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], i)
	return n
}

func chunkAAD(sha string, last bool) []byte {
	a := []byte(sha)
	if last {
		return append(a, 1)
	}
	return append(a, 0)
}

func (e *Encrypted) wrap(sha string, dek []byte, id byte) []byte {
	nonce := make([]byte, 12)
	rand.Read(nonce)
	sealed := gcm(e.Keys[id]).Seal(nil, nonce, dek, append([]byte(sha), id))
	h := append([]byte(encMagic), id)
	h = append(h, nonce...)
	return append(h, sealed...)
}

func (e *Encrypted) unwrap(sha string, hdr []byte) ([]byte, error) {
	if len(hdr) < encHdrSize || string(hdr[:4]) != encMagic {
		return nil, errNotEncrypted
	}
	id := hdr[4]
	k, ok := e.Keys[id]
	if !ok {
		return nil, fmt.Errorf("the object was encrypted with master key %d, which is not configured", id)
	}
	dek, err := gcm(k).Open(nil, hdr[5:17], hdr[17:encHdrSize], append([]byte(sha), id))
	if err != nil {
		return nil, errors.New("the object's key cannot be unwrapped: the master key is wrong or the object was altered")
	}
	return dek, nil
}

var errNotEncrypted = errors.New("not encrypted")

func (e *Encrypted) NewStaging() (string, *os.File, error)   { return e.Inner.NewStaging() }
func (e *Encrypted) StagingPath(id string) (string, error)   { return e.Inner.StagingPath(id) }
func (e *Encrypted) CleanStaging(t time.Time) (int, error)   { return e.Inner.CleanStaging(t) }
func (e *Encrypted) WriteCheck() error                       { return e.Inner.WriteCheck() }
func (e *Encrypted) Remove(sha string) error                 { return e.Inner.Remove(sha) }
func (e *Encrypted) Walk(fn func(string, int64) error) error { return e.Inner.Walk(fn) }

// seal writes plaintext from r into a new inner staging file in the encrypted format and returns its id.
func (e *Encrypted) seal(sha string, r io.Reader, hdr []byte, dek []byte) (string, error) {
	id, f, err := e.Inner.NewStaging()
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		f.Close()
		if p, perr := e.Inner.StagingPath(id); perr == nil {
			os.Remove(p)
		}
		return "", err
	}
	if _, err := f.Write(hdr); err != nil {
		return fail(err)
	}
	g := gcm(dek)
	buf := make([]byte, encChunk)
	next := make([]byte, 1)
	var i uint64
	n, err := io.ReadFull(r, buf)
	for {
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return fail(err)
		}
		last := err != nil // a short read or end of input: this is the final chunk (possibly empty)
		if !last {         // look ahead one byte so the last full chunk is marked last too
			m, e2 := io.ReadFull(r, next)
			if m == 0 {
				last = true
			} else {
				_ = e2
				if _, werr := f.Write(g.Seal(nil, chunkNonce(i), buf[:n], chunkAAD(sha, false))); werr != nil {
					return fail(werr)
				}
				i++
				buf[0] = next[0]
				k, err2 := io.ReadFull(r, buf[1:])
				n, err = 1+k, err2
				continue
			}
		}
		if _, werr := f.Write(g.Seal(nil, chunkNonce(i), buf[:n], chunkAAD(sha, true))); werr != nil {
			return fail(werr)
		}
		break
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	return id, nil
}

// Commit encrypts the verified staging file and stores it under its (plaintext) hash.
func (e *Encrypted) Commit(stagingID, sha string) (bool, error) {
	p, err := e.Inner.StagingPath(stagingID)
	if err != nil {
		return false, err
	}
	in, err := os.Open(p)
	if err != nil {
		return false, err
	}
	dek := make([]byte, 32)
	rand.Read(dek)
	sid, err := e.seal(sha, in, e.wrap(sha, dek, e.Current), dek)
	in.Close()
	if err != nil {
		return false, err
	}
	os.Remove(p)
	return CommitRaw(e.Inner, sid, sha, false)
}

func (e *Encrypted) Put(data []byte) (string, error) {
	id, f, err := e.Inner.NewStaging()
	if err != nil {
		return "", err
	}
	f.Write(data)
	f.Close()
	sha, _, err := hashStaging(e.Inner, id)
	if err != nil {
		return "", err
	}
	if _, err := e.Commit(id, sha); err != nil {
		return "", err
	}
	return sha, nil
}

func hashStaging(s Store, id string) (string, int64, error) {
	p, err := s.StagingPath(id)
	if err != nil {
		return "", 0, err
	}
	return HashFile(p)
}

// Open returns a decrypting, seekable view of the object. An object that was stored before encryption was on is returned as it is.
func (e *Encrypted) Open(sha string) (Object, error) {
	o, err := e.Inner.Open(sha)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, encHdrSize)
	n, _ := io.ReadFull(o, hdr)
	dek, err := e.unwrap(sha, hdr[:n])
	if err == errNotEncrypted {
		o.Seek(0, io.SeekStart)
		return o, nil
	}
	if err != nil {
		o.Close()
		return nil, err
	}
	total, err := o.Seek(0, io.SeekEnd)
	if err != nil {
		o.Close()
		return nil, err
	}
	body := total - int64(encHdrSize)
	if body < 16 {
		o.Close()
		return nil, errors.New("the encrypted object is truncated")
	}
	full := int64(encChunk + 16)
	chunks := (body + full - 1) / full
	lastCT := body - (chunks-1)*full
	plain := (chunks-1)*encChunk + (lastCT - 16)
	return &decReader{o: o, g: gcm(dek), sha: sha, chunks: chunks, size: plain, cur: -1}, nil
}

type decReader struct {
	o      Object
	g      cipher.AEAD
	sha    string
	chunks int64
	size   int64
	pos    int64
	cur    int64
	buf    []byte
}

func (d *decReader) load(i int64) error {
	if d.cur == i {
		return nil
	}
	full := int64(encChunk + 16)
	if _, err := d.o.Seek(int64(encHdrSize)+i*full, io.SeekStart); err != nil {
		return err
	}
	last := i == d.chunks-1
	ct := make([]byte, full)
	n, err := io.ReadFull(d.o, ct)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	pt, err := d.g.Open(nil, chunkNonce(uint64(i)), ct[:n], chunkAAD(d.sha, last))
	if err != nil {
		return errors.New("the stored object is corrupt or was altered (a chunk failed authentication)")
	}
	d.buf, d.cur = pt, i
	return nil
}

func (d *decReader) Read(p []byte) (int, error) {
	if d.pos >= d.size {
		return 0, io.EOF
	}
	i := d.pos / encChunk
	if err := d.load(i); err != nil {
		return 0, err
	}
	off := d.pos - i*encChunk
	n := copy(p, d.buf[off:])
	d.pos += int64(n)
	return n, nil
}

func (d *decReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += d.pos
	case io.SeekEnd:
		off += d.size
	}
	if off < 0 {
		return 0, errors.New("negative seek")
	}
	d.pos = off
	return off, nil
}

func (d *decReader) Close() error { return d.o.Close() }

// Rewrap points an object's data key at the current master key (or encrypts an object stored before encryption was on).
// The content is not re-encrypted when the object is already encrypted: only the header changes. It reports whether it changed anything.
func (e *Encrypted) Rewrap(sha string) (bool, error) {
	o, err := e.Inner.Open(sha)
	if err != nil {
		return false, err
	}
	defer o.Close()
	hdr := make([]byte, encHdrSize)
	n, _ := io.ReadFull(o, hdr)
	dek, uerr := e.unwrap(sha, hdr[:n])
	if uerr == nil && hdr[4] == e.Current {
		return false, nil
	}
	var sid string
	if uerr == errNotEncrypted {
		o.Seek(0, io.SeekStart)
		dek = make([]byte, 32)
		rand.Read(dek)
		sid, err = e.seal(sha, o, e.wrap(sha, dek, e.Current), dek)
	} else if uerr != nil {
		return false, uerr
	} else {
		id, f, serr := e.Inner.NewStaging()
		if serr != nil {
			return false, serr
		}
		f.Write(e.wrap(sha, dek, e.Current))
		o.Seek(int64(encHdrSize), io.SeekStart)
		_, err = io.Copy(f, o)
		f.Close()
		sid = id
	}
	if err != nil {
		return false, err
	}
	// the new copy is complete, and replaces the old one in a single step
	_, err = CommitRaw(e.Inner, sid, sha, true)
	return err == nil, err
}

// RewrapReport says what RewrapAll did.
type RewrapReport struct {
	Total, Changed int
	Failed         []string
}

// RewrapAll moves every named object to the current master key (and encrypts any stored before encryption was on).
// It is safe to repeat and to stop: an object already on the current key is left alone.
func (e *Encrypted) RewrapAll(shas []string) RewrapReport {
	r := RewrapReport{Total: len(shas)}
	for _, sha := range shas {
		changed, err := e.Rewrap(sha)
		switch {
		case err != nil:
			r.Failed = append(r.Failed, sha+": "+err.Error())
		case changed:
			r.Changed++
		}
	}
	return r
}
