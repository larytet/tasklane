package api

import (
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

func (s *Server) registerDashboards(mux *http.ServeMux) {
	ok := map[string]bool{"ok": true}
	s.handle(mux, "GET /api/v1/filters", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListFilters(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/filters", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.FilterIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateFilter(r.Context(), actorOf(r), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/filters/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.FilterIn
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.UpdateFilter(r.Context(), actorOf(r), r.PathValue("id"), in))
		}
	}))
	s.handle(mux, "DELETE /api/v1/filters/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeleteFilter(r.Context(), actorOf(r), r.PathValue("id")))
	}))

	s.handle(mux, "GET /api/v1/dashboard-kinds", s.auth(func(w http.ResponseWriter, r *http.Request) {
		dims := []string{}
		for k := range search.Dimensions {
			dims = append(dims, k)
		}
		s.json(w, 200, map[string]any{"kinds": domain.GadgetKinds, "dimensions": dims, "iframe_hosts": s.Svc.DashboardIframeHosts})
	}))
	s.handle(mux, "GET /api/v1/dashboards", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListDashboards(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/dashboards", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.DashboardIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateDashboard(r.Context(), actorOf(r), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/dashboards/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetDashboard(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/dashboards/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.DashboardIn
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.UpdateDashboard(r.Context(), actorOf(r), r.PathValue("id"), in))
		}
	}))
	s.handle(mux, "DELETE /api/v1/dashboards/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeleteDashboard(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/dashboards/{id}/clone", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.CloneDashboard(r.Context(), actorOf(r), r.PathValue("id"))
		s.replyStatus(w, 201, v, err)
	}))
	s.handle(mux, "PUT /api/v1/dashboards/{id}/favourite", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.SetFavourite(r.Context(), actorOf(r), r.PathValue("id"), true))
	}))
	s.handle(mux, "DELETE /api/v1/dashboards/{id}/favourite", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.SetFavourite(r.Context(), actorOf(r), r.PathValue("id"), false))
	}))
	s.handle(mux, "PUT /api/v1/me/default-dashboard", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.SetDefaultDashboard(r.Context(), actorOf(r), in.ID))
		}
	}))
	s.handle(mux, "POST /api/v1/dashboards/{id}/gadgets", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.GadgetIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.AddGadget(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/dashboards/{id}/gadgets/{gid}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.GadgetIn
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.UpdateGadget(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("gid"), in))
		}
	}))
	s.handle(mux, "DELETE /api/v1/dashboards/{id}/gadgets/{gid}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeleteGadget(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("gid")))
	}))
	s.handle(mux, "GET /api/v1/dashboards/{id}/gadgets/{gid}/data", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GadgetData(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("gid"))
		s.reply(w, v, err)
	}))
}
