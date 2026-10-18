package domain

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/git"
)

// Visibility of development data (GIT-11). By default anyone who can see an issue sees its pull requests, branches, commits,
// builds and deployments. An organization can switch to "restricted": details of a repository that is private (or whose
// privacy is not known yet) are then shown only to people with a linked provider account that the provider lists as having
// access to that repository, and to instance administrators. The decision is made in the database by git_repo_visible, so the
// development panel, TQL, the REST API and the MCP tools cannot disagree, and it fails closed: an access list older than a
// day is not believed. Revocation takes effect at the next sync, or at once when the account is unlinked.

type GitVisibility struct {
	Mode  string          `json:"mode"` // default or restricted
	Repos []GitRepoAccess `json:"repos,omitempty"`
}

type GitRepoAccess struct {
	ID            string  `json:"id"`
	FullName      string  `json:"full_name"`
	Private       *bool   `json:"private"` // null: not known yet, which counts as private
	Members       int     `json:"members"`
	MembersSynced *string `json:"access_list_synced_at,omitempty"`
}

func (s *Service) GitVisibility(ctx context.Context, a Actor) (GitVisibility, error) {
	if !a.Admin && !a.OrgAdmin {
		return GitVisibility{}, ErrForbidden
	}
	v := GitVisibility{}
	if err := s.DB.QueryRow(ctx, `SELECT git_visibility FROM organization WHERE id::text=$1`, a.OrgID()).Scan(&v.Mode); err != nil {
		return v, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id::text,r.full_name,r.private,(SELECT count(*) FROM git_repo_member m WHERE m.repo_id=r.id),to_char(r.members_synced_at,'YYYY-MM-DD"T"HH24:MI:SS"Z"') FROM git_repo r ORDER BY r.full_name`)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var x GitRepoAccess
		rows.Scan(&x.ID, &x.FullName, &x.Private, &x.Members, &x.MembersSynced)
		v.Repos = append(v.Repos, x)
	}
	return v, rows.Err()
}

// SetGitVisibility changes the setting for the caller's organization.
func (s *Service) SetGitVisibility(ctx context.Context, a Actor, mode string) error {
	if !a.Admin && !a.OrgAdmin {
		return ErrForbidden
	}
	if mode != "default" && mode != "restricted" {
		return invalid("the setting is default or restricted")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE organization SET git_visibility=$2 WHERE id::text=$1`, a.OrgID(), mode); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "git.visibility", a.OrgID(), map[string]any{"mode": mode})
	})
}

// SetGitRepoPrivacy overrides what the provider said about a repository (null returns to what the provider says).
func (s *Service) SetGitRepoPrivacy(ctx context.Context, a Actor, repoID string, private *bool) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE git_repo SET private_override=$2, private=COALESCE($2, private) WHERE id::text=$1`, repoID, private)
		if err != nil || tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "git.repo.privacy", repoID, map[string]any{"private": private})
	})
}

// restrictedAnywhere says whether any organization restricts development data. Only then does a sync ask the provider who has access.
func (s *Service) restrictedAnywhere(ctx context.Context) bool {
	var b bool
	s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization WHERE git_visibility='restricted')`).Scan(&b)
	return b
}

// syncGitAccess reads a repository's visibility and access list. A failure leaves the previous list in place (and ageing: after a
// day it is no longer believed) and is reported to the administrator.
func (s *Service) syncGitAccess(ctx context.Context, cl *git.Client, repoID, full, connID string) string {
	var problems []string
	if private, def, err := cl.RepoInfo(ctx, full); err != nil {
		s.noteGitHealth(ctx, connID, err)
		problems = append(problems, "repository privacy: "+trimErr(err))
	} else {
		s.DB.Exec(ctx, `UPDATE git_repo SET private=COALESCE(private_override,$2),default_branch=CASE WHEN $3<>'' THEN $3 ELSE default_branch END WHERE id::text=$1`, repoID, private, def)
	}
	members, err := cl.Members(ctx, full)
	if err != nil {
		s.noteGitHealth(ctx, connID, err)
		problems = append(problems, "access list: "+trimErr(err))
	} else {
		s.tx(ctx, func(tx pgx.Tx) error {
			tx.Exec(ctx, `DELETE FROM git_repo_member WHERE repo_id::text=$1`, repoID)
			for _, m := range members {
				tx.Exec(ctx, `INSERT INTO git_repo_member(repo_id,login) VALUES($1::uuid,$2) ON CONFLICT DO NOTHING`, repoID, strings.ToLower(m))
			}
			_, err := tx.Exec(ctx, `UPDATE git_repo SET members_synced_at=$2 WHERE id::text=$1`, repoID, s.Now())
			return err
		})
	}
	return strings.Join(problems, "; ")
}

// visArgs are the arguments of git_repo_visible for an actor: no person (the system, or an administrator, who sees everything)
// is a null user.
func (s *Service) visArgs(a Actor) (any, any) {
	if a.ID == "" || a.Admin {
		return nil, s.Now()
	}
	return a.ID, s.Now()
}

var _ = fmt.Sprint
