package domain_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/oidcmock"
)

func newPortalSAML(t *testing.T, domains ...string) (*portal, domain.SSOProvider) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	e.Svc.BaseURL = "https://tasklane.example"
	e.Svc.SAMLID = func() string { return "_req1" }
	p := &portal{e}
	p.policy(t, domain.PortalPolicy{Enabled: true})
	in := domain.SSOProvider{Label: "Acme login", Protocol: "saml", SAMLMetadata: samlText(t, "idp_metadata.xml"), ClientID: "https://tasklane.example/api/v1/auth/saml/metadata", Enabled: true, AllowedDomains: domains}
	pr, err := e.Svc.SavePortalSSO(ctx, e.Admin, "ABC", in)
	if err != nil {
		t.Fatal(err)
	}
	return p, pr
}

func (p *portal) samlSignIn(t *testing.T, fixture string) (domain.SSOResult, error) {
	t.Helper()
	to, err := p.Svc.SSOStart(ctx, "portal-abc", "/portal/ABC")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(to)
	return p.Svc.SAMLCallback(ctx, u.Query().Get("RelayState"), samlFixture(t, fixture), time.Hour, domain.ClientMeta{IP: "203.0.113.5"})
}

// PORT-01 (identity provider): the portal's own identity provider: kept by a project administrator, named domains required, absent from the staff sign-in, and a
// sign-in through it ends in a customer session and nothing else.
func TestPORT01_SSO_PortalProviderAdministration(t *testing.T) {
	p, pr := newPortalSAML(t, "example.com")
	if pr.Name != "portal-abc" || pr.PortalKey != "ABC" || pr.JIT {
		t.Fatalf("the provider: %+v", pr)
	}
	in := domain.SSOProvider{Label: "x", Protocol: "oidc", Issuer: "https://login.example.com", ClientID: "c", Enabled: true}
	if _, err := p.Svc.SavePortalSSO(ctx, p.Alice, "ABC", in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member saved: %v", err)
	}
	out := p.NewUser("out6@example.com", "Outsider", false)
	if _, err := p.Svc.SavePortalSSO(ctx, out, "ABC", in); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider saved: %v", err)
	}
	if _, err := p.Svc.SavePortalSSO(ctx, p.Admin, "ABC", in); !isInvalid(err) || !strings.Contains(err.Error(), "domains") {
		t.Fatalf("no domains: %v", err)
	}
	in.AllowedDomains = []string{"  ", "@"}
	if _, err := p.Svc.SavePortalSSO(ctx, p.Admin, "ABC", in); !isInvalid(err) {
		t.Fatalf("blank domains: %v", err)
	}
	// a project administrator who is not the instance administrator may keep it
	if err := p.Svc.AddMember(ctx, p.Admin, "ABC", p.Bob.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, err := p.Svc.GetPortalSSO(ctx, p.Bob, "ABC"); err != nil || got == nil || got.ID != pr.ID {
		t.Fatalf("a project administrator reads it: %+v %v", got, err)
	}
	// saving again replaces, it does not add
	again, err := p.Svc.SavePortalSSO(ctx, p.Admin, "ABC", domain.SSOProvider{Label: "Renamed", Protocol: "saml", SAMLMetadata: samlText(t, "idp_metadata.xml"), ClientID: pr.ClientID, Enabled: true, AllowedDomains: []string{"example.com", "other.org"}})
	if err != nil || again.ID != pr.ID || again.Label != "Renamed" {
		t.Fatalf("replace: %+v %v", again, err)
	}
	// it is not the staff sign-in's
	if l, _ := p.Svc.ListSSOProviders(ctx, p.Admin); len(l) != 0 {
		t.Fatalf("staff list shows the portal's provider: %+v", l)
	}
	if c := p.Svc.SSOChoices(ctx); len(c) != 0 {
		t.Fatalf("a staff sign-in button for the portal's provider: %v", c)
	}
	if err := p.Svc.DeleteSSOProvider(ctx, p.Admin, pr.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the staff screen deleted the portal's provider: %v", err)
	}
	info, _ := p.Svc.PortalInfoFor(ctx, "ABC")
	if info.SSO == nil || info.SSO.Name != "portal-abc" || info.SSO.Label != "Renamed" {
		t.Fatalf("the portal does not offer it: %+v", info)
	}
	// off: not offered, and a sign-in is refused
	off := again
	off.Enabled = false
	if _, err := p.Svc.SavePortalSSO(ctx, p.Admin, "ABC", off); err != nil {
		t.Fatal(err)
	}
	if info, _ := p.Svc.PortalInfoFor(ctx, "ABC"); info.SSO != nil {
		t.Fatal("a disabled provider is offered")
	}
	if _, err := p.Svc.SSOStart(ctx, "portal-abc", "/portal/ABC"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a disabled provider started: %v", err)
	}
	if err := p.Svc.DeletePortalSSO(ctx, p.Admin, "ABC"); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Svc.GetPortalSSO(ctx, p.Admin, "ABC"); got != nil {
		t.Fatal("still there after delete")
	}
	if err := p.Svc.DeletePortalSSO(ctx, p.Admin, "ABC"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestPORT01_SSO_SAMLSignInEndsInACustomerSession(t *testing.T) {
	p, _ := newPortalSAML(t, "example.com")
	res, err := p.samlSignIn(t, "assertion_signed.xml")
	if err != nil || res.PortalToken == "" || res.Token != "" || res.Redirect != "/portal/ABC" {
		t.Fatalf("%+v %v", res, err)
	}
	c, _, err := p.Svc.CustomerByToken(ctx, res.PortalToken)
	if err != nil || c.Email != "ana@example.com" {
		t.Fatalf("the customer: %+v %v", c, err)
	}
	// no staff account was made, and the customer session opens nothing of the staff side
	var staff int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE lower(email)='ana@example.com'`).Scan(&staff)
	if staff != 0 {
		t.Fatalf("%d staff accounts made for a customer", staff)
	}
	if _, err := p.Svc.Authenticate(ctx, res.PortalToken); err == nil {
		t.Fatal("a customer session was accepted as a staff session")
	}
	var audited int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.portal_sso_login'`).Scan(&audited)
	if audited != 1 {
		t.Fatalf("%d audit rows", audited)
	}
	// the customer can do what a customer does
	rt := p.rtype(t, "General")
	if _, err := p.Svc.PortalCreateRequest(ctx, c, "ABC", domain.PortalRequestIn{Type: rt.ID, Summary: "Signed in through our own provider"}, meta); err != nil {
		t.Fatal(err)
	}
	// the response was used: it cannot be used again, and a refusal sends nobody to a session
	if _, err := p.Svc.SAMLCallback(ctx, "gone", samlFixture(t, "assertion_signed.xml"), time.Hour, meta); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a replay: %v", err)
	}
}

