package domain_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/oidcmock"
)

type ssoRig struct {
	*testutil.Env
	idp *oidcmock.Server
	p   domain.SSOProvider
}

func newSSO(t *testing.T, mut func(*domain.SSOProvider)) *ssoRig {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	idp := oidcmock.New(t)
	idp.Now = e.Svc.Now
	e.Svc.BaseURL = "https://tasklane.example.com"
	e.Svc.SSOEgress = func(p egress.Policy) egress.Policy { p.AllowPrivate = true; return p }
	in := domain.SSOProvider{Name: "corp", Label: "Corp login", Issuer: idp.URL, ClientID: "tasklane", Enabled: true, JIT: true}
	if mut != nil {
		mut(&in)
	}
	p, err := e.Svc.SaveSSOProvider(ctx, e.Admin, 0, in)
	if err != nil {
		t.Fatal(err)
	}
	idp.Redirect = "https://tasklane.example.com/api/v1/auth/sso/callback"
	return &ssoRig{Env: e, idp: idp, p: p}
}

// flow runs a whole sign-in and returns the callback's result.
func (r *ssoRig) flow(t *testing.T) (domain.SSOResult, error) {
	t.Helper()
	to, err := r.Svc.SSOStart(ctx, "corp", "/#/issue/ABC-1")
	if err != nil {
		return domain.SSOResult{}, err // a provider that contradicts itself is refused before the browser leaves
	}
	back := r.idp.Authorize(to)
	u, _ := url.Parse(back)
	return r.Svc.SSOCallback(ctx, u.Query().Get("state"), u.Query().Get("code"), time.Hour, domain.ClientMeta{IP: "203.0.113.1"})
}

func TestSEC04_SSOSignInCreatesAndRecognisesAccounts(t *testing.T) {
	r := newSSO(t, nil)
	e := r.Env
	res, err := r.flow(t)
	if err != nil || !res.Created || res.User.Email != "person@example.com" || res.Redirect != "/#/issue/ABC-1" {
		t.Fatalf("%+v %v", res, err)
	}
	a, err := e.Svc.Authenticate(ctx, res.Token)
	if err != nil || a.Label != "A Person" {
		t.Fatalf("%+v %v", a, err)
	}
	// the account has no password: it cannot sign in with one
	if _, err := e.Svc.LoginWith(ctx, "person@example.com", "", time.Hour, domain.ClientMeta{}); err == nil {
		t.Fatal("an account made by single sign-on has a password")
	}
	// the same person again: the same account, found by the provider's subject even after their address changes
	r.idp.Email = "renamed@example.com"
	res2, err := r.flow(t)
	if err != nil || res2.Created || res2.User.ID != res.User.ID {
		t.Fatalf("%+v %v", res2, err)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE source='sso'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d accounts", n)
	}
	// the sign-in is in the audit log
	var audited int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.sso_login'`).Scan(&audited)
	if audited != 2 {
		t.Fatalf("%d audit rows", audited)
	}
	// the choices shown on the sign-in page name enabled providers only
	if ch := e.Svc.SSOChoices(ctx); len(ch) != 1 || ch[0]["name"] != "corp" || ch[0]["label"] != "Corp login" || len(ch[0]) != 2 {
		t.Fatalf("%v", ch)
	}
}

