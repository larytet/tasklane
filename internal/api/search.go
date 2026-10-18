package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
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
		res, err := s.Search.Search(r.Context(), sc, r.URL.Query().Get("q"), search.Page{Limit: lim, Offset: off})
		if te, ok := search.IsQueryError(err); ok {
			s.fail(w, 400, "tql_error", te.Msg, map[string]any{"pos": te.Pos})
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
