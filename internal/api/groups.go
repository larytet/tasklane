package api

import "net/http"

func (s *Server) registerGroups(mux *http.ServeMux) {
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
	s.handle(mux, "GET /api/v1/groups", adm(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListGroups(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/groups", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateGroup(r.Context(), actorOf(r), in.Name)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/groups/{id}", adm(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetGroup(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "DELETE /api/v1/groups/{id}", adm(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeleteGroup(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/groups/{id}/members", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			User string `json:"user"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.AddGroupMember(r.Context(), actorOf(r), r.PathValue("id"), in.User))
		}
	}))
	s.handle(mux, "DELETE /api/v1/groups/{id}/members/{user}", adm(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.RemoveGroupMember(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("user")))
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/groups", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ProjectGroups(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/groups/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Role string `json:"role"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.SetProjectGroup(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("id"), in.Role))
		}
	}))
	s.handle(mux, "DELETE /api/v1/projects/{key}/groups/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.SetProjectGroup(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("id"), ""))
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/permissions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.PermissionScheme(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/permissions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if s.decode(w, r, &in) {
			s.reply(w, ok, s.Svc.SetPermissionScheme(r.Context(), actorOf(r), r.PathValue("key"), in))
		}
	}))
}
