package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/git"
)

// Workflow automation from code events and smart commits (SPEC 18.4, GIT-08..10).
//
// Both run only for live webhook deliveries, never for a backfill or reconcile: replaying the history of a repository must
// not move issues that were finished long ago. Both respect the workflow (a transition it does not allow is skipped and
// reported, never forced), never touch mirrored or archived issues, and are attributed to the code event or to the mapped
// person. A rule runs at most once per code object and issue, which is also what stops a loop.

// ---------- who is who ----------

type GitIdentity struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
	Login    string `json:"login"`
	UserID   string `json:"user_id"`
	User     string `json:"user"`
}

var reLogin = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// LinkGitIdentity says that a provider account is a person. Only an instance administrator does this: it is what lets a
// commit run commands as that person, so it is a statement about who they are that someone must vouch for.
func (s *Service) LinkGitIdentity(ctx context.Context, a Actor, provider, login, userID string) (GitIdentity, error) {
	if err := adminOnly(a); err != nil {
		return GitIdentity{}, err
	}
	login = strings.ToLower(strings.TrimSpace(login))
	if provider != "github" && provider != "gitlab" {
		return GitIdentity{}, invalid("the provider is github or gitlab")
	}
	if !reLogin.MatchString(login) {
		return GitIdentity{}, invalid("that is not a provider account name")
	}
	var name string
	if err := s.DB.QueryRow(ctx, `SELECT name FROM app_user WHERE id::text=$1 AND active AND kind='human'`, userID).Scan(&name); err != nil {
		return GitIdentity{}, invalid("no such person")
	}
	g := GitIdentity{Provider: provider, Login: login, UserID: userID, User: name}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO git_identity(provider,login,user_id,created_by) VALUES($1,$2,$3::uuid,NULLIF($4,'')::uuid) RETURNING id`, provider, login, userID, a.ID).Scan(&g.ID); err != nil {
			return invalid("that account is already linked to someone")
		}
		return s.audit(ctx, tx, a, "git.identity.link", fmt.Sprintf("%s:%s", provider, login), map[string]any{"user": userID})
	})
	return g, err
}

func (s *Service) ListGitIdentities(ctx context.Context, a Actor) ([]GitIdentity, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT g.id,g.provider,g.login,g.user_id::text,u.name FROM git_identity g JOIN app_user u ON u.id=g.user_id ORDER BY g.provider,g.login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitIdentity{}
	for rows.Next() {
		var g GitIdentity
		if err := rows.Scan(&g.ID, &g.Provider, &g.Login, &g.UserID, &g.User); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) UnlinkGitIdentity(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var prov, login string
		if err := tx.QueryRow(ctx, `DELETE FROM git_identity WHERE id=$1 RETURNING provider,login`, id).Scan(&prov, &login); err != nil {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "git.identity.unlink", prov+":"+login, nil)
	})
}

// gitUser maps what the provider asserted about who did something to a person. A linked account wins; otherwise an email the
// provider vouches for as the account's own counts. A name or an email written in a commit never does (GIT-10).
func (s *Service) gitUser(ctx context.Context, q querier, provider, login, verifiedEmail string) *string {
	var id string
	if login != "" {
		if q.QueryRow(ctx, `SELECT user_id::text FROM git_identity WHERE provider=$1 AND login=lower($2)`, provider, login).Scan(&id) == nil {
			return &id
		}
		if verifiedEmail != "" && q.QueryRow(ctx, `SELECT id::text FROM app_user WHERE lower(email)=lower($1) AND active AND kind='human'`, verifiedEmail).Scan(&id) == nil {
			return &id
		}
	}
	return nil
}

// ---------- rules ----------

type GitRule struct {
	ID         int64  `json:"id,omitempty"`
	Name       string `json:"name"`
	Trigger    string `json:"trigger"`
	Transition string `json:"transition"`
	Comment    string `json:"comment"`
	Mode       string `json:"mode"` // off, dry_run (records what would happen) or on
}

type GitRuleRun struct {
	ID      int64     `json:"id"`
	Rule    string    `json:"rule"`
	Issue   string    `json:"issue"`
	Trigger string    `json:"trigger"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail"`
	At      time.Time `json:"at"`
}

var gitTriggers = map[string]bool{"branch_created": true, "pr_opened": true, "pr_merged": true, "build_failed": true}

