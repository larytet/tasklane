package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/ldapmock"
)

const (
	ldBase  = "dc=corp,dc=example"
	ldSvc   = "cn=svc-tasklane,ou=service,dc=corp,dc=example"
	ldJana  = "cn=Jana Kova,ou=people,dc=corp,dc=example"
	ldAlice = "cn=Alice A,ou=people,dc=corp,dc=example"
)

type ldapRig struct {
	*testutil.Env
	dir *ldapmock.Mock
	p   domain.SSOProvider
}

func newLDAP(t *testing.T, mut func(*domain.SSOProvider)) *ldapRig {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	e.Svc.SSOEgress = func(p egress.Policy) egress.Policy { p.AllowPrivate = true; return p }
	dir := ldapmock.New(t, ldapmock.Options{},
		ldapmock.Entry{DN: ldSvc, Password: "svc-secret"},
		ldapmock.Entry{DN: ldJana, Password: "jana-ad-pw", Attrs: map[string][]string{"sAMAccountName": {"jana.k"}, "mail": {"Jana.K@Example.com"}, "displayName": {"Jana Kova"}, "objectGUID": {"guid-jana-0001"}}},
		// Alice has a Tasklane account with a local password already; the directory knows her by the same address
		ldapmock.Entry{DN: ldAlice, Password: "alice-ad-pw", Attrs: map[string][]string{"sAMAccountName": {"alice"}, "mail": {"alice@example.com"}, "displayName": {"Alice"}, "objectGUID": {"guid-alice-001"}}},
		ldapmock.Entry{DN: "cn=No Mail,ou=people,dc=corp,dc=example", Password: "nomail-pw", Attrs: map[string][]string{"sAMAccountName": {"nomail"}}},
	)
	in := domain.SSOProvider{Protocol: "ldap", Name: "corp-ad", Label: "Corp directory", Issuer: dir.URL(), LDAPStartTLS: true, LDAPCAPEM: dir.CAPEM(),
		LDAPBindDN: ldSvc, ClientSecret: "svc-secret", LDAPBaseDN: ldBase, JIT: true, Enabled: true}
	if mut != nil {
		mut(&in)
	}
	p, err := e.Svc.SaveSSOProvider(ctx, e.Admin, 0, in)
	if err != nil {
		t.Fatal(err)
	}
	return &ldapRig{Env: e, dir: dir, p: p}
}

var ldMeta = domain.ClientMeta{IP: "203.0.113.7"}

