// Package rt is the realtime gateway: one WebSocket per tab carrying presence and change events for the issues the
// person has open. It carries no text synchronization, and nothing depends on it: when the socket is down pages
// refetch, and saves still conflict-check by version (SPEC 4.5).
package rt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/larytet/tasklane/internal/domain"
)

const (
	maxFrame     = 4096
	maxSubs      = 50
	ticketTTL    = 30 * time.Second
	authCacheTTL = 5 * time.Second
	sendBuffer   = 64
)

type Hub struct {
	Svc            *domain.Service
	OriginPatterns []string // extra allowed origins; the request's own host is always allowed
	Now            func() time.Time
	MsgsPerSecond  float64

	mu       sync.Mutex
	conns    map[*conn]struct{}
	subs     map[string]map[*conn]struct{} // issue key -> subscribers
	tickets  map[string]ticket
	draining bool
	Sent     int64 // messages written to clients, for the "no per-keystroke traffic" assertion
}

type ticket struct {
	actor domain.Actor
	exp   time.Time
}

func New(svc *domain.Service) *Hub {
	h := &Hub{Svc: svc, Now: time.Now, MsgsPerSecond: 30, conns: map[*conn]struct{}{}, subs: map[string]map[*conn]struct{}{}, tickets: map[string]ticket{}}
	svc.Publish = h.Publish
	return h
}

// Mint issues a single-use, short-lived ticket for the WebSocket handshake.
func (h *Hub) Mint(a domain.Actor) string {
	b := make([]byte, 24)
	rand.Read(b)
	t := "wt_" + hex.EncodeToString(b)
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.Now()
	for k, v := range h.tickets { // expired tickets do not accumulate
		if now.After(v.exp) {
			delete(h.tickets, k)
		}
	}
	h.tickets[t] = ticket{actor: a, exp: now.Add(ticketTTL)}
	return t
}

func (h *Hub) redeem(t string) (domain.Actor, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tk, ok := h.tickets[t]
	delete(h.tickets, t) // single use, valid or not
	if !ok || h.Now().After(tk.exp) {
		return domain.Actor{}, false
	}
	return tk.actor, true
}

type conn struct {
	h      *Hub
	ws     *websocket.Conn
	actor  domain.Actor
	out    chan []byte
	cancel context.CancelFunc
	mu     sync.Mutex
	subs   map[string]bool
	authAt map[string]time.Time // issue -> last successful authorization
	tokens float64
	last   time.Time
}

var channelRe = regexp.MustCompile(`^issue:([A-Za-z][A-Za-z0-9]{1,9}-[0-9]{1,9})$`)

type inbound struct {
	Op      string `json:"op"`
	Channel string `json:"channel"`
	Field   string `json:"field"`
}

// Handler upgrades GET /api/v1/ws?ticket=... Without a valid ticket the handshake is refused before any upgrade.
func (h *Hub) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.Draining() { // this node is going away: the client should connect to another one
			w.Header().Set("Retry-After", "1")
			http.Error(w, "this server is restarting", http.StatusServiceUnavailable)
			return
		}
		actor, ok := h.redeem(r.URL.Query().Get("ticket"))
		if !ok {
			http.Error(w, "invalid or expired ticket", http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.OriginPatterns}) // same-origin unless listed
		if err != nil {
			return // Accept already answered (403 for a bad origin)
		}
		ws.SetReadLimit(maxFrame)
		ctx, cancel := context.WithCancel(context.Background())
		c := &conn{h: h, ws: ws, actor: actor, out: make(chan []byte, sendBuffer), cancel: cancel, subs: map[string]bool{}, authAt: map[string]time.Time{}, tokens: 60, last: h.Now()}
		h.mu.Lock()
		h.conns[c] = struct{}{}
		h.mu.Unlock()
		go c.writer(ctx)
		c.reader(ctx)
		c.close()
	}
}

