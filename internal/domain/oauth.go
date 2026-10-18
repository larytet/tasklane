package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// OAuth 2.1 for MCP clients (MCP-04). Public clients register themselves (RFC 7591), a signed-in person approves the
// scopes, the client exchanges a single-use code with a PKCE verifier for a short-lived access token and a refresh token
// that rotates on every use; presenting an old refresh token again revokes the whole grant.

const (
	oauthCodeTTL    = 2 * time.Minute
	oauthAccessTTL  = time.Hour
	oauthRefreshTTL = 30 * 24 * time.Hour
	maxOAuthClients = 5000
	maxRegPerHour   = 200
)

// OAuthError is an error in the form the protocol wants it.
type OAuthError struct{ Code, Description string }

func (e *OAuthError) Error() string { return e.Code + ": " + e.Description }

func oerr(code, desc string) error { return &OAuthError{code, desc} }

func hashSecret(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func randURLToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// validRedirect: https, a loopback address over http (native apps), or a custom scheme a desktop app registered. Never
// a scheme that runs code or reads files, never a fragment.
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" || len(raw) > 500 {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "javascript", "data", "file", "vbscript", "about", "blob", "ftp":
		return false
	case "https":
		return u.Host != ""
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return true
}

// OAuthRegister adds a public client. It is open to anyone (the client has no account yet), so it is limited.
func (s *Service) OAuthRegister(ctx context.Context, name string, redirects []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", oerr("invalid_client_metadata", "client_name is 1 to 100 characters")
	}
	if len(redirects) == 0 || len(redirects) > 5 {
		return "", oerr("invalid_redirect_uri", "give between one and five redirect_uris")
	}
	for _, r := range redirects {
		if !validRedirect(r) {
			return "", oerr("invalid_redirect_uri", "a redirect_uri is https, http on localhost, or a custom application scheme")
		}
	}
	var total, recent int
	s.DB.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE created_at > $1) FROM oauth_client`, s.Now().Add(-time.Hour)).Scan(&total, &recent)
	if total >= maxOAuthClients || recent >= maxRegPerHour {
		return "", oerr("temporarily_unavailable", "too many registrations; try again later")
	}
	id := "tlc_" + randURLToken(16)
	_, err := s.DB.Exec(ctx, `INSERT INTO oauth_client(id,name,redirect_uris,created_at) VALUES($1,$2,$3,$4)`, id, name, redirects, s.Now())
	return id, err
}

type OAuthClient struct {
	ID        string
	Name      string
	Redirects []string
}

func (s *Service) oauthClient(ctx context.Context, id string) (OAuthClient, error) {
	var c OAuthClient
	err := s.DB.QueryRow(ctx, `SELECT id,name,redirect_uris FROM oauth_client WHERE id=$1`, id).Scan(&c.ID, &c.Name, &c.Redirects)
	if err == pgx.ErrNoRows {
		return c, oerr("invalid_client", "unknown client")
	}
	return c, err
}

type AuthorizeReq struct {
	ClientID, RedirectURI, Scope, State, Challenge, Method, ResponseType string
}

func parseScopes(s string) ([]string, error) {
	sc := strings.Fields(s)
	if len(sc) == 0 {
		sc = []string{"read"}
	}
	seen := map[string]bool{}
	var out []string
	for _, x := range sc {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	if err := validScopes(out); err != nil {
		return nil, oerr("invalid_scope", err.Error())
	}
	sort.Strings(out)
	return out, nil
}

// OAuthCheck validates an authorization request up to the point where an error may be sent to the client: an unknown
// client or a redirect address that was not registered is never redirected to.
func (s *Service) OAuthCheck(ctx context.Context, r AuthorizeReq) (OAuthClient, []string, error) {
	c, err := s.oauthClient(ctx, r.ClientID)
	if err != nil {
		return c, nil, err
	}
	ok := false
	for _, u := range c.Redirects {
		ok = ok || subtle.ConstantTimeCompare([]byte(u), []byte(r.RedirectURI)) == 1 // an exact match, not a prefix
	}
	if !ok {
		return c, nil, oerr("invalid_request", "the redirect_uri is not one this client registered")
	}
	sc, err := parseScopes(r.Scope)
	if err != nil {
		return c, nil, err
	}
	return c, sc, nil
}

func redirectWith(base string, params map[string]string) string {
	u, _ := url.Parse(base)
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// OAuthAuthorize records a person's decision and returns where to send the browser: back to the client with a code, or
// with an error. Only a signed-in person can decide; a token cannot approve another client.
func (s *Service) OAuthAuthorize(ctx context.Context, a Actor, r AuthorizeReq, approve bool) (string, error) {
	c, scopes, err := s.OAuthCheck(ctx, r)
	if err != nil {
		return "", err
	}
	fail := func(code string) (string, error) {
		return redirectWith(r.RedirectURI, map[string]string{"error": code, "state": r.State}), nil
	}
	if r.ResponseType != "code" {
		return fail("unsupported_response_type")
	}
	if r.Method != "S256" || len(r.Challenge) < 43 || len(r.Challenge) > 128 {
		return fail("invalid_request") // PKCE with S256 is required
	}
	if !approve {
		return fail("access_denied")
	}
	if a.Kind == "agent" || a.Kind == "bot" {
		return "", ErrForbidden
	}
	code := randURLToken(32)
	if _, err := s.DB.Exec(ctx, `INSERT INTO oauth_code(code_hash,client_id,user_id,redirect_uri,scopes,challenge,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`,
		hashSecret(code), c.ID, a.ID, r.RedirectURI, scopes, r.Challenge, s.Now().Add(oauthCodeTTL)); err != nil {
		return "", err
	}
	s.DB.Exec(ctx, `DELETE FROM oauth_code WHERE expires_at < $1`, s.Now())
	return redirectWith(r.RedirectURI, map[string]string{"code": code, "state": r.State}), nil
}

type OAuthTokens struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Service) issueTokens(ctx context.Context, tx pgx.Tx, grantID int64, userID, clientName string, scopes []string) (OAuthTokens, error) {
	access := "tl_" + hex.EncodeToString(func() []byte { b := make([]byte, 32); rand.Read(b); return b }())
	refresh := "tlr_" + randURLToken(32)
	if _, err := tx.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE oauth_grant_id=$1 AND revoked_at IS NULL`, grantID, s.Now()); err != nil {
		return OAuthTokens{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO api_token(token_hash,user_id,name,scopes,expires_at,oauth_grant_id) VALUES($1,$2,$3,$4,$5,$6)`, hashToken(access), userID, clientName, scopes, s.Now().Add(oauthAccessTTL), grantID); err != nil {
		return OAuthTokens{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE oauth_grant SET old_refresh_hashes=CASE WHEN refresh_hash IS NULL THEN old_refresh_hashes ELSE (old_refresh_hashes || refresh_hash)[greatest(cardinality(old_refresh_hashes || refresh_hash)-19,1):] END,refresh_hash=$2,refresh_expires_at=$3,last_used_at=$4 WHERE id=$1`, grantID, hashSecret(refresh), s.Now().Add(oauthRefreshTTL), s.Now()); err != nil {
		return OAuthTokens{}, err
	}
	return OAuthTokens{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(oauthAccessTTL.Seconds()), RefreshToken: refresh, Scope: strings.Join(scopes, " ")}, nil
}

