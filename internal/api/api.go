// Package api is the HTTP layer: JSON endpoints for the UI and for API clients.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mcp"
	"github.com/larytet/tasklane/internal/rt"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/web"
)

type Server struct {
	Svc         *domain.Service
	SessionTTL  time.Duration
	Secure      bool // set the Secure flag on the session cookie
	Extra       func(mux *http.ServeMux, s *Server)
	Ready       func(ctx context.Context) error
	Search      search.Searcher
	SearchAdmin *search.Admin
	BlobOrigin  string // optional separate origin for signed file links
	Hub         *rt.Hub
	MCP         *mcp.Server
	routes      []string
}

// handle registers a route and records its pattern, so a test can prove that every route is covered by the authorization matrix.
func (s *Server) handle(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	s.routes = append(s.routes, pattern)
	mux.HandleFunc(pattern, h)
}

// Routes lists the registered patterns (method and path), for the authorization matrix test.
func (s *Server) Routes() []string { s.Handler(); return append([]string(nil), s.routes...) }

type ctxKey struct{}

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
		if s.Ready != nil {
			if err := s.Ready(r.Context()); err != nil {
				http.Error(w, "not ready", 503)
				return
			}
		}
		w.Write([]byte("ready"))
	})
	s.handle(mux, "POST /api/v1/login", s.login)
	s.handle(mux, "POST /api/v1/logout", s.auth(s.logout))
	s.handle(mux, "GET /api/v1/me", s.auth(func(w http.ResponseWriter, r *http.Request) { s.json(w, 200, actorOf(r)) }))
	s.handle(mux, "GET /api/v1/users", s.auth(func(w http.ResponseWriter, r *http.Request) {
		us, err := s.Svc.ListUsers(r.Context())
		s.reply(w, us, err)
	}))

	s.handle(mux, "POST /api/v1/users", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if !actorOf(r).Admin {
			s.fail(w, 403, "forbidden", "administrators only", nil)
			return
		}
		var in struct {
			Email, Name, Password string
			Admin                 bool
		}
		if !s.decode(w, r, &in) {
			return
		}
		u, err := s.Svc.CreateUser(r.Context(), in.Email, in.Name, in.Password, in.Admin)
		s.replyStatus(w, 201, u, err)
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
		if s.Svc.Presence.Leave(key, actorOf(r).ID) && s.Hub != nil {
			s.Hub.BroadcastPresence(key)
		}
		s.json(w, 200, map[string]bool{"ok": true})
	}))
	if s.Search == nil {
		s.Search = &search.Postgres{DB: s.Svc.DB}
	}
	s.handle(mux, "GET /api/v1/bootstrap", s.auth(func(w http.ResponseWriter, r *http.Request) {
		ps, err1 := s.Svc.ListProjects(r.Context(), actorOf(r))
		us, err2 := s.Svc.ListUsers(r.Context())
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
	csp := "default-src 'self'; connect-src " + connect + "; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
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
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, a)))
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !s.decode(w, r, &in) {
		return
	}
	tok, u, err := s.Svc.Login(r.Context(), in.Email, in.Password, s.SessionTTL)
	if err != nil {
		s.fail(w, 401, "bad_credentials", "wrong email or password", nil)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(s.SessionTTL.Seconds())})
	s.json(w, 200, u)
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
		nd  *domain.NotDownloadable
	)
	switch {
	case err == nil:
		s.json(w, ok, v)
	case errors.As(err, &inv):
		s.fail(w, 400, "invalid", inv.Msg, nil)
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
