// Package api is the HTTP layer: JSON endpoints for the UI and for API clients.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mcp"
	"github.com/larytet/tasklane/internal/rt"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/web"
)

type Server struct {
	Svc        *domain.Service
	Logf       func(string, ...any) // where server-side problems are reported (default: the standard logger)
	SessionTTL time.Duration
	Secure     bool // set the Secure flag on the session cookie
	Extra      func(mux *http.ServeMux, s *Server)
	Ready      func(ctx context.Context) error
	// Degraded says what is not working that does not stop this node serving (empty: nothing); it shows on /readyz as a note, not a failure
	Degraded    func() string
	Search      search.Searcher
	SearchAdmin *search.Admin
	BlobOrigin  string // optional separate origin for signed file links
	Hub         *rt.Hub
	MCP         *mcp.Server
	// Version is what this server runs. ClientVersion is the newest browser client it knows; MinClient is the oldest it still
	// serves (default: the previous one, so a tab left open across an upgrade keeps working). A client older than that is told to
	// reload with 426. Requests that do not say (API tokens, MCP, scripts) are not clients in this sense and are never refused.
	Version   string
	MinClient int
	draining  atomic.Bool
	routes    []string
}

// Drain makes /readyz fail, the first step of a graceful stop; /healthz keeps answering (the process is alive).
func (s *Server) Drain() { s.draining.Store(true) }

// ClientVersion is the newest browser client this server speaks to (sent by the page in X-Tasklane-Client).
const ClientVersion = 1

func (s *Server) minClient() int {
	if s.MinClient > 0 {
		return s.MinClient
	}
	return ClientVersion - 1
}

// handle registers a route and records its pattern, so a test can prove that every route is covered by the authorization matrix.
func (s *Server) handle(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	s.routes = append(s.routes, pattern)
	mux.HandleFunc(pattern, s.audited(pattern, h))
}

// Routes lists the registered patterns (method and path), for the authorization matrix test.
func (s *Server) Routes() []string { s.Handler(); return append([]string(nil), s.routes...) }

type ctxKey struct{}
type holderKey struct{}

// actorHolder lets the audit wrapper, which sits outside authentication, learn who the request turned out to be.
type actorHolder struct{ a domain.Actor }

func actorOf(r *http.Request) domain.Actor {
	a, _ := r.Context().Value(ctxKey{}).(domain.Actor)
	return a
}

const cookieName = "tl_session"

