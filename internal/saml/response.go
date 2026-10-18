package saml

import (
	"bytes"
	"compress/flate"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"
)

// Expect is what a response must satisfy: who we are, who may speak, where the answer must have been sent and to which request.
type Expect struct {
	IdP       string // the identity provider's entity id (the Issuer)
	SP        string // our entity id (the Audience)
	ACS       string // our assertion consumer service address (the Destination and the Recipient)
	RequestID string // the AuthnRequest this answers: an unsolicited response is refused
	Certs     []*x509.Certificate
	Now       time.Time
	Skew      time.Duration
}

// Assertion is what a verified response says about the person.
type Assertion struct {
	ID         string
	NameID     string
	NameFormat string
	Attrs      map[string][]string
	Expires    time.Time
}

func ts(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, l := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time", s)
}

// ParseResponse checks a SAML Response and returns the assertion it carries. The checks, in order: the document is well formed and safe; it is a
// Response to our request, for our address, from the provider we trust, with a success status; there is exactly one Assertion and it is not
// encrypted; a signature covers that Assertion (directly or through the Response) and verifies against a trusted certificate, and every
// signature present must verify; and the assertion is from the same provider, is current, is for us, and is confirmed for this request.
func ParseResponse(doc []byte, x Expect) (*Assertion, error) {
	root, err := Parse(doc)
	if err != nil {
		return nil, fmt.Errorf("the response is not acceptable XML: %w", err)
	}
	if !root.is(nsP, "Response") {
		return nil, fmt.Errorf("the document is not a SAML Response")
	}
	ids, err := uniqueIDs(root)
	if err != nil {
		return nil, err
	}
	if v, _ := root.attr("Version"); v != "2.0" {
		return nil, fmt.Errorf("the response is not SAML 2.0")
	}
	if x.RequestID == "" {
		return nil, fmt.Errorf("there is no request this could answer")
	}
	if in, ok := root.attr("InResponseTo"); !ok || in != x.RequestID {
		return nil, fmt.Errorf("the response does not answer our request (unsolicited responses are not accepted)")
	}
	if d, ok := root.attr("Destination"); ok && d != x.ACS {
		return nil, fmt.Errorf("the response was sent to %q, not to us", d)
	}
	st, err := root.child(nsP, "Status")
	if err != nil || st == nil {
		return nil, fmt.Errorf("the response has no status")
	}
	sc, err := st.child(nsP, "StatusCode")
	if err != nil || sc == nil {
		return nil, fmt.Errorf("the response has no status code")
	}
	if v, _ := sc.attr("Value"); v != "urn:oasis:names:tc:SAML:2.0:status:Success" {
		return nil, fmt.Errorf("the identity provider did not sign the person in (%s)", v)
	}
	for _, c := range root.Children {
		if c.is(nsSAML, "EncryptedAssertion") {
			return nil, fmt.Errorf("encrypted assertions are not supported: turn encryption off at the identity provider (the response is signed and sent over TLS)")
		}
	}
	if iss, err := root.child(nsSAML, "Issuer"); err != nil {
		return nil, err
	} else if iss != nil && iss.text() != x.IdP {
		return nil, fmt.Errorf("the response is from %q, not from the identity provider we trust", iss.text())
	}
	as := root.children(nsSAML, "Assertion")
	if len(as) != 1 {
		return nil, fmt.Errorf("the response must carry exactly one assertion (it has %d)", len(as))
	}
	a := as[0]
	// signatures: those of the Response and of the Assertion; each one present must verify, and one must cover the assertion
	covered := false
	nsig := 0
	for _, holder := range []*Node{root, a} {
		sigs := holder.children(nsDS, "Signature")
		if len(sigs) > 1 {
			return nil, fmt.Errorf("more than one signature on an element")
		}
		for _, sig := range sigs {
			nsig++
			target, err := verifySignature(sig, ids, x.Certs, x.Now)
			if err != nil {
				return nil, fmt.Errorf("signature: %w", err)
			}
			if target == a || target == root {
				covered = true
			} else {
				return nil, fmt.Errorf("the signature covers an element that is not the assertion or the response")
			}
		}
	}
	if nsig == 0 || !covered {
		return nil, fmt.Errorf("the assertion is not signed")
	}
	// from here on only the assertion that the signature covers is read
	if v, _ := a.attr("Version"); v != "2.0" {
		return nil, fmt.Errorf("the assertion is not SAML 2.0")
	}
	aid, _ := a.attr("ID")
	if aid == "" {
		return nil, fmt.Errorf("the assertion has no ID")
	}
	if iss, err := a.child(nsSAML, "Issuer"); err != nil || iss == nil || iss.text() != x.IdP {
		return nil, fmt.Errorf("the assertion is not from the identity provider we trust")
	}
	skew := x.Skew
	// the subject, confirmed for this request, this address, this moment
	sub, err := a.child(nsSAML, "Subject")
	if err != nil || sub == nil {
		return nil, fmt.Errorf("the assertion has no subject")
	}
	nid, err := sub.child(nsSAML, "NameID")
	if err != nil || nid == nil || nid.text() == "" {
		return nil, fmt.Errorf("the assertion has no NameID")
	}
	out := &Assertion{ID: aid, NameID: nid.text(), Attrs: map[string][]string{}}
	out.NameFormat, _ = nid.attr("Format")
	confirmed := false
	for _, sc := range sub.children(nsSAML, "SubjectConfirmation") {
		if m, _ := sc.attr("Method"); m != "urn:oasis:names:tc:SAML:2.0:cm:bearer" {
			continue
		}
		d, _ := sc.child(nsSAML, "SubjectConfirmationData")
		if d == nil {
			continue
		}
		if r, _ := d.attr("Recipient"); r != x.ACS {
			continue
		}
		if in, _ := d.attr("InResponseTo"); in != x.RequestID {
			continue
		}
		nb, ok := d.attr("NotOnOrAfter")
		if !ok {
			continue
		}
		t, err := ts(nb)
		if err != nil || !x.Now.Before(t.Add(skew)) {
			continue
		}
		if b, ok := d.attr("NotBefore"); ok {
			if t, err := ts(b); err != nil || x.Now.Add(skew).Before(t) {
				continue
			}
		}
		confirmed, out.Expires = true, t
	}
	if !confirmed {
		return nil, fmt.Errorf("the assertion is not confirmed for this request, this address and this moment")
	}
	if cond, err := a.child(nsSAML, "Conditions"); err != nil {
		return nil, err
	} else if cond != nil {
		if v, ok := cond.attr("NotBefore"); ok {
			if t, err := ts(v); err != nil || x.Now.Add(skew).Before(t) {
				return nil, fmt.Errorf("the assertion is not valid yet")
			}
		}
		if v, ok := cond.attr("NotOnOrAfter"); ok {
			if t, err := ts(v); err != nil || !x.Now.Before(t.Add(skew)) {
				return nil, fmt.Errorf("the assertion has expired")
			}
		}
		for _, ar := range cond.children(nsSAML, "AudienceRestriction") {
			ok := false
			for _, au := range ar.children(nsSAML, "Audience") {
				ok = ok || au.text() == x.SP
			}
			if !ok {
				return nil, fmt.Errorf("the assertion is not meant for us")
			}
		}
		for _, c := range cond.Children {
			if !c.IsText && c.NS == nsSAML && c.Local != "AudienceRestriction" && c.Local != "OneTimeUse" && c.Local != "ProxyRestriction" {
				return nil, fmt.Errorf("the assertion has a condition we do not understand (%s)", c.Local)
			}
		}
	} else {
		return nil, fmt.Errorf("the assertion has no conditions, so no audience: it is not accepted")
	}
	for _, st := range a.children(nsSAML, "AttributeStatement") {
		for _, at := range st.children(nsSAML, "Attribute") {
			name, _ := at.attr("Name")
			for _, v := range at.children(nsSAML, "AttributeValue") {
				out.Attrs[name] = append(out.Attrs[name], v.text())
			}
		}
	}
	return out, nil
}

