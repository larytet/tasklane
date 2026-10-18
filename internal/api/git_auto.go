package api

import (
	"net/http"
	"strconv"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerGitAuto(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/projects/{key}/git/rules", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListGitRules(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/git/rules", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.GitRule
		if s.decode(w, r, &in) {
			in.ID = 0
			v, err := s.Svc.SaveGitRule(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/git/rules/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.GitRule
		if s.decode(w, r, &in) {
			in.ID, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
			if in.ID == 0 {
				s.reply(w, nil, domain.ErrNotFound)
				return
			}
			v, err := s.Svc.SaveGitRule(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/projects/{key}/git/rules/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		s.reply(w, map[string]bool{"ok": true}, s.Svc.DeleteGitRule(r.Context(), actorOf(r), r.PathValue("key"), id))
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/git/rule-runs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		v, err := s.Svc.GitRuleRuns(r.Context(), actorOf(r), r.PathValue("key"), n)
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/git/smart-commits", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode string `json:"mode"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetSmartCommits(r.Context(), actorOf(r), r.PathValue("key"), in.Mode))
		}
	}))
	s.handle(mux, "GET /api/v1/git/identities", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListGitIdentities(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/git/identities", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Provider string `json:"provider"`
			Login    string `json:"login"`
			User     string `json:"user"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.LinkGitIdentity(r.Context(), actorOf(r), in.Provider, in.Login, in.User)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/git/identities/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		s.reply(w, map[string]bool{"ok": true}, s.Svc.UnlinkGitIdentity(r.Context(), actorOf(r), id))
	}))
	s.handle(mux, "GET /api/v1/git/visibility", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GitVisibility(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/git/visibility", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode string `json:"mode"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetGitVisibility(r.Context(), actorOf(r), in.Mode))
		}
	}))
	s.handle(mux, "PUT /api/v1/git/repos/{id}/privacy", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Private *bool `json:"private"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetGitRepoPrivacy(r.Context(), actorOf(r), r.PathValue("id"), in.Private))
		}
	}))
}
