package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A small SMTP receiver for direct delivery (MX) of inbound mail (SPEC 13.1). It is not a relay and not a mail server: it
// accepts mail only for addresses its handler vouches for, answers every refusal during the conversation (so it never has to
// generate a bounce of its own and cannot cause backscatter), and says 250 only after the handler has kept the message: a
// failure to keep it is a 451 and the sending server retries. Credentials are not offered (no AUTH).

// SMTPHandler decides about recipients and takes delivery.
type SMTPHandler interface {
	// ValidRcpt returns nil, or an *SMTPError saying why not (a permanent 550 for an unknown or foreign address).
	ValidRcpt(ctx context.Context, addr string) error
	// Deliver keeps the message. A nil return is the promise a 250 stands for. An *SMTPError with a 5xx code refuses it for
	// good; any other error is a temporary failure (451) and the sender retries.
	Deliver(ctx context.Context, d Delivery) error
}

// Delivery is one accepted message with its envelope.
type Delivery struct {
	From   string // the envelope sender, empty for a bounce (<>)
	Rcpts  []string
	Raw    []byte // the message as received (dot-unstuffed, CRLF line ends)
	Remote net.Addr
	TLS    bool
}

type SMTPError struct {
	Code int
	Enh  string // enhanced status, such as 5.1.1
	Text string
}

func (e *SMTPError) Error() string { return fmt.Sprintf("%d %s %s", e.Code, e.Enh, e.Text) }

type SMTPServer struct {
	Hostname string
	TLS      *tls.Config   // STARTTLS is offered when set
	MaxBytes int64         // one message, default 25 MiB
	MaxRcpts int           // per message, default 50
	MaxConns int           // at once, default 100
	MaxPerIP int           // at once from one address, default 10
	Timeout  time.Duration // idle time for a command, default 60 s
	DataTime time.Duration // the whole DATA phase, default 10 minutes
	Handler  SMTPHandler
	Logf     func(string, ...any)

	mu    sync.Mutex
	conns int
	byIP  map[string]int
}

func (s *SMTPServer) defaults() {
	if s.MaxBytes <= 0 {
		s.MaxBytes = 25 << 20
	}
	if s.MaxRcpts <= 0 {
		s.MaxRcpts = 50
	}
	if s.MaxConns <= 0 {
		s.MaxConns = 100
	}
	if s.MaxPerIP <= 0 {
		s.MaxPerIP = 10
	}
	if s.Timeout <= 0 {
		s.Timeout = 60 * time.Second
	}
	if s.DataTime <= 0 {
		s.DataTime = 10 * time.Minute
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	if s.byIP == nil {
		s.byIP = map[string]int{}
	}
}

// Serve accepts connections until the context ends.
func (s *SMTPServer) Serve(ctx context.Context, ln net.Listener) error {
	s.defaults()
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); s.serve(ctx, c) }()
	}
}

func (s *SMTPServer) admit(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns >= s.MaxConns || s.byIP[ip] >= s.MaxPerIP {
		return false
	}
	s.conns++
	s.byIP[ip]++
	return true
}

func (s *SMTPServer) release(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns--
	if s.byIP[ip]--; s.byIP[ip] <= 0 {
		delete(s.byIP, ip)
	}
}

type smtpConn struct {
	s      *SMTPServer
	c      net.Conn
	r      *bufio.Reader
	secure bool
}

func (c *smtpConn) reply(code int, enh, text string) {
	c.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(c.c, "%d %s %s\r\n", code, enh, text)
}

const maxLine = 1000 // RFC 5321 says 512 for a command; leave room for extensions, and no more

