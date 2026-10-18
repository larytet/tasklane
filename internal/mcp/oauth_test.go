package mcp_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

func pkce() (verifier, challenge string) {
	verifier = "verifier-" + strings.Repeat("abcdefghij", 5)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

type oauthRig struct {
	*env
	client string
}

func newOAuth(t *testing.T) *oauthRig {
	e := newEnv(t)
	e.Clock.T = time.Now()
	id, err := e.Svc.OAuthRegister(ctx, "Claude Test", []string{"http://127.0.0.1:7777/callback"})
	if err != nil {
		t.Fatal(err)
	}
	return &oauthRig{env: e, client: id}
}

func (r *oauthRig) authorize(t *testing.T, a domain.Actor, scope, challenge string, approve bool) (url.Values, error) {
	to, err := r.Svc.OAuthAuthorize(ctx, a, domain.AuthorizeReq{ClientID: r.client, RedirectURI: "http://127.0.0.1:7777/callback", Scope: scope, State: "st-123", Challenge: challenge, Method: "S256", ResponseType: "code"}, approve)
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(to)
	if !strings.HasPrefix(to, "http://127.0.0.1:7777/callback?") {
		t.Fatalf("redirected to %q", to)
	}
	return u.Query(), nil
}

func (r *oauthRig) exchange(code, verifier string) (domain.OAuthTokens, error) {
	return r.Svc.OAuthToken(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "client_id": {r.client}, "redirect_uri": {"http://127.0.0.1:7777/callback"}})
}

func (r *oauthRig) refresh(tok string) (domain.OAuthTokens, error) {
	return r.Svc.OAuthToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok}, "client_id": {r.client}})
}

func (r *oauthRig) works(t *testing.T, access string) bool {
	_, _, err := r.Svc.AuthenticateAgent(ctx, access)
	return err == nil
}

func TestMCP04_AuthorizationCodeWithPKCEGivesAScopedTokenForThePerson(t *testing.T) {
	r := newOAuth(t)
	e := r.env
	verifier, challenge := pkce()
	q, err := r.authorize(t, e.Alice, "read comment", challenge, true)
	if err != nil || q.Get("state") != "st-123" || q.Get("code") == "" {
		t.Fatalf("%v %v", q, err)
	}
	tok, err := r.exchange(q.Get("code"), verifier)
	if err != nil || tok.TokenType != "Bearer" || tok.ExpiresIn != 3600 || tok.RefreshToken == "" || tok.Scope != "comment read" {
		t.Fatalf("%+v %v", tok, err)
	}
	// the token works over MCP, for the person, as an agent "via" them, within its scopes
	ag := &agent{t: t, e: e, token: tok.AccessToken}
	ag.init()
	if r := ag.ok("list_projects", nil); r.Err {
		t.Fatal("read was refused")
	}
	c := ag.ok("add_comment", map[string]any{"key": e.key, "body": "from an OAuth client"})
	if c.Err {
		t.Fatalf("%+v", c)
	}
	v, _ := e.Svc.View(ctx, e.Admin, e.key)
	last := v.Comments[len(v.Comments)-1]
	if last.Body != "from an OAuth client" || !strings.Contains(last.Author.Name, "Alice") {
		t.Fatalf("%+v", last)
	}
	h, _ := e.Svc.History(ctx, e.Admin, e.key)
	_ = h
	if r := ag.call("update_fields", map[string]any{"key": e.key, "priority": "high"}); !r.Err || !strings.Contains(r.Text, "scope") {
		t.Fatalf("a scope that was not granted was usable: %+v", r)
	}
	// the code was spent: using it again fails, whoever asks
	if _, err := r.exchange(q.Get("code"), verifier); err == nil {
		t.Fatal("a code was accepted twice")
	}
}

