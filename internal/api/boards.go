package api

import (
	"net/http"
	"strings"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

func (s *Server) registerBoards(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/projects/{key}/boards", s.auth(func(w http.ResponseWriter, r *http.Request) {
		bs, err := s.Svc.ListBoards(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, bs, err)
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/boards", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Kind string }
		if !s.decode(w, r, &in) {
			return
		}
		b, err := s.Svc.CreateBoard(r.Context(), actorOf(r), r.PathValue("key"), in.Name, in.Kind)
		s.replyStatus(w, 201, b, err)
	}))
	// the board with its cards; ?quick=Name,Name turns quick filters on. The board filter and chosen quick filters are
	// TQL, evaluated inside the caller's permission scope, and only the matching keys reach the board view.
	s.handle(mux, "GET /api/v1/boards/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		a := actorOf(r)
		b, _, _, err := s.Svc.BoardAccess(r.Context(), a, r.PathValue("id"))
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		var restrict []string
		clauses := []string{}
		if b.Filter != "" {
			clauses = append(clauses, "("+b.Filter+")")
		}
		for _, name := range strings.Split(r.URL.Query().Get("quick"), ",") {
			for _, q := range b.QuickFilters {
				if name != "" && strings.EqualFold(q.Name, strings.TrimSpace(name)) {
					clauses = append(clauses, "("+q.Query+")")
				}
			}
		}
		if len(clauses) > 0 {
			sc, err := s.scope(r)
			if err != nil {
				s.reply(w, nil, err)
				return
			}
			restrict, err = s.Search.Keys(r.Context(), sc, "project = "+b.Project+" AND "+strings.Join(clauses, " AND "), 5000)
			if err != nil {
				if te, ok := search.IsQueryError(err); ok {
					s.fail(w, 400, "tql_error", "the board filter does not parse: "+te.Msg, map[string]any{"pos": te.Pos})
					return
				}
				s.reply(w, nil, err)
				return
			}
		}
		v, err := s.Svc.GetBoard(r.Context(), a, b.ID, restrict)
		s.reply(w, v, err)
	}))
	s.handle(mux, "PATCH /api/v1/boards/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.BoardPatch
		if !s.decode(w, r, &in) {
			return
		}
		b, err := s.Svc.UpdateBoard(r.Context(), actorOf(r), r.PathValue("id"), in)
		s.reply(w, b, err)
	}))
	s.handle(mux, "PUT /api/v1/boards/{id}/columns", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Columns []domain.ColumnIn `json:"columns"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetColumns(r.Context(), actorOf(r), r.PathValue("id"), in.Columns))
	}))
	s.handle(mux, "POST /api/v1/boards/{id}/move", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key string `json:"key"`
			domain.MoveIn
		}
		if !s.decode(w, r, &in) {
			return
		}
		res, err := s.Svc.MoveCard(r.Context(), actorOf(r), r.PathValue("id"), in.Key, in.MoveIn)
		s.reply(w, res, err)
	}))
}
