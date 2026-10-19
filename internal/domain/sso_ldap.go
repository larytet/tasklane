package domain

import (
	"context"
	"crypto/x509"
	"errors"
	"log"
	"net"
	"strings"

	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/ldap"
)

// LDAP sign-in: an LDAP provider is a directory (Active Directory, OpenLDAP) that the ordinary sign-in form checks a password
// against when no local password matches. The person is found by the search account, the password is checked by binding as
// them, and the account is matched or created exactly as single sign-on does (subject, then address; allowed domains; accounts
// at first sign-in). A second factor, the network rules and the lockout apply as they do to a local password.

// prepareLDAP checks and normalises an LDAP provider before it is stored.
func (s *Service) prepareLDAP(in *SSOProvider, isNew bool) error {
	var urls []string
	for _, f := range strings.FieldsFunc(in.Issuer, func(r rune) bool { return r == ' ' || r == ',' || r == '\n' || r == '\t' }) {
		u, err := ldap.CheckURL(f)
		if err != nil {
			return invalid("%v", err)
		}
		if u.Scheme == "ldap" && !in.LDAPStartTLS && !isLoopbackHost(u.Hostname()) {
			return invalid("%s would carry passwords unencrypted: use ldaps:// or turn on StartTLS", f)
		}
		urls = append(urls, u.Scheme+"://"+u.Host)
	}
	if len(urls) == 0 {
		return invalid("an LDAP provider needs a server address, such as ldaps://dc1.example.com")
	}
	in.Issuer = strings.Join(urls, " ")
	in.LDAPBaseDN = strings.TrimSpace(in.LDAPBaseDN)
	if in.LDAPBaseDN == "" {
		return invalid("an LDAP provider needs the base DN to search under, such as dc=example,dc=com")
	}
	in.LDAPBindDN = strings.TrimSpace(in.LDAPBindDN)
	if in.LDAPBindDN != "" && in.ClientSecret == "" && isNew {
		return invalid("the search account needs its password")
	}
	in.LDAPUserFilter = strings.TrimSpace(in.LDAPUserFilter)
	if in.LDAPUserFilter == "" {
		in.LDAPUserFilter = ldap.DefaultUserFilter
	}
	if err := ldap.CheckUserFilter(in.LDAPUserFilter); err != nil {
		return invalid("%v", err)
	}
	if strings.TrimSpace(in.LDAPCAPEM) != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(in.LDAPCAPEM)) {
		return invalid("the CA certificate is not a PEM certificate")
	}
	in.LDAPEmailAttr, in.LDAPNameAttr = strings.TrimSpace(in.LDAPEmailAttr), strings.TrimSpace(in.LDAPNameAttr)
	in.ClientID, in.Scopes = "", ""
	return nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

type ldapProvider struct {
	p   SSOProvider
	cfg ldap.Config
}

// ldapProviders are the enabled directories, in name order, with their search password unsealed.
func (s *Service) ldapProviders(ctx context.Context, id int64) ([]ldapProvider, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+ssoProviderCols+`,client_secret_enc FROM sso_provider WHERE protocol='ldap' AND portal_project_id IS NULL AND (enabled OR id=$1) AND ($1=0 OR id=$1) ORDER BY name`, id)
	if err != nil {
		return nil, err
	}
	type row struct {
		p   SSOProvider
		enc []byte
	}
	var rs []row
	for rows.Next() {
		var r row
		var ps SSOProvider
		if err := rows.Scan(&ps.ID, &ps.Name, &ps.Label, &ps.Issuer, &ps.ClientID, &ps.SecretSet, &ps.Scopes, &ps.AllowedDomains, &ps.JIT, &ps.Enabled, &ps.Protocol, &ps.SAMLSSOURL, &ps.SAMLCerts, &ps.SAMLEmailAttr, &ps.SAMLNameAttr, &ps.PortalKey,
			&ps.LDAPBindDN, &ps.LDAPBaseDN, &ps.LDAPUserFilter, &ps.LDAPStartTLS, &ps.LDAPSkipVerify, &ps.LDAPCAPEM, &ps.LDAPEmailAttr, &ps.LDAPNameAttr, &r.enc); err != nil {
			rows.Close()
			return nil, err
		}
		r.p = ps
		rs = append(rs, r)
	}
	rows.Close() // read completely before unsealing, which queries the same pool
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ldapProvider
	for _, r := range rs {
		cfg := ldap.Config{URLs: strings.Fields(r.p.Issuer), StartTLS: r.p.LDAPStartTLS, InsecureSkipVerify: r.p.LDAPSkipVerify, BindDN: r.p.LDAPBindDN,
			BaseDN: r.p.LDAPBaseDN, UserFilter: r.p.LDAPUserFilter, EmailAttr: r.p.LDAPEmailAttr, NameAttr: r.p.LDAPNameAttr, AllowPrivate: s.SSOAllowPrivate}
		if s.SSOEgress != nil {
			cfg.AllowPrivate = s.SSOEgress(egress.Policy{AllowPrivate: cfg.AllowPrivate}).AllowPrivate
		}
		if r.p.LDAPCAPEM != "" {
			cfg.RootCAs = x509.NewCertPool()
			cfg.RootCAs.AppendCertsFromPEM([]byte(r.p.LDAPCAPEM))
		}
		if len(r.enc) > 0 {
			pw, err := s.unseal(ctx, r.enc)
			if err != nil {
				return nil, err
			}
			cfg.BindPassword = pw
		}
		out = append(out, ldapProvider{p: r.p, cfg: cfg})
	}
	return out, nil
}

