package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/egress"
)

// A small IMAP client for the one job Tasklane has: read new messages from a mailbox by UID, and wait for more with IDLE.
// It speaks IMAP4rev1 over implicit TLS (certificates always verified), or plain text to a loopback address only, so a
// password never crosses a network unencrypted. Every connection goes through the egress policy.

type IMAPConfig struct {
	Host         string
	Port         int
	TLS          string // "tls" (default) or "none" (loopback only)
	User, Pass   string
	Mailbox      string // default INBOX
	AllowPrivate bool   // the egress policy: private and loopback addresses need this
	TLSConfig    *tls.Config
	Timeout      time.Duration // per command, default 30 s
}

type IMAPConn struct {
	c     net.Conn
	r     *bufio.Reader
	n     int
	to    time.Duration
	Idle  bool // the server offers IDLE
	Valid uint32
	Next  uint32
	Count int
}

var ErrIMAP = errors.New("imap")

func (cfg IMAPConfig) timeout() time.Duration {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return 30 * time.Second
}

// DialIMAP connects, logs in, learns the capabilities and selects the mailbox.
func DialIMAP(ctx context.Context, cfg IMAPConfig) (*IMAPConn, error) {
	if strings.ContainsAny(cfg.User+cfg.Pass+cfg.Mailbox, "\r\n\x00") {
		return nil, fmt.Errorf("%w: credentials and mailbox name may not contain line breaks", ErrIMAP)
	}
	pol := egress.Policy{AllowPrivate: cfg.AllowPrivate}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, cfg.Host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("%w: cannot resolve %s", ErrIMAP, cfg.Host)
	}
	var ip net.IP
	for _, a := range ips {
		if err := pol.Check(a.IP); err != nil {
			return nil, err
		}
		if ip == nil {
			ip = a.IP
		}
	}
	mode := cfg.TLS
	if mode == "" {
		mode = "tls"
	}
	if mode == "none" && !ip.IsLoopback() {
		return nil, fmt.Errorf("%w: a password is not sent without TLS to a host that is not on this machine", ErrIMAP)
	}
	if mode != "none" && mode != "tls" {
		return nil, fmt.Errorf("%w: tls must be tls or none", ErrIMAP)
	}
	d := net.Dialer{Timeout: 10 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIMAP, err)
	}
	conn := raw
	if mode == "tls" {
		tc := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
		if cfg.TLSConfig != nil {
			tc = cfg.TLSConfig.Clone()
			tc.ServerName = cfg.Host
		}
		conn = tls.Client(raw, tc)
	}
	c := &IMAPConn{c: conn, r: bufio.NewReaderSize(conn, 64<<10), to: cfg.timeout()}
	fail := func(err error) (*IMAPConn, error) { conn.Close(); return nil, err }
	c.deadline()
	line, err := c.r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "* OK") && !strings.HasPrefix(line, "* PREAUTH") {
		return fail(fmt.Errorf("%w: unexpected greeting", ErrIMAP))
	}
	if _, err := c.cmd("LOGIN " + quote(cfg.User) + " " + quote(cfg.Pass)); err != nil {
		return fail(fmt.Errorf("%w: login failed", ErrIMAP))
	}
	caps, err := c.cmd("CAPABILITY")
	if err != nil {
		return fail(err)
	}
	for _, it := range caps {
		c.Idle = c.Idle || strings.Contains(strings.ToUpper(it.line), " IDLE")
	}
	mb := cfg.Mailbox
	if mb == "" {
		mb = "INBOX"
	}
	its, err := c.cmd("SELECT " + quote(mb))
	if err != nil {
		return fail(fmt.Errorf("%w: cannot open mailbox %q", ErrIMAP, mb))
	}
	for _, it := range its {
		u := strings.ToUpper(it.line)
		if i := strings.Index(u, "[UIDVALIDITY "); i >= 0 {
			c.Valid = numAfter(u[i+len("[UIDVALIDITY "):])
		}
		if i := strings.Index(u, "[UIDNEXT "); i >= 0 {
			c.Next = numAfter(u[i+len("[UIDNEXT "):])
		}
		if f := strings.Fields(u); len(f) == 3 && f[2] == "EXISTS" {
			c.Count, _ = strconv.Atoi(f[1])
		}
	}
	return c, nil
}

func numAfter(s string) uint32 {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, _ := strconv.ParseUint(s[:i], 10, 32)
	return uint32(n)
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (c *IMAPConn) deadline() { c.c.SetDeadline(time.Now().Add(c.to)) }

type imapItem struct {
	line string // the response with its literals taken out
	lit  [][]byte
}

// readItem reads one response, including any literals ({N}CRLF followed by N bytes) that are part of it.
func (c *IMAPConn) readItem(maxLit int64) (imapItem, error) {
	var it imapItem
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return it, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasSuffix(line, "}") {
			if i := strings.LastIndex(line, "{"); i >= 0 {
				if n, err := strconv.ParseInt(strings.TrimSuffix(line[i+1:], "}"), 10, 64); err == nil && n >= 0 {
					keep := n
					if maxLit > 0 && keep > maxLit {
						keep = maxLit
					}
					buf := make([]byte, keep)
					if _, err := io.ReadFull(c.r, buf); err != nil {
						return it, err
					}
					if n > keep { // too large to keep: read it through and drop the rest; the caller sees a truncated body
						if _, err := io.CopyN(io.Discard, c.r, n-keep); err != nil {
							return it, err
						}
					}
					it.lit = append(it.lit, buf)
					it.line += line[:i]
					continue
				}
			}
		}
		it.line += line
		return it, nil
	}
}

