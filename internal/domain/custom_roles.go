package domain

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Custom roles (SPEC 17.2). A role is a base, which is how the rest of the system sees the person (a viewer reads, a member also
// writes), plus extra permissions it grants among those a permission scheme can set. "Reporter": a viewer who may also create
// issues and comment. "Triager": a viewer who may comment and move issues along. A person with several grants (their own and
// their groups') has all of them, and the highest base. Roles belong to an organization and are managed by its administrators.

type CustomRole struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Base        string    `json:"base"`
	Grants      []string  `json:"grants"`
	Description string    `json:"description"`
	InUse       int       `json:"in_use"`
	Created     time.Time `json:"created_at"`
}

type CustomRoleIn struct {
	Name        string   `json:"name"`
	Base        string   `json:"base"`
	Grants      []string `json:"grants"`
	Description string   `json:"description"`
}

func validPerm(p string) bool { _, ok := permDefaults[p]; return ok }

func (in *CustomRoleIn) clean() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 60 {
		return invalid("a role needs a name of up to 60 characters")
	}
	switch strings.ToLower(in.Name) {
	case "viewer", "member", "admin", "administrator":
		return invalid("%q is a built-in role", in.Name)
	}
	if in.Base != "viewer" && in.Base != "member" {
		return invalid("the base of a role is viewer or member")
	}
	if len(in.Description) > 300 {
		return invalid("the description is at most 300 characters")
	}
	seen := map[string]bool{}
	var gs []string
	for _, g := range in.Grants {
		g = strings.TrimSpace(g)
		if !validPerm(g) {
			return invalid("%q is not a permission a role can grant (%s)", g, strings.Join(permOrder, ", "))
		}
		if !seen[g] {
			seen[g] = true
			gs = append(gs, g)
		}
	}
	sort.Strings(gs)
	if gs == nil {
		gs = []string{}
	}
	in.Grants = gs
	return nil
}

func (s *Service) roleAdmin(a Actor) error {
	if !a.Admin && !a.OrgAdmin {
		return ErrForbidden
	}
	return nil
}

// ListCustomRoles lists the roles of the person's organization (anyone signed in may see them: the names are what a project
// administrator picks from).
func (s *Service) ListCustomRoles(ctx context.Context, a Actor) ([]CustomRole, error) {
	rows, err := s.DB.Query(ctx, `SELECT r.id::text,r.name,r.base,r.grants,r.description,r.created_at,
		(SELECT count(*) FROM project_member m WHERE m.custom_role_id=r.id)+(SELECT count(*) FROM project_group_role g WHERE g.custom_role_id=r.id)
		FROM custom_role r WHERE r.org_id::text=$1 ORDER BY lower(r.name)`, a.OrgID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CustomRole{}
	for rows.Next() {
		var r CustomRole
		if err := rows.Scan(&r.ID, &r.Name, &r.Base, &r.Grants, &r.Description, &r.Created, &r.InUse); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) CreateCustomRole(ctx context.Context, a Actor, in CustomRoleIn) (CustomRole, error) {
	if err := s.roleAdmin(a); err != nil {
		return CustomRole{}, err
	}
	if err := in.clean(); err != nil {
		return CustomRole{}, err
	}
	r := CustomRole{Name: in.Name, Base: in.Base, Grants: in.Grants, Description: in.Description}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO custom_role(org_id,name,base,grants,description) VALUES($1,$2,$3,$4,$5) RETURNING id::text,created_at`, a.OrgID(), in.Name, in.Base, in.Grants, in.Description).Scan(&r.ID, &r.Created); err != nil {
			if strings.Contains(err.Error(), "custom_role_name_uq") {
				return invalid("a role called %q exists", in.Name)
			}
			return err
		}
		return s.audit(ctx, tx, a, "role.created", r.ID, map[string]any{"name": in.Name, "base": in.Base, "grants": in.Grants})
	})
	return r, err
}

// UpdateCustomRole changes a role. Everyone who has it is affected at once, which is the point of a role; the base may be changed
// too, so it is audited with what it was.
func (s *Service) UpdateCustomRole(ctx context.Context, a Actor, id string, in CustomRoleIn) error {
	if err := s.roleAdmin(a); err != nil {
		return err
	}
	if err := in.clean(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var oldBase string
		var oldGrants []string
		if err := tx.QueryRow(ctx, `SELECT base,grants FROM custom_role WHERE id::text=$1 AND org_id::text=$2 FOR UPDATE`, id, a.OrgID()).Scan(&oldBase, &oldGrants); err != nil {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE custom_role SET name=$2,base=$3,grants=$4,description=$5 WHERE id::text=$1`, id, in.Name, in.Base, in.Grants, in.Description); err != nil {
			if strings.Contains(err.Error(), "custom_role_name_uq") {
				return invalid("a role called %q exists", in.Name)
			}
			return err
		}
		// the members' stored base follows the role's, so the permission engine, which reads the base, stays in step
		if _, err := tx.Exec(ctx, `UPDATE project_member SET role=$2 WHERE custom_role_id::text=$1`, id, in.Base); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE project_group_role SET role=$2 WHERE custom_role_id::text=$1`, id, in.Base); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "role.updated", id, map[string]any{"name": in.Name, "base": in.Base, "grants": in.Grants, "was_base": oldBase, "was_grants": oldGrants})
	})
}

// DeleteCustomRole removes a role nobody has: deleting one that is in use would silently change what people may do.
func (s *Service) DeleteCustomRole(ctx context.Context, a Actor, id string) error {
	if err := s.roleAdmin(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM project_member WHERE custom_role_id::text=$1)+(SELECT count(*) FROM project_group_role WHERE custom_role_id::text=$1)`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return &ConflictError{Msg: "the role is still given to people or groups in projects; change those first"}
		}
		var name string
		if err := tx.QueryRow(ctx, `DELETE FROM custom_role WHERE id::text=$1 AND org_id::text=$2 RETURNING name`, id, a.OrgID()).Scan(&name); err != nil {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "role.deleted", id, map[string]any{"name": name})
	})
}

// resolveRole turns what an administrator typed in a project (viewer, member, admin or the name of a custom role of the project's
// organization) into the stored base and the custom role it came from (empty for a built-in).
func (s *Service) resolveRole(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, projectID, name string) (base, custom string, err error) {
	switch name {
	case "viewer", "member", "admin":
		return name, "", nil
	}
	err = q.QueryRow(ctx, `SELECT r.base,r.id::text FROM custom_role r JOIN project p ON p.org_id=r.org_id WHERE p.id::text=$1 AND lower(r.name)=lower($2)`, projectID, name).Scan(&base, &custom)
	if err != nil {
		return "", "", invalid("a role is viewer, member, admin or one of the organization's custom roles")
	}
	return base, custom, nil
}

// extraGrants are the permissions custom roles give a person in a project, beyond what the base role's rank gives.
func (s *Service) extraGrants(ctx context.Context, userID, projectID string) []string {
	var gs []string
	s.DB.QueryRow(ctx, `SELECT grants FROM effective_member WHERE project_id::text=$1 AND user_id::text=$2`, projectID, userID).Scan(&gs)
	return gs
}
