package domain

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/saml"
)

// SAML 2.0 sign-in (SP-initiated Web SSO). The person is sent to the identity provider with an AuthnRequest; it answers by POST to our
// assertion consumer service with a signed Response. Everything that makes the answer trustworthy is in internal/saml (read its header);
// here live the configuration, the request that was sent (the Response must answer it), the one-time use of an assertion, and the match
// of the person to an account, which is the same as for OpenID Connect.

type CertInfo struct {
	Subject string    `json:"subject"`
	Expires time.Time `json:"expires"`
	SHA256  string    `json:"sha256"`
}

func certInfo(pems []string) []CertInfo {
	out := []CertInfo{}
	for _, p := range pems {
		cs, err := saml.ParseCerts(p)
		if err != nil {
			continue
		}
		for _, c := range cs {
			sum := sha256.Sum256(c.Raw)
			out = append(out, CertInfo{Subject: c.Subject.String(), Expires: c.NotAfter, SHA256: hex.EncodeToString(sum[:])})
		}
	}
	return out
}

func (s *Service) samlACS() string {
	return strings.TrimRight(s.BaseURL, "/") + "/api/v1/auth/saml/acs"
}
func (s *Service) samlMetadataURL(name string) string {
	return strings.TrimRight(s.BaseURL, "/") + "/api/v1/auth/saml/" + name + "/metadata"
}

func (s *Service) samlRequestID() string {
	if s.SAMLID != nil {
		return s.SAMLID()
	}
	return "_" + hex.EncodeToString([]byte(randString(16)))
}

