package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/testutil"
)

type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) do(method, path string, body any, hdr ...string) (int, map[string]any, http.Header) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func setup(t *testing.T) (*testutil.Env, *client, *client, string) {
	e := testutil.New(t)
	e.Project()
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: 3600e9}).Handler())
	t.Cleanup(ts.Close)
	mk := func(email string) *client {
		c := &client{t: t, base: ts.URL}
		code, out, _ := c.do("POST", "/api/v1/login", map[string]string{"email": email, "password": "password123"})
		if code != 200 {
			t.Fatalf("login %s: %d %v", email, code, out)
		}
		// API tokens are the simplest bearer for tests
		u, _ := e.Svc.ListUsers(testutil.Ctx())
		for _, x := range u {
			if x.Email == email {
				tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), x.ID, "t")
				c.token = tok
			}
		}
		return c
	}
	alice, bob := mk("alice@example.com"), mk("bob@example.com")
	code, out, _ := alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "type": "Bug", "summary": "Checkout fails", "description": "orig"})
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	return e, alice, bob, out["key"].(string)
}

func TestHTTP_LoginCookieAndCSRF(t *testing.T) { // SEC-05
	e := testutil.New(t)
	e.Project()
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: 3600e9}).Handler())
	defer ts.Close()
	b, _ := json.Marshal(map[string]string{"email": "alice@example.com", "password": "password123"})
	resp, _ := http.Post(ts.URL+"/api/v1/login", "application/json", bytes.NewReader(b))
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "tl_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie flags: %+v", cookie)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/me", nil)
	req.AddCookie(cookie)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Fatalf("GET with cookie: %d", r.StatusCode)
	}
	// a cookie-authenticated write without the custom header is a cross-site risk and is refused
	req, _ = http.NewRequest("POST", ts.URL+"/api/v1/issues", strings.NewReader(`{"project":"ABC","summary":"x"}`))
	req.AddCookie(cookie)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 403 {
		t.Fatalf("CSRF guard: %d", r.StatusCode)
	}
	req.Header.Set("X-Requested-With", "tasklane")
	req, _ = http.NewRequest("POST", ts.URL+"/api/v1/issues", strings.NewReader(`{"project":"ABC","summary":"x"}`))
	req.AddCookie(cookie)
	req.Header.Set("X-Requested-With", "tasklane")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 201 {
		t.Fatalf("with header: %d", r.StatusCode)
	}
	if r, _ := http.Get(ts.URL + "/api/v1/projects"); r.StatusCode != 401 {
		t.Fatalf("anonymous: %d", r.StatusCode)
	}
	if r, _ := http.Get(ts.URL + "/healthz"); r.Header.Get("Content-Security-Policy") == "" || r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}
}

func TestHTTP_LIVE01_FiftyUsersOneWinner(t *testing.T) {
	_, alice, _, key := setup(t)
	_, v, _ := alice.do("GET", "/api/v1/issues/"+key+"/view", nil)
	seq := int(v["issue"].(map[string]any)["description_seq"].(float64))
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, out, _ := alice.do("PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": fmt.Sprint("v", i), "base_seq": seq})
			if code == 409 {
				d := out["detail"].(map[string]any)
				if d["current_seq"].(float64) != 2 || out["code"] != "text_conflict" {
					t.Errorf("409 body: %v", out)
				}
			}
			mu.Lock()
			codes[code]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if codes[200] != 1 || codes[409] != 49 {
		t.Fatalf("%v", codes)
	}
}

func TestHTTP_TextWriteModes(t *testing.T) { // LIVE-07
	_, alice, bob, key := setup(t)
	p := "/api/v1/issues/" + key + "/text/description"
	if c, _, _ := alice.do("PUT", p, map[string]any{"body": "x"}); c != 400 {
		t.Fatalf("missing base_seq: %d", c)
	}
	if c, out, _ := alice.do("POST", p+"/patch", map[string]any{"old_text": "orig", "new_text": "patched"}); c != 200 || out["seq"].(float64) != 2 {
		t.Fatalf("patch: %d %v", c, out)
	}
	if c, out, _ := alice.do("POST", p+"/patch", map[string]any{"old_text": "nope", "new_text": "x"}); c != 409 || out["code"] != "patch_mismatch" {
		t.Fatalf("patch mismatch: %d %v", c, out)
	}
	c, out, _ := bob.do("POST", p+"/replace", map[string]any{"body": "stale"}, "If-Match", "1")
	if c != 412 || out["detail"].(map[string]any)["current"] != "patched" {
		t.Fatalf("stale conditional replace: %d %v", c, out)
	}
	if c, _, _ := bob.do("POST", p+"/replace", map[string]any{"body": "fresh"}, "If-Match", "2"); c != 200 {
		t.Fatalf("fresh conditional replace: %d", c)
	}
	if c, _, _ := bob.do("POST", p+"/replace", map[string]any{"body": "bare"}); c != 200 {
		t.Fatalf("bare replace: %d", c)
	}
	c, out, _ = bob.do("GET", p+"/versions", nil)
	_ = out
	if c != 200 {
		t.Fatal(c)
	}
}

