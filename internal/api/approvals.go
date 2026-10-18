package api

import (
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerApprovals(mux *http.ServeMux) {
	s.handle(mux, "GET /api/v1/issues/{key}/approvals", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.IssueApprovals(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/issues/{key}/approvals", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.ApprovalIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.RequestApproval(r.Context(), actorOf(r), r.PathValue("key"), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/approvals", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.MyApprovals(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/approvals/{id}/decision", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Decision string `json:"decision"`
			Comment  string `json:"comment"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.DecideApproval(r.Context(), actorOf(r), r.PathValue("id"), in.Decision, in.Comment)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "POST /api/v1/approvals/{id}/cancel", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.CancelApproval(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
}