// OAuthToken is the token endpoint: authorization_code and refresh_token grants.
func (s *Service) OAuthToken(ctx context.Context, form url.Values) (OAuthTokens, error) {
	switch form.Get("grant_type") {
	case "authorization_code":
		code, verifier := form.Get("code"), form.Get("code_verifier")
		var clientID, redirect, challenge, userID string
		var scopes []string
		var exp time.Time
		// the code is spent by the first request that presents it, successful or not
		err := s.DB.QueryRow(ctx, `DELETE FROM oauth_code WHERE code_hash=$1 RETURNING client_id,user_id::text,redirect_uri,scopes,challenge,expires_at`, hashSecret(code)).Scan(&clientID, &userID, &redirect, &scopes, &challenge, &exp)
		if err != nil || !exp.After(s.Now()) {
			return OAuthTokens{}, oerr("invalid_grant", "the code is unknown, used or expired")
		}
		if clientID != form.Get("client_id") || redirect != form.Get("redirect_uri") {
			return OAuthTokens{}, oerr("invalid_grant", "the code was issued to a different client or redirect_uri")
		}
		sum := sha256.Sum256([]byte(verifier))
		if verifier == "" || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) != 1 {
			return OAuthTokens{}, oerr("invalid_grant", "the code_verifier does not match")
		}
		var active bool
		if err := s.DB.QueryRow(ctx, `SELECT active FROM app_user WHERE id::text=$1`, userID).Scan(&active); err != nil || !active {
			return OAuthTokens{}, oerr("invalid_grant", "the account is not active")
		}
		c, err := s.oauthClient(ctx, clientID)
		if err != nil {
			return OAuthTokens{}, err
		}
		var out OAuthTokens
		err = s.tx(ctx, func(tx pgx.Tx) error {
			var gid int64
			if err := tx.QueryRow(ctx, `INSERT INTO oauth_grant(client_id,user_id,scopes) VALUES($1,$2::uuid,$3) RETURNING id`, clientID, userID, scopes).Scan(&gid); err != nil {
				return err
			}
			var e error
			out, e = s.issueTokens(ctx, tx, gid, userID, c.Name, scopes)
			if e != nil {
				return e
			}
			return s.audit(ctx, tx, Actor{ID: userID, Label: "OAuth"}, "oauth.grant_created", clientID, map[string]any{"client": c.Name, "scopes": scopes})
		})
		return out, err
	case "refresh_token":
		rt := form.Get("refresh_token")
		var out OAuthTokens
		err := s.tx(ctx, func(tx pgx.Tx) error {
			var gid int64
			var clientID, userID, cur string
			var scopes []string
			var refreshExp *time.Time
			var revoked *time.Time
			var curHash []byte
			err := tx.QueryRow(ctx, `SELECT id,client_id,user_id::text,scopes,refresh_hash,refresh_expires_at,revoked_at FROM oauth_grant
				WHERE refresh_hash=$1 OR $1 = ANY(old_refresh_hashes) FOR UPDATE`, hashSecret(rt)).Scan(&gid, &clientID, &userID, &scopes, &curHash, &refreshExp, &revoked)
			if err == pgx.ErrNoRows {
				return oerr("invalid_grant", "unknown refresh token")
			}
			if err != nil {
				return err
			}
			_ = cur
			if revoked != nil {
				return oerr("invalid_grant", "this grant was revoked")
			}
			if subtle.ConstantTimeCompare(curHash, hashSecret(rt)) != 1 {
				// an older token again: someone kept a copy. The grant and everything it issued ends.
				if _, err := tx.Exec(ctx, `UPDATE oauth_grant SET revoked_at=$2 WHERE id=$1`, gid, s.Now()); err != nil {
					return err
				}
				tx.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE oauth_grant_id=$1 AND revoked_at IS NULL`, gid, s.Now())
				s.audit(ctx, tx, Actor{ID: userID, Label: "OAuth"}, "oauth.refresh_reuse", clientID, map[string]any{"grant": gid})
				return oerr("invalid_grant", "this refresh token was already used; the grant has been revoked")
			}
			if form.Get("client_id") != clientID {
				return oerr("invalid_grant", "the refresh token belongs to another client")
			}
			if refreshExp == nil || !refreshExp.After(s.Now()) {
				return oerr("invalid_grant", "the refresh token has expired")
			}
			var active bool
			if err := tx.QueryRow(ctx, `SELECT active FROM app_user WHERE id::text=$1`, userID).Scan(&active); err != nil || !active {
				return oerr("invalid_grant", "the account is not active")
			}
			var name string
			tx.QueryRow(ctx, `SELECT name FROM oauth_client WHERE id=$1`, clientID).Scan(&name)
			var e error
			out, e = s.issueTokens(ctx, tx, gid, userID, name, scopes)
			return e
		})
		if oe, ok := err.(*OAuthError); ok && oe.Code == "invalid_grant" && strings.Contains(oe.Description, "already used") {
			// the revocation above must stick even though the request failed: it ran in the transaction we rolled back
			s.DB.Exec(ctx, `UPDATE oauth_grant SET revoked_at=$2 WHERE (refresh_hash=$1 OR $1 = ANY(old_refresh_hashes)) AND revoked_at IS NULL`, hashSecret(rt), s.Now())
			s.DB.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE oauth_grant_id IN (SELECT id FROM oauth_grant WHERE refresh_hash=$1 OR $1 = ANY(old_refresh_hashes)) AND revoked_at IS NULL`, hashSecret(rt), s.Now())
		}
		return out, err
	}
	return OAuthTokens{}, oerr("unsupported_grant_type", "use authorization_code or refresh_token")
}