// projectAdmin resolves a project key and requires the actor to administer it.
func (s *Service) gitProjectAdmin(ctx context.Context, a Actor, key string) (pid string, mirrored bool, err error) {
	if pid, err = s.projectID(ctx, key); err != nil {
		return "", false, err
	}
	switch r := s.role(ctx, a, pid); {
	case r == "":
		return "", false, ErrNotFound
	case r != "admin":
		return "", false, ErrForbidden
	}
	s.DB.QueryRow(ctx, `SELECT mirror_of IS NOT NULL FROM project WHERE id::text=$1`, pid).Scan(&mirrored)
	return pid, mirrored, nil
}

func (s *Service) ListGitRules(ctx context.Context, a Actor, key string) ([]GitRule, error) {
	pid, _, err := s.gitProjectAdmin(ctx, a, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id,name,trigger,transition,comment,mode FROM git_rule WHERE project_id::text=$1 ORDER BY id`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitRule{}
	for rows.Next() {
		var r GitRule
		if err := rows.Scan(&r.ID, &r.Name, &r.Trigger, &r.Transition, &r.Comment, &r.Mode); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveGitRule creates a rule (ID 0) or replaces one. A new rule starts in dry-run unless it says otherwise, so the first thing
// an administrator sees is what it would have done.
func (s *Service) SaveGitRule(ctx context.Context, a Actor, key string, in GitRule) (GitRule, error) {
	pid, mirrored, err := s.gitProjectAdmin(ctx, a, key)
	if err != nil {
		return GitRule{}, err
	}
	in.Name, in.Transition, in.Comment = strings.TrimSpace(in.Name), strings.TrimSpace(in.Transition), strings.TrimSpace(in.Comment)
	if in.Mode == "" {
		in.Mode = "dry_run"
	}
	switch {
	case mirrored:
		return in, invalid("a mirrored project is read-only here: rules never change mirrored issues")
	case in.Name == "" || len(in.Name) > 80:
		return in, invalid("a rule needs a name of up to 80 characters")
	case !gitTriggers[in.Trigger]:
		return in, invalid("the trigger is branch_created, pr_opened, pr_merged or build_failed")
	case in.Mode != "off" && in.Mode != "dry_run" && in.Mode != "on":
		return in, invalid("the mode is off, dry_run or on")
	case in.Transition == "" && in.Comment == "":
		return in, invalid("a rule needs a transition or a comment")
	case len(in.Comment) > 500 || len(in.Transition) > 80:
		return in, invalid("the comment is up to 500 characters and the transition up to 80")
	}
	if in.Transition != "" {
		var n int
		s.DB.QueryRow(ctx, `SELECT count(*) FROM transition t JOIN project p ON p.workflow_id=t.workflow_id WHERE p.id::text=$1 AND lower(t.name)=lower($2)`, pid, in.Transition).Scan(&n)
		if n == 0 {
			return in, invalid("the project's workflow has no transition named %q", in.Transition)
		}
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if in.ID == 0 {
			if err := tx.QueryRow(ctx, `INSERT INTO git_rule(project_id,name,trigger,transition,comment,mode,created_by) VALUES($1::uuid,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid) RETURNING id`,
				pid, in.Name, in.Trigger, in.Transition, in.Comment, in.Mode, a.ID).Scan(&in.ID); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE git_rule SET name=$3,trigger=$4,transition=$5,comment=$6,mode=$7 WHERE id=$1 AND project_id::text=$2`, in.ID, pid, in.Name, in.Trigger, in.Transition, in.Comment, in.Mode)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
		return s.audit(ctx, tx, a, "git.rule.save", fmt.Sprintf("%s:%d", strings.ToUpper(key), in.ID), map[string]any{"trigger": in.Trigger, "mode": in.Mode})
	})
	return in, err
}

