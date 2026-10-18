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
	if !a.Admin {
		return Project{}, ErrForbidden
	}
	p := Project{Key: key, Name: name, Type: "software"}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO project(key,name,lead_id,workflow_id) VALUES($1,$2,$3,(SELECT id FROM workflow WHERE name='Standard')) RETURNING id`,
			key, name, a.ID).Scan(&p.ID); err != nil {
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
	return p, err
}

func (s *Service) AddMember(ctx context.Context, a Actor, projectKey, userID, role string) error {
	if role != "viewer" && role != "member" && role != "admin" {
		return invalid("role must be viewer, member or admin")
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	if r := s.role(ctx, a, pid); r != "admin" {
		return ErrForbidden
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1,$2,$3) ON CONFLICT (project_id,user_id) DO UPDATE SET role=$3`, pid, userID, role)
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
	var r string
	s.DB.QueryRow(ctx, `SELECT role FROM project_member WHERE project_id=$1 AND user_id=$2`, projectID, a.ID).Scan(&r)
	return r
}

func canWrite(role string) bool { return role == "member" || role == "admin" }

func (s *Service) ListProjects(ctx context.Context, a Actor) ([]Project, error) {
	q := `SELECT p.id,p.key,p.name,p.type,p.archived FROM project p `
	args := []any{}
	if !a.Admin {
		q += `JOIN project_member m ON m.project_id=p.id AND m.user_id=$1 `
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
		rows.Scan(&p.ID, &p.Key, &p.Name, &p.Type, &p.Archived)
		out = append(out, p)
	}
	return out, rows.Err()
}
