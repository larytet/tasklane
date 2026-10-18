package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// Tiered is a hot store and a cold store behind one interface (SPEC 17.6). New objects go to the hot tier; an object is moved to
// the cold one by MoveCold, which copies it, reads the copy back and compares it, and only then removes the hot copy. Reads are
// transparent: the hot tier is asked first and the cold one second, and because the cold copy exists before the hot one is
// removed there is no moment at which an object is in neither. It works on whatever bytes the stores hold, so under the
// encryption wrapper it moves ciphertext and never needs a key.
type Tiered struct {
	Hot, Cold Store

	hotReads, coldReads atomic.Int64
	hotNanos, coldNanos atomic.Int64 // time to open and read the first byte
}

func NewTiered(hot, cold Store) *Tiered { return &Tiered{Hot: hot, Cold: cold} }

// TierStats counts reads by tier and how long opening an object took on average, for the latency the documentation reports.
type TierStats struct {
	HotReads, ColdReads int64
	HotAvg, ColdAvg     time.Duration
}

func (t *Tiered) Stats() TierStats {
	s := TierStats{HotReads: t.hotReads.Load(), ColdReads: t.coldReads.Load()}
	if s.HotReads > 0 {
		s.HotAvg = time.Duration(t.hotNanos.Load() / s.HotReads)
	}
	if s.ColdReads > 0 {
		s.ColdAvg = time.Duration(t.coldNanos.Load() / s.ColdReads)
	}
	return s
}

func (t *Tiered) NewStaging() (string, *os.File, error) { return t.Hot.NewStaging() }
func (t *Tiered) StagingPath(id string) (string, error) { return t.Hot.StagingPath(id) }
func (t *Tiered) CleanStaging(o time.Time) (int, error) { return t.Hot.CleanStaging(o) }
func (t *Tiered) WriteCheck() error {
	if err := t.Hot.WriteCheck(); err != nil {
		return fmt.Errorf("hot tier: %w", err)
	}
	if err := t.Cold.WriteCheck(); err != nil {
		return fmt.Errorf("cold tier: %w", err)
	}
	return nil
}

func (t *Tiered) has(s Store, sha string) bool {
	o, err := s.Open(sha)
	if err != nil {
		return false
	}
	o.Close()
	return true
}

// Commit stores a new object in the hot tier, unless the cold tier already has it (de-duplication across tiers).
func (t *Tiered) Commit(stagingID, sha string) (bool, error) {
	if t.has(t.Cold, sha) {
		if p, err := t.Hot.StagingPath(stagingID); err == nil {
			os.Remove(p)
		}
		return true, nil
	}
	return t.Hot.Commit(stagingID, sha)
}

// CommitRaw puts the bytes where the object already lives (a replacement stays in its tier), else in the hot tier.
func (t *Tiered) CommitRaw(stagingID, name string, replace bool) (bool, error) {
	if replace && !t.has(t.Hot, name) && t.has(t.Cold, name) {
		return t.commitToCold(stagingID, name)
	}
	if !replace && t.has(t.Cold, name) {
		if p, err := t.Hot.StagingPath(stagingID); err == nil {
			os.Remove(p)
		}
		return true, nil
	}
	return CommitRaw(t.Hot, stagingID, name, replace)
}

// commitToCold replaces a cold object with a file staged in the hot tier's staging area (staging is always the hot tier's):
// the bytes are copied into the cold tier's own staging and committed from there.
func (t *Tiered) commitToCold(stagingID, name string) (bool, error) {
	p, err := t.Hot.StagingPath(stagingID)
	if err != nil {
		return false, err
	}
	in, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer in.Close()
	id, f, err := t.Cold.NewStaging()
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		if cp, perr := t.Cold.StagingPath(id); perr == nil {
			os.Remove(cp)
		}
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	existed, err := CommitRaw(t.Cold, id, name, true)
	if err == nil {
		os.Remove(p)
	}
	return existed, err
}