func (c *conn) writer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := c.ws.Write(wctx, websocket.MessageText, m)
			cancel()
			if err != nil {
				c.cancel()
				return
			}
			c.h.mu.Lock()
			c.h.Sent++
			c.h.mu.Unlock()
		}
	}
}

// send queues a message; a client that cannot keep up is disconnected (events are best effort, the page refetches).
func (c *conn) send(v any) {
	b, _ := json.Marshal(v)
	select {
	case c.out <- b:
	default:
		c.ws.Close(websocket.StatusPolicyViolation, "too slow")
		c.cancel()
	}
}

func (c *conn) allow() bool { // token bucket
	now := c.h.Now()
	c.tokens += now.Sub(c.last).Seconds() * c.h.MsgsPerSecond
	if c.tokens > 60 {
		c.tokens = 60
	}
	c.last = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

func (c *conn) reader(ctx context.Context) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			return // closed, or a frame above the limit (the library closes with 1009)
		}
		if typ != websocket.MessageText {
			c.ws.Close(websocket.StatusUnsupportedData, "text frames only")
			return
		}
		if !c.allow() {
			c.ws.Close(websocket.StatusPolicyViolation, "rate limit exceeded")
			return
		}
		var m inbound
		if err := json.Unmarshal(data, &m); err != nil {
			c.ws.Close(websocket.StatusInvalidFramePayloadData, "malformed message")
			return
		}
		c.handle(ctx, m)
	}
}

func denied(ch string) map[string]any {
	return map[string]any{"type": "error", "code": "denied", "channel": ch}
}

func (c *conn) handle(ctx context.Context, m inbound) {
	switch m.Op {
	case "ping":
		c.send(map[string]any{"type": "pong"})
	case "sub":
		g := channelRe.FindStringSubmatch(m.Channel)
		if g == nil {
			c.send(map[string]any{"type": "error", "code": "bad_channel", "channel": m.Channel})
			return
		}
		key := strings.ToUpper(g[1])
		c.mu.Lock()
		n := len(c.subs)
		c.mu.Unlock()
		if n >= maxSubs && !c.subs[key] {
			c.send(map[string]any{"type": "error", "code": "too_many_subscriptions"})
			return
		}
		// not found and forbidden answer identically, so existence is not revealed
		if !c.h.Svc.CanRead(ctx, c.actor, key) {
			c.send(denied(m.Channel))
			return
		}
		c.mu.Lock()
		c.subs[key] = true
		c.authAt[key] = c.h.Now()
		c.mu.Unlock()
		c.h.mu.Lock()
		if c.h.subs[key] == nil {
			c.h.subs[key] = map[*conn]struct{}{}
		}
		c.h.subs[key][c] = struct{}{}
		c.h.mu.Unlock()
		c.send(map[string]any{"type": "subscribed", "channel": m.Channel})
		c.send(c.presenceMsg(key))
	case "unsub":
		g := channelRe.FindStringSubmatch(m.Channel)
		if g != nil {
			c.unsubscribe(strings.ToUpper(g[1]))
		}
	case "presence":
		key := strings.ToUpper(strings.TrimPrefix(m.Channel, "issue:"))
		c.mu.Lock()
		ok := c.subs[key]
		c.mu.Unlock()
		if !ok {
			c.send(denied(m.Channel))
			return
		}
		_, changed, err := c.h.Svc.HeartbeatChanged(ctx, c.actor, key, m.Field)
		if err != nil {
			c.unsubscribe(key)
			c.send(denied(m.Channel))
			return
		}
		if changed { // heartbeats that change nothing cause no traffic
			c.h.BroadcastPresence(key)
		}
	default:
		c.send(map[string]any{"type": "error", "code": "unknown_op"})
	}
}

func (c *conn) presenceMsg(key string) map[string]any {
	return map[string]any{"type": "presence", "issue": key, "presence": c.h.Svc.Presence.List(key, c.actor.ID)}
}

