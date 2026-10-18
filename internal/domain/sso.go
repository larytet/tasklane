package domain

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/egress"
)

// Single sign-on with OpenID Connect (authorization code flow with PKCE). The server never sees the person's password.
// Every call to the identity provider goes through the egress policy; the id token is checked for signature (RS256, keys
// from the provider's JWKS), issuer, audience, expiry and the nonce this sign-in started with.

type SSOProvider struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Label          string   `json:"label"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"client_id"`
	ClientSecret   string   `json:"client_secret,omitempty"` // accepted on save, never returned
	SecretSet      bool     `json:"secret_set"`
	Scopes         string   `json:"scopes"`
	AllowedDomains []string `json:"allowed_domains"`
	JIT            bool     `json:"jit"`
	Enabled        bool     `json:"enabled"`
}

func (s *Service) ssoClient() *http.Client {
	pol := egress.Policy{AllowPrivate: s.SSOAllowPrivate, MaxBody: 1 << 20}
	if s.SSOEgress != nil {
		pol = s.SSOEgress(pol)
	}
	c := pol.Client()
	c.Timeout = 10 * time.Second
	return c
}

var slugRe = strings.NewReplacer(" ", "-")

func (s *Service) SaveSSOProvider(ctx context.Context, a Actor, id int64, in SSOProvider) (SSOProvider, error) {
	if err := adminOnly(a); err != nil {
		return in, err
	}
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	for _, r := range in.Name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return in, invalid("a provider name is lower-case letters, digits and hyphens")
		}
	}
	if in.Name == "" || len(in.Name) > 40 {
		return in, invalid("a provider needs a short name")
	}
	if strings.TrimSpace(in.Label) == "" {
		in.Label = in.Name
	}
	u, err := url.Parse(in.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return in, invalid("the issuer is the identity provider's address, such as https://login.example.com")
	}
	if strings.TrimSpace(in.ClientID) == "" {
		return in, invalid("a provider needs a client id")
	}
	if in.Scopes == "" {
		in.Scopes = "openid email profile"
	}
	if !strings.Contains(" "+in.Scopes+" ", " openid ") {
		return in, invalid("the scopes must include openid")
	}
	for i, d := range in.AllowedDomains {
		in.AllowedDomains[i] = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
	}
	if in.AllowedDomains == nil {
		in.AllowedDomains = []string{}
	}
	var enc any
	if in.ClientSecret != "" {
		b, err := s.seal(ctx, in.ClientSecret)
		if err != nil {
			return in, err
		}
		enc = b
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		dup := func(err error) error {
			if err != nil && strings.Contains(err.Error(), "sso_provider_name_uq") {
				return invalid("a provider named %q exists", in.Name)
			}
			return err
		}
		if id == 0 {
			if err := dup(tx.QueryRow(ctx, `INSERT INTO sso_provider(name,label,issuer,client_id,client_secret_enc,scopes,allowed_domains,jit,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
				in.Name, in.Label, strings.TrimRight(in.Issuer, "/"), in.ClientID, enc, in.Scopes, in.AllowedDomains, in.JIT, in.Enabled).Scan(&id)); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE sso_provider SET name=$2,label=$3,issuer=$4,client_id=$5,client_secret_enc=COALESCE($6,client_secret_enc),scopes=$7,allowed_domains=$8,jit=$9,enabled=$10 WHERE id=$1`,
				id, in.Name, in.Label, strings.TrimRight(in.Issuer, "/"), in.ClientID, enc, in.Scopes, in.AllowedDomains, in.JIT, in.Enabled)
			if err = dup(err); err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
		return s.audit(ctx, tx, a, "sso.provider_saved", fmt.Sprint(id), map[string]any{"name": in.Name, "issuer": in.Issuer, "enabled": in.Enabled})
	})
	if err != nil {
		return in, err
	}
	ps, err := s.ListSSOProviders(ctx, a)
	for _, p := range ps {
		if p.ID == id {
			return p, err
		}
	}
	return in, err
}