func TestSEC04_LDAPProviderIsCheckedWhenSaved(t *testing.T) {
	r := newLDAP(t, nil)
	e := r.Env
	if r.p.Protocol != "ldap" || r.p.ClientSecret != "" || !r.p.SecretSet || r.p.LDAPUserFilter == "" || r.p.Issuer != r.dir.URL() {
		t.Fatalf("%+v", r.p)
	}
	var stored []byte
	e.Svc.DB.QueryRow(ctx, `SELECT client_secret_enc FROM sso_provider WHERE id=$1`, r.p.ID).Scan(&stored)
	if len(stored) == 0 || strings.Contains(string(stored), "svc-secret") {
		t.Fatal("the search password is not sealed")
	}
	good := func() domain.SSOProvider {
		return domain.SSOProvider{Protocol: "ldap", Name: "other", Issuer: "ldaps://dc1.example.com", LDAPBaseDN: ldBase, LDAPBindDN: ldSvc, ClientSecret: "x"}
	}
	for name, mut := range map[string]func(*domain.SSOProvider){
		"plain to another host":       func(p *domain.SSOProvider) { p.Issuer = "ldap://dc1.example.com" },
		"not an ldap address":         func(p *domain.SSOProvider) { p.Issuer = "https://dc1.example.com" },
		"no address":                  func(p *domain.SSOProvider) { p.Issuer = " " },
		"no base":                     func(p *domain.SSOProvider) { p.LDAPBaseDN = "" },
		"search account, no password": func(p *domain.SSOProvider) { p.ClientSecret = "" },
		"filter without {user}":       func(p *domain.SSOProvider) { p.LDAPUserFilter = "(sAMAccountName=jana)" },
		"broken filter":               func(p *domain.SSOProvider) { p.LDAPUserFilter = "(sAMAccountName={user}" },
		"not a certificate":           func(p *domain.SSOProvider) { p.LDAPCAPEM = "hello" },
	} {
		p := good()
		mut(&p)
		if _, err := e.Svc.SaveSSOProvider(ctx, e.Admin, 0, p); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	p := good()
	p.Issuer = "ldap://dc1.example.com, ldaps://dc2.example.com:3269"
	p.LDAPStartTLS = true
	if v, err := e.Svc.SaveSSOProvider(ctx, e.Admin, 0, p); err != nil || v.Issuer != "ldap://dc1.example.com ldaps://dc2.example.com:3269" {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := e.Svc.SaveSSOProvider(ctx, e.Alice, 0, good()); err == nil {
		t.Fatal("a member saved a provider")
	}
	if _, err := e.Svc.SavePortalSSO(ctx, e.Admin, "ABC", good()); err == nil {
		t.Fatal("a portal took an LDAP provider")
	}
	// a directory is not a button on the sign-in page and cannot be started as a redirect
	for _, c := range e.Svc.SSOChoices(ctx) {
		if c["name"] == "corp-ad" {
			t.Fatal("LDAP offered as a button")
		}
	}
	if _, err := e.Svc.SSOStart(ctx, "corp-ad", "/"); err == nil {
		t.Fatal("LDAP started as a redirect")
	}
}

func TestSEC04_LDAPSignInCreatesMatchesAndRecognises(t *testing.T) {
	r := newLDAP(t, nil)
	e := r.Env
	// the directory name, the address and DOMAIN\name all work; the first creates the account
	res, err := e.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta)
	if err != nil || res.Token == "" || res.User.Email != "jana.k@example.com" || res.User.Name != "Jana Kova" || res.User.Admin {
		t.Fatalf("%+v %v", res, err)
	}
	var source, via string
	e.Svc.DB.QueryRow(ctx, `SELECT source FROM app_user WHERE id::text=$1`, res.User.ID).Scan(&source)
	e.Svc.DB.QueryRow(ctx, `SELECT via FROM session WHERE user_id::text=$1`, res.User.ID).Scan(&via)
	if source != "ldap" || via != "ldap" {
		t.Fatal(source, via)
	}
	for _, login := range []string{"Jana.K@example.com", `CORP\jana.k`} {
		res2, err := e.Svc.LoginWith(ctx, login, "jana-ad-pw", time.Hour, ldMeta)
		if err != nil || res2.User.ID != res.User.ID {
			t.Fatalf("%s: %+v %v", login, res2, err)
		}
	}
	// the account is found by the directory's identifier, so a changed address in the directory still leads to it
	r.dir.Update(ldapmock.Entry{DN: ldJana, Password: "jana-ad-pw", Attrs: map[string][]string{"sAMAccountName": {"jana.k"}, "mail": {"jana.kova@example.com"}, "objectGUID": {"guid-jana-0001"}}})
	if res2, err := e.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta); err != nil || res2.User.ID != res.User.ID {
		t.Fatalf("%+v %v", res2, err)
	}
	// a local account and its local password keep working; the directory password reaches the same account (matched by address)
	if res2, err := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, ldMeta); err != nil || res2.User.ID != e.Alice.ID {
		t.Fatalf("local: %+v %v", res2, err)
	}
	if res2, err := e.Svc.LoginWith(ctx, "alice", "alice-ad-pw", time.Hour, ldMeta); err != nil || res2.User.ID != e.Alice.ID {
		t.Fatalf("directory: %+v %v", res2, err)
	}
	// wrong password, unknown name, an entry without an address
	for _, c := range [][2]string{{"jana.k", "wrong"}, {"nobody", "jana-ad-pw"}, {"nomail", "nomail-pw"}, {"jana.k", ""}} {
		if _, err := e.Svc.LoginWith(ctx, c[0], c[1], time.Hour, ldMeta); err != domain.ErrForbidden {
			t.Errorf("%v: %v", c, err)
		}
	}
	// nothing is cached: removed from the directory is refused at once
	r.dir.Remove(ldJana)
	if _, err := e.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta); err != domain.ErrForbidden {
		t.Fatal("removed person signed in", err)
	}
}