func (s *Server) Handler() http.Handler {
	s.routes = nil
	mux := http.NewServeMux()
	s.handle(mux, "GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.handle(mux, "GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() { // a load balancer stops sending new work here while what is in flight finishes
			http.Error(w, "draining", 503)
			return
		}
		if s.Ready != nil {
			if err := s.Ready(r.Context()); err != nil {
				http.Error(w, "not ready", 503)
				return
			}
		}
		if s.Degraded != nil { // still ready: a node whose link to the cluster bus is down serves everything, only alone
			if d := s.Degraded(); d != "" {
				w.Write([]byte("ready, degraded: " + d))
				return
			}
		}
		w.Write([]byte("ready"))
	})
	s.handle(mux, "GET /api/v1/system/status", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.json(w, 200, map[string]any{"server_version": s.Version, "client_version": ClientVersion, "min_client": s.minClient(), "maintenance": s.Svc.ActiveMaintenance(r.Context()), "scheduled": s.Svc.UpcomingMaintenance(r.Context(), actorOf(r))})
	}))
	s.handle(mux, "POST /api/v1/login", s.login)
	s.handle(mux, "POST /api/v1/login/mfa", s.loginMFA)
	s.handle(mux, "POST /api/v1/login/passkey", s.loginPasskey)
	s.handle(mux, "POST /api/v1/logout", s.auth(s.logout))
	s.handle(mux, "GET /api/v1/me", s.auth(func(w http.ResponseWriter, r *http.Request) { s.json(w, 200, actorOf(r)) }))
	s.handle(mux, "GET /api/v1/users", s.auth(func(w http.ResponseWriter, r *http.Request) {
		us, err := s.Svc.ListUsersFor(r.Context(), actorOf(r))
		s.reply(w, us, err)
	}))

	s.handle(mux, "POST /api/v1/users", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if a := actorOf(r); !a.Admin && !a.OrgAdmin {
			s.fail(w, 403, "forbidden", "administrators only", nil)
			return
		}
		var in struct {
			Email, Name, Password string
			Admin                 bool
			Org                   string
		}
		if !s.decode(w, r, &in) {
			return
		}
		u, err := s.Svc.CreateUserAs(r.Context(), actorOf(r), in.Email, in.Name, in.Password, in.Admin, in.Org)
		s.replyStatus(w, 201, u, err)
	}))
	s.handle(mux, "GET /api/v1/orgs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListOrganizations(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	// the hosted edition: an organization's plan (the operator's) and its usage (the operator's, and the organization's own administrator's); they do not exist unless `hosted` is on
	s.handle(mux, "GET /api/v1/orgs/{id}/plan", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetOrgPlan(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "PUT /api/v1/orgs/{id}/plan", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.OrgPlan
		if s.decode(w, r, &in) {
			v, err := s.Svc.SetOrgPlan(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "DELETE /api/v1/orgs/{id}/plan", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.ClearOrgPlan(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "GET /api/v1/orgs/{id}/usage", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if h := r.URL.Query().Get("history"); h != "" {
			days, _ := strconv.Atoi(h)
			v, err := s.Svc.UsageHistory(r.Context(), actorOf(r), r.PathValue("id"), days)
			s.reply(w, v, err)
			return
		}
		v, err := s.Svc.OrgUsage(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	// billing (hosted only): prices, and invoices made from the month's usage and handed to the billing endpoint
	s.handle(mux, "PUT /api/v1/orgs/{id}/prices", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.Prices
		if s.decode(w, r, &in) {
			v, err := s.Svc.SetPrices(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "POST /api/v1/orgs/{id}/invoices", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Period string `json:"period"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.GenerateInvoice(r.Context(), actorOf(r), r.PathValue("id"), in.Period)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/orgs/{id}/invoices", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListInvoices(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "GET /api/v1/orgs/{id}/invoices/{number}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetInvoice(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("number"))
		if err == nil && r.URL.Query().Get("format") == "csv" {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="`+v.Number+`.csv"`)
			w.Write([]byte(domain.InvoiceCSV(v)))
			return
		}
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/orgs/{id}/invoices/{number}/send", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.SendInvoice(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("number"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/orgs/{id}/invoices/{number}/void", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.VoidInvoice(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("number"), in.Reason)
			s.reply(w, v, err)
		}
	}))
	// managed upgrades (hosted only): service terms, and the maintenance the operator announces to a tenant
	s.handle(mux, "PUT /api/v1/orgs/{id}/terms", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.ServiceTerms
		if s.decode(w, r, &in) {
			v, err := s.Svc.SetServiceTerms(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/orgs/{id}/terms", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.GetServiceTerms(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/orgs/{id}/maintenance", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.MaintenanceIn
		if s.decode(w, r, &in) {
			v, err := s.Svc.ScheduleMaintenance(r.Context(), actorOf(r), r.PathValue("id"), in)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/orgs/{id}/maintenance", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.ListMaintenance(r.Context(), actorOf(r), r.PathValue("id"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/orgs/{id}/maintenance/{nid}/cancel", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.CancelMaintenance(r.Context(), actorOf(r), r.PathValue("id"), r.PathValue("nid"), in.Reason)
			s.reply(w, v, err)
		}
	}))
	s.handle(mux, "POST /api/v1/orgs", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name          string `json:"name"`
			AdminEmail    string `json:"admin_email"`
			AdminName     string `json:"admin_name"`
			AdminPassword string `json:"admin_password"`
		}
		if s.decode(w, r, &in) {
			v, err := s.Svc.CreateOrganization(r.Context(), actorOf(r), in.Name, in.AdminEmail, in.AdminName, in.AdminPassword)
			s.replyStatus(w, 201, v, err)
		}
	}))
	s.handle(mux, "GET /api/v1/me/org", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.MyOrganization(r.Context(), actorOf(r))
		s.reply(w, v, err)
	}))
	// agent controls (administrators): the kill switch and per-agent pause take effect on the agent's next call
	s.handle(mux, "POST /api/v1/agents/kill", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ On bool }
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"killed": in.On}, s.Svc.KillAgents(r.Context(), actorOf(r), in.On))
	}))
	s.handle(mux, "POST /api/v1/agents/{id}/pause", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Paused bool }
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"paused": in.Paused}, s.Svc.PauseAgent(r.Context(), actorOf(r), r.PathValue("id"), in.Paused))
	}))
	s.handle(mux, "GET /api/v1/projects", s.auth(func(w http.ResponseWriter, r *http.Request) {
		ps, err := s.Svc.ListProjects(r.Context(), actorOf(r))
		s.reply(w, ps, err)
	}))
	s.handle(mux, "POST /api/v1/projects", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Key, Name string }
		if !s.decode(w, r, &in) {
			return
		}
		p, err := s.Svc.CreateProject(r.Context(), actorOf(r), in.Key, in.Name)
		s.replyStatus(w, 201, p, err)
	}))
	s.handle(mux, "POST /api/v1/projects/{key}/members", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			User string `json:"user"`
			Role string `json:"role"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.AddMember(r.Context(), actorOf(r), r.PathValue("key"), in.User, in.Role))
	}))

	s.handle(mux, "POST /api/v1/issues", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.CreateIn
		if !s.decode(w, r, &in) {
			return
		}
		is, err := s.Svc.CreateIssue(r.Context(), actorOf(r), in)
		if err == nil {
			w.Header().Set("Location", "/api/v1/issues/"+is.Key+"/view")
		}
		s.replyStatus(w, 201, is, err)
	}))
	s.handle(mux, "GET /api/v1/issues/{key}/view", s.auth(s.view))
	s.handle(mux, "PATCH /api/v1/issues/{key}", s.auth(s.patchIssue))
	s.handle(mux, "POST /api/v1/issues/{key}/transitions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Transition  string `json:"transition"`
			BaseVersion *int64 `json:"base_version"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		is, err := s.Svc.Transition(r.Context(), actorOf(r), r.PathValue("key"), in.Transition, in.BaseVersion)
		s.reply(w, is, err)
	}))
	s.handle(mux, "GET /api/v1/issues/{key}/history", s.auth(func(w http.ResponseWriter, r *http.Request) {
		h, err := s.Svc.History(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, h, err)
	}))

	s.handle(mux, "PUT /api/v1/issues/{key}/text/{field}", s.auth(s.saveText))
	s.handle(mux, "POST /api/v1/issues/{key}/text/{field}/patch", s.auth(s.patchText))
	s.handle(mux, "POST /api/v1/issues/{key}/text/{field}/replace", s.auth(s.replaceText))
	s.handle(mux, "GET /api/v1/issues/{key}/text/{field}/versions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		v, err := s.Svc.TextVersions(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("field"))
		s.reply(w, v, err)
	}))
	s.handle(mux, "POST /api/v1/issues/{key}/text/{field}/restore", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Seq int64 `json:"seq"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		res, err := s.Svc.RestoreText(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("field"), in.Seq)
		s.reply(w, res, err)
	}))

	s.handle(mux, "GET /api/v1/issues/{key}/drafts", s.auth(func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Svc.GetDrafts(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, d, err)
	}))
	s.handle(mux, "PUT /api/v1/issues/{key}/drafts/{field}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Body    string `json:"body"`
			BaseSeq int64  `json:"base_seq"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SaveDraft(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("field"), in.Body, in.BaseSeq))
	}))
	s.handle(mux, "DELETE /api/v1/issues/{key}/drafts/{field}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.DeleteDraft(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("field")))
	}))

	s.handle(mux, "POST /api/v1/issues/{key}/comments", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Body     string `json:"body"`
			Internal bool   `json:"internal"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		c, err := s.Svc.AddComment(r.Context(), actorOf(r), r.PathValue("key"), in.Body, in.Internal)
		s.replyStatus(w, 201, c, err)
	}))
	s.handle(mux, "PATCH /api/v1/issues/{key}/comments/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Body string `json:"body"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.EditComment(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("id"), in.Body))
	}))

	s.handle(mux, "POST /api/v1/issues/{key}/presence", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Field string `json:"field"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		p, changed, err := s.Svc.HeartbeatChanged(r.Context(), actorOf(r), r.PathValue("key"), in.Field)
		if err == nil && changed && s.Hub != nil { // clients on the socket learn of a person using the HTTP fallback
			s.Hub.BroadcastPresence(strings.ToUpper(r.PathValue("key")))
		}
		s.reply(w, p, err)
	}))
	s.handle(mux, "DELETE /api/v1/issues/{key}/presence", s.auth(func(w http.ResponseWriter, r *http.Request) {
		key := strings.ToUpper(r.PathValue("key"))
		if s.Svc.LeavePresence(key, actorOf(r).ID) && s.Hub != nil {
			s.Hub.BroadcastPresence(key)
		}
		s.json(w, 200, map[string]bool{"ok": true})
	}))
	if s.Search == nil {
		s.Search = &search.Postgres{DB: s.Svc.DB}
	}
	s.handle(mux, "GET /api/v1/bootstrap", s.auth(func(w http.ResponseWriter, r *http.Request) {
		ps, err1 := s.Svc.ListProjects(r.Context(), actorOf(r))
		us, err2 := s.Svc.ListUsersFor(r.Context(), actorOf(r))
		if err1 != nil || err2 != nil {
			s.fail(w, 500, "internal", "internal error", nil)
			return
		}
		s.json(w, 200, map[string]any{"me": actorOf(r), "projects": ps, "users": us})
	}))
	s.handle(mux, "GET /api/v1/admin/mail", s.auth(func(w http.ResponseWriter, r *http.Request) {
		l, err := s.Svc.MailLog(r.Context(), actorOf(r), 100)
		s.reply(w, l, err)
	}))
	if s.Hub != nil {
		s.handle(mux, "POST /api/v1/ws/ticket", s.auth(func(w http.ResponseWriter, r *http.Request) {
			s.json(w, 200, map[string]any{"ticket": s.Hub.Mint(actorOf(r)), "expires_in": 30})
		}))
		s.handle(mux, "GET /api/v1/ws", s.Hub.Handler())
	} else { // the realtime layer is switched off: say so plainly, so a page stops asking and carries on without it
		off := func(w http.ResponseWriter, r *http.Request) {
			s.fail(w, 404, "realtime_disabled", "live updates are switched off on this server", nil)
		}
		mux.HandleFunc("POST /api/v1/ws/ticket", off)
		mux.HandleFunc("GET /api/v1/ws", off)
	}
	s.handle(mux, "GET /api/v1/admin/fields", s.auth(func(w http.ResponseWriter, r *http.Request) {
		t, err := s.Svc.FieldTracking(r.Context(), actorOf(r))
		s.reply(w, t, err)
	}))
	s.handle(mux, "PUT /api/v1/admin/fields/{field}/history", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Track bool `json:"track"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetFieldTracking(r.Context(), actorOf(r), r.PathValue("field"), in.Track))
	}))
	s.handle(mux, "GET /api/v1/admin/pause", s.auth(func(w http.ResponseWriter, r *http.Request) {
		l, err := s.Svc.PauseStatus(r.Context(), actorOf(r))
		s.reply(w, l, err)
	}))
	s.handle(mux, "POST /api/v1/admin/pause", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.PauseIn
		if !s.decode(w, r, &in) {
			return
		}
		l, err := s.Svc.RequestPause(r.Context(), actorOf(r), in)
		s.reply(w, l, err)
	}))
	s.handle(mux, "DELETE /api/v1/admin/pause/{id}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, map[string]bool{"ok": true}, s.Svc.ReleasePause(r.Context(), actorOf(r), r.PathValue("id")))
	}))
	s.handle(mux, "POST /api/v1/issues/{key}/lifecycle", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in domain.LifecycleIn
		if !s.decode(w, r, &in) {
			return
		}
		plan, err := s.Svc.Lifecycle(r.Context(), actorOf(r), r.PathValue("key"), in)
		s.reply(w, plan, err)
	}))
	s.handle(mux, "GET /api/v1/projects/{key}/trash", s.auth(func(w http.ResponseWriter, r *http.Request) {
		t, err := s.Svc.Trash(r.Context(), actorOf(r), r.PathValue("key"))
		s.reply(w, t, err)
	}))
	s.handle(mux, "POST /api/v1/issues/{key}/links", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Type   string `json:"type"`
			Target string `json:"target"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.AddLink(r.Context(), actorOf(r), r.PathValue("key"), in.Type, in.Target))
	}))
	s.handle(mux, "PUT /api/v1/issues/{key}/legal-hold", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Hold bool `json:"hold"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		s.reply(w, map[string]bool{"ok": true}, s.Svc.SetLegalHold(r.Context(), actorOf(r), r.PathValue("key"), in.Hold))
	}))
	if s.MCP == nil {
		s.MCP = mcp.New(s.Svc, s.Search)
		if k, err := s.Svc.DerivedKey(context.Background(), "mcp-session"); err == nil {
			s.MCP.SessionKey = k // a session id any node of the cluster accepts
		}
	}
	for _, m := range []string{"POST", "GET", "DELETE"} {
		s.handle(mux, m+" /mcp", s.MCP.Handler())
	}
	s.registerSearch(mux)
	s.registerBoards(mux)
	s.registerSprints(mux)
	s.registerReports(mux)
	s.registerGit(mux)
	s.registerFields(mux)
	s.registerScreens(mux)
	s.registerWorkflows(mux)
	s.registerArchive(mux)
	s.registerAudit(mux)
	s.registerInbox(mux)
	s.registerAutomation(mux)
	s.registerSecurity(mux)
	s.registerSSO(mux)
	s.registerOAuth(mux)
	s.registerPrivacy(mux)
	s.registerGroups(mux)
	s.registerDashboards(mux)
	s.registerVersions(mux)
	s.registerApprovals(mux)
	s.registerPortal(mux)
	s.registerQueues(mux)
	s.registerQuota(mux)
	s.registerInboundMail(mux)
	s.registerGitAuto(mux)
	s.registerSCIM(mux)
	s.registerJira(mux)
	s.registerSearchAdmin(mux)
	s.registerAttachments(mux)
	s.registerExchange(mux)
	if s.Extra != nil {
		s.Extra(mux, s)
	}
	mux.Handle("/", web.Handler()) // the app shell and assets; API paths above take precedence
	return s.secure(mux)
}

