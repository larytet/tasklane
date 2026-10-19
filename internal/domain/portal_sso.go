package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Customer sign-in through an identity provider (SPEC 15: "optional sign-in through a customer identity provider (OIDC or SAML) configured per
// portal"). A portal has at most one provider, made and kept by an administrator of its project. It uses the same OpenID Connect and SAML code as
// the staff sign-in, and differs where it matters: it ends in a customer session (the portal cookie, nothing of the staff side), it never makes or
// matches a staff account, and it must name the address domains it speaks for. That last rule is the safety of the feature: a provider's own
// administrator decides what addresses it vouches for, so without a domain list the provider of one company could sign in as another company's
// customer in the same portal.

func (s *Service) portalAdminProject(ctx context.Context, a Actor, projectKey string) (string, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return "", ErrNotFound
	}
	switch s.role(ctx, a, pid) {
	case "":
		return "", ErrNotFound
	case "admin":
		return pid, nil
	}
	return "", ErrForbidden
}

func (s *Service) portalProviderID(ctx context.Context, pid string) int64 {
	var id int64
	s.DB.QueryRow(ctx, `SELECT id FROM sso_provider WHERE portal_project_id::text=$1`, pid).Scan(&id)
	return id
}

// GetPortalSSO returns the portal's provider, if it has one.
func (s *Service) GetPortalSSO(ctx context.Context, a Actor, projectKey string) (*SSOProvider, error) {
	pid, err := s.portalAdminProject(ctx, a, projectKey)
	if err != nil {
		return nil, err
	}
	id := s.portalProviderID(ctx, pid)
	if id == 0 {
		return nil, nil
	}
	p, err := s.loadSSOProvider(ctx, id)
	return &p, err
}

// SavePortalSSO makes or replaces the portal's provider.
func (s *Service) SavePortalSSO(ctx context.Context, a Actor, projectKey string, in SSOProvider) (SSOProvider, error) {
	pid, err := s.portalAdminProject(ctx, a, projectKey)
	if err != nil {
		return in, err
	}
	in.Name = "portal-" + strings.ToLower(projectKey)
	in.JIT = false // customers are made at their first sign-in either way; this switch is the staff one
	domains := 0
	for _, d := range in.AllowedDomains {
		if strings.TrimSpace(strings.TrimPrefix(d, "@")) != "" {
			domains++
		}
	}
	if domains == 0 {
		return in, invalid("name the address domains this provider signs in (such as example.com): it may only vouch for its own people")
	}
	return s.saveSSOProvider(ctx, a, s.portalProviderID(ctx, pid), in, pid)
}

// DeletePortalSSO removes it; customers sign in with a code again.
func (s *Service) DeletePortalSSO(ctx context.Context, a Actor, projectKey string) error {
	pid, err := s.portalAdminProject(ctx, a, projectKey)
	if err != nil {
		return err
	}
	id := s.portalProviderID(ctx, pid)
	if id == 0 {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM sso_provider WHERE id=$1`, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "sso.provider_deleted", fmt.Sprint(id), map[string]any{"portal": strings.ToUpper(projectKey)})
	})
}

// portalSSOChoice is the button the portal's sign-in page shows.
func (s *Service) portalSSOChoice(ctx context.Context, pid string) *PortalSSOChoice {
	var c PortalSSOChoice
	if s.DB.QueryRow(ctx, `SELECT name,label FROM sso_provider WHERE portal_project_id::text=$1 AND enabled`, pid).Scan(&c.Name, &c.Label) != nil {
		return nil
	}
	return &c
}

type PortalSSOChoice struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// portalSSOSignIn ends a sign-in through a portal's provider: the provider has vouched for an address; it must be one of the domains the provider
// was configured for and one the portal permits, and the customer is opened as by a code.
func (s *Service) portalSSOSignIn(ctx context.Context, res SSOResult, p SSOProvider, email string, meta ClientMeta) (SSOResult, error) {
	res.Redirect = "/portal/" + p.PortalKey
	dom := email[strings.LastIndex(email, "@")+1:]
	ok := false
	for _, d := range p.AllowedDomains {
		ok = ok || d == dom
	}
	if !ok { // a portal provider with no domain list vouches for nobody
		return res, s.portalSSORefused(ctx, p, email, "domain", meta)
	}
	org, _, set, _, err := s.portalOf(ctx, p.PortalKey)
	if err != nil {
		return res, ErrForbidden
	}
	addr, err := normEmail(email)
	if err != nil || !set.permits(addr) {
		return res, s.portalSSORefused(ctx, p, email, "portal rules", meta)
	}
	var tok string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var e error
		if tok, _, e = s.openCustomer(ctx, tx, org, addr); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO audit_event(actor_label,action,target,detail) VALUES($1,'security.portal_sso_login',$2,$3)`, addr+" (customer)", p.PortalKey, jsonMarshal(map[string]any{"provider": p.Name, "ip": meta.IP}))
		return e
	})
	if err != nil {
		return res, ErrForbidden
	}
	res.PortalToken = tok
	return res, nil
}

func (s *Service) portalSSORefused(ctx context.Context, p SSOProvider, email, why string, meta ClientMeta) error {
	s.DB.Exec(ctx, `INSERT INTO audit_event(actor_label,action,target,detail) VALUES('portal sign-in','security.portal_sso_refused',$1,$2)`, p.PortalKey, jsonMarshal(map[string]any{"provider": p.Name, "reason": why, "ip": meta.IP}))
	return ErrForbidden
}
