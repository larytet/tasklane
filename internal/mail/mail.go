// Package mail builds and sends messages: a Mailer interface with an SMTP transport (no TLS, STARTTLS or
// implicit TLS, authentication, certificate verification) and a MIME builder with threading headers.
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Message is one outgoing email.
type Message struct {
	From, To        string
	Subject         string
	MessageID       string // with angle brackets
	InReplyTo, Refs string
	ReplyTo         string
	Text, HTML      string
	ListUnsubscribe string
	Date            time.Time
	// Instance names this installation in X-Tasklane-Instance; every message of ours carries it and a loop counter, so
	// mail between two Tasklane installations (or back to us) is recognised and ignored.
	Instance string
}

// Mailer is the transport interface; provider APIs (SES, SendGrid, ...) implement it later.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// Permanent marks an error that retrying cannot fix (bad recipient, rejected credentials).
type Permanent struct{ Err error }

func (p *Permanent) Error() string { return p.Err.Error() }
func (p *Permanent) Unwrap() error { return p.Err }

type SMTP struct {
	Host      string
	Port      int
	TLS       string // "none", "starttls" or "tls" (implicit)
	User      string
	Pass      string
	From      string
	Hello     string
	TLSConfig *tls.Config // optional base config (tests trust a throwaway CA here); verification stays on
	Timeout   time.Duration
}

func (s *SMTP) addr() string { return net.JoinHostPort(s.Host, fmt.Sprint(s.Port)) }

func (s *SMTP) tlsConfig() *tls.Config {
	if s.TLSConfig != nil {
		c := s.TLSConfig.Clone()
		c.ServerName = s.Host
		return c
	}
	return &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12} // verification is always on
}

// session connects, negotiates TLS and authenticates; Send and Probe share it so a check tests what a send does.
func (s *SMTP) session(ctx context.Context) (*smtp.Client, error) {
	if s.Timeout == 0 {
		s.Timeout = 20 * time.Second
	}
	d := net.Dialer{Timeout: s.Timeout}
	var conn net.Conn
	var err error
	switch s.TLS {
	case "tls":
		conn, err = tls.DialWithDialer(&d, "tcp", s.addr(), s.tlsConfig())
	case "none", "starttls", "":
		conn, err = d.DialContext(ctx, "tcp", s.addr())
	default:
		return nil, &Permanent{fmt.Errorf("unknown SMTP TLS mode %q (use none, starttls or tls)", s.TLS)}
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", s.addr(), err)
	}
	conn.SetDeadline(time.Now().Add(s.Timeout * 3))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("SMTP greeting: %w", err)
	}
	fail := func(err error) (*smtp.Client, error) { c.Close(); return nil, err }
	if s.Hello != "" {
		if err := c.Hello(s.Hello); err != nil {
			return fail(err)
		}
	}
	if s.TLS == "starttls" || s.TLS == "" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fail(&Permanent{errors.New("server does not offer STARTTLS; refusing to continue in plaintext (set TLS mode to none to allow it)")})
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			return fail(fmt.Errorf("STARTTLS: %w", err))
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Pass, s.Host)); err != nil { // PlainAuth itself refuses plaintext to non-local hosts
			var te *textproto.Error
			if errors.As(err, &te) && te.Code >= 500 {
				return fail(&Permanent{fmt.Errorf("authentication rejected: %w", err)})
			}
			return fail(fmt.Errorf("authentication: %w", err))
		}
	}
	return c, nil
}

// Probe checks the whole path to the relay (connect, TLS, login) without sending anything: the admin test and `doctor`.
func (s *SMTP) Probe(ctx context.Context) error {
	c, err := s.session(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Quit()
}

// Send delivers one message. Credentials are never sent over an unencrypted connection to a remote host.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	c, err := s.session(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	from := s.From
	if m.From != "" {
		from = m.From
	}
	fa, err := mail.ParseAddress(from)
	if err != nil {
		return &Permanent{fmt.Errorf("bad sender address: %w", err)}
	}
	ta, err := mail.ParseAddress(m.To)
	if err != nil {
		return &Permanent{fmt.Errorf("bad recipient address: %w", err)}
	}
	if err := c.Mail(fa.Address); err != nil {
		return err
	}
	if err := c.Rcpt(ta.Address); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) && te.Code >= 500 {
			return &Permanent{fmt.Errorf("recipient rejected: %w", err)}
		}
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if m.From == "" {
		m.From = from
	}
	raw, err := Build(m)
	if err != nil {
		return &Permanent{err}
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// hdr makes a header value safe: no line breaks can be smuggled in (header injection).
func hdr(v string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(v) }

// Build renders the RFC 5322 message: multipart/alternative with quoted-printable text and HTML parts,
// threading headers, and Auto-Submitted so other systems do not auto-reply.
func Build(m Message) ([]byte, error) {
	if m.To == "" || m.MessageID == "" {
		return nil, errors.New("message needs a recipient and a Message-ID")
	}
	if m.Date.IsZero() {
		m.Date = time.Now()
	}
	var b bytes.Buffer
	w := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, hdr(v))
		}
	}
	w("From", m.From)
	w("To", m.To)
	w("Subject", mime.QEncoding.Encode("utf-8", hdr(m.Subject)))
	w("Date", m.Date.UTC().Format(time.RFC1123Z))
	w("Message-ID", m.MessageID)
	w("In-Reply-To", m.InReplyTo)
	w("References", m.Refs)
	w("Reply-To", m.ReplyTo)
	w("Auto-Submitted", "auto-generated")
	w("X-Auto-Response-Suppress", "All")
	w("Precedence", "bulk")
	if m.Instance != "" {
		w("X-Tasklane-Instance", m.Instance)
		w("X-Tasklane-Loop", "1")
	}
	w("List-Unsubscribe", m.ListUnsubscribe)
	w("MIME-Version", "1.0")
	mw := multipart.NewWriter(&b)
	// the boundary is random, so content can never collide with it
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%s\r\n\r\n", mw.Boundary())
	part := func(ct, body string) error {
		p, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {ct + "; charset=utf-8"}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return err
		}
		qw := quotedprintable.NewWriter(p)
		if _, err := qw.Write([]byte(body)); err != nil {
			return err
		}
		return qw.Close()
	}
	if err := part("text/plain", m.Text); err != nil {
		return nil, err
	}
	if m.HTML != "" {
		if err := part("text/html", m.HTML); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
