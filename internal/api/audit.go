package api

import (
	"context"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

// ordinaryWork lists the changing routes that are people doing their work on issues, boards and sprints. They have their
// own history, events or domain audit. Every other route that changes something and succeeds is written to the audit log,
// so a new administrative route is audited unless someone decides on purpose that it is ordinary work.
var ordinaryWork = map[string]bool{
	"POST /oauth/register": true, "POST /oauth/token": true, "POST /oauth/revoke": true, // clients with no account yet: the grant is audited by the domain
	"POST /api/v1/login": true, "POST /api/v1/login/mfa": true, "POST /api/v1/logout": true, "POST /api/v1/ws/ticket": true,
	"POST /mcp": true, "DELETE /mcp": true, // the MCP server keeps its own call log
	"POST /api/v1/mail/inbound/{id}": true, "POST /api/v1/mail/bounces/{id}": true, "POST /api/v1/git/webhooks/{id}": true, "POST /api/v1/jira/webhooks/{id}": true, // inbound, signed by the provider
	"POST /api/v1/issues": true, "PATCH /api/v1/issues/{key}": true, "POST /api/v1/issues/{key}/comments": true, "PATCH /api/v1/issues/{key}/comments/{id}": true,
	"POST /api/v1/issues/{key}/transitions": true, "POST /api/v1/issues/{key}/links": true, "POST /api/v1/issues/{key}/archive": true, "DELETE /api/v1/issues/{key}/archive": true,
	"POST /api/v1/issues/{key}/lifecycle": true, // audited by the domain
	"POST /api/v1/undo/{token}":           true, // a person's own undo, audited by the domain as issue.undo
	"PUT /api/v1/issues/{key}/versions":   true, // an edit of the issue, with its own history
	"POST /api/v1/issues/{key}/presence":  true, "DELETE /api/v1/issues/{key}/presence": true,
	"PUT /api/v1/issues/{key}/text/{field}": true, "POST /api/v1/issues/{key}/text/{field}/patch": true, "POST /api/v1/issues/{key}/text/{field}/replace": true, "POST /api/v1/issues/{key}/text/{field}/restore": true,
	"PUT /api/v1/issues/{key}/drafts/{field}": true, "DELETE /api/v1/issues/{key}/drafts/{field}": true,
	"POST /api/v1/issues/{key}/attachments": true, "DELETE /api/v1/issues/{key}/attachments/{id}": true, "POST /api/v1/issues/{key}/uploads": true, "POST /api/v1/issues/{key}/uploads/direct": true,
	"PATCH /api/v1/uploads/{id}": true, "POST /api/v1/uploads/{id}/commit": true, "DELETE /api/v1/uploads/direct/{id}": true, "POST /api/v1/uploads/direct/{id}/complete": true,
	"POST /api/v1/attachments/{id}/link": true, "POST /api/v1/attachments/{id}/thumbnail": true,
	"POST /api/v1/issues/{key}/development/links": true, "DELETE /api/v1/issues/{key}/development/links/{id}": true,
	"POST /api/v1/boards/{id}/move": true, "POST /api/v1/boards/{id}/rank": true, "POST /api/v1/boards/{id}/plan": true,
	"PUT /api/v1/sprints/{id}/retro":      true,
	"POST /api/v1/notifications/read-all": true, "POST /api/v1/notifications/{id}/state": true, "PUT /api/v1/me/notification-prefs": true, "PUT /api/v1/me/settings": true, "PUT /api/v1/me/ui-prefs": true, // a person managing their own inbox
	"PUT /api/v1/issues/{key}/watch": true, "DELETE /api/v1/issues/{key}/watch": true,
	"POST /api/v1/admin/mail/imap/{id}/check":     true, // a read: it connects and changes nothing
	"POST /api/v1/admin/sso/providers/{id}/check": true, // a read: it asks the directory and changes nothing
	"POST /api/v1/automation/rules/{id}/dry-run":  true, // a read: it shows what would happen and changes nothing
	"POST /api/v1/workflows/{id}/draft/check":     true, // a read: it plans and changes nothing
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func mutating(method string) bool {
	return method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
}

// audited wraps a route so that a successful change outside ordinary work is recorded: who, what route, which address,
// from where and the status. The domain writes its own, richer records inside the transaction of what they describe.
func (s *Server) audited(pattern string, h http.HandlerFunc) http.HandlerFunc {
	method := strings.SplitN(pattern, " ", 2)[0]
	if !mutating(method) || ordinaryWork[pattern] {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		holder := &actorHolder{}
		sw := &statusWriter{ResponseWriter: w}
		h(sw, r.WithContext(context.WithValue(r.Context(), holderKey{}, holder)))
		if sw.status/100 != 2 || holder.a.ID == "" {
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		via := "token"
		if _, err := r.Cookie(cookieName); err == nil && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			via = "session"
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		if err := s.Svc.AuditAPI(ctx, holder.a, pattern, r.URL.Path, map[string]any{"status": sw.status, "ip": ip, "via": via}); err != nil {
			s.logf("audit write failed for %s: %v", pattern, err)
		}
	}
}

func (s *Server) registerAudit(mux *http.ServeMux) {
	adm := func(next http.HandlerFunc) http.HandlerFunc {
		return s.auth(func(w http.ResponseWriter, r *http.Request) {
			if !actorOf(r).Admin {
				s.fail(w, 403, "forbidden", "administrators only", nil)
				return
			}
			next(w, r)
		})
	}
	query := func(r *http.Request) (q domain.AuditQuery) {
		v := r.URL.Query()
		q.Actor, q.Action, q.Target = v.Get("actor"), v.Get("action"), v.Get("target")
		q.From, _ = time.Parse(time.RFC3339, v.Get("from"))
		q.To, _ = time.Parse(time.RFC3339, v.Get("to"))
		q.Limit, _ = strconv.Atoi(v.Get("limit"))
		q.Before, _ = strconv.ParseInt(v.Get("before"), 10, 64)
		return
	}
	s.handle(mux, "GET /api/v1/admin/audit", adm(func(w http.ResponseWriter, r *http.Request) {
		q := query(r)
		v, err := s.Svc.QueryAudit(r.Context(), actorOf(r), q)
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/admin/audit/verify", adm(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.VerifyAudit(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/admin/audit/export", adm(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="audit.csv"`)
		if _, err := s.Svc.ExportAudit(r.Context(), actorOf(r), w, query(r)); err != nil {
			s.logf("audit export: %v", err)
		}
	}))
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
		return
	}
	log.Printf(f, a...)
}

// OrdinaryRoutes lists the routes that are not written to the audit log by the wrapper, for the test that keeps the list honest.
func OrdinaryRoutes() []string {
	out := make([]string, 0, len(ordinaryWork))
	for p := range ordinaryWork {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
