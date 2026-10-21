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
	// The sync's credential is never stored: after a start an administrator supplies it (SPEC 18.1.1, WT-03).
	s.handle(mux, "POST /api/v1/jira/connections/{id}/unlock", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token string `json:"token"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.UnlockJiraConnection(r.Context(), actorOf(r), r.PathValue("id"), in.Token))
	}))
	s.handle(mux, "PUT /api/v1/jira/projects/{key}/mode", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode string `json:"mode"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]string{"mode": in.Mode}, s.Svc.SetJiraProjectMode(r.Context(), actorOf(r), r.PathValue("key"), in.Mode))
	}))
	// A person's own token, kept in memory for their writes to Jira (WT-04). The answer never repeats it.
	s.handle(mux, "POST /api/v1/jira/session-token", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Connection string `json:"connection"`
			Email      string `json:"email"`
			Token      string `json:"token"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetJiraSessionToken(r.Context(), actorOf(r), in.Connection, in.Email, in.Token))
	}))
	s.handle(mux, "DELETE /api/v1/jira/session-token", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.Svc.ClearJiraSessionToken(actorOf(r), r.URL.Query().Get("connection"))
		s.json(w, 200, map[string]bool{"ok": true})
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
