package ldap

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/egress"
)

var (
	// ErrInvalidCredentials is the one answer for an unknown person, a wrong password and an ambiguous name, so the reply does not
	// say which accounts exist.
	ErrInvalidCredentials = errors.New("ldap: invalid credentials")
	// ErrUnavailable means no server could be asked; the person is told sign-in failed, the log says why.
	ErrUnavailable = errors.New("ldap: directory unavailable")
)

const (
	appBindRequest      = 0
	appBindResponse     = 1
	appUnbindRequest    = 2
	appSearchRequest    = 3
	appSearchEntry      = 4
	appSearchDone       = 5
	appSearchReference  = 19
	appExtendedRequest  = 23
	appExtendedResponse = 24

	resultSuccess            = 0
	resultInvalidCredentials = 49

	oidStartTLS = "1.3.6.1.4.1.1466.20037"

	// DefaultUserFilter finds a person by the name they type in Active Directory (sAMAccountName or userPrincipalName) or in
	// OpenLDAP (uid), or by their address.
	DefaultUserFilter = "(|(sAMAccountName={user})(userPrincipalName={user})(uid={user})(mail={user}))"
)

// Config says where the directory is and how people are found in it.
type Config struct {
	URLs               []string // ldap://host[:389] or ldaps://host[:636], tried in order until one answers
	StartTLS           bool     // upgrade an ldap:// connection before anything is sent
	InsecureSkipVerify bool     // accept any server certificate (a directory with a private CA that is not given)
	RootCAs            *x509.CertPool
	BindDN             string // the account that searches; empty: search anonymously
	BindPassword       string
	BaseDN             string
	UserFilter         string // contains {user}, replaced by the escaped name the person typed
	EmailAttr          string // default mail
	NameAttr           string // default displayName, then cn
	AllowPrivate       bool   // the egress policy: private and loopback addresses need this
	Timeout            time.Duration
}

// Entry is the person the directory vouched for.
type Entry struct {
	DN      string
	Subject string // a stable identifier: objectGUID (Active Directory), entryUUID (OpenLDAP), else the DN
	Email   string
	Name    string
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

// CheckURL accepts ldap:// and ldaps:// addresses with a host and nothing else.
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("%q is not an ldap:// or ldaps:// server address", raw)
	}
	return u, nil
}

// CheckUserFilter makes sure a filter template is usable: it names {user} and parses once filled in.
func CheckUserFilter(f string) error {
	if !strings.Contains(f, "{user}") {
		return fmt.Errorf("%w: the user filter must contain {user}", ErrFilter)
	}
	_, err := ParseFilter(strings.ReplaceAll(f, "{user}", "x"))
	return err
}

// Authenticate finds the person by the name they typed and checks their password by binding as them. Nothing is cached: every
// call asks the directory, so a disabled or removed account is refused at once.
func Authenticate(ctx context.Context, cfg Config, login, password string) (Entry, error) {
	login = strings.TrimSpace(login)
	if i := strings.LastIndexByte(login, '\\'); i >= 0 { // DOMAIN\name, as Windows people type it
		login = login[i+1:]
	}
	if login == "" || password == "" { // an empty password would be an anonymous bind, which most servers accept
		return Entry{}, ErrInvalidCredentials
	}
	filter := cfg.UserFilter
	if filter == "" {
		filter = DefaultUserFilter
	}
	f, err := ParseFilter(strings.ReplaceAll(filter, "{user}", EscapeFilter(login)))
	if err != nil {
		return Entry{}, err
	}
	c, err := dial(ctx, cfg)
	if err != nil {
		return Entry{}, err
	}
	defer c.close()
	if cfg.BindDN != "" {
		if err := c.bind(cfg.BindDN, cfg.BindPassword); err != nil {
			if errors.Is(err, ErrInvalidCredentials) {
				return Entry{}, fmt.Errorf("%w: the search account was refused", ErrUnavailable)
			}
			return Entry{}, err
		}
	}
	emailAttr, nameAttr := cfg.EmailAttr, cfg.NameAttr
	if emailAttr == "" {
		emailAttr = "mail"
	}
	attrs := []string{"objectGUID", "entryUUID", emailAttr, "displayName", "cn"}
	if nameAttr != "" {
		attrs = append(attrs, nameAttr)
	}
	found, err := c.search(cfg.BaseDN, f, attrs)
	if err != nil {
		return Entry{}, err
	}
	if len(found) != 1 { // nobody, or a name that matches more than one person: refuse rather than guess
		return Entry{}, ErrInvalidCredentials
	}
	e := found[0]
	if err := c.bind(e.dn, password); err != nil {
		return Entry{}, err
	}
	out := Entry{DN: e.dn, Email: e.first(emailAttr), Name: e.first(nameAttr)}
	if out.Name == "" {
		out.Name = e.first("displayName")
	}
	if out.Name == "" {
		out.Name = e.first("cn")
	}
	switch {
	case e.first("objectGUID") != "":
		out.Subject = "guid:" + hex.EncodeToString([]byte(e.first("objectGUID")))
	case e.first("entryUUID") != "":
		out.Subject = "uuid:" + strings.ToLower(e.first("entryUUID"))
	default:
		out.Subject = "dn:" + strings.ToLower(e.dn)
	}
	return out, nil
}

