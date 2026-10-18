package api

import (
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerScreens(mux *http.ServeMux) {
	// admin wraps a handler that only administrators may use, for reads whose domain call has no role check of its own
	admin := func(fn http.HandlerFunc) http.HandlerFunc {
		return s.auth(func(w http.ResponseWriter, r *http.Request) {
			if !actorOf(r).Admin {
				s.fail(w, 403, "forbidden", "administrators only", nil)
				return
			}
			fn(w, r)
		})
	}
	id := func(w http.ResponseWriter, r *http.Request) (int64, bool) {
		n, ok := pathID(r, "id")
		if !ok {
			s.fail(w, 404, "not_found", "not found", nil)
		}
		return n, ok
	}
	ok := map[string]bool{"ok": true}

	s.handle(mux, "GET /api/v1/screens", admin(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListScreens(r.Context())
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/screens", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.Screen
		if s.decode(w, r, &in) {
			v, err := s.Svc.SaveScreen(r.Context(), actorOf(r), 0, in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/screens/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, good := id(w, r)
		var in domain.Screen
		if good && s.decode(w, r, &in) {
			v, err := s.Svc.SaveScreen(r.Context(), actorOf(r), n, in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/screens/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if n, good := id(w, r); good {
			s.reply(w, ok, s.Svc.DeleteScreen(r.Context(), actorOf(r), n))
		}
	}))

	s.handle(mux, "GET /api/v1/screen-schemes", admin(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListSchemes(r.Context())
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/screen-schemes", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.Scheme
		if s.decode(w, r, &in) {
			v, err := s.Svc.SaveScheme(r.Context(), actorOf(r), 0, in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/screen-schemes/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, good := id(w, r)
		var in domain.Scheme
		if good && s.decode(w, r, &in) {
			v, err := s.Svc.SaveScheme(r.Context(), actorOf(r), n, in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/screen-schemes/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if n, good := id(w, r); good {
			s.reply(w, ok, s.Svc.DeleteScheme(r.Context(), actorOf(r), n))
		}
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/screen-scheme", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Scheme *int64 `json:"scheme"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.SetProjectScheme(r.Context(), actorOf(r), r.PathValue("key"), in.Scheme))
		}
	}))

	s.handle(mux, "GET /api/v1/field-configs", admin(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListFieldConfigs(r.Context())
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/field-configs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.FieldCfgRow
		if s.decode(w, r, &in) {
			v, err := s.Svc.SaveFieldConfig(r.Context(), actorOf(r), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/field-configs/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if n, good := id(w, r); good {
			s.reply(w, ok, s.Svc.DeleteFieldConfig(r.Context(), actorOf(r), n))
		}
	}))

	s.handle(mux, "GET /api/v1/behavior-rules", admin(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListRules(r.Context())
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/behavior-rules", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.Rule
		in.Enabled = true
		if s.decode(w, r, &in) {
			v, err := s.Svc.SaveRule(r.Context(), actorOf(r), 0, in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/behavior-rules/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, good := id(w, r)
		var in domain.Rule
		in.Enabled = true
		if good && s.decode(w, r, &in) {
			v, err := s.Svc.SaveRule(r.Context(), actorOf(r), n, in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/behavior-rules/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if n, good := id(w, r); good {
			s.reply(w, ok, s.Svc.DeleteRule(r.Context(), actorOf(r), n))
		}
	}))

	// the screen for an operation, as the create dialog and the ticket render it
	s.handle(mux, "GET /api/v1/projects/{key}/form", s.auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		op := q.Get("op")
		if op == "" {
			op = "create"
		}
		v, err := s.Svc.Form(r.Context(), actorOf(r), r.PathValue("key"), q.Get("type"), op)
		s.reply(w, v, err)
	}))
}
