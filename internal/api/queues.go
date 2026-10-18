package api

import (
	"net/http"
	"strconv"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerQueues(mux *http.ServeMux) {
	ok := map[string]bool{"ok": true}
	s.handle(mux, "GET /api/v1/projects/{key}/queues", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListQueues(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/queues", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.QueueIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateQueue(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/queues/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.QueueIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.UpdateQueue(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/queues/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeleteQueue(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "GET /api/v1/queues/{id}/issues", s.auth(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		v, err := s.Svc.QueueIssues(r.Context(), actorOf(r), r.PathValue("id"), n)
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/canned-responses", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListCanned(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/canned-responses", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.CannedIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateCanned(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/canned-responses/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.CannedIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.UpdateCanned(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/canned-responses/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, ok, s.Svc.DeleteCanned(r.Context(), actorOf(r), r.PathValue("id")))
	}))
}
