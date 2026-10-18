package api

import (
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerVersions(mux *http.ServeMux) {
	ok := map[string]bool{"ok": true}
	s.handle(mux, "GET /api/v1/projects/{key}/versions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListVersions(r.Context(), actorOf(r), r.PathValue("key"), r.URL.Query().Get("archived") == "1")
		if err != nil {
			s.reply(w, v, err)
			return
		}
		s.json(w, 200, map[string]any{"versions": v, "can_manage": s.Svc.CanManageVersions(r.Context(), actorOf(r), r.PathValue("key"))})
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/versions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.VersionIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateVersion(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/versions/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetVersion(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/versions/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.VersionIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.UpdateVersion(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/versions/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeleteVersion(r.Context(), actorOf(r), r.PathValue("id"), r.URL.Query().Get("move_to")))
	}))
	s.handle(mux, "POST /api/v1/versions/{id}/release", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.ReleaseIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.ReleaseVersion(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "POST /api/v1/versions/{id}/unrelease", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.UnreleaseVersion(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/versions/{id}/notes", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ReleaseNotes(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/issues/{key}/versions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.SetVersionsIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.SetIssueVersions(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.reply(w, v, err)
		}
	}))
}