func (c *smtpConn) line() (string, error) {
	c.c.SetReadDeadline(time.Now().Add(c.s.Timeout))
	var b []byte
	for {
		part, err := c.r.ReadSlice('\n')
		b = append(b, part...)
		if len(b) > maxLine {
			return "", errors.New("line too long")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
}

func (s *SMTPServer) serve(ctx context.Context, nc net.Conn) {
	defer nc.Close()
	ip, _, _ := net.SplitHostPort(nc.RemoteAddr().String())
	c := &smtpConn{s: s, c: nc, r: bufio.NewReaderSize(nc, 4096)}
	if !s.admit(ip) {
		c.reply(421, "4.3.2", s.Hostname+" too many connections, try again later")
		return
	}
	defer s.release(ip)
	c.reply(220, "", s.Hostname+" ESMTP Tasklane")
	greeted, hasFrom := false, false
	var from string
	var rcpts []string
	reset := func() { hasFrom, from, rcpts = false, "", nil }
	errs := 0
	bad := func(code int, enh, text string) bool {
		c.reply(code, enh, text)
		if errs++; errs >= 10 {
			c.reply(421, "4.7.0", "too many errors, closing")
			return true
		}
		return false
	}
	for ctx.Err() == nil {
		l, err := c.line()
		if err != nil {
			if err.Error() == "line too long" {
				c.reply(500, "5.5.2", "line too long")
			}
			return
		}
		verb, arg, _ := strings.Cut(l, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			if strings.TrimSpace(arg) == "" {
				if bad(501, "5.5.4", "a name is required") {
					return
				}
				continue
			}
			greeted = true
			reset()
			if strings.ToUpper(verb) == "HELO" {
				c.reply(250, "", s.Hostname)
				continue
			}
			fmt.Fprintf(nc, "250-%s\r\n250-SIZE %d\r\n250-8BITMIME\r\n250-ENHANCEDSTATUSCODES\r\n", s.Hostname, s.MaxBytes)
			if s.TLS != nil && !c.secure {
				fmt.Fprint(nc, "250-STARTTLS\r\n")
			}
			fmt.Fprint(nc, "250 HELP\r\n")
		case "STARTTLS":
			if s.TLS == nil || c.secure {
				if bad(502, "5.5.1", "STARTTLS is not available") {
					return
				}
				continue
			}
			c.reply(220, "2.0.0", "ready to start TLS")
			tc := tls.Server(nc, s.TLS)
			nc.SetDeadline(time.Now().Add(30 * time.Second))
			if err := tc.Handshake(); err != nil {
				return
			}
			c.c, c.r, c.secure = tc, bufio.NewReaderSize(tc, 4096), true
			nc = tc
			greeted = false // the client must say EHLO again
			reset()
		case "MAIL":
			if !greeted {
				if bad(503, "5.5.1", "say EHLO first") {
					return
				}
				continue
			}
			if hasFrom {
				if bad(503, "5.5.1", "a transaction is already open") {
					return
				}
				continue
			}
			addr, params, ok := pathArg(arg, "FROM:")
			if !ok {
				if bad(501, "5.5.4", "syntax: MAIL FROM:<address>") {
					return
				}
				continue
			}
			if sz := param(params, "SIZE"); sz != "" {
				if n, err := strconv.ParseInt(sz, 10, 64); err == nil && n > s.MaxBytes {
					c.reply(552, "5.3.4", "message size exceeds the limit")
					continue
				}
			}
			hasFrom, from, rcpts = true, addr, nil
			c.reply(250, "2.1.0", "ok")
		case "RCPT":
			if !hasFrom {
				if bad(503, "5.5.1", "MAIL first") {
					return
				}
				continue
			}
			addr, _, ok := pathArg(arg, "TO:")
			if !ok || addr == "" {
				if bad(501, "5.5.4", "syntax: RCPT TO:<address>") {
					return
				}
				continue
			}
			if len(rcpts) >= s.MaxRcpts {
				c.reply(452, "4.5.3", "too many recipients")
				continue
			}
			if err := s.Handler.ValidRcpt(ctx, addr); err != nil {
				var se *SMTPError
				if errors.As(err, &se) {
					c.reply(se.Code, se.Enh, se.Text)
				} else {
					c.reply(451, "4.3.0", "try again later")
				}
				if errs++; errs >= 10 {
					c.reply(421, "4.7.0", "too many errors, closing")
					return
				}
				continue
			}
			rcpts = append(rcpts, addr)
			c.reply(250, "2.1.5", "ok")
		case "DATA":
			if !hasFrom || len(rcpts) == 0 {
				if bad(503, "5.5.1", "MAIL and RCPT first") {
					return
				}
				continue
			}
			c.reply(354, "", "end the message with <CRLF>.<CRLF>")
			raw, tooBig, err := c.readData()
			if err != nil {
				return
			}
			if tooBig {
				c.reply(552, "5.3.4", "message size exceeds the limit")
				reset()
				continue
			}
			dctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			derr := s.Handler.Deliver(dctx, Delivery{From: from, Rcpts: rcpts, Raw: raw, Remote: nc.RemoteAddr(), TLS: c.secure})
			cancel()
			var se *SMTPError
			switch {
			case derr == nil:
				c.reply(250, "2.0.0", "queued")
			case errors.As(derr, &se):
				c.reply(se.Code, se.Enh, se.Text)
			default:
				s.Logf("smtp: delivery failed: %v", derr)
				c.reply(451, "4.3.0", "could not keep the message, try again later")
			}
			reset()
		case "RSET":
			reset()
			c.reply(250, "2.0.0", "ok")
		case "NOOP":
			c.reply(250, "2.0.0", "ok")
		case "QUIT":
			c.reply(221, "2.0.0", "bye")
			return
		case "VRFY", "EXPN":
			c.reply(252, "2.5.2", "cannot verify, but will accept for delivery")
		default:
			if bad(502, "5.5.2", "command not recognized") {
				return
			}
		}
	}
}

// readData reads the DATA body up to the lone dot, undoing dot-stuffing. A message over the limit is read through (so the
// conversation stays in step) and reported as too big, never held in memory.
func (c *smtpConn) readData() (raw []byte, tooBig bool, err error) {
	c.c.SetReadDeadline(time.Now().Add(c.s.DataTime))
	dr := textproto.NewReader(c.r).DotReader()
	lim := io.LimitReader(dr, c.s.MaxBytes+1)
	raw, err = io.ReadAll(lim)
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > c.s.MaxBytes {
		_, err = io.Copy(io.Discard, dr)
		return nil, true, err
	}
	// the DotReader gives LF line ends; the stored message is CRLF, as it travelled
	return []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n", "\r\n")), false, nil
}

// pathArg reads "FROM:<a@b> SIZE=1" into the address and the parameters.
func pathArg(arg, prefix string) (addr, params string, ok bool) {
	arg = strings.TrimSpace(arg)
	if len(arg) < len(prefix) || !strings.EqualFold(arg[:len(prefix)], prefix) {
		return "", "", false
	}
	rest := strings.TrimSpace(arg[len(prefix):])
	if !strings.HasPrefix(rest, "<") {
		return "", "", false
	}
	end := strings.Index(rest, ">")
	if end < 0 {
		return "", "", false
	}
	addr = rest[1:end]
	if strings.ContainsAny(addr, " \r\n\x00") {
		return "", "", false
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 && strings.HasPrefix(addr, "@") { // a source route: keep the mailbox
		addr = addr[i+1:]
	}
	return addr, strings.TrimSpace(rest[end+1:]), true
}

func param(params, name string) string {
	for _, p := range strings.Fields(params) {
		if k, v, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
