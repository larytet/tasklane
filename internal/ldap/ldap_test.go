package ldap_test

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/ldap"
	"github.com/larytet/tasklane/internal/testutil/ldapmock"
)

var ctx = context.Background()

const (
	base    = "dc=corp,dc=example"
	svcDN   = "cn=svc-tasklane,ou=service,dc=corp,dc=example"
	svcPW   = "svc-secret"
	adminsG = "cn=tasklane-users,ou=groups,dc=corp,dc=example"
)

func people() []ldapmock.Entry {
	return []ldapmock.Entry{
		{DN: svcDN, Password: svcPW, Attrs: map[string][]string{"cn": {"svc-tasklane"}}},
		{DN: "cn=Jana Kova,ou=people,dc=corp,dc=example", Password: "jana-pw", Groups: []string{adminsG},
			Attrs: map[string][]string{"sAMAccountName": {"jana.k"}, "userPrincipalName": {"jana.k@corp.example"}, "mail": {"Jana.K@Example.com"}, "displayName": {"Jana Kova"}, "objectGUID": {"\x01\x02\x03\x04guid-of-jana"}}},
		{DN: "cn=Ravi S,ou=people,dc=corp,dc=example", Password: "ravi-pw",
			Attrs: map[string][]string{"sAMAccountName": {"ravi.s"}, "mail": {"ravi@example.com"}, "cn": {"Ravi S"}, "userAccountControl": {"514"}}},
		{DN: "cn=Twin A,ou=people,dc=corp,dc=example", Password: "a", Attrs: map[string][]string{"sAMAccountName": {"twin"}, "mail": {"a@example.com"}}},
		{DN: "cn=Twin B,ou=people,dc=corp,dc=example", Password: "b", Attrs: map[string][]string{"sAMAccountName": {"twin"}, "mail": {"b@example.com"}}},
	}
}

func cfgFor(t *testing.T, m *ldapmock.Mock) ldap.Config {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(m.CAPEM())) {
		t.Fatal("ca")
	}
	return ldap.Config{URLs: []string{m.URL()}, StartTLS: !strings.HasPrefix(m.URL(), "ldaps:"), RootCAs: pool, BindDN: svcDN, BindPassword: svcPW, BaseDN: base, AllowPrivate: true}
}

