package domain

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/larytet/tasklane/internal/webauthn"
)

// Passkeys and security keys as a second factor. A password is still the first step; with a registered key the second step can be
// the key, and TOTP and recovery codes keep working beside it. Adding or removing a key needs the password again, so a stolen
// session cannot plant a key of its own. The relying party is the host of `base_url` (or `webauthn_rp_id`): a registration made
// for one host does not work on another.

const webauthnChallengeTTL = 5 * time.Minute

type Passkey struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Created  time.Time  `json:"created_at"`
	LastUsed *time.Time `json:"last_used_at,omitempty"`
}

// WebAuthnRelyingParty derives the relying party from the configuration: an explicit id and origins, or the host of base_url. An
// IP address cannot be a relying party id, so a base_url that is one leaves passkeys unavailable.
func (s *Service) webauthnRP() (webauthn.RP, error) {
	rp := webauthn.RP{ID: s.WebAuthnRPID, Origins: s.WebAuthnOrigins, RequireUV: s.WebAuthnRequireUV}
	if rp.ID == "" || len(rp.Origins) == 0 {
		u, err := url.Parse(s.BaseURL)
		if err != nil || u.Hostname() == "" {
			return rp, invalid("passkeys need base_url (or webauthn_rp_id) to be set")
		}
		if rp.ID == "" {
			rp.ID = u.Hostname()
		}
		if len(rp.Origins) == 0 {
			rp.Origins = []string{u.Scheme + "://" + u.Host}
		}
	}
	if net.ParseIP(rp.ID) != nil || strings.Contains(rp.ID, ":") {
		return rp, invalid("passkeys need a host name, not an IP address (%s): set base_url to a name", rp.ID)
	}
	return rp, nil
}

// WebAuthnAvailable says whether passkeys can be used on this instance (the page hides the control when not).
func (s *Service) WebAuthnAvailable() bool { _, err := s.webauthnRP(); return err == nil }

func (s *Service) checkOwnPassword(ctx context.Context, userID, password string) error {
	var hash *string
	if err := s.DB.QueryRow(ctx, `SELECT password_hash FROM app_user WHERE id::text=$1`, userID).Scan(&hash); err != nil || hash == nil || bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)) != nil {
		return ErrForbidden
	}
	return nil
}

