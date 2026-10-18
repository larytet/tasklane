// Package smtpmock is the one shared in-process SMTP server for tests (TESTPLAN 3.2). It speaks enough of
// SMTP to exercise a client for real (EHLO extensions, STARTTLS and implicit TLS with a throwaway CA,
// AUTH PLAIN, size limits), fails on demand at any stage, and records everything it receives, so tests
// assert on "no message", "exactly one message" or "credentials never sent in clear".
package smtpmock

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Stage names where a failure can be scripted.
const (
	Greeting = "greeting"
	Ehlo     = "ehlo"
	StartTLS = "starttls"
	Auth     = "auth"
	MailFrom = "mail"
	RcptTo   = "rcpt"
	Data     = "data" // the DATA command
	End      = "end"  // after the message body, before 250
)

// Behavior scripts one connection.
type Behavior struct {
	FailAt    string        // stage to fail at
	Code      int           // reply code at that stage (421, 450, 451, 535, 550, 554 ...)
	Text      string        // reply text
	DropAtEnd bool          // store the message, then close without replying: a lost acknowledgement
	Delay     time.Duration // pause before each reply (slow server)
	HangAt    string        // stage at which the server stops answering
}

type Options struct {
	TLS        string // "none" (default), "starttls", "tls" (implicit)
	User, Pass string // when set, AUTH PLAIN is required before MAIL
	AllowPlain bool   // advertise AUTH before TLS (a misconfigured server)
	MaxSize    int    // advertised and enforced SIZE, 0 = unlimited
	Hostname   string
	Bind       string // listen address, default 127.0.0.1 (127.0.0.2 is a non-local host for plaintext-auth tests)
	RejectRcpt map[string]bool
}

// Message is one accepted message.
type Message struct {
	From, Remote string
	Rcpts        []string
	Raw          []byte
	AuthUser     string
	TLS          bool
}

type Mock struct {
	t        *testing.T
	ln       net.Listener
	opts     Options
	ca       *x509.CertPool
	cert     tls.Certificate
	mu       sync.Mutex
	script   []Behavior
	conns    int
	msgs     []Message
	trans    []string
	authSeen []string
}

// New starts the mock on a random localhost port; it stops with the test.
func New(t *testing.T, o Options) *Mock {
	t.Helper()
	if o.Hostname == "" {
		o.Hostname = "mock.test"
	}
	m := &Mock{t: t, opts: o}
	m.makeCerts()
	if o.Bind == "" {
		o.Bind = "127.0.0.1"
	}
	m.opts.Bind = o.Bind
	ln, err := net.Listen("tcp", o.Bind+":0")
	if err != nil {
		t.Fatal(err)
	}
	if o.TLS == "tls" {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{m.cert}, MinVersion: tls.VersionTLS12})
	}
	m.ln = ln
	go m.serve()
	t.Cleanup(func() { ln.Close() })
	return m
}

func (m *Mock) Host() string { return m.opts.Bind }
func (m *Mock) Port() int    { return m.ln.Addr().(*net.TCPAddr).Port }

// CA is the pool a client must trust to verify the mock's certificate.
func (m *Mock) CA() *x509.CertPool { return m.ca }

// Script sets the behavior of the next connections, one entry per connection; later connections are well-behaved.
func (m *Mock) Script(b ...Behavior) { m.mu.Lock(); m.script = b; m.mu.Unlock() }

func (m *Mock) Messages() []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Message(nil), m.msgs...)
}
func (m *Mock) Connections() int { m.mu.Lock(); defer m.mu.Unlock(); return m.conns }

// Transcript is the raw conversation ("C: ..." and "S: ..."), message bodies elided.
func (m *Mock) Transcript() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.trans, "\n")
}

// ClearTextCredentialsSeen reports whether an AUTH command ever arrived on an unencrypted connection.
func (m *Mock) ClearTextCredentialsSeen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.authSeen {
		if a == "clear" {
			return true
		}
	}
	return false
}

func (m *Mock) makeCerts() {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtpmock test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	m.ca = x509.NewCertPool()
	m.ca.AddCert(caCert)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	m.cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (m *Mock) serve() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		var b Behavior
		if m.conns < len(m.script) {
			b = m.script[m.conns]
		}
		m.conns++
		n := m.conns
		m.mu.Unlock()
		go m.handle(c, b, n)
	}
}

type sess struct {
	m      *Mock
	c      net.Conn
	r      *bufio.Reader
	b      Behavior
	n      int
	tlsOn  bool
	authed string
	from   string
	rcpts  []string
}

func (s *sess) log(dir, line string) {
	s.m.mu.Lock()
	s.m.trans = append(s.m.trans, fmt.Sprintf("[%d] %s: %s", s.n, dir, line))
	s.m.mu.Unlock()
}

func (s *sess) reply(code int, text string) {
	if s.b.Delay > 0 {
		time.Sleep(s.b.Delay)
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		out := strconv.Itoa(code) + sep + l
		s.log("S", out)
		fmt.Fprintf(s.c, "%s\r\n", out)
	}
}

// failed applies a scripted failure for the stage; it reports whether it replied.
func (s *sess) failed(stage string) bool {
	if s.b.HangAt == stage {
		time.Sleep(time.Minute)
		return true
	}
	if s.b.FailAt == stage {
		code, text := s.b.Code, s.b.Text
		if code == 0 {
			code = 451
		}
		if text == "" {
			text = "scripted failure at " + stage
		}
		s.reply(code, text)
		if code == 421 {
			s.c.Close()
		}
		return true
	}
	return false
}

