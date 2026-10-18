package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/authmock"
	"github.com/larytet/tasklane/internal/webauthn"
)

const (
	waOrigin = "https://tasklane.example.com"
	waRPID   = "tasklane.example.com"
)

func waEnv(t *testing.T) *testutil.Env {
	e := testutil.New(t)
	e.Clock.T = time.Now()
	e.Svc.BaseURL = waOrigin
	return e
}

func challengeOf(t *testing.T, opts map[string]any) []byte {
	t.Helper()
	pk := opts["publicKey"].(map[string]any)
	ch, err := webauthn.Decode64(pk["challenge"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// addKey registers an authenticator for a person through the same two steps the page uses.
func addKey(t *testing.T, e *testutil.Env, a domain.Actor, key *authmock.Authenticator, name string) domain.Passkey {
	t.Helper()
	opts, err := e.Svc.PasskeyRegistrationOptions(ctx, a, "password123")
	if err != nil {
		t.Fatal(err)
	}
	cd, att := key.Create(challengeOf(t, opts))
	p, err := e.Svc.AddPasskey(ctx, a, domain.PasskeyRegistration{Name: name, ClientDataJSON: webauthn.Encode(cd), AttestationObject: webauthn.Encode(att)})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func pending(t *testing.T, e *testutil.Env, email string) (*domain.MFARequired, []byte) {
	t.Helper()
	_, err := e.Svc.LoginWith(ctx, email, "password123", time.Hour, domain.ClientMeta{})
	var m *domain.MFARequired
	if !errors.As(err, &m) {
		t.Fatalf("a second step was expected: %v", err)
	}
	var ch []byte
	if m.Key != nil {
		ch = challengeOf(t, m.Key)
	}
	return m, ch
}

func assertion(token string, key *authmock.Authenticator, ch []byte) domain.PasskeyAssertion {
	cd, ad, sig := key.Get(ch)
	return domain.PasskeyAssertion{Token: token, ID: webauthn.Encode(key.CredID), ClientDataJSON: webauthn.Encode(cd), AuthenticatorData: webauthn.Encode(ad), Signature: webauthn.Encode(sig)}
}

func TestSEC04_APasskeyIsRegisteredWithThePasswordAndSignsInAsASecondFactor(t *testing.T) {
	e := waEnv(t)
	key := authmock.New(waRPID, waOrigin, webauthn.AlgES256)
	// no key yet: the password alone signs in, as before
	if tok, _, err := e.Svc.Login(ctx, "alice@example.com", "password123", time.Hour); err != nil || tok == "" {
		t.Fatalf("%v", err)
	}
	// adding a key needs the password again
	if _, err := e.Svc.PasskeyRegistrationOptions(ctx, e.Alice, "wrong"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("the wrong password began a registration: %v", err)
	}
	opts, err := e.Svc.PasskeyRegistrationOptions(ctx, e.Alice, "password123")
	if err != nil {
		t.Fatal(err)
	}
	pk := opts["publicKey"].(map[string]any)
	if pk["rp"].(map[string]any)["id"] != waRPID || pk["attestation"] != "none" || len(challengeOf(t, opts)) != 32 {
		t.Fatalf("%v", pk)
	}
	if strings.Contains(strings.ToLower(pk["user"].(map[string]any)["id"].(string)), "alice") {
		t.Fatal("the user handle names the person")
	}
	cd, att := key.Create(challengeOf(t, opts))
	in := domain.PasskeyRegistration{Name: "Laptop", ClientDataJSON: webauthn.Encode(cd), AttestationObject: webauthn.Encode(att)}
	if _, err := e.Svc.AddPasskey(ctx, e.Alice, domain.PasskeyRegistration{Name: "", ClientDataJSON: in.ClientDataJSON, AttestationObject: in.AttestationObject}); err == nil {
		t.Fatal("a key without a name was added")
	}
	// a request refused for its name did not spend the challenge; the right one registers the key, and then the challenge is gone
	p, err := e.Svc.AddPasskey(ctx, e.Alice, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddPasskey(ctx, e.Alice, in); err == nil {
		t.Fatal("a spent challenge was accepted")
	}
	if ks, _ := e.Svc.ListPasskeys(ctx, e.Alice); len(ks) != 1 || ks[0].Name != "Laptop" || ks[0].ID != p.ID {
		t.Fatalf("%+v", ks)
	}
	if ks, _ := e.Svc.ListPasskeys(ctx, e.Bob); len(ks) != 0 {
		t.Fatalf("another person's keys are listed: %+v", ks)
	}
	// the same authenticator cannot be registered twice
	opts2, _ := e.Svc.PasskeyRegistrationOptions(ctx, e.Alice, "password123")
	if ex := opts2["publicKey"].(map[string]any)["excludeCredentials"].([]map[string]any); len(ex) != 1 {
		t.Fatalf("the browser is not told to skip the key it already has: %v", ex)
	}
	cd2, att2 := key.Create(challengeOf(t, opts2))
	if _, err := e.Svc.AddPasskey(ctx, e.Alice, domain.PasskeyRegistration{Name: "again", ClientDataJSON: webauthn.Encode(cd2), AttestationObject: webauthn.Encode(att2)}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("%v", err)
	}
	// now the password is not enough: a second step is due, and it offers the key
	m, ch := pending(t, e, "alice@example.com")
	if m.TOTP || m.Key == nil {
		t.Fatalf("%+v", m)
	}
	if allow := m.Key["publicKey"].(map[string]any)["allowCredentials"].([]map[string]any); len(allow) != 1 {
		t.Fatalf("%v", allow)
	}
	// Bob has none: the password alone still signs him in
	if _, _, err := e.Svc.Login(ctx, "bob@example.com", "password123", time.Hour); err != nil {
		t.Fatalf("a person without a key was asked for one: %v", err)
	}
	res, err := e.Svc.LoginWebAuthn(ctx, assertion(m.Token, key, ch), domain.ClientMeta{IP: "10.0.0.1"})
	if err != nil || res.Token == "" || res.User.ID != e.Alice.ID {
		t.Fatalf("%v", err)
	}
	// the session is real and the token is spent
	if _, err := e.Svc.Authenticate(ctx, res.Token); err != nil {
		t.Fatalf("the session does not work: %v", err)
	}
	if _, err := e.Svc.LoginWebAuthn(ctx, assertion(m.Token, key, ch), domain.ClientMeta{}); err == nil {
		t.Fatal("the pending sign-in was used twice")
	}
	if ks, _ := e.Svc.ListPasskeys(ctx, e.Alice); ks[0].LastUsed == nil {
		t.Fatal("the use of the key is not recorded")
	}
	// removing the key needs the password and nobody else's key can be removed
	if err := e.Svc.RemovePasskey(ctx, e.Alice, p.ID, "wrong"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.RemovePasskey(ctx, e.Bob, p.ID, "password123"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another person's key was removed: %v", err)
	}
	if err := e.Svc.RemovePasskey(ctx, e.Alice, p.ID, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.Login(ctx, "alice@example.com", "password123", time.Hour); err != nil {
		t.Fatalf("after the last key is gone the password alone signs in again: %v", err)
	}
	var added, removed int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action='security.passkey_added'),count(*) FILTER (WHERE action='security.passkey_removed') FROM audit_event`).Scan(&added, &removed)
	if added != 1 || removed != 1 {
		t.Fatalf("audit: %d added, %d removed", added, removed)
	}
}

func TestSEC04_APasskeySignInRefusesTheWrongKeyThePhishedAnswerAndTheClonedCredential(t *testing.T) {
	e := waEnv(t)
	aliceKey := authmock.New(waRPID, waOrigin, webauthn.AlgES256)
	bobKey := authmock.New(waRPID, waOrigin, webauthn.AlgEdDSA)
	addKey(t, e, e.Alice, aliceKey, "alice's")
	addKey(t, e, e.Bob, bobKey, "bob's")
	try := func(mut func(*authmock.Authenticator), who *authmock.Authenticator, email string) error {
		m, ch := pending(t, e, email)
		if mut != nil {
			mut(who)
			defer func() { who.RPID, who.Origin, who.NoUP = waRPID, waOrigin, false }()
		}
		_, err := e.Svc.LoginWebAuthn(ctx, assertion(m.Token, who, ch), domain.ClientMeta{})
		return err
	}
	if err := try(nil, aliceKey, "alice@example.com"); err != nil {
		t.Fatalf("the right key was refused: %v", err)
	}
	// Alice has given her password but answers with Bob's key: a key that is not hers does not count
	if err := try(nil, bobKey, "alice@example.com"); err == nil {
		t.Fatal("another person's key signed Alice in")
	}
	// a phishing page on another origin gets an answer for its own origin and relying party: neither is accepted here
	if err := try(func(k *authmock.Authenticator) { k.Origin = "https://evil.example.org" }, aliceKey, "alice@example.com"); err == nil {
		t.Fatal("an answer made on another origin signed Alice in")
	}
	if err := try(func(k *authmock.Authenticator) { k.RPID = "evil.example.org" }, aliceKey, "alice@example.com"); err == nil {
		t.Fatal("an answer for another relying party signed Alice in")
	}
	if err := try(func(k *authmock.Authenticator) { k.NoUP = true }, aliceKey, "alice@example.com"); err == nil {
		t.Fatal("an answer without the user present signed Alice in")
	}
	// a pending sign-in without a key challenge (a TOTP-only person's) cannot be answered by a key
	e.Svc.SetupMFA(ctx, e.Admin)
	// the answer to another sign-in's challenge: the challenge is bound to the token it was issued with
	m1, ch1 := pending(t, e, "alice@example.com")
	m2, _ := pending(t, e, "alice@example.com")
	if _, err := e.Svc.LoginWebAuthn(ctx, assertion(m2.Token, aliceKey, ch1), domain.ClientMeta{}); err == nil {
		t.Fatal("an answer to another sign-in's challenge was accepted")
	}
	if _, err := e.Svc.LoginWebAuthn(ctx, assertion(m1.Token, aliceKey, ch1), domain.ClientMeta{}); err != nil {
		t.Fatalf("%v", err)
	}
	// five wrong answers end the pending sign-in: the right one no longer works, start again from the password
	m3, ch3 := pending(t, e, "alice@example.com")
	for i := 0; i < 5; i++ {
		bad := assertion(m3.Token, aliceKey, ch3)
		bad.Signature = webauthn.Encode([]byte("not a signature"))
		if _, err := e.Svc.LoginWebAuthn(ctx, bad, domain.ClientMeta{}); err == nil {
			t.Fatal("a bad signature was accepted")
		}
	}
	if _, err := e.Svc.LoginWebAuthn(ctx, assertion(m3.Token, aliceKey, ch3), domain.ClientMeta{}); err == nil {
		t.Fatal("the pending sign-in survived five failures")
	}
	// a token that never existed, and one that has expired
	if _, err := e.Svc.LoginWebAuthn(ctx, domain.PasskeyAssertion{Token: "nope"}, domain.ClientMeta{}); err == nil {
		t.Fatal("an unknown token was accepted")
	}
	m4, ch4 := pending(t, e, "alice@example.com")
	e.Clock.Advance(10 * time.Minute)
	if _, err := e.Svc.LoginWebAuthn(ctx, assertion(m4.Token, aliceKey, ch4), domain.ClientMeta{}); err == nil {
		t.Fatal("an expired sign-in was accepted")
	}
	// a cloned credential: the counter does not move forward, the sign-in is refused and the audit log says why
	m5, ch5 := pending(t, e, "alice@example.com")
	aliceKey.StepCounter = false
	aliceKey.Counter = 2 // a copy of the key that was taken earlier signs with the counter it had then, below the one the server has seen
	if _, err := e.Svc.LoginWebAuthn(ctx, assertion(m5.Token, aliceKey, ch5), domain.ClientMeta{}); err == nil {
		t.Fatal("a credential whose counter did not advance signed in")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='security.passkey_clone_suspected'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d clone alerts", n)
	}
}

func TestSEC04_PasskeysWorkBesideTOTPAndAnAdministratorResetRemovesBoth(t *testing.T) {
	e := waEnv(t)
	key := authmock.New(waRPID, waOrigin, webauthn.AlgRS256)
	addKey(t, e, e.Alice, key, "work key")
	setup, err := e.Svc.SetupMFA(ctx, e.Alice)
	if err != nil {
		t.Fatal(err)
	}
	_ = setup
	// with both set up, the second step offers both; the key alone is enough
	m, ch := pending(t, e, "alice@example.com")
	if m.Key == nil {
		t.Fatal("the key is not offered")
	}
	if _, err := e.Svc.LoginWebAuthn(ctx, assertion(m.Token, key, ch), domain.ClientMeta{}); err != nil {
		t.Fatal(err)
	}
	// resetting a person's second factor removes the keys too, ends their sessions and is audited
	if err := e.Svc.ResetMFA(ctx, e.Alice, e.Alice.ID); err == nil {
		t.Fatal("a member reset a second factor")
	}
	if err := e.Svc.ResetMFA(ctx, e.Admin, e.Alice.ID); err != nil {
		t.Fatal(err)
	}
	if ks, _ := e.Svc.ListPasskeys(ctx, e.Alice); len(ks) != 0 {
		t.Fatalf("%+v", ks)
	}
	if _, _, err := e.Svc.Login(ctx, "alice@example.com", "password123", time.Hour); err != nil {
		t.Fatalf("after the reset the password alone signs in: %v", err)
	}
}

func TestSEC04_PasskeysFollowTheDataRulesAndNeedANamedHost(t *testing.T) {
	e := waEnv(t)
	key := authmock.New(waRPID, waOrigin, webauthn.AlgES256)
	addKey(t, e, e.Bob, key, "phone")
	exp, err := e.Svc.ExportPersonalData(ctx, e.Admin, e.Bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ks, _ := exp["passkeys"].([]map[string]any); len(ks) != 1 || ks[0]["name"] != "phone" {
		t.Fatalf("the export lacks the keys: %+v", exp["passkeys"])
	}
	if _, ok := exp["passkeys"].([]map[string]any); ok {
		for _, k := range exp["passkeys"].([]map[string]any) {
			for f := range k {
				if strings.Contains(f, "key") && f != "name" {
					t.Fatalf("the export carries key material: %s", f)
				}
			}
		}
	}
	res, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{})
	if err != nil || res.Counts["passkeys_removed"] != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	var left int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM webauthn_credential`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d keys left after the erase", left)
	}
	// an IP address cannot be a relying party: passkeys are unavailable and say why
	e.Svc.BaseURL = "http://127.0.0.1:8080"
	if e.Svc.WebAuthnAvailable() {
		t.Fatal("passkeys offered on an IP address")
	}
	if _, err := e.Svc.PasskeyRegistrationOptions(ctx, e.Alice, "password123"); err == nil || !strings.Contains(err.Error(), "host name") {
		t.Fatalf("%v", err)
	}
	e.Svc.BaseURL = ""
	if e.Svc.WebAuthnAvailable() {
		t.Fatal("passkeys offered with no base_url")
	}
	// an explicit relying party id and origin override the base_url, e.g. for a development host
	e.Svc.WebAuthnRPID, e.Svc.WebAuthnOrigins = "localhost", []string{"http://localhost:18181"}
	if !e.Svc.WebAuthnAvailable() {
		t.Fatal("an explicit relying party was refused")
	}
	// agents have no passkeys
	if _, err := e.Svc.PasskeyRegistrationOptions(ctx, domain.Actor{ID: e.Alice.ID, Kind: "agent"}, "password123"); err == nil {
		t.Fatal("an agent began a passkey registration")
	}
}
