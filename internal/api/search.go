package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/search/tql"
)

func (s *Server) scope(r *http.Request) (search.Scope, error) {
	a := actorOf(r)
	all, rd, wr, err := s.Svc.SearchScope(r.Context(), a)
	return search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: s.Svc.Now}, err
}

func (s *Server) registerSearch(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/issues", s.auth(func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.scope(r)
		if err != nil {
			s.replyStatus(w, 200, nil, err)
			return
		}
		lim, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		q := r.URL.Query().Get("q")
		if r.URL.Query().Get("syntax") == "text" { // the plain search box: words, quotes, AND/OR/NOT; TQL is the default
			if q, err = tql.FromText(q); err != nil {
				te, _ := search.IsQueryError(err)
				s.fail(w, 400, "tql_error", te.Msg, map[string]any{"pos": te.Pos})
				return
			}
		}
		// the person's sort (sort=field:asc|desc) when the query has none of its own; the key breaks ties so pages never overlap
		if so := r.URL.Query().Get("sort"); so != "" {
			field, dir, _ := strings.Cut(so, ":")
			dir = strings.ToUpper(dir)
			if dir == "" {
				dir = "ASC"
			}
			if (dir != "ASC" && dir != "DESC") || !(domain.UISortable[field] || (strings.HasPrefix(field, "customfield_") && s.Svc.ValidColumn(r.Context(), field))) {
				s.fail(w, 400, "invalid", "sort is field:asc or field:desc on a sortable column", map[string]string{"key": "sort"})
				return
			}
			if pq, perr := tql.Parse(q); perr == nil && len(pq.Order) == 0 {
				q = strings.TrimSpace(q) + " ORDER BY " + field + " " + dir
				if field != "key" {
					q += ", key ASC"
				}
			}
		}
		var cols []string // fields=a,b: only those columns of each row (the key always)
		if f := r.URL.Query().Get("fields"); f != "" {
			for _, c := range strings.Split(f, ",") {
				if c = strings.TrimSpace(c); c == "" {
					continue
				}
				if !s.Svc.ValidColumn(r.Context(), c) && c != "project" && c != "snippet" {
					s.fail(w, 400, "invalid", "unknown column "+strconv.Quote(c), map[string]string{"key": "fields"})
					return
				}
				cols = append(cols, c)
			}
		}
		res, err := s.Search.Search(r.Context(), sc, q, search.Page{Limit: lim, Offset: off})
		if te, ok := search.IsQueryError(err); ok {
			s.fail(w, 400, "tql_error", te.Msg, map[string]any{"pos": te.Pos})
			return
		}
		if n, _ := strconv.Atoi(r.URL.Query().Get("snippet")); err == nil && n > 0 && len(res.Rows) > 0 { // the long view: the start of each description
			if n > domain.MaxSnippetChars {
				n = domain.MaxSnippetChars
			}
			keys := make([]string, len(res.Rows))
			for i, row := range res.Rows {
				keys[i] = row.Key
			}
			if sn, e := s.Svc.Snippets(r.Context(), keys, n); e == nil {
				for i := range res.Rows {
					res.Rows[i].Snippet = sn[res.Rows[i].Key]
				}
			}
		}
		if err == nil && cols != nil {
			s.json(w, 200, s.shapeRows(r, res, cols))
			return
		}
		s.replyStatus(w, 200, res, err)
	}))
	s.handle(mux, "GET /api/v1/search/typeahead", s.auth(func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.scope(r)
		if err != nil {
			s.replyStatus(w, 200, nil, err)
			return
		}
		rows, err := s.Search.Typeahead(r.Context(), sc, r.URL.Query().Get("q"), 8)
		s.replyStatus(w, 200, rows, err)
	}))
}

var _ = domain.ErrNotFound

// shapeRows answers a list with only the columns asked for (LIST-04): the key always, status with its category, and reporter,
// created and custom fields looked up for this page with the person's own field visibility.
func (s *Server) shapeRows(r *http.Request, res *search.Result, cols []string) map[string]any {
	keys := make([]string, len(res.Rows))
	for i, row := range res.Rows {
		keys[i] = row.Key
	}
	extra := map[string]map[string]any{}
	for _, c := range cols {
		if c == "reporter" || c == "created" || strings.HasPrefix(c, "customfield_") {
			extra, _ = s.Svc.ColumnValues(r.Context(), actorOf(r), keys, cols)
			break
		}
	}
	rows := make([]map[string]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		m := map[string]any{"key": row.Key}
		for _, c := range cols {
			switch c {
			case "summary":
				m[c] = row.Summary
			case "type":
				m[c] = row.Type
			case "status":
				m[c], m["status_category"] = row.Status, row.StatusCat
			case "priority":
				m[c] = row.Priority
			case "assignee":
				m[c] = row.Assignee
			case "project":
				m[c] = row.Project
			case "labels":
				m[c] = row.Labels
			case "updated":
				m[c] = row.Updated
			case "comments":
				m[c] = row.Comments
			case "snippet":
				m[c] = row.Snippet
			default:
				if v, ok := extra[row.Key][c]; ok {
					m[c] = v
				}
			}
		}
		rows = append(rows, m)
	}
	return map[string]any{"issues": rows, "total": res.Total, "total_capped": res.TotalCapped, "query": res.Query, "engine": res.Engine, "warning": res.Warning, "fields": cols}
}
