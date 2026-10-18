package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"
)

// Fallback serves objects from Primary and, when one is not there, from Secondary: the store used while a deployment
// moves from one backend to another. Every write goes to Primary, so the old backend only ever shrinks in importance;
// a read that is answered by Secondary is copied to Primary on the way (read-through), so what people use moves first.
type Fallback struct {
	Primary, Secondary Store
	// CopyOnRead copies an object found only in Secondary into Primary (default true through NewFallback)
	CopyOnRead bool
}

func NewFallback(primary, secondary Store) *Fallback {
	return &Fallback{Primary: primary, Secondary: secondary, CopyOnRead: true}
}

func (f *Fallback) NewStaging() (string, *os.File, error) { return f.Primary.NewStaging() }
func (f *Fallback) StagingPath(id string) (string, error) { return f.Primary.StagingPath(id) }
func (f *Fallback) Commit(id, sha string) (bool, error)   { return f.Primary.Commit(id, sha) }
func (f *Fallback) WriteCheck() error                     { return f.Primary.WriteCheck() }
func (f *Fallback) Put(data []byte) (string, error)       { return f.Primary.Put(data) }
func (f *Fallback) CleanStaging(t time.Time) (int, error) { return f.Primary.CleanStaging(t) }

func (f *Fallback) Open(sha string) (Object, error) {
	o, err := f.Primary.Open(sha)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return o, err
	}
	if f.CopyOnRead {
		if cerr := copyObject(f.Secondary, f.Primary, sha); cerr == nil {
			if o, err := f.Primary.Open(sha); err == nil {
				return o, nil
			}
		}
	}
	return f.Secondary.Open(sha)
}

// Remove removes the object from both stores: an object that is unreferenced is unreferenced everywhere.
func (f *Fallback) Remove(sha string) error {
	e1 := f.Primary.Remove(sha)
	e2 := f.Secondary.Remove(sha)
	if e1 != nil && !errors.Is(e1, ErrNotFound) {
		return e1
	}
	if e2 != nil && !errors.Is(e2, ErrNotFound) {
		return e2
	}
	return nil
}

// Walk lists the objects of both stores, each once.
func (f *Fallback) Walk(fn func(sha string, size int64) error) error {
	seen := map[string]bool{}
	if err := f.Primary.Walk(func(sha string, size int64) error { seen[sha] = true; return fn(sha, size) }); err != nil {
		return err
	}
	return f.Secondary.Walk(func(sha string, size int64) error {
		if seen[sha] {
			return nil
		}
		return fn(sha, size)
	})
}

// copyObject streams one object from one store into another and checks its hash on the way: an object whose bytes do not
// hash to its name is never committed.
func copyObject(from, to Store, sha string) error {
	src, err := from.Open(sha)
	if err != nil {
		return err
	}
	defer src.Close()
	id, f, err := to.NewStaging()
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if p, perr := to.StagingPath(id); perr == nil {
			os.Remove(p)
		}
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		if p, perr := to.StagingPath(id); perr == nil {
			os.Remove(p)
		}
		return fmt.Errorf("the object %s is damaged in the source (its bytes hash to %s)", sha, got)
	}
	_, err = to.Commit(id, sha)
	return err
}

// MigrateReport says what a migration did.
type MigrateReport struct {
	Total    int
	Copied   int
	Skipped  int      // already in the destination with the right size
	Failed   []string // one line per object that could not be copied or verified
	Duration time.Duration
}

type MigrateOpts struct {
	Workers int
	// Verify re-reads every copied object from the destination and checks its hash
	Verify bool
	// Progress is called after each object with how many are done of the total
	Progress func(done, total int)
}

// Migrate copies the named objects from one store to another. It is safe to repeat and to stop: an object already in the
// destination with the right size is skipped, one that is not is copied again from the start, and nothing is ever removed
// from the source. A damaged source object is reported and not copied.
func Migrate(ctx context.Context, from, to Store, shas []string, o MigrateOpts) MigrateReport {
	start := time.Now()
	if o.Workers < 1 {
		o.Workers = 4
	}
	sort.Strings(shas)
	rep := MigrateReport{Total: len(shas)}
	var mu sync.Mutex
	var done int
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < o.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sha := range work {
				status, err := migrateOne(from, to, sha, o.Verify)
				mu.Lock()
				switch {
				case err != nil:
					rep.Failed = append(rep.Failed, sha+": "+err.Error())
				case status == "skipped":
					rep.Skipped++
				default:
					rep.Copied++
				}
				done++
				d := done
				mu.Unlock()
				if o.Progress != nil {
					o.Progress(d, len(shas))
				}
			}
		}()
	}
loop:
	for _, sha := range shas {
		select {
		case <-ctx.Done():
			break loop
		case work <- sha:
		}
	}
	close(work)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		rep.Failed = append(rep.Failed, "stopped: "+err.Error())
	}
	sort.Strings(rep.Failed)
	rep.Duration = time.Since(start)
	return rep
}

func migrateOne(from, to Store, sha string, verify bool) (string, error) {
	src, err := from.Open(sha)
	if err != nil {
		return "", fmt.Errorf("not in the source: %w", err)
	}
	size, serr := objSize(src)
	src.Close()
	if serr != nil {
		return "", serr
	}
	if dst, err := to.Open(sha); err == nil {
		dsz, _ := objSize(dst)
		same := dsz == size
		dst.Close()
		if same {
			return "skipped", nil
		}
	}
	if err := copyObject(from, to, sha); err != nil {
		return "", err
	}
	if verify {
		dst, err := to.Open(sha)
		if err != nil {
			return "", fmt.Errorf("not in the destination after copying: %w", err)
		}
		defer dst.Close()
		h := sha256.New()
		if _, err := io.Copy(h, dst); err != nil {
			return "", err
		}
		if hex.EncodeToString(h.Sum(nil)) != sha {
			return "", errors.New("the copy does not hash to its name")
		}
	}
	return "copied", nil
}

// objSize measures an object by seeking to its end, and leaves it at the start.
func objSize(o Object) (int64, error) {
	n, err := o.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	_, err = o.Seek(0, io.SeekStart)
	return n, err
}