// ---- requests and metadata ----

// AuthnRequest is the request to send the person's browser to the identity provider with (HTTP-Redirect binding: the XML deflated and base64
// encoded in the SAMLRequest parameter).
func AuthnRequest(id string, issueInstant time.Time, idpSSO, sp, acs string) (string, error) {
	xml := `<samlp:AuthnRequest xmlns:samlp="` + nsP + `" xmlns:saml="` + nsSAML + `" ID="` + html.EscapeString(id) + `" Version="2.0" IssueInstant="` + issueInstant.UTC().Format("2006-01-02T15:04:05Z") +
		`" Destination="` + html.EscapeString(idpSSO) + `" ProtocolBinding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" AssertionConsumerServiceURL="` + html.EscapeString(acs) + `">` +
		`<saml:Issuer>` + html.EscapeString(sp) + `</saml:Issuer><samlp:NameIDPolicy AllowCreate="true"/></samlp:AuthnRequest>`
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", err
	}
	w.Write([]byte(xml))
	w.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// RedirectURL is the identity provider's address with the request and the relay state attached.
func RedirectURL(idpSSO, request, relay string) string {
	sep := "?"
	if strings.Contains(idpSSO, "?") {
		sep = "&"
	}
	return idpSSO + sep + url.Values{"SAMLRequest": {request}, "RelayState": {relay}}.Encode()
}

