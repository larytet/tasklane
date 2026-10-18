package domain

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/blob"
)

type ImportOpts struct {
	DryRun         bool              `json:"dry_run"`
	RenameProjects map[string]string `json:"rename_projects"` // bundle project key -> key to create or merge into
	Pause          bool              `json:"pause"`           // hold a project pause while existing data is updated
	Source         string            `json:"source"`
	AllowExisting  bool              `json:"allow_existing"` // add to a project that already has other issues (the generic CSV import)
}

// ImportReport says what was (or, in a dry run, would be) done, and what could not be mapped.
type ImportReport struct {
	JobID    string            `json:"job_id"`
	DryRun   bool              `json:"dry_run"`
	Counts   map[string]int    `json:"counts"`
	Warnings []string          `json:"warnings"`
	Gaps     []string          `json:"gaps"` // mapping gaps: unknown statuses, users, types
	Errors   []string          `json:"errors"`
	KeyMap   map[string]string `json:"key_map"` // key in the bundle -> key here
	Manifest *BManifest        `json:"manifest,omitempty"`
	ErrorCSV string            `json:"error_csv,omitempty"` // rejected rows with the reason, as CSV
}

func (r *ImportReport) add(k string) { r.Counts[k]++ }

// files read from a bundle
type bundleFiles map[string]*zip.File

func readAll(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// ReadBundle opens a bundle, checks its format and verifies every file against the manifest's SHA-256 before anything is used.
func ReadBundle(r io.ReaderAt, size int64) (*BManifest, *BundleData, bundleFiles, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, nil, nil, invalid("not a Tasklane bundle: %v", err)
	}
	files := bundleFiles{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	mf := files["manifest.json"]
	if mf == nil {
		return nil, nil, nil, invalid("not a Tasklane bundle: manifest.json is missing")
	}
	mb, err := readAll(mf)
	if err != nil {
		return nil, nil, nil, err
	}
	var man BManifest
	if err := json.Unmarshal(mb, &man); err != nil || man.Format != BundleFormat {
		return nil, nil, nil, invalid("not a Tasklane bundle: the manifest is unreadable")
	}
	if man.Version > BundleVersion {
		return nil, nil, nil, invalid("this bundle is format version %d, newer than this server understands (%d); upgrade Tasklane", man.Version, BundleVersion)
	}
	names := make([]string, 0, len(man.Files))
	for n := range man.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names { // verify every file: a truncated or tampered bundle is rejected as a whole
		f := files[n]
		if f == nil {
			return nil, nil, nil, invalid("the bundle is incomplete: %s is listed in the manifest but missing", n)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, nil, err
		}
		h := sha256.New()
		nb, err := io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return nil, nil, nil, invalid("the bundle is damaged at %s: %v", n, err)
		}
		if hex.EncodeToString(h.Sum(nil)) != man.Files[n].SHA256 || nb != man.Files[n].Bytes {
			return nil, nil, nil, invalid("the bundle is corrupt: the checksum of %s does not match the manifest", n)
		}
	}
	d := &BundleData{}
	var rerr error
	if man.Kind == "csv" {
		rerr = readCSVBundle(files, d)
	} else {
		rerr = readJSONBundle(files, d)
	}
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	return &man, d, files, nil
}

