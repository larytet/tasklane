package api

import "net/http"

func (s *Server) registerQuota(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/projects/{key}/storage", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.StorageUsage(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/storage-quota", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			MiB *int64 `json:"mib"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetStorageQuota(r.Context(), actorOf(r), r.PathValue("key"), in.MiB))
		}
	}))
	s.handle(mux, "GET /api/v1/admin/storage", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.StorageOverview(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
}
