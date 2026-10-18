package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/larytet/tasklane/internal/domain"
)

var scopeText = map[string]string{
	"read":        "Read issues, comments and attachments you can see",
	"comment":     "Add comments",
	"edit-fields": "Change priority, labels, assignee and text",
	"create":      "Create issues and subtasks",
	"transition":  "Move issues through their workflow",
	"claim":       "Claim issues and work on them",
}

func baseURLOf(s *Server, r *http.Request) string {
	if s.Svc.BaseURL != "" {
		return strings.TrimRight(s.Svc.BaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) oauthJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	s.json(w, code, v)
}

func (s *Server) oauthFail(w http.ResponseWriter, err error) {
	var oe *domain.OAuthError
	if errors.As(err, &oe) {
		code := 400
		if oe.Code == "invalid_client" {
			code = 401
		}
		s.oauthJSON(w, code, map[string]string{"error": oe.Code, "error_description": oe.Description})
		return
	}
	s.oauthJSON(w, 500, map[string]string{"error": "server_error", "error_description": "something went wrong"})
}

func authReq(q url.Values) domain.AuthorizeReq {
	return domain.AuthorizeReq{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), Scope: q.Get("scope"), State: q.Get("state"),
		Challenge: q.Get("code_challenge"), Method: q.Get("code_challenge_method"), ResponseType: q.Get("response_type")}
}

func (s *Server) registerOAuth(mux *http.ServeMux) {
	resourceMeta := func(w http.ResponseWriter, r *http.Request) {
		b := baseURLOf(s, r)
		s.json(w, 200, map[string]any{"resource": b + "/mcp", "authorization_servers": []string{b}, "scopes_supported": domain.AllScopes, "bearer_methods_supported": []string{"header"}})
	}
	s.handle(mux, "GET /.well-known/oauth-protected-resource", resourceMeta)
	s.handle(mux, "GET /.well-known/oauth-protected-resource/mcp", resourceMeta)
	s.handle(mux, "GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		b := baseURLOf(s, r)
		s.json(w, 200, map[string]any{"issuer": b, "authorization_endpoint": b + "/oauth/authorize", "token_endpoint": b + "/oauth/token", "registration_endpoint": b + "/oauth/register", "revocation_endpoint": b + "/oauth/revoke",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": domain.AllScopes})
	})
	s.handle(mux, "POST /oauth/register", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string   `json:"client_name"`
			Redirects []string `json:"redirect_uris"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
			s.oauthFail(w, &domain.OAuthError{Code: "invalid_client_metadata", Description: "the body is not valid JSON"})
			return
		}
		id, err := s.Svc.OAuthRegister(r.Context(), in.Name, in.Redirects)
		if err != nil {
			s.oauthFail(w, err)
			return
		}
		s.oauthJSON(w, 201, map[string]any{"client_id": id, "client_name": in.Name, "redirect_uris": in.Redirects, "token_endpoint_auth_method": "none",
			"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
	})
	// the browser lands here from the client; the consent screen is part of the application
	s.handle(mux, "GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if _, _, err := s.Svc.OAuthCheck(r.Context(), authReq(q)); err != nil {
			s.oauthFail(w, err) // never redirect to an address that was not registered
			return
		}
		http.Redirect(w, r, "/#/oauth/consent?"+q.Encode(), http.StatusFound)
	})
	s.handle(mux, "GET /api/v1/oauth/consent", s.auth(func(w http.ResponseWriter, r *http.Request) {
		c, scopes, err := s.Svc.OAuthCheck(r.Context(), authReq(r.URL.Query()))
		if err != nil {
			var oe *domain.OAuthError
			if errors.As(err, &oe) {
				s.fail(w, 400, oe.Code, oe.Description, nil)
				return
			}
			s.reply(w, nil, err)
			return
		}
		list := []map[string]string{}
		for _, sc := range scopes {
			list = append(list, map[string]string{"name": sc, "description": scopeText[sc]})
		}
		u, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		s.json(w, 200, map[string]any{"client": c.Name, "scopes": list, "redirect_to": u.Scheme + "://" + u.Host})
	}))
	s.handle(mux, "POST /api/v1/oauth/authorize", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			s.fail(w, 403, "forbidden", "only a signed-in person can approve an application", nil)
			return
		}
		var in struct {
			ClientID, RedirectURI, Scope, State, Method, ResponseType string
			Challenge                                                 string `json:"code_challenge"`
			Approve                                                   bool   `json:"approve"`
		}
		var raw map[string]any
		if !s.decode(w, r, &raw) {
			return
		}
		get := func(k string) string { v, _ := raw[k].(string); return v }
		in.Approve, _ = raw["approve"].(bool)
		to, err := s.Svc.OAuthAuthorize(r.Context(), actorOf(r), domain.AuthorizeReq{ClientID: get("client_id"), RedirectURI: get("redirect_uri"), Scope: get("scope"), State: get("state"),
			Challenge: get("code_challenge"), Method: get("code_challenge_method"), ResponseType: get("response_type")}, in.Approve)
		if err != nil {
			var oe *domain.OAuthError
			if errors.As(err, &oe) {
				s.fail(w, 400, oe.Code, oe.Description, nil)
				return
			}
			s.reply(w, nil, err)
			return
		}
		s.json(w, 200, map[string]string{"redirect": to})
	}))
	s.handle(mux, "POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		if err := r.ParseForm(); err != nil {
			s.oauthFail(w, &domain.OAuthError{Code: "invalid_request", Description: "a form-encoded body is required"})
			return
		}
		t, err := s.Svc.OAuthToken(r.Context(), r.PostForm)
		if err != nil {
			s.oauthFail(w, err)
			return
		}
		s.oauthJSON(w, 200, t)
	})
	s.handle(mux, "POST /oauth/revoke", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		r.ParseForm()
		s.Svc.OAuthRevoke(r.Context(), r.PostForm.Get("token"))
		s.oauthJSON(w, 200, map[string]string{})
	})
	s.handle(mux, "GET /api/v1/me/oauth/grants", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListOAuthGrants(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "DELETE /api/v1/me/oauth/grants/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := pathID(r, "id"); ok {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.RevokeOAuthGrant(r.Context(), actorOf(r), id))
		} else {
			s.fail(w, 404, "not_found", "not found", nil)
		}
	}))
}
