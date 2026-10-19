package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Third-party importers (SPEC 18.2, "basic support"): GitHub Issues, Trello, Linear and Asana. Each reads the tool's own export and builds
// the same bundle the generic importer builds, so everything else is shared: the dry run and its report, per-row problems that do not stop the
// good ones, re-importing the same file updating in place (every issue and comment carries the tool's own id), and rollback by import id.
// Basic means issues, comments, labels, assignees, status, dates and the links the source offers; no automation, boards or permissions, and
// no files are fetched. People are matched by an address: the exports mostly name people by login or name, so the caller says which address
// each belongs to (user_map); anyone left out is reported and left unassigned.
//
// The presets are data: the defaults below (and the values a request sends, which win) are the whole mapping.

type ToolImportIn struct {
	Project     string            `json:"project"`
	DryRun      bool              `json:"dry_run"`
	UserMap     map[string]string `json:"user_map"`     // login, username or name in the export -> email address here
	StatusMap   map[string]string `json:"status_map"`   // state, list, section or status in the export -> status here
	TypeMap     map[string]string `json:"type_map"`     // label (GitHub) -> issue type here
	PriorityMap map[string]string `json:"priority_map"` // priority in the export -> lowest..highest
	Epics       *bool             `json:"epics"`        // Linear: make an Epic of each project and put its issues under it (default true)
	Mapping     *JSONMapping      `json:"mapping"`      // the generic JSON importer: where the issues are and the path of each field
}

var importTools = map[string]string{"github": "GitHub Issues", "trello": "Trello", "linear": "Linear", "asana": "Asana", "json": "Generic JSON (JSONPath mapping)"}

// ImportTools lists the tools, for screens.
func ImportTools() map[string]string { return importTools }

// default statuses by words in the source's own name for the state
var (
	doneWords = regexp.MustCompile(`(?i)\b(done|complete|completed|closed|shipped|released|resolved|canceled|cancelled|duplicate|won't fix|wontfix)\b`)
	wipWords  = regexp.MustCompile(`(?i)\b(doing|in progress|started|wip|review|in review|testing|qa|active)\b`)
)

// presetCategory is the status category the words in a source's state name suggest.
func presetCategory(name string) string {
	switch {
	case doneWords.MatchString(name):
		return "done"
	case wipWords.MatchString(name):
		return "in_progress"
	}
	return "todo"
}

var linearPriority = map[string]string{"urgent": "highest", "high": "high", "medium": "medium", "low": "low", "no priority": "medium"}

type toolBuild struct {
	s     *Service
	pkey  string
	tool  string
	in    ToolImportIn
	d     *BundleData
	rep   *ImportReport
	users map[string]bool
	gap   map[string]bool
	seen  map[string]bool
	known map[string]string // the project's statuses by lower-case name
	byCat map[string]string // the first status of each category
}

func (b *toolBuild) note(k, msg string) {
	if !b.gap[k] {
		b.gap[k] = true
		b.rep.Gaps = append(b.rep.Gaps, msg)
	}
}

func lowerMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

// person resolves who the export names to an address; "" when it cannot, which is reported once.
func (b *toolBuild) person(who, role string) string {
	who = strings.TrimSpace(who)
	if who == "" {
		return ""
	}
	if strings.Contains(who, "@") {
		e := strings.ToLower(who)
		b.users[e] = true
		return e
	}
	if e, ok := lowerMap(b.in.UserMap)[strings.ToLower(who)]; ok && strings.Contains(e, "@") {
		e = strings.ToLower(e)
		b.users[e] = true
		return e
	}
	b.note("user:"+strings.ToLower(who), fmt.Sprintf("%s %q has no address in the user map: left out (add %q to user_map to keep them)", role, who, who))
	return ""
}

// status is the project's status for the source's state: the caller's mapping first (it must name a status the project has), otherwise the
// first status of the category the words in the name suggest.
func (b *toolBuild) status(name string) string {
	if t, ok := lowerMap(b.in.StatusMap)[strings.ToLower(strings.TrimSpace(name))]; ok {
		if real, ok := b.known[strings.ToLower(t)]; ok {
			return real
		}
		b.note("status:"+t, fmt.Sprintf("status_map sends %q to %q, which this project's workflow does not have: the nearest status is used", name, t))
	}
	if real, ok := b.known[strings.ToLower(strings.TrimSpace(name))]; ok { // the source's state is already one of ours
		return real
	}
	if st := b.byCat[presetCategory(name)]; st != "" {
		return st
	}
	return map[string]string{"todo": "To Do", "in_progress": "In Progress", "done": "Done"}[presetCategory(name)]
}

