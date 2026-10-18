package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/git"
)

// Git write actions, connection health and disconnect (GIT-14, GIT-18). Writing to a provider is opt-in per connection:
// until an administrator turns it on nothing is ever sent, and a token that may read but not write fails with a clear
// message and a health warning, never silently.

// ---------- connection options and health ----------

type GitConnOptions struct {
	WriteEnabled *bool `json:"write_enabled"`
}

func (s *Service) SetGitConnectionOptions(ctx context.Context, a Actor, id string, o GitConnOptions) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if o.WriteEnabled != nil {
			tag, err := tx.Exec(ctx, `UPDATE git_connection SET write_enabled=$2 WHERE id::text=$1 AND NOT disabled`, id, *o.WriteEnabled)
			if err != nil || tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			if err := s.audit(ctx, tx, a, "git.connection.write_actions", id, map[string]any{"enabled": *o.WriteEnabled}); err != nil {
				return err
			}
		}
		return nil
	})
}

// noteGitHealth records what a call to the provider just showed about the connection, so that a revoked or too-weak token is a
// warning an administrator sees, not a silent failure.
func (s *Service) noteGitHealth(ctx context.Context, connID string, err error) {
	health, detail := "ok", ""
	switch {
	case err == nil: // a read that worked does not show that writes do: a standing "cannot write" stays until a write works
		s.DB.Exec(ctx, `UPDATE git_connection SET health='ok',health_detail='',health_at=$2 WHERE id::text=$1 AND health<>'no_write_permission'`, connID, s.Now())
		return
	case errors.Is(err, git.ErrBadCredentials):
		health, detail = "token_rejected", "the provider rejected the connection's token: it was revoked, has expired, or is wrong. Replace it."
	case errors.Is(err, git.ErrNoPermission):
		health, detail = "no_write_permission", "the token can read but is not allowed to write: give it permission to write repository contents (GitHub) or the api scope (GitLab), or turn write actions off."
	default:
		return // a network error or a rate limit says nothing about the token
	}
	s.DB.Exec(ctx, `UPDATE git_connection SET health=$2,health_detail=$3,health_at=$4 WHERE id::text=$1`, connID, health, detail, s.Now())
}

// clearGitHealth records that a write worked.
func (s *Service) clearGitHealth(ctx context.Context, connID string) {
	s.DB.Exec(ctx, `UPDATE git_connection SET health='ok',health_detail='',health_at=$2 WHERE id::text=$1`, connID, s.Now())
}

// CheckGitConnection asks the provider who the token belongs to, which proves it is still accepted.
func (s *Service) CheckGitConnection(ctx context.Context, a Actor, id string) (GitConnection, error) {
	if err := adminOnly(a); err != nil {
		return GitConnection{}, err
	}
	cl, _, err := s.GitClientFor(ctx, id)
	if err != nil {
		return GitConnection{}, err
	}
	if _, err = cl.Whoami(ctx); err != nil && !errors.Is(err, git.ErrBadCredentials) {
		s.DB.Exec(ctx, `UPDATE git_connection SET health='error',health_detail=$2,health_at=$3 WHERE id::text=$1`, id, "the provider could not be reached: "+trimErr(err), s.Now())
	} else {
		s.noteGitHealth(ctx, id, err)
	}
	conns, _ := s.ListGitConnections(ctx, a)
	for _, c := range conns {
		if c.ID == id {
			return c, nil
		}
	}
	return GitConnection{}, ErrNotFound
}

// DisconnectGitConnection ends a connection. keep_read_only stops everything (webhooks are refused, nothing is synced or written)
// and keeps the links and cached data readable on the issues; purge removes them. Tasklane never created the webhook at the
// provider, so removing it there is the administrator's step.
func (s *Service) DisconnectGitConnection(ctx context.Context, a Actor, id, mode string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	switch mode {
	case "", "purge":
		return s.tx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM git_connection WHERE id::text=$1`, id)
			if err != nil || tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			return s.audit(ctx, tx, a, "git.connection.purge", id, nil)
		})
	case "keep_read_only":
		return s.tx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE git_connection SET disabled=true,write_enabled=false,health='unknown',health_detail='disconnected: kept read-only',health_at=$2 WHERE id::text=$1`, id, s.Now())
			if err != nil || tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			return s.audit(ctx, tx, a, "git.connection.keep_read_only", id, nil)
		})
	}
	return invalid("the mode is purge or keep_read_only")
}

// ---------- create a branch from an issue ----------

