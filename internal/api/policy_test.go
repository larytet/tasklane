package api_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func cidr(t *testing.T, s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// call makes a request as a signed-in cookie session, optionally naming the visitor's address in X-Forwarded-For.
func call(t *testing.T, ts *httptest.Server, method, path string, body any, ck *http.Cookie, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "tasklane")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

type policyEnv struct {
	*testutil.Env
	a, b   domain.Actor // org administrators of two organizations
	aUser  domain.Actor // an ordinary person in organization A
	ts     *httptest.Server
	orgA   string
	orgB   string
	inside map[string]string // headers that put a visitor on the allowed network
}

func newPolicyEnv(t *testing.T) *policyEnv {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	oa, err := e.Svc.CreateOrganization(testutil.Ctx(), e.Admin, "Acme", "boss@acme.example", "Acme Boss", "password123")
	if err != nil {
		t.Fatal(err)
	}
	ob, err := e.Svc.CreateOrganization(testutil.Ctx(), e.Admin, "Globex", "boss@globex.example", "Globex Boss", "password123")
	if err != nil {
		t.Fatal(err)
	}
	pe := &policyEnv{Env: e, orgA: oa.ID, orgB: ob.ID}
	ul, _ := e.Svc.ListUsersFor(testutil.Ctx(), e.Admin)
	for _, u := range ul {
		switch u.Email {
		case "boss@acme.example":
			pe.a, _ = e.Svc.ActorFor(testutil.Ctx(), u.ID)
		case "boss@globex.example":
			pe.b, _ = e.Svc.ActorFor(testutil.Ctx(), u.ID)
		}
	}
	u, err := e.Svc.CreateUserAs(testutil.Ctx(), pe.a, "pat@acme.example", "Pat", "password123", false, "")
	if err != nil {
		t.Fatal(err)
	}
	pe.aUser, _ = e.Svc.ActorFor(testutil.Ctx(), u.ID)
	// the test server connects from 127.0.0.1, which is trusted as a proxy: the visitor's address is whatever X-Forwarded-For says
	e.Svc.TrustedProxies = []*net.IPNet{cidr(t, "127.0.0.0/8")}
	pe.ts = httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: 24 * time.Hour}).Handler())
	t.Cleanup(pe.ts.Close)
	pe.inside = map[string]string{"X-Forwarded-For": "203.0.113.9"}
	return pe
}

// enrol gives a person an authenticator app, the way the account page does.
func (p *policyEnv) enrol(t *testing.T, a domain.Actor) string {
	t.Helper()
	ctx := testutil.Ctx()
	setup, err := p.Svc.SetupMFA(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.EnableMFA(ctx, a, totpNow(t, setup.Secret, p.Clock.Now()), ""); err != nil {
		t.Fatal(err)
	}
	return setup.Secret
}

// session signs a person with an authenticator app in, both steps, and returns the cookie.
func (p *policyEnv) session(t *testing.T, email, secret string, ip string) *http.Cookie {
	t.Helper()
	ctx := testutil.Ctx()
	meta := domain.ClientMeta{IP: ip}
	_, err := p.Svc.LoginWith(ctx, email, "password123", time.Hour, meta)
	var need *domain.MFARequired
	if !errors.As(err, &need) {
		t.Fatalf("a second step was expected: %v", err)
	}
	p.Clock.Advance(31 * time.Second) // a fresh code: the last one is spent
	res, err := p.Svc.LoginMFA(ctx, need.Token, totpNow(t, secret, p.Clock.Now()), meta)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "tl_session", Value: res.Token}
}

// signInAs signs a person in by password (they have no second factor in these tests) and returns the cookie and status.
func (p *policyEnv) signInAs(t *testing.T, email string) (*http.Cookie, int) {
	return p.signIn(t, email, nil)
}

// session2 signs in an administrator that has an authenticator app (the policy tests enrol them first).
func (p *policyEnv) session2(t *testing.T, email string) *http.Cookie {
	t.Helper()
	ctx := testutil.Ctx()
	ul, _ := p.Svc.ListUsersFor(ctx, p.Admin)
	var a domain.Actor
	for _, u := range ul {
		if u.Email == email {
			a, _ = p.Svc.ActorFor(ctx, u.ID)
		}
	}
	secret := p.enrol(t, a)
	return p.session(t, email, secret, "")
}

