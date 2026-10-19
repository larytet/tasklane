// Package redismock is an in-process Redis (RESP2) for tests: AUTH, SELECT, PING, SUBSCRIBE and PUBLISH, with the failures the cluster tests
// need: the whole server going away and coming back on the same address, and one client (by its AUTH user name) being cut off.
package redismock

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type Server struct {
	t         testing.TB
	mu        sync.Mutex
	ln        net.Listener
	addr      string
	Password  string
	conns     map[*client]bool
	cut       map[string]bool
	subs      map[string]map[*client]bool
	Published int
}

type client struct {
	c    net.Conn
	user string
	wmu  sync.Mutex
}

func New(t testing.TB) *Server {
	s := &Server{t: t, conns: map[*client]bool{}, cut: map[string]bool{}, subs: map[string]map[*client]bool{}}
	s.Start()
	t.Cleanup(s.Stop)
	return s
}

// Addr is host:port; it stays the same across Stop and Start.
func (s *Server) Addr() string { return s.addr }

func (s *Server) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		return
	}
	a := s.addr
	if a == "" {
		a = "127.0.0.1:0"
	}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port may linger for a moment after a stop
		if ln, err = net.Listen("tcp", a); err == nil {
			break
		}
		sleepMS(20)
	}
	if err != nil {
		s.t.Fatalf("redismock: %v", err)
	}
	s.ln, s.addr = ln, ln.Addr().String()
	go s.accept(ln)
}

// Stop closes the listener and every connection, as a Redis that went down does.
func (s *Server) Stop() {
	s.mu.Lock()
	ln := s.ln
	s.ln = nil
	cs := make([]*client, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.conns = map[*client]bool{}
	s.subs = map[string]map[*client]bool{}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, c := range cs {
		c.c.Close()
	}
}

// Cut disconnects every client that authenticated as user and refuses it until Heal (a partition between that node and Redis).
func (s *Server) Cut(user string) {
	s.mu.Lock()
	s.cut[user] = true
	var victims []*client
	for c := range s.conns {
		if c.user == user {
			victims = append(victims, c)
		}
	}
	s.mu.Unlock()
	for _, c := range victims {
		c.c.Close()
	}
}

func (s *Server) Heal(user string) {
	s.mu.Lock()
	delete(s.cut, user)
	s.mu.Unlock()
}

// Subscribers counts the connections subscribed to a channel.
func (s *Server) Subscribers(channel string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[channel])
}

func (s *Server) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		cl := &client{c: c}
		s.mu.Lock()
		s.conns[cl] = true
		s.mu.Unlock()
		go s.serve(cl)
	}
}

func (s *Server) drop(cl *client) {
	s.mu.Lock()
	delete(s.conns, cl)
	for _, m := range s.subs {
		delete(m, cl)
	}
	s.mu.Unlock()
	cl.c.Close()
}

func (cl *client) send(format string, a ...any) error {
	cl.wmu.Lock()
	defer cl.wmu.Unlock()
	_, err := fmt.Fprintf(cl.c, format, a...)
	return err
}

func readCmd(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return strings.Fields(line), nil
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		l, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sz, _ := strconv.Atoi(strings.TrimSpace(l[1:]))
		buf := make([]byte, sz+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		out = append(out, string(buf[:sz]))
	}
	return out, nil
}

func (s *Server) serve(cl *client) {
	defer s.drop(cl)
	r := bufio.NewReader(cl.c)
	authed := s.Password == ""
	for {
		args, err := readCmd(r)
		if err != nil || len(args) == 0 {
			return
		}
		switch strings.ToUpper(args[0]) {
		case "AUTH":
			pass := args[len(args)-1]
			user := ""
			if len(args) == 3 {
				user = args[1]
			}
			s.mu.Lock()
			blocked := s.cut[user]
			s.mu.Unlock()
			if blocked {
				return // the partition: the connection simply dies
			}
			if s.Password != "" && pass != s.Password {
				cl.send("-WRONGPASS invalid username-password pair\r\n")
				continue
			}
			cl.user, authed = user, true
			cl.send("+OK\r\n")
		case "SELECT":
			cl.send("+OK\r\n")
		case "PING":
			if !authed {
				cl.send("-NOAUTH Authentication required.\r\n")
				continue
			}
			cl.send("+PONG\r\n")
		case "INFO":
			body := "# Server\r\nredis_version:7.0.0-mock\r\n"
			cl.send("$%d\r\n%s\r\n", len(body), body)
		case "SUBSCRIBE":
			if !authed {
				cl.send("-NOAUTH Authentication required.\r\n")
				continue
			}
			for i, ch := range args[1:] {
				s.mu.Lock()
				if s.subs[ch] == nil {
					s.subs[ch] = map[*client]bool{}
				}
				s.subs[ch][cl] = true
				s.mu.Unlock()
				cl.send("*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(ch), ch, i+1)
			}
		case "PUBLISH":
			if !authed {
				cl.send("-NOAUTH Authentication required.\r\n")
				continue
			}
			ch, payload := args[1], args[2]
			s.mu.Lock()
			s.Published++
			var targets []*client
			for c := range s.subs[ch] {
				targets = append(targets, c)
			}
			s.mu.Unlock()
			for _, c := range targets {
				c.send("*3\r\n$7\r\nmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(ch), ch, len(payload), payload)
			}
			cl.send(":%d\r\n", len(targets))
		default:
			cl.send("-ERR unknown command\r\n")
		}
	}
}
