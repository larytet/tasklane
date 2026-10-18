package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/search/tql"
)

// ErrUnsupported means a query uses something the index cannot answer faithfully (history clauses, some fields); the
// router sends it to PostgreSQL. ErrTermLimit means the caller can read more projects than the index filter allows.
var (
	ErrUnsupported = errors.New("the index cannot answer this query")
	ErrTermLimit   = errors.New("the caller can read more projects than the index filter allows")
)

// OpenSearch answers search-box and list queries from the index. It returns only issue ids from the cluster; every id is
// checked again against PostgreSQL (permissions, trash, moves) and every displayed value is read from PostgreSQL.
type OpenSearch struct {
	C         *OSClient
	PG        *Postgres
	Alias     string
	TermLimit int // most projects sent as an index filter (default 1024)
	Now       func() time.Time
}

func (o *OpenSearch) alias() string {
	if o.Alias == "" {
		return "tasklane-issues"
	}
	return o.Alias
}

func (o *OpenSearch) limit() int {
	if o.TermLimit == 0 {
		return 1024
	}
	return o.TermLimit
}

type osTr struct {
	sc     Scope
	pg     *Postgres
	ctx    context.Context
	dc     *compiler
	ranked bool
}

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, fmt.Sprintf(format, a...))
}

func term(f string, v any) map[string]any     { return map[string]any{"term": map[string]any{f: v}} }
func terms(f string, v any) map[string]any    { return map[string]any{"terms": map[string]any{f: v}} }
func boolq(k string, v ...any) map[string]any { return map[string]any{"bool": map[string]any{k: v}} }
func not(q map[string]any) map[string]any     { return boolq("must_not", q) }
func exists(f string) map[string]any          { return map[string]any{"exists": map[string]any{"field": f}} }

func (t *osTr) node(n tql.Node) (map[string]any, error) {
	switch x := n.(type) {
	case tql.Bool:
		l, err := t.node(x.L)
		if err != nil {
			return nil, err
		}
		r, err := t.node(x.R)
		if err != nil {
			return nil, err
		}
		if x.Or {
			b := boolq("should", l, r)
			b["bool"].(map[string]any)["minimum_should_match"] = 1
			return b, nil
		}
		return boolq("must", l, r), nil
	case tql.Not:
		s, err := t.node(x.X)
		if err != nil {
			return nil, err
		}
		return not(s), nil
	case tql.Cmp:
		return t.cmp(x)
	}
	return nil, unsupported("unknown node")
}

func neg(isNeg bool, q map[string]any) map[string]any {
	if isNeg {
		return not(q)
	}
	return q
}