func (s *Service) DeleteGitRule(ctx context.Context, a Actor, key string, id int64) error {
	pid, _, err := s.gitProjectAdmin(ctx, a, key)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM git_rule WHERE id=$1 AND project_id::text=$2`, id, pid)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "git.rule.delete", fmt.Sprintf("%s:%d", strings.ToUpper(key), id), nil)
	})
}

func (s *Service) GitRuleRuns(ctx context.Context, a Actor, key string, limit int) ([]GitRuleRun, error) {
	pid, _, err := s.gitProjectAdmin(ctx, a, key)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id,g.name,i.key,r.trigger,r.outcome,r.detail,r.at FROM git_rule_run r JOIN git_rule g ON g.id=r.rule_id JOIN issue i ON i.id=r.issue_id
		WHERE g.project_id::text=$1 ORDER BY r.id DESC LIMIT $2`, pid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitRuleRun{}
	for rows.Next() {
		var x GitRuleRun
		if err := rows.Scan(&x.ID, &x.Rule, &x.Issue, &x.Trigger, &x.Outcome, &x.Detail, &x.At); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SetSmartCommits chooses where commit commands run: off (the default), only on the default branch (and for pull requests
// merged into it), or on any branch.
func (s *Service) SetSmartCommits(ctx context.Context, a Actor, key, mode string) error {
	pid, mirrored, err := s.gitProjectAdmin(ctx, a, key)
	if err != nil {
		return err
	}
	if mode != "off" && mode != "default_branch" && mode != "any" {
		return invalid("the setting is off, default_branch or any")
	}
	if mirrored && mode != "off" {
		return invalid("a mirrored project is read-only here: smart commits never change mirrored issues")
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE project SET git_smart_commits=$2 WHERE id::text=$1`, pid, mode); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "git.smart_commits.setting", strings.ToUpper(key), map[string]any{"mode": mode})
	})
}

// ---------- acting on a code event ----------

// gitApplied is what applying one event did, for the rules and smart commits that follow it.
type gitApplied struct {
	objID   string
	ev      git.Event
	existed bool   // the object was already known
	prev    string // its state before this event
}

func (g gitApplied) triggers() []string {
	e := g.ev
	switch e.Kind {
	case git.KindBranch:
		if !g.existed && !e.Deleted {
			return []string{"branch_created"}
		}
	case git.KindPR:
		var out []string
		if e.State == "open" && (!g.existed || g.prev == "draft") {
			out = append(out, "pr_opened")
		}
		if e.State == "merged" && g.prev != "merged" {
			out = append(out, "pr_merged")
		}
		return out
	case git.KindBuild:
		if e.State == "failing" && g.prev != "failing" {
			return []string{"build_failed"}
		}
	}
	return nil
}

func providerName(p string) string {
	if p == "gitlab" {
		return "GitLab"
	}
	return "GitHub"
}

// label is how the action is attributed: "GitHub, PR #12".
func (g gitApplied) label(provider string) string {
	pn := providerName(provider)
	switch g.ev.Kind {
	case git.KindPR:
		if provider == "gitlab" {
			return fmt.Sprintf("%s, MR !%s", pn, g.ev.ExtID)
		}
		return fmt.Sprintf("%s, PR #%s", pn, g.ev.ExtID)
	case git.KindBranch:
		return fmt.Sprintf("%s, branch %s", pn, g.ev.Branch)
	case git.KindBuild:
		return fmt.Sprintf("%s, build %s", pn, g.ev.Title)
	}
	return pn
}

// codeActor is the actor a rule acts as: the system, labelled with the event, running as an administrator-level actor but
// through the same workflow checks a person meets.
func (s *Service) codeActor(ctx context.Context, createdBy, label string) Actor {
	id := ""
	if createdBy != "" {
		s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE id::text=$1 AND active`, createdBy).Scan(&id)
	}
	if id == "" {
		s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE is_admin AND active ORDER BY created_at LIMIT 1`).Scan(&id)
	}
	return Actor{ID: id, Label: label, Admin: true, Kind: autoActorKind}
}

type issueRef struct{ id, key, projectID, status string }

func (s *Service) transitionByName(ctx context.Context, issueID, name string) (tid, to string, err error) {
	err = s.DB.QueryRow(ctx, `SELECT t.id::text,ts.name FROM transition t JOIN issue i ON i.id=$1 JOIN project p ON p.id=i.project_id JOIN status ts ON ts.id=t.to_status
		WHERE t.workflow_id=p.workflow_id AND (lower(t.name)=lower($2) OR lower(ts.name)=lower($2)) AND (t.from_status IS NULL OR t.from_status=i.status_id) ORDER BY t.from_status IS NULL LIMIT 1`, issueID, name).Scan(&tid, &to)
	return
}

// runGitRules applies the rules of every project the event's issues belong to. Each (rule, code object, issue) runs once.
func (s *Service) runGitRules(ctx context.Context, r *repoInfo, g gitApplied) {
	trigs := g.triggers()
	if len(trigs) == 0 {
		return
	}
	var rows pgx.Rows
	var err error
	if g.ev.Kind == git.KindBuild { // a build belongs to the issues linked to the commit, branch or pull request it ran on
		if g.ev.SHA == "" {
			return
		}
		rows, err = s.DB.Query(ctx, `SELECT DISTINCT i.id::text,i.key,i.project_id::text,st.name FROM git_object o JOIN git_link l ON l.object_id=o.id JOIN issue i ON i.id=l.issue_id JOIN status st ON st.id=i.status_id
			WHERE o.repo_id::text=$1 AND o.sha=$2 AND o.kind IN ('pr','branch','commit') AND i.deleted_at IS NULL`, r.id, g.ev.SHA)
	} else {
		rows, err = s.DB.Query(ctx, `SELECT i.id::text,i.key,i.project_id::text,st.name FROM git_link l JOIN issue i ON i.id=l.issue_id JOIN status st ON st.id=i.status_id
			WHERE l.object_id::text=$1 AND i.deleted_at IS NULL`, g.objID)
	}
	if err != nil {
		return
	}
	var issues []issueRef
	for rows.Next() {
		var x issueRef
		rows.Scan(&x.id, &x.key, &x.projectID, &x.status)
		issues = append(issues, x)
	}
	rows.Close()
	for _, is := range issues {
		inRepo := false
		for _, pid := range r.projects {
			inRepo = inRepo || pid == is.projectID
		}
		if !inRepo {
			continue
		}
		for _, trig := range trigs {
			s.applyRulesTo(ctx, r, g, is, trig)
		}
	}
}

func (s *Service) applyRulesTo(ctx context.Context, r *repoInfo, g gitApplied, is issueRef, trig string) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,transition,comment,mode,COALESCE(created_by::text,'') FROM git_rule WHERE project_id::text=$1 AND trigger=$2 AND mode<>'off' ORDER BY id`, is.projectID, trig)
	if err != nil {
		return
	}
	type rule struct {
		id                                  int64
		name, transition, comment, mode, by string
	}
	var rs []rule
	for rows.Next() {
		var x rule
		rows.Scan(&x.id, &x.name, &x.transition, &x.comment, &x.mode, &x.by)
		rs = append(rs, x)
	}
	rows.Close()
	for _, ru := range rs {
		// claim the run: a second delivery, a replay or a re-opened pull request finds it taken and does nothing
		tag, err := s.DB.Exec(ctx, `INSERT INTO git_rule_run(rule_id,object_id,issue_id,trigger,outcome,detail) VALUES($1,$2::uuid,$3::uuid,$4,'skipped','claimed') ON CONFLICT DO NOTHING`, ru.id, g.objID, is.id, trig)
		if err != nil || tag.RowsAffected() == 0 {
			continue
		}
		outcome, detail := s.execGitRule(ctx, r, g, is, ru.mode, ru.transition, ru.comment, ru.by)
		s.DB.Exec(ctx, `UPDATE git_rule_run SET outcome=$4,detail=$5,at=$6 WHERE rule_id=$1 AND object_id::text=$2 AND issue_id::text=$3`, ru.id, g.objID, is.id, outcome, detail, s.Now())
	}
}

func (s *Service) execGitRule(ctx context.Context, r *repoInfo, g gitApplied, is issueRef, mode, transition, comment, createdBy string) (outcome, detail string) {
	var archived, mirrored bool
	s.DB.QueryRow(ctx, `SELECT i.archived_at IS NOT NULL, p.mirror_of IS NOT NULL FROM issue i JOIN project p ON p.id=i.project_id WHERE i.id::text=$1`, is.id).Scan(&archived, &mirrored)
	switch {
	case mirrored:
		return "skipped", "mirrored issues are never changed by a rule"
	case archived:
		return "skipped", "the issue is archived"
	}
	var tid, to string
	if transition != "" {
		var err error
		if tid, to, err = s.transitionByName(ctx, is.id, transition); err != nil {
			return "skipped", fmt.Sprintf("the transition %q is not available from %q", transition, is.status)
		}
	}
	if mode == "dry_run" {
		var parts []string
		if transition != "" {
			parts = append(parts, fmt.Sprintf("move %s from %q to %q", is.key, is.status, to))
		}
		if comment != "" {
			parts = append(parts, "comment on "+is.key)
		}
		return "dry_run", "would " + strings.Join(parts, " and ") + " (" + g.label(r.provider) + ")"
	}
	actor := s.codeActor(ctx, createdBy, g.label(r.provider))
	var did []string
	if transition != "" {
		if _, err := s.Transition(ctx, actor, is.key, tid, nil); err != nil {
			return "skipped", "the workflow did not allow it: " + trimErr(err)
		}
		did = append(did, fmt.Sprintf("moved from %q to %q", is.status, to))
	}
	if comment != "" {
		text := comment
		if g.ev.URL != "" {
			text += "\n\n" + g.ev.URL
		}
		if _, err := s.AddComment(ctx, actor, is.key, text, false); err != nil {
			return "failed", "commented: " + trimErr(err)
		}
		did = append(did, "commented")
	}
	return "applied", strings.Join(did, ", ")
}

// ---------- smart commits ----------

type smartCmd struct {
	key  string
	name string
	arg  string
	body bool // found in a pull request description
}

var reCmd = regexp.MustCompile(`(?:^|\s)#([A-Za-z][A-Za-z0-9_-]{0,40})`)

// parseSmart finds, line by line, the issue keys and the #commands that follow the first key of that line: "ABC-1 ABC-2 #comment
// fixed #in-review" gives each key both commands. A command's argument runs to the next command or the end of the line.
func parseSmart(re *regexp.Regexp, text string) []smartCmd {
	var out []smartCmd
	for _, line := range strings.Split(text, "\n") {
		hits := keyHits(re, line)
		if len(hits) == 0 {
			continue
		}
		var keys []string
		for _, h := range hits {
			keys = append(keys, h.key)
		}
		rest := line[hits[0].end:]
		cs := reCmd.FindAllStringSubmatchIndex(rest, -1)
		for i, c := range cs {
			end := len(rest)
			if i+1 < len(cs) {
				end = cs[i+1][0]
			}
			name := strings.ToLower(rest[c[2]:c[3]])
			arg := strings.TrimSpace(rest[c[1]:end])
			for _, k := range keys {
				out = append(out, smartCmd{key: k, name: name, arg: arg})
			}
		}
	}
	return out
}

func (s *Service) runSmartCommits(ctx context.Context, r *repoInfo, g gitApplied) {
	e := g.ev
	var title, body string
	switch {
	case e.Kind == git.KindCommit && !e.Deleted:
		title, body = e.Title, e.Body
	case e.Kind == git.KindPR && e.State == "merged" && g.prev != "merged":
		title, body = e.Title, e.Body // the title's commands run when the pull request merges; its description's never do
	default:
		return
	}
	re := issueKeyRe(r.projects)
	if re == nil {
		return
	}
	var cmds []smartCmd
	if e.Kind == git.KindCommit {
		cmds = parseSmart(re, title+"\n"+body)
	} else {
		cmds = parseSmart(re, title)
		for _, c := range parseSmart(re, body) {
			c.body = true
			cmds = append(cmds, c)
		}
	}
	if len(cmds) == 0 {
		return
	}
	def := r.defaultBranch
	if e.DefaultBranch != "" {
		def = e.DefaultBranch
	}
	branch := e.Branch
	if e.Kind == git.KindPR {
		branch = e.Base
	}
	for i, c := range cmds {
		idx := i
		var is issueRef
		var mode string
		var mirrored bool
		err := s.DB.QueryRow(ctx, `SELECT i.id::text,i.key,i.project_id::text,st.name,p.git_smart_commits,p.mirror_of IS NOT NULL FROM issue i JOIN project p ON p.id=i.project_id JOIN status st ON st.id=i.status_id
			WHERE i.deleted_at IS NULL AND (i.key=$1 OR i.id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1)) LIMIT 1`, c.key).Scan(&is.id, &is.key, &is.projectID, &is.status, &mode, &mirrored)
		if err != nil || mode == "off" {
			continue // an unknown key, or a project that has not turned smart commits on: not a command
		}
		inRepo := false
		for _, pid := range r.projects {
			inRepo = inRepo || pid == is.projectID
		}
		if !inRepo {
			continue
		}
		tag, err := s.DB.Exec(ctx, `INSERT INTO git_smart_run(object_id,issue_key,idx,command,outcome,reason) VALUES($1::uuid,$2,$3,$4,'failed','claimed') ON CONFLICT DO NOTHING`, g.objID, is.key, idx, "#"+c.name)
		if err != nil || tag.RowsAffected() == 0 {
			continue // already handled: a replay never runs a command twice
		}
		outcome, reason, uid := s.runSmart(ctx, r, g, e, is, mode, def, branch, c)
		var uidArg any
		if uid != "" {
			uidArg = uid
		}
		s.DB.Exec(ctx, `UPDATE git_smart_run SET outcome=$4,reason=$5,user_id=$6::uuid,at=$7 WHERE object_id::text=$1 AND issue_key=$2 AND idx=$3`, g.objID, is.key, idx, outcome, reason, uidArg, s.Now())
		s.auditSmart(ctx, r, g, is, c, outcome, reason, uid)
	}
}

func (s *Service) auditSmart(ctx context.Context, r *repoInfo, g gitApplied, is issueRef, c smartCmd, outcome, reason, uid string) {
	act := Actor{Label: g.label(r.provider)}
	if uid != "" {
		act.ID = uid
	}
	s.tx(ctx, func(tx pgx.Tx) error {
		return s.audit(ctx, tx, act, "git.smart_commit", is.key, map[string]any{"command": "#" + c.name, "outcome": outcome, "reason": reason, "repo": g.ev.Repo, "sha": g.ev.SHA, "object": g.ev.ExtID, "kind": g.ev.Kind})
	})
}

func (s *Service) runSmart(ctx context.Context, r *repoInfo, g gitApplied, e git.Event, is issueRef, mode, def, branch string, c smartCmd) (outcome, reason, uid string) {
	if c.body {
		return "ignored", "commands in a pull request description are never run; put them in a commit message or the title", ""
	}
	if mode == "default_branch" && (def == "" || branch != def) {
		return "ignored", fmt.Sprintf("smart commits run only on the default branch (%s); this is on %q", gitOr(def, "unknown"), branch), ""
	}
	u := s.gitUser(ctx, s.DB, r.provider, e.Login, e.VerifiedEmail)
	if u == nil {
		who := e.Login
		if who == "" {
			who = "no provider account asserted"
		}
		return "ignored", "the author is not linked to a Tasklane user (" + who + "); an administrator can link the account", ""
	}
	act, err := s.actorFor(ctx, *u)
	if err != nil {
		return "ignored", "the linked person is no longer active", *u
	}
	act.Label = act.Label + " (via " + g.label(r.provider) + ")"
	switch c.name {
	case "comment":
		if c.arg == "" {
			return "ignored", "#comment needs some text", *u
		}
		text := c.arg
		if e.URL != "" {
			text += "\n\n" + e.URL
		}
		if _, err := s.AddComment(ctx, act, is.key, text, false); err != nil {
			return refusal(err, *u)
		}
		return "applied", "comment added", *u
	case "time":
		return "ignored", "#time is not available: time tracking is not built yet", *u
	}
	name := strings.NewReplacer("-", " ", "_", " ").Replace(c.name)
	tid, to, terr := s.transitionByName(ctx, is.id, name)
	if terr != nil {
		return "ignored", fmt.Sprintf("unknown command #%s: no transition of that name from %q", c.name, is.status), *u
	}
	if _, err := s.Transition(ctx, act, is.key, tid, nil); err != nil {
		return refusal(err, *u)
	}
	return "applied", fmt.Sprintf("moved from %q to %q", is.status, to), *u
}

func gitOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// refusal classifies an error from acting as a person: not allowed is an ignored command, anything else a failure.
func refusal(err error, uid string) (string, string, string) {
	var inv *InvalidError
	switch {
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrNotFound):
		return "ignored", "not permitted: the person may not do that on this issue", uid
	case errors.As(err, &inv):
		return "ignored", "refused: " + trimErr(err), uid
	}
	return "failed", trimErr(err), uid
}
