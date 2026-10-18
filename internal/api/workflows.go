package api

import (
	"errors"
	"net/http"

	"github.com/larytet/tasklane/internal/domain"
)

func (s *Server) registerWorkflows(mux *http.ServeMux) {
	adm := func(w http.ResponseWriter, r *http.Request) bool {
		if !actorOf(r).Admin {
			s.fail(w, 403, "forbidden", "administrators only", nil)
			return false
		}
		return true
	}
	s.handle(mux, "GET /api/v1/workflows", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListWorkflows(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/workflows", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name       string       `json:"name"`
			Template   string       `json:"template"`
			Definition *domain.WDef `json:"definition"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateWorkflow(r.Context(), actorOf(r), in.Name, in.Template, in.Definition)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "PUT /api/v1/projects/{key}/workflow", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Workflow string `json:"workflow"`
		}
		if s.decode(w, r, &in) {
			s.reply(w, map[string]bool{"ok": true}, s.Svc.SetProjectWorkflow(r.Context(), actorOf(r), r.PathValue("key"), in.Workflow))
		}
	}))
	s.handle(mux, "GET /api/v1/workflow-templates", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, domain.WorkflowTemplateNames(), nil)
	}))
	s.handle(mux, "GET /api/v1/workflows/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetWorkflow(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/workflows/{id}/export", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ExportWorkflow(r.Context(), actorOf(r), r.PathValue("id"))
		if err == nil {
			w.Header().Set("Content-Disposition", `attachment; filename="workflow.json"`)
		}
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/workflows/{id}/draft", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.StartWorkflowDraft(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/workflows/{id}/draft", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var d domain.WDef
		if s.decode(w, r, &d) {
			v, err := s.Svc.SaveWorkflowDraft(r.Context(), actorOf(r), r.PathValue("id"), d)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/workflows/{id}/draft", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.AbandonWorkflowDraft(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/workflows/{id}/draft/check", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mapping map[string]string `json:"mapping"`
		}
		if !adm(w, r) || !s.decode(w, r, &in) {
			return
		}
		v, err := s.Svc.PlanWorkflowPublish(r.Context(), actorOf(r), r.PathValue("id"), in.Mapping)
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/workflows/{id}/publish", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mapping map[string]string `json:"mapping"`
			Note    string            `json:"note"`
		}
		if !adm(w, r) || !s.decode(w, r, &in) {
			return
		}
		v, err := s.Svc.PublishWorkflow(r.Context(), actorOf(r), r.PathValue("id"), in.Mapping, in.Note)
		var refused *domain.PublishRefused
		if errors.As(err, &refused) {
			s.fail(w, 409, "publish_refused", err.Error(), refused.Plan)
			return
		}
		s.reply(w, v, err)
	}))
}
