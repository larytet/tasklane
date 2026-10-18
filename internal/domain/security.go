package domain

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Account security (SEC-04): sign-in with lockout, two-factor authentication (TOTP and recovery codes), sessions and API
// tokens people can list and revoke, password changes. Every check reads the database on every request, so a revoked
// session or token, a deactivated account or a switched-off second factor takes effect on the next request.

const (
	maxLoginFailures = 5
	lockoutWindow    = 15 * time.Minute
	mfaPendingTTL    = 5 * time.Minute
	maxMFAAttempts   = 5
)

// ErrMFARequired is returned by Login for an account with two-factor authentication: the password was right, a code is due.
type MFARequired struct {
	Token string
	TOTP  bool           // a code from an authenticator app (or a recovery code) is accepted
	Key   map[string]any // when the person has a passkey or security key: the options for navigator.credentials.get
}

func (e *MFARequired) Error() string { return "a second factor is required" }

// LockedError: too many failed sign-ins for this address; it applies whether or not the account exists, so it reveals nothing.
type LockedError struct{ Until time.Time }

func (e *LockedError) Error() string { return "too many failed sign-ins; try again later" }

type ClientMeta struct{ IP, UserAgent string }

type LoginResult struct {
	Token string
	User  User
}

func (s *Service) minPassword() int {
	if s.PasswordMinLength > 8 {
		return s.PasswordMinLength
	}
	return 8 // never weaker than eight characters
}

func (s *Service) checkPassword(email, pw string) error {
	if len(pw) < s.minPassword() {
		return invalid("a password needs at least %d characters", s.minPassword())
	}
	if strings.EqualFold(pw, email) {
		return invalid("a password cannot be the email address")
	}
	return nil
}

func (s *Service) lockedUntil(ctx context.Context, email string) (time.Time, bool) {
	rows, err := s.DB.Query(ctx, `SELECT at FROM login_attempt WHERE email=$1 AND NOT ok AND at > $2 AND at > COALESCE((SELECT max(at) FROM login_attempt WHERE email=$1 AND ok), '-infinity') ORDER BY at DESC LIMIT $3`,
		email, s.Now().Add(-lockoutWindow), maxLoginFailures)
	if err != nil {
		return time.Time{}, false
	}
	defer rows.Close()
	var ts []time.Time
	for rows.Next() {
		var t time.Time
		rows.Scan(&t)
		ts = append(ts, t)
	}
	if len(ts) < maxLoginFailures {
		return time.Time{}, false
	}
	return ts[len(ts)-1].Add(lockoutWindow), true // the oldest of the last five: the lock lifts when it ages out
}

func (s *Service) recordAttempt(ctx context.Context, email string, meta ClientMeta, ok bool) {
	s.DB.Exec(ctx, `INSERT INTO login_attempt(email,ip,ok,at) VALUES($1,$2,$3,$4)`, email, meta.IP, ok, s.Now())
	s.DB.Exec(ctx, `DELETE FROM login_attempt WHERE at < $1`, s.Now().Add(-24*time.Hour))
}