// prepareSAML checks and completes a SAML provider before it is saved.
func (s *Service) prepareSAML(ctx context.Context, in *SSOProvider) error {
	if strings.TrimSpace(in.SAMLMetadata) != "" {
		entity, sso, certs, err := saml.IdPMetadata([]byte(in.SAMLMetadata))
		if err != nil {
			return invalid("the metadata is not usable: %v", err)
		}
		in.Issuer, in.SAMLSSOURL, in.SAMLCerts = entity, sso, certs
	}
	in.Issuer = strings.TrimSpace(in.Issuer)
	if in.Issuer == "" {
		return invalid("a SAML provider needs the identity provider's entity id")
	}
	u, err := url.Parse(strings.TrimSpace(in.SAMLSSOURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return invalid("the sign-on address is where the identity provider asks people to sign in (https://...)")
	}
	in.SAMLSSOURL = strings.TrimSpace(in.SAMLSSOURL)
	if len(in.SAMLCerts) == 0 {
		return invalid("a SAML provider needs the identity provider's signing certificate")
	}
	var norm []string
	for _, c := range in.SAMLCerts {
		cs, err := saml.ParseCerts(c)
		if err != nil {
			return invalid("a certificate could not be read: %v", err)
		}
		for _, x := range cs {
			if x.PublicKeyAlgorithm != x509.RSA {
				return invalid("only RSA signing certificates are supported")
			}
			if !x.NotAfter.After(s.Now()) {
				return invalid("the certificate for %s expired on %s", x.Subject.CommonName, x.NotAfter.Format("2006-01-02"))
			}
			norm = append(norm, "-----BEGIN CERTIFICATE-----\n"+base64.StdEncoding.EncodeToString(x.Raw)+"\n-----END CERTIFICATE-----\n")
		}
	}
	in.SAMLCerts = norm
	in.ClientID = strings.TrimSpace(in.ClientID)
	if in.ClientID == "" {
		in.ClientID = s.samlMetadataURL(in.Name) // our entity id
	}
	in.Scopes, in.SAMLEmailAttr, in.SAMLNameAttr = "openid email profile", strings.TrimSpace(in.SAMLEmailAttr), strings.TrimSpace(in.SAMLNameAttr)
	return nil
}

// SAMLMetadata is the document the identity provider's administrator needs: our entity id and where assertions go.
func (s *Service) SAMLMetadata(ctx context.Context, name string) (string, error) {
	var client string
	if err := s.DB.QueryRow(ctx, `SELECT client_id FROM sso_provider WHERE lower(name)=lower($1) AND protocol='saml'`, name).Scan(&client); err != nil {
		return "", ErrNotFound
	}
	return saml.SPMetadata(client, s.samlACS()), nil
}

// samlStart records the request and returns the identity provider's address with it attached.
func (s *Service) samlStart(ctx context.Context, p SSOProvider, redirect string) (string, error) {
	id, state := s.samlRequestID(), randString(16)
	req, err := saml.AuthnRequest(id, s.Now(), p.SAMLSSOURL, p.ClientID, s.samlACS())
	if err != nil {
		return "", err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO sso_state(state,provider_id,nonce,verifier,redirect,expires_at) VALUES($1,$2,$3,'',$4,$5)`, state, p.ID, id, safeRedirect(redirect), s.Now().Add(10*time.Minute)); err != nil {
		return "", err
	}
	s.DB.Exec(ctx, `DELETE FROM sso_state WHERE expires_at < $1`, s.Now())
	return saml.RedirectURL(p.SAMLSSOURL, req, state), nil
}

var samlEmailAttrs = []string{"email", "mail", "emailaddress", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress", "urn:oid:0.9.2342.19200300.100.1.3"}
var samlNameAttrs = []string{"displayname", "name", "cn", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name", "urn:oid:2.16.840.1.113730.3.1.241"}

func firstAttr(attrs map[string][]string, wanted string, fallbacks []string) string {
	lower := map[string]string{}
	for k, v := range attrs {
		if len(v) > 0 && strings.TrimSpace(v[0]) != "" {
			lower[strings.ToLower(k)] = strings.TrimSpace(v[0])
		}
	}
	for _, k := range append([]string{wanted}, fallbacks...) {
		if k == "" {
			continue
		}
		if v, ok := lower[strings.ToLower(k)]; ok {
			return v
		}
	}
	return ""
}

// SAMLCallback finishes a SAML sign-in: the relay state names the request that was sent (and is used once), the response is verified against
// that request and the provider's certificates, the assertion is accepted once, and the person is matched to an account.
func (s *Service) SAMLCallback(ctx context.Context, relay, response string, ttl time.Duration, meta ClientMeta) (SSOResult, error) {
	var res SSOResult
	var pid int64
	var reqID, redirect string
	var exp time.Time
	err := s.DB.QueryRow(ctx, `DELETE FROM sso_state WHERE state=$1 RETURNING provider_id,nonce,redirect,expires_at`, relay).Scan(&pid, &reqID, &redirect, &exp)
	if err != nil || !exp.After(s.Now()) {
		return res, ErrForbidden
	}
	res.Redirect = redirect
	var p SSOProvider
	var jit bool
	var certs []string
	err = s.DB.QueryRow(ctx, `SELECT id,name,issuer,client_id,allowed_domains,jit,saml_certs,saml_email_attr,saml_name_attr FROM sso_provider WHERE id=$1 AND enabled AND protocol='saml'`, pid).
		Scan(&p.ID, &p.Name, &p.Issuer, &p.ClientID, &p.AllowedDomains, &jit, &certs, &p.SAMLEmailAttr, &p.SAMLNameAttr)
	if err != nil {
		return res, ErrForbidden
	}
	if len(response) > 3<<19 { // base64 of the largest document the parser takes
		return res, ErrForbidden
	}
	doc, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(response), ""))
	if err != nil {
		return res, ErrForbidden
	}
	var cs []*x509.Certificate
	for _, c := range certs {
		x, err := saml.ParseCerts(c)
		if err != nil {
			continue
		}
		cs = append(cs, x...)
	}
	a, err := saml.ParseResponse(doc, saml.Expect{IdP: p.Issuer, SP: p.ClientID, ACS: s.samlACS(), RequestID: reqID, Certs: cs, Now: s.Now(), Skew: 2 * time.Minute})
	if err != nil {
		s.DB.Exec(ctx, `INSERT INTO audit_event(actor_label,action,target,detail) VALUES('SAML','security.saml_refused',$1,$2)`, p.Name, jsonMarshal(map[string]any{"reason": err.Error(), "ip": meta.IP}))
		return res, ErrForbidden // the reason is kept for the administrator, never shown to the person
	}
	// an assertion is accepted once
	exp = a.Expires
	if exp.Before(s.Now()) {
		exp = s.Now().Add(10 * time.Minute)
	}
	tag, err := s.DB.Exec(ctx, `INSERT INTO saml_assertion(provider_id,assertion_id,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, p.ID, a.ID, exp.Add(time.Hour))
	if err != nil || tag.RowsAffected() == 0 {
		return res, ErrForbidden
	}
	s.DB.Exec(ctx, `DELETE FROM saml_assertion WHERE expires_at < $1`, s.Now())
	email := firstAttr(a.Attrs, p.SAMLEmailAttr, samlEmailAttrs)
	if email == "" && strings.Contains(a.NameID, "@") {
		email = a.NameID
	}
	name := firstAttr(a.Attrs, p.SAMLNameAttr, samlNameAttrs)
	return s.ssoSignIn(ctx, res, p, jit, a.NameID, email, name, true, ttl, meta) // the provider vouches for its own people's addresses
}