func (m *Mock) handle(c net.Conn, b Behavior, n int) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	s := &sess{m: m, c: c, r: bufio.NewReader(c), b: b, n: n}
	if m.opts.TLS == "tls" {
		s.tlsOn = true
		if tc, ok := c.(*tls.Conn); ok {
			if err := tc.Handshake(); err != nil {
				return
			}
		}
	}
	if s.failed(Greeting) {
		return
	}
	s.reply(220, m.opts.Hostname+" ESMTP smtpmock")
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.log("C", redact(line))
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch verb {
		case "EHLO", "HELO":
			if s.failed(Ehlo) {
				continue
			}
			ext := []string{m.opts.Hostname, "8BITMIME", "PIPELINING"}
			if m.opts.MaxSize > 0 {
				ext = append(ext, "SIZE "+strconv.Itoa(m.opts.MaxSize))
			}
			if m.opts.TLS == "starttls" && !s.tlsOn {
				ext = append(ext, "STARTTLS")
			}
			if m.opts.User != "" && (s.tlsOn || m.opts.TLS == "none" || m.opts.AllowPlain) {
				ext = append(ext, "AUTH PLAIN")
			}
			s.reply(250, strings.Join(ext, "\n"))
		case "STARTTLS":
			if m.opts.TLS != "starttls" || s.tlsOn {
				s.reply(502, "STARTTLS not available")
				continue
			}
			if s.failed(StartTLS) {
				continue
			}
			s.reply(220, "Ready to start TLS")
			tc := tls.Server(s.c, &tls.Config{Certificates: []tls.Certificate{m.cert}, MinVersion: tls.VersionTLS12})
			if err := tc.Handshake(); err != nil {
				return
			}
			s.c, s.r, s.tlsOn = tc, bufio.NewReader(tc), true
			s.from, s.rcpts = "", nil
		case "AUTH":
			kind := "clear"
			if s.tlsOn {
				kind = "tls"
			}
			m.mu.Lock()
			m.authSeen = append(m.authSeen, kind)
			m.mu.Unlock()
			if s.failed(Auth) {
				continue
			}
			parts := strings.Fields(line)
			var resp string
			if len(parts) >= 3 {
				resp = parts[2]
			} else {
				s.reply(334, "")
				l, err := s.r.ReadString('\n')
				if err != nil {
					return
				}
				resp = strings.TrimSpace(l)
				s.log("C", "<auth response>")
			}
			raw, _ := base64.StdEncoding.DecodeString(resp)
			f := strings.Split(string(raw), "\x00")
			if len(parts) < 2 || strings.ToUpper(parts[1]) != "PLAIN" || len(f) != 3 || f[1] != m.opts.User || f[2] != m.opts.Pass {
				s.reply(535, "5.7.8 authentication credentials invalid")
				continue
			}
			s.authed = f[1]
			s.reply(235, "2.7.0 authenticated")
		case "MAIL":
			if m.opts.User != "" && s.authed == "" {
				s.reply(530, "5.7.0 authentication required")
				continue
			}
			if s.failed(MailFrom) {
				continue
			}
			s.from = between(line, "<", ">")
			s.reply(250, "OK")
		case "RCPT":
			if s.failed(RcptTo) {
				continue
			}
			to := between(line, "<", ">")
			if m.opts.RejectRcpt[to] {
				s.reply(550, "5.1.1 no such user")
				continue
			}
			s.rcpts = append(s.rcpts, to)
			s.reply(250, "OK")
		case "DATA":
			if s.failed(Data) {
				continue
			}
			s.reply(354, "End data with <CR><LF>.<CR><LF>")
			var body bytes.Buffer
			for {
				l, err := s.r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				if strings.HasPrefix(l, "..") {
					l = l[1:]
				}
				body.WriteString(l)
				if m.opts.MaxSize > 0 && body.Len() > m.opts.MaxSize {
					break
				}
			}
			s.log("C", fmt.Sprintf("<%d bytes of message>", body.Len()))
			if m.opts.MaxSize > 0 && body.Len() > m.opts.MaxSize {
				s.reply(552, "5.3.4 message too large")
				continue
			}
			if s.b.FailAt == End {
				s.failed(End)
				continue
			}
			msg := Message{From: s.from, Rcpts: append([]string(nil), s.rcpts...), Raw: body.Bytes(), AuthUser: s.authed, TLS: s.tlsOn, Remote: s.c.RemoteAddr().String()}
			m.mu.Lock()
			m.msgs = append(m.msgs, msg)
			m.mu.Unlock()
			if s.b.DropAtEnd { // the message is stored, the acknowledgement never arrives
				s.log("S", "<connection dropped without a reply>")
				return
			}
			s.reply(250, "2.0.0 queued")
			s.from, s.rcpts = "", nil
		case "RSET":
			s.from, s.rcpts = "", nil
			s.reply(250, "OK")
		case "NOOP":
			s.reply(250, "OK")
		case "QUIT":
			s.reply(221, "bye")
			return
		default:
			s.reply(500, "command not recognized")
		}
	}
}

func redact(l string) string {
	if strings.HasPrefix(strings.ToUpper(l), "AUTH ") {
		f := strings.Fields(l)
		if len(f) >= 3 {
			return f[0] + " " + f[1] + " <redacted>"
		}
	}
	return l
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	j := strings.LastIndex(s, b)
	if i < 0 || j <= i {
		return ""
	}
	return s[i+1 : j]
}
