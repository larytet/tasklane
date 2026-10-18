package saml

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The fixtures in testdata are made by gen.py with lxml (libxml2's exclusive canonicalization) and the Python cryptography package: nothing in
// them was produced by this package, so a good fixture verifying proves the canonicalization here agrees with an independent implementation.

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func expect(t *testing.T) Expect {
	t.Helper()
	certs, err := ParseCerts(string(load(t, "idp.pem")))
	if err != nil {
		t.Fatal(err)
	}
	return Expect{IdP: "https://idp.example/entity", SP: "https://tasklane.example/api/v1/auth/saml/metadata", ACS: "https://tasklane.example/api/v1/auth/saml/acs",
		RequestID: "_req1", Certs: certs, Now: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), Skew: time.Minute}
}

func TestGoodResponsesVerifyAndYieldThePerson(t *testing.T) {
	for _, name := range []string{"assertion_signed", "response_signed", "both_signed", "inclusive_prefix", "special_chars", "sha512", "default_ds_ns", "other_prefixes"} {
		a, err := ParseResponse(load(t, name+".xml"), expect(t))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a.NameID != "ana@example.com" || a.Attrs["email"][0] != "ana@example.com" || a.ID != "_assert1" {
			t.Fatalf("%s: %+v", name, a)
		}
	}
	a, _ := ParseResponse(load(t, "special_chars.xml"), expect(t))
	if got := a.Attrs["displayName"][0]; !strings.Contains(got, `<"Nova&k">`) || !strings.Contains(got, "Åž") || !strings.Contains(got, "second line") {
		t.Fatalf("special characters: %q", got)
	}
}

func TestRefusedResponses(t *testing.T) {
	cases := map[string]string{
		"other_key":                "not made by a trusted certificate",
		"unsigned":                 "not signed",
		"sha1":                     "not supported",
		"wrong_audience":           "not meant for us",
		"expired":                  "not confirmed for this request",
		"not_yet":                  "not valid yet",
		"wrong_recipient":          "not confirmed for this request",
		"wrong_inresponse":         "unsolicited",
		"wrong_destination":        "not to us",
		"wrong_issuer":             "not from the identity provider we trust",
		"no_conditions":            "no conditions",
		"status_failure":           "did not sign the person in",
		"xsw_two_assertions":       "exactly one assertion",
		"xsw_duplicate_id":         "appears twice",
		"xsw_signed_in_extensions": "not signed",
		"bad_reference":            "names no element",
		"doctype":                  "document type declaration",
		"encrypted":                "encrypted assertions",
		"extra_transform":          "transform",
	}
	for name, want := range cases {
		_, err := ParseResponse(load(t, name+".xml"), expect(t))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: got %v, want an error containing %q", name, err, want)
		}
	}
}

// Changing a signed document in any way that matters breaks it: the person, the audience, a signed attribute, the signature value.
func TestTamperingBreaksTheSignature(t *testing.T) {
	good := string(load(t, "assertion_signed.xml"))
	for name, mut := range map[string]func(string) string{
		"the person":   func(s string) string { return strings.Replace(s, "ana@example.com", "admin@example.com", 1) },
		"the lifetime": func(s string) string { return strings.Replace(s, "10:05:00Z", "23:05:00Z", 1) },
		"the audience": func(s string) string {
			return strings.Replace(s, "tasklane.example/api/v1/auth/saml/metadata", "tasklane.example/api/v1/auth/saml/metadatb", 1)
		},
		"the value": func(s string) string {
			i := strings.Index(s, "<ds:SignatureValue>") + len("<ds:SignatureValue>")
			c := s[i]
			if c == 'A' {
				c = 'B'
			} else {
				c = 'A'
			}
			return s[:i] + string(c) + s[i+1:]
		},
		"the digest": func(s string) string {
			i := strings.Index(s, "<ds:DigestValue>") + len("<ds:DigestValue>")
			c := s[i]
			if c == 'A' {
				c = 'B'
			} else {
				c = 'A'
			}
			return s[:i] + string(c) + s[i+1:]
		},
	} {
		bad := mut(good)
		if bad == good {
			t.Fatalf("%s: the mutation changed nothing", name)
		}
		if _, err := ParseResponse([]byte(bad), expect(t)); err == nil {
			t.Fatalf("changing %s was not caught", name)
		}
	}
	// time and keys: the same good response is refused outside its lifetime and with a different trusted key
	x := expect(t)
	x.Now = x.Now.Add(2 * time.Hour)
	if _, err := ParseResponse([]byte(good), x); err == nil {
		t.Fatal("an old response was accepted")
	}
	other, _ := ParseCerts(string(load(t, "other.pem")))
	x = expect(t)
	x.Certs = other
	if _, err := ParseResponse([]byte(good), x); err == nil {
		t.Fatal("accepted without the signing certificate")
	}
	// an expired certificate does not verify
	x = expect(t)
	x.Now = time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := ParseResponse([]byte(good), x); err == nil {
		t.Fatal("a response verified against an expired certificate")
	}
}

func TestParseIsStrict(t *testing.T) {
	for name, doc := range map[string]string{
		"two roots":         `<a/><b/>`,
		"mismatched":        `<a><b></a></b>`,
		"undeclared prefix": `<a:x/>`,
		"a PI":              `<a><?php x ?></a>`,
		"an entity":         `<a>&foo;</a>`,
		"unfinished":        `<a><b>`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	deep := strings.Repeat("<a>", 100) + strings.Repeat("</a>", 100)
	if _, err := Parse([]byte(deep)); err == nil {
		t.Fatal("a very deep document was accepted")
	}
	if _, err := Parse(make([]byte, MaxSize+1)); err == nil {
		t.Fatal("a huge document was accepted")
	}
}

func TestMetadataAndRequest(t *testing.T) {
	entity, sso, certs, err := IdPMetadata(load(t, "idp_metadata.xml"))
	if err != nil || entity != "https://idp.example/entity" || sso != "https://idp.example/sso" || len(certs) != 1 {
		t.Fatalf("idp metadata: %q %q %d %v", entity, sso, len(certs), err)
	}
	cs, err := ParseCerts(certs[0])
	if err != nil || len(cs) != 1 {
		t.Fatalf("the certificate from metadata: %v", err)
	}
	if _, _, _, err := IdPMetadata([]byte(`<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="x"/>`)); err == nil {
		t.Fatal("metadata without an identity provider was accepted")
	}
	md := SPMetadata("https://sp.example/meta", "https://sp.example/acs?a=1&b=2")
	root, err := Parse([]byte(md))
	if err != nil || root.Local != "EntityDescriptor" {
		t.Fatalf("our own metadata does not parse: %v", err)
	}
	if !strings.Contains(md, "a=1&amp;b=2") {
		t.Fatalf("the address is not escaped: %s", md)
	}
	req, err := AuthnRequest("_x", time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC), "https://idp.example/sso", "https://sp.example/meta", "https://sp.example/acs")
	if err != nil || req == "" {
		t.Fatal(err)
	}
	if u := RedirectURL("https://idp.example/sso?tenant=1", req, "relay"); !strings.HasPrefix(u, "https://idp.example/sso?tenant=1&") || !strings.Contains(u, "RelayState=relay") {
		t.Fatalf("redirect: %s", u)
	}
}
