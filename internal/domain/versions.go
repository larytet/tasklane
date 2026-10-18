package domain

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Versions (SPEC 12): a project's releases. An issue is fixed in, or affects, any number of versions of its own project. Anyone who can
// see the project sees its versions; project administrators manage them; people who can edit an issue set its versions. A version can
// be released, which warns about the issues in it that are not done, and can move them on to another version.

type Version struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Start       string `json:"start_date,omitempty"`
	ReleaseDate string `json:"release_date,omitempty"`
	Released    bool   `json:"released"`
	ReleasedAt  string `json:"released_at,omitempty"`
	Archived    bool   `json:"archived"`
	// Overdue: not released and its release date has passed.
	Overdue bool `json:"overdue,omitempty"`
	// CanManage: the caller administers the project (release, edit, delete).
	CanManage bool             `json:"can_manage"`
	Progress  *VersionProgress `json:"progress,omitempty"`
}

// VersionProgress counts the issues whose fix version this is, by status category, for the caller (who sees only what they may read).
type VersionProgress struct {
	Todo       int `json:"todo"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
	Total      int `json:"total"`
	Unresolved int `json:"unresolved"` // not done: what a release would leave behind
}

type VersionIn struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Start       *string `json:"start_date"` // YYYY-MM-DD; "" clears
	ReleaseDate *string `json:"release_date"`
	Archived    *bool   `json:"archived"`
}

const versionCols = `v.id::text, p.key, v.name, v.description, COALESCE(to_char(v.start_date,'YYYY-MM-DD'),''), COALESCE(to_char(v.release_date,'YYYY-MM-DD'),''), v.released, COALESCE(to_char(v.released_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),''), v.archived`

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	err := row.Scan(&v.ID, &v.Project, &v.Name, &v.Description, &v.Start, &v.ReleaseDate, &v.Released, &v.ReleasedAt, &v.Archived)
	return v, err
}

func normDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return "", invalid("a date is written YYYY-MM-DD")
	}
	return s, nil
}

func normVersionName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if n == "" || len([]rune(n)) > 60 {
		return "", invalid("a version name is 1 to 60 characters")
	}
	return n, nil
}

// loadVersion loads a version and the caller's role in its project; a person who cannot see the project is told it does not exist.
func (s *Service) loadVersion(ctx context.Context, a Actor, id string) (v Version, pid, role string, err error) {
	row := s.DB.QueryRow(ctx, `SELECT `+versionCols+`, p.id::text FROM version v JOIN project p ON p.id=v.project_id WHERE v.id::text=$1`, id)
	err = row.Scan(&v.ID, &v.Project, &v.Name, &v.Description, &v.Start, &v.ReleaseDate, &v.Released, &v.ReleasedAt, &v.Archived, &pid)
	if err != nil && (err == pgx.ErrNoRows || strings.Contains(err.Error(), "invalid input syntax for type uuid")) {
		err = ErrNotFound
	}
	if err != nil {
		return v, "", "", err
	}
	if role = s.role(ctx, a, pid); role == "" {
		return v, "", "", ErrNotFound
	}
	return v, pid, role, nil
}

func (s *Service) decorate(ctx context.Context, a Actor, vs []Version, withProgress, manage bool) error {
	today := s.Now().UTC().Format("2006-01-02")
	for i := range vs {
		vs[i].CanManage = manage
		vs[i].Overdue = !vs[i].Released && vs[i].ReleaseDate != "" && vs[i].ReleaseDate < today
		if !withProgress {
			continue
		}
		var p VersionProgress
		err := s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE st.category='todo'), count(*) FILTER (WHERE st.category='in_progress'), count(*) FILTER (WHERE st.category='done')
			FROM issue_version iv JOIN issue i ON i.id=iv.issue_id AND i.deleted_at IS NULL AND ($2 OR NOT i.restricted) JOIN status st ON st.id=i.status_id
			WHERE iv.version_id::text=$1 AND iv.kind='fix'`, vs[i].ID, a.Admin).Scan(&p.Todo, &p.InProgress, &p.Done)
		if err != nil {
			return err
		}
		p.Total = p.Todo + p.InProgress + p.Done
		p.Unresolved = p.Todo + p.InProgress
		vs[i].Progress = &p
	}
	return nil
}

