package api

import (
	"net/http"
	"strconv"
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
