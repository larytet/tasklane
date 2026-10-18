package mail_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/testutil/smtpmock"
)

func msg() mail.Message {
	return mail.Message{From: "Tasklane <tasklane@example.com>", To: "Jana K. <jana@example.com>", Subject: "[ABC-1] Checkout fails",
		MessageID: "<issue.1.5@example.com>", InReplyTo: "<issue.1@example.com>", Refs: "<issue.1@example.com>",
		Text: "Hello\r\n-- Reply above this line --\r\n", HTML: "<p>Hello</p>", Date: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
}

func client(m *smtpmock.Mock, mode, user, pass string) *mail.SMTP {
	return &mail.SMTP{Host: "localhost", Port: m.Port(), TLS: mode, User: user, Pass: pass, From: "tasklane@example.com",
		TLSConfig: &tls.Config{RootCAs: m.CA()}, Timeout: 3 * time.Second}
}

func isPermanent(err error) bool { var p *mail.Permanent; return errors.As(err, &p) }

func TestMAIL01_TransportsAndAuth(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		opts    smtpmock.Options
		client  func(*smtpmock.Mock) *mail.SMTP
		wantTLS bool
		auth    string
	}{
		{"no TLS, no auth", smtpmock.Options{TLS: "none"}, func(m *smtpmock.Mock) *mail.SMTP { return client(m, "none", "", "") }, false, ""},
		{"STARTTLS with auth", smtpmock.Options{TLS: "starttls", User: "u", Pass: "p"}, func(m *smtpmock.Mock) *mail.SMTP { return client(m, "starttls", "u", "p") }, true, "u"},
		{"implicit TLS with auth", smtpmock.Options{TLS: "tls", User: "u", Pass: "p"}, func(m *smtpmock.Mock) *mail.SMTP { return client(m, "tls", "u", "p") }, true, "u"},
		{"STARTTLS is the default mode", smtpmock.Options{TLS: "starttls"}, func(m *smtpmock.Mock) *mail.SMTP { return client(m, "", "", "") }, true, ""},
	}
	for _, c := range cases {
		m := smtpmock.New(t, c.opts)
		if err := c.client(m).Send(ctx, msg()); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, m.Transcript())
		}
		got := m.Messages()
		if len(got) != 1 || got[0].TLS != c.wantTLS || got[0].AuthUser != c.auth || got[0].Rcpts[0] != "jana@example.com" || got[0].From != "tasklane@example.com" {
			t.Fatalf("%s: %+v\n%s", c.name, got, m.Transcript())
		}
		if m.ClearTextCredentialsSeen() {
			t.Fatalf("%s: credentials sent in clear", c.name)
		}
	}
}

func TestMAIL01_SecurityFailures(t *testing.T) {
	ctx := context.Background()
	// certificate verification failure: the client does not trust the mock's CA, and does not fall back
	m := smtpmock.New(t, smtpmock.Options{TLS: "starttls", User: "u", Pass: "p"})
	c := client(m, "starttls", "u", "p")
	c.TLSConfig = nil
	err := c.Send(ctx, msg())
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted certificate accepted or unclear: %v", err)
	}
	if len(m.Messages()) != 0 || m.ClearTextCredentialsSeen() {
		t.Fatal("message or credentials went out after a failed verification")
	}
	// the server stops offering STARTTLS (downgrade): refuse, never continue in plaintext, never send credentials
	m = smtpmock.New(t, smtpmock.Options{TLS: "none", User: "u", Pass: "p"})
	err = client(m, "starttls", "u", "p").Send(ctx, msg())
	if err == nil || !isPermanent(err) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("downgrade not refused: %v", err)
	}
	if len(m.Messages()) != 0 || m.ClearTextCredentialsSeen() {
		t.Fatal("downgrade leaked credentials or mail")
	}
	// a server that advertises AUTH before TLS, on a host that is not loopback: the client must refuse to
	// send the password in clear, and say so
	m = smtpmock.New(t, smtpmock.Options{TLS: "none", User: "u", Pass: "p", AllowPlain: true, Bind: "127.0.0.2"})
	c = client(m, "none", "u", "p")
	c.Host = m.Host()
	err = c.Send(ctx, msg())
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("plaintext credentials to a remote host: %v", err)
	}
	if m.ClearTextCredentialsSeen() || len(m.Messages()) != 0 {
		t.Fatal("credentials or mail sent in clear to a non-local host")
	}
	// wrong credentials: permanent, clear message
	m = smtpmock.New(t, smtpmock.Options{TLS: "starttls", User: "u", Pass: "p"})
	err = client(m, "starttls", "u", "wrong").Send(ctx, msg())
	if err == nil || !isPermanent(err) || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("wrong credentials: %v", err)
	}
	// bad mode name
	if err := (&mail.SMTP{Host: "x", Port: 1, TLS: "ssl3"}).Send(ctx, msg()); !isPermanent(err) {
		t.Fatalf("bad mode: %v", err)
	}
}