func TestSEC04_LDAPSignInFollowsTheAccountRules(t *testing.T) {
	// no accounts at first sign-in: only people who already have one
	r := newLDAP(t, func(p *domain.SSOProvider) { p.JIT = false })
	e := r.Env
	if _, err := e.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta); err != domain.ErrForbidden {
		t.Fatal("account created without JIT", err)
	}
	if _, err := e.Svc.LoginWith(ctx, "alice", "alice-ad-pw", time.Hour, ldMeta); err != nil {
		t.Fatal(err)
	}
	// a deactivated account stays out whatever the directory says
	e.Svc.DB.Exec(ctx, `UPDATE app_user SET active=false WHERE id::text=$1`, e.Alice.ID)
	if _, err := e.Svc.LoginWith(ctx, "alice", "alice-ad-pw", time.Hour, ldMeta); err != domain.ErrForbidden {
		t.Fatal("deactivated account signed in", err)
	}
	// allowed domains
	r = newLDAP(t, func(p *domain.SSOProvider) { p.AllowedDomains = []string{"corp.example"} })
	if _, err := r.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta); err != domain.ErrForbidden {
		t.Fatal("address outside the allowed domains", err)
	}
	// a disabled provider is not asked
	r = newLDAP(t, func(p *domain.SSOProvider) { p.Enabled = false })
	if _, err := r.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta); err != domain.ErrForbidden || len(r.dir.Binds()) != 0 {
		t.Fatal("disabled provider used", err, r.dir.Binds())
	}
}

func TestSEC04_LDAPSignInKeepsSecondFactorLockoutAndOutages(t *testing.T) {
	r := newLDAP(t, nil)
	e := r.Env
	e.Clock.T = time.Date(2026, 10, 19, 12, 0, 10, 0, time.UTC)
	// a second factor is asked for after a directory password exactly as after a local one
	first, _ := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, ldMeta)
	setup, _ := e.Svc.SetupMFA(ctx, e.Alice)
	if _, err := e.Svc.EnableMFA(ctx, e.Alice, totp(t, setup.Secret, e.Clock.T), first.Token); err != nil {
		t.Fatal(err)
	}
	_, err := e.Svc.LoginWith(ctx, "alice", "alice-ad-pw", time.Hour, ldMeta)
	var need *domain.MFARequired
	if !asMFA(err, &need) {
		t.Fatal("directory password skipped the second factor", err)
	}
	// the directory is down: directory people cannot sign in, local passwords still work
	r.dir.Down(true)
	if _, err := e.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta); err != domain.ErrForbidden {
		t.Fatal(err)
	}
	if _, err := e.Svc.LoginWith(ctx, "bob@example.com", "password123", time.Hour, ldMeta); err != nil {
		t.Fatal("local sign-in broken by a directory outage", err)
	}
	r.dir.Down(false)
	// repeated failures lock the name typed, which also spares the directory account from someone guessing through Tasklane
	for range 5 {
		e.Svc.LoginWith(ctx, "jana.k", "guess", time.Hour, ldMeta)
	}
	binds := len(r.dir.Binds())
	if _, err := e.Svc.LoginWith(ctx, "jana.k", "jana-ad-pw", time.Hour, ldMeta); err == nil {
		t.Fatal("not locked")
	} else if _, ok := err.(*domain.LockedError); !ok || len(r.dir.Binds()) != binds {
		t.Fatal("a locked name still reached the directory", err)
	}
}

func TestSEC04_LDAPCheckForAdministrators(t *testing.T) {
	r := newLDAP(t, nil)
	e := r.Env
	v, err := e.Svc.LDAPCheck(ctx, e.Admin, r.p.ID, "jana.k", "jana-ad-pw")
	if err != nil || v["ok"] != true || v["dn"] != ldJana || v["account"] != "created at first sign-in" {
		t.Fatalf("%v %v", v, err)
	}
	if v, _ := e.Svc.LDAPCheck(ctx, e.Admin, r.p.ID, "alice", "alice-ad-pw"); v["account"] != "existing" {
		t.Fatal(v)
	}
	if v, _ := e.Svc.LDAPCheck(ctx, e.Admin, r.p.ID, "jana.k", "wrong"); v["ok"] != false || !strings.Contains(v["error"].(string), "invalid credentials") {
		t.Fatal(v)
	}
	if v, _ := e.Svc.LDAPCheck(ctx, e.Admin, r.p.ID, "nomail", "nomail-pw"); v["ok"] != false || !strings.Contains(v["error"].(string), "no email") {
		t.Fatal(v)
	}
	// it signs nobody in and creates nothing
	var n int
	e.Svc.DB.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE email='jana.k@example.com'`).Scan(&n)
	if n != 0 {
		t.Fatal("the check created an account")
	}
	if _, err := e.Svc.LDAPCheck(ctx, e.Alice, r.p.ID, "jana.k", "jana-ad-pw"); err == nil {
		t.Fatal("a member ran the check")
	}
}
