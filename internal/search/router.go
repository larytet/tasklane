package search

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Router is the Searcher the application uses (SPEC 10.1, TQL-08). Consumers that must be exact (Keys: board filters,
// reports, bulk targets) and queries that need history always run on PostgreSQL. Search boxes, lists and typeahead use the
// index when it is enabled and healthy, and fall back to PostgreSQL, with a warning, when it is not. The index being slow
// or down never blocks anything: the worst case is the PostgreSQL engine's answer.
type Router struct {
	PG    *Postgres
	OS    *OpenSearch // nil: PostgreSQL only
	Index *OSIndex    // lag, backlog and overlay; may be nil in tests that only route
	Now   func() time.Time
	// LagWarn is the lag past which the answer carries "search results may be a few seconds behind" (default 10 s).
	LagWarn time.Duration
	// Cooldown is how long a failing index is left alone before it is tried again (default 20 s).
	Cooldown time.Duration
	// Overlay is how far back a person's own edits are overlaid on list results (default 2 minutes).
	Overlay time.Duration

	mu        sync.Mutex
	downUntil time.Time
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Router) usable() bool {
	if r.OS == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.now().Before(r.downUntil)
}

func (r *Router) markDown() {
	cd := r.Cooldown
	if cd == 0 {
		cd = 20 * time.Second
	}
	r.mu.Lock()
	r.downUntil = r.now().Add(cd)
	r.mu.Unlock()
}

// usesHistory reports whether a query has WAS or CHANGED clauses.
func usesHistory(n tql.Node) bool {
	switch x := n.(type) {
	case tql.Bool:
		return usesHistory(x.L) || usesHistory(x.R)
	case tql.Not:
		return usesHistory(x.X)
	case tql.Cmp:
		return x.Op == "was" || x.Op == "wasnot" || x.Op == "changed"
	}
	return false
}

func (r *Router) Keys(ctx context.Context, sc Scope, src string, limit int) ([]string, error) {
	return r.PG.Keys(ctx, sc, src, limit)
}

func (r *Router) Search(ctx context.Context, sc Scope, src string, pg Page) (*Result, error) {
	if r.OS == nil {
		return r.PG.Search(ctx, sc, src, pg)
	}
	q, err := tql.Parse(src)
	if err != nil {
		return nil, err // the same error the PostgreSQL engine would give
	}
	if q.Where != nil && usesHistory(q.Where) {
		return r.PG.Search(ctx, sc, src, pg)
	}
	if !r.usable() {
		res, err := r.PG.Search(ctx, sc, src, pg)
		if res != nil {
			res.Warning = "search is limited: the search index is unavailable, so results come from the primary database"
		}
		return res, err
	}
	res, err := r.OS.Search(ctx, sc, src, pg)
	switch {
	case err == nil:
	case errors.Is(err, ErrUnsupported):
		return r.PG.Search(ctx, sc, src, pg)
	case errors.Is(err, ErrTermLimit):
		res, err := r.PG.Search(ctx, sc, src, pg)
		if res != nil {
			res.Warning = "you can read very many projects: this search ran on the primary database"
		}
		return res, err
	default:
		if _, isQuery := IsQueryError(err); isQuery {
			return nil, err
		}
		r.markDown()
		res, perr := r.PG.Search(ctx, sc, src, pg)
		if res != nil {
			res.Warning = "search is limited: the search index is unavailable, so results come from the primary database"
		}
		return res, perr
	}
	r.annotate(ctx, sc, src, pg, res)
	return res, nil
}

// annotate adds the lag indicator and overlays the caller's own recent edits, so a change they just made does not appear
// to vanish from, or linger in, a list that the index has not caught up with yet.
func (r *Router) annotate(ctx context.Context, sc Scope, src string, pg Page, res *Result) {
	if r.Index == nil {
		return
	}
	warn := r.LagWarn
	if warn == 0 {
		warn = 10 * time.Second
	}
	if b, err := r.Index.Backlog(ctx); err == nil && b.Lag > 0 {
		res.LagSeconds = b.Lag.Seconds()
		if b.Lag > warn {
			res.Warning = "search results may be a few seconds behind"
		}
	}
	if sc.UserID == "" {
		return
	}
	win := r.Overlay
	if win == 0 {
		win = 2 * time.Minute
	}
	pending, err := r.Index.PendingKeys(ctx)
	if err != nil || len(pending) == 0 {
		return // the index has heard everything: its answer needs no correction
	}
	rows, err := r.PG.DB.Query(ctx, `SELECT DISTINCT i.key FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE h.actor_id::text=$1 AND h.at > $2 AND i.key = ANY($3)`, sc.UserID, r.now().Add(-win), pending)
	if err != nil {
		return
	}
	var recent []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		recent = append(recent, k)
	}
	rows.Close()
	if len(recent) == 0 {
		return
	}
	only := sc
	only.OnlyKeys = recent
	fresh, err := r.PG.Search(ctx, only, src, Page{Limit: 200})
	if err != nil {
		return
	}
	match := map[string]bool{}
	for _, row := range fresh.Rows {
		match[row.Key] = true
	}
	isRecent := map[string]bool{}
	for _, k := range recent {
		isRecent[k] = true
	}
	kept := res.Rows[:0:0]
	have := map[string]bool{}
	for _, row := range res.Rows {
		if isRecent[row.Key] && !match[row.Key] {
			continue // the index still shows it matching, but their own change moved it out of the list
		}
		kept = append(kept, row)
		have[row.Key] = true
	}
	var add []Row
	for _, row := range fresh.Rows {
		if !have[row.Key] && pg.Offset == 0 {
			add = append(add, row) // matches now, not indexed yet: shown at the top of the first page
		}
	}
	res.Rows = append(add, kept...)
}

func (r *Router) Typeahead(ctx context.Context, sc Scope, term string, limit int) ([]Row, error) {
	if !r.usable() {
		return r.PG.Typeahead(ctx, sc, term, limit)
	}
	rows, err := r.OS.Typeahead(ctx, sc, term, limit)
	if err != nil {
		if !errors.Is(err, ErrTermLimit) {
			r.markDown()
		}
		return r.PG.Typeahead(ctx, sc, term, limit)
	}
	return rows, nil
}

var _ Searcher = (*Router)(nil)
