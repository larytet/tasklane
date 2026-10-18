// Package webauthn verifies WebAuthn registrations and sign-ins (W3C WebAuthn Level 2) with the standard library: the relying party
// side only, for passkeys and security keys used as a second factor. Attestation is not verified: Tasklane asks for none, and what a
// browser sends is treated as a claim, not as a trust anchor.
package webauthn

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// COSE algorithm identifiers.
const (
	AlgES256 = -7
	AlgEdDSA = -8
	AlgRS256 = -257
)

// RP is the relying party: the identifier (a registrable domain, never an IP address), the origins pages may come from, and
// whether the authenticator must have verified the person (a PIN or biometric) as well as being present.
type RP struct {
	ID        string
	Origins   []string
	RequireUV bool
}

var b64 = base64.RawURLEncoding

func Encode(b []byte) string            { return b64.EncodeToString(b) }
func Decode64(s string) ([]byte, error) { return b64.DecodeString(s) }

// NewChallenge returns 32 random bytes.
func NewChallenge() []byte {
	c := make([]byte, 32)
	rand.Read(c)
	return c
}

// Credential is what a registration yields and a sign-in needs: the identifier, the public key (PKIX DER), its algorithm and the
// signature counter.
type Credential struct {
	ID        []byte
	PublicKey []byte
	Alg       int
	SignCount uint32
	AAGUID    []byte
	UV        bool
}

type clientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

func (rp RP) checkClient(raw []byte, typ string, challenge []byte) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return errors.New("the client data is not valid")
	}
	if cd.Type != typ {
		return errors.New("the client data is for another operation")
	}
	got, err := b64.DecodeString(cd.Challenge)
	if err != nil || subtle.ConstantTimeCompare(got, challenge) != 1 || len(challenge) == 0 {
		return errors.New("the challenge does not match")
	}
	ok := false
	for _, o := range rp.Origins {
		if cd.Origin == o {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("the origin %q is not one this server accepts", cd.Origin)
	}
	if cd.CrossOrigin {
		return errors.New("a cross-origin request is not accepted")
	}
	return nil
}

const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
	flagED = 0x80
)

type authData struct {
	rpIDHash  []byte
	flags     byte
	signCount uint32
	aaguid    []byte
	credID    []byte
	coseKey   []byte // the CBOR bytes of the credential public key
}

func parseAuthData(b []byte, withCred bool) (authData, error) {
	var a authData
	if len(b) < 37 {
		return a, errors.New("the authenticator data is too short")
	}
	a.rpIDHash, a.flags, a.signCount = b[:32], b[32], binary.BigEndian.Uint32(b[33:37])
	rest := b[37:]
	if a.flags&flagAT != 0 {
		if !withCred {
			return a, errors.New("unexpected credential data in a sign-in")
		}
		if len(rest) < 18 {
			return a, errors.New("the credential data is too short")
		}
		a.aaguid = rest[:16]
		n := int(binary.BigEndian.Uint16(rest[16:18]))
		rest = rest[18:]
		if n == 0 || n > 1023 || len(rest) < n {
			return a, errors.New("the credential id is not valid")
		}
		a.credID, rest = rest[:n], rest[n:]
		_, used, err := Decode(rest)
		if err != nil {
			return a, errors.New("the credential public key is not valid")
		}
		a.coseKey, rest = rest[:used], rest[used:]
	} else if withCred {
		return a, errors.New("the registration carries no credential")
	}
	if a.flags&flagED != 0 { // extension data, which is not used: it must still be well-formed
		_, used, err := Decode(rest)
		if err != nil {
			return a, errors.New("the extension data is not valid")
		}
		rest = rest[used:]
	}
	if len(rest) != 0 {
		return a, errors.New("the authenticator data has extra bytes")
	}
	return a, nil
}

func (rp RP) checkAuth(a authData) error {
	want := sha256.Sum256([]byte(rp.ID))
	if subtle.ConstantTimeCompare(a.rpIDHash, want[:]) != 1 {
		return errors.New("the authenticator answered for another relying party")
	}
	if a.flags&flagUP == 0 {
		return errors.New("the user was not present")
	}
	if rp.RequireUV && a.flags&flagUV == 0 {
		return errors.New("the user was not verified by the authenticator")
	}
	return nil
}

