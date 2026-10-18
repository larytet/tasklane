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
	"strings"
	"time"
)

// CSVMapping tells the generic CSV importer what each column means (SPEC 18.2, "Generic CSV"). It can be saved as a template.
type CSVMapping struct {
	Columns     map[string]string `json:"columns"`      // header -> field: key, summary, description, type, status, priority, assignee, reporter, labels, parent, created, updated, resolved, comment
	TypeMap     map[string]string `json:"type_map"`     // value in the file -> issue type here
	StatusMap   map[string]string `json:"status_map"`   // value in the file -> status here
	PriorityMap map[string]string `json:"priority_map"` // value in the file -> lowest..highest
	LabelSep    string            `json:"label_sep"`    // default ";"
	DateFormats []string          `json:"date_formats"` // Go layouts tried in order; RFC 3339 and 2006-01-02 are always tried; a date without a zone is read as UTC
	DefaultType string            `json:"default_type"` // used when the type column is empty or unmapped, default Task
}

var csvFields = map[string]bool{"key": true, "summary": true, "description": true, "type": true, "status": true, "priority": true, "assignee": true, "reporter": true,
	"labels": true, "parent": true, "created": true, "updated": true, "resolved": true, "comment": true}

type CSVImportIn struct {
	Project string     `json:"project"`
	Mapping CSVMapping `json:"mapping"`
	DryRun  bool       `json:"dry_run"`
}