type conn struct {
	c   net.Conn
	r   *bufio.Reader
	id  int64
	to  time.Duration
	tls bool
}

func dial(ctx context.Context, cfg Config) (*conn, error) {
	if len(cfg.URLs) == 0 {
		return nil, fmt.Errorf("%w: no server address", ErrUnavailable)
	}
	var last error
	for _, raw := range cfg.URLs {
		c, err := dialOne(ctx, cfg, raw)
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func dialOne(ctx context.Context, cfg Config, raw string) (*conn, error) {
	u, err := CheckURL(raw)
	if err != nil {
		return nil, err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"ldap": "389", "ldaps": "636"}[u.Scheme]
	}
	pol := egress.Policy{AllowPrivate: cfg.AllowPrivate}
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("%w: cannot resolve %s", ErrUnavailable, host)
		}
		ips = ips[:0]
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	for _, ip := range ips {
		if err := pol.Check(ip); err != nil { // one bad answer is enough to refuse: rebinding tricks mix them
			return nil, err
		}
	}
	if u.Scheme == "ldap" && !cfg.StartTLS && !ips[0].IsLoopback() {
		return nil, fmt.Errorf("%w: passwords are not sent unencrypted to %s; use ldaps:// or StartTLS", ErrUnavailable, host)
	}
	d := net.Dialer{Timeout: cfg.timeout()}
	raw2, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].String(), port))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	tc := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: cfg.RootCAs, InsecureSkipVerify: cfg.InsecureSkipVerify}
	c := &conn{c: raw2, to: cfg.timeout()}
	if u.Scheme == "ldaps" {
		c.c, c.tls = tls.Client(raw2, tc), true
	}
	c.r = bufio.NewReader(c.c)
	if u.Scheme == "ldap" && cfg.StartTLS {
		if err := c.startTLS(tc); err != nil {
			raw2.Close()
			return nil, err
		}
	}
	return c, nil
}

func (c *conn) close() {
	c.id++
	c.c.SetDeadline(time.Now().Add(time.Second))
	c.c.Write(Seq(ClassUniversal, TagSequence, Int(TagInteger, c.id), &Packet{Class: ClassApplication, Tag: appUnbindRequest}).Encode())
	c.c.Close()
}

func (c *conn) send(op *Packet) (int64, error) {
	c.id++
	msg := Seq(ClassUniversal, TagSequence, Int(TagInteger, c.id), op)
	c.c.SetDeadline(time.Now().Add(c.to))
	_, err := c.c.Write(msg.Encode())
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return c.id, nil
}

