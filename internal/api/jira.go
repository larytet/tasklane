package api

import (
	"io"
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerJira(mux *http.ServeMux) {
	// A webhook is only a hint that wakes the sync; the signature proves it came from the connected site.
	s.handle(mux, "POST /api/v1/jira/webhooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, domain.MaxWebhookBytes+1))
		if err != nil {
			s.fail(w, 400, "bad_request", "could not read the request", nil)
			return
		}
		err = s.Svc.ReceiveJiraWebhook(r.Context(), r.PathValue("id"), r.Header, body)
		var ptl *domain.PayloadTooLarge
		if asErr(err, &ptl) {
			s.fail(w, 413, "too_large", err.Error(), nil)
			return
		}
		s.replyStatus(w, 202, map[string]bool{"accepted": true}, err)
	})
	s.handle(mux, "GET /api/v1/jira/connections", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListJiraConnections(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/jira/connections", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.JiraConnIn
		if !s.decode(w, r, &in) {
			return
		}
		c, secret, err := s.Svc.CreateJiraConnection(r.Context(), actorOf(r), in)
		s.replyStatus(w, 201, map[string]any{"connection": c, "webhook_secret": secret, "webhook_url": "/api/v1/jira/webhooks/" + c.ID}, err)
	}))
	s.handle(mux, "POST /api/v1/jira/connections/{id}/projects", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			JiraKey string `json:"jira_key"`
			Key     string `json:"key"`
			Name    string `json:"name"`
			JQL     string `json:"jql"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		p, err := s.Svc.MirrorJiraProject(r.Context(), actorOf(r), r.PathValue("id"), in.JiraKey, in.Key, in.Name, in.JQL)
		s.replyStatus(w, 201, p, err)
	}))
	s.handle(mux, "POST /api/v1/jira/projects/{key}/sync", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Svc.SyncJiraProject(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, map[string]int{"issues": n}, err)
	}))
	s.handle(mux, "POST /api/v1/jira/projects/{key}/reconcile", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ReconcileJira(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/jira/projects/{key}/cutover", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.CutOverJira(r.Context(), actorOf(r), r.PathValue("key")))
	}))
	s.handle(mux, "GET /api/v1/jira/status", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.JiraStatus(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
}
