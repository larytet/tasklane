package api

import "net/http"

func (s *Server) registerArchive(mux *http.ServeMux) {
	s.handle(mux, "POST /api/v1/issues/{key}/archive", s.auth(func(w http.ResponseWriter, r *http.Request) {
		ctx, undo := s.wantUndo(r)
		is, err := s.Svc.ArchiveIssue(ctx, actorOf(r), r.PathValue("key"))
		s.giveUndo(w, undo, err)
		s.reply(w, is, err)
	}))
	s.handle(mux, "DELETE /api/v1/issues/{key}/archive", s.auth(func(w http.ResponseWriter, r *http.Request) {
		is, err := s.Svc.UnarchiveIssue(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, is, err)
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/archive-policy", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !actorOf(r).Admin {
			s.fail(w, 403, "forbidden", "administrators only", nil)
			return
		}
		d, err := s.Svc.ArchivePolicy(r.Context(), r.PathValue("key"))
		s.reply(w, map[string]any{"days": d}, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/archive-policy", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Days *int `json:"days"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetArchivePolicy(r.Context(), actorOf(r), r.PathValue("key"), in.Days))
		}
	}))
}
