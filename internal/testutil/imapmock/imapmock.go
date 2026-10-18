// Package imapmock is the one shared in-process IMAP server for tests (TESTPLAN 3.2). It speaks the part of IMAP4rev1 a
// polling reader uses (LOGIN, CAPABILITY, SELECT, UID SEARCH, UID FETCH, IDLE, NOOP, LOGOUT), including the quirk that
// "UID SEARCH UID n:*" answers with the last message when n is above every UID, and it can change its UIDVALIDITY, drop a
// connection in the middle of a fetch, refuse a login and push new-mail notices to IDLE clients.
package imapmock

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type msg struct {
	uid uint32
	raw []byte
}

type Options struct {
	User, Pass string
	NoIdle     bool // do not advertise IDLE
}

type Mock struct {
	t                                 *testing.T
	ln                                net.Listener
	o                                 Options
	mu                                sync.Mutex
	msgs                              []msg
	next                              uint32
	valid                             uint32
	idlers                            map[chan struct{}]bool
	logins, fetches, conns, badLogins int
	dropAfter                         int // close the connection after this many more fetches (0 = never)
	failFetch                         map[uint32]bool
}

func New(t *testing.T, o Options) *Mock {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &Mock{t: t, ln: ln, o: o, next: 1, valid: 1000, idlers: map[chan struct{}]bool{}, failFetch: map[uint32]bool{}}
	go m.accept()
	t.Cleanup(func() { ln.Close() })
	return m
}

func (m *Mock) Port() int { return m.ln.Addr().(*net.TCPAddr).Port }

// Append adds a message and returns its UID; IDLE clients are told.
func (m *Mock) Append(raw []byte) uint32 {
	m.mu.Lock()
	uid := m.next
	m.next++
	m.msgs = append(m.msgs, msg{uid, raw})
	for ch := range m.idlers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	m.mu.Unlock()
	return uid
}

// Reset empties the mailbox and changes UIDVALIDITY, as a server does after it rebuilds a mailbox: the old UIDs mean nothing.
func (m *Mock) Reset(msgs ...[]byte) {
	m.mu.Lock()
	m.msgs, m.next = nil, 1
	m.valid++
	m.mu.Unlock()
	for _, r := range msgs {
		m.Append(r)
	}
}

func (m *Mock) DropAfterFetches(n int)        { m.mu.Lock(); m.dropAfter = n; m.mu.Unlock() }
func (m *Mock) FailFetch(uid uint32, on bool) { m.mu.Lock(); m.failFetch[uid] = on; m.mu.Unlock() }
func (m *Mock) Fetches() int                  { m.mu.Lock(); defer m.mu.Unlock(); return m.fetches }
func (m *Mock) Connections() int              { m.mu.Lock(); defer m.mu.Unlock(); return m.conns }
func (m *Mock) Logins() int                   { m.mu.Lock(); defer m.mu.Unlock(); return m.logins }
func (m *Mock) BadLogins() int                { m.mu.Lock(); defer m.mu.Unlock(); return m.badLogins }
func (m *Mock) Idlers() int                   { m.mu.Lock(); defer m.mu.Unlock(); return len(m.idlers) }
func (m *Mock) Count() int                    { m.mu.Lock(); defer m.mu.Unlock(); return len(m.msgs) }

func (m *Mock) accept() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.mu.Lock()
		m.conns++
		m.mu.Unlock()
		go m.serve(c)
	}
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s[1 : len(s)-1])
	}
	return s
}