// ldapSignIn tries the password against each enabled directory. It returns the account the first directory that knows the person
// leads to; ErrForbidden when no directory accepted the password or the account may not sign in.
func (s *Service) ldapSignIn(ctx context.Context, login, password string, meta ClientMeta) (string, error) {
	provs, err := s.ldapProviders(ctx, 0)
	if err != nil || len(provs) == 0 {
		return "", ErrForbidden
	}
	for _, lp := range provs {
		e, err := ldap.Authenticate(ctx, lp.cfg, login, password)
		if errors.Is(err, ldap.ErrInvalidCredentials) {
			continue
		}
		if err != nil {
			log.Printf("ldap provider %s: %v", lp.p.Name, err) // the person is told sign-in failed; the operator needs the reason
			continue
		}
		email := strings.ToLower(strings.TrimSpace(e.Email))
		if !strings.Contains(email, "@") {
			log.Printf("ldap provider %s: %s has no address in %q, so it cannot be matched to an account", lp.p.Name, e.DN, firstNonEmpty(lp.p.LDAPEmailAttr, "mail"))
			return "", ErrForbidden
		}
		uid, _, err := s.ssoAccount(ctx, lp.p, lp.p.JIT, e.Subject, email, e.Name, meta)
		return uid, err
	}
	return "", ErrForbidden
}

// LDAPCheck is the administrator's test of a directory: it signs nobody in, and says what a sign-in with these credentials
// would find (or why it fails), so a provider can be set up without trial and error on the sign-in page.
func (s *Service) LDAPCheck(ctx context.Context, a Actor, id int64, login, password string) (map[string]any, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	provs, err := s.ldapProviders(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(provs) != 1 || id == 0 {
		return nil, ErrNotFound
	}
	out := map[string]any{"ok": false}
	e, err := ldap.Authenticate(ctx, provs[0].cfg, login, password)
	if err != nil {
		out["error"] = err.Error()
		return out, nil
	}
	out["dn"], out["subject"], out["email"], out["name"] = e.DN, e.Subject, e.Email, e.Name
	email := strings.ToLower(strings.TrimSpace(e.Email))
	var existing string
	s.DB.QueryRow(ctx, `SELECT email FROM app_user WHERE email=$1`, email).Scan(&existing)
	allowed := len(provs[0].p.AllowedDomains) == 0
	for _, d := range provs[0].p.AllowedDomains {
		allowed = allowed || strings.HasSuffix(email, "@"+d)
	}
	switch {
	case !strings.Contains(email, "@"):
		out["error"] = "the entry has no email address in " + firstNonEmpty(provs[0].p.LDAPEmailAttr, "mail")
	case !allowed:
		out["error"] = "the address " + email + " is not in the provider's allowed domains"
	case existing != "":
		out["ok"], out["account"] = true, "existing"
	case provs[0].p.JIT:
		out["ok"], out["account"] = true, "created at first sign-in"
	default:
		out["error"] = "no account has this address and accounts are not created at first sign-in"
	}
	return out, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