func (p *policyEnv) signIn(t *testing.T, email string, hdr map[string]string) (*http.Cookie, int) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email, "password": "password123"})
	req, _ := http.NewRequest("POST", p.ts.URL+"/api/v1/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "tasklane")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return sessionCookie(resp), resp.StatusCode
}

func TestSEC04_ThePolicyIsSetOnlyByTheOrganizationsAdministratorsAndGuardsAgainstLockOut(t *testing.T) {
	p := newPolicyEnv(t)
	ctx := testutil.Ctx()
	pol := domain.SecurityPolicy{IPAllowlist: []string{"203.0.113.0/24", "198.51.100.7"}, SessionMaxHours: 8, RequireSecondFactor: true}
	if err := p.Svc.SetSecurityPolicy(ctx, p.aUser, "", pol, "203.0.113.9"); err == nil {
		t.Fatal("a member changed the policy")
	}
	if _, err := p.Svc.GetSecurityPolicy(ctx, p.aUser, ""); err == nil {
		t.Fatal("a member read the policy")
	}
	// another organization's administrator can neither read nor change it
	if err := p.Svc.SetSecurityPolicy(ctx, p.b, p.orgA, pol, "203.0.113.9"); err == nil {
		t.Fatal("an administrator of another organization changed this one's policy")
	}
	if _, err := p.Svc.GetSecurityPolicy(ctx, p.b, p.orgA); err == nil {
		t.Fatal("an administrator of another organization read this one's policy")
	}
	// validation
	for _, bad := range []domain.SecurityPolicy{{IPAllowlist: []string{"not a network"}}, {IPAllowlist: []string{"300.1.1.1/8"}}, {SessionMaxHours: -1}, {SessionMaxHours: 100000}} {
		if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", bad, "203.0.113.9"); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	// the same for a second factor: an administrator who has none may not require one, or they would hold themselves at the
	// enrolment page; once they have one they may
	if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{RequireSecondFactor: true}, "203.0.113.9"); err == nil || !strings.Contains(err.Error(), "your own second factor") {
		t.Fatalf("%v", err)
	}
	secretA := p.enrol(t, p.a)
	// the lock-out guard: an administrator may not limit the networks to ones that leave their own address out
	if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", pol, "192.0.2.1"); err == nil || !strings.Contains(err.Error(), "lock you out") {
		t.Fatalf("%v", err)
	}
	if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", pol, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	got, err := p.Svc.GetSecurityPolicy(ctx, p.a, "")
	if err != nil || !got.RequireSecondFactor || got.SessionMaxHours != 8 || len(got.IPAllowlist) != 2 || got.IPAllowlist[1] != "198.51.100.7/32" || got.WithoutSecondFactor == nil || *got.WithoutSecondFactor != 1 { // Pat only: the administrator has one now
		t.Fatalf("%+v %v", got, err)
	}
	// the other organization is untouched, and the instance administrator can set any organization's, from anywhere
	if o, _ := p.Svc.GetSecurityPolicy(ctx, p.b, ""); o.RequireSecondFactor || len(o.IPAllowlist) != 0 {
		t.Fatalf("%+v", o)
	}
	if err := p.Svc.SetSecurityPolicy(ctx, p.Admin, p.orgB, domain.SecurityPolicy{IPAllowlist: []string{"192.0.2.0/24"}}, "10.9.9.9"); err != nil {
		t.Fatal(err)
	}
	var n int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.policy_changed'`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d audit records", n)
	}
	// over HTTP: the same rules, with the address taken from the proxy header
	p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{}, "203.0.113.9")
	ck := p.session(t, "boss@acme.example", secretA, "203.0.113.9")
	r, _ := call(t, p.ts, "PUT", "/api/v1/admin/security-policy", map[string]any{"ip_allowlist": []string{"203.0.113.0/24"}}, ck, map[string]string{"X-Forwarded-For": "192.0.2.50"})
	if r.StatusCode != 400 {
		t.Fatalf("a lock-out was allowed over HTTP: %d", r.StatusCode)
	}
	if r, _ := call(t, p.ts, "PUT", "/api/v1/admin/security-policy", map[string]any{"ip_allowlist": []string{"203.0.113.0/24"}}, ck, p.inside); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
}

