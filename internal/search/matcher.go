package search

import (
	"context"
	"time"
)

// Matcher answers "does this issue match this query" and "which issues match" for automation. It sees everything (the
// rule's own permissions are decided when the rule is saved), and archived and trashed issues never match.
type Matcher struct {
	Engine Searcher
	Now    func() time.Time
}

func (m Matcher) scope(keys []string) Scope {
	now := m.Now
	if now == nil {
		now = time.Now
	}
	return Scope{All: true, OnlyKeys: keys, WeekStart: time.Monday, Now: now}
}

func (m Matcher) Matches(ctx context.Context, key, query string) (bool, error) {
	res, err := m.Engine.Search(ctx, m.scope([]string{key}), query, Page{Limit: 1})
	if err != nil {
		return false, err
	}
	return len(res.Rows) > 0, nil
}

func (m Matcher) Keys(ctx context.Context, query string, limit int) ([]string, error) {
	return m.Engine.Keys(ctx, m.scope(nil), query, limit)
}