func eachLine(f *zip.File, fn func([]byte) error) error {
	if f == nil {
		return nil
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if e := fn(line); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func jl[T any](files bundleFiles, name string, out *[]T) error {
	return eachLine(files[name], func(b []byte) error {
		var x T
		if err := json.Unmarshal(b, &x); err != nil {
			return invalid("%s: %v", name, err)
		}
		*out = append(*out, x)
		return nil
	})
}

func readJSONBundle(files bundleFiles, d *BundleData) error {
	for _, step := range []func() error{
		func() error { return jl(files, "users.jsonl", &d.Users) }, func() error { return jl(files, "workflows.jsonl", &d.Workflows) },
		func() error { return jl(files, "projects.jsonl", &d.Projects) }, func() error { return jl(files, "issues.jsonl", &d.Issues) },
		func() error { return jl(files, "comments.jsonl", &d.Comments) }, func() error { return jl(files, "links.jsonl", &d.Links) },
		func() error { return jl(files, "attachments.jsonl", &d.Attachments) }, func() error { return jl(files, "aliases.jsonl", &d.Aliases) },
		func() error { return jl(files, "removed_values.jsonl", &d.Removed) }, func() error { return jl(files, "history.jsonl", &d.History) },
		func() error { return jl(files, "text_versions.jsonl", &d.TextVersions) },
	} {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func readCSV(files bundleFiles, name string) ([][]string, error) {
	f := files[name]
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	cr := csv.NewReader(rc)
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, invalid("%s: %v", name, err)
	}
	if len(rows) > 0 {
		rows = rows[1:]
	}
	return rows, nil
}

func uncsv(v string) string { // undo the formula-injection guard on read
	if strings.HasPrefix(v, "'") && len(v) > 1 && strings.ContainsRune("=+-@\t\r", rune(v[1])) {
		return v[1:]
	}
	return v
}

func ptime(v string) time.Time { t, _ := time.Parse(time.RFC3339Nano, v); return t } // RFC3339Nano parses whole seconds too
func pptime(v string) *time.Time {
	if v == "" {
		return nil
	}
	t := ptime(v)
	return &t
}

func readCSVBundle(files bundleFiles, d *BundleData) error {
	get := func(r []string, i int) string {
		if i < len(r) {
			return uncsv(r[i])
		}
		return ""
	}
	rows, err := readCSV(files, "issues.csv")
	if err != nil {
		return err
	}
	for _, r := range rows {
		v, _ := strconv.ParseInt(get(r, 18), 10, 64)
		d.Issues = append(d.Issues, BIssue{ExternalID: get(r, 0), ID: get(r, 0), Key: get(r, 1), Project: get(r, 2), Type: get(r, 3), Status: get(r, 4), Priority: get(r, 5), Reporter: get(r, 6), Assignee: get(r, 7),
			Labels: splitNonEmpty(get(r, 8), ";"), Parent: get(r, 9), Rank: get(r, 10), Summary: get(r, 11), Description: get(r, 12), Created: ptime(get(r, 13)), Updated: ptime(get(r, 14)), Resolved: pptime(get(r, 15)),
			Deleted: get(r, 16) == "true", DeletedAt: pptime(get(r, 17)), Version: v, Estimate: pfloat(get(r, 19))})
	}
	if rows, err = readCSV(files, "comments.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		d.Comments = append(d.Comments, BComment{ID: get(r, 0), Issue: get(r, 1), Author: get(r, 2), Body: get(r, 3), Internal: get(r, 4) == "true", Created: ptime(get(r, 5)), Edited: pptime(get(r, 6))})
	}
	if rows, err = readCSV(files, "links.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		d.Links = append(d.Links, BLink{From: get(r, 0), To: get(r, 1), Type: get(r, 2)})
	}
	if rows, err = readCSV(files, "attachments.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		sz, _ := strconv.ParseInt(get(r, 4), 10, 64)
		d.Attachments = append(d.Attachments, BAttachment{Issue: get(r, 0), Name: get(r, 1), MIME: get(r, 2), SHA256: get(r, 3), Size: sz, Uploader: get(r, 5), Created: ptime(get(r, 6)), Path: get(r, 7)})
	}
	if rows, err = readCSV(files, "users.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		d.Users = append(d.Users, BUser{Email: get(r, 0), Name: get(r, 1), Kind: get(r, 2)})
	}
	if rows, err = readCSV(files, "workflows.csv"); err != nil {
		return err
	}
	wfs := map[string]*BWorkflow{}
	var order []string
	for _, r := range rows {
		w := wfs[get(r, 0)]
		if w == nil {
			w = &BWorkflow{Name: get(r, 0)}
			wfs[w.Name] = w
			order = append(order, w.Name)
		}
		if get(r, 1) == "status" {
			pos, _ := strconv.Atoi(get(r, 4))
			w.Statuses = append(w.Statuses, BStatus{Name: get(r, 2), Category: get(r, 3), Position: pos})
		} else {
			ft := strings.SplitN(get(r, 5), ">", 2)
			if len(ft) == 2 {
				w.Transitions = append(w.Transitions, BTransition{Name: get(r, 2), From: ft[0], To: ft[1]})
			}
		}
	}
	for _, n := range order {
		d.Workflows = append(d.Workflows, *wfs[n])
	}
	if rows, err = readCSV(files, "projects.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		next, _ := strconv.ParseInt(get(r, 5), 10, 64)
		p := BProject{Key: get(r, 0), Name: get(r, 1), Type: get(r, 2), Workflow: get(r, 3), Lead: get(r, 4), Next: next, Archived: get(r, 6) == "true"}
		for _, m := range splitNonEmpty(get(r, 7), ";") {
			if i := strings.LastIndex(m, ":"); i > 0 {
				p.Members = append(p.Members, BMember{Email: m[:i], Role: m[i+1:]})
			}
		}
		d.Projects = append(d.Projects, p)
	}
	if rows, err = readCSV(files, "aliases.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		d.Aliases = append(d.Aliases, BAlias{OldKey: get(r, 0), Issue: get(r, 1)})
	}
	if rows, err = readCSV(files, "removed_values.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		x := BRemoved{Issue: get(r, 0), Kind: get(r, 1)}
		json.Unmarshal([]byte(get(r, 2)), &x.Data)
		d.Removed = append(d.Removed, x)
	}
	if rows, err = readCSV(files, "history.csv"); err != nil {
		return err
	}
	for _, r := range rows {
		h := BHistory{Issue: get(r, 0), Field: get(r, 1), Actor: get(r, 4), At: ptime(get(r, 5))}
		if b := get(r, 2); b != "" {
			h.Before = &b
		}
		if af := get(r, 3); af != "" {
			h.After = &af
		}
		d.History = append(d.History, h)
	}
	return nil
}

func splitNonEmpty(s, sep string) []string {
	out := []string{}
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Import applies a bundle. It verifies the bundle first, then runs the whole import in one transaction, so a dry run is the
// same code rolled back and a failure leaves nothing half applied.
func (s *Service) Import(ctx context.Context, a Actor, r io.ReaderAt, size int64, o ImportOpts) (*ImportReport, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	man, d, files, err := ReadBundle(r, size)
	if err != nil {
		return nil, err
	}
	rep := &ImportReport{DryRun: o.DryRun, Counts: map[string]int{}, Warnings: []string{}, Gaps: []string{}, Errors: []string{}, KeyMap: map[string]string{}, Manifest: man}
	if o.Source == "" {
		o.Source = "bundle"
	}
	return s.applyBundle(ctx, a, d, files, o, rep)
}

type importCtx struct {
	s       *Service
	tx      pgx.Tx
	a       Actor
	o       ImportOpts
	rep     *ImportReport
	jobID   string
	users   map[string]string // email -> id
	issues  map[string]string // bundle key -> issue id
	keys    map[string]string // bundle key -> key here
	files   bundleFiles
	created []string // project keys created, for rollback
}

func (c *importCtx) user(email string) *string {
	if email == "" {
		return nil
	}
	if id, ok := c.users[strings.ToLower(email)]; ok {
		return &id
	}
	return nil
}

func (s *Service) applyBundle(ctx context.Context, a Actor, d *BundleData, files bundleFiles, o ImportOpts, rep *ImportReport) (*ImportReport, error) {
	// updating existing projects can race with writers: hold a project pause around the apply when asked
	if o.Pause && !o.DryRun {
		for _, p := range d.Projects {
			k := p.Key
			if nk, ok := o.RenameProjects[p.Key]; ok {
				k = nk
			}
			if _, err := s.projectID(ctx, k); err == nil {
				l, err := s.RequestPause(ctx, a, PauseIn{Scope: "project:" + strings.ToUpper(k), Owner: "import", HoldMS: 600000})
				if err != nil {
					return rep, err
				}
				defer s.ReleasePause(context.Background(), a, l.ID)
			}
		}
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return rep, err
	}
	defer tx.Rollback(ctx)
	c := &importCtx{s: s, tx: tx, a: a, o: o, rep: rep, users: map[string]string{}, issues: map[string]string{}, keys: map[string]string{}, files: files}
	if err := tx.QueryRow(ctx, `INSERT INTO import_job(source,actor,state) VALUES($1,$2,$3) RETURNING id`, o.Source, a.Label, map[bool]string{true: "dry-run", false: "running"}[o.DryRun]).Scan(&c.jobID); err != nil {
		return rep, err
	}
	rep.JobID = c.jobID
	if err := c.users_(ctx, d); err != nil {
		return rep, err
	}
	if err := c.workflows(ctx, d); err != nil {
		return rep, err
	}
	if err := c.projects(ctx, d); err != nil {
		return rep, err
	}
	if len(rep.Errors) > 0 {
		return rep, &ConflictError{"the import cannot run: " + strings.Join(rep.Errors, "; ")}
	}
	if err := c.issuesStep(ctx, d); err != nil {
		return rep, err
	}
	if err := c.rest(ctx, d); err != nil {
		return rep, err
	}
	sum, _ := json.Marshal(map[string]any{"counts": rep.Counts, "projects_created": c.created})
	tx.Exec(ctx, `UPDATE import_job SET state=$2, summary=$3 WHERE id=$1`, c.jobID, map[bool]string{true: "dry-run", false: "done"}[o.DryRun], sum)
	if o.DryRun {
		return rep, nil
	}
	if err := s.audit(ctx, tx, a, "import", c.jobID, map[string]any{"counts": rep.Counts, "source": o.Source}); err != nil {
		return rep, err
	}
	if err := tx.Commit(ctx); err != nil {
		return rep, err
	}
	return rep, nil
}

func (c *importCtx) users_(ctx context.Context, d *BundleData) error {
	for _, u := range d.Users {
		em := strings.ToLower(strings.TrimSpace(u.Email))
		if em == "" {
			continue
		}
		var id string
		err := c.tx.QueryRow(ctx, `SELECT id FROM app_user WHERE email=$1`, em).Scan(&id)
		if err == pgx.ErrNoRows { // an unknown person becomes an inactive placeholder with no credentials
			kind := u.Kind
			if kind == "" {
				kind = "human"
			}
			if err := c.tx.QueryRow(ctx, `INSERT INTO app_user(email,name,kind,active) VALUES($1,$2,$3,false) RETURNING id`, em, u.Name, kind).Scan(&id); err != nil {
				return err
			}
			c.rep.add("users_created")
			c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("%s was not found here: an inactive placeholder was created", em))
		} else if err != nil {
			return err
		} else {
			c.rep.add("users_matched")
		}
		c.users[em] = id
	}
	return nil
}

func (c *importCtx) workflows(ctx context.Context, d *BundleData) error {
	for _, w := range d.Workflows {
		var id string
		err := c.tx.QueryRow(ctx, `SELECT id FROM workflow WHERE name=$1`, w.Name).Scan(&id)
		if err == pgx.ErrNoRows {
			if err := c.tx.QueryRow(ctx, `INSERT INTO workflow(name) VALUES($1) RETURNING id`, w.Name).Scan(&id); err != nil {
				return err
			}
			ids := map[string]string{}
			for _, st := range w.Statuses {
				var sid string
				if err := c.tx.QueryRow(ctx, `INSERT INTO status(workflow_id,name,category,position) VALUES($1,$2,$3,$4) RETURNING id`, id, st.Name, st.Category, st.Position).Scan(&sid); err != nil {
					return err
				}
				ids[st.Name] = sid
			}
			for _, t := range w.Transitions {
				var from any
				if t.From != "" {
					from = ids[t.From]
				}
				if _, err := c.tx.Exec(ctx, `INSERT INTO transition(workflow_id,name,from_status,to_status) VALUES($1,$2,$3,$4)`, id, t.Name, from, ids[t.To]); err != nil {
					return err
				}
			}
			c.rep.add("workflows_created")
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (c *importCtx) projects(ctx context.Context, d *BundleData) error {
	count := map[string]int{}
	for _, i := range d.Issues {
		count[i.Project]++
	}
	for _, p := range d.Projects {
		key := strings.ToUpper(p.Key)
		if nk, ok := c.o.RenameProjects[p.Key]; ok {
			key = strings.ToUpper(nk)
		}
		var pid string
		var existingIssues int
		err := c.tx.QueryRow(ctx, `SELECT id FROM project WHERE key=$1`, key).Scan(&pid)
		if err == nil {
			if site := c.s.mirrorSite(ctx, key); site != "" { // a mirrored project is only ever written by its sync
				return &ReadOnlyMirror{Project: key, SiteURL: site}
			}
			c.tx.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id=$1`, pid).Scan(&existingIssues)
			var matching int
			c.tx.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id=$1 AND external_id IS NOT NULL`, pid).Scan(&matching)
			if existingIssues > 0 && matching == 0 && key == strings.ToUpper(p.Key) && !c.o.AllowExisting {
				c.rep.Errors = append(c.rep.Errors, fmt.Sprintf("project %s already exists with issues that did not come from this bundle: import under another key (rename_projects)", key))
				continue
			}
			c.rep.add("projects_merged")
		} else if err == pgx.ErrNoRows {
			var wf string
			if err := c.tx.QueryRow(ctx, `SELECT id FROM workflow WHERE name=$1`, p.Workflow).Scan(&wf); err != nil {
				if err2 := c.tx.QueryRow(ctx, `SELECT id FROM workflow WHERE name='Standard'`).Scan(&wf); err2 != nil {
					return invalid("workflow %q does not exist here", p.Workflow)
				}
				c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("workflow %q is not available: project %s uses Standard", p.Workflow, key))
			}
			typ := p.Type
			if typ == "" {
				typ = "software"
			}
			if err := c.tx.QueryRow(ctx, `INSERT INTO project(key,name,type,lead_id,workflow_id,next_number,archived) VALUES($1,$2,$3,$4,$5,1,$6) RETURNING id`, key, p.Name, typ, c.user(p.Lead), wf, p.Archived).Scan(&pid); err != nil {
				return err
			}
			c.created = append(c.created, key)
			c.rep.add("projects_created")
		} else {
			return err
		}
		for _, m := range p.Members {
			if uid := c.user(m.Email); uid != nil {
				c.tx.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, pid, *uid, m.Role)
			}
		}
		if _, err := c.tx.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1,$2,'admin') ON CONFLICT DO NOTHING`, pid, c.a.ID); err != nil {
			return err
		}
	}
	return nil
}

func keyNumber(key string) int64 {
	i := strings.LastIndex(key, "-")
	n, _ := strconv.ParseInt(key[i+1:], 10, 64)
	return n
}

func typeLevel(name string) int {
	switch strings.ToLower(name) {
	case "epic", "initiative":
		return 1
	case "subtask", "sub-task":
		return -1
	}
	return 0
}

func (c *importCtx) issuesStep(ctx context.Context, d *BundleData) error {
	// parents before children
	order := make([]BIssue, len(d.Issues))
	copy(order, d.Issues)
	sort.SliceStable(order, func(i, j int) bool { return typeLevel(order[i].Type) > typeLevel(order[j].Type) })
	projKey := map[string]string{}
	for _, p := range d.Projects {
		k := strings.ToUpper(p.Key)
		if nk, ok := c.o.RenameProjects[p.Key]; ok {
			k = strings.ToUpper(nk)
		}
		projKey[p.Key] = k
	}
	maxNum := map[string]int64{}
	// the numbers the bundle uses per project, so that preserved numbers cannot collide with allocated ones
	for _, i := range order {
		pk := projKey[i.Project]
		pid := ""
		c.tx.QueryRow(ctx, `SELECT id FROM project WHERE key=$1`, pk).Scan(&pid)
		if pid == "" {
			c.rep.Errors = append(c.rep.Errors, fmt.Sprintf("issue %s refers to project %s, which is not in the bundle", i.Key, i.Project))
			continue
		}
		ext := i.ExternalID
		if ext == "" {
			ext = i.Key
		}
		if err := c.importIssue(ctx, i, pk, pid, ext, maxNum); err != nil {
			return err
		}
	}
	for pk, n := range maxNum {
		c.tx.Exec(ctx, `UPDATE project SET next_number=GREATEST(next_number,$2) WHERE key=$1`, pk, n+1)
	}
	for _, p := range d.Projects {
		c.tx.Exec(ctx, `UPDATE project SET next_number=GREATEST(next_number,$2) WHERE key=$1`, projKey[p.Key], p.Next)
	}
	return nil
}

func (c *importCtx) statusFor(ctx context.Context, projectID, name string) (string, error) {
	var id string
	if err := c.tx.QueryRow(ctx, `SELECT st.id FROM project p JOIN status st ON st.workflow_id=p.workflow_id WHERE p.id=$1 AND lower(st.name)=lower($2)`, projectID, name).Scan(&id); err == nil {
		return id, nil
	}
	c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("status %q does not exist in the target workflow: mapped to the initial status", name))
	id, _, err := initialStatus(ctx, c.tx, projectID)
	return id, err
}

func (c *importCtx) importIssue(ctx context.Context, i BIssue, pk, pid, ext string, maxNum map[string]int64) error {
	typeID, _, err := typeNamed(ctx, c.tx, i.Type)
	if err != nil { // an unknown type is created with a level inferred from its name
		lvl := typeLevel(i.Type)
		if e := c.tx.QueryRow(ctx, `INSERT INTO issue_type(name,level) VALUES($1,$2) ON CONFLICT (name) DO UPDATE SET name=EXCLUDED.name RETURNING id`, i.Type, lvl).Scan(&typeID); e != nil {
			return e
		}
		c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("issue type %q was created (level %d)", i.Type, lvl))
	}
	sid, err := c.statusFor(ctx, pid, i.Status)
	if err != nil {
		return err
	}
	reporter := c.user(i.Reporter)
	if reporter == nil {
		reporter = &c.a.ID
	}
	var parentID any
	if i.Parent != "" {
		if pidv, ok := c.issues[i.Parent]; ok {
			parentID = pidv
		} else {
			c.rep.Warnings = append(c.rep.Warnings, fmt.Sprintf("%s: parent %s is not in the bundle; the link was dropped", i.Key, i.Parent))
		}
	}
	prio := i.Priority
	if !priorities[prio] {
		prio = "medium"
	}
	labels := i.Labels
	if labels == nil {
		labels = []string{}
	}
	var id, curKey, curSummary, curDesc, curStatus, curPrio, curType string
	var curAssignee *string
	var curLabels []string
	err = c.tx.QueryRow(ctx, `SELECT i.id,i.key,i.summary,COALESCE(d.body,''),i.status_id::text,i.priority,i.issue_type_id::text,i.assignee_id,i.labels FROM issue i LEFT JOIN issue_text d ON d.issue_id=i.id AND d.field='description' WHERE i.project_id=$1 AND i.external_id=$2`, pid, ext).
		Scan(&id, &curKey, &curSummary, &curDesc, &curStatus, &curPrio, &curType, &curAssignee, &curLabels)
	asg := c.user(i.Assignee)
	if err == nil { // idempotent re-import: update in place, only what differs
		changed := false
		if curSummary != i.Summary {
			c.bumpText(ctx, id, "summary", i.Summary)
			changed = true
		}
		if curDesc != i.Description {
			c.bumpText(ctx, id, "description", i.Description)
			changed = true
		}
		sameAssignee := (asg == nil && curAssignee == nil) || (asg != nil && curAssignee != nil && *asg == *curAssignee)
		if curStatus != sid || curPrio != prio || curType != typeID || !sameAssignee || strings.Join(curLabels, ",") != strings.Join(labels, ",") {
			var av any
			if asg != nil {
				av = *asg
			}
			if _, err := c.tx.Exec(ctx, `UPDATE issue SET status_id=$2,priority=$3,issue_type_id=$4,assignee_id=$5,labels=$6,resolved_at=$7,version=version+1 WHERE id=$1`, id, sid, prio, typeID, av, labels, i.Resolved); err != nil {
				return err
			}
			changed = true
		}
		if changed {
			c.tx.Exec(ctx, `UPDATE issue SET updated_at=$2 WHERE id=$1`, id, nonZero(i.Updated))
			c.rep.add("issues_updated")
			outbox(ctx, c.tx, "issue:"+curKey, "issue.updated", map[string]any{"key": curKey, "actor": c.a.Label, "actor_id": c.a.ID})
		} else {
			c.rep.add("issues_unchanged")
		}
		c.issues[i.Key], c.keys[i.Key] = id, curKey
		c.rep.KeyMap[i.Key] = curKey
		return nil
	}
	if err != pgx.ErrNoRows {
		return err
	}
	// a new issue: keep the number when it is free (a new or renamed project), otherwise allocate the next one
	num := keyNumber(i.Key)
	var taken bool
	c.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM issue WHERE project_id=$1 AND number=$2)`, pid, num).Scan(&taken)
	var nextNum int64
	c.tx.QueryRow(ctx, `SELECT next_number FROM project WHERE id=$1`, pid).Scan(&nextNum)
	if num <= 0 || taken || num < nextNum {
		// allocate, but never below a number already in use (preserved numbers of this import are not yet counted in next_number)
		c.tx.Exec(ctx, `UPDATE project SET next_number=GREATEST(next_number,(SELECT COALESCE(max(number),0)+1 FROM issue WHERE project_id=$1)) WHERE id=$1`, pid)
		if err := c.tx.QueryRow(ctx, `UPDATE project SET next_number=next_number+1 WHERE id=$1 RETURNING next_number-1`, pid).Scan(&num); err != nil {
			return err
		}
	}
	if num > maxNum[pk] {
		maxNum[pk] = num
	}
	newKey := pk + "-" + itoa(num)
	created := nonZero(i.Created)
	var av any
	if asg != nil {
		av = *asg
	}
	rank := i.Rank
	if rank == "" {
		rank = rankFor(num)
	}
	if err := c.tx.QueryRow(ctx, `INSERT INTO issue(project_id,number,key,issue_type_id,status_id,priority,reporter_id,assignee_id,labels,parent_id,rank,summary,created_at,updated_at,resolved_at,deleted_at,deleted_by,purge_after,delete_batch,external_id,import_id,estimate)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22) RETURNING id`,
		pid, num, newKey, typeID, sid, prio, reporter, av, labels, parentID, rank, i.Summary, created, nonZeroOr(i.Updated, created), i.Resolved, i.DeletedAt, nilIfDeleted(i.Deleted, c.a.ID), i.PurgeAfter, nilIfDeletedBatch(i.Deleted), ext, c.jobID, i.Estimate).Scan(&id); err != nil {
		return err
	}
	c.tx.Exec(ctx, `INSERT INTO issue_text(issue_id,field,body,updated_by,last_save_id) VALUES($1,'description',$2,$3,'')`, id, i.Description, c.a.ID)
	for f, v := range map[string]string{"summary": i.Summary, "description": i.Description} {
		c.tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode,at) VALUES($1,$2,1,$3,$4,$5,'import',$6)`, id, f, v, c.a.ID, c.a.Label, created)
	}
	if i.Key != newKey {
		c.tx.Exec(ctx, `INSERT INTO issue_key_alias(old_key,issue_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, i.Key, id) // an old key keeps resolving
		c.rep.add("keys_aliased")
	}
	c.issues[i.Key], c.keys[i.Key] = id, newKey
	c.rep.KeyMap[i.Key] = newKey
	c.rep.add("issues_created")
	outbox(ctx, c.tx, "issue:"+newKey, "issue.created", map[string]any{"key": newKey, "imported": true, "actor": c.a.Label, "actor_id": c.a.ID})
	return nil
}

func nonZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
func nonZeroOr(t, d time.Time) time.Time {
	if t.IsZero() {
		return d
	}
	return t
}
func nilIfDeleted(del bool, id string) any {
	if del {
		return id
	}
	return nil
}
func nilIfDeletedBatch(del bool) any {
	if del {
		return newUUID()
	}
	return nil
}

func (c *importCtx) bumpText(ctx context.Context, id, field, body string) {
	var seq int64
	if field == "summary" {
		c.tx.QueryRow(ctx, `UPDATE issue SET summary=$2,summary_seq=summary_seq+1 WHERE id=$1 RETURNING summary_seq`, id, body).Scan(&seq)
	} else {
		c.tx.QueryRow(ctx, `UPDATE issue_text SET body=$3,text_seq=text_seq+1,updated_at=now() WHERE issue_id=$1 AND field=$2 RETURNING text_seq`, id, field, body).Scan(&seq)
	}
	c.tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode) VALUES($1,$2,$3,$4,$5,$6,'import')`, id, field, seq, body, c.a.ID, c.a.Label)
}

func (c *importCtx) rest(ctx context.Context, d *BundleData) error {
	for _, cm := range d.Comments {
		iid, ok := c.issues[cm.Issue]
		if !ok {
			continue
		}
		var exists bool
		c.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id=$1 AND external_id=$2)`, iid, cm.ID).Scan(&exists)
		if exists {
			continue
		}
		au := c.user(cm.Author)
		if au == nil {
			au = &c.a.ID
		}
		if _, err := c.tx.Exec(ctx, `INSERT INTO comment(issue_id,author_id,body,internal,created_at,edited_at,external_id,import_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			iid, *au, cm.Body, cm.Internal, nonZero(cm.Created), cm.Edited, cm.ID, c.jobID); err != nil {
			return err
		}
		c.rep.add("comments_created")
	}
	for _, l := range d.Links {
		f, ok1 := c.issues[l.From]
		t, ok2 := c.issues[l.To]
		if ok1 && ok2 && f != t {
			if tag, err := c.tx.Exec(ctx, `INSERT INTO issue_link(from_id,to_id,type) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, f, t, l.Type); err == nil && tag.RowsAffected() > 0 {
				c.rep.add("links_created")
			}
		}
	}
	for _, at := range d.Attachments {
		if err := c.attachment(ctx, at); err != nil {
			return err
		}
	}
	for _, al := range d.Aliases {
		if iid, ok := c.issues[al.Issue]; ok {
			var clash bool
			c.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM issue WHERE key=$1) OR EXISTS(SELECT 1 FROM issue_key_alias WHERE old_key=$1 AND issue_id<>$2)`, al.OldKey, iid).Scan(&clash)
			if clash {
				c.rep.Warnings = append(c.rep.Warnings, fmt.Sprintf("alias %s was not imported: the key is in use here", al.OldKey))
				continue
			}
			if tag, err := c.tx.Exec(ctx, `INSERT INTO issue_key_alias(old_key,issue_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, al.OldKey, iid); err == nil && tag.RowsAffected() > 0 {
				c.rep.add("aliases_created")
			}
		}
	}
	for _, rv := range d.Removed {
		if iid, ok := c.issues[rv.Issue]; ok {
			if cs, ok := rv.Data["children"].([]any); ok { // keys in the bundle become ids here
				var ids []any
				for _, k := range cs {
					if id, ok := c.issues[fmt.Sprint(k)]; ok {
						ids = append(ids, id)
					}
				}
				rv.Data["children"] = ids
			}
			b, _ := json.Marshal(rv.Data)
			var exists bool
			c.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM removed_values WHERE issue_id=$1 AND kind=$2 AND data=$3::jsonb)`, iid, rv.Kind, b).Scan(&exists)
			if !exists {
				c.tx.Exec(ctx, `INSERT INTO removed_values(issue_id,kind,data,expires_at) VALUES($1,$2,$3,$4)`, iid, rv.Kind, b, c.s.Now().Add(c.s.trashRetention()))
				c.rep.add("removed_values_created")
			}
		}
	}
	// history and text versions of newly created issues (an update keeps its own history)
	created := map[string]bool{}
	for k := range c.issues {
		created[k] = true
	}
	for _, h := range d.History {
		iid, ok := c.issues[h.Issue]
		if !ok || h.Field == "created" && false {
			continue
		}
		var has bool
		c.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM issue_history WHERE issue_id=$1 AND field=$2 AND at=$3 AND actor_label=$4 AND before IS NOT DISTINCT FROM $5 AND after IS NOT DISTINCT FROM $6)`, iid, h.Field, h.At, h.Actor, h.Before, h.After).Scan(&has)
		if has {
			continue
		}
		c.tx.Exec(ctx, `INSERT INTO issue_history(issue_id,field,before,after,actor_label,at) VALUES($1,$2,$3,$4,$5,$6)`, iid, h.Field, h.Before, h.After, h.Actor, h.At)
		c.rep.add("history_created")
	}
	for _, v := range d.TextVersions {
		iid, ok := c.issues[v.Issue]
		if !ok {
			continue
		}
		tag, err := c.tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_label,mode,at) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (issue_id,field,seq) DO NOTHING`, iid, v.Field, v.Seq, v.Body, v.Actor, v.Mode, v.At)
		if err == nil && tag.RowsAffected() > 0 {
			c.rep.add("text_versions_created")
			// keep the counters at the highest imported version, so the next save continues the sequence
			if v.Field == "summary" {
				c.tx.Exec(ctx, `UPDATE issue SET summary_seq=GREATEST(summary_seq,$2) WHERE id=$1`, iid, v.Seq)
			} else {
				c.tx.Exec(ctx, `UPDATE issue_text SET text_seq=GREATEST(text_seq,$3) WHERE issue_id=$1 AND field=$2`, iid, v.Field, v.Seq)
			}
		}
	}
	return nil
}

func (c *importCtx) attachment(ctx context.Context, at BAttachment) error {
	iid, ok := c.issues[at.Issue]
	if !ok {
		return nil
	}
	var exists bool
	c.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attachment a JOIN blob b ON b.id=a.blob_id WHERE a.issue_id=$1 AND a.name=$2 AND b.sha256=$3)`, iid, at.Name, at.SHA256).Scan(&exists)
	if exists {
		return nil
	}
	f := c.files[at.Path]
	if at.Path == "" || f == nil {
		c.rep.Warnings = append(c.rep.Warnings, fmt.Sprintf("attachment %q of %s has no file in the bundle: skipped", at.Name, at.Issue))
		return nil
	}
	if c.s.Blobs == nil || c.s.Blobs.Store == nil {
		c.rep.Warnings = append(c.rep.Warnings, fmt.Sprintf("attachment %q of %s skipped: attachments are not configured here", at.Name, at.Issue))
		return nil
	}
	if c.o.DryRun {
		c.rep.add("attachments_created")
		return nil
	}
	var blobID string
	err := c.tx.QueryRow(ctx, `SELECT id FROM blob WHERE sha256=$1`, at.SHA256).Scan(&blobID)
	if err == pgx.ErrNoRows {
		sid, sf, err := c.s.Blobs.Store.NewStaging()
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			sf.Close()
			return err
		}
		h := sha256.New()
		head := &bytes.Buffer{}
		sc := &blob.Scanner{}
		sig := ""
		buf := make([]byte, 1<<20)
		var total int64
		for {
			n, rerr := rc.Read(buf)
			if n > 0 {
				h.Write(buf[:n])
				sf.Write(buf[:n])
				total += int64(n)
				if head.Len() < 512 {
					head.Write(buf[:min(n, 512-head.Len())])
				}
				if sig == "" {
					sig = sc.Feed(buf[:n])
				}
			}
			if rerr != nil {
				break
			}
		}
		rc.Close()
		sf.Close()
		path, _ := c.s.Blobs.Store.StagingPath(sid)
		if got := hex.EncodeToString(h.Sum(nil)); got != at.SHA256 {
			os.Remove(path)
			return invalid("attachment %q: the file does not match its checksum", at.Name)
		}
		mime, class := blob.Sniff(head.Bytes(), at.Name)
		state, detail := "clean", ""
		if sig != "" {
			state, detail = "blocked", sig
		}
		if _, err := c.s.Blobs.Store.Commit(sid, at.SHA256); err != nil {
			return err
		}
		if err := c.tx.QueryRow(ctx, `INSERT INTO blob(sha256,size,mime,class,scan_state,scan_detail) VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, at.SHA256, total, mime, string(class), state, detail).Scan(&blobID); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	c.tx.Exec(ctx, `UPDATE blob SET refcount=refcount+1, zero_since=NULL WHERE id=$1`, blobID)
	up := c.user(at.Uploader)
	var cls string
	c.tx.QueryRow(ctx, `SELECT class FROM blob WHERE id=$1`, blobID).Scan(&cls)
	thumb := "none"
	if cls == "image" {
		thumb = "pending"
	}
	_, err = c.tx.Exec(ctx, `INSERT INTO attachment(issue_id,blob_id,name,declared_mime,uploader_id,thumb_state,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, iid, blobID, blob.SafeName(at.Name), at.MIME, up, thumb, nonZero(at.Created))
	if err == nil {
		c.rep.add("attachments_created")
	}
	return err
}