// ImportCSV imports rows into an existing project. Each row is checked on its own: a bad row is reported (row number and reason,
// also as an error CSV) and skipped, the good rows are imported. Re-importing the same file updates in place.
func (s *Service) ImportCSV(ctx context.Context, a Actor, data []byte, in CSVImportIn) (*ImportReport, error) {
	if !a.Admin {
		pid, err := s.projectID(ctx, in.Project)
		if err != nil {
			return nil, ErrNotFound
		}
		if r := s.role(ctx, a, pid); r == "" {
			return nil, ErrNotFound
		} else if r != "admin" {
			return nil, ErrForbidden
		}
	}
	pid, err := s.projectID(ctx, in.Project)
	if err != nil {
		return nil, invalid("project %q does not exist: create it first", in.Project)
	}
	var pkey string
	s.DB.QueryRow(ctx, `SELECT key FROM project WHERE id=$1`, pid).Scan(&pkey)
	m := in.Mapping
	for h, f := range m.Columns {
		if !csvFields[f] {
			return nil, invalid("column %q is mapped to %q, which is not a field (key, summary, description, type, status, priority, assignee, reporter, labels, parent, created, updated, resolved, comment)", h, f)
		}
	}
	has := map[string]bool{}
	for _, f := range m.Columns {
		has[f] = true
	}
	if !has["summary"] {
		return nil, invalid("map one column to summary: every issue needs one")
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // a UTF-8 byte order mark from a spreadsheet export
	cr := csv.NewReader(bytes.NewReader(data))
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	head, err := cr.Read()
	if err != nil {
		return nil, invalid("the file has no header row")
	}
	col := map[string][]int{}
	for i, h := range head {
		if f, ok := m.Columns[strings.TrimSpace(h)]; ok {
			col[f] = append(col[f], i)
		}
	}
	for h := range m.Columns {
		found := false
		for _, x := range head {
			if strings.TrimSpace(x) == h {
				found = true
			}
		}
		if !found {
			return nil, invalid("the mapping names a column %q that is not in the file (columns: %s)", h, strings.Join(head, ", "))
		}
	}
	sep := m.LabelSep
	if sep == "" {
		sep = ";"
	}
	layouts := append(append([]string{}, m.DateFormats...), time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02")
	parseDate := func(v string) (time.Time, error) {
		for _, l := range layouts {
			if t, err := time.Parse(l, strings.TrimSpace(v)); err == nil {
				return t, nil
			}
		}
		return time.Time{}, fmt.Errorf("cannot read %q as a date (use 2026-10-19, 2026-10-19 14:30 or RFC 3339, or list your layout in date_formats)", v)
	}
	get := func(rec []string, f string) string {
		for _, i := range col[f] {
			if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
				return strings.TrimSpace(rec[i])
			}
		}
		return ""
	}
	rep := &ImportReport{DryRun: in.DryRun, Counts: map[string]int{}, Warnings: []string{}, Gaps: []string{}, Errors: []string{}, KeyMap: map[string]string{}}
	var errRows [][]string
	d := &BundleData{Projects: []BProject{{Key: pkey}}}
	users := map[string]bool{}
	gap := map[string]bool{}
	note := func(k, msg string) {
		if !gap[k] {
			gap[k] = true
			rep.Gaps = append(rep.Gaps, msg)
		}
	}
	seen := map[string]bool{}
	for row := 2; ; row++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		fail := func(msg string) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("row %d: %s", row, msg))
			errRows = append(errRows, append([]string{fmt.Sprint(row), msg}, rec...))
		}
		if err != nil {
			fail("unreadable row: " + err.Error())
			continue
		}
		blank := true
		for _, c := range rec {
			if strings.TrimSpace(c) != "" {
				blank = false
			}
		}
		if blank {
			continue
		}
		bi := BIssue{Project: pkey, Type: m.DefaultType, Priority: "medium", Labels: []string{}, Created: time.Now(), Summary: get(rec, "summary")}
		if bi.Summary == "" {
			fail("the summary is empty")
			continue
		}
		if len([]rune(bi.Summary)) > maxSummary {
			bi.Summary = string([]rune(bi.Summary)[:maxSummary])
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("row %d: the summary was shortened to %d characters", row, maxSummary))
		}
		bi.Summary = strings.ReplaceAll(strings.ReplaceAll(bi.Summary, "\r", " "), "\n", " ")
		bi.Description, _ = Sanitize(get(rec, "description"))
		if bi.Type == "" {
			bi.Type = "Task"
		}
		if v := get(rec, "type"); v != "" {
			if t, ok := m.TypeMap[v]; ok {
				bi.Type = t
			} else if _, _, e := func() (string, int, error) { return "", 0, nil }(); e == nil && isKnownType(v) {
				bi.Type = v
			} else {
				note("type:"+v, fmt.Sprintf("issue type %q is not mapped: %s is used", v, bi.Type))
			}
		}
		bi.Status = "To Do"
		if v := get(rec, "status"); v != "" {
			if t, ok := m.StatusMap[v]; ok {
				bi.Status = t
			} else if isKnownStatus(v) {
				bi.Status = v
			} else {
				note("status:"+v, fmt.Sprintf("status %q is not mapped: To Do is used", v))
			}
		}
		if v := get(rec, "priority"); v != "" {
			if t, ok := m.PriorityMap[v]; ok {
				v = t
			}
			v = strings.ToLower(v)
			if !priorities[v] {
				note("priority:"+v, fmt.Sprintf("priority %q is not mapped: medium is used", get(rec, "priority")))
			} else {
				bi.Priority = v
			}
		}
		for f, dst := range map[string]*string{"assignee": &bi.Assignee, "reporter": &bi.Reporter} {
			if v := strings.ToLower(get(rec, f)); v != "" {
				if !strings.Contains(v, "@") {
					fail(fmt.Sprintf("%s %q is not an email address", f, v))
					bi.Summary = ""
					break
				}
				*dst = v
				users[v] = true
			}
		}
		if bi.Summary == "" {
			continue
		}
		for _, l := range col["labels"] {
			if l < len(rec) {
				for _, x := range splitNonEmpty(rec[l], sep) {
					if strings.ContainsAny(x, " \t") {
						x = strings.ReplaceAll(x, " ", "-")
					}
					bi.Labels = append(bi.Labels, x)
				}
			}
		}
		bad := false
		for _, f := range []string{"created", "updated", "resolved"} {
			if v := get(rec, f); v != "" {
				t, e := parseDate(v)
				if e != nil {
					fail(f + ": " + e.Error())
					bad = true
					break
				}
				switch f {
				case "created":
					bi.Created = t
				case "updated":
					bi.Updated = t
				case "resolved":
					bi.Resolved = &t
				}
			}
		}
		if bad {
			continue
		}
		if bi.Resolved != nil && bi.Status == "To Do" {
			bi.Status = "Done"
		}
		bi.Parent = get(rec, "parent")
		ext := get(rec, "key")
		if ext == "" { // a stable identity so that re-importing the same file updates instead of duplicating
			h := sha256.Sum256([]byte(bi.Summary + "\x00" + bi.Description + "\x00" + bi.Created.UTC().Format(time.RFC3339Nano)))
			ext = "csv:" + hex.EncodeToString(h[:8])
		}
		if seen[ext] {
			fail(fmt.Sprintf("the key %q appears twice in the file", ext))
			continue
		}
		seen[ext] = true
		bi.Key, bi.ExternalID = ext, ext
		d.Issues = append(d.Issues, bi)
		for _, i := range col["comment"] {
			if i < len(rec) && strings.TrimSpace(rec[i]) != "" {
				body, _ := Sanitize(strings.TrimSpace(rec[i]))
				h := sha256.Sum256([]byte(ext + "\x00" + body))
				d.Comments = append(d.Comments, BComment{ID: "csv:" + hex.EncodeToString(h[:8]), Issue: ext, Author: "", Body: body, Created: bi.Created})
			}
		}
	}
	for e := range users {
		d.Users = append(d.Users, BUser{Email: e, Name: strings.Split(e, "@")[0], Kind: "human"})
	}
	// the error report as CSV, for the person to fix and re-run
	var eb bytes.Buffer
	ew := csv.NewWriter(&eb)
	ew.Write(append([]string{"row", "problem"}, head...))
	for _, r := range errRows {
		ew.Write(r)
	}
	ew.Flush()
	rep.Counts["rows_rejected"] = len(errRows)
	rep.ErrorCSV = eb.String()
	// a bad row never blocks the good ones: report errors, import the rest (a project clash is not an error here: adding to a project is the point)
	rows := rep.Errors
	rep.Errors = []string{}
	out, err := s.applyBundle(ctx, a, d, nil, ImportOpts{DryRun: in.DryRun, Source: "csv", AllowExisting: true}, rep)
	if out != nil {
		out.Errors = append(rows, out.Errors...)
	}
	return out, err
}

