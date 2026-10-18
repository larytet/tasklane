package webauthn_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/larytet/tasklane/internal/testutil/authmock"
	"github.com/larytet/tasklane/internal/webauthn"
)

const (
	rpID   = "tasklane.example.com"
	origin = "https://tasklane.example.com"
)

var rp = webauthn.RP{ID: rpID, Origins: []string{origin}}

func register(t *testing.T, a *authmock.Authenticator, r webauthn.RP) (webauthn.Credential, error) {
	t.Helper()
	ch := webauthn.NewChallenge()
	cd, att := a.Create(ch)
	return r.VerifyRegistration(ch, cd, att)
}

func TestSEC04_WebAuthnRegistersAndSignsInWithEachAlgorithm(t *testing.T) {
	for name, alg := range map[string]int{"ES256": webauthn.AlgES256, "RS256": webauthn.AlgRS256, "EdDSA": webauthn.AlgEdDSA} {
		t.Run(name, func(t *testing.T) {
			a := authmock.New(rpID, origin, alg)
			cred, err := register(t, a, rp)
			if err != nil || !bytes.Equal(cred.ID, a.CredID) || cred.Alg != alg {
				t.Fatalf("%v %+v", err, cred)
			}
			for i := 0; i < 3; i++ {
				ch := webauthn.NewChallenge()
				cd, ad, sig := a.Get(ch)
				n, err := rp.VerifyAssertion(ch, cred, cd, ad, sig)
				if err != nil {
					t.Fatalf("sign-in %d: %v", i, err)
				}
				cred.SignCount = n
			}
		})
	}
}

func TestSEC04_WebAuthnRegistrationRefusesEachWrongThing(t *testing.T) {
	mk := func() *authmock.Authenticator { return authmock.New(rpID, origin, webauthn.AlgES256) }
	// wrong relying party id, origin, user absent, cross-origin
	if a := mk(); true {
		a.RPID = "evil.example.org"
		if _, err := register(t, a, rp); err == nil {
			t.Error("a credential for another relying party id was accepted")
		}
	}
	if a := mk(); true {
		a.Origin = "https://evil.example.org"
		if _, err := register(t, a, rp); err == nil {
			t.Error("a registration from another origin was accepted")
		}
	}
	if a := mk(); true {
		a.NoUP = true
		if _, err := register(t, a, rp); err == nil {
			t.Error("a registration without the user present was accepted")
		}
	}
	if a := mk(); true {
		a.CrossOrigin = true
		if _, err := register(t, a, rp); err == nil {
			t.Error("a cross-origin registration was accepted")
		}
	}
	// user verification when required
	strict := webauthn.RP{ID: rpID, Origins: []string{origin}, RequireUV: true}
	if _, err := register(t, mk(), strict); err == nil {
		t.Error("an unverified user was accepted when verification is required")
	}
	if a := mk(); true {
		a.UV = true
		if _, err := register(t, a, strict); err != nil {
			t.Errorf("a verified user was refused: %v", err)
		}
	}
	// challenge: another one, an empty one, replayed against a different operation
	a := mk()
	ch := webauthn.NewChallenge()
	cd, att := a.Create(ch)
	if _, err := rp.VerifyRegistration(webauthn.NewChallenge(), cd, att); err == nil {
		t.Error("a registration answering another challenge was accepted")
	}
	if _, err := rp.VerifyRegistration(nil, cd, att); err == nil {
		t.Error("an empty challenge was accepted")
	}
	// a sign-in's client data is not a registration
	gcd, _, _ := a.Get(ch)
	if _, err := rp.VerifyRegistration(ch, gcd, att); err == nil {
		t.Error("client data of a sign-in was accepted for a registration")
	}
	// garbage and truncation never panic and never succeed
	for n := 0; n < len(att); n += 7 {
		if _, err := rp.VerifyRegistration(ch, cd, att[:n]); err == nil {
			t.Errorf("a truncated attestation of %d bytes was accepted", n)
		}
	}
	if _, err := rp.VerifyRegistration(ch, cd, append(append([]byte(nil), att...), 0)); err == nil {
		t.Error("an attestation with a trailing byte was accepted")
	}
	if _, err := rp.VerifyRegistration(ch, []byte("{"), att); err == nil {
		t.Error("bad client data was accepted")
	}
}

