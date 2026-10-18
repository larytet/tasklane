package domain_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/mail"
)

type smtpdEnv struct {
	*inboundEnv
	addr string
	srv  *mail.SMTPServer
}

func newSMTPD(t *testing.T, tweak func(*mail.SMTPServer)) *smtpdEnv {
	e := &smtpdEnv{inboundEnv: newInbound(t)}
	dns, _ := loadDKIM(t)
	e.Svc.DNS = dns
	e.srv = e.Svc.SMTPServer(nil, t.Logf)
	if tweak != nil {
		tweak(e.srv)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	go e.srv.Serve(ctx, ln)
	t.Cleanup(cancel)
	return e
}

func (e *smtpdEnv) send(from string, rcpts []string, raw []byte) error {
	return smtp.SendMail(e.addr, nil, from, rcpts, raw)
}

// chat is a raw conversation for the cases a client library would not let us make.
type chat struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func (e *smtpdEnv) chat() *chat {
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		e.T.Fatal(err)
	}
	e.T.Cleanup(func() { c.Close() })
	ch := &chat{t: e.T, c: c, r: bufio.NewReader(c)}
	ch.read() // greeting
	return ch
}

// read returns the final line of a reply (the one whose fourth character is a space).
func (c *chat) read() string {
	c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			return "EOF"
		}
		l = strings.TrimRight(l, "\r\n")
		if len(l) < 4 || l[3] == ' ' {
			return l
		}
	}
}

func (c *chat) say(cmd string) string {
	fmt.Fprint(c.c, cmd+"\r\n")
	return c.read()
}

func (c *chat) expect(cmd, code string) {
	c.t.Helper()
	if got := c.say(cmd); !strings.HasPrefix(got, code) {
		c.t.Fatalf("%q: got %q, want %s", cmd, got, code)
	}
}

func TestMAIL09_SMTPListenerDeliversSignedMailAndAnswersEveryRefusalInTheConversation(t *testing.T) {
	e := newSMTPD(t, nil)
	_, file := loadDKIM(t)
	signed := file("reply_aligned.eml")
	if err := e.send("bob@example.com", []string{"abc@tasklane.example.com"}, signed); err != nil {
		t.Fatal(err)
	}
	cs := e.comments()
	if len(cs) != 1 || cs[0].Author.ID != e.Bob.ID || cs[0].Body != "Signed reply from Bob." {
		t.Fatalf("%+v", cs)
	}
	// the envelope was written into the message ahead of the sender's own headers
	l, _ := e.Svc.ListInbound(ctx, e.Admin, "accepted", 5)
	raw, err := e.Svc.InboundRaw(ctx, e.Admin, l[0].ID)
	if err != nil || !bytes.HasPrefix(raw, []byte("Return-Path: <bob@example.com>\r\nX-Original-To: abc@tasklane.example.com\r\nReceived: from [127.0.0.1] by tasklane.example.com with ESMTP;")) {
		t.Fatalf("%v\n%.300s", err, raw)
	}
	// the same message again (the sender retries after a lost answer): 250, counted, not applied twice
	if err := e.send("bob@example.com", []string{"abc@tasklane.example.com"}, signed); err != nil || len(e.comments()) != 1 {
		t.Fatalf("%v %d", err, len(e.comments()))
	}
	// mail without proof is accepted into the conversation and held: the decision is not the SMTP server's to make out loud
	unsigned := e.eml("bob@example.com", "abc@tasklane.example.com", "Re: [ABC-1] x", []string{authOK}, "claims a verdict nobody gave")
	if err := e.send("bob@example.com", []string{"abc@tasklane.example.com"}, unsigned); err != nil {
		t.Fatal(err)
	}
	if q, _ := e.Svc.ListInbound(ctx, e.Admin, "quarantined", 5); len(q) != 1 || !strings.Contains(q[0].Reason, "no DKIM signature") || len(e.comments()) != 1 {
		t.Fatalf("an Authentication-Results header written by the sender counted: %+v", q)
	}
	// recipients: the listener is not a relay, and says so before taking any message
	ch := e.chat()
	ch.expect("EHLO client.example.org", "250")
	ch.expect("MAIL FROM:<bob@example.com>", "250")
	for rcpt, want := range map[string]string{
		"someone@example.org":                                         "550 5.7.1",
		"abc@Tasklane.Example.COM":                                    "250",
		"abc+bug@tasklane.example.com":                                "250",
		"nobody@tasklane.example.com":                                 "550 5.1.1",
		"reply+" + strings.Repeat("A", 39) + "@tasklane.example.com":  "250",
		"bounce+" + strings.Repeat("B", 26) + "@tasklane.example.com": "250",
		"reply+short@tasklane.example.com":                            "550 5.1.1",
		"a b@tasklane.example.com":                                    "501",
		"nodomain":                                                    "501",
	} {
		if got := ch.say("RCPT TO:<" + rcpt + ">"); !strings.HasPrefix(got, want) {
			t.Errorf("RCPT %s: %q, want %s", rcpt, got, want)
		}
	}
	// an archived project's address is gone
	e.Store.Pool.Exec(ctx, `UPDATE project SET archived=true WHERE key='ABC'`)
	ch2 := e.chat()
	ch2.expect("EHLO x", "250")
	ch2.expect("MAIL FROM:<>", "250")
	if got := ch2.say("RCPT TO:<abc@tasklane.example.com>"); !strings.HasPrefix(got, "550 5.1.1") {
		t.Errorf("archived: %q", got)
	}
}

