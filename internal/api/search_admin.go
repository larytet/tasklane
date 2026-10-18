package api

import (
	"errors"
	"log"
	"net/http"

	"github.com/larytet/tasklane/internal/search"
)

func (s *Server) registerSearchAdmin(mux *http.ServeMux) {
	admin := func(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return s.auth(func(w http.ResponseWriter, r *http.Request) {
			if !actorOf(r).Admin {
				s.fail(w, 403, "forbidden", "administrators only", nil)
				return
			}
			h(w, r)
		})
	}
	needOS := func(w http.ResponseWriter) bool {
		if s.SearchAdmin == nil || s.SearchAdmin.Index == nil {
			s.fail(w, 400, "invalid", "the search engine is PostgreSQL: there is no separate index to maintain", nil)
			return false
		}
		return true
	}
	busy := func(w http.ResponseWriter, err error) bool {
		if errors.Is(err, search.ErrBusy) {
			s.fail(w, 409, "busy", err.Error(), nil)
			return true
		}
		return false
	}
	s.handle(mux, "GET /api/v1/admin/search", admin(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, s.SearchAdmin.Status(r.Context()), nil)
	}))
	s.handle(mux, "POST /api/v1/admin/search/rebuild", admin(func(w http.ResponseWriter, r *http.Request) {
		if !needOS(w) {
			return
		}
		if err := s.SearchAdmin.StartRebuild(log.Printf); err != nil {
			if !busy(w, err) {
				s.reply(w, nil, err)
			}
			return
		}
		s.replyStatus(w, 202, map[string]bool{"started": true}, nil)
	}))
	s.handle(mux, "POST /api/v1/admin/search/reconcile", admin(func(w http.ResponseWriter, r *http.Request) {
		if !needOS(w) {
			return
		}
		var in struct{ Project string }
		if !s.decode(w, r, &in) {
			return
		}
		rep, err := s.SearchAdmin.Reconcile(r.Context(), in.Project)
		if !busy(w, err) {
			s.reply(w, rep, err)
		}
	}))
	s.handle(mux, "POST /api/v1/admin/search/reindex", admin(func(w http.ResponseWriter, r *http.Request) {
		if !needOS(w) {
			return
		}
		var in struct{ Project string }
		if !s.decode(w, r, &in) {
			return
		}
		n, err := s.SearchAdmin.ReindexProject(r.Context(), in.Project)
		if !busy(w, err) {
			s.reply(w, map[string]int{"documents": n}, err)
		}
	}))
	s.handle(mux, "POST /api/v1/admin/search/rollback", admin(func(w http.ResponseWriter, r *http.Request) {
		if !needOS(w) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.SearchAdmin.Rollback(r.Context()))
	}))
}