func (b *toolBuild) isDone(status string) bool {
	return strings.EqualFold(status, b.byCat["done"]) || (b.byCat["done"] == "" && strings.EqualFold(status, "Done"))
}

func (b *toolBuild) priority(v string) string {
	if t, ok := lowerMap(b.in.PriorityMap)[strings.ToLower(strings.TrimSpace(v))]; ok {
		v = t
	} else if p, ok := linearPriority[strings.ToLower(strings.TrimSpace(v))]; ok && b.tool == "linear" {
		v = p
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "medium"
	}
	if !priorities[v] {
		b.note("priority:"+v, fmt.Sprintf("priority %q is not mapped: medium is used", v))
		return "medium"
	}
	return v
}

func (b *toolBuild) label(l string) string {
	l = strings.TrimSpace(l)
	l = strings.Join(strings.Fields(l), "-")
	if len(l) > 50 {
		l = l[:50]
	}
	return l
}

var toolLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z0700", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", "Mon Jan 02 2006 15:04:05 GMT-0700", "01/02/2006", "1/2/2006"}

func toolTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if i := strings.Index(v, " ("); i > 0 { // "Tue Oct 19 2026 14:30:00 GMT+0000 (Coordinated Universal Time)"
		v = v[:i]
	}
	for _, l := range toolLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (b *toolBuild) add(i BIssue) bool {
	if b.seen[i.Key] {
		b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("%s appears twice in the file", i.Key))
		return false
	}
	b.seen[i.Key] = true
	if len([]rune(i.Summary)) > maxSummary {
		i.Summary = string([]rune(i.Summary)[:maxSummary])
		b.rep.Warnings = append(b.rep.Warnings, fmt.Sprintf("%s: the summary was shortened to %d characters", i.Key, maxSummary))
	}
	i.Summary = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(i.Summary, "\r", " "), "\n", " "))
	i.Description, _ = Sanitize(i.Description)
	if i.Labels == nil {
		i.Labels = []string{}
	}
	if i.Updated.IsZero() {
		i.Updated = i.Created
	}
	i.ExternalID, i.Project = i.Key, b.pkey
	b.d.Issues = append(b.d.Issues, i)
	return true
}

func (b *toolBuild) comment(issue, id, author, body string, at time.Time) {
	body, _ = Sanitize(strings.TrimSpace(body))
	if body == "" {
		return
	}
	h := sha256.Sum256([]byte(issue + "\x00" + id))
	b.d.Comments = append(b.d.Comments, BComment{ID: b.tool + ":" + hex.EncodeToString(h[:8]), Issue: issue, Author: author, Body: body, Created: at})
}

