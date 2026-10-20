package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// UIPrefs are all of a person's own UI settings (SPEC 4.6), in the same row as the UserSettings they extend, so they follow the person
// to every browser and device. Every key has a default; a person who never saved gets the defaults and version 0.
type UIPrefs struct {
	UserSettings
	Theme         string   `json:"theme"`          // dark | light; empty: not chosen, each browser keeps its own (dark by default)
	Density       string   `json:"density"`        // compact | default | comfortable
	Columns       []string `json:"columns"`        // list columns in order; key and summary are always there
	Sort          UISort   `json:"sort"`           // the list's sort when the query has none
	FontScale     int      `json:"font_scale"`     // percent: 90, 100, 115, 130
	ReducedMotion string   `json:"reduced_motion"` // system | on | off
	HighContrast  bool     `json:"high_contrast"`
	Shortcuts     bool     `json:"shortcuts"`  // single-key shortcuts (WCAG 2.2 SC 2.1.4); Ctrl/Cmd chords always work
	HintsSeen     []string `json:"hints_seen"` // onboarding hints dismissed: create, views, settings
}

type UISort struct {
	Field string `json:"field"` // empty: the query's own order (rank, then most recently updated)
	Dir   string `json:"dir"`   // asc | desc
}

// UIPrefsDoc is what GET and PUT answer: the settings and the version a later save must name in If-Match.
type UIPrefsDoc struct {
	Prefs   UIPrefs `json:"prefs"`
	Version int64   `json:"version"`
}

// UIPrefError names the key a save was refused for (HTTP 400, detail.key).
type UIPrefError struct{ Key, Msg string }

func (e *UIPrefError) Error() string { return e.Key + ": " + e.Msg }

// UIPrefConflict: the If-Match version is not the stored one, another tab saved first (HTTP 412 with the current settings).
type UIPrefConflict struct{ Current UIPrefsDoc }

func (e *UIPrefConflict) Error() string {
	return fmt.Sprintf("your settings were changed elsewhere (now version %d)", e.Current.Version)
}

// UIColumns are the built-in list columns, in their default order; custom fields are customfield_<id>.
var UIColumns = []string{"type", "key", "summary", "priority", "assignee", "status", "updated", "created", "reporter", "labels", "comments"}

// UISortable are the built-in columns a list can be sorted by (the TQL ORDER BY fields); custom fields sort too.
var UISortable = map[string]bool{"key": true, "summary": true, "priority": true, "assignee": true, "status": true, "updated": true, "created": true, "reporter": true, "type": true}

var UIHints = map[string]bool{"create": true, "views": true, "settings": true}

var customColRe = regexp.MustCompile(`^customfield_([0-9]{1,18})$`)

func DefaultUIPrefs() UIPrefs {
	return UIPrefs{UserSettings: defaultSettings(), Theme: "", Density: "default", Columns: []string{"type", "key", "summary", "priority", "assignee", "status", "updated"},
		FontScale: 100, ReducedMotion: "system", Shortcuts: true, HintsSeen: []string{}}
}

func (s *Service) MyUIPrefs(ctx context.Context, a Actor) (UIPrefsDoc, error) {
	if a.ID == "" {
		return UIPrefsDoc{}, ErrForbidden
	}
	return s.uiPrefsOf(ctx, s.DB, a.ID, false)
}

