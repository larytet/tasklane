package domain_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// totp is written out here on purpose: the test must not share code with the server (RFC 6238, SHA-1, 30 s, 6 digits).
func totp(t *testing.T, b32 string, at time.Time) string {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(b32)
	if err != nil {
		t.Fatal(err)
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(at.Unix()/30))
	m := hmac.New(sha1.New, secret)
	m.Write(b[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0xf
	code := (uint32(sum[off]&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])) % 1000000
	return fmt.Sprintf("%06d", code)
}

func TestSEC04_TokensExpireAndRevokeAtOnceAndScopedTokensAreNotRESTCredentials(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	tok, info, err := e.Svc.CreateOwnToken(ctx, e.Alice, "ci", 24*time.Hour)
	if err != nil || info.Expires == nil || !strings.HasPrefix(tok, "tl_") {
		t.Fatalf("%v %+v", err, info)
	}
	if a, err := e.Svc.Authenticate(ctx, tok); err != nil || a.ID != e.Alice.ID {
		t.Fatalf("%v", err)
	}
	// the secret is shown once: the list never contains it
	ts, _ := e.Svc.ListTokens(ctx, e.Alice)
	if len(ts) != 1 || ts[0].LastUsed == nil || strings.Contains(fmt.Sprint(ts), tok) {
		t.Fatalf("%+v", ts)
	}
	e.Clock.T = e.Clock.T.Add(25 * time.Hour)
	if _, err := e.Svc.Authenticate(ctx, tok); err == nil {
		t.Fatal("an expired token still works")
	}
	forever, finfo, _ := e.Svc.CreateOwnToken(ctx, e.Alice, "forever", 0)
	if _, err := e.Svc.Authenticate(ctx, forever); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.RevokeOwnToken(ctx, e.Alice, finfo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, forever); err == nil {
		t.Fatal("a revoked token still works")
	}
	if err := e.Svc.RevokeOwnToken(ctx, e.Bob, finfo.ID); err != domain.ErrNotFound {
		t.Fatalf("someone else's token: %v", err)
	}
	// a scoped agent token works for the MCP server's own check, not as a general REST credential
	scoped, err := e.Svc.CreateScopedToken(ctx, e.Admin, e.Alice.ID, "agent", []string{"read"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.AuthenticateAgent(ctx, scoped); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, scoped); err == nil {
		t.Fatal("a read-only agent token was accepted by the REST API with the user's full rights")
	}
	// bad lifetimes and names
	if _, _, err := e.Svc.CreateOwnToken(ctx, e.Alice, " ", time.Hour); err == nil {
		t.Fatal("nameless token")
	}
	if _, _, err := e.Svc.CreateOwnToken(ctx, e.Alice, "x", 10*365*24*time.Hour); err == nil {
		t.Fatal("ten year token")
	}
}

func TestSEC04_SessionsAreListedAndRevokedAtOnce(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	meta := domain.ClientMeta{IP: "203.0.113.5", UserAgent: "Firefox"}
	a, _ := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	b, _ := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, domain.ClientMeta{IP: "198.51.100.7", UserAgent: "Chrome"})
	ss, err := e.Svc.ListSessions(ctx, e.Alice, a.Token)
	if err != nil || len(ss) != 2 {
		t.Fatalf("%v %d", err, len(ss))
	}
	var cur, other domain.SessionInfo
	for _, s := range ss {
		if s.Current {
			cur = s
		} else {
			other = s
		}
	}
	if cur.IP != "203.0.113.5" || cur.UserAgent != "Firefox" || other.UserAgent != "Chrome" {
		t.Fatalf("%+v %+v", cur, other)
	}
	if _, err := e.Svc.Authenticate(ctx, b.Token); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.RevokeSession(ctx, e.Alice, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, b.Token); err == nil {
		t.Fatal("a revoked session still works")
	}
	if err := e.Svc.RevokeSession(ctx, e.Bob, cur.ID); err != domain.ErrNotFound {
		t.Fatalf("someone else's session: %v", err)
	}
	c, _ := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	if n, err := e.Svc.RevokeOtherSessions(ctx, e.Alice, a.Token); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := e.Svc.Authenticate(ctx, c.Token); err == nil {
		t.Fatal("another session survived")
	}
	if _, err := e.Svc.Authenticate(ctx, a.Token); err != nil {
		t.Fatal("the current session was ended")
	}
	// offboarding: an administrator ends every session and token, a deactivated account stops at once
	tok, _, _ := e.Svc.CreateOwnToken(ctx, e.Alice, "t", 0)
	n, err := e.Svc.RevokeUserAccess(ctx, e.Admin, e.Alice.ID, true)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := e.Svc.Authenticate(ctx, a.Token); err == nil {
		t.Fatal("a session survived offboarding")
	}
	if _, err := e.Svc.Authenticate(ctx, tok); err == nil {
		t.Fatal("a token survived offboarding")
	}
	if _, err := e.Svc.RevokeUserAccess(ctx, e.Alice, e.Bob.ID, true); err == nil {
		t.Fatal("a member revoked someone's access")
	}
	d, _ := e.Svc.LoginWith(ctx, "bob@example.com", "password123", time.Hour, meta)
	e.Store.Pool.Exec(ctx, `UPDATE app_user SET active=false WHERE email='bob@example.com'`)
	if _, err := e.Svc.Authenticate(ctx, d.Token); err == nil {
		t.Fatal("a deactivated account's session still works")
	}
}

func TestSEC04_TwoFactorEnrolmentSignInReplayAndRecovery(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Date(2026, 10, 19, 12, 0, 10, 0, time.UTC)
	meta := domain.ClientMeta{IP: "203.0.113.5"}
	first, _ := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta) // a session from before the second factor
	setup, err := e.Svc.SetupMFA(ctx, e.Alice)
	if err != nil || !strings.HasPrefix(setup.URL, "otpauth://totp/") || !strings.Contains(setup.URL, "secret="+setup.Secret) {
		t.Fatalf("%+v %v", setup, err)
	}
	if svg, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(setup.QR, "data:image/svg+xml;base64,")); err != nil || !strings.HasPrefix(setup.QR, "data:image/svg+xml;base64,") || !strings.HasPrefix(string(svg), "<svg ") {
		t.Fatalf("no QR code for the address: %.60q %v", setup.QR, err)
	}
	// until a code is confirmed nothing changes
	if _, err := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta); err != nil {
		t.Fatalf("enrolment alone must not change sign-in: %v", err)
	}
	if _, err := e.Svc.EnableMFA(ctx, e.Alice, "000000", first.Token); err == nil {
		t.Fatal("a wrong code enabled it")
	}
	codes, err := e.Svc.EnableMFA(ctx, e.Alice, totp(t, setup.Secret, e.Clock.T), first.Token)
	if err != nil || len(codes) != 10 {
		t.Fatalf("%v %d", err, len(codes))
	}
	if _, err := e.Svc.Authenticate(ctx, first.Token); err != nil {
		t.Fatal("the session that enrolled was ended")
	}
	if on, left := e.Svc.MFAStatus(ctx, e.Alice); !on || left != 10 {
		t.Fatalf("%v %d", on, left)
	}
	if _, err := e.Svc.SetupMFA(ctx, e.Alice); err == nil {
		t.Fatal("enrolling again while on")
	}
	// the password alone no longer signs in
	_, err = e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	var need *domain.MFARequired
	if !asMFA(err, &need) {
		t.Fatalf("%v", err)
	}
	// the code used to enable cannot be used again, and a wrong code is refused
	if _, err := e.Svc.LoginMFA(ctx, need.Token, totp(t, setup.Secret, e.Clock.T), meta); err == nil {
		t.Fatal("a code was accepted twice")
	}
	e.Clock.T = e.Clock.T.Add(31 * time.Second)
	if _, err := e.Svc.LoginMFA(ctx, need.Token, "123456", meta); err == nil {
		t.Fatal("a wrong code was accepted")
	}
	res, err := e.Svc.LoginMFA(ctx, need.Token, totp(t, setup.Secret, e.Clock.T), meta)
	if err != nil || res.Token == "" {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Authenticate(ctx, res.Token); err != nil {
		t.Fatal(err)
	}
	// that step is spent: the same code cannot sign in again, even with a fresh password step
	_, err = e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	asMFA(err, &need)
	if _, err := e.Svc.LoginMFA(ctx, need.Token, totp(t, setup.Secret, e.Clock.T), meta); err == nil {
		t.Fatal("a spent code signed in again")
	}
	// clock drift: one step either side is tolerated, two steps is not
	e.Clock.T = e.Clock.T.Add(61 * time.Second)
	_, err = e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	asMFA(err, &need)
	if _, err := e.Svc.LoginMFA(ctx, need.Token, totp(t, setup.Secret, e.Clock.T.Add(-90*time.Second)), meta); err == nil {
		t.Fatal("a code from three steps ago")
	}
	if _, err := e.Svc.LoginMFA(ctx, need.Token, totp(t, setup.Secret, e.Clock.T.Add(-30*time.Second)), meta); err != nil {
		t.Fatalf("one step of drift: %v", err)
	}
	// five wrong codes kill the pending sign-in: the password has to be entered again
	_, err = e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	asMFA(err, &need)
	for i := 0; i < 5; i++ {
		e.Svc.LoginMFA(ctx, need.Token, "000000", meta)
	}
	e.Clock.T = e.Clock.T.Add(31 * time.Second)
	if _, err := e.Svc.LoginMFA(ctx, need.Token, totp(t, setup.Secret, e.Clock.T), meta); err == nil {
		t.Fatal("a pending sign-in survived five wrong codes")
	}
	// recovery codes work once each
	_, err = e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	asMFA(err, &need)
	if _, err := e.Svc.LoginMFA(ctx, need.Token, codes[0], meta); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	_, err = e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	asMFA(err, &need)
	if _, err := e.Svc.LoginMFA(ctx, need.Token, codes[0], meta); err == nil {
		t.Fatal("a recovery code worked twice")
	}
	if _, left := e.Svc.MFAStatus(ctx, e.Alice); left != 9 {
		t.Fatalf("%d left", left)
	}
	// using a recovery code is recorded (it was not, for a while: the audit statement was invalid and its error ignored)
	var used int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.recovery_code_used' AND target=$1`, e.Alice.ID).Scan(&used)
	if used != 1 {
		t.Fatalf("%d audit records of a recovery code being used", used)
	}
	// the pending token expires
	_, err = e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	asMFA(err, &need)
	e.Clock.T = e.Clock.T.Add(6 * time.Minute)
	if _, err := e.Svc.LoginMFA(ctx, need.Token, codes[1], meta); err == nil {
		t.Fatal("an expired pending sign-in")
	}
	// turning it off needs the password; an administrator can reset it and that ends the person's sessions
	if err := e.Svc.DisableMFA(ctx, e.Alice, "wrong"); err == nil {
		t.Fatal("disabled without the password")
	}
	if err := e.Svc.ResetMFA(ctx, e.Alice, e.Alice.ID); err == nil {
		t.Fatal("a member reset a second factor")
	}
	if err := e.Svc.ResetMFA(ctx, e.Admin, e.Alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, res.Token); err == nil {
		t.Fatal("a session survived the reset")
	}
	if _, err := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta); err != nil {
		t.Fatalf("after the reset: %v", err)
	}
	// the secret is not stored in the clear
	var stored []byte
	e.Store.Pool.QueryRow(ctx, `SELECT secret_enc FROM user_mfa LIMIT 1`).Scan(&stored)
	if len(stored) != 0 {
		t.Fatalf("a row is left after the reset")
	}
	s2, _ := e.Svc.SetupMFA(ctx, e.Bob)
	e.Store.Pool.QueryRow(ctx, `SELECT secret_enc FROM user_mfa WHERE user_id::text=$1`, e.Bob.ID).Scan(&stored)
	if strings.Contains(string(stored), s2.Secret) {
		t.Fatal("the secret is stored in the clear")
	}
}

