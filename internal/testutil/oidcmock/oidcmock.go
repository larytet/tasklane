// Package oidcmock is an in-process OpenID Connect identity provider for tests. It is strict like the real thing: codes
// are single use, the PKCE verifier must match the challenge, the client id, secret and redirect address must match, and
// id tokens are signed RS256 with a key published at its JWKS address. Tests can also make it misbehave.
package oidcmock

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type Server struct {
	*httptest.Server
	t        *testing.T
	mu       sync.Mutex
	key      *rsa.PrivateKey
	kid      string
	other    *rsa.PrivateKey
	ClientID string
	Secret   string
	Redirect string
	codes    map[string]*grant
	Now      func() time.Time
	// the person who signs in
	Sub, Email, Name string
	EmailVerified    any
	// Misbehaviour, for tests of the checks
	Aud, Nonce, Iss, Alg string
	ExpOffset            time.Duration
	SignWithOtherKey     bool
	OmitIDToken          bool
	TokenRequests        int
}

type grant struct {
	challenge, nonce, clientID, redirect string
	used                                 bool
}

func New(t *testing.T) *Server {
	s := &Server{t: t, codes: map[string]*grant{}, Now: time.Now, ClientID: "tasklane", Sub: "sub-1", Email: "person@example.com", Name: "A Person", EmailVerified: true}
	s.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	s.other, _ = rsa.GenerateKey(rand.Reader, 2048)
	s.kid = "k1"
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Server.Close)
	return s
}

// RotateKey replaces the signing key under a new id, as a provider does on a schedule.
func (s *Server) RotateKey(kid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	s.kid = kid
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		iss := s.URL
		if s.Iss != "" {
			iss = s.Iss
		}
		json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "authorization_endpoint": s.URL + "/authorize", "token_endpoint": s.URL + "/token", "jwks_uri": s.URL + "/jwks"})
	case "/jwks":
		s.mu.Lock()
		defer s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": s.kid, "n": b64(s.key.N.Bytes()), "e": b64(big.NewInt(int64(s.key.E)).Bytes())}}})
	case "/token":
		s.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Authorize plays the browser: it takes the address the application sent the person to, "signs the person in", and returns
// the address the provider would send the browser back to (with the code and the state).
func (s *Server) Authorize(authURL string) string {
	s.t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, s.URL+"/authorize") {
		s.t.Fatalf("not an address at this provider: %q", authURL)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		s.t.Fatalf("the authorization request is missing required parts: %v", q)
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		s.t.Fatalf("the scope lacks openid: %q", q.Get("scope"))
	}
	code := fmt.Sprintf("code-%d", len(s.codes)+1)
	s.mu.Lock()
	s.codes[code] = &grant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), clientID: q.Get("client_id"), redirect: q.Get("redirect_uri")}
	s.mu.Unlock()
	back := q.Get("redirect_uri") + "?code=" + code + "&state=" + url.QueryEscape(q.Get("state"))
	return back
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.TokenRequests++
	s.mu.Unlock()
	fail := func(msg string) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	if r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" {
		fail("unsupported_grant_type")
		return
	}
	s.mu.Lock()
	g := s.codes[r.Form.Get("code")]
	if g == nil || g.used {
		s.mu.Unlock()
		fail("invalid_grant")
		return
	}
	g.used = true
	s.mu.Unlock()
	if r.Form.Get("client_id") != s.ClientID || g.clientID != s.ClientID || r.Form.Get("redirect_uri") != g.redirect {
		fail("invalid_client")
		return
	}
	if s.Secret != "" && r.Form.Get("client_secret") != s.Secret {
		fail("invalid_client")
		return
	}
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if b64(sum[:]) != g.challenge {
		fail("invalid_grant") // PKCE: the verifier is not the one the challenge came from
		return
	}
	if s.OmitIDToken {
		json.NewEncoder(w).Encode(map[string]string{"access_token": "x", "token_type": "Bearer"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "token_type": "Bearer", "id_token": s.IDToken(g.nonce)})
}

// IDToken builds a signed token for the configured person, with whatever misbehaviour is switched on.
func (s *Server) IDToken(nonce string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss, aud := s.URL, s.ClientID
	if s.Iss != "" {
		iss = s.Iss
	}
	if s.Aud != "" {
		aud = s.Aud
	}
	if s.Nonce != "" {
		nonce = s.Nonce
	}
	now := s.Now()
	claims := map[string]any{"iss": iss, "aud": aud, "sub": s.Sub, "email": s.Email, "email_verified": s.EmailVerified, "name": s.Name, "nonce": nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour + s.ExpOffset).Unix()}
	alg := "RS256"
	if s.Alg != "" {
		alg = s.Alg
	}
	hb, _ := json.Marshal(map[string]string{"alg": alg, "kid": s.kid, "typ": "JWT"})
	pb, _ := json.Marshal(claims)
	signing := b64(hb) + "." + b64(pb)
	switch alg {
	case "none":
		return signing + "."
	case "HS256": // a forgery signed with the public key as if it were a shared secret
		return signing + "." + b64([]byte("not a real signature"))
	}
	k := s.key
	if s.SignWithOtherKey {
		k = s.other
	}
	h := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, h[:])
	return signing + "." + b64(sig)
}