func (s *Service) uiPrefsOf(ctx context.Context, q querier, userID string, lock bool) (UIPrefsDoc, error) {
	d := UIPrefsDoc{Prefs: DefaultUIPrefs()}
	var raw []byte
	sql := `SELECT settings, version FROM user_setting WHERE user_id=$1`
	if lock {
		sql += ` FOR UPDATE`
	}
	err := q.QueryRow(ctx, sql, userID).Scan(&raw, &d.Version)
	if err == pgx.ErrNoRows {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	json.Unmarshal(raw, &d.Prefs) // keys saved before a default changed keep their saved value; missing keys keep the default
	if d.Prefs.HintsSeen == nil {
		d.Prefs.HintsSeen = []string{}
	}
	// a custom field trashed since it was chosen drops out of the list instead of failing it
	d.Prefs.Columns = s.liveColumns(ctx, q, d.Prefs.Columns)
	if customColRe.MatchString(d.Prefs.Sort.Field) && !s.customFieldLive(ctx, q, d.Prefs.Sort.Field) {
		d.Prefs.Sort = UISort{}
	}
	return d, nil
}

func (s *Service) customFieldLive(ctx context.Context, q querier, col string) bool {
	m := customColRe.FindStringSubmatch(col)
	if m == nil {
		return false
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	var ok bool
	q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM custom_field WHERE id=$1 AND trashed_at IS NULL)`, id).Scan(&ok)
	return ok
}

func (s *Service) liveColumns(ctx context.Context, q querier, cols []string) []string {
	out := []string{}
	for _, c := range cols {
		if customColRe.MatchString(c) && !s.customFieldLive(ctx, q, c) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// SetUIPrefs replaces the person's settings with raw, a JSON object of any subset of the keys (the rest keep their current value).
// ifMatch, when given, must be the stored version: a second tab saving on an older copy gets UIPrefConflict and nothing is stored.
// Unknown keys, wrong types and values out of range are refused naming the key.
func (s *Service) SetUIPrefs(ctx context.Context, a Actor, raw []byte, ifMatch *int64) (UIPrefsDoc, error) {
	if a.ID == "" {
		return UIPrefsDoc{}, ErrForbidden
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil || in == nil {
		return UIPrefsDoc{}, &UIPrefError{Key: "", Msg: "the settings must be a JSON object"}
	}
	var out UIPrefsDoc
	err := s.tx(ctx, func(tx pgx.Tx) error {
		cur, err := s.uiPrefsOf(ctx, tx, a.ID, true)
		if err != nil {
			return err
		}
		if ifMatch != nil && *ifMatch != cur.Version {
			return &UIPrefConflict{Current: cur}
		}
		p := cur.Prefs
		for k, v := range in {
			if err := s.setUIPref(ctx, tx, &p, k, v); err != nil {
				return err
			}
		}
		b, _ := json.Marshal(p)
		if err := tx.QueryRow(ctx, `INSERT INTO user_setting(user_id,settings) VALUES($1,$2)
			ON CONFLICT (user_id) DO UPDATE SET settings=EXCLUDED.settings, version=user_setting.version+1 RETURNING version`, a.ID, b).Scan(&out.Version); err != nil {
			return err
		}
		out.Prefs = p
		return nil
	})
	return out, err
}

func oneOf(v json.RawMessage, key string, allowed ...string) (string, error) {
	var x string
	if err := strictJSON(v, &x); err != nil {
		return "", &UIPrefError{key, "must be a string"}
	}
	for _, a := range allowed {
		if x == a {
			return x, nil
		}
	}
	return "", &UIPrefError{key, fmt.Sprintf("must be one of %v", allowed)}
}

func strictJSON(v json.RawMessage, into any) error {
	if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
		return fmt.Errorf("null")
	}
	return json.Unmarshal(v, into)
}

func (s *Service) setUIPref(ctx context.Context, q querier, p *UIPrefs, k string, v json.RawMessage) error {
	var err error
	switch k {
	case "theme":
		p.Theme, err = oneOf(v, k, "", "dark", "light")
	case "density":
		p.Density, err = oneOf(v, k, "compact", "default", "comfortable")
	case "list_view":
		p.ListView, err = oneOf(v, k, "short", "long")
	case "page_width":
		p.PageWidth, err = oneOf(v, k, "wide", "narrow")
	case "snippet_chars":
		var n int
		if strictJSON(v, &n) != nil {
			return &UIPrefError{k, "must be a whole number"}
		}
		if n < 1 || n > MaxSnippetChars {
			return &UIPrefError{k, fmt.Sprintf("must be between 1 and %d", MaxSnippetChars)}
		}
		p.SnippetChars = n
	case "reduced_motion":
		p.ReducedMotion, err = oneOf(v, k, "system", "on", "off")
	case "font_scale":
		var n int
		if strictJSON(v, &n) != nil {
			return &UIPrefError{k, "must be a whole number"}
		}
		if n != 90 && n != 100 && n != 115 && n != 130 {
			return &UIPrefError{k, "must be 90, 100, 115 or 130"}
		}
		p.FontScale = n
	case "high_contrast", "shortcuts":
		var b bool
		if strictJSON(v, &b) != nil {
			return &UIPrefError{k, "must be true or false"}
		}
		if k == "shortcuts" {
			p.Shortcuts = b
		} else {
			p.HighContrast = b
		}
	case "columns":
		var cols []string
		if strictJSON(v, &cols) != nil {
			return &UIPrefError{k, "must be a list of column names"}
		}
		if len(cols) > 30 {
			return &UIPrefError{k, "at most 30 columns"}
		}
		builtin := map[string]bool{}
		for _, c := range UIColumns {
			builtin[c] = true
		}
		seen, out := map[string]bool{}, []string{}
		for _, c := range cols {
			if seen[c] {
				continue
			}
			if !builtin[c] && !s.customFieldLive(ctx, q, c) {
				return &UIPrefError{k, fmt.Sprintf("unknown column %q", c)}
			}
			seen[c] = true
			out = append(out, c)
		}
		// the key and the summary identify the row: they cannot be hidden
		for _, must := range []string{"key", "summary"} {
			if !seen[must] {
				out = append(out, must)
			}
		}
		p.Columns = out
	case "sort":
		var so UISort
		dec := json.NewDecoder(bytes.NewReader(v))
		dec.DisallowUnknownFields()
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || dec.Decode(&so) != nil {
			return &UIPrefError{k, `must be {"field": ..., "dir": "asc"|"desc"}`}
		}
		if so.Field != "" && !UISortable[so.Field] && !s.customFieldLive(ctx, q, so.Field) {
			return &UIPrefError{k, fmt.Sprintf("cannot sort by %q", so.Field)}
		}
		if so.Dir == "" {
			so.Dir = "asc"
		}
		if so.Dir != "asc" && so.Dir != "desc" {
			return &UIPrefError{k, "dir must be asc or desc"}
		}
		p.Sort = so
	case "hints_seen":
		var hs []string
		if strictJSON(v, &hs) != nil {
			return &UIPrefError{k, "must be a list of hint names"}
		}
		seen, out := map[string]bool{}, []string{}
		for _, x := range hs {
			if !UIHints[x] {
				return &UIPrefError{k, fmt.Sprintf("unknown hint %q (create, views, settings)", x)}
			}
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
		p.HintsSeen = out
	default:
		return &UIPrefError{k, "unknown setting"}
	}
	return err
}

// ColumnValues gives, for the issues of a list page, the columns the search row does not carry: reporter, created and custom
// fields (customfield_<id>). Custom values come through CustomValues, so a field the person may not see (restricted by role, or
// trashed) never reaches them. The keys come from a search already scoped to the person; each is checked again here.
func (s *Service) ColumnValues(ctx context.Context, a Actor, keys, cols []string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	want := map[string]bool{}
	custom := false
	for _, c := range cols {
		want[c] = true
		if customColRe.MatchString(c) {
			custom = true
		}
	}
	for _, k := range keys {
		is, role, err := s.access(ctx, a, k)
		if err != nil {
			continue
		}
		m := map[string]any{}
		if want["reporter"] || want["created"] {
			var rep *string
			var created time.Time
			s.DB.QueryRow(ctx, `SELECT u.name, i.created_at FROM issue i LEFT JOIN app_user u ON u.id=i.reporter_id WHERE i.id=$1`, is.ID).Scan(&rep, &created)
			if want["reporter"] {
				m["reporter"] = ""
				if rep != nil {
					m["reporter"] = *rep
				}
			}
			if want["created"] {
				m["created"] = created
			}
		}
		if custom {
			var ptid, ppid string
			s.DB.QueryRow(ctx, `SELECT i.issue_type_id::text,i.project_id::text FROM issue i WHERE i.id=$1`, is.ID).Scan(&ptid, &ppid)
			if cv, err := s.CustomValues(ctx, a, is.ID, ppid, ptid, role); err == nil {
				for c := range want {
					if v, ok := cv[c]; ok && customColRe.MatchString(c) {
						m[c] = v
					}
				}
			}
		}
		out[is.Key] = m
	}
	return out, nil
}

// ValidColumn says whether a list may ask for the column: a built-in one, or a live custom field.
func (s *Service) ValidColumn(ctx context.Context, c string) bool {
	for _, b := range UIColumns {
		if b == c {
			return true
		}
	}
	return customColRe.MatchString(c) && s.customFieldLive(ctx, s.DB, c)
}