func TestSEC04_LDAPFilterParseAndEscape(t *testing.T) {
	for _, ok := range []string{"(cn=x)", "(&(objectClass=user)(sAMAccountName=jo\\2a))", "(|(a=1)(b=*)(c=ab*cd*ef))", "(!(userAccountControl:1.2.840.113556.1.4.803:=2))", "(memberOf:" + ldapmock.InChain + ":=cn=g,dc=x)", "(age>=3)", ldap.DefaultUserFilter} {
		if _, err := ldap.ParseFilter(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "cn=x", "(cn=x", "(&)", "(cn=a(b)", "(=x)", "(cn=\\4)", "(cn=x))", "(!(a=1)(b=2))"} {
		if _, err := ldap.ParseFilter(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	// what a person types is a value, never filter syntax
	if got := ldap.EscapeFilter(`*)(cn=*`); got != `\2a\29\28cn=\2a` {
		t.Fatal(got)
	}
	if err := ldap.CheckUserFilter("(sAMAccountName=jo)"); err == nil {
		t.Fatal("a filter without {user} accepted")
	}
	for _, u := range []string{"http://dc", "ldap://", "ldaps://dc/ou=x", "ldap://u:p@dc"} {
		if _, err := ldap.CheckURL(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

func TestSEC04_LDAPAuthenticate(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ldapmock.Options
	}{{"starttls", ldapmock.Options{}}, {"ldaps", ldapmock.Options{TLS: true}}} {
		t.Run(tc.name, func(t *testing.T) {
			m := ldapmock.New(t, tc.opts, people()...)
			cfg := cfgFor(t, m)
			for _, login := range []string{"jana.k", "JANA.K", "jana.k@corp.example", "Jana.K@Example.com", `CORP\jana.k`} {
				e, err := ldap.Authenticate(ctx, cfg, login, "jana-pw")
				if err != nil || e.Email != "Jana.K@Example.com" || e.Name != "Jana Kova" || !strings.HasPrefix(e.Subject, "guid:01020304") || e.DN != "cn=Jana Kova,ou=people,dc=corp,dc=example" {
					t.Fatalf("%s: %+v %v", login, e, err)
				}
			}
			// the service account searches, then the person's own DN is bound with their password; nothing went in clear
			b := m.Binds()
			if len(b) < 2 || b[0] != svcDN || b[1] != "cn=Jana Kova,ou=people,dc=corp,dc=example" || len(m.PlainBinds()) != 0 {
				t.Fatalf("binds %v plain %v", b, m.PlainBinds())
			}
			e, err := ldap.Authenticate(ctx, cfg, "ravi.s", "ravi-pw")
			if err != nil || e.Name != "Ravi S" || e.Subject != "dn:cn=ravi s,ou=people,dc=corp,dc=example" {
				t.Fatalf("%+v %v", e, err)
			}
			for _, c := range []struct{ login, pw string }{
				{"jana.k", "wrong"}, {"nobody", "jana-pw"}, {"twin", "a"}, // wrong password, unknown, ambiguous
				{"jana.k", ""}, {"", "x"}, // an empty password is an anonymous bind on many servers; never sent
				{"*", "jana-pw"}, {"jana*", "jana-pw"}, {"*)(sAMAccountName=jana.k", "jana-pw"}, // what is typed is not filter syntax
			} {
				if _, err := ldap.Authenticate(ctx, cfg, c.login, c.pw); !errors.Is(err, ldap.ErrInvalidCredentials) {
					t.Errorf("%q/%q: %v", c.login, c.pw, err)
				}
			}
		})
	}
}

func TestSEC04_LDAPFilterRestrictsWhoMaySignIn(t *testing.T) {
	m := ldapmock.New(t, ldapmock.Options{}, people()...)
	cfg := cfgFor(t, m)
	// only members of the group, through nesting, and not disabled accounts: written in the filter as in Active Directory
	cfg.UserFilter = "(&(sAMAccountName={user})(memberOf:" + ldapmock.InChain + ":=" + adminsG + ")(!(userAccountControl:1.2.840.113556.1.4.803:=2)))"
	if _, err := ldap.Authenticate(ctx, cfg, "jana.k", "jana-pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := ldap.Authenticate(ctx, cfg, "ravi.s", "ravi-pw"); !errors.Is(err, ldap.ErrInvalidCredentials) {
		t.Fatal("outside the group", err)
	}
}

func TestSEC04_LDAPTransportAndFailures(t *testing.T) {
	m := ldapmock.New(t, ldapmock.Options{}, people()...)
	cfg := cfgFor(t, m)
	// a certificate nobody vouches for is refused, unless the administrator turned verification off
	c := cfg
	c.RootCAs = nil
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); !errors.Is(err, ldap.ErrUnavailable) {
		t.Fatal("unverified certificate accepted", err)
	}
	c.InsecureSkipVerify = true
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); err != nil {
		t.Fatal(err)
	}
	// a server that cannot do StartTLS is not sent a password
	n := ldapmock.New(t, ldapmock.Options{NoStartTLS: true}, people()...)
	if _, err := ldap.Authenticate(ctx, cfgFor(t, n), "jana.k", "jana-pw"); !errors.Is(err, ldap.ErrUnavailable) || len(n.Binds()) != 0 {
		t.Fatal(err, n.Binds())
	}
	// the egress policy: loopback and private addresses need the setting
	c = cfg
	c.AllowPrivate = false
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); !errors.Is(err, egress.ErrBlocked) {
		t.Fatal("private address reached", err)
	}
	// a wrong search account is the administrator's problem, not the person's password
	c = cfg
	c.BindPassword = "stale"
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); !errors.Is(err, ldap.ErrUnavailable) {
		t.Fatal(err)
	}
	// the first server is down: the next one answers
	dead := ldapmock.New(t, ldapmock.Options{}, people()...)
	dead.Down(true)
	c = cfg
	c.URLs = []string{"ldap://127.0.0.1:1", dead.URL(), m.URL()}
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); err != nil {
		t.Fatal(err)
	}
	c.URLs = []string{dead.URL()}
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); !errors.Is(err, ldap.ErrUnavailable) {
		t.Fatal(err)
	}
	// plain ldap:// without StartTLS only to this machine
	c = cfg
	c.StartTLS = false
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); err != nil || len(m.PlainBinds()) == 0 {
		t.Fatal("loopback plain", err)
	}
	c.URLs, c.Timeout = []string{"ldap://10.255.255.1"}, 200*time.Millisecond // refused before any connection is tried
	if _, err := ldap.Authenticate(ctx, c, "jana.k", "jana-pw"); err == nil || !strings.Contains(err.Error(), "not sent unencrypted") {
		t.Fatal("plain to another host", err)
	}
}
