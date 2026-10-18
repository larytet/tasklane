package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

// The portal's cookie is not the staff cookie: staff authentication never reads it and portal authentication never reads the staff one
// or a bearer token, so a session of one kind opens nothing of the other (SPEC 15.1).
const portalCookie = "tl_portal"

type custKey struct{}

func customerOf(r *http.Request) domain.Customer {
	c, _ := r.Context().Value(custKey{}).(domain.Customer)
	return c
}

// portalAuth resolves the customer session cookie. Unsafe requests must carry X-Requested-With, as for staff.
func (s *Server) portalAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(portalCookie)
		if err != nil {
			s.fail(w, 401, "unauthorized", "sign in required", nil)
			return
		}
		cust, _, err := s.Svc.CustomerByToken(r.Context(), c.Value)
		if err != nil {
			s.fail(w, 401, "unauthorized", "sign in required", nil)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Requested-With") != "tasklane" {
			s.fail(w, 403, "csrf", "missing X-Requested-With header", nil)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), custKey{}, cust)))
	}
}

// portalCSRF guards the sign-in routes the same way, so a page on another site cannot make a browser start a sign-in.
func (s *Server) portalPublic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Requested-With") != "tasklane" {
			s.fail(w, 403, "csrf", "missing X-Requested-With header", nil)
			return
		}
		next(w, r)
	}
}

func (s *Server) startPortalSession(w http.ResponseWriter, tok string, c domain.Customer) {
	http.SetCookie(w, &http.Cookie{Name: portalCookie, Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600})
	s.json(w, 200, c)
}

func (s *Server) portalFailed(w http.ResponseWriter, err error) {
	var locked *domain.LockedError
	if errors.As(err, &locked) {
		s.fail(w, 429, "locked", "too many attempts; try again later", nil)
		return
	}
	s.fail(w, 400, "invalid", err.Error(), nil)
}

func (s *Server) registerPortal(mux *http.ServeMux) {
	ok := map[string]bool{"ok": true}
	s.handle(mux, "GET /api/v1/portal/{key}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.PortalInfoFor(r.Context(), r.PathValue("key"))
		s.reply(w, v, err)
	})
	s.handle(mux, "POST /api/v1/portal/{key}/code", s.portalPublic(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email   string `json:"email"`
			Website string `json:"website"` // the honeypot: a field people never see
		}
		if !s.decode(w, r, &in) {
			return
		}
		if err := s.Svc.PortalRequestCode(r.Context(), r.PathValue("key"), in.Email, in.Website, s.clientMeta(r)); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				s.reply(w, ok, err)
				return
			}
			s.portalFailed(w, err)
			return
		}
		s.json(w, 200, ok)
	}))
	s.handle(mux, "POST /api/v1/portal/{key}/verify", s.portalPublic(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Email, Code string }
		if !s.decode(w, r, &in) {
			return
		}
		tok, c, err := s.Svc.PortalVerify(r.Context(), r.PathValue("key"), in.Email, in.Code, s.clientMeta(r))
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				s.reply(w, ok, err)
				return
			}
			s.portalFailed(w, err)
			return
		}
		s.startPortalSession(w, tok, c)
	}))
	s.handle(mux, "POST /api/v1/portal/{key}/login", s.portalPublic(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Email, Password string }
		if !s.decode(w, r, &in) {
			return
		}
		tok, c, err := s.Svc.PortalLoginPassword(r.Context(), r.PathValue("key"), in.Email, in.Password, s.clientMeta(r))
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				s.reply(w, ok, err)
				return
			}
			s.portalFailed(w, err)
			return
		}
		s.startPortalSession(w, tok, c)
	}))
	s.handle(mux, "POST /api/v1/portal/link", s.portalPublic(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Token string }
		if !s.decode(w, r, &in) {
			return
		}
		tok, c, err := s.Svc.PortalVerifyLink(r.Context(), in.Token, s.clientMeta(r))
		if err != nil {
			s.portalFailed(w, err)
			return
		}
		s.startPortalSession(w, tok, c)
	}))
	s.handle(mux, "GET /api/v1/portal/me", s.portalAuth(func(w http.ResponseWriter, r *http.Request) {
		s.json(w, 200, customerOf(r))
	}))
	s.handle(mux, "PUT /api/v1/portal/me", s.portalAuth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Language, Timezone string }
		if s.decode(w, r, &in) {
			v, err := s.Svc.UpdateCustomer(r.Context(), customerOf(r), in.Name, in.Language, in.Timezone)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/portal/password", s.portalAuth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Password string }
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.SetCustomerPassword(r.Context(), customerOf(r), in.Password))
		}
	}))
	s.handle(mux, "POST /api/v1/portal/logout", s.portalAuth(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(portalCookie); err == nil {
			s.Svc.PortalLogout(r.Context(), c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: portalCookie, Value: "", Path: "/", MaxAge: -1})
		s.json(w, 200, ok)
	}))
	// staff administer a portal
	s.handle(mux, "GET /api/v1/projects/{key}/portal", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetPortalPolicy(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/portal", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.PortalPolicy
		if s.decode(w, r, &in) {
			v, err := s.Svc.SetPortalPolicy(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/portal/block", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email string `json:"email"`
			Block bool   `json:"block"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.BlockCustomer(r.Context(), actorOf(r), r.PathValue("key"), in.Email, in.Block))
		}
	}))
}
