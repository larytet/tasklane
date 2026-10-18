package domain

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/mail"
)

// The built-in SMTP receiver (MAIL-09): direct delivery to this server for the reply, bounce and project addresses of the
// installation's mail domain. It accepts only what the installation would act on, so it is not an open relay, and it never
// sends a bounce of its own: a message that cannot be accepted is refused in the conversation. Mail it receives is never
// "trusted": it carries no verdict from a receiver of ours, so a message needs its own DKIM proof like any other, and any
// Authentication-Results header the sender wrote is ignored.

type smtpHandler struct{ s *Service }

var (
	reReplyLocal  = regexp.MustCompile(`(?i)^(reply|bounce)\+[A-Za-z0-9_-]{20,80}$`)
	errRelay      = &mail.SMTPError{Code: 550, Enh: "5.7.1", Text: "relaying denied: this server accepts mail only for its own addresses"}
	errNoSuchUser = &mail.SMTPError{Code: 550, Enh: "5.1.1", Text: "no such mailbox here"}
)

func (h smtpHandler) ValidRcpt(ctx context.Context, addr string) error {
	local, dom, ok := strings.Cut(addr, "@")
	if !ok || local == "" || strings.ContainsAny(local+dom, " <>\"\\,;()[]") {
		return &mail.SMTPError{Code: 501, Enh: "5.1.3", Text: "bad address"}
	}
	if d := h.s.mailDomain(); d == "" || !strings.EqualFold(dom, d) {
		return errRelay
	}
	if reReplyLocal.MatchString(local) {
		return nil
	}
	key := strings.ToUpper(strings.SplitN(local, "+", 2)[0])
	var one int
	if err := h.s.DB.QueryRow(ctx, `SELECT 1 FROM project WHERE key=$1 AND NOT archived`, key).Scan(&one); err != nil {
		return errNoSuchUser
	}
	return nil
}

func (h smtpHandler) Deliver(ctx context.Context, d mail.Delivery) error {
	if h.s.mailDomain() == "" {
		return fmt.Errorf("email is not configured")
	}
	// The envelope goes into the message, ahead of whatever the sender wrote: the return path (a bounce has none), who it was for
	// (a Bcc'd address is in no header), and where it came from. Our headers come first, so they are the ones a reader finds.
	var b strings.Builder
	rp := "<>"
	if d.From != "" {
		rp = "<" + d.From + ">"
	}
	ip := ""
	if d.Remote != nil {
		ip, _, _ = net.SplitHostPort(d.Remote.String())
	}
	fmt.Fprintf(&b, "Return-Path: %s\r\n", rp)
	for _, r := range d.Rcpts {
		fmt.Fprintf(&b, "X-Original-To: %s\r\n", r)
	}
	proto := "ESMTP"
	if d.TLS {
		proto = "ESMTPS"
	}
	fmt.Fprintf(&b, "Received: from [%s] by %s with %s; %s\r\n", ip, h.s.mailDomain(), proto, h.s.Now().UTC().Format(time.RFC1123Z))
	raw := append([]byte(b.String()), d.Raw...)
	// untrusted: no Authentication-Results from a receiver of ours exists for this message
	_, err := h.s.IngestMail(ctx, "", false, raw)
	return err
}

// SMTPServer builds the receiver for this installation.
func (s *Service) SMTPServer(tc *tls.Config, logf func(string, ...any)) *mail.SMTPServer {
	return &mail.SMTPServer{Hostname: s.mailDomain(), TLS: tc, MaxBytes: s.inboundLimits().MaxBytes, Handler: smtpHandler{s}, Logf: logf}
}

// RunSMTPListener accepts mail on addr until the context ends.
func (s *Service) RunSMTPListener(ctx context.Context, addr string, tc *tls.Config, logf func(string, ...any)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	logf("smtp listener on %s for %s", ln.Addr(), s.mailDomain())
	return s.SMTPServer(tc, logf).Serve(ctx, ln)
}