func TestMCP04_CodeExchangeChecksEverythingItWasIssuedFor(t *testing.T) {
	r := newOAuth(t)
	e := r.env
	verifier, challenge := pkce()
	fresh := func() string {
		q, err := r.authorize(t, e.Alice, "read", challenge, true)
		if err != nil {
			t.Fatal(err)
		}
		return q.Get("code")
	}
	if _, err := r.exchange(fresh(), "not-the-verifier-"+strings.Repeat("x", 30)); err == nil {
		t.Fatal("the wrong PKCE verifier was accepted")
	}
	if _, err := r.exchange(fresh(), ""); err == nil {
		t.Fatal("no PKCE verifier was accepted")
	}
	other, _ := e.Svc.OAuthRegister(ctx, "Another", []string{"http://127.0.0.1:8888/cb"})
	form := func(code string) url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "client_id": {r.client}, "redirect_uri": {"http://127.0.0.1:7777/callback"}}
	}
	f := form(fresh())
	f.Set("client_id", other)
	if _, err := e.Svc.OAuthToken(ctx, f); err == nil {
		t.Fatal("a code was exchanged by another client")
	}
	f = form(fresh())
	f.Set("redirect_uri", "http://127.0.0.1:7777/elsewhere")
	if _, err := e.Svc.OAuthToken(ctx, f); err == nil {
		t.Fatal("a code was exchanged with another redirect_uri")
	}
	code := fresh()
	e.Clock.T = e.Clock.T.Add(3 * time.Minute)
	if _, err := r.exchange(code, verifier); err == nil {
		t.Fatal("an expired code was accepted")
	}
	if _, err := e.Svc.OAuthToken(ctx, url.Values{"grant_type": {"password"}}); err == nil {
		t.Fatal("an unsupported grant type")
	}
	// the account must still be active when the code is exchanged
	code = fresh()
	e.Store.Pool.Exec(ctx, `UPDATE app_user SET active=false WHERE id::text=$1`, e.Alice.ID)
	if _, err := r.exchange(code, verifier); err == nil {
		t.Fatal("a deactivated person's code was exchanged")
	}
}

func TestMCP04_AuthorizationRequestsAreValidatedAndRedirectsAreExact(t *testing.T) {
	r := newOAuth(t)
	e := r.env
	_, challenge := pkce()
	for _, bad := range []string{"javascript:alert(1)", "file:///etc/passwd", "data:text/html,x", "http://evil.example.com/cb", "https://example.com/cb#frag", "not a url"} {
		if _, err := e.Svc.OAuthRegister(ctx, "Bad", []string{bad}); err == nil {
			t.Errorf("registered the redirect %q", bad)
		}
	}
	for _, good := range []string{"https://app.example.com/cb", "http://localhost:3000/cb", "http://127.0.0.1/cb", "cursor://anysphere.cursor-mcp/oauth/callback"} {
		if _, err := e.Svc.OAuthRegister(ctx, "Good", []string{good}); err != nil {
			t.Errorf("refused %q: %v", good, err)
		}
	}
	if _, err := e.Svc.OAuthRegister(ctx, "", []string{"https://a.example.com/cb"}); err == nil {
		t.Error("a nameless client")
	}
	// an unregistered redirect address is an error and never a redirect, however close it is
	for _, redirect := range []string{"http://127.0.0.1:7777/callback/", "http://127.0.0.1:7777/callbac", "http://127.0.0.1:7777/callback?x=1", "http://127.0.0.1:7778/callback"} {
		_, err := e.Svc.OAuthAuthorize(ctx, e.Alice, domain.AuthorizeReq{ClientID: r.client, RedirectURI: redirect, Scope: "read", Challenge: challenge, Method: "S256", ResponseType: "code"}, true)
		if oe, ok := err.(*domain.OAuthError); !ok || oe.Code != "invalid_request" {
			t.Errorf("%q: %v", redirect, err)
		}
	}
	if _, err := e.Svc.OAuthAuthorize(ctx, e.Alice, domain.AuthorizeReq{ClientID: "tlc_nope", RedirectURI: "http://127.0.0.1:7777/callback", Challenge: challenge, Method: "S256", ResponseType: "code"}, true); err == nil {
		t.Error("an unknown client")
	}
	// problems the client can be told about come back as a redirect with an error and its state
	for name, a := range map[string]domain.AuthorizeReq{
		"no pkce":         {Method: "", Challenge: ""},
		"plain pkce":      {Method: "plain", Challenge: challenge},
		"short challenge": {Method: "S256", Challenge: "abc"},
		"implicit":        {Method: "S256", Challenge: challenge, ResponseType: "token"},
	} {
		a.ClientID, a.RedirectURI, a.Scope, a.State = r.client, "http://127.0.0.1:7777/callback", "read", "xyz"
		if a.ResponseType == "" {
			a.ResponseType = "code"
		}
		to, err := e.Svc.OAuthAuthorize(ctx, e.Alice, a, true)
		u, _ := url.Parse(to)
		if err != nil || u.Query().Get("error") == "" || u.Query().Get("code") != "" || u.Query().Get("state") != "xyz" {
			t.Errorf("%s: %q %v", name, to, err)
		}
	}
	if _, err := e.Svc.OAuthAuthorize(ctx, e.Alice, domain.AuthorizeReq{ClientID: r.client, RedirectURI: "http://127.0.0.1:7777/callback", Scope: "read admin", Challenge: challenge, Method: "S256", ResponseType: "code"}, true); err == nil {
		t.Error("a scope that does not exist")
	}
	// a person who declines is sent back with access_denied and no code
	q, err := r.authorize(t, e.Alice, "read", challenge, false)
	if err != nil || q.Get("error") != "access_denied" || q.Get("code") != "" || q.Get("state") != "st-123" {
		t.Fatalf("%v %v", q, err)
	}
	// an agent or a token cannot approve an application
	ag := e.newAgent("sneaky", "member", nil)
	if _, err := r.authorize(t, domain.Actor{ID: ag.user.ID, Label: "Agent", Kind: "agent"}, "read", challenge, true); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
}