func TestSEC04_AnAllowListRefusesSignInAndUseFromOtherNetworksAndTheHeaderIsOnlyBelievedFromTrustedProxies(t *testing.T) {
	p := newPolicyEnv(t)
	ctx := testutil.Ctx()
	if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{IPAllowlist: []string{"203.0.113.0/24"}}, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	outside := map[string]string{"X-Forwarded-For": "198.51.100.7"}
	// sign-in: from outside it fails in the same words as a wrong password; from inside it works
	if ck, code := p.signIn(t, "pat@acme.example", outside); ck != nil || code != 401 {
		t.Fatalf("sign-in from outside: %d", code)
	}
	ck, code := p.signIn(t, "pat@acme.example", p.inside)
	if ck == nil || code != 200 {
		t.Fatalf("sign-in from inside: %d", code)
	}
	// a session in use: from the allowed network it works, from another it is refused at once, with the reason
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, p.inside); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
	r, body := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, outside)
	if r.StatusCode != 403 || body["code"] != "ip_not_allowed" {
		t.Fatalf("%d %v", r.StatusCode, body)
	}
	// API tokens are held to it too
	tok, _ := p.Svc.CreateAPIToken(ctx, p.aUser.ID, "ci")
	if r, _ := getWith(t, p.ts, "/api/v1/projects", tok, outside); r.StatusCode != 403 {
		t.Fatalf("a token worked from outside: %d", r.StatusCode)
	}
	if r, _ := getWith(t, p.ts, "/api/v1/projects", tok, p.inside); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
	// the agent endpoint (MCP) applies the same rule through the same check
	pa, _ := p.Svc.ActorFor(ctx, p.aUser.ID)
	if err := p.Svc.EnforceNetwork(ctx, pa, "198.51.100.7"); err == nil {
		t.Fatal("the network check let an outside address through")
	}
	if err := p.Svc.EnforceNetwork(ctx, pa, "203.0.113.200"); err != nil {
		t.Fatal(err)
	}
	// a header that a visitor writes himself, with no trusted proxy in between, is ignored
	p.Svc.TrustedProxies = nil
	r, _ = call(t, p.ts, "GET", "/api/v1/projects", nil, ck, p.inside)
	if r.StatusCode != 403 {
		t.Fatalf("a forged X-Forwarded-For was believed: %d", r.StatusCode)
	}
	p.Svc.TrustedProxies = []*net.IPNet{cidr(t, "127.0.0.0/8")}
	// a chain: the nearest address that is not a proxy is the visitor (a visitor cannot prepend his way in)
	r, _ = call(t, p.ts, "GET", "/api/v1/projects", nil, ck, map[string]string{"X-Forwarded-For": "203.0.113.9, 198.51.100.7"})
	if r.StatusCode != 403 {
		t.Fatalf("an address placed first in the chain was believed: %d", r.StatusCode)
	}
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, map[string]string{"X-Forwarded-For": "198.51.100.7, 203.0.113.9"}); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
	// the other organization, and the instance administrator, are not subject to this list
	ckB, code := p.signIn(t, "boss@globex.example", outside)
	if ckB == nil || code != 200 {
		t.Fatalf("another organization was held to this list: %d", code)
	}
	ckAdmin, code := p.signIn(t, "admin@example.com", outside)
	_ = ckAdmin
	if code != 200 {
		// the testutil administrator is the instance administrator in the Default organization
		t.Fatalf("the instance administrator was refused: %d", code)
	}
	// loosening it takes effect at once for people already signed in
	p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{}, "203.0.113.9")
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, outside); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
}