func (s *Service) newSession(ctx context.Context, userID string, ttl time.Duration, meta ClientMeta) (string, error) {
	tok := newToken()
	ua := meta.UserAgent
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO session(token_hash,user_id,expires_at,user_agent,ip,last_seen_at) VALUES($1,$2,$3,$4,$5,$6)`, hashToken(tok), userID, s.Now().Add(ttl), ua, meta.IP, s.Now())
	return tok, err
}

// LoginWith checks a password. Without two-factor authentication it returns a session; with it, an *MFARequired carrying
// the token for the second step. Repeated failures lock the address for fifteen minutes.
func (s *Service) LoginWith(ctx context.Context, email, password string, ttl time.Duration, meta ClientMeta) (LoginResult, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if until, locked := s.lockedUntil(ctx, email); locked {
		return LoginResult{}, &LockedError{Until: until}
	}
	var u User
	var hash *string
	var active bool
	err := s.DB.QueryRow(ctx, `SELECT id,email,name,kind,is_admin,password_hash,active FROM app_user WHERE email=$1`, email).Scan(&u.ID, &u.Email, &u.Name, &u.Kind, &u.Admin, &hash, &active)
	if err != nil || hash == nil || !active {
		bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5BUAdfHnVbCySeS0W5iuEQ2sYD8mS"), []byte(password)) // constant-ish time
		s.recordAttempt(ctx, email, meta, false)
		return LoginResult{}, ErrForbidden
	}
	if bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)) != nil {
		s.recordAttempt(ctx, email, meta, false)
		return LoginResult{}, ErrForbidden
	}
	var enabled bool
	s.DB.QueryRow(ctx, `SELECT enabled FROM user_mfa WHERE user_id=$1`, u.ID).Scan(&enabled)
	keyOpts, keyChallenge := s.webauthnLoginOptions(ctx, u.ID)
	if enabled || keyOpts != nil {
		pt := newToken()
		if _, err := s.DB.Exec(ctx, `INSERT INTO mfa_pending(token_hash,user_id,expires_at,ttl_secs,user_agent,ip,wa_challenge) VALUES($1,$2,$3,$4,$5,$6,$7)`, hashToken(pt), u.ID, s.Now().Add(mfaPendingTTL), int(ttl.Seconds()), meta.UserAgent, meta.IP, keyChallenge); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{User: u}, &MFARequired{Token: pt, TOTP: enabled, Key: keyOpts}
	}
	s.recordAttempt(ctx, email, meta, true)
	tok, err := s.newSession(ctx, u.ID, ttl, meta)
	return LoginResult{Token: tok, User: u}, err
}

// Login is LoginWith without client details; an account with two-factor authentication gets an *MFARequired.
func (s *Service) Login(ctx context.Context, email, password string, ttl time.Duration) (string, User, error) {
	r, err := s.LoginWith(ctx, email, password, ttl, ClientMeta{})
	return r.Token, r.User, err
}

// ----- TOTP

func totpAt(secret []byte, step int64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(b[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0xf
	code := (uint32(sum[off]&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])) % 1000000
	return fmt.Sprintf("%06d", code)
}

// verifyTOTP accepts the code for the current time step and the one on each side (clock drift), but never a step at or
// before the last one accepted, so a code is good once.
func verifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	cur := now.Unix() / 30
	for d := int64(-1); d <= 1; d++ {
		step := cur + d
		if step > lastStep && subtle.ConstantTimeCompare([]byte(totpAt(secret, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

type MFASetup struct {
	Secret string `json:"secret"`  // base32, to type into an authenticator
	URL    string `json:"otpauth"` // the same as a QR-code address
}

// SetupMFA starts enrolment: a new secret that does nothing until a code from it is confirmed.
func (s *Service) SetupMFA(ctx context.Context, a Actor) (MFASetup, error) {
	var email string
	var enabled bool
	if err := s.DB.QueryRow(ctx, `SELECT email FROM app_user WHERE id=$1`, a.ID).Scan(&email); err != nil {
		return MFASetup{}, err
	}
	s.DB.QueryRow(ctx, `SELECT enabled FROM user_mfa WHERE user_id=$1`, a.ID).Scan(&enabled)
	if enabled {
		return MFASetup{}, invalid("two-factor authentication is already on; turn it off first to enrol a new device")
	}
	secret := make([]byte, 20)
	rand.Read(secret)
	enc, err := s.seal(ctx, hex.EncodeToString(secret))
	if err != nil {
		return MFASetup{}, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO user_mfa(user_id,secret_enc,enabled) VALUES($1,$2,false) ON CONFLICT (user_id) DO UPDATE SET secret_enc=EXCLUDED.secret_enc,last_step=0,recovery_hashes='{}'`, a.ID, enc); err != nil {
		return MFASetup{}, err
	}
	b32 := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	q := url.Values{"secret": {b32}, "issuer": {"Tasklane"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	return MFASetup{Secret: b32, URL: "otpauth://totp/" + url.PathEscape("Tasklane:"+email) + "?" + q.Encode()}, nil
}

func (s *Service) mfaSecret(ctx context.Context, userID string) ([]byte, int64, bool, error) {
	var enc []byte
	var last int64
	var enabled bool
	if err := s.DB.QueryRow(ctx, `SELECT secret_enc,last_step,enabled FROM user_mfa WHERE user_id=$1`, userID).Scan(&enc, &last, &enabled); err != nil {
		return nil, 0, false, err
	}
	hx, err := s.unseal(ctx, enc)
	if err != nil {
		return nil, 0, false, err
	}
	b, err := hex.DecodeString(hx)
	return b, last, enabled, err
}

func newRecoveryCodes() (plain, hashed []string) {
	for i := 0; i < 10; i++ {
		b := make([]byte, 5)
		rand.Read(b)
		c := hex.EncodeToString(b)
		c = c[:5] + "-" + c[5:]
		plain = append(plain, c)
		h := sha256.Sum256([]byte(c))
		hashed = append(hashed, hex.EncodeToString(h[:]))
	}
	return
}

// EnableMFA confirms enrolment with a code and returns the recovery codes, shown once. Other sessions end: they signed in
// without the second factor.
func (s *Service) EnableMFA(ctx context.Context, a Actor, code, currentToken string) ([]string, error) {
	secret, last, enabled, err := s.mfaSecret(ctx, a.ID)
	if err == pgx.ErrNoRows || err == nil && enabled {
		return nil, invalid("start enrolment first, and only once")
	}
	if err != nil {
		return nil, err
	}
	step, ok := verifyTOTP(secret, code, s.Now(), last)
	if !ok {
		return nil, invalid("that code is not right; check the time on your device")
	}
	plain, hashed := newRecoveryCodes()
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE user_mfa SET enabled=true,last_step=$2,recovery_hashes=$3 WHERE user_id=$1`, a.ID, step, hashed); err != nil {
			return err
		}
		// a second factor changes who can sign in: other devices sign in again
		if _, err := tx.Exec(ctx, `DELETE FROM session WHERE user_id=$1 AND token_hash <> $2`, a.ID, hashToken(currentToken)); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "security.mfa_enabled", a.ID, map[string]any{})
	})
	return plain, err
}

// DisableMFA turns the second factor off; the person proves who they are with their password.
func (s *Service) DisableMFA(ctx context.Context, a Actor, password string) error {
	var hash *string
	if err := s.DB.QueryRow(ctx, `SELECT password_hash FROM app_user WHERE id=$1`, a.ID).Scan(&hash); err != nil || hash == nil || bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)) != nil {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM user_mfa WHERE user_id=$1`, a.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "security.mfa_disabled", a.ID, map[string]any{})
	})
}

