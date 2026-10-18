package mail_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/testutil/imapmock"
)

func imapCfg(m *imapmock.Mock) mail.IMAPConfig {
	return mail.IMAPConfig{Host: "127.0.0.1", Port: m.Port(), TLS: "none", User: "box", Pass: "p\"w\\d", AllowPrivate: true, Timeout: 3 * time.Second}
}

func TestMAIL09_IMAPClientLoginSelectSearchFetch(t *testing.T) {
	m := imapmock.New(t, imapmock.Options{User: "box", Pass: `p"w\d`})
	m1 := []byte("Subject: one\r\n\r\nfirst message\r\n")
	big := append([]byte("Subject: big\r\n\r\n"), bytes.Repeat([]byte("x"), 100000)...)
	m.Append(m1)
	m.Append(big)
	c, err := mail.DialIMAP(context.Background(), imapCfg(m))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Valid != 1000 || c.Next != 3 || c.Count != 2 || !c.Idle {
		t.Fatalf("%+v", c)
	}
	uids, err := c.UIDsAfter(0)
	if err != nil || len(uids) != 2 || uids[0] != 1 || uids[1] != 2 {
		t.Fatalf("%v %v", uids, err)
	}
	// "UID SEARCH 3:*" answers with the last message on a real server; the client filters it out
	if uids, err := c.UIDsAfter(2); err != nil || len(uids) != 0 {
		t.Fatalf("an old message came back as new: %v %v", uids, err)
	}
	got, err := c.Fetch(1, 1<<20)
	if err != nil || !bytes.Equal(got, m1) {
		t.Fatalf("%q %v", got, err)
	}
	// a message over the limit is cut at limit+1 so the caller can see it is too large, and the connection stays usable
	cut, err := c.Fetch(2, 1000)
	if err != nil || len(cut) != 1001 {
		t.Fatalf("%d %v", len(cut), err)
	}
	if got, err := c.Fetch(1, 1<<20); err != nil || !bytes.Equal(got, m1) {
		t.Fatalf("the connection was damaged by a cut message: %v", err)
	}
	m.FailFetch(1, true)
	if _, err := c.Fetch(1, 1<<20); err == nil {
		t.Fatal("a refused fetch was not an error")
	}
	if _, err := c.Fetch(99, 1<<20); err == nil {
		t.Fatal("a missing message was not an error")
	}
}

func TestMAIL09_IMAPClientRefusesWhatIsUnsafe(t *testing.T) {
	m := imapmock.New(t, imapmock.Options{User: "box", Pass: "pw"})
	base := imapCfg(m)
	base.Pass = "pw"
	ctx := context.Background()
	bad := base
	bad.Pass = "wrong"
	if _, err := mail.DialIMAP(ctx, bad); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("%v", err)
	}
	inj := base
	inj.User = "box\r\nt9 DELETE INBOX"
	if _, err := mail.DialIMAP(ctx, inj); err == nil {
		t.Fatal("a line break in a credential was sent")
	}
	// the egress policy: a loopback server needs it to be allowed
	before := m.Connections()
	blocked := base
	blocked.AllowPrivate = false
	if _, err := mail.DialIMAP(ctx, blocked); err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("%v", err)
	}
	if m.Connections() != before {
		t.Fatalf("%d connections were opened to a blocked address", m.Connections()-before)
	}
	// no password in the clear to a host that is not on this machine: 192.0.2.1 is a documentation address, never contacted
	remote := base
	remote.Host, remote.AllowPrivate = "192.0.2.1", true
	if _, err := mail.DialIMAP(ctx, remote); err == nil || !strings.Contains(err.Error(), "without TLS") {
		t.Fatalf("%v", err)
	}
	// and TLS against a server that does not speak it fails rather than falling back
	tlsCfg := base
	tlsCfg.TLS = "tls"
	if _, err := mail.DialIMAP(ctx, tlsCfg); err == nil {
		t.Fatal("a TLS connection to a plaintext server succeeded")
	}
	if m.Logins() != 0 || m.BadLogins() != 1 {
		t.Fatalf("logins %d bad %d (a credential must never reach a server over a failed or refused connection)", m.Logins(), m.BadLogins())
	}
}

func TestMAIL09_IMAPIdleWakesOnNewMailAndTimesOut(t *testing.T) {
	m := imapmock.New(t, imapmock.Options{User: "box", Pass: "p\"w\\d"})
	cfg := imapCfg(m)
	cfg.Pass = `p"w\d`
	c, err := mail.DialIMAP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// nothing arrives: the wait ends at its limit and says so
	start := time.Now()
	if got, err := c.WaitNew(context.Background(), 300*time.Millisecond); err != nil || got || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("%v %v %v", got, err, time.Since(start))
	}
	// a message arrives while waiting: the wait ends at once
	go func() { time.Sleep(150 * time.Millisecond); m.Append([]byte("Subject: x\r\n\r\ny\r\n")) }()
	start = time.Now()
	got, err := c.WaitNew(context.Background(), 5*time.Second)
	if err != nil || !got || time.Since(start) > time.Second {
		t.Fatalf("%v %v %v", got, err, time.Since(start))
	}
	// the connection is good for a command afterwards
	if uids, err := c.UIDsAfter(0); err != nil || len(uids) != 1 {
		t.Fatalf("%v %v", uids, err)
	}
	// a cancelled context ends the wait
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start = time.Now()
	if _, err := c.WaitNew(ctx, 10*time.Second); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("%v %v", err, time.Since(start))
	}
	// a server without IDLE is reported, not hung on
	n := imapmock.New(t, imapmock.Options{User: "box", Pass: "pw", NoIdle: true})
	nc := imapCfg(n)
	nc.Pass = "pw"
	c2, err := mail.DialIMAP(context.Background(), nc)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if c2.Idle {
		t.Fatal("IDLE was assumed")
	}
	if _, err := c2.WaitNew(context.Background(), time.Second); err == nil {
		t.Fatal("waited on a server without IDLE")
	}
}