func (s *Service) credentialDescriptors(ctx context.Context, userID string) ([]map[string]any, error) {
	rows, err := s.DB.Query(ctx, `SELECT cred_id FROM webauthn_credential WHERE user_id::text=$1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"type": "public-key", "id": webauthn.Encode(id)})
	}
	return out, rows.Err()
}

// PasskeyRegistrationOptions starts adding a key: it checks the password and returns what navigator.credentials.create needs.
func (s *Service) PasskeyRegistrationOptions(ctx context.Context, a Actor, password string) (map[string]any, error) {
	rp, err := s.webauthnRP()
	if err != nil {
		return nil, err
	}
	if a.ID == "" || a.Kind == "agent" {
		return nil, ErrForbidden
	}
	if err := s.checkOwnPassword(ctx, a.ID, password); err != nil {
		return nil, err
	}
	var email, name string
	if err := s.DB.QueryRow(ctx, `SELECT email,name FROM app_user WHERE id::text=$1`, a.ID).Scan(&email, &name); err != nil {
		return nil, err
	}
	ch := webauthn.NewChallenge()
	if _, err := s.DB.Exec(ctx, `INSERT INTO webauthn_challenge(user_id,challenge,expires_at) VALUES($1,$2,$3) ON CONFLICT (user_id) DO UPDATE SET challenge=$2, expires_at=$3`, a.ID, ch, s.Now().Add(webauthnChallengeTTL)); err != nil {
		return nil, err
	}
	exclude, err := s.credentialDescriptors(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	uid := strings.ReplaceAll(a.ID, "-", "") // the user handle: opaque, not the address
	return map[string]any{"publicKey": map[string]any{
		"rp":                     map[string]any{"id": rp.ID, "name": "Tasklane"},
		"user":                   map[string]any{"id": webauthn.Encode([]byte(uid)), "name": email, "displayName": name},
		"challenge":              webauthn.Encode(ch),
		"pubKeyCredParams":       []map[string]any{{"type": "public-key", "alg": webauthn.AlgES256}, {"type": "public-key", "alg": webauthn.AlgEdDSA}, {"type": "public-key", "alg": webauthn.AlgRS256}},
		"timeout":                60000,
		"attestation":            "none",
		"excludeCredentials":     exclude,
		"authenticatorSelection": map[string]any{"residentKey": "preferred", "userVerification": "preferred"},
	}}, nil
}

type PasskeyRegistration struct {
	Name              string `json:"name"`
	ClientDataJSON    string `json:"client_data_json"`   // base64url
	AttestationObject string `json:"attestation_object"` // base64url
}

// AddPasskey finishes adding a key: the answer to the challenge from PasskeyRegistrationOptions, verified.
func (s *Service) AddPasskey(ctx context.Context, a Actor, in PasskeyRegistration) (Passkey, error) {
	rp, err := s.webauthnRP()
	if err != nil {
		return Passkey{}, err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 60 {
		return Passkey{}, invalid("name the key (up to 60 characters)")
	}
	var ch []byte
	var exp time.Time
	// the challenge is used once, whatever the outcome
	if err := s.DB.QueryRow(ctx, `DELETE FROM webauthn_challenge WHERE user_id::text=$1 RETURNING challenge,expires_at`, a.ID).Scan(&ch, &exp); err != nil || !exp.After(s.Now()) {
		return Passkey{}, invalid("the registration expired: start again")
	}
	cd, err1 := webauthn.Decode64(in.ClientDataJSON)
	att, err2 := webauthn.Decode64(in.AttestationObject)
	if err1 != nil || err2 != nil {
		return Passkey{}, invalid("the answer is not valid")
	}
	cred, err := rp.VerifyRegistration(ch, cd, att)
	if err != nil {
		return Passkey{}, invalid("the key could not be registered: %v", err)
	}
	p := Passkey{Name: name}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO webauthn_credential(user_id,cred_id,public_key,alg,sign_count,name) VALUES($1,$2,$3,$4,$5,$6) RETURNING id,created_at`,
			a.ID, cred.ID, cred.PublicKey, cred.Alg, int64(cred.SignCount), name).Scan(&p.ID, &p.Created); err != nil {
			if strings.Contains(err.Error(), "webauthn_credential_cred_id_key") {
				return invalid("that key is already registered")
			}
			return err
		}
		return s.audit(ctx, tx, a, "security.passkey_added", a.ID, map[string]any{"name": name})
	})
	return p, err
}

