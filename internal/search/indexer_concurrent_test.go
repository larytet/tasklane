package search_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

// Two nodes run an indexer at once (always the case with more than one node, and during a rolling upgrade): their batches overlap on issues, and
// they must take the rows in the same order, or two of them can wait for each other (found by the rolling-upgrade test, a deadlock in the log).
func TestTQL10_ConcurrentIndexersDoNotDeadlock(t *testing.T) {
	f := newFx(t)
	var keys []string
	for i := 0; i < 8; i++ {
		is := f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprintf("issue %d", i)})
		keys = append(keys, is.Key)
	}
	f.index()
	errs := 0
	for round := 0; round < 40; round++ {
		for pass := 0; pass < 2; pass++ { // every issue changes searchable text twice, so batches taken by different nodes mention the same issues
			for _, k := range keys {
				l := []string{fmt.Sprintf("round%dpass%d", round, pass)}
				if _, err := f.Svc.UpdateFields(ctx, f.Alice, k, nil, false, domain.Changes{Labels: &l}); err != nil {
					t.Fatal(err)
				}
			}
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ix := &search.Indexer{DB: f.Store.Pool, Batch: 4}
				for {
					n, err := ix.RunOnce(ctx)
					if err != nil {
						mu.Lock()
						errs++
						mu.Unlock()
						t.Logf("round %d: %v", round, err)
						return
					}
					if n == 0 {
						return
					}
				}
			}()
		}
		wg.Wait()
		f.index() // whatever a failed batch left
	}
	if errs != 0 {
		t.Fatalf("%d indexer batches failed", errs)
	}
}