// ResetMFA lets an administrator switch a person's second factor off (a lost phone and no recovery codes) and ends their sessions.
func (s *Service) ResetMFA(ctx context.Context, a Actor, userID string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM user_mfa WHERE user_id::text=$1`, userID)
		if err != nil {
			return err
		}
		keys, err := tx.Exec(ctx, `DELETE FROM webauthn_credential WHERE user_id::text=$1`, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected()+keys.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM session WHERE user_id::text=$1`, userID); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "security.mfa_reset", userID, map[string]any{})
	})
}

func (s *Service) MFAStatus(ctx context.Context, a Actor) (enabled bool, recoveryLeft int) {
	var hs []string
	s.DB.QueryRow(ctx, `SELECT enabled,recovery_hashes FROM user_mfa WHERE user_id=$1`, a.ID).Scan(&enabled, &hs)
	return enabled, len(hs)
}

// LoginMFA finishes a sign-in: the token from the password step and a code from the authenticator or a recovery code.
func (s *Service) LoginMFA(ctx context.Context, pendingToken, code string, meta ClientMeta) (LoginResult, error) {
	var userID string
	var attempts, ttlSecs int
	var exp time.Time
	err := s.DB.QueryRow(ctx, `SELECT user_id::text,attempts,ttl_secs,expires_at FROM mfa_pending WHERE token_hash=$1`, hashToken(pendingToken)).Scan(&userID, &attempts, &ttlSecs, &exp)
	if err != nil || !exp.After(s.Now()) {
		s.DB.Exec(ctx, `DELETE FROM mfa_pending WHERE token_hash=$1`, hashToken(pendingToken))
		return LoginResult{}, ErrForbidden
	}
	fail := func() (LoginResult, error) {
		if attempts+1 >= maxMFAAttempts {
			s.DB.Exec(ctx, `DELETE FROM mfa_pending WHERE token_hash=$1`, hashToken(pendingToken)) // start again from the password
		} else {
			s.DB.Exec(ctx, `UPDATE mfa_pending SET attempts=attempts+1 WHERE token_hash=$1`, hashToken(pendingToken))
		}
		return LoginResult{}, ErrForbidden
	}
	secret, last, enabled, err := s.mfaSecret(ctx, userID)
	if err != nil || !enabled {
		return fail()
	}
	var u User
	var active bool
	if err := s.DB.QueryRow(ctx, `SELECT id,email,name,kind,is_admin,active FROM app_user WHERE id::text=$1`, userID).Scan(&u.ID, &u.Email, &u.Name, &u.Kind, &u.Admin, &active); err != nil || !active {
		return fail()
	}
	if step, ok := verifyTOTP(secret, code, s.Now(), last); ok {
		// the step is claimed atomically: two requests with the same code cannot both sign in
		tag, err := s.DB.Exec(ctx, `UPDATE user_mfa SET last_step=$2 WHERE user_id=$1 AND last_step<$2`, userID, step)
		if err != nil || tag.RowsAffected() == 0 {
			return fail()
		}
	} else {
		h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(code))))
		tag, err := s.DB.Exec(ctx, `UPDATE user_mfa SET recovery_hashes=array_remove(recovery_hashes,$2) WHERE user_id=$1 AND $2 = ANY(recovery_hashes)`, userID, hex.EncodeToString(h[:]))
		if err != nil || tag.RowsAffected() == 0 {
			return fail()
		}
		s.auditAbout(ctx, userID, u.Name, "security.recovery_code_used")
	}
	s.DB.Exec(ctx, `DELETE FROM mfa_pending WHERE token_hash=$1`, hashToken(pendingToken))
	s.recordAttempt(ctx, u.Email, meta, true)
	tok, err := s.newSession(ctx, u.ID, time.Duration(ttlSecs)*time.Second, meta)
	return LoginResult{Token: tok, User: u}, err
}