// SPMetadata is the document an administrator gives the identity provider: our entity id and where assertions are to be sent.
func SPMetadata(sp, acs string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<md:EntityDescriptor xmlns:md="` + nsMD + `" entityID="` + html.EscapeString(sp) + `">` +
		`<md:SPSSODescriptor AuthnRequestsSigned="false" WantAssertionsSigned="true" protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">` +
		`<md:NameIDFormat>urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress</md:NameIDFormat>` +
		`<md:AssertionConsumerService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="` + html.EscapeString(acs) + `" index="0" isDefault="true"/>` +
		`</md:SPSSODescriptor></md:EntityDescriptor>` + "\n"
}

// ParseCerts reads one or more certificates, as PEM blocks or as the bare base64 of a DER certificate (the way metadata carries them).
func ParseCerts(text string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := []byte(text)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("a %s is not a certificate", blk.Type)
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		der, err := b64(text)
		if err != nil {
			return nil, fmt.Errorf("no certificate found")
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("no certificate found: %v", err)
		}
		out = append(out, c)
	}
	return out, nil
}

// IdPMetadata reads an identity provider's metadata: its entity id, where to send the browser (the Redirect binding), and its signing certificates.
func IdPMetadata(doc []byte) (entity, sso string, certs []string, err error) {
	root, err := Parse(doc)
	if err != nil {
		return "", "", nil, err
	}
	ed := root
	if !ed.is(nsMD, "EntityDescriptor") {
		return "", "", nil, fmt.Errorf("this is not SAML metadata")
	}
	entity, _ = ed.attr("entityID")
	idp, err := ed.child(nsMD, "IDPSSODescriptor")
	if err != nil || idp == nil {
		return "", "", nil, fmt.Errorf("the metadata describes no identity provider")
	}
	for _, s := range idp.children(nsMD, "SingleSignOnService") {
		if b, _ := s.attr("Binding"); b == "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" {
			sso, _ = s.attr("Location")
		}
	}
	for _, kd := range idp.children(nsMD, "KeyDescriptor") {
		if u, ok := kd.attr("use"); ok && u != "signing" {
			continue
		}
		kd.walk(func(n *Node) {
			if n.is(nsDS, "X509Certificate") {
				certs = append(certs, "-----BEGIN CERTIFICATE-----\n"+strings.Join(strings.Fields(n.text()), "")+"\n-----END CERTIFICATE-----\n")
			}
		})
	}
	if entity == "" || sso == "" || len(certs) == 0 {
		return "", "", nil, fmt.Errorf("the metadata lacks an entity id, a redirect sign-on address, or a signing certificate")
	}
	return entity, sso, certs, nil
}