func (t *osTr) cmp(x tql.Cmp) (map[string]any, error) {
	isNeg := false
	switch x.Op {
	case "!=", "notin", "isnot", "!~":
		isNeg = true
	case "was", "wasnot", "changed":
		return nil, unsupported("history clauses read the history store")
	}
	switch x.Field {
	case "text", "summary", "description", "comment":
		return t.text(x)
	case "project", "key", "issuekey", "type", "issuetype", "status", "priority", "parent":
		field := map[string]string{"project": "project", "key": "key", "issuekey": "key", "type": "type", "issuetype": "type", "status": "status", "priority": "priority", "parent": "parent"}[x.Field]
		upper := x.Field == "project" || x.Field == "key" || x.Field == "issuekey" || x.Field == "parent"
		switch x.Op {
		case "=", "!=", "in", "notin":
			var vs []string
			for _, v := range x.Vals {
				if v.Empty {
					if x.Field == "parent" {
						return neg(isNeg, term("parent", "")), nil
					}
					return nil, unsupported("EMPTY on %s", x.Field)
				}
				if v.Fn != "" {
					return nil, unsupported("function %s()", v.Fn)
				}
				s := strings.TrimSpace(v.S)
				if upper {
					s = strings.ToUpper(s)
				} else {
					s = strings.ToLower(s)
				}
				vs = append(vs, s)
			}
			q := terms(field, vs)
			if field == "key" { // an old key finds the issue forever
				q = boolq("should", terms("key", vs), terms("aliases", vs))
				q["bool"].(map[string]any)["minimum_should_match"] = 1
			}
			return neg(isNeg, q), nil
		case "is", "isnot":
			if x.Field == "parent" {
				if x.Op == "is" {
					return term("parent", ""), nil
				}
				return not(term("parent", "")), nil
			}
		}
		return nil, unsupported("operator on %s", x.Field)
	case "statuscategory":
		var vs []string
		for _, v := range x.Vals {
			cat, ok := statusCats[strings.ToLower(v.S)]
			if !ok {
				return nil, errAt(v.Pos, "unknown status category %q (use To Do, In Progress or Done)", v.S)
			}
			vs = append(vs, cat)
		}
		if x.Op != "=" && x.Op != "!=" && x.Op != "in" && x.Op != "notin" {
			return nil, unsupported("operator on statusCategory")
		}
		return neg(isNeg, terms("status_category", vs)), nil
	case "assignee", "reporter":
		field := x.Field
		switch x.Op {
		case "is", "isnot":
			if x.Field == "reporter" {
				return nil, unsupported("reporter is never empty")
			}
			if x.Op == "is" {
				return term("assignee", ""), nil
			}
			return not(term("assignee", "")), nil
		case "=", "!=", "in", "notin":
			var should []any
			for _, v := range x.Vals {
				switch {
				case v.Empty:
					if field == "reporter" {
						return nil, unsupported("reporter cannot be EMPTY")
					}
					should = append(should, term("assignee", ""))
				case v.Fn == "currentuser":
					should = append(should, term(field, t.sc.UserID))
				case v.Fn != "":
					return nil, unsupported("function %s()", v.Fn)
				default:
					ids, err := t.userIDs(strings.ToLower(strings.TrimSpace(v.S)))
					if err != nil {
						return nil, err
					}
					if len(ids) == 0 {
						ids = []string{"no-such-user"}
					}
					should = append(should, terms(field, ids))
				}
			}
			q := boolq("should", should...)
			q["bool"].(map[string]any)["minimum_should_match"] = 1
			return neg(isNeg, q), nil
		}
		return nil, unsupported("operator on %s", x.Field)
	case "label", "labels":
		switch x.Op {
		case "=", "!=", "in", "notin":
			var vs []string
			for _, v := range x.Vals {
				if v.Empty {
					return neg(isNeg, term("labels_count", 0)), nil
				}
				vs = append(vs, v.S)
			}
			return neg(isNeg, terms("labels", vs)), nil
		case "is":
			return term("labels_count", 0), nil
		case "isnot":
			return not(term("labels_count", 0)), nil
		}
		return nil, unsupported("operator on labels")
	case "created", "updated", "resolved", "resolutiondate":
		field := map[string]string{"created": "created", "updated": "updated", "resolved": "resolved", "resolutiondate": "resolved"}[x.Field]
		switch x.Op {
		case "is":
			return not(exists(field)), nil
		case "isnot":
			return exists(field), nil
		case "=", "!=", ">", "<", ">=", "<=":
			tm, day, err := t.cmp2().date(x.Vals[0])
			if err != nil {
				return nil, err
			}
			rng := func(m map[string]any) map[string]any { return map[string]any{"range": map[string]any{field: m}} }
			f := func(tt time.Time) string { return tt.UTC().Format(time.RFC3339Nano) }
			switch {
			case day && (x.Op == "=" || x.Op == "!="):
				return neg(x.Op == "!=", rng(map[string]any{"gte": f(tm), "lt": f(tm.AddDate(0, 0, 1))})), nil
			case x.Op == "=":
				return term(field, f(tm)), nil
			case x.Op == "!=":
				return not(term(field, f(tm))), nil
			default:
				return rng(map[string]any{map[string]string{">": "gt", "<": "lt", ">=": "gte", "<=": "lte"}[x.Op]: f(tm)}), nil
			}
		}
		return nil, unsupported("operator on %s", x.Field)
	}
	return nil, unsupported("field %s", x.Field) // inputrequested, unknown fields: PostgreSQL gives the proper answer or error
}

func (t *osTr) cmp2() *compiler {
	if t.dc == nil {
		t.dc = &compiler{sc: t.sc, nowFn: t.sc.Now}
		if t.dc.nowFn == nil {
			t.dc.nowFn = time.Now
		}
	}
	return t.dc
}

