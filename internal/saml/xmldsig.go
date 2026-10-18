package saml

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	algExcC14N   = "http://www.w3.org/2001/10/xml-exc-c14n#"
	algEnveloped = "http://www.w3.org/2000/09/xmldsig#enveloped-signature"
)

var sigAlgs = map[string]crypto.Hash{
	"http://www.w3.org/2001/04/xmldsig-more#rsa-sha256": crypto.SHA256,
	"http://www.w3.org/2001/04/xmldsig-more#rsa-sha384": crypto.SHA384,
	"http://www.w3.org/2001/04/xmldsig-more#rsa-sha512": crypto.SHA512,
}

var digestAlgs = map[string]crypto.Hash{
	"http://www.w3.org/2001/04/xmlenc#sha256":       crypto.SHA256,
	"http://www.w3.org/2001/04/xmldsig-more#sha384": crypto.SHA384,
	"http://www.w3.org/2001/04/xmlenc#sha512":       crypto.SHA512,
}

func hashOf(h crypto.Hash, data []byte) []byte {
	switch h {
	case crypto.SHA384:
		s := sha512.Sum384(data)
		return s[:]
	case crypto.SHA512:
		s := sha512.Sum512(data)
		return s[:]
	}
	s := sha256.Sum256(data)
	return s[:]
}

func b64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
}

// uniqueIDs refuses a document in which two elements carry the same ID: a signature names its target by ID, and a second element with that
// ID is how a signed element is shadowed by an unsigned one.
func uniqueIDs(root *Node) (map[string]*Node, error) {
	ids := map[string]*Node{}
	var err error
	root.walk(func(n *Node) {
		if v, ok := n.attr("ID"); ok {
			if _, dup := ids[v]; dup {
				err = fmt.Errorf("the ID %q appears twice", v)
			}
			ids[v] = n
		}
	})
	return ids, err
}

// verifySignature checks one ds:Signature element against the trusted certificates and returns the element it signs. It accepts exactly:
// exclusive canonicalization of SignedInfo, an RSA signature with SHA-256, SHA-384 or SHA-512, and a single reference to an element of this
// document by ID, with the enveloped-signature and exclusive canonicalization transforms and a SHA-2 digest. Everything else is refused.
func verifySignature(sig *Node, ids map[string]*Node, certs []*x509.Certificate, now time.Time) (*Node, error) {
	si, err := sig.child(nsDS, "SignedInfo")
	if err != nil || si == nil {
		return nil, fmt.Errorf("the signature has no single SignedInfo")
	}
	cm, err := si.child(nsDS, "CanonicalizationMethod")
	if err != nil || cm == nil {
		return nil, fmt.Errorf("the signature has no canonicalization method")
	}
	if alg, _ := cm.attr("Algorithm"); alg != algExcC14N {
		return nil, fmt.Errorf("canonicalization %q is not supported (exclusive canonicalization without comments only)", alg)
	}
	sm, err := si.child(nsDS, "SignatureMethod")
	if err != nil || sm == nil {
		return nil, fmt.Errorf("the signature has no signature method")
	}
	alg, _ := sm.attr("Algorithm")
	hash, ok := sigAlgs[alg]
	if !ok {
		return nil, fmt.Errorf("signature method %q is not supported (RSA with SHA-256, SHA-384 or SHA-512)", alg)
	}
	refs := si.children(nsDS, "Reference")
	if len(refs) != 1 {
		return nil, fmt.Errorf("the signature must have exactly one reference")
	}
	ref := refs[0]
	uri, _ := ref.attr("URI")
	if !strings.HasPrefix(uri, "#") || len(uri) < 2 {
		return nil, fmt.Errorf("the reference must name an element of this document by ID")
	}
	target := ids[uri[1:]]
	if target == nil {
		return nil, fmt.Errorf("the reference names no element")
	}
	// the transforms: the enveloped-signature transform, then exclusive canonicalization (with an optional prefix list); nothing else
	var inclusive []string
	if tr, err := ref.child(nsDS, "Transforms"); err != nil {
		return nil, err
	} else if tr != nil {
		sawC14N := false
		for _, t := range tr.children(nsDS, "Transform") {
			a, _ := t.attr("Algorithm")
			switch a {
			case algEnveloped:
				if sawC14N {
					return nil, fmt.Errorf("the enveloped-signature transform must come before canonicalization")
				}
			case algExcC14N:
				sawC14N = true
				for _, in := range t.Children {
					if in.IsText {
						continue
					}
					if in.Local != "InclusiveNamespaces" {
						return nil, fmt.Errorf("an unexpected element in a transform")
					}
					if pl, ok := in.attr("PrefixList"); ok {
						inclusive = strings.Fields(pl)
					}
				}
			default:
				return nil, fmt.Errorf("transform %q is not supported", a)
			}
		}
	}
	dm, err := ref.child(nsDS, "DigestMethod")
	if err != nil || dm == nil {
		return nil, fmt.Errorf("the reference has no digest method")
	}
	dalg, _ := dm.attr("Algorithm")
	dh, ok := digestAlgs[dalg]
	if !ok {
		return nil, fmt.Errorf("digest method %q is not supported (SHA-256, SHA-384 or SHA-512)", dalg)
	}
	dv, err := ref.child(nsDS, "DigestValue")
	if err != nil || dv == nil {
		return nil, fmt.Errorf("the reference has no digest value")
	}
	want, err := b64(dv.text())
	if err != nil {
		return nil, fmt.Errorf("the digest value is not base64")
	}
	// the digest of what is signed: the target without this signature, canonicalized
	got := hashOf(dh, canonicalize(target, sig, inclusive))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return nil, fmt.Errorf("the digest of the signed element does not match: the document was changed after it was signed")
	}
	// the signature over SignedInfo
	sv, err := sig.child(nsDS, "SignatureValue")
	if err != nil || sv == nil {
		return nil, fmt.Errorf("the signature has no value")
	}
	raw, err := b64(sv.text())
	if err != nil {
		return nil, fmt.Errorf("the signature value is not base64")
	}
	digest := hashOf(hash, canonicalize(si, nil, nil))
	for _, c := range certs {
		if now.Before(c.NotBefore) || now.After(c.NotAfter) {
			continue
		}
		pub, ok := c.PublicKey.(*rsa.PublicKey)
		if !ok {
			continue
		}
		if rsa.VerifyPKCS1v15(pub, hash, digest, raw) == nil {
			return target, nil
		}
	}
	return nil, fmt.Errorf("the signature was not made by a trusted certificate that is valid now")
}