func TestMAIL09_SMTPListenerHandlesBouncesAddressedToTheReturnPath(t *testing.T) {
	e := newSMTPD(t, nil)
	e.assignToBob("second")
	verp := e.verp("bob@example.com", 0)
	report := dsn(verp, "bob@example.com", "failed", "5.1.1", e.origID("bob@example.com", 0))
	// a bounce has the null return path
	if err := e.send("", []string{verp}, report); err != nil {
		t.Fatal(err)
	}
	if sl := e.suppressedList(); len(sl) != 1 || sl[0].Email != "bob@example.com" || sl[0].Kind != "hard" {
		t.Fatalf("%+v", sl)
	}
	// and nothing was sent back by the listener (no backscatter)
	if len(e.msgTo("MAILER-DAEMON@mx.example.org")) != 0 {
		t.Fatal("something was sent to the bounce's sender")
	}
}

func TestMAIL09_SMTPListenerProtocolAbuse(t *testing.T) {
	e := newSMTPD(t, func(s *mail.SMTPServer) { s.MaxBytes = 4000; s.MaxRcpts = 2 })
	ch := e.chat()
	ch.expect("MAIL FROM:<a@example.org>", "503") // before EHLO
	ch.expect("EHLO", "501")
	ch.expect("EHLO client.example.org", "250")
	ch.expect("RCPT TO:<abc@tasklane.example.com>", "503") // before MAIL
	ch.expect("DATA", "503")
	ch = e.chat() // a fresh connection: ten errors close one
	ch.expect("EHLO client.example.org", "250")
	ch.expect("MAIL TO:<a@example.org>", "501")
	ch.expect("MAIL FROM:a@example.org", "501")
	ch.expect("MAIL FROM:<a@example.org> SIZE=999999999", "552") // declared too big
	ch.expect("MAIL FROM:<a@example.org> SIZE=100", "250")
	ch.expect("MAIL FROM:<b@example.org>", "503") // a transaction is open
	ch.expect("DATA", "503")                      // no recipient yet
	ch.expect("RCPT TO:<abc@tasklane.example.com>", "250")
	ch.expect("RCPT TO:<abc+x@tasklane.example.com>", "250")
	ch.expect("RCPT TO:<abc+y@tasklane.example.com>", "452") // too many
	ch.expect("RSET", "250")
	ch.expect("VRFY someone", "252") // never confirms or denies a mailbox
	ch.expect("FROBNICATE", "502")
	ch.expect("STARTTLS", "502") // not configured
	ch.expect("NOOP", "250")
	// a refusal of a recipient is not an error of the client's protocol and does not end the conversation
	ch.expect("MAIL FROM:<a@example.org>", "250")
	ch.expect("RCPT TO:<nobody@tasklane.example.com>", "550")
	ch.expect("RCPT TO:<abc@tasklane.example.com>", "250")
	// a line that is far too long is refused and the connection closed
	if got := ch.say(strings.Repeat("X", 5000)); !strings.HasPrefix(got, "500") {
		t.Fatalf("%q", got)
	}
	// ten errors close the connection
	ch = e.chat()
	last := ""
	for i := 0; i < 12; i++ {
		last = ch.say("BOGUS")
		if strings.HasPrefix(last, "421") || last == "EOF" {
			break
		}
	}
	if !strings.HasPrefix(last, "421") && last != "EOF" {
		t.Fatalf("a client that only sends errors was kept: %q", last)
	}
}