func (s *Service) ListSSOProviders(ctx context.Context, a Actor) ([]SSOProvider, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id,name,label,issuer,client_id,client_secret_enc IS NOT NULL,scopes,allowed_domains,jit,enabled FROM sso_provider ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SSOProvider{}
	for rows.Next() {
		var p SSOProvider
		if err := rows.Scan(&p.ID, &p.Name, &p.Label, &p.Issuer, &p.ClientID, &p.SecretSet, &p.Scopes, &p.AllowedDomains, &p.JIT, &p.Enabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) DeleteSSOProvider(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM sso_provider WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "sso.provider_deleted", fmt.Sprint(id), map[string]any{})
	})
}

// SSOChoices are the sign-in buttons: enabled providers only, nothing else about them.
func (s *Service) SSOChoices(ctx context.Context) []map[string]string {
	rows, err := s.DB.Query(ctx, `SELECT name,label FROM sso_provider WHERE enabled ORDER BY name`)
	out := []map[string]string{}
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n, l string
		rows.Scan(&n, &l)
		out = append(out, map[string]string{"name": n, "label": l})
	}
	return out
}

// ----- discovery and keys

type oidcDoc struct {
	Issuer, AuthEndpoint, TokenEndpoint, JWKS string
}

type ssoCache struct {
	mu   sync.Mutex
	docs map[string]struct {
		d  oidcDoc
		at time.Time
	}
	keys map[string]struct {
		k  map[string]*rsa.PublicKey
		at time.Time
	}
}

var ssoC = &ssoCache{docs: map[string]struct {
	d  oidcDoc
	at time.Time
}{}, keys: map[string]struct {
	k  map[string]*rsa.PublicKey
	at time.Time
}{}}

