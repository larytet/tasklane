package api_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/testutil"
)

func totpNow(t *testing.T, b32 string, now time.Time) string {
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b32)
	if err != nil {
		t.Fatal(err)
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(now.Unix()/30))
	m := hmac.New(sha1.New, secret)
	m.Write(b[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0xf
	return fmt.Sprintf("%06d", (uint32(sum[off]&0x7f)<<24|uint32(sum[off+1])<<16|uint32(sum[off+2])<<8|uint32(sum[off+3]))%1000000)
}

func post(t *testing.T, ts *httptest.Server, path string, body any, hdr map[string]string, cookies ...*http.Cookie) (*http.Response, map[string]any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "tasklane")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
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

func sessionCookie(r *http.Response) *http.Cookie {
	for _, c := range r.Cookies() {
		if c.Name == "tl_session" {
			return c
		}
	}
	return nil
}

func TestSEC04_HTTPSignInTwoFactorLockoutAndTokens(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: time.Hour}).Handler())
	defer ts.Close()
	login := func(pw string) (*http.Response, map[string]any) {
		return post(t, ts, "/api/v1/login", map[string]string{"email": "alice@example.com", "password": pw}, nil)
	}
	resp, _ := login("password123")
	cookie := sessionCookie(resp)
	if resp.StatusCode != 200 || cookie == nil {
		t.Fatal(resp.StatusCode)
	}
	authed := func(path string, body any) (*http.Response, map[string]any) {
		return post(t, ts, path, body, nil, cookie)
	}
	// enrol in two-factor authentication
	_, setup := authed("/api/v1/me/2fa/setup", nil)
	secret, _ := setup["secret"].(string)
	if secret == "" {
		t.Fatalf("%v", setup)
	}
	r2, en := authed("/api/v1/me/2fa/enable", map[string]string{"code": totpNow(t, secret, e.Clock.T)})
	codes, _ := en["recovery_codes"].([]any)
	if r2.StatusCode != 200 || len(codes) != 10 {
		t.Fatalf("%d %v", r2.StatusCode, en)
	}
	// signing in now gives a second step, and no session cookie until it is done
	resp, body := login("password123")
	if resp.StatusCode != 200 || body["mfa_required"] != true || sessionCookie(resp) != nil {
		t.Fatalf("%d %v", resp.StatusCode, body)
	}
	token, _ := body["mfa_token"].(string)
	if resp, _ = post(t, ts, "/api/v1/login/mfa", map[string]string{"mfa_token": token, "code": "999999"}, nil); resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	resp, _ = post(t, ts, "/api/v1/login/mfa", map[string]string{"mfa_token": token, "code": codes[0].(string)}, nil)
	if resp.StatusCode != 200 || sessionCookie(resp) == nil {
		t.Fatalf("%d", resp.StatusCode)
	}
	// the sessions list shows this device; a token can be made, used and revoked
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/me/sessions", nil)
	req.AddCookie(sessionCookie(resp))
	lr, _ := http.DefaultClient.Do(req)
	var ss []map[string]any
	json.NewDecoder(lr.Body).Decode(&ss)
	lr.Body.Close()
	if len(ss) == 0 || ss[0]["current"] != true {
		t.Fatalf("%v", ss)
	}
	r3, tk := post(t, ts, "/api/v1/me/tokens", map[string]any{"name": "ci", "expires_in_days": 7}, nil, sessionCookie(resp))
	secretTok, _ := tk["token"].(string)
	if r3.StatusCode != 201 || !strings.HasPrefix(secretTok, "tl_") {
		t.Fatalf("%d %v", r3.StatusCode, tk)
	}
	gr := func(tok string) int {
		rq, _ := http.NewRequest("GET", ts.URL+"/api/v1/bootstrap", nil)
		rq.Header.Set("Authorization", "Bearer "+tok)
		x, _ := http.DefaultClient.Do(rq)
		x.Body.Close()
		return x.StatusCode
	}
	if gr(secretTok) != 200 {
		t.Fatal("a fresh token does not work")
	}
	id := tk["info"].(map[string]any)["id"].(string)
	dreq, _ := http.NewRequest("DELETE", ts.URL+"/api/v1/me/tokens/"+id, nil)
	dreq.AddCookie(sessionCookie(resp))
	dreq.Header.Set("X-Requested-With", "tasklane")
	dr, _ := http.DefaultClient.Do(dreq)
	dr.Body.Close()
	if dr.StatusCode != 200 || gr(secretTok) != 401 {
		t.Fatalf("a revoked token: delete %d, use %d", dr.StatusCode, gr(secretTok))
	}
	// an agent token with scopes is not a REST credential
	agent, _ := e.Svc.CreateScopedToken(testutil.Ctx(), e.Admin, e.Alice.ID, "agent", []string{"read"}, time.Hour)
	if gr(agent) != 401 {
		t.Fatal("a scoped token worked on REST")
	}
	// lockout: five wrong passwords, then a 429 with Retry-After even for the right one
	for i := 0; i < 5; i++ {
		if resp, _ = post(t, ts, "/api/v1/login", map[string]string{"email": "bob@example.com", "password": "nope"}, nil); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp, body = post(t, ts, "/api/v1/login", map[string]string{"email": "bob@example.com", "password": "password123"}, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || body["code"] != "locked" {
		t.Fatalf("%d %v %q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
}