type BranchIn struct {
	Repo string `json:"repo"` // owner/name; optional when the issue's project maps to one writable repository
	Base string `json:"base"` // optional; the repository's default branch
	Name string `json:"name"` // optional; "{key}-{slug}" by default
}

type BranchOut struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	URL     string `json:"url,omitempty"`
	Command string `json:"command"` // the git command that gets it
}

var reBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)

// slugify makes the readable part of a branch name from an issue summary.
func slugify(summary string, max int) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(summary) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > max {
		out = strings.Trim(out[:max], "-")
	}
	return out
}

type writableRepo struct{ connID, repoID, full, provider, def string }

// writableRepos lists the repositories mapped to the issue's project whose connection has write actions on.
func (s *Service) writableRepos(ctx context.Context, projectID string) ([]writableRepo, error) {
	rows, err := s.DB.Query(ctx, `SELECT c.id::text,r.id::text,r.full_name,c.provider,r.default_branch FROM git_repo r JOIN git_connection c ON c.id=r.connection_id JOIN git_repo_project rp ON rp.repo_id=r.id
		WHERE rp.project_id::text=$1 AND c.write_enabled AND NOT c.disabled ORDER BY r.full_name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []writableRepo
	for rows.Next() {
		var w writableRepo
		rows.Scan(&w.connID, &w.repoID, &w.full, &w.provider, &w.def)
		out = append(out, w)
	}
	return out, rows.Err()
}

// CreateGitBranch creates a branch for an issue at the provider. The person needs write access to the issue; the connection
// must have write actions on. The default name is "{key}-{slug}", and when it is taken a numbered one is made.
func (s *Service) CreateGitBranch(ctx context.Context, a Actor, key string, in BranchIn) (BranchOut, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return BranchOut{}, err
	}
	if is.Archived != nil || !canWrite(role) {
		return BranchOut{}, ErrForbidden
	}
	var projectID, summary string
	var mirrored bool
	s.DB.QueryRow(ctx, `SELECT p.id::text,i.summary,p.mirror_of IS NOT NULL FROM issue i JOIN project p ON p.id=i.project_id WHERE i.id::text=$1`, is.ID).Scan(&projectID, &summary, &mirrored)
	if mirrored {
		return BranchOut{}, invalid("a mirrored issue is read-only here")
	}
	repos, err := s.writableRepos(ctx, projectID)
	if err != nil {
		return BranchOut{}, err
	}
	var w *writableRepo
	switch {
	case in.Repo != "":
		for i := range repos {
			if strings.EqualFold(repos[i].full, in.Repo) {
				w = &repos[i]
			}
		}
		if w == nil {
			return BranchOut{}, invalid("%q is not a repository of this project with write actions turned on", in.Repo)
		}
	case len(repos) == 1:
		w = &repos[0]
	case len(repos) == 0:
		return BranchOut{}, invalid("no repository of this project has write actions turned on: an administrator enables them on the connection")
	default:
		names := make([]string, len(repos))
		for i, r := range repos {
			names[i] = r.full
		}
		return BranchOut{}, invalid("this project has several repositories; name one of: %s", strings.Join(names, ", "))
	}
	generated := in.Name == ""
	name := in.Name
	if generated {
		name = is.Key + "-" + slugify(summary, 40)
		name = strings.TrimSuffix(name, "-")
	}
	switch {
	case !reBranch.MatchString(name) || strings.Contains(name, "..") || strings.Contains(name, "//") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock"):
		return BranchOut{}, invalid("that is not a valid branch name")
	case !strings.Contains(strings.ToUpper(name), is.Key):
		return BranchOut{}, invalid("the branch name must contain the issue key %s, so that Tasklane links the branch to the issue", is.Key)
	}
	cl, _, err := s.GitClientFor(ctx, w.connID)
	if err != nil {
		return BranchOut{}, err
	}
	base := in.Base
	if base == "" {
		if base = w.def; base == "" {
			if base, err = cl.DefaultBranch(ctx, w.full); err != nil || base == "" {
				s.noteGitHealth(ctx, w.connID, err)
				return BranchOut{}, s.writeFailure(w, err, "find the repository's default branch")
			}
		}
	}
	var url string
	tryName := name
	for n := 1; ; n++ {
		if n > 1 {
			tryName = fmt.Sprintf("%s-%d", name, n)
		}
		url, err = cl.CreateBranch(ctx, w.full, tryName, base)
		if errors.Is(err, git.ErrExists) && generated && n < 6 {
			continue
		}
		break
	}
	s.noteGitHealth(ctx, w.connID, err)
	if err != nil {
		s.writeLog(ctx, w.connID, "branch", is.Key, tryName, "failed", trimErr(err), a.ID)
		if errors.Is(err, git.ErrExists) {
			return BranchOut{}, invalid("the branch %q already exists in %s: choose another name", tryName, w.full)
		}
		return BranchOut{}, s.writeFailure(w, err, "create the branch")
	}
	s.clearGitHealth(ctx, w.connID)
	s.writeLog(ctx, w.connID, "branch", is.Key, tryName, "done", "created at "+base, a.ID)
	s.tx(ctx, func(tx pgx.Tx) error {
		return s.audit(ctx, tx, a, "git.branch.create", is.Key, map[string]any{"repo": w.full, "branch": tryName, "base": base})
	})
	cmd := fmt.Sprintf("git fetch origin %s && git switch %s", tryName, tryName)
	return BranchOut{Repo: w.full, Branch: tryName, Base: base, URL: url, Command: cmd}, nil
}

func (s *Service) writeFailure(w *writableRepo, err error, what string) error {
	switch {
	case errors.Is(err, git.ErrBadCredentials):
		return invalid("the provider rejected the connection's token while trying to %s: an administrator must replace the token", what)
	case errors.Is(err, git.ErrNoPermission):
		return invalid("the connection's token is not allowed to %s in %s: it needs permission to write repository contents (GitHub) or the api scope (GitLab)", what, w.full)
	case errors.Is(err, git.ErrNoBase):
		return invalid("the base branch does not exist in %s", w.full)
	}
	return invalid("the provider refused: %s", trimErr(err))
}

func (s *Service) writeLog(ctx context.Context, connID, kind, key, ref, outcome, detail, userID string) {
	var uid any
	if userID != "" {
		uid = userID
	}
	s.DB.Exec(ctx, `INSERT INTO git_write_log(connection_id,kind,issue_key,ref,outcome,detail,user_id,at) VALUES($1::uuid,$2,$3,$4,$5,$6,$7::uuid,$8)`, connID, kind, key, ref, outcome, detail, uid, s.Now())
}

// ---------- back-link comment on the pull request ----------

// postBacklinks comments once on each linked pull request or merge request to say which Tasklane issue it is linked to. Only
// for live events and only where write actions are on. A failure is logged and the claim released, so a later event retries.
func (s *Service) postBacklinks(ctx context.Context, r *repoInfo, g gitApplied) {
	if g.ev.Kind != git.KindPR || s.BaseURL == "" || !r.write || r.disabled {
		return
	}
	rows, err := s.DB.Query(ctx, `SELECT i.id::text,i.key FROM git_link l JOIN issue i ON i.id=l.issue_id JOIN project p ON p.id=i.project_id
		WHERE l.object_id::text=$1 AND i.deleted_at IS NULL AND p.mirror_of IS NULL AND NOT EXISTS (SELECT 1 FROM git_backlink b WHERE b.object_id=l.object_id AND b.issue_id=l.issue_id)`, g.objID)
	if err != nil {
		return
	}
	type iss struct{ id, key string }
	var is []iss
	for rows.Next() {
		var x iss
		rows.Scan(&x.id, &x.key)
		is = append(is, x)
	}
	rows.Close()
	if len(is) == 0 {
		return
	}
	cl, _, err := s.GitClientFor(ctx, r.connID)
	if err != nil {
		return
	}
	for _, x := range is {
		tag, err := s.DB.Exec(ctx, `INSERT INTO git_backlink(object_id,issue_id) VALUES($1::uuid,$2::uuid) ON CONFLICT DO NOTHING`, g.objID, x.id)
		if err != nil || tag.RowsAffected() == 0 {
			continue
		}
		// the key and a link, nothing from the issue: a pull request may be public and the issue is not
		text := fmt.Sprintf("Linked to Tasklane issue [%s](%s/#/issue/%s).", x.key, strings.TrimRight(s.BaseURL, "/"), x.key)
		cerr := cl.Comment(ctx, g.ev.Repo, g.ev.ExtID, text)
		s.noteGitHealth(ctx, r.connID, cerr)
		if cerr != nil {
			s.DB.Exec(ctx, `DELETE FROM git_backlink WHERE object_id::text=$1 AND issue_id::text=$2`, g.objID, x.id)
			s.writeLog(ctx, r.connID, "backlink", x.key, g.ev.ExtID, "failed", trimErr(cerr), "")
			continue
		}
		s.clearGitHealth(ctx, r.connID)
		s.writeLog(ctx, r.connID, "backlink", x.key, g.ev.ExtID, "done", "commented on "+g.ev.Repo+" #"+g.ev.ExtID, "")
	}
}