// ListVersions lists a project's versions, unreleased first by release date, with progress; archived ones only when asked.
func (s *Service) ListVersions(ctx context.Context, a Actor, projectKey string, archived bool) ([]Version, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	role := s.role(ctx, a, pid)
	if role == "" {
		return nil, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT `+versionCols+` FROM version v JOIN project p ON p.id=v.project_id WHERE v.project_id=$1 AND ($2 OR NOT v.archived)
		ORDER BY v.released, v.release_date NULLS LAST, lower(v.name)`, pid, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Version{}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	rows.Close()
	return out, s.decorate(ctx, a, out, true, role == "admin")
}

// CanManageVersions reports whether the caller administers the project (so a screen can offer what only an administrator may do).
func (s *Service) CanManageVersions(ctx context.Context, a Actor, projectKey string) bool {
	pid, err := s.projectID(ctx, projectKey)
	return err == nil && s.role(ctx, a, pid) == "admin"
}

// GetVersion returns one version with its progress.
func (s *Service) GetVersion(ctx context.Context, a Actor, id string) (Version, error) {
	v, _, role, err := s.loadVersion(ctx, a, id)
	if err != nil {
		return v, err
	}
	vs := []Version{v}
	err = s.decorate(ctx, a, vs, true, role == "admin")
	return vs[0], err
}

func (s *Service) versionAdmin(ctx context.Context, a Actor, projectKey string) (string, func(), error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return "", nil, err
	}
	switch s.role(ctx, a, pid) {
	case "":
		return "", nil, ErrNotFound
	case "admin":
	default:
		return "", nil, ErrForbidden
	}
	end, err := s.admit(ctx, projectKey)
	return pid, end, err
}

func versionConflict(err error) error {
	if err != nil && strings.Contains(err.Error(), "version_project_name_uq") {
		return invalid("this project already has a version with that name")
	}
	return err
}

// CreateVersion adds a version to a project (project administrators).
func (s *Service) CreateVersion(ctx context.Context, a Actor, projectKey string, in VersionIn) (Version, error) {
	pid, end, err := s.versionAdmin(ctx, a, projectKey)
	if err != nil {
		return Version{}, err
	}
	defer end()
	if in.Name == nil {
		return Version{}, invalid("a version needs a name")
	}
	name, err := normVersionName(*in.Name)
	if err != nil {
		return Version{}, err
	}
	var start, rel string
	if in.Start != nil {
		if start, err = normDate(*in.Start); err != nil {
			return Version{}, err
		}
	}
	if in.ReleaseDate != nil {
		if rel, err = normDate(*in.ReleaseDate); err != nil {
			return Version{}, err
		}
	}
	if start != "" && rel != "" && rel < start {
		return Version{}, invalid("the release date is before the start date")
	}
	desc := ""
	if in.Description != nil {
		desc = *in.Description
	}
	var id string
	err = s.DB.QueryRow(ctx, `INSERT INTO version(project_id,name,description,start_date,release_date) VALUES($1,$2,$3,NULLIF($4,'')::date,NULLIF($5,'')::date) RETURNING id::text`, pid, name, desc, start, rel).Scan(&id)
	if err != nil {
		return Version{}, versionConflict(err)
	}
	return s.GetVersion(ctx, a, id)
}

// UpdateVersion changes a version's name, description, dates or archived state (project administrators).
func (s *Service) UpdateVersion(ctx context.Context, a Actor, id string, in VersionIn) (Version, error) {
	v, _, _, err := s.loadVersion(ctx, a, id)
	if err != nil {
		return v, err
	}
	_, end, err := s.versionAdmin(ctx, a, v.Project)
	if err != nil {
		return v, err
	}
	defer end()
	if in.Name != nil {
		if v.Name, err = normVersionName(*in.Name); err != nil {
			return v, err
		}
	}
	if in.Description != nil {
		v.Description = *in.Description
	}
	if in.Start != nil {
		if v.Start, err = normDate(*in.Start); err != nil {
			return v, err
		}
	}
	if in.ReleaseDate != nil {
		if v.ReleaseDate, err = normDate(*in.ReleaseDate); err != nil {
			return v, err
		}
	}
	if in.Archived != nil {
		v.Archived = *in.Archived
	}
	if v.Start != "" && v.ReleaseDate != "" && v.ReleaseDate < v.Start {
		return v, invalid("the release date is before the start date")
	}
	if _, err = s.DB.Exec(ctx, `UPDATE version SET name=$2,description=$3,start_date=NULLIF($4,'')::date,release_date=NULLIF($5,'')::date,archived=$6 WHERE id::text=$1`,
		id, v.Name, v.Description, v.Start, v.ReleaseDate, v.Archived); err != nil {
		return v, versionConflict(err)
	}
	return s.GetVersion(ctx, a, id)
}

// DeleteVersion removes a version. Issues that had it are moved to moveTo (another version of the project) when it is given, and
// otherwise simply lose it.
func (s *Service) DeleteVersion(ctx context.Context, a Actor, id, moveTo string) error {
	v, pid, _, err := s.loadVersion(ctx, a, id)
	if err != nil {
		return err
	}
	_, end, err := s.versionAdmin(ctx, a, v.Project)
	if err != nil {
		return err
	}
	defer end()
	return s.tx(ctx, func(tx pgx.Tx) error {
		if moveTo != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM version WHERE id::text=$1 AND project_id=$2 AND id::text<>$3)`, moveTo, pid, id).Scan(&ok); err != nil || !ok {
				return invalid("the version to move issues to must be another version of this project")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO issue_version(issue_id,version_id,kind) SELECT issue_id,$2::uuid,kind FROM issue_version WHERE version_id::text=$1 ON CONFLICT DO NOTHING`, id, moveTo); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM version WHERE id::text=$1`, id)
		return err
	})
}

