package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Groups and permission schemes (SPEC 17.2).

type Group struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Members []User      `json:"members,omitempty"`
	Count   int         `json:"count"`
	Roles   []GroupRole `json:"projects,omitempty"`
}

type GroupRole struct {
	Project string `json:"project"`
	Role    string `json:"role"`
}

func (s *Service) CreateGroup(ctx context.Context, a Actor, name string) (Group, error) {
	if err := adminOnly(a); err != nil {
		return Group{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return Group{}, invalid("a group needs a name of up to 80 characters")
	}
	var g Group
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO user_group(name) VALUES($1) RETURNING id::text,name`, name).Scan(&g.ID, &g.Name); err != nil {
			if strings.Contains(err.Error(), "user_group_name_uq") {
				return invalid("a group named %q exists", name)
			}
			return err
		}
		return s.audit(ctx, tx, a, "group.created", g.ID, map[string]any{"name": name})
	})
	return g, err
}

func (s *Service) ListGroups(ctx context.Context, a Actor) ([]Group, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT g.id::text,g.name,(SELECT count(*) FROM user_group_member m WHERE m.group_id=g.id) FROM user_group g ORDER BY lower(g.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Count); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) GetGroup(ctx context.Context, a Actor, id string) (Group, error) {
	if err := adminOnly(a); err != nil {
		return Group{}, err
	}
	var g Group
	if err := s.DB.QueryRow(ctx, `SELECT id::text,name FROM user_group WHERE id::text=$1`, id).Scan(&g.ID, &g.Name); err != nil {
		return g, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT u.id::text,u.email,u.name,u.kind,u.is_admin FROM user_group_member m JOIN app_user u ON u.id=m.user_id WHERE m.group_id::text=$1 ORDER BY lower(u.name)`, id)
	if err != nil {
		return g, err
	}
	for rows.Next() {
		var u User
		rows.Scan(&u.ID, &u.Email, &u.Name, &u.Kind, &u.Admin)
		g.Members = append(g.Members, u)
	}
	rows.Close()
	g.Count = len(g.Members)
	pr, err := s.DB.Query(ctx, `SELECT p.key,r.role FROM project_group_role r JOIN project p ON p.id=r.project_id WHERE r.group_id::text=$1 ORDER BY p.key`, id)
	if err != nil {
		return g, err
	}
	defer pr.Close()
	for pr.Next() {
		var r GroupRole
		pr.Scan(&r.Project, &r.Role)
		g.Roles = append(g.Roles, r)
	}
	return g, pr.Err()
}

func (s *Service) DeleteGroup(ctx context.Context, a Actor, id string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM user_group WHERE id::text=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "group.deleted", id, map[string]any{})
	})
}

// AddGroupMember adds a person (by id or email) to a group.
func (s *Service) AddGroupMember(ctx context.Context, a Actor, groupID, who string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var uid string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE active AND (id::text=$1 OR lower(email)=lower($1))`, strings.TrimSpace(who)).Scan(&uid); err != nil {
			return invalid("%q is not an active user", who)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO user_group_member(group_id,user_id) SELECT id,$2::uuid FROM user_group WHERE id::text=$1 ON CONFLICT DO NOTHING`, groupID, uid)
		if err != nil {
			return err
		}
		var exists bool
		tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_group WHERE id::text=$1)`, groupID).Scan(&exists)
		if !exists {
			return ErrNotFound
		}
		if tag.RowsAffected() > 0 {
			return s.audit(ctx, tx, a, "group.member_added", groupID, map[string]any{"user": uid})
		}
		return nil
	})
}

func (s *Service) RemoveGroupMember(ctx context.Context, a Actor, groupID, userID string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM user_group_member WHERE group_id::text=$1 AND user_id::text=$2`, groupID, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "group.member_removed", groupID, map[string]any{"user": userID})
	})
}

// SetProjectGroup gives a group a role in a project (project administrators); an empty role removes it.
func (s *Service) SetProjectGroup(ctx context.Context, a Actor, projectKey, groupID, role string) error {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	if !s.projectAdmin(ctx, a, pid) {
		if s.role(ctx, a, pid) == "" {
			return ErrNotFound
		}
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		base, custom := role, ""
		if role != "" {
			var rerr error
			if base, custom, rerr = s.resolveRole(ctx, tx, pid, role); rerr != nil {
				return rerr
			}
		}
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM user_group WHERE id::text=$1`, groupID).Scan(&one); err != nil {
			return invalid("unknown group")
		}
		if role == "" {
			if _, err := tx.Exec(ctx, `DELETE FROM project_group_role WHERE project_id::text=$1 AND group_id::text=$2`, pid, groupID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `INSERT INTO project_group_role(project_id,group_id,role,custom_role_id) VALUES($1::uuid,$2::uuid,$3,$4) ON CONFLICT (project_id,group_id) DO UPDATE SET role=EXCLUDED.role, custom_role_id=EXCLUDED.custom_role_id`, pid, groupID, base, nilIfEmpty(custom)); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "project.group_role", strings.ToUpper(projectKey), map[string]any{"group": groupID, "role": role})
	})
}

func (s *Service) ProjectGroups(ctx context.Context, a Actor, projectKey string) ([]map[string]any, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	if !s.projectAdmin(ctx, a, pid) {
		if s.role(ctx, a, pid) == "" {
			return nil, ErrNotFound
		}
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT g.id::text,g.name,r.role,(SELECT count(*) FROM user_group_member m WHERE m.group_id=g.id) FROM project_group_role r JOIN user_group g ON g.id=r.group_id WHERE r.project_id::text=$1 ORDER BY lower(g.name)`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, role string
		var n int
		rows.Scan(&id, &name, &role, &n)
		out = append(out, map[string]any{"id": id, "name": name, "role": role, "members": n})
	}
	return out, rows.Err()
}

