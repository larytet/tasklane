package search

import (
	"context"
	"fmt"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Aggregation (for dashboard gadgets): how many issues a query matches, grouped by one or two properties. It reads PostgreSQL, like
// the other exact consumers, and applies the same scope as a search, so a count can never include an issue the person could not
// see in a list.

// Dimensions are the properties issues can be grouped by, with the SQL that yields each one's label.
var Dimensions = map[string]string{
	"status":         `st.name`,
	"statuscategory": `st.category`,
	"priority":       `i.priority`,
	"type":           `t.name`,
	"project":        `p.key`,
	"assignee":       `COALESCE(asg.name,'Unassigned')`,
	"reporter":       `rp.name`,
	"label":          `COALESCE(lb.label,'(no label)')`,
}

type AggRow struct {
	A     string `json:"a"`
	B     string `json:"b,omitempty"`
	Count int    `json:"count"`
}

type AggResult struct {
	Rows  []AggRow `json:"rows"`
	Total int      `json:"total"` // issues matched (an issue with several labels counts once here, once per label in the rows)
}

// Aggregate groups the issues a query matches by one dimension, or two (b may be ""). At most limit groups come back, largest first.
func (e *Postgres) Aggregate(ctx context.Context, sc Scope, src, a, b string, limit int) (*AggResult, error) {
	ea, ok := Dimensions[a]
	if !ok {
		return nil, fmt.Errorf("cannot group by %q", a)
	}
	eb := ""
	if b != "" {
		if eb, ok = Dimensions[b]; !ok {
			return nil, fmt.Errorf("cannot group by %q", b)
		}
	}
	q, err := tql.Parse(src)
	if err != nil {
		return nil, err
	}
	cq, err := compile(q, e.withFields(ctx, sc))
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	from := listFrom
	if a == "label" || b == "label" {
		from += `LEFT JOIN LATERAL unnest(i.labels) lb(label) ON true `
	}
	sel, grp := ea+` AS a`, `a`
	if eb != "" {
		sel, grp = ea+` AS a, `+eb+` AS b`, `a, b`
	} else {
		sel += `, '' AS b`
		grp = `a`
	}
	sql := fmt.Sprintf(`SELECT %s, count(*) FROM (SELECT i.id, %s `+from+` WHERE `+cq.where+`) x GROUP BY %s ORDER BY count(*) DESC, a, b LIMIT %d`, "a, b", sel, "a, b", limit)
	_ = grp
	rows, err := e.DB.Query(ctx, sql, cq.args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate: %w", err)
	}
	defer rows.Close()
	res := &AggResult{Rows: []AggRow{}}
	for rows.Next() {
		var r AggRow
		if err := rows.Scan(&r.A, &r.B, &r.Count); err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var n int
	if err := e.DB.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1`+listFrom+` WHERE `+cq.where+` LIMIT `+fmt.Sprint(TotalCap+1)+`) x`, cq.args...).Scan(&n); err != nil {
		return nil, err
	}
	if n > TotalCap {
		n = TotalCap
	}
	res.Total = n
	return res, nil
}