func TestSEC04_ClientIPFollowsTheForwardedChainOnlyThroughTrustedProxies(t *testing.T) {
	e := testutil.New(t)
	e.Svc.TrustedProxies = []*net.IPNet{cidr(t, "10.0.0.0/8"), cidr(t, "192.168.1.1/32")}
	for _, c := range []struct{ remote, xff, want string }{
		{"203.0.113.5:4000", "", "203.0.113.5"},
		{"203.0.113.5:4000", "1.2.3.4", "203.0.113.5"},                       // not a proxy: the header is ignored
		{"10.1.1.1:4000", "", "10.1.1.1"},                                    // a proxy that sent no header
		{"10.1.1.1:4000", "198.51.100.7", "198.51.100.7"},                    // one proxy
		{"10.1.1.1:4000", "198.51.100.7, 10.2.2.2", "198.51.100.7"},          // two proxies
		{"10.1.1.1:4000", "6.6.6.6, 198.51.100.7, 10.2.2.2", "198.51.100.7"}, // a visitor's own claim is to the left of the real one
		{"10.1.1.1:4000", "garbage", "10.1.1.1"},                             // nothing to believe
		{"10.1.1.1:4000", "198.51.100.7, garbage", "10.1.1.1"},
		{"[2001:db8::1]:4000", "1.2.3.4", "2001:db8::1"},
		{"no-port", "", "no-port"},
	} {
		if got := e.Svc.ClientIP(c.remote, c.xff); got != c.want {
			t.Errorf("%s with %q: %s, want %s", c.remote, c.xff, got, c.want)
		}
	}
	e.Svc.TrustedProxies = nil
	if got := e.Svc.ClientIP("10.1.1.1:4000", "198.51.100.7"); got != "10.1.1.1" {
		t.Errorf("with no trusted proxy: %s", got)
	}
}

func TestSEC04_ASessionNeverOutlivesTheOrganizationsLongestAndTheLimitAppliesAtOnce(t *testing.T) {
	p := newPolicyEnv(t)
	ctx := testutil.Ctx()
	p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{SessionMaxHours: 2}, "")
	ck, _ := p.signIn(t, "pat@acme.example", nil)
	var exp time.Time
	p.Store.Pool.QueryRow(ctx, `SELECT max(expires_at) FROM session s JOIN app_user u ON u.id=s.user_id WHERE u.email='pat@acme.example'`).Scan(&exp)
	if d := exp.Sub(p.Clock.Now()); d > 2*time.Hour+time.Minute || d < time.Hour {
		t.Fatalf("the session lasts %s, not the 2 h the organization allows (the server asked for 24 h)", d)
	}
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, nil); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
	p.Clock.Advance(3 * time.Hour)
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, nil); r.StatusCode != 401 {
		t.Fatalf("a session outlived the limit: %d", r.StatusCode)
	}
	// another organization keeps the server's 24 hours
	ckB, _ := p.signIn(t, "boss@globex.example", nil)
	p.Clock.Advance(-3 * time.Hour)
	_ = ckB
	p.Store.Pool.QueryRow(ctx, `SELECT max(expires_at) FROM session s JOIN app_user u ON u.id=s.user_id WHERE u.email='boss@globex.example'`).Scan(&exp)
	if d := exp.Sub(p.Clock.Now()); d < 20*time.Hour {
		t.Fatalf("another organization's session was cut to %s", d)
	}
}

