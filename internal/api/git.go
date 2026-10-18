package api

import (
	"io"
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerGit(mux *http.ServeMux) {
	// The webhook is not behind a session: it proves itself with the provider's signature or token. Everything about a
	// refusal is the same for every caller (403), so nothing leaks about which part was wrong.
	s.handle(mux, "POST /api/v1/git/webhooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, domain.MaxWebhookBytes+1))
		if err != nil {
			s.fail(w, 400, "bad_request", "could not read the request", nil)
			return
		}
		res, err := s.Svc.ReceiveGitWebhook(r.Context(), r.PathValue("id"), r.RemoteAddr, r.Header, body)
		if err != nil {
			var ptl *domain.PayloadTooLarge
			if asErr(err, &ptl) {
				s.fail(w, 413, "too_large", err.Error(), nil)
				return
			}
		}
		s.replyStatus(w, 200, res, err)
	})
	s.handle(mux, "GET /api/v1/git/connections", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListGitConnections(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/git/connections", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.GitConnIn
		if !s.decode(w, r, &in) {
			return
		}
		c, secret, err := s.Svc.CreateGitConnection(r.Context(), actorOf(r), in)
		// the webhook secret is shown once, here, and never again
		s.replyStatus(w, 201, map[string]any{"connection": c, "webhook_secret": secret, "webhook_url": "/api/v1/git/webhooks/" + c.ID}, err)
	}))
	s.handle(mux, "DELETE /api/v1/git/connections/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.DeleteGitConnection(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/git/connections/{id}/repos", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			FullName string   `json:"full_name"`
			Projects []string `json:"projects"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		repo, err := s.Svc.MapGitRepo(r.Context(), actorOf(r), r.PathValue("id"), in.FullName, in.Projects)
		s.replyStatus(w, 201, repo, err)
	}))
	s.handle(mux, "GET /api/v1/git/repos", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListGitRepos(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/git/repos/{id}/sync", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Svc.SyncGitRepo(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, map[string]int{"events": n}, err)
	}))
	s.handle(mux, "GET /api/v1/issues/{key}/development", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.Development(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/issues/{key}/development/links", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.replyStatus(w, 201, map[string]bool{"ok": true}, s.Svc.AddGitLink(r.Context(), actorOf(r), r.PathValue("key"), in.URL))
	}))
	s.handle(mux, "DELETE /api/v1/issues/{key}/development/links/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.RemoveGitLink(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("id")))
	}))
}