// ImportTool imports an export of one of the tools into an existing project (project administrators). Like every import it can be a dry run,
// and the same file imported again updates what it made.
func (s *Service) ImportTool(ctx context.Context, a Actor, tool string, data []byte, in ToolImportIn) (*ImportReport, error) {
	if _, ok := importTools[tool]; !ok {
		return nil, invalid("unknown tool %q (github, trello, linear, asana, json)", tool)
	}
	pid, err := s.projectID(ctx, in.Project)
	if err != nil {
		return nil, invalid("project %q does not exist: create it first", in.Project)
	}
	if !a.Admin {
		if r := s.role(ctx, a, pid); r == "" {
			return nil, ErrNotFound
		} else if r != "admin" {
			return nil, ErrForbidden
		}
	}
	var pkey string
	s.DB.QueryRow(ctx, `SELECT key FROM project WHERE id=$1`, pid).Scan(&pkey)
	if site := s.mirrorSite(ctx, pkey); site != "" {
		return nil, &ReadOnlyMirror{Project: pkey, SiteURL: site}
	}
	rep := &ImportReport{DryRun: in.DryRun, Counts: map[string]int{}, Warnings: []string{}, Gaps: []string{}, Errors: []string{}, KeyMap: map[string]string{}}
	b := &toolBuild{s: s, pkey: pkey, tool: tool, in: in, d: &BundleData{Projects: []BProject{{Key: pkey}}}, rep: rep, users: map[string]bool{}, gap: map[string]bool{}, seen: map[string]bool{},
		known: map[string]string{}, byCat: map[string]string{}}
	srows, err := s.DB.Query(ctx, `SELECT st.name,st.category FROM project p JOIN status st ON st.workflow_id=p.workflow_id AND NOT st.retired WHERE p.id=$1 ORDER BY st.position`, pid)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var n, c string
		srows.Scan(&n, &c)
		b.known[strings.ToLower(n)] = n
		if b.byCat[c] == "" {
			b.byCat[c] = n
		}
	}
	srows.Close()
	var versions []toolVersion
	switch tool {
	case "github":
		versions, err = b.github(data)
	case "trello":
		err = b.trello(data)
	case "linear":
		err = b.linear(data)
	case "asana":
		err = b.asana(data)
	case "json":
		err = b.generic(data)
	}
	if err != nil {
		return nil, err
	}
	if len(b.d.Issues) == 0 {
		return nil, invalid("the file has no issues the %s importer can read", importTools[tool])
	}
	emails := make([]string, 0, len(b.users))
	for e := range b.users {
		emails = append(emails, e)
	}
	sort.Strings(emails)
	for _, e := range emails {
		b.d.Users = append(b.d.Users, BUser{Email: e, Name: strings.Split(e, "@")[0], Kind: "human"})
	}
	soft := rep.Errors
	rep.Errors = []string{}
	out, err := s.applyBundle(ctx, a, b.d, nil, ImportOpts{DryRun: in.DryRun, Source: tool, AllowExisting: true}, rep)
	if out != nil {
		out.Errors = append(soft, out.Errors...)
	}
	if err != nil {
		return out, err
	}
	if len(versions) > 0 {
		s.applyToolVersions(ctx, pid, versions, out, in.DryRun)
	}
	return out, nil
}

// ---- GitHub Issues ----

type toolVersion struct {
	Name     string
	Due      string
	Released bool
	Issues   []string // bundle keys
}

type ghUser struct {
	Login string `json:"login"`
}

type ghIssue struct {
	Number      int64    `json:"number"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason"`
	HTMLURL     string   `json:"html_url"`
	User        ghUser   `json:"user"`
	Assignee    *ghUser  `json:"assignee"`
	Assignees   []ghUser `json:"assignees"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Milestone *struct {
		Title string `json:"title"`
		DueOn string `json:"due_on"`
		State string `json:"state"`
	} `json:"milestone"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	ClosedAt  string `json:"closed_at"`
}

type ghComment struct {
	ID        int64  `json:"id"`
	IssueURL  string `json:"issue_url"`
	Body      string `json:"body"`
	User      ghUser `json:"user"`
	CreatedAt string `json:"created_at"`
}

var ghIssueNo = regexp.MustCompile(`/issues/(\d+)$`)