// VersionIssue is an issue named in a release warning or in release notes.
type VersionIssue struct {
	Key      string   `json:"key"`
	Summary  string   `json:"summary"`
	Type     string   `json:"type"`
	Status   string   `json:"status"`
	Category string   `json:"status_category"`
	Assignee string   `json:"assignee,omitempty"`
	PRs      []string `json:"pull_requests,omitempty"`
}

type ReleaseIn struct {
	// MoveTo is another unreleased version that the issues which are not done are moved to
	MoveTo string `json:"move_to"`
	// Confirm releases although issues that are not done stay on the version
	Confirm bool `json:"confirm"`
	// Date is the release date (default: today)
	Date string `json:"date"`
}

type ReleaseResult struct {
	Released   bool           `json:"released"`
	Version    Version        `json:"version"`
	Unresolved []VersionIssue `json:"unresolved,omitempty"` // the warning: issues not done on a version that was not released, or that were moved or left
	Moved      int            `json:"moved,omitempty"`
}

func (s *Service) versionIssues(ctx context.Context, a Actor, versionID string, notDone bool) ([]VersionIssue, error) {
	rows, err := s.DB.Query(ctx, `SELECT i.key,i.summary,t.name,st.name,st.category,COALESCE(asg.name,'')
		FROM issue_version iv JOIN issue i ON i.id=iv.issue_id AND i.deleted_at IS NULL AND ($3 OR NOT i.restricted) JOIN status st ON st.id=i.status_id
		JOIN issue_type t ON t.id=i.issue_type_id LEFT JOIN app_user asg ON asg.id=i.assignee_id
		WHERE iv.version_id::text=$1 AND iv.kind='fix' AND ($2 = false OR st.category<>'done') ORDER BY t.level DESC, i.number`, versionID, notDone, a.Admin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VersionIssue{}
	for rows.Next() {
		var x VersionIssue
		if err := rows.Scan(&x.Key, &x.Summary, &x.Type, &x.Status, &x.Category, &x.Assignee); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ReleaseVersion releases a version (project administrators). When issues that are not done remain on it, nothing happens until the
// caller says what to do: move them to another version (MoveTo) or release anyway (Confirm); the first answer is the warning.
func (s *Service) ReleaseVersion(ctx context.Context, a Actor, id string, in ReleaseIn) (ReleaseResult, error) {
	v, pid, _, err := s.loadVersion(ctx, a, id)
	if err != nil {
		return ReleaseResult{}, err
	}
	_, end, err := s.versionAdmin(ctx, a, v.Project)
	if err != nil {
		return ReleaseResult{}, err
	}
	defer end()
	if v.Released {
		return ReleaseResult{}, invalid("this version is already released")
	}
	date := s.Now().UTC().Format("2006-01-02")
	if in.Date != "" {
		if date, err = normDate(in.Date); err != nil {
			return ReleaseResult{}, err
		}
	}
	open, err := s.versionIssues(ctx, a, id, true)
	if err != nil {
		return ReleaseResult{}, err
	}
	if len(open) > 0 && in.MoveTo == "" && !in.Confirm {
		return ReleaseResult{Version: v, Unresolved: open}, nil
	}
	res := ReleaseResult{Released: true}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if in.MoveTo != "" && len(open) > 0 {
			var toName string
			var released bool
			err := tx.QueryRow(ctx, `SELECT name,released FROM version WHERE id::text=$1 AND project_id=$2 AND id::text<>$3`, in.MoveTo, pid, id).Scan(&toName, &released)
			if err == pgx.ErrNoRows {
				return invalid("the version to move issues to must be another version of this project")
			}
			if err != nil {
				return err
			}
			if released {
				return invalid("issues cannot be moved to a version that is already released")
			}
			for _, x := range open {
				var iid string
				if err := tx.QueryRow(ctx, `SELECT id::text FROM issue WHERE key=$1 FOR UPDATE`, x.Key).Scan(&iid); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `DELETE FROM issue_version WHERE issue_id::text=$1 AND version_id::text=$2 AND kind='fix'`, iid, id); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO issue_version(issue_id,version_id,kind) VALUES($1,$2,'fix') ON CONFLICT DO NOTHING`, iid, in.MoveTo); err != nil {
					return err
				}
				before, after := v.Name, toName
				if err := s.hist(ctx, tx, iid, a, "fix_versions", &before, &after); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE issue SET version=version+1, updated_at=now() WHERE id::text=$1`, iid); err != nil {
					return err
				}
				res.Moved++
			}
		}
		_, err := tx.Exec(ctx, `UPDATE version SET released=true, released_at=$2, release_date=$3::date WHERE id::text=$1`, id, s.Now(), date)
		return err
	})
	if err != nil {
		return ReleaseResult{}, err
	}
	if res.Version, err = s.GetVersion(ctx, a, id); err != nil {
		return res, err
	}
	if res.Moved == 0 && in.MoveTo == "" {
		res.Unresolved = open // released with these left on it: said, not hidden
	}
	return res, nil
}