func TestMAIL01_FailureClassification(t *testing.T) {
	ctx := context.Background()
	type tc struct {
		name      string
		b         smtpmock.Behavior
		permanent bool
	}
	for _, c := range []tc{
		{"greeting 421", smtpmock.Behavior{FailAt: smtpmock.Greeting, Code: 421}, false},
		{"mail from 451", smtpmock.Behavior{FailAt: smtpmock.MailFrom, Code: 451}, false},
		{"rcpt 450 greylist", smtpmock.Behavior{FailAt: smtpmock.RcptTo, Code: 450}, false},
		{"rcpt 550 no such user", smtpmock.Behavior{FailAt: smtpmock.RcptTo, Code: 550}, true},
		{"data 554", smtpmock.Behavior{FailAt: smtpmock.Data, Code: 554}, false},
		{"end 451", smtpmock.Behavior{FailAt: smtpmock.End, Code: 451}, false},
		{"end 552 too large", smtpmock.Behavior{FailAt: smtpmock.End, Code: 552}, false},
	} {
		m := smtpmock.New(t, smtpmock.Options{TLS: "none"})
		m.Script(c.b)
		err := client(m, "none", "", "").Send(ctx, msg())
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if isPermanent(err) != c.permanent {
			t.Errorf("%s: permanent=%v, want %v (%v)", c.name, isPermanent(err), c.permanent, err)
		}
		if len(m.Messages()) != 0 {
			t.Errorf("%s: a message was stored", c.name)
		}
	}
	// a slow server does not hang the client past its timeout
	m := smtpmock.New(t, smtpmock.Options{TLS: "none"})
	m.Script(smtpmock.Behavior{HangAt: smtpmock.Ehlo})
	c := client(m, "none", "", "")
	c.Timeout = 300 * time.Millisecond
	start := time.Now()
	if err := c.Send(ctx, msg()); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("hang not bounded: %v after %v", err, time.Since(start))
	}
	// a lost acknowledgement: the message is stored, the client sees an error, and a retry would duplicate it,
	// which is why the queue's idempotency key is per event and recipient (tested with the queue)
	m = smtpmock.New(t, smtpmock.Options{TLS: "none"})
	m.Script(smtpmock.Behavior{DropAtEnd: true})
	if err := client(m, "none", "", "").Send(ctx, msg()); err == nil {
		t.Fatal("dropped connection reported as success")
	}
	if len(m.Messages()) != 1 {
		t.Fatalf("stored %d", len(m.Messages()))
	}
	// connection refused
	m = smtpmock.New(t, smtpmock.Options{TLS: "none"})
	c = client(m, "none", "", "")
	c.Port = 1
	if err := c.Send(ctx, msg()); err == nil || isPermanent(err) {
		t.Fatalf("refused connection must be retryable: %v", err)
	}
}

func parse(t *testing.T, raw []byte) (*netmail.Message, string, string) {
	t.Helper()
	m, err := netmail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q %v", mt, err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var text, html string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(quotedprintable.NewReader(p))
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain") {
			text = string(b)
		} else if strings.HasPrefix(p.Header.Get("Content-Type"), "text/html") {
			html = string(b)
		}
	}
	return m, text, html
}

func TestMAIL03_MessageFormat(t *testing.T) {
	ctx := context.Background()
	m := smtpmock.New(t, smtpmock.Options{TLS: "none"})
	in := msg()
	in.Subject = "[ABC-1] Zahlung fehlgeschlagen für Über 日本語"
	in.Text = "Grüße 日本語\r\n.leading dot line\r\n-- Reply above this line --\r\n" + strings.Repeat("long line ", 200)
	in.HTML = "<p>Grüße</p>"
	in.ListUnsubscribe = "<mailto:unsubscribe@example.com>"
	in.ReplyTo = "tasklane+abc-1@example.com"
	if err := client(m, "none", "", "").Send(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, text, html := parse(t, m.Messages()[0].Raw)
	dec := new(mime.WordDecoder)
	if subj, _ := dec.DecodeHeader(got.Header.Get("Subject")); subj != in.Subject {
		t.Fatalf("subject %q", subj)
	}
	for k, want := range map[string]string{"Message-Id": "<issue.1.5@example.com>", "In-Reply-To": "<issue.1@example.com>", "References": "<issue.1@example.com>",
		"Auto-Submitted": "auto-generated", "List-Unsubscribe": "<mailto:unsubscribe@example.com>", "Reply-To": "tasklane+abc-1@example.com", "Mime-Version": "1.0"} {
		if got.Header.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Header.Get(k), want)
		}
	}
	if !strings.Contains(text, "Grüße 日本語") || !strings.Contains(text, "\n.leading dot line") || !strings.Contains(text, "Reply above this line") || html != "<p>Grüße</p>" {
		t.Fatalf("bodies round-trip: %q / %q", text, html)
	}
	// no remote content and no tracking in what the app generates is asserted with the notifier; here the raw message
	// must stay within line-length limits
	for _, l := range strings.Split(string(m.Messages()[0].Raw), "\r\n") {
		if len(l) > 998 {
			t.Fatalf("line of %d bytes", len(l))
		}
	}
}

func TestMAIL03_HeaderInjectionIsImpossible(t *testing.T) {
	in := msg()
	in.Subject = "hello\r\nBcc: attacker@example.com"
	in.To = "jana@example.com"
	in.ReplyTo = "x@example.com\r\nX-Evil: 1"
	raw, err := mail.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.SplitN(string(raw), "\r\n\r\n", 2)[0]
	for _, bad := range []string{"\r\nBcc:", "\r\nX-Evil:"} {
		if strings.Contains(head, bad) {
			t.Fatalf("injected header %q in:\n%s", bad, head)
		}
	}
	if _, err := mail.Build(mail.Message{To: "", MessageID: "<x@y>"}); err == nil {
		t.Fatal("message without recipient built")
	}
	// bad addresses are permanent errors before anything is sent
	m := smtpmock.New(t, smtpmock.Options{TLS: "none"})
	bad := msg()
	bad.To = "not an address"
	if err := client(m, "none", "", "").Send(context.Background(), bad); !isPermanent(err) {
		t.Fatalf("%v", err)
	}
	if m.Connections() > 1 && len(m.Messages()) != 0 {
		t.Fatal("sent to a bad address")
	}
}