// github reads the REST answer of GET /repos/{owner}/{repo}/issues?state=all (a JSON array), or an object {"issues": [...], "comments": [...]}
// where the comments are the answer of GET /repos/{owner}/{repo}/issues/comments.
func (b *toolBuild) github(data []byte) ([]toolVersion, error) {
	var issues []ghIssue
	var comments []ghComment
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &issues); err != nil {
			return nil, invalid("the file is not a list of GitHub issues: %v", err)
		}
	} else {
		var obj struct {
			Issues   []ghIssue   `json:"issues"`
			Comments []ghComment `json:"comments"`
		}
		if err := json.Unmarshal(data, &obj); err != nil {
			return nil, invalid("the file is not JSON from GitHub: %v", err)
		}
		issues, comments = obj.Issues, obj.Comments
	}
	byNo := map[string][]ghComment{}
	for _, c := range comments {
		if m := ghIssueNo.FindStringSubmatch(c.IssueURL); m != nil {
			byNo[m[1]] = append(byNo[m[1]], c)
		}
	}
	types := lowerMap(b.in.TypeMap)
	ms := map[string]*toolVersion{}
	var order []string
	prs := 0
	for _, g := range issues {
		if g.PullRequest != nil { // GitHub lists pull requests among the issues: they are code, not work items
			prs++
			continue
		}
		key := "gh:" + strconv.FormatInt(g.Number, 10)
		i := BIssue{Key: key, Summary: g.Title, Type: "Task", Priority: "medium", Created: time.Now()}
		if i.Summary == "" {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("#%d: the title is empty", g.Number))
			continue
		}
		desc := g.Body
		if g.HTMLURL != "" {
			desc += "\n\nImported from GitHub: " + g.HTMLURL
		}
		i.Description = desc
		if t, ok := toolTime(g.CreatedAt); ok {
			i.Created = t
		}
		if t, ok := toolTime(g.UpdatedAt); ok {
			i.Updated = t
		}
		if g.State == "closed" {
			i.Status = b.status("closed")
			t, ok := toolTime(g.ClosedAt)
			if !ok {
				t = i.Updated
			}
			i.Resolved = &t
		} else {
			i.Status = b.status("open")
		}
		for _, l := range g.Labels {
			if t, ok := types[strings.ToLower(l.Name)]; ok {
				i.Type = t
			} else if regexp.MustCompile(`(?i)^(bug|defect)$`).MatchString(l.Name) {
				i.Type = "Bug"
			}
			if strings.HasPrefix(strings.ToLower(l.Name), "priority:") {
				i.Priority = b.priority(strings.TrimSpace(l.Name[len("priority:"):]))
				continue
			}
			i.Labels = append(i.Labels, b.label(l.Name))
		}
		who := ""
		if g.Assignee != nil {
			who = g.Assignee.Login
		} else if len(g.Assignees) > 0 {
			who = g.Assignees[0].Login
		}
		i.Assignee = b.person(who, "assignee")
		i.Reporter = b.person(g.User.Login, "reporter")
		if !b.add(i) {
			continue
		}
		for _, c := range byNo[strconv.FormatInt(g.Number, 10)] {
			t, ok := toolTime(c.CreatedAt)
			if !ok {
				t = i.Created
			}
			b.comment(key, strconv.FormatInt(c.ID, 10), b.person(c.User.Login, "commenter"), c.Body, t)
		}
		if g.Milestone != nil && g.Milestone.Title != "" {
			v := ms[g.Milestone.Title]
			if v == nil {
				v = &toolVersion{Name: g.Milestone.Title, Released: g.Milestone.State == "closed"}
				if t, ok := toolTime(g.Milestone.DueOn); ok {
					v.Due = t.UTC().Format("2006-01-02")
				}
				ms[v.Name] = v
				order = append(order, v.Name)
			}
			v.Issues = append(v.Issues, key)
		}
	}
	if prs > 0 {
		b.rep.Warnings = append(b.rep.Warnings, fmt.Sprintf("%d pull request(s) in the file were skipped: they are code, not work items", prs))
	}
	var out []toolVersion
	for _, n := range order {
		out = append(out, *ms[n])
	}
	return out, nil
}

// applyToolVersions turns GitHub milestones into versions of the project and puts the issues in them (a milestone that is closed is released).
func (s *Service) applyToolVersions(ctx context.Context, pid string, vs []toolVersion, rep *ImportReport, dry bool) {
	for _, v := range vs {
		if dry {
			rep.Counts["versions"]++
			continue
		}
		var vid string
		err := s.DB.QueryRow(ctx, `INSERT INTO version(project_id,name,release_date,released,released_at) VALUES($1,$2,NULLIF($3,'')::date,$4,CASE WHEN $4 THEN now() END)
			ON CONFLICT (project_id, lower(name)) DO UPDATE SET name=version.name RETURNING id::text`, pid, v.Name, v.Due, v.Released).Scan(&vid)
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("milestone %q could not be made a version: %v", v.Name, err))
			continue
		}
		rep.Counts["versions"]++
		for _, k := range v.Issues {
			if key, ok := rep.KeyMap[k]; ok {
				if _, err := s.DB.Exec(ctx, `INSERT INTO issue_version(issue_id,version_id,kind) SELECT id,$2::uuid,'fix' FROM issue WHERE key=$1 ON CONFLICT DO NOTHING`, key, vid); err == nil {
					rep.Counts["issues_versioned"]++
				}
			}
		}
	}
}