func TestHTTP_ScalarIfMatchAndMerge(t *testing.T) { // LIVE-14, LIVE-16
	_, alice, bob, key := setup(t)
	base := 1
	if c, _, _ := alice.do("PATCH", "/api/v1/issues/"+key, map[string]any{"priority": "high"}, "If-Match", "1"); c != 200 {
		t.Fatal(c)
	}
	// strict If-Match on the stale version: 412, and a real conflict only
	c, out, _ := bob.do("PATCH", "/api/v1/issues/"+key, map[string]any{"priority": "low"}, "If-Match", "1")
	if c != 412 || out["detail"].(map[string]any)["current_version"].(float64) != 2 {
		t.Fatalf("%d %v", c, out)
	}
	// non-strict base_version: a different field merges
	if c, out, _ := bob.do("PATCH", "/api/v1/issues/"+key, map[string]any{"labels": []string{"a"}, "base_version": base}); c != 200 || out["priority"] != "high" {
		t.Fatalf("%d %v", c, out)
	}
	// same field from a stale base: 409 and nothing applied
	if c, _, _ := bob.do("PATCH", "/api/v1/issues/"+key, map[string]any{"priority": "low", "base_version": base}); c != 409 {
		t.Fatalf("%d", c)
	}
}

func TestHTTP_ViewETagAndPermissions(t *testing.T) {
	e, alice, _, key := setup(t)
	c, _, h := alice.do("GET", "/api/v1/issues/"+key+"/view", nil)
	etag := h.Get("ETag")
	if c != 200 || etag == "" || h.Get("Server-Timing") == "" {
		t.Fatalf("%d %q %q", c, etag, h.Get("Server-Timing"))
	}
	if c, _, _ := alice.do("GET", "/api/v1/issues/"+key+"/view", nil, "If-None-Match", etag); c != 304 {
		t.Fatalf("etag revalidation: %d", c)
	}
	alice.do("POST", "/api/v1/issues/"+key+"/comments", map[string]any{"body": "hi"})
	if c, _, _ := alice.do("GET", "/api/v1/issues/"+key+"/view", nil, "If-None-Match", etag); c != 200 {
		t.Fatalf("etag stale after comment: %d", c)
	}
	// outsider gets 404, not 403, so existence is not revealed
	u, _ := e.Svc.CreateUser(testutil.Ctx(), "eve@example.com", "Eve", "password123", false)
	tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), u.ID, "eve")
	eve := &client{t: t, base: alice.base, token: tok}
	if c, _, _ := eve.do("GET", "/api/v1/issues/"+key+"/view", nil); c != 404 {
		t.Fatalf("outsider: %d", c)
	}
	if c, _, _ := eve.do("PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": "x", "base_seq": 1}); c != 404 {
		t.Fatalf("outsider write: %d", c)
	}
}

func TestHTTP_PresenceAndDrafts(t *testing.T) {
	_, alice, bob, key := setup(t)
	bob.do("POST", "/api/v1/issues/"+key+"/presence", map[string]any{"field": "description"})
	if c, _, _ := alice.do("POST", "/api/v1/issues/"+key+"/presence", map[string]any{}); c != 200 {
		t.Fatal(c)
	}
	_, v, _ := alice.do("GET", "/api/v1/issues/"+key+"/view", nil)
	pr := v["presence"].([]any)
	if len(pr) != 1 || pr[0].(map[string]any)["editing"] != "description" {
		t.Fatalf("%v", v["presence"])
	}
	if c, _, _ := alice.do("PUT", "/api/v1/issues/"+key+"/drafts/description", map[string]any{"body": "wip", "base_seq": 1}); c != 200 {
		t.Fatal(c)
	}
	if c, _, _ := bob.do("GET", "/api/v1/issues/"+key+"/drafts", nil); c != 200 {
		t.Fatal(c)
	}
}