// ----- sessions

type SessionInfo struct {
	ID        int64      `json:"id"`
	Created   time.Time  `json:"created"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Expires   time.Time  `json:"expires"`
	IP        string     `json:"ip"`
	UserAgent string     `json:"user_agent"`
	Current   bool       `json:"current"`
}

func (s *Service) ListSessions(ctx context.Context, a Actor, currentToken string) ([]SessionInfo, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,created_at,last_seen_at,expires_at,ip,user_agent,token_hash=$3 FROM session WHERE user_id=$1 AND expires_at>$2 ORDER BY id DESC`, a.ID, s.Now(), hashToken(currentToken))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionInfo{}
	for rows.Next() {
		var x SessionInfo
		if err := rows.Scan(&x.ID, &x.Created, &x.LastSeen, &x.Expires, &x.IP, &x.UserAgent, &x.Current); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// RevokeSession ends one of the person's own sessions at once.
func (s *Service) RevokeSession(ctx context.Context, a Actor, id int64) error {
	tag, err := s.DB.Exec(ctx, `DELETE FROM session WHERE id=$1 AND user_id=$2`, id, a.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeOtherSessions signs the person out everywhere except where they are.
func (s *Service) RevokeOtherSessions(ctx context.Context, a Actor, currentToken string) (int, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM session WHERE user_id=$1 AND token_hash <> $2`, a.ID, hashToken(currentToken))
	return int(tag.RowsAffected()), err
}

// RevokeUserAccess ends every session of a person and, when asked, their API tokens (offboarding). Administrators only.
func (s *Service) RevokeUserAccess(ctx context.Context, a Actor, userID string, tokens bool) (int, error) {
	if err := adminOnly(a); err != nil {
		return 0, err
	}
	n := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM app_user WHERE id::text=$1)`, userID).Scan(&exists); err != nil || !exists {
			return ErrNotFound
		}
		tag, err := tx.Exec(ctx, `DELETE FROM session WHERE user_id::text=$1`, userID)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		if tokens {
			tag, err := tx.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE user_id::text=$1 AND revoked_at IS NULL`, userID, s.Now())
			if err != nil {
				return err
			}
			n += int(tag.RowsAffected())
			if _, err := tx.Exec(ctx, `UPDATE oauth_grant SET revoked_at=$2 WHERE user_id::text=$1 AND revoked_at IS NULL`, userID, s.Now()); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, a, "security.access_revoked", userID, map[string]any{"tokens": tokens, "ended": n})
	})
	return n, err
}

// ----- API tokens

type TokenInfo struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Scopes   []string   `json:"scopes,omitempty"`
	Created  time.Time  `json:"created"`
	Expires  *time.Time `json:"expires,omitempty"`
	Revoked  *time.Time `json:"revoked,omitempty"`
	LastUsed *time.Time `json:"last_used,omitempty"`
}

func (s *Service) ListTokens(ctx context.Context, a Actor) ([]TokenInfo, error) {
	rows, err := s.DB.Query(ctx, `SELECT id::text,name,scopes,created_at,expires_at,revoked_at,last_used_at FROM api_token WHERE user_id=$1 ORDER BY created_at DESC`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		if err := rows.Scan(&t.ID, &t.Name, &t.Scopes, &t.Created, &t.Expires, &t.Revoked, &t.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CreateOwnToken makes a REST token for the person. It can expire; the secret is shown once and only its hash is kept.
func (s *Service) CreateOwnToken(ctx context.Context, a Actor, name string, ttl time.Duration) (string, TokenInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", TokenInfo{}, invalid("a token needs a name of up to 100 characters")
	}
	if ttl < 0 || ttl > 5*365*24*time.Hour {
		return "", TokenInfo{}, invalid("a token lasts up to five years; leave the lifetime out for one that does not expire")
	}
	tok := "tl_" + newToken()
	var exp any
	if ttl > 0 {
		exp = s.Now().Add(ttl)
	}
	var info TokenInfo
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO api_token(token_hash,user_id,name,expires_at) VALUES($1,$2,$3,$4) RETURNING id::text,name,created_at,expires_at`, hashToken(tok), a.ID, name, exp).Scan(&info.ID, &info.Name, &info.Created, &info.Expires); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "security.token_created", info.ID, map[string]any{"name": name, "expires": exp != nil})
	})
	return tok, info, err
}