func (s *Service) ListPasskeys(ctx context.Context, a Actor) ([]Passkey, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,created_at,last_used_at FROM webauthn_credential WHERE user_id::text=$1 ORDER BY id`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.ID, &p.Name, &p.Created, &p.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RemovePasskey deletes one of the person's own keys; the password is asked again.
func (s *Service) RemovePasskey(ctx context.Context, a Actor, id int64, password string) error {
	if err := s.checkOwnPassword(ctx, a.ID, password); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var name string
		if err := tx.QueryRow(ctx, `DELETE FROM webauthn_credential WHERE id=$1 AND user_id::text=$2 RETURNING name`, id, a.ID).Scan(&name); err != nil {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "security.passkey_removed", a.ID, map[string]any{"name": name})
	})
}

// webauthnChallengeFor makes the sign-in challenge for a person with keys, returning the options the browser needs and the
// challenge to keep with the pending sign-in.
func (s *Service) webauthnLoginOptions(ctx context.Context, userID string) (map[string]any, []byte) {
	rp, err := s.webauthnRP()
	if err != nil {
		return nil, nil
	}
	allow, err := s.credentialDescriptors(ctx, userID)
	if err != nil || len(allow) == 0 {
		return nil, nil
	}
	ch := webauthn.NewChallenge()
	uv := "preferred"
	if rp.RequireUV {
		uv = "required"
	}
	return map[string]any{"publicKey": map[string]any{"challenge": webauthn.Encode(ch), "rpId": rp.ID, "timeout": 60000, "allowCredentials": allow, "userVerification": uv}}, ch
}

type PasskeyAssertion struct {
	Token             string `json:"mfa_token"`
	ID                string `json:"id"`                 // base64url credential id
	ClientDataJSON    string `json:"client_data_json"`   // base64url
	AuthenticatorData string `json:"authenticator_data"` // base64url
	Signature         string `json:"signature"`          // base64url
}

// LoginWebAuthn finishes a sign-in with a key: the token from the password step and the key's signature over the challenge that
// went with it. Only a key registered to the person who gave the password counts.
func (s *Service) LoginWebAuthn(ctx context.Context, in PasskeyAssertion, meta ClientMeta) (LoginResult, error) {
	var userID string
	var attempts, ttlSecs int
	var exp time.Time
	var ch []byte
	err := s.DB.QueryRow(ctx, `SELECT user_id::text,attempts,ttl_secs,expires_at,wa_challenge FROM mfa_pending WHERE token_hash=$1`, hashToken(in.Token)).Scan(&userID, &attempts, &ttlSecs, &exp, &ch)
	if err != nil || !exp.After(s.Now()) || len(ch) == 0 {
		s.DB.Exec(ctx, `DELETE FROM mfa_pending WHERE token_hash=$1`, hashToken(in.Token))
		return LoginResult{}, ErrForbidden
	}
	fail := func() (LoginResult, error) {
		if attempts+1 >= maxMFAAttempts {
			s.DB.Exec(ctx, `DELETE FROM mfa_pending WHERE token_hash=$1`, hashToken(in.Token))
		} else {
			s.DB.Exec(ctx, `UPDATE mfa_pending SET attempts=attempts+1 WHERE token_hash=$1`, hashToken(in.Token))
		}
		return LoginResult{}, ErrForbidden
	}
	rp, err := s.webauthnRP()
	if err != nil {
		return fail()
	}
	id, e1 := webauthn.Decode64(in.ID)
	cd, e2 := webauthn.Decode64(in.ClientDataJSON)
	ad, e3 := webauthn.Decode64(in.AuthenticatorData)
	sig, e4 := webauthn.Decode64(in.Signature)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return fail()
	}
	var credRow int64
	var cred webauthn.Credential
	var count int64
	if err := s.DB.QueryRow(ctx, `SELECT id,public_key,alg,sign_count FROM webauthn_credential WHERE cred_id=$1 AND user_id::text=$2`, id, userID).Scan(&credRow, &cred.PublicKey, &cred.Alg, &count); err != nil {
		return fail() // not this person's key
	}
	cred.SignCount = uint32(count)
	n, err := rp.VerifyAssertion(ch, cred, cd, ad, sig)
	if err != nil {
		if errors.Is(err, webauthn.ErrCloned) {
			s.auditAbout(ctx, userID, "system", "security.passkey_clone_suspected")
		}
		return fail()
	}
	var u User
	var active bool
	if err := s.DB.QueryRow(ctx, `SELECT id,email,name,kind,is_admin,active FROM app_user WHERE id::text=$1`, userID).Scan(&u.ID, &u.Email, &u.Name, &u.Kind, &u.Admin, &active); err != nil || !active {
		return fail()
	}
	// the challenge is spent by the pending row being claimed: two answers to one challenge cannot both sign in
	tag, err := s.DB.Exec(ctx, `DELETE FROM mfa_pending WHERE token_hash=$1`, hashToken(in.Token))
	if err != nil || tag.RowsAffected() == 0 {
		return LoginResult{}, ErrForbidden
	}
	s.DB.Exec(ctx, `UPDATE webauthn_credential SET sign_count=$2, last_used_at=$3 WHERE id=$1`, credRow, int64(n), s.Now())
	s.recordAttempt(ctx, u.Email, meta, true)
	tok, err := s.newSession(ctx, u.ID, time.Duration(ttlSecs)*time.Second, meta)
	return LoginResult{Token: tok, User: u}, err
}