// UnreleaseVersion takes a version back to unreleased (project administrators).
func (s *Service) UnreleaseVersion(ctx context.Context, a Actor, id string) (Version, error) {
	v, _, _, err := s.loadVersion(ctx, a, id)
	if err != nil {
		return v, err
	}
	_, end, err := s.versionAdmin(ctx, a, v.Project)
	if err != nil {
		return v, err
	}
	defer end()
	if !v.Released {
		return v, invalid("this version is not released")
	}
	if _, err := s.DB.Exec(ctx, `UPDATE version SET released=false, released_at=NULL WHERE id::text=$1`, id); err != nil {
		return v, err
	}
	return s.GetVersion(ctx, a, id)
}

// IssueVersions are an issue's versions by name.
type IssueVersions struct {
	Fix     []string `json:"fix"`
	Affects []string `json:"affects"`
}

func (s *Service) issueVersions(ctx context.Context, issueID string) IssueVersions {
	out := IssueVersions{Fix: []string{}, Affects: []string{}}
	rows, err := s.DB.Query(ctx, `SELECT iv.kind, v.name FROM issue_version iv JOIN version v ON v.id=iv.version_id WHERE iv.issue_id::text=$1 ORDER BY v.released, v.release_date NULLS LAST, lower(v.name)`, issueID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, n string
		rows.Scan(&k, &n)
		if k == "fix" {
			out.Fix = append(out.Fix, n)
		} else {
			out.Affects = append(out.Affects, n)
		}
	}
	return out
}

type SetVersionsIn struct {
	Fix     *[]string `json:"fix"`
	Affects *[]string `json:"affects"`
}