// ---- Trello ----

type trelloBoard struct {
	Lists []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Closed bool   `json:"closed"`
	} `json:"lists"`
	Cards []struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Desc         string   `json:"desc"`
		IDList       string   `json:"idList"`
		Closed       bool     `json:"closed"`
		Due          string   `json:"due"`
		DueComplete  bool     `json:"dueComplete"`
		LastActivity string   `json:"dateLastActivity"`
		ShortURL     string   `json:"shortUrl"`
		IDMembers    []string `json:"idMembers"`
		IDChecklists []string `json:"idChecklists"`
		Labels       []struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"labels"`
		Attachments []struct {
			Name     string `json:"name"`
			URL      string `json:"url"`
			IsUpload bool   `json:"isUpload"`
		} `json:"attachments"`
	} `json:"cards"`
	Checklists []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		CheckItems []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"checkItems"`
	} `json:"checklists"`
	Members []struct {
		ID       string `json:"id"`
		FullName string `json:"fullName"`
		Username string `json:"username"`
	} `json:"members"`
	Actions []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Date string `json:"date"`
		Data struct {
			Text string `json:"text"`
			Card struct {
				ID string `json:"id"`
			} `json:"card"`
		} `json:"data"`
		MemberCreator struct {
			Username string `json:"username"`
			FullName string `json:"fullName"`
		} `json:"memberCreator"`
	} `json:"actions"`
}

// trello reads a board's JSON export (Menu, More, Print and export, Export as JSON): lists become statuses, cards issues, checklist items
// subtasks, comments the comment actions, labels and members as described in the report.
func (b *toolBuild) trello(data []byte) error {
	var t trelloBoard
	if err := json.Unmarshal(data, &t); err != nil {
		return invalid("the file is not a Trello board export: %v", err)
	}
	if len(t.Cards) == 0 {
		return invalid("the board export has no cards")
	}
	lists := map[string]string{}
	for _, l := range t.Lists {
		lists[l.ID] = l.Name
	}
	members := map[string][2]string{}
	for _, m := range t.Members {
		members[m.ID] = [2]string{m.Username, m.FullName}
	}
	checks := map[string]int{}
	for i, c := range t.Checklists {
		checks[c.ID] = i
	}
	uploads := 0
	for _, c := range t.Cards {
		key := "trello:" + c.ID
		i := BIssue{Key: key, Summary: c.Name, Type: "Task", Priority: "medium", Created: time.Now()}
		if strings.TrimSpace(i.Summary) == "" {
			b.rep.Errors = append(b.rep.Errors, "card "+c.ID+": the name is empty")
			continue
		}
		i.Status = b.status(lists[c.IDList])
		if t, ok := toolTime(c.LastActivity); ok {
			i.Created, i.Updated = t, t // the export carries no creation time on the card: the id's embedded time is not read
		}
		if id := c.ID; len(id) >= 8 { // a Trello id begins with the creation time as hex seconds
			if sec, err := strconv.ParseInt(id[:8], 16, 64); err == nil && sec > 1e8 {
				i.Created = time.Unix(sec, 0).UTC()
			}
		}
		desc := c.Desc
		var extra []string
		if c.ShortURL != "" {
			extra = append(extra, "Imported from Trello: "+c.ShortURL)
		}
		if tm, ok := toolTime(c.Due); ok {
			extra = append(extra, "Due: "+tm.UTC().Format("2006-01-02"))
		}
		for _, a := range c.Attachments {
			if a.IsUpload {
				uploads++
				extra = append(extra, "Attachment (file not copied): "+a.Name)
			} else if a.URL != "" {
				extra = append(extra, "Attachment: "+a.Name+" "+a.URL)
			}
		}
		for _, l := range c.Labels {
			n := l.Name
			if n == "" {
				n = l.Color
			}
			if n != "" {
				i.Labels = append(i.Labels, b.label(n))
			}
		}
		var who []string
		for n, id := range c.IDMembers {
			m := members[id]
			ref := m[0]
			if ref == "" {
				ref = m[1]
			}
			if n == 0 {
				if e := b.person(m[0], "member"); e != "" {
					i.Assignee = e
				} else if e := b.person(m[1], "member"); e != "" {
					i.Assignee = e
				}
			} else {
				who = append(who, ref)
			}
		}
		if len(who) > 0 {
			extra = append(extra, "Other members: "+strings.Join(who, ", "))
		}
		if c.Closed {
			i.Status = b.status("done")
			i.Labels = append(i.Labels, "trello-archived")
			tm := i.Updated
			if tm.IsZero() {
				tm = i.Created
			}
			i.Resolved = &tm
		}
		if i.Resolved == nil && b.isDone(i.Status) {
			tm := i.Updated
			i.Resolved = &tm
		}
		if len(extra) > 0 {
			desc += "\n\n" + strings.Join(extra, "\n")
		}
		i.Description = desc
		if !b.add(i) {
			continue
		}
		for _, cid := range c.IDChecklists { // checklist items become subtasks
			ci, ok := checks[cid]
			if !ok {
				continue
			}
			for _, it := range t.Checklists[ci].CheckItems {
				sub := BIssue{Key: "trello:" + it.ID, Type: "Subtask", Parent: key, Summary: it.Name, Status: b.status("todo"), Priority: "medium", Created: i.Created, Assignee: ""}
				if it.State == "complete" {
					sub.Status = b.status("done")
					tm := i.Updated
					sub.Resolved = &tm
				}
				if strings.TrimSpace(sub.Summary) != "" {
					b.add(sub)
				}
			}
		}
	}
	for _, a := range t.Actions {
		if a.Type != "commentCard" || a.Data.Card.ID == "" {
			continue
		}
		at, ok := toolTime(a.Date)
		if !ok {
			at = time.Now()
		}
		who := b.person(a.MemberCreator.Username, "commenter")
		if who == "" {
			who = b.person(a.MemberCreator.FullName, "commenter")
		}
		b.comment("trello:"+a.Data.Card.ID, a.ID, who, a.Data.Text, at)
	}
	if uploads > 0 {
		b.note("uploads", fmt.Sprintf("%d attached file(s) were not copied: their names are in the descriptions", uploads))
	}
	return nil
}

