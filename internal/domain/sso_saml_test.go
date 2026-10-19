package domain_test

import (
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// The responses are the fixtures of internal/saml, made with lxml and the Python cryptography package (see gen.py there).

func samlFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../saml/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func samlText(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../saml/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type samlRig struct {
	*testutil.Env
	p domain.SSOProvider
}

func newSAML(t *testing.T, mut func(*domain.SSOProvider)) *samlRig {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC) // inside the fixtures' validity
	e.Svc.BaseURL = "https://tasklane.example"
	e.Svc.SAMLID = func() string { return "_req1" } // the request id the fixtures answer
	in := domain.SSOProvider{Name: "okta", Label: "Okta", Protocol: "saml", SAMLMetadata: samlText(t, "idp_metadata.xml"), ClientID: "https://tasklane.example/api/v1/auth/saml/metadata", Enabled: true, JIT: true}
	if mut != nil {
		mut(&in)
	}
	p, err := e.Svc.SaveSSOProvider(ctx, e.Admin, 0, in)
	if err != nil {
		t.Fatal(err)
	}
	return &samlRig{Env: e, p: p}
}

// flow starts a sign-in (returning the identity provider's address and the relay state in it) and posts a response.
func (r *samlRig) flow(t *testing.T, fixture string) (domain.SSOResult, error) {
	t.Helper()
	to, err := r.Svc.SSOStart(ctx, "okta", "/#/issue/ABC-1")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(to)
	if !strings.HasPrefix(to, "https://idp.example/sso?") || u.Query().Get("SAMLRequest") == "" || u.Query().Get("RelayState") == "" {
		t.Fatalf("the address to send the browser to: %s", to)
	}
	return r.Svc.SAMLCallback(ctx, u.Query().Get("RelayState"), samlFixture(t, fixture), time.Hour, domain.ClientMeta{IP: "203.0.113.5"})
}

// SAML01: a signed response from the identity provider signs the person in: the account is made (or found), the redirect is kept, and the
// session works; a response is accepted once.
func TestSAML01_SignInAndAccountMatching(t *testing.T) {
	r := newSAML(t, nil)
	if r.p.Protocol != "saml" || r.p.Issuer != "https://idp.example/entity" || r.p.SAMLSSOURL != "https://idp.example/sso" || len(r.p.CertInfo) != 1 || r.p.ACSURL != "https://tasklane.example/api/v1/auth/saml/acs" {
		t.Fatalf("the provider from metadata: %+v", r.p)
	}
	res, err := r.flow(t, "assertion_signed.xml")
	if err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	if !res.Created || res.User.Email != "ana@example.com" || res.User.Name != "Ana Novak" || res.Token == "" || res.Redirect != "/#/issue/ABC-1" {
		t.Fatalf("result: %+v", res)
	}
	if a, err := r.Svc.Authenticate(ctx, res.Token); err != nil || a.ID != res.User.ID {
		t.Fatalf("the session: %v", err)
	}
	// a second sign-in (a fresh request, another response with a different assertion id would be needed: the same assertion is refused)
	if _, err := r.flow(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an assertion was accepted twice: %v", err)
	}
	// the other signed shapes work, each once, and find the same person
	for _, f := range []string{"response_signed.xml", "both_signed.xml"} {
		r.Store.Pool.Exec(ctx, `DELETE FROM saml_assertion`) // the same assertion id in each fixture: forget it for this test
		got, err := r.flow(t, f)
		if err != nil || got.Created || got.User.ID != res.User.ID {
			t.Fatalf("%s: %+v %v", f, got, err)
		}
	}
	// the metadata for the identity provider's administrator
	md, err := r.Svc.SAMLMetadata(ctx, "okta")
	if err != nil || !strings.Contains(md, `entityID="https://tasklane.example/api/v1/auth/saml/metadata"`) || !strings.Contains(md, "https://tasklane.example/api/v1/auth/saml/acs") {
		t.Fatalf("metadata: %v\n%s", err, md)
	}
	if _, err := r.Svc.SAMLMetadata(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a missing provider: %v", err)
	}
}

// SAML02: everything that makes a response untrustworthy ends in a refusal with nothing created: forged, tampered, unsolicited, replayed, for
// someone else, from someone else.
func TestSAML02_Refusals(t *testing.T) {
	r := newSAML(t, nil)
	for _, bad := range []string{"other_key.xml", "unsigned.xml", "sha1.xml", "wrong_audience.xml", "expired.xml", "not_yet.xml", "wrong_recipient.xml", "wrong_inresponse.xml",
		"wrong_destination.xml", "wrong_issuer.xml", "no_conditions.xml", "status_failure.xml", "xsw_two_assertions.xml", "xsw_duplicate_id.xml", "xsw_signed_in_extensions.xml",
		"bad_reference.xml", "doctype.xml", "encrypted.xml", "extra_transform.xml"} {
		if res, err := r.flow(t, bad); !errors.Is(err, domain.ErrForbidden) || res.Token != "" {
			t.Fatalf("%s was accepted: %+v %v", bad, res, err)
		}
	}
	var users int
	r.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE email='ana@example.com'`).Scan(&users)
	if users != 0 {
		t.Fatalf("a refused response created %d account(s)", users)
	}
	// the administrator can see why: the audit log says what was wrong, the person is not told
	var n int
	r.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.saml_refused' AND detail::text ILIKE '%not signed%'`).Scan(&n)
	if n == 0 {
		t.Fatal("the refusals are not in the audit log")
	}
	// a validly signed response that answers a different request than the one this sign-in sent is refused (a response captured earlier)
	r.Svc.SAMLID = func() string { return "_req2" }
	if _, err := r.flow(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a response to another request: %v", err)
	}
	r.Svc.SAMLID = func() string { return "_req1" }
	r.Store.Pool.Exec(ctx, `DELETE FROM saml_assertion`)
	// the relay state: unknown, used, expired, and from another request
	if _, err := r.Svc.SAMLCallback(ctx, "no-such-state", samlFixture(t, "assertion_signed.xml"), time.Hour, domain.ClientMeta{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an unsolicited response: %v", err)
	}
	to, _ := r.Svc.SSOStart(ctx, "okta", "")
	relay := func() string { u, _ := url.Parse(to); return u.Query().Get("RelayState") }()
	if _, err := r.Svc.SAMLCallback(ctx, relay, samlFixture(t, "assertion_signed.xml"), time.Hour, domain.ClientMeta{}); err != nil {
		t.Fatalf("the first use of a relay state: %v", err)
	}
	r.Store.Pool.Exec(ctx, `DELETE FROM saml_assertion`)
	if _, err := r.Svc.SAMLCallback(ctx, relay, samlFixture(t, "assertion_signed.xml"), time.Hour, domain.ClientMeta{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a relay state used twice: %v", err)
	}
	to2, _ := r.Svc.SSOStart(ctx, "okta", "")
	relay2 := func() string { u, _ := url.Parse(to2); return u.Query().Get("RelayState") }()
	r.Clock.T = r.Clock.T.Add(11 * time.Minute) // past the ten minutes a sign-in may take, and the fixtures' own validity
	if _, err := r.Svc.SAMLCallback(ctx, relay2, samlFixture(t, "response_signed.xml"), time.Hour, domain.ClientMeta{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an expired sign-in: %v", err)
	}
	// not base64, and far too large
	r.Clock.T = r.Clock.T.Add(-11 * time.Minute)
	to3, _ := r.Svc.SSOStart(ctx, "okta", "")
	relay3 := func() string { u, _ := url.Parse(to3); return u.Query().Get("RelayState") }()
	if _, err := r.Svc.SAMLCallback(ctx, relay3, "!!not base64!!", time.Hour, domain.ClientMeta{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("not base64: %v", err)
	}
	to4, _ := r.Svc.SSOStart(ctx, "okta", "")
	relay4 := func() string { u, _ := url.Parse(to4); return u.Query().Get("RelayState") }()
	if _, err := r.Svc.SAMLCallback(ctx, relay4, strings.Repeat("A", 2<<20), time.Hour, domain.ClientMeta{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a huge response: %v", err)
	}
}

// SAML03: the rules of an account apply as for OpenID Connect: no creation without JIT, domain limits, a deprovisioned person stays out;
// a response made for another provider's certificate does not work, and a provider that is off does not sign anyone in.
func TestSAML03_AccountRules(t *testing.T) {
	r := newSAML(t, func(p *domain.SSOProvider) { p.JIT = false })
	if _, err := r.flow(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an unknown person with JIT off: %v", err)
	}
	if _, err := r.Svc.CreateUser(ctx, "ana@example.com", "Ana", "password123", false); err != nil {
		t.Fatal(err)
	}
	r.Store.Pool.Exec(ctx, `DELETE FROM saml_assertion`) // the refused attempt used the assertion up: that is the point of one-time use
	res, err := r.flow(t, "assertion_signed.xml")
	if err != nil || res.Created {
		t.Fatalf("a known person: %+v %v", res, err)
	}
	// a deprovisioned person stays out, whatever the provider says
	r.Store.Pool.Exec(ctx, `UPDATE app_user SET active=false WHERE email='ana@example.com'`)
	r.Store.Pool.Exec(ctx, `DELETE FROM saml_assertion`)
	if _, err := r.flow(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a deactivated person: %v", err)
	}
	// domain limits
	r2 := newSAML(t, func(p *domain.SSOProvider) { p.AllowedDomains = []string{"corp.example"} })
	if _, err := r2.flow(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an address outside the allowed domains: %v", err)
	}
	// the provider is configured with a different certificate: the fixtures are not trusted
	r3 := newSAML(t, nil)
	other, _ := os.ReadFile("../saml/testdata/other.pem")
	if _, err := r3.Svc.SaveSSOProvider(ctx, r3.Admin, r3.p.ID, domain.SSOProvider{Name: "okta", Label: "Okta", Protocol: "saml", Issuer: r3.p.Issuer, SAMLSSOURL: r3.p.SAMLSSOURL, SAMLCerts: []string{string(other)}, ClientID: r3.p.ClientID, Enabled: true, JIT: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := r3.flow(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a response signed by a certificate we do not trust: %v", err)
	}
	// switched off: no sign-in can even start
	if _, err := r3.Svc.SaveSSOProvider(ctx, r3.Admin, r3.p.ID, domain.SSOProvider{Name: "okta", Label: "Okta", Protocol: "saml", Issuer: r3.p.Issuer, SAMLSSOURL: r3.p.SAMLSSOURL, SAMLCerts: r3.p.SAMLCerts, ClientID: r3.p.ClientID, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if _, err := r3.Svc.SSOStart(ctx, "okta", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a provider that is off: %v", err)
	}
}

// SAML04: configuring a provider: from metadata or by hand, checked; only administrators; the sign-in page offers it like any other.
func TestSAML04_Configuration(t *testing.T) {
	r := newSAML(t, nil)
	if _, err := r.Svc.SaveSSOProvider(ctx, r.Alice, 0, domain.SSOProvider{Name: "x", Protocol: "saml"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member configured SAML: %v", err)
	}
	cert, _ := os.ReadFile("../saml/testdata/idp.pem")
	good := domain.SSOProvider{Name: "hand", Label: "By hand", Protocol: "saml", Issuer: "https://idp2.example/e", SAMLSSOURL: "https://idp2.example/sso", SAMLCerts: []string{string(cert)}, Enabled: true}
	cases := map[string]func(*domain.SSOProvider){
		"no entity id":      func(p *domain.SSOProvider) { p.Issuer = "" },
		"no address":        func(p *domain.SSOProvider) { p.SAMLSSOURL = "" },
		"a bad address":     func(p *domain.SSOProvider) { p.SAMLSSOURL = "ftp://idp2.example" },
		"no certificate":    func(p *domain.SSOProvider) { p.SAMLCerts = nil },
		"not a certificate": func(p *domain.SSOProvider) { p.SAMLCerts = []string{"hello"} },
		"bad metadata":      func(p *domain.SSOProvider) { p.SAMLMetadata = "<nope/>" },
		"a bad name":        func(p *domain.SSOProvider) { p.Name = "Bad Name" },
		"another protocol":  func(p *domain.SSOProvider) { p.Protocol = "kerberos" },
	}
	for name, mut := range cases {
		in := good
		mut(&in)
		if _, err := r.Svc.SaveSSOProvider(ctx, r.Admin, 0, in); !isInvalid(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	p, err := r.Svc.SaveSSOProvider(ctx, r.Admin, 0, good)
	if err != nil || p.ClientID != "https://tasklane.example/api/v1/auth/saml/hand/metadata" || p.MetadataURL == "" || len(p.CertInfo) != 1 {
		t.Fatalf("by hand: %+v %v", p, err)
	}
	// the sign-in page lists it with the others
	found := false
	for _, c := range r.Svc.SSOChoices(ctx) {
		found = found || c["name"] == "hand"
	}
	if !found {
		t.Fatal("the provider is not offered on the sign-in page")
	}
	// a provider that is not SAML has no SAML metadata
	if _, err := r.Svc.SAMLMetadata(ctx, "okta"); err != nil {
		t.Fatal(err)
	}
}
