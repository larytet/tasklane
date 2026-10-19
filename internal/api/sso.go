package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerSSO(mux *http.ServeMux) {
	adm := func(next http.HandlerFunc) http.HandlerFunc {
		return s.auth(func(w http.ResponseWriter, r *http.Request) {
			if !actorOf(r).Admin {
				s.fail(w, 403, "forbidden", "administrators only", nil)
				return
			}
			next(w, r)
		})
	}
	ok := map[string]bool{"ok": true}
	// the portal's own provider, kept by an administrator of the project
	s.handle(mux, "GET /api/v1/projects/{key}/portal/sso", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetPortalSSO(r.Context(), actorOf(r), r.PathValue("key"))
		if err == nil && v == nil {
			s.json(w, 200, map[string]any{"configured": false})
			return
		}
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/portal/sso", s.auth(func(w http.ResponseWriter, r *http.Request) {
		in := domain.SSOProvider{Enabled: true}
		if s.decode(w, r, &in) {
			v, err := s.Svc.SavePortalSSO(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/projects/{key}/portal/sso", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeletePortalSSO(r.Context(), actorOf(r), r.PathValue("key")))
	}))
	s.handle(mux, "GET /api/v1/admin/sso/providers", adm(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListSSOProviders(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/admin/sso/providers", adm(func(w http.ResponseWriter, r *http.Request) {
		in := domain.SSOProvider{JIT: true, Enabled: true}
		if s.decode(w, r, &in) {
			v, err := s.Svc.SaveSSOProvider(r.Context(), actorOf(r), 0, in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/admin/sso/providers/{id}", adm(func(w http.ResponseWriter, r *http.Request) {
		id, good := pathID(r, "id")
		in := domain.SSOProvider{JIT: true, Enabled: true}
		if good && s.decode(w, r, &in) {
			v, err := s.Svc.SaveSSOProvider(r.Context(), actorOf(r), id, in)
			s.reply(w, v, err)
		} else if !good {
			s.fail(w, 404, "not_found", "not found", nil)
		}
	}))
	s.handle(mux, "DELETE /api/v1/admin/sso/providers/{id}", adm(func(w http.ResponseWriter, r *http.Request) {
		if id, good := pathID(r, "id"); good {
			s.reply(w, ok, s.Svc.DeleteSSOProvider(r.Context(), actorOf(r), id))
		} else {
			s.fail(w, 404, "not_found", "not found", nil)
		}
	}))
	// the sign-in page asks which buttons to show: enabled providers' names only
	s.handle(mux, "GET /api/v1/auth/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		s.json(w, 200, s.Svc.SSOChoices(r.Context()))
	})
	s.handle(mux, "GET /api/v1/auth/sso/{name}/start", func(w http.ResponseWriter, r *http.Request) {
		to, err := s.Svc.SSOStart(r.Context(), r.PathValue("name"), r.URL.Query().Get("redirect"))
		if errors.Is(err, domain.ErrNotFound) {
			s.fail(w, 404, "not_found", "no such sign-in provider", nil)
			return
		}
		if err != nil {
			s.fail(w, 502, "sso_unavailable", "the identity provider could not be reached", nil)
			return
		}
		http.Redirect(w, r, to, http.StatusFound)
	})
	s.handle(mux, "GET /api/v1/auth/sso/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("error") != "" || q.Get("code") == "" || q.Get("state") == "" {
			http.Redirect(w, r, "/?sso=denied", http.StatusFound)
			return
		}
		res, err := s.Svc.SSOCallback(r.Context(), q.Get("state"), q.Get("code"), s.SessionTTL, s.clientMeta(r))
		s.ssoDone(w, r, res, err)
	})
	// SAML 2.0: the metadata an identity provider's administrator needs, and the address the identity provider posts its signed response to
	s.handle(mux, "GET /api/v1/auth/saml/{name}/metadata", func(w http.ResponseWriter, r *http.Request) {
		doc, err := s.Svc.SAMLMetadata(r.Context(), r.PathValue("name"))
		if err != nil {
			s.fail(w, 404, "not_found", "no such SAML provider", nil)
			return
		}
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		w.Write([]byte(doc))
	})
	s.handle(mux, "POST /api/v1/auth/saml/acs", func(w http.ResponseWriter, r *http.Request) {
		// a form posted by the person's browser from the identity provider: no session and no CSRF header can exist, and none is needed: the
		// relay state names a request we made, is used once, and the response must answer that request
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		if err := r.ParseForm(); err != nil || r.PostForm.Get("SAMLResponse") == "" || r.PostForm.Get("RelayState") == "" {
			http.Redirect(w, r, "/?sso=denied", http.StatusFound)
			return
		}
		res, err := s.Svc.SAMLCallback(r.Context(), r.PostForm.Get("RelayState"), r.PostForm.Get("SAMLResponse"), s.SessionTTL, s.clientMeta(r))
		s.ssoDone(w, r, res, err)
	})
	s.handle(mux, "GET /api/v1/admin/scim/tokens", adm(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListSCIMTokens(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/admin/scim/tokens", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		tok, info, err := s.Svc.CreateSCIMToken(r.Context(), actorOf(r), in.Name)
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		s.json(w, 201, map[string]any{"token": tok, "info": info}) // shown once
	}))
	s.handle(mux, "DELETE /api/v1/admin/scim/tokens/{id}", adm(func(w http.ResponseWriter, r *http.Request) {
		if id, good := pathID(r, "id"); good {
			s.reply(w, ok, s.Svc.RevokeSCIMToken(r.Context(), actorOf(r), id))
		} else {
			s.fail(w, 404, "not_found", "not found", nil)
		}
	}))
}

// ssoDone ends a sign-in through an identity provider: the session cookie of the side it belongs to (staff, or a portal's customers) and a
// redirect to a path on this server. A refusal sends the person back to where they started, with no reason: it would help a guesser.
func (s *Server) ssoDone(w http.ResponseWriter, r *http.Request, res domain.SSOResult, err error) {
	if err != nil {
		to := "/?sso=denied"
		if strings.HasPrefix(res.Redirect, "/portal/") {
			to = res.Redirect + "?sso=denied"
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	if res.PortalToken != "" {
		http.SetCookie(w, &http.Cookie{Name: portalCookie, Value: res.PortalToken, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600})
		http.Redirect(w, r, res.Redirect, http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: res.Token, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(s.SessionTTL.Seconds())})
	http.Redirect(w, r, res.Redirect, http.StatusFound) // a path on this server: the domain only stores addresses that start with one slash
}