// ---- Linear and Asana (CSV exports) ----

// csvRows reads a CSV with a header row into maps by lower-case header.
func csvRows(data []byte) ([]string, []map[string]string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	head, err := cr.Read()
	if err != nil {
		return nil, nil, invalid("the file has no header row")
	}
	for i := range head {
		head[i] = strings.ToLower(strings.TrimSpace(head[i]))
	}
	var rows []map[string]string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return head, rows, invalid("unreadable row: %v", err)
		}
		m := map[string]string{}
		blank := true
		for i, c := range rec {
			if i < len(head) {
				m[head[i]] = strings.TrimSpace(c)
				if strings.TrimSpace(c) != "" {
					blank = false
				}
			}
		}
		if !blank {
			rows = append(rows, m)
		}
	}
	return head, rows, nil
}

func need(head []string, cols ...string) error {
	have := map[string]bool{}
	for _, h := range head {
		have[h] = true
	}
	for _, c := range cols {
		if !have[c] {
			return invalid("the file has no %q column (is it the right export? columns: %s)", c, strings.Join(head, ", "))
		}
	}
	return nil
}

// linear reads Linear's CSV export. Priorities, states and labels come over; a project becomes an Epic over its issues (unless switched off),
// a sub-issue a Subtask, the estimate the estimate, a cycle a label (cycle-N: there are no boards to put sprints on).
func (b *toolBuild) linear(data []byte) error {
	head, rows, err := csvRows(data)
	if err != nil {
		return err
	}
	if err := need(head, "id", "title"); err != nil {
		return err
	}
	epics := b.in.Epics == nil || *b.in.Epics
	made := map[string]string{}
	cycles := false
	for n, r := range rows {
		key := "linear:" + r["id"]
		if r["id"] == "" {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("row %d: the ID is empty", n+2))
			continue
		}
		i := BIssue{Key: key, Summary: r["title"], Type: "Task", Description: r["description"], Created: time.Now(), Priority: b.priority(r["priority"])}
		if i.Summary == "" {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("row %d (%s): the title is empty", n+2, r["id"]))
			continue
		}
		i.Status = b.status(r["status"])
		if t, ok := toolTime(r["created"]); ok {
			i.Created = t
		}
		if t, ok := toolTime(r["updated"]); ok {
			i.Updated = t
		}
		if t, ok := toolTime(r["completed"]); ok {
			i.Resolved, i.Status = &t, b.status("done")
		} else if t, ok := toolTime(r["canceled"]); ok {
			i.Resolved, i.Status = &t, b.status("done")
		}
		if i.Resolved == nil && b.isDone(i.Status) {
			t := i.Updated
			i.Resolved = &t
		}
		if r["estimate"] != "" {
			if f, err := strconv.ParseFloat(r["estimate"], 64); err == nil && f >= 0 {
				i.Estimate = &f
			}
		}
		for _, l := range strings.Split(r["labels"], ",") {
			if l = b.label(l); l != "" {
				i.Labels = append(i.Labels, l)
			}
		}
		if n := r["cycle number"]; n != "" {
			i.Labels = append(i.Labels, "cycle-"+n)
			cycles = true
		}
		i.Assignee, i.Reporter = b.person(r["assignee"], "assignee"), b.person(r["creator"], "creator")
		switch {
		case r["parent issue"] != "":
			i.Type, i.Parent = "Subtask", "linear:"+r["parent issue"]
		case epics && r["project"] != "":
			ek := "linear-project:" + r["project"]
			if made[ek] == "" {
				made[ek] = ek
				b.add(BIssue{Key: ek, Type: "Epic", Summary: r["project"], Status: b.status("todo"), Priority: "medium", Created: i.Created, Description: "A Linear project."})
			}
			i.Parent = ek
		}
		b.add(i)
	}
	if cycles {
		b.note("cycles", "cycles are kept as labels (cycle-N), not sprints: a sprint belongs to a board")
	}
	return nil
}