func (s *Service) RevokeOwnToken(ctx context.Context, a Actor, id string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE api_token SET revoked_at=$3 WHERE id::text=$1 AND user_id=$2 AND revoked_at IS NULL`, id, a.ID, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "security.token_revoked", id, map[string]any{})
	})
}

// ----- passwords

// ChangePassword sets a new password for someone who knows the current one, and signs out every other session.
func (s *Service) ChangePassword(ctx context.Context, a Actor, current, next, currentToken string) error {
	var hash *string
	var email string
	if err := s.DB.QueryRow(ctx, `SELECT password_hash,email FROM app_user WHERE id=$1`, a.ID).Scan(&hash, &email); err != nil || hash == nil {
		return ErrForbidden
	}
	if bcrypt.CompareHashAndPassword([]byte(*hash), []byte(current)) != nil {
		return ErrForbidden
	}
	if err := s.checkPassword(email, next); err != nil {
		return err
	}
	if current == next {
		return invalid("the new password must differ from the current one")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE app_user SET password_hash=$2,password_changed_at=$3 WHERE id=$1`, a.ID, string(h), s.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM session WHERE user_id=$1 AND token_hash <> $2`, a.ID, hashToken(currentToken)); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "security.password_changed", a.ID, map[string]any{})
	})
}

var _ = errors.New

// auditAbout records a security event about a person that no administrator caused (a recovery code used, a credential that may
// have been cloned). The parameters are separate because PostgreSQL cannot give one placeholder a uuid column and a text one.
func (s *Service) auditAbout(ctx context.Context, userID, label, action string) {
	if _, err := s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,$3,$4,'{}')`, userID, label, action, userID); err != nil {
		log.Printf("audit %s: %v", action, err)
	}
}