func (m *Mock) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := func(f string, a ...any) { fmt.Fprintf(c, f+"\r\n", a...) }
	w("* OK IMAP4rev1 mock ready")
	authed := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.SplitN(strings.TrimRight(line, "\r\n"), " ", 3)
		if len(f) < 2 {
			w("* BAD")
			continue
		}
		tag, cmd := f[0], strings.ToUpper(f[1])
		rest := ""
		if len(f) == 3 {
			rest = f[2]
		}
		switch {
		case cmd == "CAPABILITY":
			caps := "IMAP4rev1 UIDPLUS"
			if !m.o.NoIdle {
				caps += " IDLE"
			}
			w("* CAPABILITY %s", caps)
			w("%s OK done", tag)
		case cmd == "LOGIN":
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) == 2 && unquote(parts[0]) == m.o.User && unquote(parts[1]) == m.o.Pass {
				authed = true
				m.mu.Lock()
				m.logins++
				m.mu.Unlock()
				w("%s OK logged in", tag)
			} else {
				m.mu.Lock()
				m.badLogins++
				m.mu.Unlock()
				w("%s NO [AUTHENTICATIONFAILED] invalid credentials", tag)
			}
		case !authed:
			w("%s NO log in first", tag)
		case cmd == "SELECT" || cmd == "EXAMINE":
			m.mu.Lock()
			n, valid, next := len(m.msgs), m.valid, m.next
			m.mu.Unlock()
			w("* %d EXISTS", n)
			w("* OK [UIDVALIDITY %d] valid", valid)
			w("* OK [UIDNEXT %d] next", next)
			w("%s OK [READ-WRITE] selected", tag)
		case cmd == "NOOP":
			w("%s OK", tag)
		case cmd == "LOGOUT":
			w("* BYE")
			w("%s OK", tag)
			return
		case cmd == "UID" && strings.HasPrefix(strings.ToUpper(rest), "SEARCH"):
			spec := strings.Fields(strings.ToUpper(rest))
			m.mu.Lock()
			var uids []string
			lo := uint32(1)
			if len(spec) >= 3 && spec[1] == "UID" {
				a, _, _ := strings.Cut(spec[2], ":")
				n, _ := strconv.ParseUint(a, 10, 32)
				lo = uint32(n)
			}
			var max uint32
			for _, x := range m.msgs {
				if x.uid > max {
					max = x.uid
				}
				if x.uid >= lo {
					uids = append(uids, strconv.FormatUint(uint64(x.uid), 10))
				}
			}
			if len(uids) == 0 && max > 0 { // "n:*" always includes the last message
				uids = append(uids, strconv.FormatUint(uint64(max), 10))
			}
			m.mu.Unlock()
			w("* SEARCH %s", strings.Join(uids, " "))
			w("%s OK done", tag)
		case cmd == "UID" && strings.HasPrefix(strings.ToUpper(rest), "FETCH"):
			fs := strings.Fields(rest)
			uid64, _ := strconv.ParseUint(fs[1], 10, 32)
			m.mu.Lock()
			m.fetches++
			drop := m.dropAfter > 0 && m.fetches >= m.dropAfter
			if drop {
				m.dropAfter = 0
			}
			failed := m.failFetch[uint32(uid64)]
			var found *msg
			for i := range m.msgs {
				if m.msgs[i].uid == uint32(uid64) {
					found = &m.msgs[i]
				}
			}
			m.mu.Unlock()
			if drop { // the connection dies in the middle of the answer
				if found != nil {
					fmt.Fprintf(c, "* 1 FETCH (UID %d BODY[] {%d}\r\n", found.uid, len(found.raw))
					c.Write(found.raw[:len(found.raw)/2])
				}
				return
			}
			if failed || found == nil {
				w("%s NO message not available", tag)
				continue
			}
			fmt.Fprintf(c, "* 1 FETCH (UID %d BODY[] {%d}\r\n", found.uid, len(found.raw))
			c.Write(found.raw)
			w(")")
			w("%s OK done", tag)
		case cmd == "IDLE":
			if m.o.NoIdle {
				w("%s BAD IDLE not offered", tag)
				continue
			}
			ch := make(chan struct{}, 4)
			m.mu.Lock()
			m.idlers[ch] = true
			m.mu.Unlock()
			w("+ idling")
			done := make(chan struct{})
			go func() { // DONE ends it
				l, _ := r.ReadString('\n')
				_ = l
				close(done)
			}()
		idle:
			for {
				select {
				case <-ch:
					m.mu.Lock()
					n := len(m.msgs)
					m.mu.Unlock()
					w("* %d EXISTS", n)
				case <-done:
					break idle
				case <-time.After(30 * time.Second):
					break idle
				}
			}
			m.mu.Lock()
			delete(m.idlers, ch)
			m.mu.Unlock()
			w("%s OK IDLE terminated", tag)
		default:
			w("%s BAD unknown command", tag)
		}
	}
}
