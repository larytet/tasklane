package domain

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Customer organizations and shared requests (SPEC 15.1). A customer organization is a company: a name and the customers who belong to
// it (staff of the project's portal manage them). A request is seen by the customer who raised it and by two kinds of other customers,
// only when its owner chooses: people they add to that one request, and everyone in their customer organization. Either way the others
// see that request and its public conversation and nothing else of the owner's.

type CustomerOrg struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// ListCustomerOrgs lists the customer organizations of the portal's organization (project administrators of a portal).
func (s *Service) ListCustomerOrgs(ctx context.Context, a Actor, projectKey string) ([]CustomerOrg, error) {
	if _, err := s.GetPortalPolicy(ctx, a, projectKey); err != nil {
		return nil, err
	}
	pid, _ := s.projectID(ctx, projectKey)
	rows, err := s.DB.Query(ctx, `SELECT o.id::text,o.name,COALESCE(array_agg(c.email ORDER BY c.email) FILTER (WHERE c.id IS NOT NULL), '{}')
		FROM customer_org o LEFT JOIN customer c ON c.customer_org_id=o.id WHERE o.org_id=(SELECT org_id FROM project WHERE id::text=$1) GROUP BY o.id,o.name ORDER BY lower(o.name)`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CustomerOrg{}
	for rows.Next() {
		var o CustomerOrg
		if err := rows.Scan(&o.ID, &o.Name, &o.Members); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Service) CreateCustomerOrg(ctx context.Context, a Actor, projectKey, name string) (CustomerOrg, error) {
	if _, err := s.GetPortalPolicy(ctx, a, projectKey); err != nil {
		return CustomerOrg{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 100 {
		return CustomerOrg{}, invalid("a customer organization needs a name of at most 100 characters")
	}
	pid, _ := s.projectID(ctx, projectKey)
	var o CustomerOrg
	err := s.DB.QueryRow(ctx, `INSERT INTO customer_org(org_id,name) VALUES((SELECT org_id FROM project WHERE id::text=$1),$2) RETURNING id::text,name`, pid, name).Scan(&o.ID, &o.Name)
	if err != nil && strings.Contains(err.Error(), "customer_org_name_uq") {
		return o, invalid("there is already a customer organization with that name")
	}
	o.Members = []string{}
	return o, err
}

func (s *Service) customerOrgOf(ctx context.Context, a Actor, projectKey, id string) (string, error) {
	if _, err := s.GetPortalPolicy(ctx, a, projectKey); err != nil {
		return "", err
	}
	pid, _ := s.projectID(ctx, projectKey)
	var org string
	if err := s.DB.QueryRow(ctx, `SELECT o.org_id::text FROM customer_org o WHERE o.id::text=$1 AND o.org_id=(SELECT org_id FROM project WHERE id::text=$2)`, id, pid).Scan(&org); err != nil {
		return "", ErrNotFound // also another tenant's, and a malformed id
	}
	return org, nil
}

func (s *Service) DeleteCustomerOrg(ctx context.Context, a Actor, projectKey, id string) error {
	if _, err := s.customerOrgOf(ctx, a, projectKey, id); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, `DELETE FROM customer_org WHERE id::text=$1`, id) // members stay as customers, with no organization
	return err
}

// SetCustomerOrgMembers adds and removes members by address. A person who has never signed in gets their record made now (unverified,
// and subject to the portal's allow and block lists); removing someone takes their shared access away at once.
func (s *Service) SetCustomerOrgMembers(ctx context.Context, a Actor, projectKey, id string, add, remove []string) (CustomerOrg, error) {
	org, err := s.customerOrgOf(ctx, a, projectKey, id)
	if err != nil {
		return CustomerOrg{}, err
	}
	_, _, set, _, err := s.portalOf(ctx, projectKey)
	if err != nil {
		set = PortalPolicy{} // a closed portal may still have its companies set up
	}
	if len(add)+len(remove) > 200 {
		return CustomerOrg{}, invalid("change at most 200 members at a time")
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		for _, e := range add {
			addr, err := normEmail(e)
			if err != nil {
				return err
			}
			if !set.permits(addr) {
				return invalid("%s cannot use this portal (it is blocked or not on the allow list)", addr)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO customer(org_id,email,customer_org_id) VALUES($1,$2,$3) ON CONFLICT (org_id, lower(email)) DO UPDATE SET customer_org_id=$3`, org, addr, id); err != nil {
				return err
			}
		}
		for _, e := range remove {
			addr, err := normEmail(e)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE customer SET customer_org_id=NULL WHERE org_id::text=$1 AND lower(email)=$2 AND customer_org_id::text=$3`, org, addr, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return CustomerOrg{}, err
	}
	all, err := s.ListCustomerOrgs(ctx, a, projectKey)
	if err != nil {
		return CustomerOrg{}, err
	}
	for _, o := range all {
		if o.ID == id {
			return o, nil
		}
	}
	return CustomerOrg{}, ErrNotFound
}

// ---- sharing a request ----

type PortalSharing struct {
	CanShareWithOrg bool        `json:"can_share_with_organization"` // the owner belongs to a customer organization
	Organization    string      `json:"organization,omitempty"`      // its name
	SharedWithOrg   bool        `json:"shared_with_organization"`
	People          []PortalWho `json:"people"`
}

type PortalWho struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

func (s *Service) sharingOf(ctx context.Context, issueID string, c Customer, sharedOrg bool) *PortalSharing {
	sh := &PortalSharing{SharedWithOrg: sharedOrg, People: []PortalWho{}, Organization: c.Org, CanShareWithOrg: c.Org != ""}
	rows, err := s.DB.Query(ctx, `SELECT cu.email,cu.name FROM request_participant rp JOIN customer cu ON cu.id=rp.customer_id WHERE rp.issue_id::text=$1 ORDER BY cu.email`, issueID)
	if err != nil {
		return sh
	}
	defer rows.Close()
	for rows.Next() {
		var w PortalWho
		rows.Scan(&w.Email, &w.Name)
		sh.People = append(sh.People, w)
	}
	return sh
}

type PortalShareIn struct {
	Organization *bool    `json:"organization"` // share with (true) or stop sharing with (false) the customer organization
	Add          []string `json:"add"`
	Remove       []string `json:"remove"`
}

// PortalShare changes who else sees a request. Only the person who raised it may; people added must be allowed to use the portal.
func (s *Service) PortalShare(ctx context.Context, c Customer, projectKey, key string, in PortalShareIn) (PortalRequest, error) {
	r, err := s.PortalGetRequest(ctx, c, projectKey, key)
	if err != nil {
		return r, err
	}
	if !r.Mine {
		return r, ErrForbidden
	}
	_, _, set, _, err := s.portalOf(ctx, projectKey)
	if err != nil {
		return r, err
	}
	if in.Organization != nil && *in.Organization && c.Org == "" {
		return r, invalid("you do not belong to a customer organization")
	}
	if len(in.Add) > 20 || len(in.Remove) > 20 {
		return r, invalid("change at most 20 people at a time")
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var iid string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM issue WHERE key=$1`, r.Key).Scan(&iid); err != nil {
			return err
		}
		if in.Organization != nil {
			if _, err := tx.Exec(ctx, `UPDATE portal_request SET shared_with_org=$2 WHERE issue_id::text=$1`, iid, *in.Organization); err != nil {
				return err
			}
		}
		for _, e := range in.Add {
			addr, err := normEmail(e)
			if err != nil {
				return err
			}
			if addr == strings.ToLower(c.Email) {
				continue
			}
			if !set.permits(addr) {
				return invalid("%s cannot use this portal", addr)
			}
			var cid string
			if err := tx.QueryRow(ctx, `INSERT INTO customer(org_id,email) VALUES($1,$2) ON CONFLICT (org_id, lower(email)) DO UPDATE SET email=customer.email RETURNING id::text`, c.OrgID, addr).Scan(&cid); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO request_participant(issue_id,customer_id,added_by) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, iid, cid, c.ID); err != nil {
				return err
			}
		}
		for _, e := range in.Remove {
			addr, err := normEmail(e)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM request_participant WHERE issue_id::text=$1 AND customer_id IN (SELECT id FROM customer WHERE org_id::text=$2 AND lower(email)=$3)`, iid, c.OrgID, addr); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	return s.PortalGetRequest(ctx, c, projectKey, key)
}