// OAuthRevoke revokes a token's whole grant. It answers the same whatever the token was, as the protocol asks.
func (s *Service) OAuthRevoke(ctx context.Context, token string) {
	if token == "" {
		return
	}
	var gid *int64
	s.DB.QueryRow(ctx, `SELECT oauth_grant_id FROM api_token WHERE token_hash=$1`, hashToken(token)).Scan(&gid)
	if gid == nil {
		var id int64
		if s.DB.QueryRow(ctx, `SELECT id FROM oauth_grant WHERE refresh_hash=$1 OR $1 = ANY(old_refresh_hashes)`, hashSecret(token)).Scan(&id) == nil {
			gid = &id
		}
	}
	if gid == nil {
		return
	}
	s.DB.Exec(ctx, `UPDATE oauth_grant SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL`, *gid, s.Now())
	s.DB.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE oauth_grant_id=$1 AND revoked_at IS NULL`, *gid, s.Now())
}

type OAuthGrant struct {
	ID       int64      `json:"id"`
	Client   string     `json:"client"`
	Scopes   []string   `json:"scopes"`
	Created  time.Time  `json:"created"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

// ListOAuthGrants shows the apps a person has connected.
func (s *Service) ListOAuthGrants(ctx context.Context, a Actor) ([]OAuthGrant, error) {
	rows, err := s.DB.Query(ctx, `SELECT g.id,c.name,g.scopes,g.created_at,g.last_used_at FROM oauth_grant g JOIN oauth_client c ON c.id=g.client_id WHERE g.user_id=$1 AND g.revoked_at IS NULL ORDER BY g.id DESC`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OAuthGrant{}
	for rows.Next() {
		var g OAuthGrant
		if err := rows.Scan(&g.ID, &g.Client, &g.Scopes, &g.Created, &g.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeOAuthGrant disconnects one of the person's apps at once.
func (s *Service) RevokeOAuthGrant(ctx context.Context, a Actor, id int64) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE oauth_grant SET revoked_at=$3 WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, id, a.ID, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE oauth_grant_id=$1 AND revoked_at IS NULL`, id, s.Now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "oauth.grant_revoked", "", map[string]any{"grant": id})
	})
}
