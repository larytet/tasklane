// Package authmock is a virtual WebAuthn authenticator for tests (TESTPLAN 3.2): it makes registrations and sign-ins the way a
// browser and a security key do, and can be told to get things wrong so that the checks can be tested.
package authmock

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"sort"

	"github.com/larytet/tasklane/internal/webauthn"
)

type Authenticator struct {
	RPID    string
	Origin  string
	Alg     int
	CredID  []byte
	Counter uint32
	// Faults, for negative tests.
	NoUP, UV, CrossOrigin bool
	StepCounter           bool // add one to the counter on each sign-in (a real one does; many platform ones keep it at 0)
	Format                string
	key                   crypto.Signer
}

func New(rpID, origin string, alg int) *Authenticator {
	a := &Authenticator{RPID: rpID, Origin: origin, Alg: alg, StepCounter: true, Format: "none"}
	a.CredID = make([]byte, 32)
	rand.Read(a.CredID)
	var err error
	switch alg {
	case webauthn.AlgES256:
		a.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case webauthn.AlgRS256:
		a.key, err = rsa.GenerateKey(rand.Reader, 2048)
	case webauthn.AlgEdDSA:
		_, a.key, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil || a.key == nil {
		panic("authmock: key")
	}
	return a
}

// ---- a minimal CBOR encoder

func head(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 256:
		return []byte{major<<5 | 24, byte(n)}
	case n < 65536:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
	b := make([]byte, 5)
	b[0] = major<<5 | 26
	binary.BigEndian.PutUint32(b[1:], uint32(n))
	return b
}

type kv struct{ k, v any }

func enc(v any) []byte {
	switch x := v.(type) {
	case int:
		if x >= 0 {
			return head(0, uint64(x))
		}
		return head(1, uint64(-1-x))
	case []byte:
		return append(head(2, uint64(len(x))), x...)
	case string:
		return append(head(3, uint64(len(x))), x...)
	case []kv:
		out := head(5, uint64(len(x)))
		sort.SliceStable(x, func(i, j int) bool { return string(enc(x[i].k)) < string(enc(x[j].k)) })
		for _, p := range x {
			out = append(append(out, enc(p.k)...), enc(p.v)...)
		}
		return out
	}
	panic("authmock: cannot encode")
}

func pad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	return append(make([]byte, n-len(b)), b...)
}

func (a *Authenticator) cose() []byte {
	switch k := a.key.(type) {
	case *ecdsa.PrivateKey:
		return enc([]kv{{1, 2}, {3, a.Alg}, {-1, 1}, {-2, pad(k.X.Bytes(), 32)}, {-3, pad(k.Y.Bytes(), 32)}})
	case *rsa.PrivateKey:
		return enc([]kv{{1, 3}, {3, a.Alg}, {-1, k.N.Bytes()}, {-2, []byte{1, 0, 1}}})
	case ed25519.PrivateKey:
		return enc([]kv{{1, 1}, {3, a.Alg}, {-1, 6}, {-2, []byte(k.Public().(ed25519.PublicKey))}})
	}
	panic("authmock: key type")
}

func (a *Authenticator) flags(extra byte) byte {
	f := extra
	if !a.NoUP {
		f |= 0x01
	}
	if a.UV {
		f |= 0x04
	}
	return f
}

func (a *Authenticator) clientData(typ string, challenge []byte) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": webauthn.Encode(challenge), "origin": a.Origin, "crossOrigin": a.CrossOrigin})
	return b
}

// Create answers navigator.credentials.create: the client data and the attestation object.
func (a *Authenticator) Create(challenge []byte) (clientDataJSON, attestationObject []byte) {
	h := sha256.Sum256([]byte(a.RPID))
	ad := append([]byte(nil), h[:]...)
	ad = append(ad, a.flags(0x40))
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, a.Counter)
	ad = append(ad, cnt...)
	ad = append(ad, make([]byte, 16)...) // AAGUID: none
	l := make([]byte, 2)
	binary.BigEndian.PutUint16(l, uint16(len(a.CredID)))
	ad = append(append(append(ad, l...), a.CredID...), a.cose()...)
	return a.clientData("webauthn.create", challenge), enc([]kv{{"fmt", a.Format}, {"attStmt", []kv{}}, {"authData", ad}})
}

// Get answers navigator.credentials.get: the client data, the authenticator data and the signature.
func (a *Authenticator) Get(challenge []byte) (clientDataJSON, authenticatorData, signature []byte) {
	if a.StepCounter {
		a.Counter++
	}
	h := sha256.Sum256([]byte(a.RPID))
	ad := append([]byte(nil), h[:]...)
	ad = append(ad, a.flags(0))
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, a.Counter)
	ad = append(ad, cnt...)
	cd := a.clientData("webauthn.get", challenge)
	ch := sha256.Sum256(cd)
	signed := append(append([]byte(nil), ad...), ch[:]...)
	var sig []byte
	var err error
	switch k := a.key.(type) {
	case *ecdsa.PrivateKey:
		d := sha256.Sum256(signed)
		sig, err = ecdsa.SignASN1(rand.Reader, k, d[:])
	case *rsa.PrivateKey:
		d := sha256.Sum256(signed)
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
	case ed25519.PrivateKey:
		sig = ed25519.Sign(k, signed)
	}
	if err != nil {
		panic(err)
	}
	return cd, ad, sig
}