// secure adds the baseline response headers (strict CSP, no sniffing, no framing) and Server-Timing.
func (s *Server) secure(next http.Handler) http.Handler {
	// direct uploads send bytes from the browser to the object store, so the page may connect to that origin and no other
	connect := "'self'"
	if s.Svc != nil && s.Svc.Blobs != nil {
		if o, ok := s.Svc.Blobs.Store.(interface{ Origin() string }); ok {
			connect += " " + o.Origin()
		}
	}
	frames := "'none'"
	if s.Svc != nil && len(s.Svc.DashboardIframeHosts) > 0 {
		frames = ""
		for _, h := range s.Svc.DashboardIframeHosts {
			frames += " https://" + h
		}
		frames = strings.TrimSpace(frames)
	}
	csp := "default-src 'self'; frame-src " + frames + "; connect-src " + connect + "; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("X-Tasklane-Server", s.Version)
			h.Set("X-Tasklane-Client-Min", strconv.Itoa(s.minClient()))
			if cv := r.Header.Get("X-Tasklane-Client"); cv != "" {
				if n, err := strconv.Atoi(cv); err == nil && n < s.minClient() {
					s.fail(w, 426, "client_too_old", "this page is out of date: reload it to continue (your drafts are kept)", map[string]any{"client": n, "min": s.minClient(), "reload": true})
					return
				}
			}
		}
		start := time.Now()
		next.ServeHTTP(&timingWriter{ResponseWriter: w, start: start}, r)
	})
}