func isKnownType(v string) bool {
	switch strings.ToLower(v) {
	case "epic", "story", "task", "bug", "subtask":
		return true
	}
	return false
}

func isKnownStatus(v string) bool {
	switch strings.ToLower(v) {
	case "to do", "in progress", "done":
		return true
	}
	return false
}

// ---- saved mapping templates (the wizard's mapping is reusable)

func (s *Service) SaveCSVTemplate(ctx context.Context, a Actor, name string, m CSVMapping) error {
	if !a.Admin {
		return ErrForbidden
	}
	if strings.TrimSpace(name) == "" || len(name) > 80 {
		return invalid("give the template a name of up to 80 characters")
	}
	b, _ := json.Marshal(m)
	_, err := s.DB.Exec(ctx, `INSERT INTO setting(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=$2`, "csvmap:"+name, string(b))
	return err
}

func (s *Service) CSVTemplate(ctx context.Context, a Actor, name string) (CSVMapping, error) {
	var m CSVMapping
	if !a.Admin {
		return m, ErrForbidden
	}
	var v string
	if err := s.DB.QueryRow(ctx, `SELECT value FROM setting WHERE key=$1`, "csvmap:"+name).Scan(&v); err != nil {
		return m, ErrNotFound
	}
	return m, json.Unmarshal([]byte(v), &m)
}