// VerifyRegistration checks a new credential and returns it. clientDataJSON and attestationObject are the raw bytes the browser
// returned.
func (rp RP) VerifyRegistration(challenge, clientDataJSON, attestationObject []byte) (Credential, error) {
	if err := rp.checkClient(clientDataJSON, "webauthn.create", challenge); err != nil {
		return Credential{}, err
	}
	v, used, err := Decode(attestationObject)
	m, ok := v.(map[any]any)
	if err != nil || !ok || used != len(attestationObject) {
		return Credential{}, errors.New("the attestation object is not valid")
	}
	adBytes, ok := m["authData"].([]byte)
	if !ok {
		return Credential{}, errors.New("the attestation object has no authenticator data")
	}
	if _, ok := m["fmt"].(string); !ok {
		return Credential{}, errors.New("the attestation object has no format")
	}
	ad, err := parseAuthData(adBytes, true)
	if err != nil {
		return Credential{}, err
	}
	if err := rp.checkAuth(ad); err != nil {
		return Credential{}, err
	}
	der, alg, err := coseToPKIX(ad.coseKey)
	if err != nil {
		return Credential{}, err
	}
	return Credential{ID: append([]byte(nil), ad.credID...), PublicKey: der, Alg: alg, SignCount: ad.signCount, AAGUID: append([]byte(nil), ad.aaguid...), UV: ad.flags&flagUV != 0}, nil
}

// VerifyAssertion checks a sign-in against a stored credential and returns the new signature counter. A counter that does not
// move forward (when either side uses one) means the credential may have been cloned and is refused.
func (rp RP) VerifyAssertion(challenge []byte, cred Credential, clientDataJSON, authenticatorData, signature []byte) (uint32, error) {
	if err := rp.checkClient(clientDataJSON, "webauthn.get", challenge); err != nil {
		return 0, err
	}
	ad, err := parseAuthData(authenticatorData, false)
	if err != nil {
		return 0, err
	}
	if err := rp.checkAuth(ad); err != nil {
		return 0, err
	}
	key, err := x509.ParsePKIXPublicKey(cred.PublicKey)
	if err != nil {
		return 0, errors.New("the stored public key is not valid")
	}
	ch := sha256.Sum256(clientDataJSON)
	signed := append(append([]byte(nil), authenticatorData...), ch[:]...)
	if err := verifySig(key, cred.Alg, signed, signature); err != nil {
		return 0, errors.New("the signature is not valid")
	}
	if (cred.SignCount != 0 || ad.signCount != 0) && ad.signCount <= cred.SignCount {
		return 0, ErrCloned
	}
	return ad.signCount, nil
}

// ErrCloned says the counter did not advance: a copy of the credential may exist.
var ErrCloned = errors.New("the signature counter did not advance; the credential may have been cloned")

func verifySig(key crypto.PublicKey, alg int, signed, sig []byte) error {
	switch k := key.(type) {
	case *ecdsa.PublicKey:
		if alg != AlgES256 {
			return errors.New("algorithm mismatch")
		}
		h := sha256.Sum256(signed)
		if !ecdsa.VerifyASN1(k, h[:], sig) {
			return errors.New("bad signature")
		}
		return nil
	case *rsa.PublicKey:
		if alg != AlgRS256 {
			return errors.New("algorithm mismatch")
		}
		h := sha256.Sum256(signed)
		return rsa.VerifyPKCS1v15(k, crypto.SHA256, h[:], sig)
	case ed25519.PublicKey:
		if alg != AlgEdDSA || !ed25519.Verify(k, signed, sig) {
			return errors.New("bad signature")
		}
		return nil
	}
	return errors.New("unsupported key")
}

// coseToPKIX turns a COSE_Key into PKIX DER, accepting only ES256 (P-256), RS256 and EdDSA (Ed25519).
func coseToPKIX(b []byte) ([]byte, int, error) {
	v, _, err := Decode(b)
	m, ok := v.(map[any]any)
	if err != nil || !ok {
		return nil, 0, errors.New("the public key is not a COSE key")
	}
	get := func(k int64) any { return m[k] }
	kty, _ := get(1).(int64)
	alg, _ := get(3).(int64)
	switch {
	case kty == 2 && alg == AlgES256: // EC2, P-256
		crv, _ := get(-1).(int64)
		x, _ := get(-2).([]byte)
		y, _ := get(-3).([]byte)
		if crv != 1 || len(x) != 32 || len(y) != 32 {
			return nil, 0, errors.New("the elliptic curve key is not P-256")
		}
		pk := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pk.Curve.IsOnCurve(pk.X, pk.Y) {
			return nil, 0, errors.New("the point is not on the curve")
		}
		der, err := x509.MarshalPKIXPublicKey(pk)
		return der, AlgES256, err
	case kty == 3 && alg == AlgRS256: // RSA
		n, _ := get(-1).([]byte)
		e, _ := get(-2).([]byte)
		if len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return nil, 0, errors.New("the RSA key is too small or malformed")
		}
		pk := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		der, err := x509.MarshalPKIXPublicKey(pk)
		return der, AlgRS256, err
	case kty == 1 && alg == AlgEdDSA: // OKP, Ed25519
		crv, _ := get(-1).(int64)
		x, _ := get(-2).([]byte)
		if crv != 6 || len(x) != ed25519.PublicKeySize {
			return nil, 0, errors.New("the Ed25519 key is malformed")
		}
		der, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(bytes.Clone(x)))
		return der, AlgEdDSA, err
	}
	return nil, 0, fmt.Errorf("the key type %d with algorithm %d is not supported", kty, alg)
}