func (s *Service) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := s.ssoClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (s *Service) discover(ctx context.Context, issuer string) (oidcDoc, error) {
	ssoC.mu.Lock()
	if c, ok := ssoC.docs[issuer]; ok && time.Since(c.at) < time.Hour {
		ssoC.mu.Unlock()
		return c.d, nil
	}
	ssoC.mu.Unlock()
	var raw struct {
		Issuer  string `json:"issuer"`
		Auth    string `json:"authorization_endpoint"`
		Token   string `json:"token_endpoint"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := s.getJSON(ctx, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", &raw); err != nil {
		return oidcDoc{}, err
	}
	if strings.TrimRight(raw.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return oidcDoc{}, fmt.Errorf("the provider says it is %q, not %q", raw.Issuer, issuer) // a mismatch means the document is not the provider's
	}
	d := oidcDoc{Issuer: raw.Issuer, AuthEndpoint: raw.Auth, TokenEndpoint: raw.Token, JWKS: raw.JWKSURI}
	if d.AuthEndpoint == "" || d.TokenEndpoint == "" || d.JWKS == "" {
		return d, fmt.Errorf("the provider's configuration is incomplete")
	}
	ssoC.mu.Lock()
	ssoC.docs[issuer] = struct {
		d  oidcDoc
		at time.Time
	}{d, time.Now()}
	ssoC.mu.Unlock()
	return d, nil
}

func (s *Service) jwks(ctx context.Context, uri string, force bool) (map[string]*rsa.PublicKey, error) {
	ssoC.mu.Lock()
	if c, ok := ssoC.keys[uri]; ok && !force && time.Since(c.at) < 10*time.Minute {
		ssoC.mu.Unlock()
		return c.k, nil
	}
	ssoC.mu.Unlock()
	var set struct {
		Keys []struct {
			Kty, Kid, Alg, Use, N, E string
		} `json:"keys"`
	}
	if err := s.getJSON(ctx, uri, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	ssoC.mu.Lock()
	ssoC.keys[uri] = struct {
		k  map[string]*rsa.PublicKey
		at time.Time
	}{keys, time.Now()}
	ssoC.mu.Unlock()
	return keys, nil
}

// verifyIDToken checks an id token's signature and claims; it returns the claims only if every check passes.
func (s *Service) verifyIDToken(ctx context.Context, doc oidcDoc, clientID, nonce, raw string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("the id token is malformed")
	}
	var hdr struct{ Alg, Kid string }
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &hdr) != nil {
		return nil, fmt.Errorf("the id token header is malformed")
	}
	if hdr.Alg != "RS256" { // never "none", never a symmetric algorithm keyed by something public
		return nil, fmt.Errorf("the id token is signed with %q; only RS256 is accepted", hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("the id token signature is malformed")
	}
	verify := func(force bool) error {
		keys, err := s.jwks(ctx, doc.JWKS, force)
		if err != nil {
			return err
		}
		k, ok := keys[hdr.Kid]
		if !ok {
			return fmt.Errorf("unknown signing key")
		}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig)
	}
	if err := verify(false); err != nil {
		if err := verify(true); err != nil { // the provider may have rotated its keys
			return nil, fmt.Errorf("the id token signature is not valid")
		}
	}
	pb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("the id token payload is malformed")
	}
	var c map[string]any
	if json.Unmarshal(pb, &c) != nil {
		return nil, fmt.Errorf("the id token payload is malformed")
	}
	if iss, _ := c["iss"].(string); strings.TrimRight(iss, "/") != strings.TrimRight(doc.Issuer, "/") {
		return nil, fmt.Errorf("the id token is from another issuer")
	}
	okAud := false
	switch a := c["aud"].(type) {
	case string:
		okAud = a == clientID
	case []any:
		for _, x := range a {
			okAud = okAud || x == clientID
		}
	}
	if !okAud {
		return nil, fmt.Errorf("the id token is for another application")
	}
	exp, _ := c["exp"].(float64)
	if int64(exp) <= s.Now().Unix()-60 {
		return nil, fmt.Errorf("the id token has expired")
	}
	if iat, ok := c["iat"].(float64); ok && int64(iat) > s.Now().Unix()+300 {
		return nil, fmt.Errorf("the id token is from the future")
	}
	if n, _ := c["nonce"].(string); n == "" || n != nonce {
		return nil, fmt.Errorf("the id token does not belong to this sign-in")
	}
	return c, nil
}

// ----- the flow

func randString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func safeRedirect(r string) string {
	if strings.HasPrefix(r, "/") && !strings.HasPrefix(r, "//") && !strings.Contains(r, "\\") {
		return r
	}
	return "/"
}

func (s *Service) ssoRedirectURI() string {
	return strings.TrimRight(s.BaseURL, "/") + "/api/v1/auth/sso/callback"
}

// SSOStart begins a sign-in and returns the address to send the browser to.
func (s *Service) SSOStart(ctx context.Context, name, redirect string) (string, error) {
	var p SSOProvider
	err := s.DB.QueryRow(ctx, `SELECT id,name,issuer,client_id,scopes FROM sso_provider WHERE lower(name)=lower($1) AND enabled`, name).Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &p.Scopes)
	if err != nil {
		return "", ErrNotFound
	}
	doc, err := s.discover(ctx, p.Issuer)
	if err != nil {
		return "", fmt.Errorf("the identity provider could not be reached: %w", err)
	}
	state, nonce, verifier := randString(16), randString(16), randString(32)
	if _, err := s.DB.Exec(ctx, `INSERT INTO sso_state(state,provider_id,nonce,verifier,redirect,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, state, p.ID, nonce, verifier, safeRedirect(redirect), s.Now().Add(10*time.Minute)); err != nil {
		return "", err
	}
	s.DB.Exec(ctx, `DELETE FROM sso_state WHERE expires_at < $1`, s.Now())
	chal := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {p.ClientID}, "redirect_uri": {s.ssoRedirectURI()}, "scope": {p.Scopes}, "state": {state}, "nonce": {nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(chal[:])}, "code_challenge_method": {"S256"}}
	sep := "?"
	if strings.Contains(doc.AuthEndpoint, "?") {
		sep = "&"
	}
	return doc.AuthEndpoint + sep + q.Encode(), nil
}

type SSOResult struct {
	Token    string
	User     User
	Redirect string
	Created  bool
}