// SetIssueVersions replaces an issue's fix versions and/or affects versions (people who can edit the issue). Names are the project's
// versions, matched without regard to case; an archived version cannot be newly added.
func (s *Service) SetIssueVersions(ctx context.Context, a Actor, key string, in SetVersionsIn) (IssueVersions, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return IssueVersions{}, err
	}
	if !canWrite(role) || is.Archived != nil {
		return IssueVersions{}, ErrForbidden
	}
	if in.Fix == nil && in.Affects == nil {
		return IssueVersions{}, invalid("no changes")
	}
	end, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return IssueVersions{}, perr
	}
	defer end()
	pid, err := s.projectID(ctx, is.Project)
	if err != nil {
		return IssueVersions{}, err
	}
	before := s.issueVersions(ctx, is.ID)
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var cur int64
		if err := tx.QueryRow(ctx, `SELECT version FROM issue WHERE id=$1 FOR UPDATE`, is.ID).Scan(&cur); err != nil {
			return err
		}
		changed := []string{}
		for _, kv := range []struct {
			kind, field string
			names       *[]string
			old         []string
		}{{"fix", "fix_versions", in.Fix, before.Fix}, {"affects", "affects_versions", in.Affects, before.Affects}} {
			if kv.names == nil {
				continue
			}
			seen := map[string]bool{}
			var ids, names []string
			for _, n := range *kv.names {
				n = strings.TrimSpace(n)
				if n == "" || seen[strings.ToLower(n)] {
					continue
				}
				seen[strings.ToLower(n)] = true
				var vid, real string
				var arch bool
				err := tx.QueryRow(ctx, `SELECT id::text,name,archived FROM version WHERE project_id=$1 AND lower(name)=lower($2)`, pid, n).Scan(&vid, &real, &arch)
				if err == pgx.ErrNoRows {
					return invalid("the project has no version %q", n)
				}
				if err != nil {
					return err
				}
				had := false
				for _, o := range kv.old {
					had = had || strings.EqualFold(o, real)
				}
				if arch && !had {
					return invalid("version %q is archived", real)
				}
				ids, names = append(ids, vid), append(names, real)
			}
			sort.Strings(names)
			oldSorted := append([]string{}, kv.old...)
			sort.Strings(oldSorted)
			if strings.Join(names, "\x00") == strings.Join(oldSorted, "\x00") {
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM issue_version WHERE issue_id=$1 AND kind=$2`, is.ID, kv.kind); err != nil {
				return err
			}
			for _, vid := range ids {
				if _, err := tx.Exec(ctx, `INSERT INTO issue_version(issue_id,version_id,kind) VALUES($1,$2,$3)`, is.ID, vid, kv.kind); err != nil {
					return err
				}
			}
			b, n := strings.Join(oldSorted, ", "), strings.Join(names, ", ")
			if err := s.hist(ctx, tx, is.ID, a, kv.field, &b, &n); err != nil {
				return err
			}
			changed = append(changed, kv.field)
		}
		if len(changed) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE issue SET version=version+1, updated_at=now() WHERE id=$1`, is.ID); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+is.Key, "issue.updated", map[string]any{"key": is.Key, "fields": changed, "version": cur + 1, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return IssueVersions{}, err
	}
	out := s.issueVersions(ctx, is.ID)
	s.emit(Event{Type: "issue.updated", Issue: is.Key, Fields: []string{"versions"}, Actor: a.Label, ActorID: a.ID})
	return out, nil
}

// ReleaseNotes is the text of the release: what was done in the version, by kind of issue, with the merged pull requests the caller may
// see (a restricted organization hides the details of a repository the caller cannot see).
type ReleaseNotes struct {
	Version  Version        `json:"version"`
	Issues   []VersionIssue `json:"issues"`
	Markdown string         `json:"markdown"`
}

func (s *Service) ReleaseNotes(ctx context.Context, a Actor, id string) (ReleaseNotes, error) {
	v, _, _, err := s.loadVersion(ctx, a, id)
	if err != nil {
		return ReleaseNotes{}, err
	}
	is, err := s.versionIssues(ctx, a, id, false)
	if err != nil {
		return ReleaseNotes{}, err
	}
	for i := range is {
		rows, err := s.DB.Query(ctx, `SELECT o.title FROM git_link l JOIN git_object o ON o.id=l.object_id AND o.kind='pr' AND o.state='merged'
			WHERE l.issue_id=(SELECT id FROM issue WHERE key=$1) AND ($2 OR git_repo_visible(o.repo_id, $3::uuid, $4::timestamptz)) ORDER BY o.title`, is[i].Key, a.Admin, a.ID, s.Now())
		if err != nil {
			return ReleaseNotes{}, err
		}
		for rows.Next() {
			var t string
			rows.Scan(&t)
			is[i].PRs = append(is[i].PRs, t)
		}
		rows.Close()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s %s\n", v.Project, v.Name)
	if v.Released && v.ReleaseDate != "" {
		fmt.Fprintf(&b, "\nReleased %s.\n", v.ReleaseDate)
	}
	if v.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", v.Description)
	}
	byType := map[string][]VersionIssue{}
	var types []string
	for _, x := range is {
		if x.Category != "done" {
			continue
		}
		if _, ok := byType[x.Type]; !ok {
			types = append(types, x.Type)
		}
		byType[x.Type] = append(byType[x.Type], x)
	}
	for _, t := range types {
		fmt.Fprintf(&b, "\n## %s\n\n", t)
		for _, x := range byType[t] {
			fmt.Fprintf(&b, "- %s %s", x.Key, x.Summary)
			if len(x.PRs) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(x.PRs, "; "))
			}
			b.WriteString("\n")
		}
	}
	var open []VersionIssue
	for _, x := range is {
		if x.Category != "done" {
			open = append(open, x)
		}
	}
	if len(open) > 0 {
		b.WriteString("\n## Not done\n\n")
		for _, x := range open {
			fmt.Fprintf(&b, "- %s %s (%s)\n", x.Key, x.Summary, x.Status)
		}
	}
	if len(is) == 0 {
		b.WriteString("\nNo issues are fixed in this version.\n")
	}
	return ReleaseNotes{Version: v, Issues: is, Markdown: b.String()}, nil
}