type timingWriter struct {
	http.ResponseWriter
	start time.Time
	done  bool
}

func (t *timingWriter) WriteHeader(c int) {
	if !t.done {
		t.done = true
		t.Header().Set("Server-Timing", fmt.Sprintf("app;dur=%.1f", float64(time.Since(t.start).Microseconds())/1000))
	}
	t.ResponseWriter.WriteHeader(c)
}

// Unwrap and Hijack keep WebSocket upgrades and http.ResponseController working through the middleware.
func (t *timingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func (t *timingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := t.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("hijacking is not supported")
}

func (t *timingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *timingWriter) Write(b []byte) (int, error) {
	if !t.done {
		t.WriteHeader(200)
	}
	return t.ResponseWriter.Write(b)
}

// auth resolves the session cookie or bearer token. Cookie-authenticated unsafe requests must
// carry the X-Requested-With header, which a cross-site form or image cannot set (CSRF guard).
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, viaCookie := "", false
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			tok = strings.TrimPrefix(h, "Bearer ")
		} else if c, err := r.Cookie(cookieName); err == nil {
			tok, viaCookie = c.Value, true
		}
		a, err := s.Svc.Authenticate(r.Context(), tok)
		if err != nil {
			s.fail(w, 401, "unauthorized", "sign in required", nil)
			return
		}
		if viaCookie && r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Requested-With") != "tasklane" {
			s.fail(w, 403, "csrf", "missing X-Requested-With header", nil)
			return
		}
		// the organization's security policy: which networks may be used, and whether a second factor is compulsory (the pages that
		// set one up stay reachable)
		switch perr := s.Svc.EnforcePolicy(r.Context(), a, s.clientIP(r), tok, viaCookie, secondFactorExempt(r.URL.Path)); {
		case errors.Is(perr, domain.ErrIPNotAllowed):
			s.fail(w, 403, "ip_not_allowed", perr.Error(), nil)
			return
		case errors.Is(perr, domain.ErrSecondFactorRequired):
			s.fail(w, 403, "second_factor_required", perr.Error(), nil)
			return
		}
		if h, ok := r.Context().Value(holderKey{}).(*actorHolder); ok {
			h.a = a
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, a)))
	}
}

