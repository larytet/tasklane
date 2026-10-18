package api

import (
	"net/http"
	"strings"
	"time"
)

// bearer returns the credential a request came with (a session cookie or a bearer token).
func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) registerSecurity(mux *http.ServeMux) {
	ok := map[string]bool{"ok": true}
	s.handle(mux, "GET /api/v1/me/sessions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListSessions(r.Context(), actorOf(r), bearer(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "DELETE /api/v1/me/sessions/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if id, good := pathID(r, "id"); good {
			s.reply(w, ok, s.Svc.RevokeSession(r.Context(), actorOf(r), id))
		} else {
			s.fail(w, 404, "not_found", "not found", nil)
		}
	}))
	s.handle(mux, "POST /api/v1/me/sessions/revoke-others", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Svc.RevokeOtherSessions(r.Context(), actorOf(r), bearer(r))
		s.reply(w, map[string]int{"ended": n}, err)
	}))
	s.handle(mux, "GET /api/v1/me/tokens", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListTokens(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/me/tokens", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name        string `json:"name"`
			ExpiresDays int    `json:"expires_in_days"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		tok, info, err := s.Svc.CreateOwnToken(r.Context(), actorOf(r), in.Name, time.Duration(in.ExpiresDays)*24*time.Hour)
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		s.json(w, 201, map[string]any{"token": tok, "info": info}) // the secret is shown this once
	}))
	s.handle(mux, "DELETE /api/v1/me/tokens/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.RevokeOwnToken(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/me/password", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Current string `json:"current"`
			New     string `json:"new"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.ChangePassword(r.Context(), actorOf(r), in.Current, in.New, bearer(r)))
		}
	}))
	s.handle(mux, "GET /api/v1/me/2fa", s.auth(func(w http.ResponseWriter, r *http.Request) {
		on, left := s.Svc.MFAStatus(r.Context(), actorOf(r))
		s.json(w, 200, map[string]any{"enabled": on, "recovery_codes_left": left})
	}))
	s.handle(mux, "POST /api/v1/me/2fa/setup", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.SetupMFA(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/me/2fa/enable", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		codes, err := s.Svc.EnableMFA(r.Context(), actorOf(r), in.Code, bearer(r))
		s.reply(w, map[string]any{"recovery_codes": codes}, err) // shown once
	}))
	s.handle(mux, "DELETE /api/v1/me/2fa", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Password string `json:"password"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.DisableMFA(r.Context(), actorOf(r), in.Password))
		}
	}))
	s.handle(mux, "POST /api/v1/users/{id}/revoke-access", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tokens bool `json:"tokens"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		n, err := s.Svc.RevokeUserAccess(r.Context(), actorOf(r), r.PathValue("id"), in.Tokens)
		s.reply(w, map[string]int{"ended": n}, err)
	}))
	s.handle(mux, "POST /api/v1/users/{id}/2fa/reset", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.ResetMFA(r.Context(), actorOf(r), r.PathValue("id")))
	}))
}