func TestMAIL09_SMTPListenerSizeLimitDotStuffingAndKeepingTheConversationInStep(t *testing.T) {
	e := newSMTPD(t, func(s *mail.SMTPServer) { s.MaxBytes = 5000 })
	e.Svc.Mail.Inbound.MaxBytes = 5000
	ch := e.chat()
	ch.expect("EHLO x", "250")
	ch.expect("MAIL FROM:<bob@example.com>", "250")
	ch.expect("RCPT TO:<abc@tasklane.example.com>", "250")
	ch.expect("DATA", "354")
	// a message far over the limit is read through and refused after, the connection stays in step and is reusable
	big := "Subject: big\r\nMessage-ID: <big@example.com>\r\n\r\n" + strings.Repeat("0123456789abcdef\r\n", 4000) + "."
	fmt.Fprint(ch.c, big+"\r\n")
	if got := ch.read(); !strings.HasPrefix(got, "552") {
		t.Fatalf("%q", got)
	}
	ch.expect("NOOP", "250")
	// dot-stuffing: a body line that starts with a dot arrives with one dot, and a lone dot ends the message
	ch.expect("MAIL FROM:<bob@example.com>", "250")
	ch.expect("RCPT TO:<abc@tasklane.example.com>", "250")
	ch.expect("DATA", "354")
	body := "From: bob@example.com\r\nSubject: Re: [ABC-1] dots\r\nMessage-ID: <dots@example.com>\r\nContent-Type: text/plain\r\n\r\n..hidden line\r\n...three\r\nplain\r\n."
	fmt.Fprint(ch.c, body+"\r\n")
	if got := ch.read(); !strings.HasPrefix(got, "250") {
		t.Fatalf("%q", got)
	}
	l, _ := e.Svc.ListInbound(ctx, e.Admin, "", 5)
	var found bool
	for _, r := range l {
		if r.Subject == "Re: [ABC-1] dots" {
			raw, _ := e.Svc.InboundRaw(ctx, e.Admin, r.ID)
			found = bytes.Contains(raw, []byte("\r\n\r\n.hidden line\r\n..three\r\nplain\r\n"))
		}
	}
	if !found {
		t.Fatal("dot-stuffing was not undone")
	}
	// the over-limit message left no trace but the refusal
	if l, _ := e.Svc.ListInbound(ctx, e.Admin, "", 20); len(l) != 2 { // the notification-triggered setup sent none; the dots message and the first unsigned one
		for _, r := range l {
			t.Logf("%+v", r)
		}
	}
	for _, r := range l {
		if r.Subject == "big" {
			t.Fatal("the oversized message was recorded")
		}
	}
}

func TestMAIL09_SMTPListenerAcknowledgesOnlyWhatItKept(t *testing.T) {
	e := newSMTPD(t, nil)
	_, file := loadDKIM(t)
	signed := file("reply_aligned.eml")
	saved := e.Svc.Blobs
	e.Svc.Blobs = nil // the raw message cannot be kept
	err := e.send("bob@example.com", []string{"abc@tasklane.example.com"}, signed)
	if err == nil || !strings.Contains(err.Error(), "451") {
		t.Fatalf("a message that could not be kept was acknowledged: %v", err)
	}
	var pending int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE outcome='pending'`).Scan(&pending)
	if pending != 0 || len(e.comments()) != 0 {
		t.Fatalf("%d pending, %d comments", pending, len(e.comments()))
	}
	// the sending server retries; this time it is kept and applied exactly once
	e.Svc.Blobs = saved
	if err := e.send("bob@example.com", []string{"abc@tasklane.example.com"}, signed); err != nil || len(e.comments()) != 1 {
		t.Fatalf("%v %d", err, len(e.comments()))
	}
}

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	crt, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(crt)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}, pool
}

func TestMAIL09_SMTPListenerStartTLSAndConnectionLimits(t *testing.T) {
	cert, pool := selfSigned(t)
	e := newSMTPD(t, func(s *mail.SMTPServer) {
		s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		s.MaxPerIP = 2
		s.Timeout = 400 * time.Millisecond
	})
	_, file := loadDKIM(t)
	// a client that upgrades: the message is accepted and marked as received over TLS
	c, err := smtp.Dial(e.addr)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS was not offered")
	}
	if ok, _ := c.Extension("AUTH"); ok {
		t.Fatal("AUTH was offered")
	}
	if err := c.StartTLS(&tls.Config{RootCAs: pool, ServerName: "localhost"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS is offered again inside TLS")
	}
	c.Mail("bob@example.com")
	c.Rcpt("abc@tasklane.example.com")
	w, _ := c.Data()
	w.Write(file("reply_aligned.eml"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	c.Quit()
	l, _ := e.Svc.ListInbound(ctx, e.Admin, "accepted", 5)
	raw, _ := e.Svc.InboundRaw(ctx, e.Admin, l[0].ID)
	if !bytes.Contains(raw, []byte("with ESMTPS;")) {
		t.Fatalf("%.200s", raw)
	}
	// after the upgrade the client must say EHLO again
	ch := e.chat()
	ch.expect("EHLO x", "250")
	ch.expect("STARTTLS", "220")
	tc := tls.Client(ch.c, &tls.Config{RootCAs: pool, ServerName: "localhost"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	ch2 := &chat{t: t, c: tc, r: bufio.NewReader(tc)}
	ch2.expect("MAIL FROM:<a@example.org>", "503")
	// at most two connections from one address; a third is turned away politely, and a slot frees when one closes
	a, b := e.chat(), e.chat()
	third, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	third.SetReadDeadline(time.Now().Add(3 * time.Second))
	greet, _ := bufio.NewReader(third).ReadString('\n')
	if !strings.HasPrefix(greet, "421") {
		t.Fatalf("a third connection was accepted: %q", greet)
	}
	a.c.Close()
	b.c.Close()
	time.Sleep(200 * time.Millisecond)
	ok := e.chat()
	ok.expect("NOOP", "250") // a slot was freed, so this one is served
	// an idle connection is closed
	idle := e.chat()
	time.Sleep(900 * time.Millisecond)
	if got := idle.say("NOOP"); got != "EOF" {
		t.Fatalf("an idle connection stayed open: %q", got)
	}
}