// clientIP is where a request really came from: the peer, or the address a trusted proxy reports.
func (s *Server) clientIP(r *http.Request) string {
	return s.Svc.ClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
}

func (s *Server) clientMeta(r *http.Request) domain.ClientMeta {
	return domain.ClientMeta{IP: s.clientIP(r), UserAgent: r.UserAgent()}
}

// secondFactorExempt lists what a person who must set up a second factor may still reach: who they are, signing out, and the
// pages that set a factor up.
func secondFactorExempt(path string) bool {
	switch path {
	case "/api/v1/me", "/api/v1/logout", "/api/v1/system/status":
		return true
	}
	return strings.HasPrefix(path, "/api/v1/me/2fa") || strings.HasPrefix(path, "/api/v1/me/passkeys")
}

func (s *Server) startSession(w http.ResponseWriter, tok string, u domain.User) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(s.SessionTTL.Seconds())})
	s.json(w, 200, u)
}

// signInFailed answers a refused sign-in: the same words whether the account exists or not, a lockout with when to retry.
func (s *Server) signInFailed(w http.ResponseWriter, err error) {
	var locked *domain.LockedError
	if errors.As(err, &locked) {
		secs := int(time.Until(locked.Until).Seconds()) + 1
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		s.fail(w, 429, "locked", "too many failed sign-ins; try again later", nil)
		return
	}
	s.fail(w, 401, "bad_credentials", "wrong email or password", nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !s.decode(w, r, &in) {
		return
	}
	res, err := s.Svc.LoginWith(r.Context(), in.Email, in.Password, s.SessionTTL, s.clientMeta(r))
	var mfa *domain.MFARequired
	if errors.As(err, &mfa) {
		out := map[string]any{"mfa_required": true, "mfa_token": mfa.Token, "totp": mfa.TOTP}
		if mfa.Key != nil {
			out["passkey"] = mfa.Key
		}
		s.json(w, 200, out)
		return
	}
	if err != nil {
		s.signInFailed(w, err)
		return
	}
	s.startSession(w, res.Token, res.User)
}

func (s *Server) loginMFA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"mfa_token"`
		Code  string `json:"code"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	res, err := s.Svc.LoginMFA(r.Context(), in.Token, in.Code, s.clientMeta(r))
	if err != nil {
		s.fail(w, 401, "bad_code", "that code is not right", nil)
		return
	}
	s.startSession(w, res.Token, res.User)
}