// cmd sends a tagged command and collects the untagged responses until its tagged answer. NO and BAD are errors.
func (c *IMAPConn) cmd(text string) ([]imapItem, error) { return c.cmdMax(text, 0) }

func (c *IMAPConn) cmdMax(text string, maxLit int64) ([]imapItem, error) {
	c.n++
	tag := fmt.Sprintf("t%d", c.n)
	c.deadline()
	if _, err := io.WriteString(c.c, tag+" "+text+"\r\n"); err != nil {
		return nil, err
	}
	var out []imapItem
	for {
		it, err := c.readItem(maxLit)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(it.line, tag+" ") {
			if !strings.HasPrefix(it.line[len(tag)+1:], "OK") {
				return nil, fmt.Errorf("%w: %s", ErrIMAP, strings.TrimSpace(it.line[len(tag)+1:]))
			}
			return out, nil
		}
		out = append(out, it)
	}
}

// UIDsAfter lists the UIDs above last, in order. (A server answers "n:*" with the last message even when n is above every UID,
// so the result is filtered.)
func (c *IMAPConn) UIDsAfter(last uint32) ([]uint32, error) {
	its, err := c.cmd(fmt.Sprintf("UID SEARCH UID %d:*", last+1))
	if err != nil {
		return nil, err
	}
	var uids []uint32
	for _, it := range its {
		f := strings.Fields(it.line)
		if len(f) >= 2 && strings.EqualFold(f[1], "SEARCH") {
			for _, s := range f[2:] {
				if n, err := strconv.ParseUint(s, 10, 32); err == nil && uint32(n) > last {
					uids = append(uids, uint32(n))
				}
			}
		}
	}
	return uids, nil
}

// Fetch returns the raw message without marking it read. A message larger than limit comes back cut at limit+1 bytes, which
// the caller treats as too large.
func (c *IMAPConn) Fetch(uid uint32, limit int64) ([]byte, error) {
	its, err := c.cmdMax(fmt.Sprintf("UID FETCH %d (BODY.PEEK[])", uid), limit+1)
	if err != nil {
		return nil, err
	}
	for _, it := range its {
		if strings.Contains(strings.ToUpper(it.line), "FETCH") && len(it.lit) > 0 {
			return it.lit[0], nil
		}
	}
	return nil, fmt.Errorf("%w: message %d is gone", ErrIMAP, uid)
}

// WaitNew waits up to max for the server to announce new mail (IDLE). It returns true when something arrived and false on
// timeout; a server without IDLE is waited on by the caller.
func (c *IMAPConn) WaitNew(ctx context.Context, max time.Duration) (bool, error) {
	if !c.Idle {
		return false, errors.New("the server does not offer IDLE")
	}
	c.n++
	tag := fmt.Sprintf("t%d", c.n)
	c.deadline()
	if _, err := io.WriteString(c.c, tag+" IDLE\r\n"); err != nil {
		return false, err
	}
	line, err := c.r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "+") {
		return false, fmt.Errorf("%w: IDLE was refused", ErrIMAP)
	}
	arrived := false
	end := time.Now().Add(max)
	stop := make(chan struct{})
	go func() { // a cancelled context ends the wait
		select {
		case <-ctx.Done():
			c.c.SetReadDeadline(time.Now())
		case <-stop:
		}
	}()
	defer close(stop)
	for !arrived {
		c.c.SetReadDeadline(end)
		it, err := c.readItem(0)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			return false, err
		}
		f := strings.Fields(it.line)
		if len(f) >= 3 && (f[2] == "EXISTS" || f[2] == "RECENT") {
			arrived = true
		}
	}
	c.deadline()
	if _, err := io.WriteString(c.c, "DONE\r\n"); err != nil {
		return arrived, err
	}
	for {
		it, err := c.readItem(0)
		if err != nil {
			return arrived, err
		}
		if strings.HasPrefix(it.line, tag+" ") {
			if ctx.Err() != nil {
				return arrived, ctx.Err()
			}
			return arrived, nil
		}
	}
}

func (c *IMAPConn) Close() {
	c.c.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(c.c, fmt.Sprintf("t%d LOGOUT\r\n", c.n+1))
	c.c.Close()
}

// Noop asks the server to report anything new (a server without IDLE shows new mail after it).
func (c *IMAPConn) Noop() error { _, err := c.cmd("NOOP"); return err }