func TestSEC04_WebAuthnSignInRefusesEachWrongThing(t *testing.T) {
	a := authmock.New(rpID, origin, webauthn.AlgES256)
	cred, err := register(t, a, rp)
	if err != nil {
		t.Fatal(err)
	}
	ch := webauthn.NewChallenge()
	cd, ad, sig := a.Get(ch)
	good := func() error { _, err := rp.VerifyAssertion(ch, cred, cd, ad, sig); return err }
	if err := good(); err != nil {
		t.Fatal(err)
	}
	flip := func(b []byte, i int) []byte { c := append([]byte(nil), b...); c[i] ^= 1; return c }
	if _, err := rp.VerifyAssertion(webauthn.NewChallenge(), cred, cd, ad, sig); err == nil {
		t.Error("another challenge accepted")
	}
	if _, err := rp.VerifyAssertion(ch, cred, cd, ad, flip(sig, len(sig)-1)); err == nil {
		t.Error("a damaged signature was accepted")
	}
	if _, err := rp.VerifyAssertion(ch, cred, cd, flip(ad, 36), sig); err == nil {
		t.Error("damaged authenticator data was accepted")
	}
	if _, err := rp.VerifyAssertion(ch, cred, append(append([]byte(nil), cd...), ' '), ad, sig); err == nil {
		t.Error("changed client data was accepted: it is part of what was signed")
	}
	// another person's key
	other := authmock.New(rpID, origin, webauthn.AlgES256)
	oc, _ := register(t, other, rp)
	if _, err := rp.VerifyAssertion(ch, oc, cd, ad, sig); err == nil {
		t.Error("a signature from another key was accepted")
	}
	// an algorithm that does not match the key
	c2 := cred
	c2.Alg = webauthn.AlgRS256
	if _, err := rp.VerifyAssertion(ch, c2, cd, ad, sig); err == nil {
		t.Error("an algorithm mismatch was accepted")
	}
	// the registration's client data is not a sign-in's
	rcd, _ := a.Create(ch)
	if _, err := rp.VerifyAssertion(ch, cred, rcd, ad, sig); err == nil {
		t.Error("registration client data accepted for a sign-in")
	}
	// the wrong relying party
	if _, err := (webauthn.RP{ID: "other.example.com", Origins: []string{origin}}).VerifyAssertion(ch, cred, cd, ad, sig); err == nil {
		t.Error("a sign-in for another relying party id was accepted")
	}
	// truncation never panics
	for n := 0; n < len(ad); n += 5 {
		rp.VerifyAssertion(ch, cred, cd, ad[:n], sig)
	}
}

func TestSEC04_WebAuthnSignatureCounterMustAdvanceOrTheCredentialMayBeCloned(t *testing.T) {
	a := authmock.New(rpID, origin, webauthn.AlgES256)
	cred, _ := register(t, a, rp)
	ch := webauthn.NewChallenge()
	cd, ad, sig := a.Get(ch) // counter 1
	n, err := rp.VerifyAssertion(ch, cred, cd, ad, sig)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	cred.SignCount = n
	// the same counter again (a copy of the credential signing, or a replay of the response with a new challenge)
	a.StepCounter = false
	ch2 := webauthn.NewChallenge()
	cd, ad, sig = a.Get(ch2)
	if _, err := rp.VerifyAssertion(ch2, cred, cd, ad, sig); !errors.Is(err, webauthn.ErrCloned) {
		t.Fatalf("a counter that did not advance: %v", err)
	}
	// authenticators that never count (counter 0 on both sides) are fine
	b := authmock.New(rpID, origin, webauthn.AlgEdDSA)
	b.StepCounter = false
	bc, _ := register(t, b, rp)
	for i := 0; i < 2; i++ {
		ch := webauthn.NewChallenge()
		cd, ad, sig := b.Get(ch)
		if _, err := rp.VerifyAssertion(ch, bc, cd, ad, sig); err != nil {
			t.Fatalf("a credential that does not count was refused: %v", err)
		}
	}
}

func TestSEC04_CBORReaderIsBoundedAndStrict(t *testing.T) {
	for _, bad := range [][]byte{
		{}, {0xff}, {0x5f}, {0x9f}, {0xbf}, // indefinite lengths and a stray break
		{0x5a, 0xff, 0xff, 0xff, 0xff},                         // a byte string that claims 4 GiB
		{0x9b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // an array of 2^64-1
		{0xa2, 0x01, 0x01, 0x01, 0x02},                         // a duplicate key
		{0xa1, 0x41, 0x61, 0x01},                               // a byte-string key
		{0xc0, 0x01},                                           // a tag
	} {
		if _, _, err := webauthn.Decode(bad); err == nil {
			t.Errorf("% x was accepted", bad)
		}
	}
	deep := bytes.Repeat([]byte{0x81}, 50) // 50 nested arrays
	if _, _, err := webauthn.Decode(append(deep, 0x00)); err == nil {
		t.Error("deep nesting was accepted")
	}
	if v, n, err := webauthn.Decode([]byte{0xa1, 0x01, 0x82, 0x01, 0x20, 0x99}); err != nil || n != 5 || v.(map[any]any)[int64(1)].([]any)[1] != int64(-1) {
		t.Errorf("%v %d %v", v, n, err)
	}
}

func TestSEC04_WebAuthnRefusesWeakOrUnknownKeys(t *testing.T) {
	// a P-256 point that is not on the curve is refused when the key is read, not when it is used
	a := authmock.New(rpID, origin, webauthn.AlgES256)
	ch := webauthn.NewChallenge()
	cd, att := a.Create(ch)
	bad := append([]byte(nil), att...)
	// the last bytes of the attestation are the key's y coordinate: damage one
	bad[len(bad)-1] ^= 0xff
	if _, err := rp.VerifyRegistration(ch, cd, bad); err == nil {
		t.Error("a point off the curve was accepted")
	}
}

// The parsers read bytes from the network: whatever they are given, they must return an error or a value, never panic or hang.
func FuzzParsers(f *testing.F) {
	a := authmock.New(rpID, origin, webauthn.AlgES256)
	ch := webauthn.NewChallenge()
	cd, att := a.Create(ch)
	gcd, gad, gsig := a.Get(ch)
	f.Add(att, cd, gad, gsig)
	f.Add(att[:len(att)/2], gcd, gad[:20], gsig[:5])
	cred, _ := rp.VerifyRegistration(ch, cd, att)
	f.Fuzz(func(t *testing.T, attestation, clientData, authData, sig []byte) {
		webauthn.Decode(attestation)
		rp.VerifyRegistration(ch, clientData, attestation)
		rp.VerifyAssertion(ch, cred, clientData, authData, sig)
	})
}