func (s *Server) loginPasskey(w http.ResponseWriter, r *http.Request) {
	var in domain.PasskeyAssertion
	if !s.decode(w, r, &in) {
		return
	}
	res, err := s.Svc.LoginWebAuthn(r.Context(), in, s.clientMeta(r))
	if err != nil {
		s.fail(w, 401, "bad_key", "that key could not sign you in", nil)
		return
	}
	s.startSession(w, res.Token, res.User)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.Svc.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	s.json(w, 200, map[string]bool{"ok": true})
}

func (s *Server) view(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.View(r.Context(), actorOf(r), r.PathValue("key"))
	if err != nil {
		s.reply(w, v, err)
		return
	}
	etag := fmt.Sprintf(`"%s-%d-%d-%d-%d"`, v.Issue.Key, v.Issue.Version, v.Issue.SummarySeq, v.Issue.DescSeq, len(v.Comments))
	if v.Issue.Updated.UnixNano() != 0 {
		etag = etag[:len(etag)-1] + fmt.Sprintf("-%d\"", v.Issue.Updated.UnixNano())
	}
	if len(v.Presence) == 0 {
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	s.json(w, 200, v)
}

func (s *Server) patchIssue(w http.ResponseWriter, r *http.Request) {
	var in struct {
		domain.Changes
		BaseVersion *int64 `json:"base_version"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	base, strict := in.BaseVersion, false
	if im := r.Header.Get("If-Match"); im != "" {
		n, err := strconv.ParseInt(strings.Trim(im, `"`), 10, 64)
		if err != nil {
			s.fail(w, 400, "bad_request", "If-Match must be the issue version", nil)
			return
		}
		base, strict = &n, true
	}
	is, err := s.Svc.UpdateFields(r.Context(), actorOf(r), r.PathValue("key"), base, strict, in.Changes)
	var vc *domain.VersionConflict
	if errors.As(err, &vc) && strict {
		s.fail(w, 412, "precondition_failed", err.Error(), vc)
		return
	}
	s.reply(w, is, err)
}

func (s *Server) saveText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body    string `json:"body"`
		BaseSeq *int64 `json:"base_seq"`
		Mode    string `json:"mode"`
		SaveID  string `json:"save_id"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	if in.BaseSeq == nil {
		s.fail(w, 400, "bad_request", "base_seq is required; use /replace for a bare overwrite", nil)
		return
	}
	res, err := s.Svc.SaveText(r.Context(), actorOf(r), r.PathValue("key"), domain.SaveIn{Field: r.PathValue("field"), Body: in.Body, BaseSeq: *in.BaseSeq, Mode: in.Mode, SaveID: in.SaveID})
	s.reply(w, res, err)
}

func (s *Server) patchText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Old string `json:"old_text"`
		New string `json:"new_text"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	res, err := s.Svc.PatchText(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("field"), in.Old, in.New)
	s.reply(w, res, err)
}

