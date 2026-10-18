package api

import (
	"net/http"
	"strconv"

	"github.com/larytet/tasklane/internal/domain"
)

func pathID(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil
}

func (s *Server) registerFields(mux *http.ServeMux) {
	bad := func(w http.ResponseWriter) { s.fail(w, 404, "not_found", "not found", nil) }
	s.handle(mux, "GET /api/v1/fields", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListFields(r.Context(), r.URL.Query().Get("trashed") == "1" && actorOf(r).Admin)
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/fields", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.FieldIn
		if !s.decode(w, r, &in) {
			return
		}
		f, err := s.Svc.CreateField(r.Context(), actorOf(r), in)
		s.replyStatus(w, 201, f, err)
	}))
	s.handle(mux, "PATCH /api/v1/fields/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			bad(w)
			return
		}
		var in domain.FieldIn
		if !s.decode(w, r, &in) {
			return
		}
		f, err := s.Svc.UpdateField(r.Context(), actorOf(r), id, in)
		s.reply(w, f, err)
	}))
	s.handle(mux, "DELETE /api/v1/fields/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			bad(w)
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.TrashField(r.Context(), actorOf(r), id))
	}))
	s.handle(mux, "POST /api/v1/fields/{id}/restore", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			bad(w)
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.RestoreField(r.Context(), actorOf(r), id))
	}))
	s.handle(mux, "GET /api/v1/fields/{id}/contexts", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok || !actorOf(r).Admin {
			if ok {
				s.fail(w, 403, "forbidden", "administrators only", nil)
				return
			}
			bad(w)
			return
		}
		v, err := s.Svc.ListContexts(r.Context(), id)
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/fields/{id}/contexts", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			bad(w)
			return
		}
		var in domain.ContextIn
		if !s.decode(w, r, &in) {
			return
		}
		c, err := s.Svc.AddContext(r.Context(), actorOf(r), id, in)
		s.replyStatus(w, 201, c, err)
	}))
	s.handle(mux, "DELETE /api/v1/fields/{id}/contexts/{ctx}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		cid, ok2 := pathID(r, "ctx")
		if !ok || !ok2 {
			bad(w)
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.RemoveContext(r.Context(), actorOf(r), id, cid))
	}))
	s.handle(mux, "POST /api/v1/fields/{id}/contexts/{ctx}/options", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		cid, ok2 := pathID(r, "ctx")
		if !ok || !ok2 {
			bad(w)
			return
		}
		var in struct{ Value, Color string }
		if !s.decode(w, r, &in) {
			return
		}
		o, err := s.Svc.AddOption(r.Context(), actorOf(r), id, cid, in.Value, in.Color)
		s.replyStatus(w, 201, o, err)
	}))
	s.handle(mux, "PATCH /api/v1/fields/options/{option}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		oid, ok := pathID(r, "option")
		if !ok {
			bad(w)
			return
		}
		var in struct{ Archived bool }
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.ArchiveOption(r.Context(), actorOf(r), oid, in.Archived))
	}))
	s.handle(mux, "POST /api/v1/fields/{id}/merge-options", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			bad(w)
			return
		}
		var in struct{ From, Into string }
		if !s.decode(w, r, &in) {
			return
		}
		n, err := s.Svc.MergeOptions(r.Context(), actorOf(r), id, in.From, in.Into)
		s.reply(w, map[string]int{"issues": n}, err)
	}))
	s.handle(mux, "POST /api/v1/fields/{id}/replace", s.auth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r, "id")
		if !ok {
			bad(w)
			return
		}
		var in struct {
			With int64 `json:"with"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		n, err := s.Svc.ReplaceField(r.Context(), actorOf(r), id, in.With)
		s.reply(w, map[string]int{"issues": n}, err)
	}))
	s.handle(mux, "GET /api/v1/fields/warnings", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.FieldWarnings(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	// the form definition for creating or editing an issue of a type: the fields that apply, with options, defaults and required flags
	s.handle(mux, "GET /api/v1/projects/{key}/fields", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ProjectFields(r.Context(), actorOf(r), r.PathValue("key"), r.URL.Query().Get("type"))
		s.reply(w, v, err)
	}))
}