func TestSEC04_SSOAuthorizationRequestUsesPKCEStateAndNonce(t *testing.T) {
	r := newSSO(t, func(p *domain.SSOProvider) { p.ClientSecret = "shh-secret" })
	r.idp.Secret = "shh-secret"
	to, err := r.Svc.SSOStart(ctx, "corp", "")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(to[strings.Index(to, "?")+1:])
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(q.Get(k)) < 16 {
			t.Fatalf("%s is too short: %q", k, q.Get(k))
		}
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "https://tasklane.example.com/api/v1/auth/sso/callback" || q.Get("client_id") != "tasklane" {
		t.Fatalf("%v", q)
	}
	// two sign-ins never share their secrets
	to2, _ := r.Svc.SSOStart(ctx, "corp", "")
	q2, _ := url.ParseQuery(to2[strings.Index(to2, "?")+1:])
	if q2.Get("state") == q.Get("state") || q2.Get("nonce") == q.Get("nonce") || q2.Get("code_challenge") == q.Get("code_challenge") {
		t.Fatal("secrets repeated")
	}
	// the client secret is sent to the token endpoint, stored sealed, and never returned
	back := r.idp.Authorize(to)
	u, _ := url.Parse(back)
	if _, err := r.Svc.SSOCallback(ctx, u.Query().Get("state"), u.Query().Get("code"), time.Hour, domain.ClientMeta{}); err != nil {
		t.Fatalf("with the right secret: %v", err)
	}
	var enc []byte
	r.Store.Pool.QueryRow(ctx, `SELECT client_secret_enc FROM sso_provider`).Scan(&enc)
	if len(enc) == 0 || strings.Contains(string(enc), "shh-secret") {
		t.Fatal("the client secret is not stored encrypted")
	}
	ps, _ := r.Svc.ListSSOProviders(ctx, r.Admin)
	if !ps[0].SecretSet || ps[0].ClientSecret != "" {
		t.Fatalf("%+v", ps[0])
	}
	// an edit that leaves the secret out keeps it
	ps[0].Label = "Renamed"
	if _, err := r.Svc.SaveSSOProvider(ctx, r.Admin, ps[0].ID, ps[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.flow(t); err != nil {
		t.Fatalf("the secret was lost by an edit: %v", err)
	}
	// a wrong secret at the provider fails the sign-in
	r.idp.Secret = "something-else"
	if _, err := r.flow(t); err == nil {
		t.Fatal("signed in with a refused client secret")
	}
	// the redirect after sign-in is a path on this server and nothing else
	for _, bad := range []string{"https://evil.example.com", "//evil.example.com", "/\\evil", "javascript:alert(1)"} {
		to, _ := r.Svc.SSOStart(ctx, "corp", bad)
		r.idp.Secret = "shh-secret"
		u, _ := url.Parse(r.idp.Authorize(to))
		res, err := r.Svc.SSOCallback(ctx, u.Query().Get("state"), u.Query().Get("code"), time.Hour, domain.ClientMeta{})
		if err != nil || res.Redirect != "/" {
			t.Fatalf("%q -> %q %v", bad, res.Redirect, err)
		}
	}
}

func TestSEC04_SSORefusesForgedAndStaleSignIns(t *testing.T) {
	cases := map[string]func(r *ssoRig){
		"wrong audience":           func(r *ssoRig) { r.idp.Aud = "another-app" },
		"wrong nonce":              func(r *ssoRig) { r.idp.Nonce = "replayed-nonce" },
		"expired token":            func(r *ssoRig) { r.idp.ExpOffset = -2 * time.Hour },
		"another issuer":           func(r *ssoRig) { r.idp.Iss = "https://evil.example.com" },
		"alg none":                 func(r *ssoRig) { r.idp.Alg = "none" },
		"symmetric algorithm":      func(r *ssoRig) { r.idp.Alg = "HS256" },
		"another key":              func(r *ssoRig) { r.idp.SignWithOtherKey = true },
		"no id token":              func(r *ssoRig) { r.idp.OmitIDToken = true },
		"unverified email":         func(r *ssoRig) { r.idp.EmailVerified = false },
		"verified as string false": func(r *ssoRig) { r.idp.EmailVerified = "false" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newSSO(t, nil)
			mut(r)
			if _, err := r.flow(t); err == nil {
				t.Fatal("a sign-in that should have been refused was accepted")
			}
			var n int
			r.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE source='sso'`).Scan(&n)
			if n != 0 || len(r.Env.Svc.SSOChoices(ctx)) != 1 {
				t.Fatalf("an account was created by a refused sign-in")
			}
		})
	}
	t.Run("state is single use and expires", func(t *testing.T) {
		r := newSSO(t, nil)
		to, _ := r.Svc.SSOStart(ctx, "corp", "")
		u, _ := url.Parse(r.idp.Authorize(to))
		st, code := u.Query().Get("state"), u.Query().Get("code")
		if _, err := r.Svc.SSOCallback(ctx, st, code, time.Hour, domain.ClientMeta{}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Svc.SSOCallback(ctx, st, code, time.Hour, domain.ClientMeta{}); err == nil {
			t.Fatal("a callback was accepted twice")
		}
		if _, err := r.Svc.SSOCallback(ctx, "made-up-state", code, time.Hour, domain.ClientMeta{}); err == nil {
			t.Fatal("an unknown state was accepted")
		}
		to2, _ := r.Svc.SSOStart(ctx, "corp", "")
		u2, _ := url.Parse(r.idp.Authorize(to2))
		r.Clock.T = r.Clock.T.Add(11 * time.Minute)
		if _, err := r.Svc.SSOCallback(ctx, u2.Query().Get("state"), u2.Query().Get("code"), time.Hour, domain.ClientMeta{}); err == nil {
			t.Fatal("an expired sign-in was accepted")
		}
	})
	t.Run("a code belongs to one sign-in: a stolen code with another state fails", func(t *testing.T) {
		r := newSSO(t, nil)
		toA, _ := r.Svc.SSOStart(ctx, "corp", "")
		toB, _ := r.Svc.SSOStart(ctx, "corp", "")
		ua, _ := url.Parse(r.idp.Authorize(toA))
		ub, _ := url.Parse(r.idp.Authorize(toB))
		// B's code with A's state: the PKCE verifier is A's, the code's challenge is B's
		if _, err := r.Svc.SSOCallback(ctx, ua.Query().Get("state"), ub.Query().Get("code"), time.Hour, domain.ClientMeta{}); err == nil {
			t.Fatal("a code from another sign-in was accepted")
		}
	})
	t.Run("keys rotate", func(t *testing.T) {
		r := newSSO(t, nil)
		if _, err := r.flow(t); err != nil {
			t.Fatal(err)
		}
		r.idp.RotateKey("k2") // the old key set is cached; an unknown key id makes the server look again
		r.idp.Sub = "sub-2"
		r.idp.Email = "second@example.com"
		if _, err := r.flow(t); err != nil {
			t.Fatalf("after a key rotation: %v", err)
		}
	})
	t.Run("a private identity provider is refused unless allowed", func(t *testing.T) {
		r := newSSO(t, nil)
		r.Svc.SSOEgress = nil
		if _, err := r.Svc.SSOStart(ctx, "corp", ""); err == nil {
			t.Fatal("the egress policy let the server call a loopback identity provider")
		}
	})
}

func TestSEC04_SSOAccountMatchingAndPolicy(t *testing.T) {
	t.Run("an existing account is linked by a verified email", func(t *testing.T) {
		r := newSSO(t, nil)
		r.idp.Email = "alice@example.com"
		res, err := r.flow(t)
		if err != nil || res.Created || res.User.ID != r.Alice.ID {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("only allowed domains", func(t *testing.T) {
		r := newSSO(t, func(p *domain.SSOProvider) { p.AllowedDomains = []string{"@Corp.example"} })
		r.idp.Email = "someone@elsewhere.example"
		if _, err := r.flow(t); err == nil {
			t.Fatal("a domain outside the allow-list signed in")
		}
		r.idp.Email = "someone@corp.example"
		if _, err := r.flow(t); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no automatic accounts when switched off", func(t *testing.T) {
		r := newSSO(t, func(p *domain.SSOProvider) { p.JIT = false })
		if _, err := r.flow(t); err == nil {
			t.Fatal("an account was created with JIT off")
		}
		r.idp.Email = "bob@example.com" // but people who have an account may sign in
		r.idp.Sub = "bob-sub"
		if res, err := r.flow(t); err != nil || res.User.ID != r.Bob.ID {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("a deactivated account stays out", func(t *testing.T) {
		r := newSSO(t, nil)
		res, err := r.flow(t)
		if err != nil {
			t.Fatal(err)
		}
		r.Store.Pool.Exec(ctx, `UPDATE app_user SET active=false WHERE id::text=$1`, res.User.ID)
		if _, err := r.flow(t); err == nil {
			t.Fatal("a deactivated person signed in through the provider")
		}
	})
	t.Run("a disabled provider offers no sign-in", func(t *testing.T) {
		r := newSSO(t, nil)
		ps, _ := r.Svc.ListSSOProviders(ctx, r.Admin)
		ps[0].Enabled = false
		r.Svc.SaveSSOProvider(ctx, r.Admin, ps[0].ID, ps[0])
		if _, err := r.Svc.SSOStart(ctx, "corp", ""); err != domain.ErrNotFound {
			t.Fatalf("%v", err)
		}
		if len(r.Svc.SSOChoices(ctx)) != 0 {
			t.Fatal("a disabled provider is listed")
		}
	})
	t.Run("only administrators configure providers", func(t *testing.T) {
		r := newSSO(t, nil)
		if _, err := r.Svc.SaveSSOProvider(ctx, r.Alice, 0, domain.SSOProvider{Name: "x", Issuer: "https://x.example.com", ClientID: "c"}); err == nil {
			t.Fatal("a member added a provider")
		}
		for name, in := range map[string]domain.SSOProvider{
			"bad name":   {Name: "Bad Name!", Issuer: "https://x.example.com", ClientID: "c"},
			"bad issuer": {Name: "x", Issuer: "not a url", ClientID: "c"},
			"no client":  {Name: "x", Issuer: "https://x.example.com"},
			"no openid":  {Name: "x", Issuer: "https://x.example.com", ClientID: "c", Scopes: "email"},
			"duplicate":  {Name: "CORP", Issuer: "https://x.example.com", ClientID: "c"},
		} {
			if _, err := r.Svc.SaveSSOProvider(ctx, r.Admin, 0, in); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})
}