func TestPORT01_SSO_ProviderMayOnlyVouchForItsDomainsAndThePortalRulesStillApply(t *testing.T) {
	// the provider speaks for other.org only: ana@example.com is refused, however well signed
	p, _ := newPortalSAML(t, "other.org")
	res, err := p.samlSignIn(t, "assertion_signed.xml")
	if !errors.Is(err, domain.ErrForbidden) || res.PortalToken != "" {
		t.Fatalf("a domain the provider does not speak for: %+v %v", res, err)
	}
	if res.Redirect != "/portal/ABC" {
		t.Fatalf("a refusal loses the portal it came from: %q", res.Redirect)
	}
	var refused int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.portal_sso_refused'`).Scan(&refused)
	if refused != 1 {
		t.Fatalf("%d refusals audited", refused)
	}
	var made int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM customer WHERE lower(email)='ana@example.com'`).Scan(&made)
	if made != 0 {
		t.Fatal("a customer was made for a refused sign-in")
	}
	// the portal's own block list holds
	p2, _ := newPortalSAML(t, "example.com")
	p2.policy(t, domain.PortalPolicy{Enabled: true, Block: []string{"ana@example.com"}})
	if _, err := p2.samlSignIn(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a blocked address signed in: %v", err)
	}
	// and so does a portal that is switched off
	p3, _ := newPortalSAML(t, "example.com")
	p3.policy(t, domain.PortalPolicy{Enabled: false})
	if _, err := p3.samlSignIn(t, "assertion_signed.xml"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a closed portal let someone in: %v", err)
	}
}

func TestPORT01_SSO_OpenIDConnectSignIn(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	idp := oidcmock.New(t)
	idp.Now = e.Svc.Now
	e.Svc.BaseURL = "https://tasklane.example.com"
	e.Svc.SSOEgress = func(p egress.Policy) egress.Policy { p.AllowPrivate = true; return p }
	p := &portal{e}
	p.policy(t, domain.PortalPolicy{Enabled: true})
	if _, err := e.Svc.SavePortalSSO(ctx, e.Admin, "ABC", domain.SSOProvider{Label: "Acme", Issuer: idp.URL, ClientID: "tasklane", Enabled: true, AllowedDomains: []string{"example.com"}}); err != nil {
		t.Fatal(err)
	}
	idp.Redirect = "https://tasklane.example.com/api/v1/auth/sso/callback"
	to, err := e.Svc.SSOStart(ctx, "portal-abc", "/portal/ABC")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(idp.Authorize(to))
	res, err := e.Svc.SSOCallback(ctx, u.Query().Get("state"), u.Query().Get("code"), time.Hour, meta)
	if err != nil || res.PortalToken == "" || res.Token != "" || res.Redirect != "/portal/ABC" {
		t.Fatalf("%+v %v", res, err)
	}
	c, _, err := e.Svc.CustomerByToken(ctx, res.PortalToken)
	if err != nil || c.Email != "person@example.com" {
		t.Fatalf("%+v %v", c, err)
	}
	var staff int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE source='sso'`).Scan(&staff)
	if staff != 0 {
		t.Fatalf("%d staff accounts made", staff)
	}
	// a provider that vouches for an address outside its domains is refused
	idp.Email = "someone@elsewhere.example"
	to, _ = e.Svc.SSOStart(ctx, "portal-abc", "/portal/ABC")
	u, _ = url.Parse(idp.Authorize(to))
	if _, err := e.Svc.SSOCallback(ctx, u.Query().Get("state"), u.Query().Get("code"), time.Hour, meta); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an address outside the provider's domains: %v", err)
	}
}
