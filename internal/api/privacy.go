package api

import (
	"encoding/json"
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerPrivacy(mux *http.ServeMux) {
	adm := func(next http.HandlerFunc) http.HandlerFunc {
		return s.auth(func(w http.ResponseWriter, r *http.Request) {
			if !actorOf(r).Admin {
				s.fail(w, 403, "forbidden", "administrators only", nil)
				return
			}
			next(w, r)
		})
	}
	s.handle(mux, "GET /api/v1/admin/users/{id}/personal-data", adm(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ExportPersonalData(r.Context(), actorOf(r), r.PathValue("id"))
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="personal-data.json"`)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	}))
	s.handle(mux, "POST /api/v1/admin/users/{id}/erase", adm(func(w http.ResponseWriter, r *http.Request) {
		var in domain.EraseOpts
		if !s.decode(w, r, &in) {
			return
		}
		v, err := s.Svc.ErasePerson(r.Context(), actorOf(r), r.PathValue("id"), in)
		s.reply(w, v, err)
	}))
}