func TestMCP04_RefreshTokensRotateAndReuseRevokesTheGrant(t *testing.T) {
	r := newOAuth(t)
	e := r.env
	verifier, challenge := pkce()
	q, _ := r.authorize(t, e.Alice, "read", challenge, true)
	t1, err := r.exchange(q.Get("code"), verifier)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := r.refresh(t1.RefreshToken)
	if err != nil || t2.AccessToken == t1.AccessToken || t2.RefreshToken == t1.RefreshToken {
		t.Fatalf("%+v %v", t2, err)
	}
	if r.works(t, t1.AccessToken) || !r.works(t, t2.AccessToken) {
		t.Fatal("after a refresh only the newest access token works")
	}
	t3, err := r.refresh(t2.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	// someone presents a refresh token that was already used: the grant and every token it issued end
	if _, err := r.refresh(t1.RefreshToken); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("%v", err)
	}
	if r.works(t, t3.AccessToken) {
		t.Fatal("the grant survived a refresh-token replay")
	}
	if _, err := r.refresh(t3.RefreshToken); err == nil {
		t.Fatal("the legitimate holder could still refresh after the grant was revoked")
	}
	// the wrong client cannot use someone else's refresh token
	q, _ = r.authorize(t, e.Alice, "read", challenge, true)
	good, _ := r.exchange(q.Get("code"), verifier)
	other, _ := e.Svc.OAuthRegister(ctx, "Another", []string{"http://127.0.0.1:8888/cb"})
	if _, err := e.Svc.OAuthToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {good.RefreshToken}, "client_id": {other}}); err == nil {
		t.Fatal("another client refreshed the grant")
	}
	// access tokens last an hour, refresh tokens thirty days
	e.Clock.T = e.Clock.T.Add(61 * time.Minute)
	if r.works(t, good.AccessToken) {
		t.Fatal("an access token outlived its hour")
	}
	fresh, err := r.refresh(good.RefreshToken)
	if err != nil || !r.works(t, fresh.AccessToken) {
		t.Fatalf("%v", err)
	}
	e.Clock.T = e.Clock.T.Add(31 * 24 * time.Hour)
	if _, err := r.refresh(fresh.RefreshToken); err == nil {
		t.Fatal("an expired refresh token worked")
	}
}

func TestMCP04_RevocationEndsAccessAtOnce(t *testing.T) {
	r := newOAuth(t)
	e := r.env
	verifier, challenge := pkce()
	grant := func() domain.OAuthTokens {
		q, _ := r.authorize(t, e.Alice, "read", challenge, true)
		tk, err := r.exchange(q.Get("code"), verifier)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	// the revocation endpoint accepts either token and ends the whole grant, and answers the same for junk
	a := grant()
	e.Svc.OAuthRevoke(ctx, a.AccessToken)
	if r.works(t, a.AccessToken) {
		t.Fatal("a revoked access token works")
	}
	if _, err := r.refresh(a.RefreshToken); err == nil {
		t.Fatal("a revoked grant refreshed")
	}
	b := grant()
	e.Svc.OAuthRevoke(ctx, b.RefreshToken)
	if r.works(t, b.AccessToken) {
		t.Fatal("revoking the refresh token left the access token alive")
	}
	e.Svc.OAuthRevoke(ctx, "junk")
	e.Svc.OAuthRevoke(ctx, "")
	// the person lists and disconnects their apps
	c := grant()
	gs, err := e.Svc.ListOAuthGrants(ctx, e.Alice)
	if err != nil || len(gs) != 1 || gs[0].Client != "Claude Test" || strings.Join(gs[0].Scopes, ",") != "read" {
		t.Fatalf("%+v %v", gs, err)
	}
	if err := e.Svc.RevokeOAuthGrant(ctx, e.Bob, gs[0].ID); err != domain.ErrNotFound {
		t.Fatalf("someone else's grant: %v", err)
	}
	if err := e.Svc.RevokeOAuthGrant(ctx, e.Alice, gs[0].ID); err != nil {
		t.Fatal(err)
	}
	if r.works(t, c.AccessToken) {
		t.Fatal("a disconnected app still works")
	}
	if gs, _ := e.Svc.ListOAuthGrants(ctx, e.Alice); len(gs) != 0 {
		t.Fatalf("%+v", gs)
	}
	// offboarding ends grants too, and a deactivated account cannot refresh
	d := grant()
	if _, err := e.Svc.RevokeUserAccess(ctx, e.Admin, e.Alice.ID, true); err != nil {
		t.Fatal(err)
	}
	if r.works(t, d.AccessToken) {
		t.Fatal("offboarding left an app connected")
	}
	if _, err := r.refresh(d.RefreshToken); err == nil {
		t.Fatal("offboarding left a refresh token alive")
	}
}

func TestMCP04_AClientNeverExceedsThePerson(t *testing.T) {
	r := newOAuth(t)
	e := r.env
	// a viewer approves every scope: the client still cannot change anything, because the person cannot
	viewer := e.NewUser("vera@example.com", "Vera V.", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	verifier, challenge := pkce()
	q, err := r.authorize(t, viewer, strings.Join(domain.AllScopes, " "), challenge, true)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.exchange(q.Get("code"), verifier)
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent{t: t, e: e, token: tok.AccessToken}
	ag.init()
	if r := ag.ok("get_issue", map[string]any{"key": e.key}); r.Err {
		t.Fatal("a viewer's client cannot read")
	}
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"add_comment", map[string]any{"key": e.key, "body": "x"}},
		{"update_fields", map[string]any{"key": e.key, "priority": "low"}},
		{"transition_issue", map[string]any{"key": e.key, "transition": "Start progress"}},
	} {
		if res := ag.call(call.tool, call.args); !res.Err {
			t.Errorf("%s worked for a viewer's client", call.tool)
		}
	}
}