func (c *conn) unsubscribe(key string) {
	c.mu.Lock()
	delete(c.subs, key)
	delete(c.authAt, key)
	c.mu.Unlock()
	c.h.mu.Lock()
	delete(c.h.subs[key], c)
	c.h.mu.Unlock()
}

func (c *conn) close() {
	c.cancel()
	c.ws.Close(websocket.StatusNormalClosure, "")
	c.mu.Lock()
	keys := make([]string, 0, len(c.subs))
	for k := range c.subs {
		keys = append(keys, k)
	}
	c.mu.Unlock()
	for _, k := range keys {
		c.unsubscribe(k)
	}
	c.h.mu.Lock()
	delete(c.h.conns, c)
	c.h.mu.Unlock()
	// presence leaves with the last tab of that user on the issue
	for _, k := range keys {
		if !c.h.userSubscribed(c.actor.ID, k) && c.h.Svc.Presence.Leave(k, c.actor.ID) {
			c.h.BroadcastPresence(k)
		}
	}
}

func (h *Hub) userSubscribed(userID, key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs[key] {
		if c.actor.ID == userID {
			return true
		}
	}
	return false
}

// authorized re-checks a subscriber's access, cached for a few seconds so revocation takes effect quickly.
func (c *conn) authorized(ctx context.Context, key string) bool {
	c.mu.Lock()
	at, ok := c.authAt[key]
	c.mu.Unlock()
	if !ok {
		return false
	}
	if c.h.Now().Sub(at) < authCacheTTL {
		return true
	}
	if !c.h.Svc.CanRead(ctx, c.actor, key) {
		c.unsubscribe(key)
		c.send(denied("issue:" + key))
		return false
	}
	c.mu.Lock()
	c.authAt[key] = c.h.Now()
	c.mu.Unlock()
	return true
}

func (h *Hub) subscribers(key string) []*conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*conn, 0, len(h.subs[key]))
	for c := range h.subs[key] {
		out = append(out, c)
	}
	return out
}

// BroadcastPresence sends each subscriber the current set of others on the issue (never their own entry).
func (h *Hub) BroadcastPresence(key string) {
	for _, c := range h.subscribers(key) {
		if c.authorized(context.Background(), key) {
			c.send(c.presenceMsg(key))
		}
	}
}

// Publish fans a committed change out to the issue's authorized subscribers.
func (h *Hub) Publish(e domain.Event) {
	for _, c := range h.subscribers(e.Issue) {
		if c.authorized(context.Background(), e.Issue) {
			c.send(map[string]any{"type": "event", "event": e})
		}
	}
}

// SweepLoop expires presence that missed its heartbeats and tells the people on those issues.
func (h *Hub) SweepLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, k := range h.Svc.Presence.Sweep() {
				h.BroadcastPresence(k)
			}
		}
	}
}

// Draining says whether Drain has been called.
func (h *Hub) Draining() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.draining
}

// Drain is the realtime side of a graceful stop (SPEC 17.8): new connections are refused (so a load balancer's next try lands on
// another node) and every open one is told to reconnect after a random delay within spread, so that a whole node's clients do not
// come back in the same instant, and is then closed. The pages work without the socket in the meantime.
func (h *Hub) Drain(spread time.Duration) {
	h.mu.Lock()
	h.draining = true
	cs := make([]*conn, 0, len(h.conns))
	for c := range h.conns {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range cs {
		wg.Add(1)
		go func(c *conn) {
			defer wg.Done()
			after := int64(0)
			if spread > 0 {
				var b [4]byte
				rand.Read(b[:])
				after = int64(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3])) % spread.Milliseconds()
			}
			c.send(map[string]any{"type": "reconnect", "after_ms": after})
			for i := 0; i < 50 && len(c.out) > 0; i++ { // give the message a moment to be written
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			c.ws.Close(websocket.StatusGoingAway, "this server is restarting")
			c.cancel()
		}(c)
	}
	wg.Wait()
}

// Stats for tests and metrics.
func (h *Hub) Stats() (conns int, sent int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns), h.Sent
}
