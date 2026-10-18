package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/git"
)

// DevItem is one row of the Development section.
type DevItem struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Repo      string    `json:"repo"`
	Title     string    `json:"title"`
	State     string    `json:"state,omitempty"`
	Review    string    `json:"review,omitempty"`
	Author    string    `json:"author,omitempty"`
	SHA       string    `json:"sha,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Env       string    `json:"environment,omitempty"`
	URL       string    `json:"url,omitempty"`
	Manual    bool      `json:"manual,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DevPanel is the development section of a ticket: Tasklane's own store only, the provider is never called on this path.
type DevPanel struct {
	Summary     string    `json:"summary"`
	PRs         []DevItem `json:"pull_requests"`
	Branches    []DevItem `json:"branches"`
	Commits     []DevItem `json:"commits"`
	Builds      []DevItem `json:"builds"`
	Deployments []DevItem `json:"deployments"`
	Build       string    `json:"build,omitempty"` // passing, running or failing across the builds shown
	// Automation is what rules and smart commands did to this issue, or why they did not: the record people look for when an
	// issue moved, or a command in a commit seemed to be ignored.
	Automation []DevAuto `json:"automation,omitempty"`
}

type DevAuto struct {
	Source  string    `json:"source"` // rule or smart_commit
	Outcome string    `json:"outcome"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
}

const devCommitCap = 25

func (s *Service) devItems(ctx context.Context, q string, args ...any) ([]DevItem, error) {
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DevItem{}
	for rows.Next() {
		var it DevItem
		if err := rows.Scan(&it.ID, &it.Kind, &it.Repo, &it.Title, &it.State, &it.Review, &it.Author, &it.SHA, &it.Branch, &it.Env, &it.URL, &it.Manual, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

const devCols = `o.id::text,o.kind,r.full_name,o.title,o.state,o.review,o.author,o.sha,o.branch,o.env,o.url`

// Development returns the ticket's development panel. Reading needs the same right as reading the issue; deleted issues
// and issues the caller cannot see are indistinguishable from missing ones.
func (s *Service) Development(ctx context.Context, a Actor, key string) (DevPanel, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return DevPanel{}, err
	}
	d := DevPanel{}
	linked := `FROM git_link l JOIN git_object o ON o.id=l.object_id JOIN git_repo r ON r.id=o.repo_id WHERE l.issue_id::text=$1`
	if d.PRs, err = s.devItems(ctx, `SELECT `+devCols+`,l.manual,o.provider_at `+linked+` AND o.kind='pr' ORDER BY o.provider_at DESC`, is.ID); err != nil {
		return d, err
	}
	if d.Branches, err = s.devItems(ctx, `SELECT `+devCols+`,l.manual,o.provider_at `+linked+` AND o.kind='branch' ORDER BY o.provider_at DESC, o.title`, is.ID); err != nil {
		return d, err
	}
	if d.Commits, err = s.devItems(ctx, `SELECT `+devCols+`,l.manual,o.provider_at `+linked+` AND o.kind='commit' ORDER BY o.provider_at DESC LIMIT $2`, is.ID, devCommitCap); err != nil {
		return d, err
	}
	// builds belong to the pull requests and branches by head commit; deployments by any linked commit
	if d.Builds, err = s.devItems(ctx, `SELECT DISTINCT ON (b.repo_id,b.title,b.sha) b.id::text,b.kind,r.full_name,b.title,b.state,b.review,b.author,b.sha,b.branch,b.env,b.url,false,b.provider_at
		FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.kind IN ('pr','branch') AND o.sha<>''
		JOIN git_object b ON b.repo_id=o.repo_id AND b.kind='build' AND b.sha=o.sha JOIN git_repo r ON r.id=b.repo_id
		WHERE l.issue_id::text=$1 ORDER BY b.repo_id,b.title,b.sha,b.provider_at DESC`, is.ID); err != nil {
		return d, err
	}
	if d.Deployments, err = s.devItems(ctx, `SELECT DISTINCT ON (x.repo_id,x.env) x.id::text,x.kind,r.full_name,x.title,x.state,x.review,x.author,x.sha,x.branch,x.env,x.url,false,x.provider_at
		FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.sha<>'' AND o.kind IN ('pr','branch','commit')
		JOIN git_object x ON x.repo_id=o.repo_id AND x.kind='deployment' AND x.sha=o.sha JOIN git_repo r ON r.id=x.repo_id
		WHERE l.issue_id::text=$1 ORDER BY x.repo_id,x.env,x.provider_at DESC`, is.ID); err != nil {
		return d, err
	}
	d.Build = ""
	for _, b := range d.Builds {
		switch {
		case b.State == "failing":
			d.Build = "failing"
		case b.State == "running" && d.Build != "failing":
			d.Build = "running"
		case b.State == "passing" && d.Build == "":
			d.Build = "passing"
		}
	}
	d.Summary = devSummary(d)
	// the last few things code events did here (their reasons name no one's private details: a person is never named)
	rows, err := s.DB.Query(ctx, `SELECT 'rule',r.outcome,g.name||': '||r.detail,r.at FROM git_rule_run r JOIN git_rule g ON g.id=r.rule_id WHERE r.issue_id::text=$1 AND r.detail<>'claimed'
		UNION ALL SELECT 'smart_commit',m.outcome,m.command||': '||m.reason,m.at FROM git_smart_run m WHERE m.issue_key=$2 AND m.reason<>'claimed' ORDER BY 4 DESC LIMIT 10`, is.ID, is.Key)
	if err == nil {
		for rows.Next() {
			var x DevAuto
			rows.Scan(&x.Source, &x.Outcome, &x.Text, &x.At)
			d.Automation = append(d.Automation, x)
		}
		rows.Close()
	}
	return d, nil
}

func devSummary(d DevPanel) string {
	var parts []string
	open, merged := 0, 0
	for _, p := range d.PRs {
		switch p.State {
		case "merged":
			merged++
		case "closed":
		default:
			open++
		}
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	if open > 0 {
		parts = append(parts, plural(open, "PR open", "PRs open"))
	}
	if merged > 0 {
		parts = append(parts, plural(merged, "PR merged", "PRs merged"))
	}
	if n := len(d.Commits); n > 0 {
		parts = append(parts, plural(n, "commit", "commits"))
	}
	if len(d.Branches) > 0 && len(d.PRs) == 0 {
		parts = append(parts, plural(len(d.Branches), "branch", "branches"))
	}
	if d.Build != "" {
		parts = append(parts, "build "+d.Build)
	}
	for _, x := range d.Deployments {
		if x.State == "success" {
			parts = append(parts, "deployed to "+x.Env)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

var (
	ghURL = regexp.MustCompile(`^/([^/]+/[^/]+)/(pull/(\d+)|commit/([0-9a-f]{7,40})|tree/(.+))$`)
	glURL = regexp.MustCompile(`^/(.+?)/-/(merge_requests/(\d+)|commit/([0-9a-f]{7,40})|tree/(.+))$`)
)

// AddGitLink links an issue to an object the integration already knows, given its URL (a pull request, a commit or a
// branch page). Manual links are never removed by automatic relinking.
func (s *Service) AddGitLink(ctx context.Context, a Actor, key, link string) error {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" {
		return invalid("paste the address of a pull request, merge request, commit or branch")
	}
	var full, kind, ext string
	for _, re := range []*regexp.Regexp{ghURL, glURL} {
		if m := re.FindStringSubmatch(strings.TrimSuffix(u.Path, "/")); m != nil {
			full = m[1]
			switch {
			case m[3] != "":
				kind, ext = git.KindPR, m[3]
			case m[4] != "":
				kind, ext = git.KindCommit, m[4]
			default:
				kind, ext = git.KindBranch, m[5]
			}
			break
		}
	}
	if kind == "" {
		return invalid("that does not look like a pull request, merge request, commit or branch address")
	}
	var oid string
	q := `SELECT o.id::text FROM git_object o JOIN git_repo r ON r.id=o.repo_id JOIN git_connection c ON c.id=r.connection_id
		WHERE lower(r.full_name)=lower($1) AND o.kind=$2 AND o.ext_id=$3 AND c.base_url<>'' AND EXISTS (SELECT 1 FROM git_repo_project rp JOIN project p ON p.id=rp.project_id WHERE rp.repo_id=r.id AND p.key=$4)`
	if kind == git.KindCommit { // a short sha matches by prefix
		q = strings.Replace(q, "o.ext_id=$3", "o.ext_id LIKE $3||'%'", 1)
	}
	if err := s.DB.QueryRow(ctx, q, full, kind, ext, is.Project).Scan(&oid); err != nil {
		if err == pgx.ErrNoRows {
			return invalid("that is not known yet: the repository must be connected and mapped to this project, and the item appears after its next event or sync")
		}
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO git_link(issue_id,object_id,manual) VALUES($1::uuid,$2::uuid,true) ON CONFLICT (issue_id,object_id) DO UPDATE SET manual=true`, is.ID, oid)
	if err == nil {
		s.emit(Event{Type: "issue.updated", Issue: is.Key, Fields: []string{"development"}, Actor: a.Label, ActorID: a.ID})
	}
	return err
}

