package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

// Resource subscriptions (MCP-15). A client subscribes to `tasklane://issue/KEY`, `tasklane://issue/KEY/development` or
// `tasklane://search?q=<TQL>` with `resources/subscribe`, and receives `notifications/resources/updated` on the session's event stream
// (a GET with the session id and `Accept: text/event-stream`; the stdio proxy opens it for the client). A notification says that the
// resource changed, not what it now holds: the client reads it again, and that read is authorized like any other. Before every
// notification the token is checked again and the person's access to the issue, so a revoked token or a lost permission ends the
// stream and nothing is told to someone who may no longer know.

const (
	maxSubscriptions = 100
	streamBuffer     = 256
	keepAliveEvery   = 20 * time.Second
)

type stream struct {
	agent  string
	mu     sync.Mutex
	subs   map[string]bool
	events chan domain.Event
	busy   bool // a GET is attached
}

func newStream(agent string) *stream {
	return &stream{agent: agent, subs: map[string]bool{}, events: make(chan domain.Event, streamBuffer)}
}

var reIssueURI = regexp.MustCompile(`^tasklane://issue/([A-Za-z][A-Za-z0-9]{1,9}-[0-9]{1,9})(/development)?$`)

// parseSubURI says what a subscription URI names: an issue (and which view of it) or a search.
func parseSubURI(uri string) (key string, dev bool, query string, isSearch bool, err error) {
	if m := reIssueURI.FindStringSubmatch(uri); m != nil {
		return strings.ToUpper(m[1]), m[2] != "", "", false, nil
	}
	if strings.HasPrefix(uri, "tasklane://search?") {
		u, perr := url.Parse(uri)
		if perr != nil || strings.TrimSpace(u.Query().Get("q")) == "" {
			return "", false, "", false, fmt.Errorf("a search resource needs a query: tasklane://search?q=<TQL>")
		}
		return "", false, u.Query().Get("q"), true, nil
	}
	return "", false, "", false, fmt.Errorf("unknown resource: %s", uri)
}

// onEvent is the listener the domain calls after each committed change. It only queues: it must never wait.
func (s *Server) onEvent(e domain.Event) {
	s.mu.Lock()
	sts := make([]*stream, 0, len(s.streams))
	for _, st := range s.streams {
		sts = append(sts, st)
	}
	s.mu.Unlock()
	for _, st := range sts {
		st.mu.Lock()
		has := len(st.subs) > 0
		st.mu.Unlock()
		if !has {
			continue
		}
		select {
		case st.events <- e:
		default: // a client that does not keep up loses notifications, and finds out when it reads: events are hints, not the data
		}
	}
}

func (s *Server) subscribe(w http.ResponseWriter, r *http.Request, req rpcReq, actor domain.Actor, scopes []string, sid string, on bool) {
	var p struct {
		URI string `json:"uri"`
	}
	json.Unmarshal(req.Params, &p)
	fail := func(msg string) { s.writeJSON(w, 200, rpcError(req.ID, -32602, msg)) }
	s.mu.Lock()
	st := s.streams[sid]
	s.mu.Unlock()
	if sid == "" || st == nil {
		fail("subscribe within a session: initialize first and send Mcp-Session-Id")
		return
	}
	key, _, q, isSearch, err := parseSubURI(p.URI)
	if err != nil {
		fail(err.Error())
		return
	}
	if !hasScope(scopes, "read") {
		fail("denied: the token lacks the read scope")
		return
	}
	if on {
		if isSearch {
			if _, qerr := s.searchOne(r.Context(), actor, q, "", 1); qerr != nil {
				fail(errorText(qerr))
				return
			}
		} else if !s.Svc.CanRead(r.Context(), actor, key) {
			fail("not found")
			return
		}
		st.mu.Lock()
		if !st.subs[p.URI] && len(st.subs) >= maxSubscriptions {
			st.mu.Unlock()
			fail(fmt.Sprintf("at most %d subscriptions per session", maxSubscriptions))
			return
		}
		st.subs[p.URI] = true
		st.mu.Unlock()
	} else {
		st.mu.Lock()
		delete(st.subs, p.URI)
		st.mu.Unlock()
	}
	s.writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
}

