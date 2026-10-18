package api

import (
	"net/http"
	"strconv"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerAutomation(mux *http.ServeMux) {
	id := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		n, ok := pathID(r, "id")
		if !ok {
			s.fail(w, 404, "not_found", "not found", nil)
		}
		return n, ok
	}
	s.handle(mux, "GET /api/v1/automation/rules", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListAutoRules(r.Context(), actorOf(r), r.URL.Query().Get("project"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/automation/rules", s.auth(func(w http.ResponseWriter, r *http.Request) {
		in := domain.AutoRule{Enabled: true}
		if s.decode(w, r, &in) {
			v, err := s.Svc.SaveAutoRule(r.Context(), actorOf(r), 0, in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/automation/rules/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if n, ok := id(w, r); ok {
			v, err := s.Svc.GetAutoRule(r.Context(), actorOf(r), n)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/automation/rules/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, ok := id(w, r)
		in := domain.AutoRule{Enabled: true}
		if ok && s.decode(w, r, &in) {
			v, err := s.Svc.SaveAutoRule(r.Context(), actorOf(r), n, in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/automation/rules/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if n, ok := id(w, r); ok {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.DeleteAutoRule(r.Context(), actorOf(r), n))
		}
	}))
	s.handle(mux, "POST /api/v1/automation/rules/{id}/dry-run", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, ok := id(w, r)
		var in struct {
			Issue string `json:"issue"`
		}
		if ok && s.decode(w, r, &in) {
			v, err := s.Svc.DryRunAutoRule(r.Context(), actorOf(r), n, in.Issue)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/automation/rules/{id}/runs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if n, ok := id(w, r); ok {
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			v, err := s.Svc.AutoRuns(r.Context(), actorOf(r), n, limit)
			s.reply(w, v, err)
		}
	}))
}
