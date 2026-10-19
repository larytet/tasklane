// Package redis is the small part of the Redis protocol the cluster needs: publish and subscribe over one connection each, with
// authentication, TLS, and reconnecting with backoff. It is used for what must reach every node quickly and is allowed to be lost (live
// change events and presence); nothing stored depends on it, and a node whose connection is down keeps working alone. It speaks to
// Redis and to Valkey (RESP2).
package redis

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	Addr     string
	User     string
	Password string
	DB       int
	TLS      *tls.Config
	Timeout  time.Duration // dial and command timeout (default 3 s)
}

// ParseURL reads redis://[user:password@]host[:port][/db] and rediss:// (TLS).
func ParseURL(raw string) (Options, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Host == "" {
		return Options{}, fmt.Errorf("redis_url must look like redis://host:6379 or rediss://user:password@host:6379/0")
	}
	o := Options{Addr: u.Host, Timeout: 3 * time.Second}
	if !strings.Contains(u.Host, ":") {
		o.Addr += ":6379"
	}
	if u.User != nil {
		o.User = u.User.Username()
		o.Password, _ = u.User.Password()
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		if o.DB, err = strconv.Atoi(p); err != nil || o.DB < 0 {
			return Options{}, fmt.Errorf("redis_url: the database number %q is not a number", p)
		}
	}
	if u.Scheme == "rediss" {
		o.TLS = &tls.Config{ServerName: strings.Split(u.Host, ":")[0], MinVersion: tls.VersionTLS12}
	}
	return o, nil
}

type conn struct {
	c net.Conn
	r *bufio.Reader
	t time.Duration
}

func dial(ctx context.Context, o Options) (*conn, error) {
	t := o.Timeout
	if t <= 0 {
		t = 3 * time.Second
	}
	d := net.Dialer{Timeout: t}
	c, err := d.DialContext(ctx, "tcp", o.Addr)
	if err != nil {
		return nil, err
	}
	if o.TLS != nil {
		tc := tls.Client(c, o.TLS)
		tc.SetDeadline(time.Now().Add(t))
		if err := tc.Handshake(); err != nil {
			c.Close()
			return nil, err
		}
		c = tc
	}
	cn := &conn{c: c, r: bufio.NewReaderSize(c, 64<<10), t: t}
	if o.Password != "" {
		args := []string{"AUTH", o.Password}
		if o.User != "" {
			args = []string{"AUTH", o.User, o.Password}
		}
		if _, err := cn.do(args...); err != nil {
			c.Close()
			return nil, err
		}
	}
	if o.DB != 0 {
		if _, err := cn.do("SELECT", strconv.Itoa(o.DB)); err != nil {
			c.Close()
			return nil, err
		}
	}
	return cn, nil
}

func (c *conn) write(args ...string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	c.c.SetWriteDeadline(time.Now().Add(c.t))
	_, err := io.WriteString(c.c, b.String())
	return err
}

type reply struct {
	kind  byte
	str   string
	n     int64
	items []reply
}

var errNull = errors.New("null reply")

func (c *conn) read(deadline time.Time) (reply, error) {
	c.c.SetReadDeadline(deadline)
	return readReply(c.r, 0)
}

func readReply(r *bufio.Reader, depth int) (reply, error) {
	if depth > 3 {
		return reply{}, errors.New("redis: reply nested too deeply")
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return reply{}, err
	}
	if len(line) < 3 {
		return reply{}, errors.New("redis: short reply")
	}
	kind, body := line[0], strings.TrimRight(line[1:], "\r\n")
	switch kind {
	case '+':
		return reply{kind: kind, str: body}, nil
	case '-':
		return reply{kind: kind, str: body}, errors.New("redis: " + body)
	case ':':
		n, _ := strconv.ParseInt(body, 10, 64)
		return reply{kind: kind, n: n}, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n > 16<<20 {
			return reply{}, errors.New("redis: bad bulk length")
		}
		if n < 0 {
			return reply{kind: kind}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return reply{}, err
		}
		return reply{kind: kind, str: string(buf[:n])}, nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n > 1024 {
			return reply{}, errors.New("redis: bad array length")
		}
		out := reply{kind: kind}
		for i := 0; i < n; i++ {
			x, err := readReply(r, depth+1)
			if err != nil {
				return reply{}, err
			}
			out.items = append(out.items, x)
		}
		return out, nil
	}
	return reply{}, fmt.Errorf("redis: unexpected reply type %q", kind)
}

func (c *conn) do(args ...string) (reply, error) {
	if err := c.write(args...); err != nil {
		return reply{}, err
	}
	return c.read(time.Now().Add(c.t))
}

// Status is what a node can tell about its link to Redis.
type Status struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since"`
	LastError string    `json:"last_error,omitempty"`
	Dropped   int64     `json:"dropped"`   // messages not sent because the link was down or its queue was full
	Received  int64     `json:"received"`  // messages from other nodes
	Published int64     `json:"published"` // messages sent
}

type msg struct {
	channel string
	payload []byte
	at      time.Time
}

// Bus publishes to and subscribes on Redis channels. Publishing never blocks and never fails the caller: a message is queued for a sender
// goroutine, and one that cannot be sent promptly is dropped and counted.
type Bus struct {
	opt  Options
	subs map[string]func([]byte)
	q    chan msg

	mu        sync.Mutex
	subUp     bool
	pubUp     bool
	since     time.Time
	lastErr   string
	dropped   atomic.Int64
	received  atomic.Int64
	published atomic.Int64
	// MaxAge is how old a queued message may be when it is finally sent (default 2 s): live events that late are worse than none, because
	// pages catch up from the database anyway.
	MaxAge time.Duration
}

