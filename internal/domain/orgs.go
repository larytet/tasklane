package domain

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Organizations (SPEC 2, 17.4, SEC-09). An installation starts with one, "Default", and behaves as a single-organization
// install until a second is created. A person and a project each belong to exactly one organization and nothing crosses
// the line: membership, group roles, search, lists and every project-scoped path are bounded by it (the membership view
// joins a person only to projects of their own organization). The instance administrator creates organizations and sees
// all of them, as the operator; an organization administrator administers their own organization only.

type Organization struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	Users    int       `json:"users"`
	Projects int       `json:"projects"`
}

// CreateOrganization makes an organization and its first administrator, who belongs to it and is not an instance administrator.
func (s *Service) CreateOrganization(ctx context.Context, a Actor, name, adminEmail, adminName, adminPassword string) (Organization, error) {
	if err := adminOnly(a); err != nil {
		return Organization{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 120 {
		return Organization{}, invalid("an organization needs a name of up to 120 characters")
	}
	var o Organization
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO organization(name) VALUES($1) RETURNING id,name,created_at`, name).Scan(&o.ID, &o.Name, &o.Created); err != nil {
			return invalid("an organization with that name exists")
		}
		return s.audit(ctx, tx, a, "org.create", o.ID, map[string]any{"name": name})
	})
	if err != nil {
		return Organization{}, err
	}
	if _, err := s.createUser(ctx, adminEmail, adminName, adminPassword, true, o.ID, false); err != nil {
		s.DB.Exec(ctx, `DELETE FROM organization WHERE id::text=$1`, o.ID) // no organization without someone to run it
		return Organization{}, err
	}
	o.Users = 1
	return o, nil
}

func (s *Service) ListOrganizations(ctx context.Context, a Actor) ([]Organization, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT o.id,o.name,o.created_at,
		(SELECT count(*) FROM app_user u WHERE u.org_id=o.id AND u.active),
		(SELECT count(*) FROM project p WHERE p.org_id=o.id) FROM organization o ORDER BY o.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Organization{}
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.Created, &o.Users, &o.Projects); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MyOrganization is the organization the actor belongs to.
func (s *Service) MyOrganization(ctx context.Context, a Actor) (Organization, error) {
	var o Organization
	err := s.DB.QueryRow(ctx, `SELECT id,name,created_at FROM organization WHERE id::text=$1`, a.OrgID()).Scan(&o.ID, &o.Name, &o.Created)
	return o, err
}

// ActorFor is the actor for a person, as authentication would build it (used by tests and by jobs acting for a person).
func (s *Service) ActorFor(ctx context.Context, userID string) (Actor, error) {
	return s.actorFor(ctx, userID)
}