func hasScope(scopes []string, want string) bool {
	for _, sc := range scopes {
		if sc == want {
			return true
		}
	}
	return false
}

// searchOne runs a TQL query as the actor, optionally restricted to one issue, and returns the rows.
func (s *Server) searchOne(ctx context.Context, a domain.Actor, q, onlyKey string, limit int) (*search.Result, error) {
	all, rd, wr, err := s.Svc.SearchScope(ctx, a)
	if err != nil {
		return nil, err
	}
	if onlyKey != "" {
		base, order := q, ""
		if i := strings.Index(strings.ToUpper(q), " ORDER BY "); i >= 0 {
			base, order = q[:i], q[i:]
		}
		q = "(" + base + ") AND key = " + onlyKey + order
	}
	res, err := s.Search.Search(ctx, search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: s.Now}, q, search.Page{Limit: limit})
	if err != nil {
		if te, ok := search.IsQueryError(err); ok {
			return res, &domain.InvalidError{Msg: "query error: " + te.Error()}
		}
	}
	return res, err
}

// streamHandler serves the GET of Streamable HTTP: the session's server-sent events.
func (s *Server) streamHandler(w http.ResponseWriter, r *http.Request, actor domain.Actor, tok string) {
	sid := r.Header.Get("Mcp-Session-Id")
	s.mu.Lock()
	sess, okSess := s.sessions[sid]
	st := s.streams[sid]
	s.mu.Unlock()
	if sid == "" {
		http.Error(w, "the event stream belongs to a session: send Mcp-Session-Id from initialize", http.StatusBadRequest)
		return
	}
	if !okSess || st == nil {
		http.Error(w, "unknown session: initialize again", http.StatusNotFound)
		return
	}
	if sess.agent != actor.ID {
		http.Error(w, "unknown session: initialize again", http.StatusNotFound) // another person's session is not even acknowledged
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		http.Error(w, "send Accept: text/event-stream", http.StatusNotAcceptable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported here", http.StatusInternalServerError)
		return
	}
	st.mu.Lock()
	if st.busy {
		st.mu.Unlock()
		http.Error(w, "this session already has an event stream", http.StatusConflict)
		return
	}
	st.busy = true
	st.mu.Unlock()
	defer func() { st.mu.Lock(); st.busy = false; st.mu.Unlock() }()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": stream open\n\n")
	fl.Flush()
	tick := time.NewTicker(keepAliveEvery)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case e := <-st.events:
			if !s.deliver(r.Context(), w, fl, st, tok, e) {
				return
			}
		}
	}
}

// deliver turns one change into the notifications it causes. It returns false when the stream must end (the token no longer works).
func (s *Server) deliver(ctx context.Context, w http.ResponseWriter, fl http.Flusher, st *stream, tok string, e domain.Event) bool {
	actor, scopes, err := s.Svc.AuthenticateAgent(ctx, tok) // revoked, expired, paused or killed: nothing more is said
	if err != nil || !hasScope(scopes, "read") {
		return false
	}
	st.mu.Lock()
	uris := make([]string, 0, len(st.subs))
	for u := range st.subs {
		uris = append(uris, u)
	}
	st.mu.Unlock()
	canRead := -1
	for _, u := range uris {
		key, dev, q, isSearch, err := parseSubURI(u)
		if err != nil {
			continue
		}
		devEvent := len(e.Fields) == 1 && e.Fields[0] == "development" // a change to the code linked to the issue, not to the issue
		if !isSearch && dev != devEvent {
			continue // the development view changes with its own events, the issue with the others
		}
		hit := false
		if isSearch {
			if res, err := s.searchOne(ctx, actor, q, strings.ToUpper(e.Issue), 1); err == nil && len(res.Rows) > 0 {
				hit = true
			}
		} else if key == strings.ToUpper(e.Issue) {
			if canRead < 0 {
				canRead = 0
				if s.Svc.CanRead(ctx, actor, key) {
					canRead = 1
				}
			}
			hit = canRead == 1 // an issue the person can no longer see is not announced
		}
		if !hit {
			continue
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/resources/updated", "params": map[string]any{"uri": u}})
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		fl.Flush()
	}
	return true
}