func TestSEC04_ARequiredSecondFactorHoldsPeopleWithoutOneToTheEnrolmentPagesUntilTheyAddOne(t *testing.T) {
	p := newPolicyEnv(t)
	ctx := testutil.Ctx()
	ck, code := p.signIn(t, "pat@acme.example", nil)
	if ck == nil || code != 200 {
		t.Fatalf("%d", code)
	}
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, nil); r.StatusCode != 200 {
		t.Fatalf("before the policy: %d", r.StatusCode)
	}
	p.enrol(t, p.a) // the administrator first
	if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{RequireSecondFactor: true}, ""); err != nil {
		t.Fatal(err)
	}
	// the person who is already signed in is held at once, and told why
	r, body := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, nil)
	if r.StatusCode != 403 || body["code"] != "second_factor_required" {
		t.Fatalf("%d %v", r.StatusCode, body)
	}
	if r, _ := call(t, p.ts, "POST", "/api/v1/issues", map[string]any{"project": "ABC", "type": "Task", "summary": "x"}, ck, nil); r.StatusCode != 403 {
		t.Fatalf("a write got through: %d", r.StatusCode)
	}
	// who they are, signing out and the pages that set a factor up stay reachable
	for _, path := range []string{"/api/v1/me", "/api/v1/me/2fa", "/api/v1/me/passkeys", "/api/v1/system/status"} {
		if r, _ := call(t, p.ts, "GET", path, nil, ck, nil); r.StatusCode != 200 {
			t.Errorf("%s: %d", path, r.StatusCode)
		}
	}
	// a person's API token is not held (it is an explicit credential they made earlier), and neither is another organization
	tok, _ := p.Svc.CreateAPIToken(ctx, p.aUser.ID, "script")
	if r, _ := getWith(t, p.ts, "/api/v1/projects", tok, nil); r.StatusCode != 200 {
		t.Fatalf("a token was held: %d", r.StatusCode)
	}
	ckB, _ := p.signIn(t, "boss@globex.example", nil)
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ckB, nil); r.StatusCode != 200 {
		t.Fatalf("another organization was held: %d", r.StatusCode)
	}
	// enrolling an authenticator app releases the hold at once
	_, setup := call(t, p.ts, "POST", "/api/v1/me/2fa/setup", nil, ck, nil)
	secret := setup["secret"].(string)
	if r, _ := call(t, p.ts, "POST", "/api/v1/me/2fa/enable", map[string]any{"code": totpNow(t, secret, p.Clock.Now())}, ck, nil); r.StatusCode != 200 {
		t.Fatalf("enrolment: %d", r.StatusCode)
	}
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ck, nil); r.StatusCode != 200 {
		t.Fatalf("still held after enrolling: %d", r.StatusCode)
	}
	// from now on signing in takes the second step, as for anyone with one
	if _, code := p.signIn(t, "pat@acme.example", nil); code != 200 {
		t.Fatalf("%d", code)
	}
	// single sign-on sessions are not held: the provider authenticates them
	u2, _ := p.Svc.CreateUserAs(ctx, p.a, "sam@acme.example", "Sam", "password123", false, "")
	ckSSO, _ := p.signIn(t, "sam@acme.example", nil)
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ckSSO, nil); r.StatusCode != 403 {
		t.Fatalf("a person with no factor was not held: %d", r.StatusCode)
	}
	p.Store.Pool.Exec(ctx, `UPDATE session SET via='sso' WHERE user_id::text=$1`, u2.ID)
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ckSSO, nil); r.StatusCode != 200 {
		t.Fatalf("a single sign-on session was held: %d", r.StatusCode)
	}
	// turning the requirement off releases everyone at once
	ckHeld, _ := p.signIn(t, "sam@acme.example", nil)
	p.Store.Pool.Exec(ctx, `UPDATE session SET via='password' WHERE user_id::text=$1`, u2.ID)
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ckHeld, nil); r.StatusCode != 403 {
		t.Fatalf("%d", r.StatusCode)
	}
	p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{}, "")
	if r, _ := call(t, p.ts, "GET", "/api/v1/projects", nil, ckHeld, nil); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
}