func New(o Options) *Bus {
	return &Bus{opt: o, subs: map[string]func([]byte){}, q: make(chan msg, 1024), MaxAge: 2 * time.Second}
}

// Subscribe registers a handler for a channel; call before Run.
func (b *Bus) Subscribe(channel string, f func([]byte)) { b.subs[channel] = f }

// Publish queues a message and returns at once.
func (b *Bus) Publish(channel string, payload []byte) {
	select {
	case b.q <- msg{channel, payload, time.Now()}:
	default:
		b.dropped.Add(1)
	}
}

func (b *Bus) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{Connected: b.subUp && b.pubUp, Since: b.since, LastError: b.lastErr, Dropped: b.dropped.Load(), Received: b.received.Load(), Published: b.published.Load()}
}

func (b *Bus) set(sub, pub *bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	was := b.subUp && b.pubUp
	if sub != nil {
		b.subUp = *sub
	}
	if pub != nil {
		b.pubUp = *pub
	}
	if err != nil {
		b.lastErr = err.Error()
	}
	if now := b.subUp && b.pubUp; now != was {
		b.since = time.Now()
		if now {
			b.lastErr = ""
		}
	}
}

func backoff(n int) time.Duration {
	d := 100 * time.Millisecond << max(0, min(n, 5))
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// Run keeps both connections up until ctx ends.
func (b *Bus) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.runSub(ctx) }()
	go func() { defer wg.Done(); b.runPub(ctx) }()
	wg.Wait()
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func (b *Bus) runSub(ctx context.Context) {
	f := false
	for fails := 0; ctx.Err() == nil; fails++ {
		err := b.subOnce(ctx, &fails)
		b.set(&f, nil, err)
		if !sleep(ctx, backoff(fails)) {
			return
		}
	}
}

func (b *Bus) subOnce(ctx context.Context, fails *int) error {
	c, err := dial(ctx, b.opt)
	if err != nil {
		return err
	}
	defer c.c.Close()
	go func() { <-ctx.Done(); c.c.Close() }()
	chans := make([]string, 0, len(b.subs))
	for ch := range b.subs {
		chans = append(chans, ch)
	}
	if len(chans) == 0 {
		<-ctx.Done()
		return nil
	}
	if err := c.write(append([]string{"SUBSCRIBE"}, chans...)...); err != nil {
		return err
	}
	for range chans { // one confirmation per channel
		if _, err := c.read(time.Now().Add(c.t)); err != nil {
			return err
		}
	}
	up := true
	b.set(&up, nil, nil)
	*fails = -1 // the next backoff starts over
	for {
		// a quiet channel is normal: a ping every so often finds a dead connection that never said so
		r, err := c.read(time.Now().Add(15 * time.Second))
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			if err := c.write("PING"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if len(r.items) == 3 && r.items[0].str == "message" {
			b.received.Add(1)
			if f := b.subs[r.items[1].str]; f != nil {
				f([]byte(r.items[2].str))
			}
		}
	}
}

func (b *Bus) runPub(ctx context.Context) {
	for fails := 0; ctx.Err() == nil; fails++ {
		err := b.pubOnce(ctx, &fails)
		f := false
		b.set(nil, &f, err)
		b.drainStale()
		if !sleep(ctx, backoff(fails)) {
			return
		}
	}
}

// drainStale discards what accumulated while the link was down: it would arrive late and out of order with what comes next.
func (b *Bus) drainStale() {
	for {
		select {
		case <-b.q:
			b.dropped.Add(1)
		default:
			return
		}
	}
}

func (b *Bus) pubOnce(ctx context.Context, fails *int) error {
	c, err := dial(ctx, b.opt)
	if err != nil {
		return err
	}
	defer c.c.Close()
	if _, err := c.do("PING"); err != nil {
		return err
	}
	up := true
	b.set(nil, &up, nil)
	*fails = -1
	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-b.q:
			if time.Since(m.at) > b.MaxAge {
				b.dropped.Add(1)
				continue
			}
			if _, err := c.do("PUBLISH", m.channel, string(m.payload)); err != nil {
				b.dropped.Add(1)
				return err
			}
			b.published.Add(1)
		case <-time.After(10 * time.Second):
			if _, err := c.do("PING"); err != nil {
				return err
			}
		}
	}
}

// Ping checks that a server answers (for the doctor) and returns the version it reports (Redis or Valkey).
func Ping(ctx context.Context, o Options) (string, error) {
	c, err := dial(ctx, o)
	if err != nil {
		return "", err
	}
	defer c.c.Close()
	r, err := c.do("PING")
	if err != nil {
		return "", err
	}
	if r.str != "PONG" {
		return "", fmt.Errorf("redis: unexpected answer %q", r.str)
	}
	info, err := c.do("INFO", "server")
	if err != nil {
		return "", nil // answering PING is what matters; some deployments forbid INFO
	}
	for _, line := range strings.Split(info.str, "\n") {
		line = strings.TrimSpace(line)
		for _, k := range []string{"valkey_version:", "redis_version:"} {
			if strings.HasPrefix(line, k) {
				v := strings.TrimPrefix(line, k)
				if k == "valkey_version:" {
					return "Valkey " + v, nil
				}
				return "Redis " + v, nil
			}
		}
	}
	return "", nil
}