// SSOCallback finishes a sign-in: the state is used once, the code is exchanged, the id token is verified, and the person
// is matched to an account (by the provider's subject, then by verified email) or created when the provider allows it.
func (s *Service) SSOCallback(ctx context.Context, state, code string, ttl time.Duration, meta ClientMeta) (SSOResult, error) {
	var res SSOResult
	var pid int64
	var nonce, verifier, redirect string
	var exp time.Time
	err := s.DB.QueryRow(ctx, `DELETE FROM sso_state WHERE state=$1 RETURNING provider_id,nonce,verifier,redirect,expires_at`, state).Scan(&pid, &nonce, &verifier, &redirect, &exp)
	if err != nil || !exp.After(s.Now()) {
		return res, ErrForbidden // unknown, used or expired
	}
	res.Redirect = redirect
	var p SSOProvider
	var enc []byte
	var jit bool
	err = s.DB.QueryRow(ctx, `SELECT id,name,issuer,client_id,client_secret_enc,allowed_domains,jit FROM sso_provider WHERE id=$1 AND enabled`, pid).Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &enc, &p.AllowedDomains, &jit)
	if err != nil {
		return res, ErrForbidden
	}
	doc, err := s.discover(ctx, p.Issuer)
	if err != nil {
		return res, ErrForbidden
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {s.ssoRedirectURI()}, "client_id": {p.ClientID}, "code_verifier": {verifier}}
	if len(enc) > 0 {
		secret, err := s.unseal(ctx, enc)
		if err != nil {
			return res, err
		}
		form.Set("client_secret", secret)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", doc.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.ssoClient().Do(req)
	if err != nil {
		return res, ErrForbidden
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok) != nil || tok.IDToken == "" {
		return res, ErrForbidden
	}
	claims, err := s.verifyIDToken(ctx, doc, p.ClientID, nonce, tok.IDToken)
	if err != nil {
		return res, ErrForbidden
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(email))
	verified := claims["email_verified"] == true || claims["email_verified"] == "true"
	name, _ := claims["name"].(string)
	if sub == "" || email == "" || !verified || !strings.Contains(email, "@") {
		return res, ErrForbidden // an address the provider does not vouch for is not an identity
	}
	if len(p.AllowedDomains) > 0 {
		dom := email[strings.LastIndex(email, "@")+1:]
		ok := false
		for _, d := range p.AllowedDomains {
			ok = ok || d == dom
		}
		if !ok {
			return res, ErrForbidden
		}
	}
	var uid string
	var active bool
	err = s.tx(ctx, func(tx pgx.Tx) error {
		e := tx.QueryRow(ctx, `SELECT u.id::text,u.active FROM sso_identity i JOIN app_user u ON u.id=i.user_id WHERE i.provider_id=$1 AND i.subject=$2`, p.ID, sub).Scan(&uid, &active)
		if e == pgx.ErrNoRows {
			e = tx.QueryRow(ctx, `SELECT id::text,active FROM app_user WHERE email=$1`, email).Scan(&uid, &active)
			if e == pgx.ErrNoRows {
				if !jit {
					return ErrForbidden
				}
				if name == "" {
					name = email[:strings.Index(email, "@")]
				}
				if e := tx.QueryRow(ctx, `INSERT INTO app_user(email,name,source) VALUES($1,$2,'sso') RETURNING id::text`, email, name).Scan(&uid); e != nil {
					return e
				}
				active, res.Created = true, true
			} else if e != nil {
				return e
			}
			if _, e := tx.Exec(ctx, `INSERT INTO sso_identity(provider_id,subject,user_id) VALUES($1,$2,$3::uuid) ON CONFLICT DO NOTHING`, p.ID, sub, uid); e != nil {
				return e
			}
		} else if e != nil {
			return e
		}
		if !active {
			return ErrForbidden // a deprovisioned person stays out, whatever the provider says
		}
		return s.audit(ctx, tx, Actor{ID: uid, Label: name}, "security.sso_login", uid, map[string]any{"provider": p.Name, "created": res.Created, "ip": meta.IP})
	})
	if err != nil {
		return res, ErrForbidden
	}
	if err := s.DB.QueryRow(ctx, `SELECT id::text,email,name,kind,is_admin FROM app_user WHERE id::text=$1`, uid).Scan(&res.User.ID, &res.User.Email, &res.User.Name, &res.User.Kind, &res.User.Admin); err != nil {
		return res, err
	}
	res.Token, err = s.newSession(ctx, uid, ttl, meta)
	return res, err
}