func TestSEC04_AnEmailDomainRestrictionStopsAccountsOutsideItOnEveryPathAndLeavesExistingPeopleAlone(t *testing.T) {
	p := newPolicyEnv(t)
	ctx := testutil.Ctx()
	// before the restriction anyone can be added
	if _, err := p.Svc.CreateUserAs(ctx, p.a, "early@elsewhere.example", "Early", "password123", false, ""); err != nil {
		t.Fatal(err)
	}
	// the list is normalised, and nonsense is refused
	for _, bad := range []string{"*.acme.example", "acme", "pat@acme.example", "ac me.example", "-bad.example", strings.Repeat("a", 64) + ".example"} {
		if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{AllowedEmailDomains: []string{bad}}, ""); err == nil {
			t.Errorf("%q was accepted as a domain", bad)
		}
	}
	if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{AllowedEmailDomains: []string{" @Acme.Example ", "partner.example", "acme.example", ""}}, ""); err != nil {
		t.Fatal(err)
	}
	pol, _ := p.Svc.GetSecurityPolicy(ctx, p.a, "")
	if len(pol.AllowedEmailDomains) != 2 || pol.AllowedEmailDomains[0] != "acme.example" || pol.AllowedEmailDomains[1] != "partner.example" {
		t.Fatalf("%v", pol.AllowedEmailDomains)
	}
	// the people who already have another address are counted and left alone
	if pol.PeopleOutsideDomains == nil || *pol.PeopleOutsideDomains != 1 {
		t.Fatalf("%v", pol.PeopleOutsideDomains)
	}
	if _, code := p.signInAs(t, "early@elsewhere.example"); code != 200 {
		t.Fatalf("an existing person was locked out by the restriction: %d", code)
	}
	// making an account: inside works (any case), outside is refused with the allowed domains named
	if _, err := p.Svc.CreateUserAs(ctx, p.a, "New.Person@ACME.example", "New", "password123", false, ""); err != nil {
		t.Fatalf("an address in an allowed domain: %v", err)
	}
	_, err := p.Svc.CreateUserAs(ctx, p.a, "x@elsewhere.example", "X", "password123", false, "")
	if err == nil || !strings.Contains(err.Error(), "allowed domains") || !strings.Contains(err.Error(), "acme.example") {
		t.Fatalf("an address outside the domains: %v", err)
	}
	// over HTTP the same, as an ordinary bad request
	ckBoss := p.session2(t, "boss@acme.example")
	r, body := call(t, p.ts, "POST", "/api/v1/users", map[string]any{"email": "y@elsewhere.example", "name": "Y", "password": "password123"}, ckBoss, nil)
	if r.StatusCode != 400 || !strings.Contains(fmt.Sprint(body["error"]), "allowed domains") {
		t.Fatalf("%d %v", r.StatusCode, body)
	}
	// the database is the backstop for every other path (SCIM, single sign-on, anything added later): a direct insert is refused too
	if _, err := p.Store.Pool.Exec(ctx, `INSERT INTO app_user(email,name,org_id) VALUES('z@elsewhere.example','Z',$1)`, p.orgA); err == nil {
		t.Fatal("the database accepted an account outside the allowed domains")
	}
	// ... but a placeholder that cannot sign in (made by an import or the Jira mirror) is not an account yet
	if _, err := p.Store.Pool.Exec(ctx, `INSERT INTO app_user(email,name,org_id,active) VALUES('placeholder@elsewhere.example','P',$1,false)`, p.orgA); err != nil {
		t.Fatalf("a placeholder was refused: %v", err)
	}
	// ... and it cannot be activated by a route that does not check (SCIM sets active)
	if _, err := p.Store.Pool.Exec(ctx, `UPDATE app_user SET active=true WHERE email='placeholder@elsewhere.example'`); err == nil {
		t.Fatal("a placeholder outside the domains was activated")
	}
	// a person who already has another address can be deactivated and renamed, but not re-activated
	p.Store.Pool.Exec(ctx, `UPDATE app_user SET name='Renamed' WHERE email='early@elsewhere.example'`)
	if _, err := p.Store.Pool.Exec(ctx, `UPDATE app_user SET active=false WHERE email='early@elsewhere.example'`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.Pool.Exec(ctx, `UPDATE app_user SET active=true WHERE email='early@elsewhere.example'`); err == nil {
		t.Fatal("an account outside the domains was re-activated")
	}
	// another organization has its own rules: here anything goes
	if _, err := p.Svc.CreateUserAs(ctx, p.b, "anyone@elsewhere.example", "Anyone", "password123", false, ""); err != nil {
		t.Fatalf("another organization was held to this list: %v", err)
	}
	// removing the restriction allows everyone again; the change is audited with the list
	if err := p.Svc.SetSecurityPolicy(ctx, p.a, "", domain.SecurityPolicy{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.CreateUserAs(ctx, p.a, "free@elsewhere.example", "Free", "password123", false, ""); err != nil {
		t.Fatalf("after the restriction was removed: %v", err)
	}
	var n int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.policy_changed' AND detail->'allowed_email_domains' @> '"partner.example"'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d audit records carry the list", n)
	}
}

// SCIM is the path a company's directory uses: an account outside the domains is refused there too, in words the directory can show.
func TestSEC04_SCIMCannotProvisionAddressesOutsideTheAllowedDomains(t *testing.T) {
	e := testutil.New(t)
	ctx := testutil.Ctx()
	if err := e.Svc.SetSecurityPolicy(ctx, e.Admin, "", domain.SecurityPolicy{AllowedEmailDomains: []string{"example.com"}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.SCIMCreate(ctx, domain.SCIMIn{UserName: "in@example.com", DisplayName: "In"}); err != nil {
		t.Fatalf("an allowed address: %v", err)
	}
	if _, _, err := e.Svc.SCIMCreate(ctx, domain.SCIMIn{UserName: "out@other.example", DisplayName: "Out"}); err == nil || !strings.Contains(err.Error(), "allowed domains") {
		t.Fatalf("an address outside the domains: %v", err)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE email='out@other.example'`).Scan(&n)
	if n != 0 {
		t.Fatal("the refused account exists")
	}
}
