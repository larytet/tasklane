package testutil

import (
	"strings"

	"github.com/larytet/tasklane/internal/domain"
)

// Reconcile is the measurement (SPEC 21): newest-first versions of one text field and the tokens whose saves were
// acknowledged. It returns the E1 (lost acknowledged save), E2 (stale save replaced a newer change) and E4
// (stored content differs from what was saved: duplicated) counts, and whether the sequence has a gap.
func Reconcile(tokens []string, vs []domain.TextVersion) (e1, e2, e4 int, gap bool) {
	final := vs[0].Body
	for _, tk := range tokens {
		switch c := strings.Count(final, tk); {
		case c == 0:
			e1++
		case c > 1:
			e4++
		}
	}
	for i := 0; i < len(vs)-1; i++ {
		newer, older := vs[i], vs[i+1]
		if newer.Seq != older.Seq+1 {
			gap = true
		}
		for _, b := range strings.Split(older.Body, "\n\n") {
			if !strings.Contains(newer.Body, b) {
				e2++
				break
			}
		}
	}
	if int64(1+len(tokens)) != vs[0].Seq {
		gap = true
	}
	return
}