func (t *Tiered) Open(sha string) (Object, error) {
	start := time.Now()
	if o, err := t.Hot.Open(sha); err == nil {
		t.hotReads.Add(1)
		t.hotNanos.Add(int64(time.Since(start)))
		return o, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	o, err := t.Cold.Open(sha)
	if err == nil {
		t.coldReads.Add(1)
		t.coldNanos.Add(int64(time.Since(start)))
	}
	return o, err
}

func (t *Tiered) Put(data []byte) (string, error) {
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if t.has(t.Cold, sha) {
		return sha, nil
	}
	return t.Hot.Put(data)
}

func (t *Tiered) Remove(sha string) error {
	e1, e2 := t.Hot.Remove(sha), t.Cold.Remove(sha)
	if e1 != nil && !errors.Is(e1, ErrNotFound) {
		return e1
	}
	if e2 != nil && !errors.Is(e2, ErrNotFound) {
		return e2
	}
	return nil
}

// Walk lists both tiers; an object present in both (a move in progress) is listed once.
func (t *Tiered) Walk(fn func(sha string, size int64) error) error {
	seen := map[string]bool{}
	if err := t.Hot.Walk(func(sha string, size int64) error { seen[sha] = true; return fn(sha, size) }); err != nil {
		return err
	}
	return t.Cold.Walk(func(sha string, size int64) error {
		if seen[sha] {
			return nil
		}
		return fn(sha, size)
	})
}

// Where says which tier holds the object: "hot", "cold", "both" (a move was interrupted before the hot copy was removed) or "".
func (t *Tiered) Where(sha string) string {
	h, c := t.has(t.Hot, sha), t.has(t.Cold, sha)
	switch {
	case h && c:
		return "both"
	case h:
		return "hot"
	case c:
		return "cold"
	}
	return ""
}

// MoveCold moves one object from the hot tier to the cold one. It is safe to repeat and to stop at any point: the object is
// always readable, an interrupted move leaves it in both tiers and the next call finishes it.
func (t *Tiered) MoveCold(sha string) error {
	where := t.Where(sha)
	switch where {
	case "":
		return ErrNotFound
	case "cold":
		return nil
	}
	src, err := t.Hot.Open(sha)
	if err != nil {
		return err
	}
	defer src.Close()
	// the bytes as they are in the hot tier, with their own hash, so the copy can be checked whatever they are (ciphertext too)
	want, size, err := hashReader(src)
	if err != nil {
		return err
	}
	if where == "hot" {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		id, f, err := t.Cold.NewStaging()
		if err != nil {
			return err
		}
		fail := func(err error) error {
			f.Close()
			if p, perr := t.Cold.StagingPath(id); perr == nil {
				os.Remove(p)
			}
			return err
		}
		if _, err := io.Copy(f, src); err != nil {
			return fail(err)
		}
		if err := f.Close(); err != nil {
			return fail(err)
		}
		if _, err := CommitRaw(t.Cold, id, sha, false); err != nil {
			return err // the staging file stays for a retry or the cleaner
		}
	}
	// read the cold copy back: the hot one goes only if the cold one is the same bytes
	cold, err := t.Cold.Open(sha)
	if err != nil {
		return fmt.Errorf("the cold copy cannot be read back: %w", err)
	}
	defer cold.Close()
	got, gsize, err := hashReader(cold)
	if err != nil {
		return fmt.Errorf("the cold copy cannot be read back: %w", err)
	}
	if got != want || gsize != size {
		return fmt.Errorf("the cold copy of %s differs from the hot one (%d bytes, hash %s; wanted %d bytes, %s): the hot copy is kept", sha, gsize, got[:12], size, want[:12])
	}
	return t.Hot.Remove(sha)
}

func hashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// AsTiered finds the tiered store inside the wrappers (encryption, migration fallback) around it.
func AsTiered(s Store) *Tiered {
	switch v := s.(type) {
	case *Tiered:
		return v
	case *Encrypted:
		return AsTiered(v.Inner)
	case *Fallback:
		return AsTiered(v.Primary)
	}
	return nil
}