// RollbackImport removes what one import run created, by its id, and nothing else: updated records are left as they are.
func (s *Service) RollbackImport(ctx context.Context, a Actor, jobID string) (int, error) {
	if !a.Admin {
		return 0, ErrForbidden
	}
	n := 0
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var state string
		var summary []byte
		if err := tx.QueryRow(ctx, `SELECT state,summary FROM import_job WHERE id=$1 FOR UPDATE`, jobID).Scan(&state, &summary); err != nil {
			return ErrNotFound
		}
		if state != "done" {
			return invalid("only a completed import can be rolled back (this one is %s)", state)
		}
		rows, err := tx.Query(ctx, `SELECT id FROM issue WHERE import_id=$1`, jobID)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			if _, err := s.purgeOne(ctx, tx, id); err != nil {
				return err
			}
			n++
		}
		tx.Exec(ctx, `DELETE FROM comment WHERE import_id=$1`, jobID)
		var sm struct {
			Created []string `json:"projects_created"`
		}
		json.Unmarshal(summary, &sm)
		for _, k := range sm.Created { // a project the import created is removed only if nothing else lives in it now
			var left int
			tx.QueryRow(ctx, `SELECT count(*) FROM issue i JOIN project p ON p.id=i.project_id WHERE p.key=$1`, k).Scan(&left)
			if left == 0 {
				tx.Exec(ctx, `DELETE FROM project WHERE key=$1`, k)
			}
		}
		tx.Exec(ctx, `UPDATE import_job SET state='rolled-back' WHERE id=$1`, jobID)
		return s.audit(ctx, tx, a, "import.rollback", jobID, map[string]any{"issues": n})
	})
	return n, err
}

func pfloat(s string) *float64 {
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}