func (s *Server) replaceText(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Body string `json:"body"`
	}
	if !s.decode(w, r, &in) {
		return
	}
	var im *int64
	if h := r.Header.Get("If-Match"); h != "" {
		n, err := strconv.ParseInt(strings.Trim(h, `"`), 10, 64)
		if err != nil {
			s.fail(w, 400, "bad_request", "If-Match must be the field's text_seq", nil)
			return
		}
		im = &n
	}
	res, err := s.Svc.ReplaceText(r.Context(), actorOf(r), r.PathValue("key"), r.PathValue("field"), in.Body, im)
	s.reply(w, res, err)
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, 3<<20)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil && err != io.EOF {
		s.fail(w, 400, "bad_request", "invalid JSON body: "+err.Error(), nil)
		return false
	}
	return true
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, code int, kind, msg string, detail any) {
	s.json(w, code, map[string]any{"error": msg, "code": kind, "detail": detail})
}

func (s *Server) reply(w http.ResponseWriter, v any, err error) { s.replyStatus(w, 200, v, err) }

// replyStatus maps domain errors to HTTP: 400 invalid, 403 forbidden, 404 not found,
// 409 stale save or version (with the data the client needs to resolve it), 412 failed precondition.
func (s *Server) replyStatus(w http.ResponseWriter, ok int, v any, err error) {
	var (
		inv *domain.InvalidError
		tc  *domain.TextConflict
		vc  *domain.VersionConflict
		tp  *domain.TextPrecondition
		pm  *domain.PatchMismatch
		om  *domain.OffsetMismatch
		be  *domain.BlockedError
		rom *domain.ReadOnlyMirror
		ep  *domain.ErrPaused
		ce  *domain.ConflictError
		qe  *domain.QuotaError
		pl  *domain.PlanLimitError
		ue  *domain.UpstreamError
		nd  *domain.NotDownloadable
	)
	switch {
	case err == nil:
		s.json(w, ok, v)
	case errors.As(err, &inv):
		s.fail(w, 400, "invalid", inv.Msg, nil)
	case errors.As(err, &ue):
		s.fail(w, 502, "upstream", ue.Msg, nil)
	case errors.As(err, &pl):
		s.fail(w, 402, "plan_limit", pl.Error(), map[string]any{"resource": pl.Resource, "limit": pl.Limit, "used": pl.Used})
	case errors.As(err, &qe):
		s.fail(w, 413, "quota_exceeded", qe.Error(), map[string]int64{"quota": qe.Quota, "used": qe.Used})
	case errors.Is(err, domain.ErrForbidden):
		s.fail(w, 403, "forbidden", "you do not have permission to do this", nil)
	case errors.Is(err, domain.ErrNotFound):
		s.fail(w, 404, "not_found", "not found", nil)
	case errors.Is(err, domain.ErrGone):
		s.fail(w, 410, "gone", "this issue was deleted; its text stays in your draft", nil)
	case errors.As(err, &tc):
		s.fail(w, 409, "text_conflict", "the text was changed by "+tc.By+" while you were editing", tc)
	case errors.As(err, &vc):
		s.fail(w, 409, "version_conflict", err.Error(), vc)
	case errors.As(err, &tp):
		s.fail(w, 412, "precondition_failed", err.Error(), tp)
	case errors.As(err, &ep):
		w.Header().Set("Retry-After", strconv.Itoa(int(ep.RetryAfter.Seconds())+1))
		s.fail(w, 503, "paused", ep.Error(), map[string]any{"scope": ep.Scope, "retry_after": int(ep.RetryAfter.Seconds()) + 1})
	case errors.Is(err, domain.ErrDrainTimeout):
		s.fail(w, 409, "drain_timeout", err.Error(), nil)
	case errors.As(err, &be):
		s.fail(w, 409, "blocked", be.Error(), be.Plan)
	case errors.As(err, &rom):
		s.fail(w, 403, "read_only_mirror", rom.Error(), map[string]string{"project": rom.Project, "jira_url": rom.SiteURL})
	case errors.As(err, &ce):
		s.fail(w, 409, "conflict", ce.Error(), nil)
	case errors.As(err, &nd):
		s.fail(w, 423, "not_downloadable", "this file is "+nd.State+" and cannot be downloaded", nil)
	case isDiskFull(err):
		// nothing was saved and nothing is left half-written; the operator must free space. Said plainly, not as a generic fault.
		w.Header().Set("Retry-After", "30")
		s.fail(w, 507, "storage_full", "the server has run out of storage space: nothing was saved. Ask an administrator to free some, then try again", nil)
	case errors.Is(err, domain.ErrBusy):
		w.Header().Set("Retry-After", "2")
		s.fail(w, 503, "busy", "too many uploads in progress; retry shortly", nil)
	case errors.As(err, &om):
		w.Header().Set("Upload-Offset", strconv.FormatInt(om.Offset, 10))
		s.fail(w, 409, "offset_mismatch", err.Error(), om)
	case errors.As(err, &pm):
		s.fail(w, 409, "patch_mismatch", err.Error(), pm)
	default:
		s.fail(w, 500, "internal", "internal error", nil)
	}
}

func asErr[T any](err error, target *T) bool { return errors.As(err, target) }

// isDiskFull recognises a full disk or an exhausted quota, from the filesystem (the blob store, staging files) or from PostgreSQL
// (SQLSTATE 53100 disk_full, 53200 out_of_memory is not this).
func isDiskFull(err error) bool {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "53100"
}
