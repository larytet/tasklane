package search

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/larytet/tasklane/internal/search/tql"
)

const TotalCap = 10000

type Row struct {
	Key       string    `json:"key"`
	Summary   string    `json:"summary"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	StatusCat string    `json:"status_category"`
	Priority  string    `json:"priority"`
	Assignee  string    `json:"assignee,omitempty"`
	Project   string    `json:"project"`
	Labels    []string  `json:"labels"`
	Updated   time.Time `json:"updated"`
	Comments  int       `json:"comments"`
}

type Result struct {
	Rows        []Row  `json:"issues"`
	Total       int    `json:"total"`
	TotalCapped bool   `json:"total_capped"` // true when the real total is at least Total
	Query       string `json:"query"`
}

type Page struct{ Limit, Offset int }

// Postgres is the default search engine (SPEC 10.1).
type Postgres struct{ DB *pgxpool.Pool }

const listFrom = ` FROM issue i JOIN project p ON p.id=i.project_id JOIN issue_type t ON t.id=i.issue_type_id JOIN status st ON st.id=i.status_id
	JOIN app_user rp ON rp.id=i.reporter_id LEFT JOIN app_user asg ON asg.id=i.assignee_id LEFT JOIN issue par ON par.id=i.parent_id `

// Search parses, compiles under the scope and runs a TQL query.
func (e *Postgres) Search(ctx context.Context, sc Scope, src string, pg Page) (*Result, error) {
	q, err := tql.Parse(src)
	if err != nil {
		return nil, err
	}
	cq, err := compile(q, sc)
	if err != nil {
		return nil, err
	}
	if pg.Limit <= 0 || pg.Limit > 200 {
		pg.Limit = 50
	}
	if pg.Offset < 0 {
		pg.Offset = 0
	}
	order := cq.order
	sel := `SELECT i.key,i.summary,t.name,st.name,st.category,i.priority,COALESCE(asg.name,''),p.key,i.labels,i.updated_at,
		(SELECT count(*) FROM comment c WHERE c.issue_id=i.id)`
	from := listFrom
	args := append([]any{}, cq.args...)
	if order == "" {
		if cq.rankArg > 0 { // text search: exact key first, then relevance, then recency
			from += `LEFT JOIN issue_search rs ON rs.issue_id=i.id `
			order = fmt.Sprintf("(i.key = upper($%d)) DESC, ts_rank(rs.tsv, plainto_tsquery('simple', unaccent($%d))) DESC, i.updated_at DESC", cq.rankArg, cq.rankArg)
		} else {
			order = "i.updated_at DESC"
		}
	}
	order += ", i.id" // total order makes paging deterministic
	args = append(args, pg.Limit, pg.Offset)
	sql := sel + from + " WHERE " + cq.where + " ORDER BY " + order + fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))
	// the capped total runs concurrently with the page query (DESIGN 11.1: parallel fan-in over the pool)
	var n int
	var cntErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		cnt := `SELECT count(*) FROM (SELECT 1` + listFrom + ` WHERE ` + cq.where + fmt.Sprintf(` LIMIT %d) x`, TotalCap+1)
		cntErr = e.DB.QueryRow(ctx, cnt, cq.args...).Scan(&n)
	}()
	rows, err := e.DB.Query(ctx, sql, args...)
	if err != nil {
		<-done
		return nil, fmt.Errorf("search: %w", err)
	}
	res := &Result{Rows: []Row{}, Query: src}
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Key, &r.Summary, &r.Type, &r.Status, &r.StatusCat, &r.Priority, &r.Assignee, &r.Project, &r.Labels, &r.Updated, &r.Comments); err != nil {
			rows.Close()
			<-done
			return nil, err
		}
		res.Rows = append(res.Rows, r)
	}
	rows.Close()
	<-done
	if rows.Err() != nil {
		return nil, rows.Err()
	}
	if cntErr != nil {
		return nil, cntErr
	}
	if n > TotalCap {
		n, res.TotalCapped = TotalCap, true
	}
	res.Total = n
	return res, nil
}

// Typeahead matches issue keys by prefix and summaries by substring (trigram index), within the scope.
func (e *Postgres) Typeahead(ctx context.Context, sc Scope, term string, limit int) ([]Row, error) {
	if limit <= 0 || limit > 20 {
		limit = 8
	}
	if len(term) < 1 || len(term) > 200 {
		return []Row{}, nil
	}
	scope, args := "", []any{term, limit}
	if !sc.All {
		scope = ` AND i.project_id = ANY($3::uuid[])`
		args = append(args, sc.Readable)
	}
	rows, err := e.DB.Query(ctx, `SELECT i.key,i.summary,t.name,st.name,st.category,i.priority,COALESCE(asg.name,''),p.key,i.labels,i.updated_at,0`+listFrom+
		` WHERE i.deleted_at IS NULL`+scope+` AND (i.key ILIKE $1 || '%' OR EXISTS (SELECT 1 FROM issue_search s WHERE s.issue_id=i.id AND s.summary_norm LIKE '%' || lower(unaccent($1)) || '%'))
		ORDER BY (i.key ILIKE $1 || '%') DESC, i.updated_at DESC LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		rows.Scan(&r.Key, &r.Summary, &r.Type, &r.Status, &r.StatusCat, &r.Priority, &r.Assignee, &r.Project, &r.Labels, &r.Updated, &r.Comments)
		out = append(out, r)
	}
	return out, rows.Err()
}

// IsQueryError reports whether err is a user-facing TQL parse or semantic error.
func IsQueryError(err error) (*tql.Error, bool) {
	var te *tql.Error
	if errors.As(err, &te) {
		return te, true
	}
	return nil, false
}

var _ = pgx.ErrNoRows

// Keys returns the keys of the issues a query matches inside the scope, up to limit (at most 5000): what a board's
// filter and quick filters hand to the board view. The permission scope is added exactly as for Search.
func (e *Postgres) Keys(ctx context.Context, sc Scope, src string, limit int) ([]string, error) {
	q, err := tql.Parse(src)
	if err != nil {
		return nil, err
	}
	cq, err := compile(q, sc)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	rows, err := e.DB.Query(ctx, `SELECT i.key`+listFrom+` WHERE `+cq.where+fmt.Sprintf(` LIMIT %d`, limit), cq.args...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