// ----- permission schemes

// Permissions a project's scheme can set, with the lowest role that has each by default.
var permDefaults = map[string]string{"create": "member", "comment": "member", "transition": "member", "assign": "member", "manage_sprints": "admin"}
var permOrder = []string{"create", "comment", "transition", "assign", "manage_sprints"}

func rankOfRole(r string) int {
	switch r {
	case "admin":
		return 3
	case "member":
		return 2
	case "viewer":
		return 1
	}
	return 0
}

// can reports whether a role may do something in a project: the project's scheme, or the default. An administrator can
// always do everything, so a scheme can never lock a project out of its own administration.
func (s *Service) can(ctx context.Context, a Actor, projectID, role, perm string) bool {
	if role == "admin" {
		return true
	}
	if role == "" {
		return false
	}
	min := permDefaults[perm]
	var raw []byte
	if s.DB.QueryRow(ctx, `SELECT rules FROM permission_scheme WHERE project_id::text=$1`, projectID).Scan(&raw) == nil {
		var rules map[string]string
		if json.Unmarshal(raw, &rules) == nil && rules[perm] != "" {
			min = rules[perm]
		}
	}
	if rankOfRole(role) >= rankOfRole(min) {
		return true
	}
	// a custom role may grant what the rank does not
	for _, g := range s.extraGrants(ctx, a.ID, projectID) {
		if g == perm {
			return true
		}
	}
	return false
}

type PermissionTable struct {
	Defaults  map[string]string `json:"defaults"`
	Overrides map[string]string `json:"overrides"`
	Effective map[string]string `json:"effective"`
}

func (s *Service) PermissionScheme(ctx context.Context, a Actor, projectKey string) (PermissionTable, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return PermissionTable{}, err
	}
	role := s.role(ctx, a, pid)
	if role == "" {
		return PermissionTable{}, ErrNotFound
	}
	t := PermissionTable{Defaults: permDefaults, Overrides: map[string]string{}, Effective: map[string]string{}}
	var raw []byte
	if s.DB.QueryRow(ctx, `SELECT rules FROM permission_scheme WHERE project_id::text=$1`, pid).Scan(&raw) == nil {
		json.Unmarshal(raw, &t.Overrides)
	}
	for _, p := range permOrder {
		t.Effective[p] = permDefaults[p]
		if o := t.Overrides[p]; o != "" {
			t.Effective[p] = o
		}
	}
	return t, nil
}

// SetPermissionScheme sets which role each action needs in a project (project administrators). A permission left out keeps
// its current setting; an empty role goes back to the default.
func (s *Service) SetPermissionScheme(ctx context.Context, a Actor, projectKey string, rules map[string]string) error {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	if !s.projectAdmin(ctx, a, pid) {
		if s.role(ctx, a, pid) == "" {
			return ErrNotFound
		}
		return ErrForbidden
	}
	for p, r := range rules {
		if _, ok := permDefaults[p]; !ok {
			return invalid("unknown permission %q (%s)", p, strings.Join(permOrder, ", "))
		}
		if r != "" && r != "viewer" && r != "member" && r != "admin" {
			return invalid("a permission needs the role viewer, member or admin")
		}
		if r == "viewer" && p != "create" && p != "comment" { // changing anything else also needs edit rights, which a viewer never has
			return invalid("only create and comment can be opened to viewers")
		}
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		cur := map[string]string{}
		if tx.QueryRow(ctx, `SELECT rules FROM permission_scheme WHERE project_id::text=$1 FOR UPDATE`, pid).Scan(&raw) == nil {
			json.Unmarshal(raw, &cur)
		}
		for p, r := range rules {
			if r == "" {
				delete(cur, p)
			} else {
				cur[p] = r
			}
		}
		b, _ := json.Marshal(cur)
		if _, err := tx.Exec(ctx, `INSERT INTO permission_scheme(project_id,rules) VALUES($1::uuid,$2) ON CONFLICT (project_id) DO UPDATE SET rules=EXCLUDED.rules`, pid, b); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "permission_scheme.save", strings.ToUpper(projectKey), map[string]any{"rules": cur})
	})
}

var _ = fmt.Sprint
