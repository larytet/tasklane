// Package ldapmock is the one shared in-process LDAP v3 directory for tests (TESTPLAN 3.2). It speaks what a sign-in uses: simple
// bind, StartTLS, subtree search with the usual filters (and Active Directory's nested-group rule on memberOf), unbind; and it can
// listen with TLS from the start (ldaps), refuse StartTLS and record every bind it was asked for. Like many real servers it
// treats a bind with an empty password as an anonymous bind and answers success, which is the trap a client must not fall into.
package ldapmock

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/ldap"
)

// InChain is Active Directory's LDAP_MATCHING_RULE_IN_CHAIN: memberOf matches through nested groups.
const InChain = "1.2.840.113556.1.4.1941"

type Entry struct {
	DN       string
	Password string
	Attrs    map[string][]string
	Groups   []string // the groups the person is in, directly or through nesting (what memberOf:InChain sees)
}

type Options struct {
	TLS        bool // listen with TLS from the first byte (ldaps://)
	NoStartTLS bool // refuse the StartTLS extended operation
	Anonymous  bool // allow searching without a bind
}

type Mock struct {
	t       *testing.T
	ln      net.Listener
	opts    Options
	cert    tls.Certificate
	caPEM   string
	mu      sync.Mutex
	entries []Entry
	binds   []string
	plain   []string // the binds that arrived on a connection that was not encrypted
	down    bool
}

func New(t *testing.T, opts Options, entries ...Entry) *Mock {
	m := &Mock{t: t, opts: opts, entries: entries}
	m.makeCerts()
	var err error
	if opts.TLS {
		m.ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{m.cert}})
	} else {
		m.ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.ln.Close() })
	go m.serve()
	return m
}

// URL is the address to configure: ldaps://127.0.0.1:port or ldap://127.0.0.1:port.
func (m *Mock) URL() string {
	if m.opts.TLS {
		return "ldaps://" + m.ln.Addr().String()
	}
	return "ldap://" + m.ln.Addr().String()
}

// CAPEM is the certificate of the authority that signed the server's certificate.
func (m *Mock) CAPEM() string { return m.caPEM }

// Binds are the DNs bound as, in order (an anonymous bind is recorded as "").
func (m *Mock) Binds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.binds...)
}

// PlainBinds are the binds that were sent over a connection without TLS.
func (m *Mock) PlainBinds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.plain...)
}

// Down makes the directory hang up on every new connection, as a stopped server would.
func (m *Mock) Down(d bool) { m.mu.Lock(); m.down = d; m.mu.Unlock() }

// Update replaces a person's entry (by DN), for a password change or a disabled account in the middle of a test.
func (m *Mock) Update(e Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.entries {
		if strings.EqualFold(m.entries[i].DN, e.DN) {
			m.entries[i] = e
			return
		}
	}
	m.entries = append(m.entries, e)
}

func (m *Mock) Remove(dn string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.entries {
		if strings.EqualFold(m.entries[i].DN, dn) {
			m.entries = append(m.entries[:i], m.entries[i+1:]...)
			return
		}
	}
}

func (m *Mock) makeCerts() {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ldapmock test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	m.caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
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
		down := m.down
		m.mu.Unlock()
		if down {
			c.Close()
			continue
		}
		go m.session(c)
	}
}

func str(s string) *ldap.Packet { return ldap.Str(ldap.ClassUniversal, ldap.TagOctetString, s) }

func result(app int, code int64, msg string, extra ...*ldap.Packet) *ldap.Packet {
	return ldap.Seq(ldap.ClassApplication, app, append([]*ldap.Packet{ldap.Int(ldap.TagEnumerated, code), str(""), str(msg)}, extra...)...)
}