func (t *osTr) userIDs(name string) ([]string, error) {
	rows, err := t.pg.DB.Query(t.ctx, `SELECT id::text FROM app_user WHERE lower(email)=$1 OR lower(name)=$1`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (t *osTr) text(x tql.Cmp) (map[string]any, error) {
	if x.Op != "~" && x.Op != "!~" {
		return nil, errAt(x.Pos, "%s supports only ~ and !~", x.Field)
	}
	v := x.Vals[0]
	if v.Fn != "" || v.Empty {
		return nil, errAt(v.Pos, "%s ~ needs a word or phrase", x.Field)
	}
	termS := strings.TrimSpace(v.S)
	if termS == "" {
		return nil, errAt(v.Pos, "empty search text")
	}
	match := func(f string) map[string]any {
		return map[string]any{"match": map[string]any{f: map[string]any{"query": termS, "operator": "and"}}}
	}
	var should []any
	switch x.Field {
	case "summary": // substring and prefix search on summaries, at least three characters (SPEC 10.1)
		if len(termS) < 3 {
			return nil, unsupported("summary search needs three characters")
		}
		should = append(should, map[string]any{"wildcard": map[string]any{"summary_norm": "*" + strings.ToLower(escWild(termS)) + "*"}})
	case "description":
		should = append(should, match("description"))
	case "comment":
		should = append(should, match("comments"))
		if in := t.internalComments(termS); in != nil {
			should = append(should, in)
		}
	default:
		if keyLike.MatchString(termS) {
			should = append(should, term("key", strings.ToUpper(termS)))
		}
		should = append(should, match("summary"), match("description"), match("comments"))
		if in := t.internalComments(termS); in != nil {
			should = append(should, in)
		}
		t.ranked = t.ranked || x.Op == "~"
	}
	q := boolq("should", should...)
	q["bool"].(map[string]any)["minimum_should_match"] = 1
	return neg(x.Op == "!~", q), nil
}

func (t *osTr) internalComments(termS string) map[string]any {
	m := map[string]any{"match": map[string]any{"comments_internal": map[string]any{"query": termS, "operator": "and"}}}
	if t.sc.All {
		return m
	}
	if len(t.sc.Writable) == 0 {
		return nil
	}
	return map[string]any{"bool": map[string]any{"must": []any{m}, "filter": []any{terms("project_id", t.sc.Writable)}}}
}

func escWild(s string) string {
	return strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`).Replace(s)
}

// translate builds the index query for a parsed TQL query under the caller's scope, with the permission pre-filter that is
// built from current access (never stored in the index), and the sort.
func (o *OpenSearch) translate(ctx context.Context, sc Scope, q *tql.Query) (query map[string]any, sort []any, ranked bool, err error) {
	t := &osTr{sc: sc, pg: o.PG, ctx: ctx}
	var filter []any
	if !sc.All {
		if len(sc.Readable) == 0 {
			return map[string]any{"bool": map[string]any{"must_not": []any{map[string]any{"match_all": map[string]any{}}}}}, nil, false, nil
		}
		if len(sc.Readable) > o.limit() {
			return nil, nil, false, ErrTermLimit
		}
		filter = append(filter, terms("project_id", sc.Readable))
	}
	if sc.OnlyKeys != nil {
		filter = append(filter, terms("key", sc.OnlyKeys))
	}
	must := []any{}
	if q.Where != nil {
		w, err := t.node(q.Where)
		if err != nil {
			return nil, nil, false, err
		}
		must = append(must, w)
	}
	if len(must) == 0 {
		must = append(must, map[string]any{"match_all": map[string]any{}})
	}
	for _, od := range q.Order {
		field := map[string]string{"key": "number", "issuekey": "number", "created": "created", "updated": "updated", "resolved": "resolved", "priority": "priority_rank", "status": "status", "type": "type", "summary": "summary_norm", "assignee": "assignee_name"}[od.Field]
		if field == "" {
			return nil, nil, false, unsupported("ORDER BY %s", od.Field)
		}
		dir := "asc"
		if od.Desc {
			dir = "desc"
		}
		sort = append(sort, map[string]any{field: map[string]any{"order": dir, "missing": "_last"}})
	}
	return map[string]any{"bool": map[string]any{"must": must, "filter": filter}}, sort, t.ranked, nil
}

// Search runs a TQL query on the index and returns authorized, PostgreSQL-read rows.
func (o *OpenSearch) Search(ctx context.Context, sc Scope, src string, pg Page) (*Result, error) {
	q, err := tql.Parse(src)
	if err != nil {
		return nil, err
	}
	query, sort, ranked, err := o.translate(ctx, sc, q)
	if err != nil {
		return nil, err
	}
	if pg.Limit <= 0 || pg.Limit > 200 {
		pg.Limit = 50
	}
	if pg.Offset < 0 {
		pg.Offset = 0
	}
	if pg.Offset+pg.Limit > TotalCap { // the index's own window: deeper pages are PostgreSQL's job
		return nil, unsupported("deep paging")
	}
	if len(sort) == 0 {
		if ranked {
			sort = append(sort, map[string]any{"_score": map[string]any{"order": "desc"}})
		}
		sort = append(sort, map[string]any{"updated": map[string]any{"order": "desc"}})
	}
	sort = append(sort, map[string]any{"id": map[string]any{"order": "asc"}}) // a total order makes paging deterministic
	sr, err := o.C.Search(ctx, o.alias(), map[string]any{"query": query, "sort": sort, "from": pg.Offset, "size": pg.Limit, "_source": false, "track_total_hits": TotalCap + 1})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(sr.Hits))
	for _, h := range sr.Hits {
		ids = append(ids, h.ID)
	}
	rows, err := o.PG.Hydrate(ctx, sc, ids) // the authorization step: PostgreSQL decides what may be shown
	if err != nil {
		return nil, err
	}
	res := &Result{Rows: rows, Query: src, Engine: "opensearch", Total: sr.Total}
	if dropped := len(ids) - len(rows); dropped > 0 {
		res.Total -= dropped
		if res.Total < len(rows) {
			res.Total = len(rows)
		}
	}
	if res.Total > TotalCap {
		res.Total, res.TotalCapped = TotalCap, true
	}
	return res, nil
}

// Typeahead matches keys by prefix and summaries by substring on the index, within the caller's projects.
func (o *OpenSearch) Typeahead(ctx context.Context, sc Scope, termS string, limit int) ([]Row, error) {
	if limit <= 0 || limit > 20 {
		limit = 8
	}
	if len(termS) < 1 || len(termS) > 200 {
		return []Row{}, nil
	}
	var filter []any
	if !sc.All {
		if len(sc.Readable) == 0 {
			return []Row{}, nil
		}
		if len(sc.Readable) > o.limit() {
			return nil, ErrTermLimit
		}
		filter = append(filter, terms("project_id", sc.Readable))
	}
	should := []any{map[string]any{"prefix": map[string]any{"key": strings.ToUpper(termS)}}}
	if len(termS) >= 3 {
		should = append(should, map[string]any{"wildcard": map[string]any{"summary_norm": "*" + strings.ToLower(escWild(termS)) + "*"}})
	}
	body := map[string]any{"query": map[string]any{"bool": map[string]any{"should": should, "minimum_should_match": 1, "filter": filter}},
		"sort": []any{map[string]any{"updated": map[string]any{"order": "desc"}}, map[string]any{"id": map[string]any{"order": "asc"}}}, "size": limit * 2, "_source": false}
	sr, err := o.C.Search(ctx, o.alias(), body)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(sr.Hits))
	for _, h := range sr.Hits {
		ids = append(ids, h.ID)
	}
	rows, err := o.PG.Hydrate(ctx, sc, ids)
	if err != nil {
		return nil, err
	}
	// exact key prefix first, as on PostgreSQL
	up := strings.ToUpper(termS)
	var first, rest []Row
	for _, r := range rows {
		if strings.HasPrefix(r.Key, up) {
			first = append(first, r)
		} else {
			rest = append(rest, r)
		}
	}
	out := append(first, rest...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (o *OpenSearch) Keys(ctx context.Context, sc Scope, src string, limit int) ([]string, error) {
	return o.PG.Keys(ctx, sc, src, limit) // exact consumers never read the index
}