func asMFA(err error, target **domain.MFARequired) bool {
	m, ok := err.(*domain.MFARequired)
	if ok {
		*target = m
	}
	return ok
}

func TestSEC04_LockoutAfterRepeatedFailuresWithoutRevealingAccounts(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	meta := domain.ClientMeta{IP: "203.0.113.9"}
	for i := 0; i < 5; i++ {
		if _, err := e.Svc.LoginWith(ctx, "alice@example.com", "wrong-password", time.Hour, meta); err != domain.ErrForbidden {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// now even the right password is refused, with a time to retry
	_, err := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	locked, ok := err.(*domain.LockedError)
	if !ok || !locked.Until.After(e.Clock.T) || locked.Until.After(e.Clock.T.Add(16*time.Minute)) {
		t.Fatalf("%v", err)
	}
	// an unknown address locks in exactly the same way, so locking tells an attacker nothing
	for i := 0; i < 5; i++ {
		e.Svc.LoginWith(ctx, "nobody@example.com", "guess", time.Hour, meta)
	}
	if _, err := e.Svc.LoginWith(ctx, "nobody@example.com", "guess", time.Hour, meta); err == nil || err == domain.ErrForbidden {
		t.Fatalf("an unknown account did not lock: %v", err)
	}
	// others are not affected, and the lock lifts
	if _, err := e.Svc.LoginWith(ctx, "bob@example.com", "password123", time.Hour, meta); err != nil {
		t.Fatalf("another account was locked: %v", err)
	}
	e.Clock.T = e.Clock.T.Add(16 * time.Minute)
	if _, err := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta); err != nil {
		t.Fatalf("the lock did not lift: %v", err)
	}
	// a success clears the count: four failures, a success, four more failures do not lock
	for i := 0; i < 4; i++ {
		e.Svc.LoginWith(ctx, "alice@example.com", "wrong", time.Hour, meta)
	}
	e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	for i := 0; i < 4; i++ {
		e.Svc.LoginWith(ctx, "alice@example.com", "wrong", time.Hour, meta)
	}
	if _, err := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta); err != nil {
		t.Fatalf("failures before a success counted: %v", err)
	}
}