func (m *Mock) session(c net.Conn) {
	defer func() { c.Close() }()
	encrypted := m.opts.TLS
	r := bufio.NewReader(c)
	authed := false // anonymous until a bind with a password succeeds
	for {
		c.SetDeadline(time.Now().Add(30 * time.Second))
		msg, err := ldap.ReadPacket(r)
		if err != nil || len(msg.Children) < 2 {
			return
		}
		id, _ := msg.Children[0].IntValue()
		op := msg.Children[1]
		reply := func(p *ldap.Packet) bool {
			_, err := c.Write(ldap.Seq(ldap.ClassUniversal, ldap.TagSequence, ldap.Int(ldap.TagInteger, id), p).Encode())
			return err == nil
		}
		switch {
		case op.Is(ldap.ClassApplication, 2): // unbind
			return
		case op.Is(ldap.ClassApplication, 23): // extended: only StartTLS
			if len(op.Children) == 0 || string(op.Children[0].Value) != "1.3.6.1.4.1.1466.20037" || encrypted || m.opts.NoStartTLS {
				reply(result(24, 2, "unsupported"))
				continue
			}
			if !reply(result(24, 0, "")) {
				return
			}
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{m.cert}})
			if tc.Handshake() != nil {
				return
			}
			c, r, encrypted = tc, bufio.NewReader(tc), true
		case op.Is(ldap.ClassApplication, 0): // bind
			if len(op.Children) < 3 {
				return
			}
			dn, pw := string(op.Children[1].Value), string(op.Children[2].Value)
			m.mu.Lock()
			m.binds = append(m.binds, dn)
			if !encrypted {
				m.plain = append(m.plain, dn)
			}
			ok := pw == "" // anonymous bind: the trap
			for _, e := range m.entries {
				if strings.EqualFold(e.DN, dn) && e.Password != "" && e.Password == pw {
					ok = true
				}
			}
			m.mu.Unlock()
			if !ok {
				authed = false
				reply(result(1, 49, "80090308: LdapErr: DSID-0C09044E, comment: AcceptSecurityContext error, data 52e, v4563"))
				continue
			}
			authed = pw != ""
			reply(result(1, 0, ""))
		case op.Is(ldap.ClassApplication, 3): // search
			if !authed && !m.opts.Anonymous {
				reply(result(5, 1, "000004DC: LdapErr: DSID-0C090A5C, comment: In order to perform this operation a successful bind must be completed on the connection."))
				continue
			}
			base := strings.ToLower(string(op.Children[0].Value))
			limit, _ := op.Children[3].IntValue()
			filter := op.Children[6]
			var hits []Entry
			m.mu.Lock()
			for _, e := range m.entries {
				if strings.HasSuffix(strings.ToLower(e.DN), base) && match(filter, e) {
					hits = append(hits, e)
				}
			}
			m.mu.Unlock()
			code := int64(0)
			if limit > 0 && int64(len(hits)) > limit {
				hits, code = hits[:limit], 4
			}
			for _, e := range hits {
				attrs := ldap.Seq(ldap.ClassUniversal, ldap.TagSequence)
				for k, vs := range e.Attrs {
					set := ldap.Seq(ldap.ClassUniversal, ldap.TagSet)
					for _, v := range vs {
						set.Children = append(set.Children, str(v))
					}
					attrs.Children = append(attrs.Children, ldap.Seq(ldap.ClassUniversal, ldap.TagSequence, str(k), set))
				}
				if !reply(ldap.Seq(ldap.ClassApplication, 4, str(e.DN), attrs)) {
					return
				}
			}
			reply(result(5, code, ""))
		default:
			return
		}
	}
}

func (e Entry) values(attr string) []string {
	for k, v := range e.Attrs {
		if strings.EqualFold(k, attr) {
			return v
		}
	}
	return nil
}

// match evaluates a filter against an entry, comparing case-insensitively as Active Directory does for names.
func match(f *ldap.Packet, e Entry) bool {
	if f.Class != ldap.ClassContext {
		return false
	}
	switch f.Tag {
	case ldap.FilterAnd:
		for _, c := range f.Children {
			if !match(c, e) {
				return false
			}
		}
		return true
	case ldap.FilterOr:
		for _, c := range f.Children {
			if match(c, e) {
				return true
			}
		}
		return false
	case ldap.FilterNot:
		return len(f.Children) == 1 && !match(f.Children[0], e)
	case ldap.FilterPresent:
		return len(e.values(string(f.Value))) > 0
	case ldap.FilterEquality, ldap.FilterApprox:
		if len(f.Children) != 2 {
			return false
		}
		for _, v := range e.values(string(f.Children[0].Value)) {
			if strings.EqualFold(v, string(f.Children[1].Value)) {
				return true
			}
		}
		return false
	case ldap.FilterSubstrings:
		if len(f.Children) != 2 {
			return false
		}
		for _, v := range e.values(string(f.Children[0].Value)) {
			s, ok := strings.ToLower(v), true
			for _, part := range f.Children[1].Children {
				p := strings.ToLower(string(part.Value))
				switch part.Tag {
				case 0:
					ok = ok && strings.HasPrefix(s, p)
					s = strings.TrimPrefix(s, p)
				case 2:
					ok = ok && strings.HasSuffix(s, p)
				default:
					i := strings.Index(s, p)
					ok = ok && i >= 0
					if i >= 0 {
						s = s[i+len(p):]
					}
				}
			}
			if ok {
				return true
			}
		}
		return false
	case ldap.FilterExtensible:
		var rule, attr, val string
		for _, c := range f.Children {
			switch c.Tag {
			case 1:
				rule = string(c.Value)
			case 2:
				attr = string(c.Value)
			case 3:
				val = string(c.Value)
			}
		}
		vals := e.values(attr)
		if rule == InChain && strings.EqualFold(attr, "memberOf") {
			vals = e.Groups
		}
		if rule == "1.2.840.113556.1.4.803" { // bit and: userAccountControl:...:=2 is "disabled"
			want := atoi(val)
			for _, v := range vals {
				if atoi(v)&want == want && want != 0 {
					return true
				}
			}
			return false
		}
		for _, v := range vals {
			if strings.EqualFold(v, val) {
				return true
			}
		}
		return false
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