// next reads the next message for id and returns its protocol operation.
func (c *conn) next(id int64) (*Packet, error) {
	c.c.SetDeadline(time.Now().Add(c.to))
	m, err := ReadPacket(c.r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !m.Is(ClassUniversal, TagSequence) || len(m.Children) < 2 {
		return nil, errBER
	}
	got, err := m.Children[0].IntValue()
	if err != nil {
		return nil, err
	}
	if got != id { // id 0 is the server's notice of disconnection; anything else is out of step
		return nil, fmt.Errorf("%w: unexpected message %d", ErrUnavailable, got)
	}
	return m.Children[1], nil
}

// result reads an LDAPResult: the code and the server's diagnostic message.
func result(op *Packet) (int64, string, error) {
	if len(op.Children) < 3 {
		return 0, "", errBER
	}
	code, err := op.Children[0].IntValue()
	return code, string(op.Children[2].Value), err
}

func (c *conn) startTLS(tc *tls.Config) error {
	id, err := c.send(Seq(ClassApplication, appExtendedRequest, Str(ClassContext, 0, oidStartTLS)))
	if err != nil {
		return err
	}
	op, err := c.next(id)
	if err != nil {
		return err
	}
	if !op.Is(ClassApplication, appExtendedResponse) {
		return errBER
	}
	if code, msg, err := result(op); err != nil || code != resultSuccess {
		return fmt.Errorf("%w: StartTLS refused (%d %s)", ErrUnavailable, code, msg)
	}
	tconn := tls.Client(c.c, tc)
	tconn.SetDeadline(time.Now().Add(c.to))
	if err := tconn.Handshake(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	c.c, c.r, c.tls = tconn, bufio.NewReader(tconn), true
	return nil
}

func (c *conn) bind(dn, password string) error {
	if password == "" {
		return ErrInvalidCredentials
	}
	id, err := c.send(Seq(ClassApplication, appBindRequest, Int(TagInteger, 3), Str(ClassUniversal, TagOctetString, dn), Str(ClassContext, 0, password)))
	if err != nil {
		return err
	}
	op, err := c.next(id)
	if err != nil {
		return err
	}
	if !op.Is(ClassApplication, appBindResponse) {
		return errBER
	}
	code, msg, err := result(op)
	switch {
	case err != nil:
		return err
	case code == resultSuccess:
		return nil
	case code == resultInvalidCredentials:
		return ErrInvalidCredentials
	}
	return fmt.Errorf("%w: bind answered %d %s", ErrUnavailable, code, msg)
}

type found struct {
	dn    string
	attrs map[string][]string
}

func (f found) first(attr string) string {
	if v := f.attrs[strings.ToLower(attr)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (c *conn) search(base string, filter *Packet, attrs []string) ([]found, error) {
	al := Seq(ClassUniversal, TagSequence)
	for _, a := range attrs {
		al.Children = append(al.Children, Str(ClassUniversal, TagOctetString, a))
	}
	req := Seq(ClassApplication, appSearchRequest, Str(ClassUniversal, TagOctetString, base), Int(TagEnumerated, 2) /* whole subtree */, Int(TagEnumerated, 0), /* never dereference aliases */
		Int(TagInteger, 2) /* two answers are enough to know the name is ambiguous */, Int(TagInteger, int64(c.to/time.Second)), Bool(false), filter, al)
	id, err := c.send(req)
	if err != nil {
		return nil, err
	}
	var out []found
	for {
		op, err := c.next(id)
		if err != nil {
			return nil, err
		}
		switch {
		case op.Is(ClassApplication, appSearchEntry):
			if len(op.Children) < 2 {
				return nil, errBER
			}
			f := found{dn: string(op.Children[0].Value), attrs: map[string][]string{}}
			for _, a := range op.Children[1].Children {
				if len(a.Children) < 2 {
					continue
				}
				k := strings.ToLower(string(a.Children[0].Value))
				for _, v := range a.Children[1].Children {
					f.attrs[k] = append(f.attrs[k], string(v.Value))
				}
			}
			out = append(out, f)
		case op.Is(ClassApplication, appSearchReference): // a referral to another server: not followed
		case op.Is(ClassApplication, appSearchDone):
			code, msg, err := result(op)
			if err != nil {
				return nil, err
			}
			if code == 4 && len(out) > 1 { // size limit exceeded: more than one match, which is what we wanted to know
				return out, nil
			}
			if code != resultSuccess {
				return nil, fmt.Errorf("%w: search answered %d %s", ErrUnavailable, code, msg)
			}
			return out, nil
		default:
			return nil, errBER
		}
	}
}