func TestMCP04_HTTPDiscoveryAndTokenEndpoint(t *testing.T) {
	r := newOAuth(t)
	e := r.env
	get := func(path string, hdr map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	// an unauthenticated MCP call says where to find the authorization server
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+e.srv.URL+`/.well-known/oauth-protected-resource"`) {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	_, body := get("/.well-known/oauth-protected-resource", nil)
	var pr map[string]any
	json.Unmarshal([]byte(body), &pr)
	if pr["resource"] != e.srv.URL+"/mcp" || pr["authorization_servers"].([]any)[0] != e.srv.URL {
		t.Fatalf("%s", body)
	}
	_, body = get("/.well-known/oauth-authorization-server", nil)
	var as map[string]any
	json.Unmarshal([]byte(body), &as)
	if as["code_challenge_methods_supported"].([]any)[0] != "S256" || as["token_endpoint"] != e.srv.URL+"/oauth/token" || as["registration_endpoint"] != e.srv.URL+"/oauth/register" || as["token_endpoint_auth_methods_supported"].([]any)[0] != "none" {
		t.Fatalf("%s", body)
	}
	// dynamic registration over HTTP, then the browser landing on the authorization endpoint
	rb := strings.NewReader(`{"client_name":"Registered By HTTP","redirect_uris":["http://127.0.0.1:5555/cb"]}`)
	rr, _ := http.Post(e.srv.URL+"/oauth/register", "application/json", rb)
	var reg map[string]any
	json.NewDecoder(rr.Body).Decode(&reg)
	rr.Body.Close()
	if rr.StatusCode != 201 || !strings.HasPrefix(reg["client_id"].(string), "tlc_") || reg["token_endpoint_auth_method"] != "none" {
		t.Fatalf("%d %v", rr.StatusCode, reg)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	_, challenge := pkce()
	au := e.srv.URL + "/oauth/authorize?response_type=code&client_id=" + reg["client_id"].(string) + "&redirect_uri=" + url.QueryEscape("http://127.0.0.1:5555/cb") + "&scope=read&state=s&code_challenge=" + challenge + "&code_challenge_method=S256"
	ar, _ := noFollow.Get(au)
	ar.Body.Close()
	if ar.StatusCode != 302 || !strings.HasPrefix(ar.Header.Get("Location"), "/#/oauth/consent?") {
		t.Fatalf("%d %q", ar.StatusCode, ar.Header.Get("Location"))
	}
	bad, _ := noFollow.Get(e.srv.URL + "/oauth/authorize?client_id=" + reg["client_id"].(string) + "&redirect_uri=" + url.QueryEscape("http://127.0.0.1:5555/other"))
	bad.Body.Close()
	if bad.StatusCode == 302 {
		t.Fatal("redirected to an address that was not registered")
	}
	// the token endpoint speaks the protocol's error format and never caches
	tr, _ := http.PostForm(e.srv.URL+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {"nope"}})
	var terr map[string]string
	json.NewDecoder(tr.Body).Decode(&terr)
	tr.Body.Close()
	if tr.StatusCode != 400 || terr["error"] != "invalid_grant" || tr.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v %q", tr.StatusCode, terr, tr.Header.Get("Cache-Control"))
	}
}