// asana reads Asana's CSV export of a project. Sections become statuses (by the words in their names, or by status_map), tasks with a parent
// task become subtasks, tags labels, completion the resolved date.
func (b *toolBuild) asana(data []byte) error {
	head, rows, err := csvRows(data)
	if err != nil {
		return err
	}
	if err := need(head, "task id", "name"); err != nil {
		return err
	}
	byName := map[string]string{}
	for _, r := range rows {
		if r["parent task"] == "" && r["name"] != "" {
			byName[r["name"]] = "asana:" + r["task id"]
		}
	}
	for n, r := range rows {
		key := "asana:" + r["task id"]
		if r["task id"] == "" {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("row %d: the task id is empty", n+2))
			continue
		}
		i := BIssue{Key: key, Summary: r["name"], Type: "Task", Priority: b.priority(r["priority"]), Created: time.Now()}
		if i.Summary == "" {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("row %d (%s): the name is empty", n+2, r["task id"]))
			continue
		}
		i.Status = b.status(r["section/column"])
		if t, ok := toolTime(r["created at"]); ok {
			i.Created = t
		}
		if t, ok := toolTime(r["last modified"]); ok {
			i.Updated = t
		}
		if t, ok := toolTime(r["completed at"]); ok {
			i.Resolved, i.Status = &t, b.status("done")
		}
		if i.Resolved == nil && b.isDone(i.Status) {
			t := i.Updated
			i.Resolved = &t
		}
		desc := r["notes"]
		if r["due date"] != "" {
			desc += "\n\nDue: " + r["due date"]
		}
		i.Description = desc
		for _, l := range strings.Split(r["tags"], ",") {
			if l = b.label(l); l != "" {
				i.Labels = append(i.Labels, l)
			}
		}
		who := r["assignee email"]
		if who == "" {
			who = r["assignee"]
		}
		i.Assignee = b.person(who, "assignee")
		if pt := r["parent task"]; pt != "" {
			if pk, ok := byName[pt]; ok {
				i.Type, i.Parent = "Subtask", pk
			} else {
				b.note("parent:"+pt, fmt.Sprintf("the parent task %q is not in the file: %q is imported as a task", pt, r["name"]))
			}
		}
		b.add(i)
	}
	return nil
}