// RemoveGitLink removes a link. An automatic link comes back only if the text still names the issue when the item next changes.
func (s *Service) RemoveGitLink(ctx context.Context, a Actor, key, objectID string) error {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM git_link WHERE issue_id::text=$1 AND object_id::text=$2`, is.ID, objectID)
	if err != nil && strings.Contains(err.Error(), "invalid input syntax") {
		return ErrNotFound
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err == nil {
		s.emit(Event{Type: "issue.updated", Issue: is.Key, Fields: []string{"development"}, Actor: a.Label, ActorID: a.ID})
	}
	return err
}

// GitClientFor builds the provider client for a connection under its egress policy.
func (s *Service) GitClientFor(ctx context.Context, connID string) (*git.Client, bool, error) {
	var prov, base string
	var tokEnc []byte
	var priv bool
	if err := s.DB.QueryRow(ctx, `SELECT provider,base_url,token_enc,allow_private FROM git_connection WHERE id::text=$1`, connID).Scan(&prov, &base, &tokEnc, &priv); err != nil {
		return nil, false, ErrNotFound
	}
	tok, err := s.unseal(ctx, tokEnc)
	if err != nil {
		return nil, priv, err
	}
	pol := egress.Policy{AllowPrivate: priv, MaxBody: 32 << 20}
	if s.GitEgress != nil {
		pol = s.GitEgress(pol)
	}
	return &git.Client{Provider: prov, BaseURL: base, Token: tok, HTTP: pol.Client(), ETags: map[string]string{}, MaxWait: 30 * time.Second, Now: s.Now}, priv, nil
}

// SyncGitRepo backfills or reconciles one repository: it reads recent pull requests, branches, commits and builds and
// applies them through the same path as webhooks, so the provider's newer version always wins. The first run goes back
// `backfill_days`; later runs go back to just before the last one. A provider rate limit is recorded, not retried hot.
func (s *Service) SyncGitRepo(ctx context.Context, a Actor, repoID string) (int, error) {
	if err := adminOnly(a); err != nil {
		return 0, err
	}
	return s.syncGitRepo(ctx, repoID)
}

func (s *Service) syncGitRepo(ctx context.Context, repoID string) (int, error) {
	var connID, full string
	var last *time.Time
	var etagsRaw []byte
	var days int
	var disabled bool
	err := s.DB.QueryRow(ctx, `SELECT r.connection_id::text,r.full_name,r.last_sync_at,r.etags,c.backfill_days,c.disabled FROM git_repo r JOIN git_connection c ON c.id=r.connection_id WHERE r.id::text=$1`, repoID).
		Scan(&connID, &full, &last, &etagsRaw, &days, &disabled)
	if err == pgx.ErrNoRows || (err != nil && strings.Contains(err.Error(), "invalid input syntax")) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if disabled {
		return 0, invalid("this connection was disconnected and is kept read-only: nothing is synced")
	}
	cl, _, err := s.GitClientFor(ctx, connID)
	if err != nil {
		return 0, err
	}
	json.Unmarshal(etagsRaw, &cl.ETags)
	if cl.ETags == nil {
		cl.ETags = map[string]string{}
	}
	started := s.Now()
	since := started.AddDate(0, 0, -days)
	if last != nil {
		since = last.Add(-time.Hour)
	}
	evs, err := cl.Backfill(ctx, full, since, 1000)
	s.noteGitHealth(ctx, connID, err)
	if err != nil {
		s.DB.Exec(ctx, `UPDATE git_repo SET sync_error=$2 WHERE id::text=$1`, repoID, trimErr(err))
		return 0, err
	}
	if err := s.applyGitEvents(ctx, connID, evs, false); err != nil {
		s.DB.Exec(ctx, `UPDATE git_repo SET sync_error=$2 WHERE id::text=$1`, repoID, trimErr(err))
		return 0, err
	}
	et, _ := json.Marshal(cl.ETags)
	s.DB.Exec(ctx, `UPDATE git_repo SET last_sync_at=$2,sync_error='',etags=$3 WHERE id::text=$1`, repoID, started, et)
	return len(evs), nil
}

// RunGitWorkers applies the webhook inbox continuously and reconciles one repository at a time every few minutes, at low
// priority, so interactive requests never wait on the provider.
func (s *Service) RunGitWorkers(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastRec := time.Time{}
	for {
		if _, err := s.ProcessGitInbox(ctx, 100); err != nil && ctx.Err() == nil {
			logf("git inbox: %v", err)
		}
		if time.Since(lastRec) > 5*time.Minute {
			lastRec = time.Now()
			var id string
			if s.DB.QueryRow(ctx, `SELECT r.id::text FROM git_repo r JOIN git_connection c ON c.id=r.connection_id WHERE c.token_enc IS NOT NULL ORDER BY r.last_sync_at NULLS FIRST LIMIT 1`).Scan(&id) == nil {
				if _, err := s.syncGitRepo(ctx, id); err != nil && ctx.Err() == nil {
					logf("git sync: %v", err)
				}
			}
			s.DB.Exec(ctx, `DELETE FROM git_inbox WHERE processed_at IS NOT NULL AND processed_at < now() - interval '7 days'`)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
