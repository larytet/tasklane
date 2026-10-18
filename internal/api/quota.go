package api

import (
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

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
	s.handle(mux, "GET /api/v1/admin/storage/tiering", s.auth(func(w http.ResponseWriter, r *http.Request) {
		a := actorOf(r)
		rules, err := s.Svc.TierRules(r.Context(), a)
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		st, err := s.Svc.TierStatus(r.Context(), a)
		s.reply(w, map[string]any{"rules": rules, "status": st}, err)
	}))
	s.handle(mux, "PUT /api/v1/admin/storage/tiering/rules", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Rules []domain.TierRule `json:"rules"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetTierRules(r.Context(), actorOf(r), in.Rules))
		}
	}))
	s.handle(mux, "POST /api/v1/admin/storage/tiering/run", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !actorOf(r).Admin {
			s.reply(w, nil, domain.ErrForbidden)
			return
		}
		v, err := s.Svc.RunTiering(r.Context(), 200)
		s.reply(w, v, err)
	}))
}
