package domain

import (
	"context"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

type Project struct {
	ID       string `json:"id"`
	Key      string `json:"key"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Archived bool   `json:"archived"`
	Mirrored bool   `json:"mirrored,omitempty"` // mirrored from Jira: read-only here
	Role     string `json:"role,omitempty"`     // the caller's own role in it, in lists made for them (viewer, member, admin)
}

// EnsureDefaults creates the standard issue types and workflow template; idempotent.
func (s *Service) EnsureDefaults(ctx context.Context) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		for _, t := range []struct {
			n string
			l int
		}{{"Epic", 1}, {"Story", 0}, {"Task", 0}, {"Bug", 0}, {"Subtask", -1}} {
			if _, err := tx.Exec(ctx, `INSERT INTO issue_type(name,level) VALUES($1,$2) ON CONFLICT (name) DO NOTHING`, t.n, t.l); err != nil {
				return err
			}
		}
		var wf string
		err := tx.QueryRow(ctx, `SELECT id FROM workflow WHERE name='Standard'`).Scan(&wf)
		if err == nil {
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO workflow(name) VALUES('Standard') RETURNING id`).Scan(&wf); err != nil {
			return err
		}
		ids := map[string]string{}
		for i, st := range []struct{ n, c string }{{"To Do", "todo"}, {"In Progress", "in_progress"}, {"Done", "done"}} {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO status(workflow_id,name,category,position) VALUES($1,$2,$3,$4) RETURNING id`, wf, st.n, st.c, i).Scan(&id); err != nil {
				return err
			}
			ids[st.n] = id
		}
		for _, t := range []struct{ n, from, to string }{
			{"Start progress", "To Do", "In Progress"}, {"Done", "In Progress", "Done"},
			{"Reopen", "Done", "To Do"}, {"Back to To Do", "In Progress", "To Do"}, {"Resolve", "To Do", "Done"},
		} {
			if _, err := tx.Exec(ctx, `INSERT INTO transition(workflow_id,name,from_status,to_status) VALUES($1,$2,$3,$4)`, wf, t.n, ids[t.from], ids[t.to]); err != nil {
				return err
			}
		}
		return nil
	})
}

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

func (s *Service) CreateProject(ctx context.Context, a Actor, key, name string) (Project, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !keyRe.MatchString(key) {
		return Project{}, invalid("project key must be 2-10 characters, letters and digits, starting with a letter")
	}
	if strings.TrimSpace(name) == "" {
		return Project{}, invalid("project name is required")
	}
	if !a.Admin && !a.OrgAdmin {
		return Project{}, ErrForbidden
	}
	p := Project{Key: key, Name: name, Type: "software"}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO project(key,name,lead_id,workflow_id,org_id) VALUES($1,$2,$3,(SELECT id FROM workflow WHERE name='Standard'),$4) RETURNING id`,
			key, name, a.ID, a.OrgID()).Scan(&p.ID); err != nil {
			if strings.Contains(err.Error(), "project_key_key") {
				return invalid("project key %s is already in use", key)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1,$2,'admin')`, p.ID, a.ID); err != nil {
			return err
		}
		return outbox(ctx, tx, "project:"+p.Key, "project.created", p)
	})
	return p, planErr(err)
}

func (s *Service) AddMember(ctx context.Context, a Actor, projectKey, userID, role string) error {
	endWrite, perr := s.admit(ctx, projectKey)
	if perr != nil {
		return perr
	}
	defer endWrite()
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	switch r := s.role(ctx, a, pid); {
	case r == "":
		return ErrNotFound // no existence leak for people who cannot see the project
	case r != "admin":
		return ErrForbidden
	}
	// a person of another organization (or no one at all) cannot be added: the answer is the same for both, so it does not
	// reveal who exists elsewhere
	var ok bool
	if s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM app_user u JOIN project p ON p.org_id=u.org_id WHERE u.id::text=$1 AND p.id::text=$2)`, userID, pid).Scan(&ok); !ok {
		return invalid("no such user in this organization")
	}
	base, custom, err := s.resolveRole(ctx, s.DB, pid, role)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role,custom_role_id) VALUES($1,$2,$3,$4) ON CONFLICT (project_id,user_id) DO UPDATE SET role=$3, custom_role_id=$4`, pid, userID, base, nilIfEmpty(custom))
	return err
}

func (s *Service) projectID(ctx context.Context, key string) (string, error) {
	var id string
	err := s.DB.QueryRow(ctx, `SELECT id FROM project WHERE key=$1`, strings.ToUpper(key)).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", ErrNotFound
	}
	return id, err
}

// role returns "", "viewer", "member" or "admin". Global admins are project admins everywhere.
func (s *Service) role(ctx context.Context, a Actor, projectID string) string {
	if a.Admin {
		return "admin"
	}
	if a.OrgAdmin { // an organization administrator is a project administrator in their own organization only
		var same bool
		if s.DB.QueryRow(ctx, `SELECT org_id::text=$2 FROM project WHERE id::text=$1`, projectID, a.OrgID()).Scan(&same); same {
			return "admin"
		}
	}
	var r string
	s.DB.QueryRow(ctx, `SELECT role FROM effective_member WHERE project_id=$1 AND user_id=$2`, projectID, a.ID).Scan(&r)
	return r
}

func canWrite(role string) bool { return role == "member" || role == "admin" }

// canIn is can for a project named by key.
func (s *Service) canIn(ctx context.Context, a Actor, projectKey, role, perm string) bool {
	if role == "admin" {
		return true
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return false
	}
	return s.can(ctx, a, pid, role, perm)
}

func (s *Service) ListProjects(ctx context.Context, a Actor) ([]Project, error) {
	q := `SELECT p.id,p.key,p.name,p.type,p.archived,p.mirror_of IS NOT NULL,COALESCE(m.role,'') FROM project p `
	args := []any{}
	switch {
	case a.Admin:
		q = strings.Replace(q, "COALESCE(m.role,'')", "'admin'", 1)
	case a.OrgAdmin: // every project of their organization, administered by them
		q = strings.Replace(q, "COALESCE(m.role,'')", "'admin'", 1)
		q += `WHERE p.org_id::text=$1 `
		args = append(args, a.OrgID())
	default:
		q += `JOIN effective_member m ON m.project_id=p.id AND m.user_id=$1 `
		args = append(args, a.ID)
	}
	rows, err := s.DB.Query(ctx, q+`ORDER BY p.key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		rows.Scan(&p.ID, &p.Key, &p.Name, &p.Type, &p.Archived, &p.Mirrored, &p.Role)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SearchScope returns the project ids the actor may read, and those where internal comments are visible.
func (s *Service) SearchScope(ctx context.Context, a Actor) (all bool, readable, writable []string, err error) {
	if a.Admin {
		return true, nil, nil, nil
	}
	if a.OrgAdmin { // everything in their organization, and nothing outside it
		rows, err := s.DB.Query(ctx, `SELECT id FROM project WHERE org_id::text=$1`, a.OrgID())
		if err != nil {
			return false, nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			rows.Scan(&id)
			readable = append(readable, id)
			writable = append(writable, id)
		}
		return false, readable, writable, rows.Err()
	}
	rows, err := s.DB.Query(ctx, `SELECT project_id, role FROM effective_member WHERE user_id=$1`, a.ID)
	if err != nil {
		return false, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, role string
		rows.Scan(&id, &role)
		readable = append(readable, id)
		if canWrite(role) {
			writable = append(writable, id)
		}
	}
	return false, readable, writable, rows.Err()
}