func TestSEC04_PasswordChangeAndPolicy(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	meta := domain.ClientMeta{}
	a, _ := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	b, _ := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta)
	if err := e.Svc.ChangePassword(ctx, e.Alice, "wrong", "a-new-password", a.Token); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	for _, bad := range []string{"short", "alice@example.com", "password123"} {
		if err := e.Svc.ChangePassword(ctx, e.Alice, "password123", bad, a.Token); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if err := e.Svc.ChangePassword(ctx, e.Alice, "password123", "a-new-password", a.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, b.Token); err == nil {
		t.Fatal("another session survived a password change")
	}
	if _, err := e.Svc.Authenticate(ctx, a.Token); err != nil {
		t.Fatal("the session that changed the password was ended")
	}
	if _, err := e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, meta); err == nil {
		t.Fatal("the old password still works")
	}
	if _, err := e.Svc.LoginWith(ctx, "alice@example.com", "a-new-password", time.Hour, meta); err != nil {
		t.Fatal(err)
	}
	// the policy is configurable
	e.Svc.PasswordMinLength = 14
	if _, err := e.Svc.CreateUser(ctx, "dave@example.com", "Dave", "twelve-chars", false); err == nil {
		t.Fatal("a 12 character password passed a 14 character policy")
	}
	if _, err := e.Svc.CreateUser(ctx, "dave@example.com", "Dave", "fourteen-chars!", false); err != nil {
		t.Fatal(err)
	}
}
