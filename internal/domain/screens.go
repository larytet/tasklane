package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Screens, screen schemes, field configurations and behavior rules (SPEC 8). They are data edited at runtime and decide
// which fields a person sees on create, edit and view, which are required or hidden, and what depends on what.

var builtinFields = map[string]string{"description": "Description", "priority": "Priority", "assignee": "Assignee", "labels": "Labels"}
var builtinOrder = []string{"description", "priority", "assignee", "labels"}

var operations = map[string]bool{"create": true, "edit": true, "view": true}

type ScreenTab struct {
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
}

type Screen struct {
	ID          int64       `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Tabs        []ScreenTab `json:"tabs"`
}

type SchemeEntry struct {
	Operation string `json:"operation"`
	Type      string `json:"type"` // empty: the default for every issue type
	Screen    int64  `json:"screen"`
}

type Scheme struct {
	ID      int64         `json:"id"`
	Name    string        `json:"name"`
	Entries []SchemeEntry `json:"entries"`
}

type FieldCfgRow struct {
	ID       int64   `json:"id"`
	Project  string  `json:"project"` // empty: every project
	Type     string  `json:"type"`    // empty: every issue type
	Field    string  `json:"field"`
	Required *bool   `json:"required"`
	Hidden   *bool   `json:"hidden"`
	HelpText *string `json:"help_text"`
}

type Rule struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Project   string `json:"project"`
	Type      string `json:"type"`
	Position  int    `json:"position"`
	Enabled   bool   `json:"enabled"`
	WhenField string `json:"when_field"`
	WhenOp    string `json:"when_op"`
	WhenValue any    `json:"when_value"`
	WhenRole  string `json:"when_role"`
	Action    string `json:"action"`
	Target    string `json:"target"`
}

func (s *Service) validFieldKey(ctx context.Context, q querier, key string) error {
	if _, ok := builtinFields[key]; ok {
		return nil
	}
	if id, ok := parseFieldKey(key); ok {
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM custom_field WHERE id=$1`, id).Scan(&n); err == nil && n == 1 {
			return nil
		}
	}
	return invalid("%q is not a field (built-in fields: description, priority, assignee, labels; custom fields are customfield_<id>)", key)
}

func canonKey(key string) string {
	key = strings.TrimSpace(key)
	if id, ok := parseFieldKey(key); ok {
		return fieldKey(id)
	}
	return strings.ToLower(key)
}

// ----- screens

func (s *Service) SaveScreen(ctx context.Context, a Actor, id int64, in Screen) (Screen, error) {
	if err := adminOnly(a); err != nil {
		return Screen{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Screen{}, invalid("a screen needs a name")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if id == 0 {
			if err := tx.QueryRow(ctx, `INSERT INTO screen(name,description) VALUES($1,$2) RETURNING id`, in.Name, in.Description).Scan(&id); err != nil {
				if strings.Contains(err.Error(), "screen_name_uq") {
					return invalid("a screen named %q exists", in.Name)
				}
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE screen SET name=$2,description=$3 WHERE id=$1`, id, in.Name, in.Description)
			if err != nil {
				if strings.Contains(err.Error(), "screen_name_uq") {
					return invalid("a screen named %q exists", in.Name)
				}
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			if _, err := tx.Exec(ctx, `DELETE FROM screen_tab WHERE screen_id=$1`, id); err != nil {
				return err
			}
		}
		seen := map[string]bool{}
		for i, t := range in.Tabs {
			if strings.TrimSpace(t.Name) == "" {
				return invalid("a tab needs a name")
			}
			var tid int64
			if err := tx.QueryRow(ctx, `INSERT INTO screen_tab(screen_id,name,position) VALUES($1,$2,$3) RETURNING id`, id, strings.TrimSpace(t.Name), i).Scan(&tid); err != nil {
				return err
			}
			for j, f := range t.Fields {
				f = canonKey(f)
				if err := s.validFieldKey(ctx, tx, f); err != nil {
					return err
				}
				if seen[f] {
					return invalid("%s is on the screen twice", f)
				}
				seen[f] = true
				if _, err := tx.Exec(ctx, `INSERT INTO screen_field(tab_id,field_key,position) VALUES($1,$2,$3)`, tid, f, j); err != nil {
					return err
				}
			}
		}
		return s.audit(ctx, tx, a, "screen.save", fmt.Sprint(id), map[string]any{"name": in.Name})
	})
	if err != nil {
		return Screen{}, err
	}
	return s.GetScreen(ctx, id)
}

func (s *Service) GetScreen(ctx context.Context, id int64) (Screen, error) {
	all, err := s.ListScreens(ctx)
	if err != nil {
		return Screen{}, err
	}
	for _, sc := range all {
		if sc.ID == id {
			return sc, nil
		}
	}
	return Screen{}, ErrNotFound
}

func (s *Service) ListScreens(ctx context.Context) ([]Screen, error) {
	rows, err := s.DB.Query(ctx, `SELECT s.id,s.name,s.description,t.id,t.name,f.field_key FROM screen s
		LEFT JOIN screen_tab t ON t.screen_id=s.id LEFT JOIN screen_field f ON f.tab_id=t.id ORDER BY lower(s.name),s.id,t.position,t.id,f.position,f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Screen{}
	lastTab := int64(-1)
	for rows.Next() {
		var sid int64
		var name, desc string
		var tid *int64
		var tname, fkey *string
		if err := rows.Scan(&sid, &name, &desc, &tid, &tname, &fkey); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != sid {
			out = append(out, Screen{ID: sid, Name: name, Description: desc, Tabs: []ScreenTab{}})
			lastTab = -1
		}
		sc := &out[len(out)-1]
		if tid != nil {
			if *tid != lastTab {
				sc.Tabs = append(sc.Tabs, ScreenTab{Name: *tname, Fields: []string{}})
				lastTab = *tid
			}
			if fkey != nil {
				t := &sc.Tabs[len(sc.Tabs)-1]
				t.Fields = append(t.Fields, *fkey)
			}
		}
	}
	return out, rows.Err()
}

func (s *Service) DeleteScreen(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM screen_scheme_entry WHERE screen_id=$1`, id).Scan(&n)
	if n > 0 {
		return invalid("the screen is used by %d screen scheme entr%s; remove those first", n, map[bool]string{true: "y", false: "ies"}[n == 1])
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM screen WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ----- screen schemes

func (s *Service) SaveScheme(ctx context.Context, a Actor, id int64, in Scheme) (Scheme, error) {
	if err := adminOnly(a); err != nil {
		return Scheme{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Scheme{}, invalid("a screen scheme needs a name")
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		dup := func(err error) error {
			if err != nil && strings.Contains(err.Error(), "screen_scheme_name_uq") {
				return invalid("a screen scheme named %q exists", in.Name)
			}
			return err
		}
		if id == 0 {
			if err := dup(tx.QueryRow(ctx, `INSERT INTO screen_scheme(name) VALUES($1) RETURNING id`, in.Name).Scan(&id)); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE screen_scheme SET name=$2 WHERE id=$1`, id, in.Name)
			if err = dup(err); err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			if _, err := tx.Exec(ctx, `DELETE FROM screen_scheme_entry WHERE scheme_id=$1`, id); err != nil {
				return err
			}
		}
		for _, e := range in.Entries {
			if !operations[e.Operation] {
				return invalid("an operation is create, edit or view (got %q)", e.Operation)
			}
			var typeID *string
			if e.Type != "" {
				var t string
				if err := tx.QueryRow(ctx, `SELECT id::text FROM issue_type WHERE lower(name)=lower($1)`, e.Type).Scan(&t); err != nil {
					return invalid("unknown issue type %q", e.Type)
				}
				typeID = &t
			}
			if _, err := tx.Exec(ctx, `INSERT INTO screen_scheme_entry(scheme_id,operation,issue_type_id,screen_id) VALUES($1,$2,$3::uuid,$4)`, id, e.Operation, typeID, e.Screen); err != nil {
				if strings.Contains(err.Error(), "screen_scheme_entry_uq") {
					return invalid("the %s operation is mapped twice for the same issue type", e.Operation)
				}
				if strings.Contains(err.Error(), "screen_scheme_entry_screen_id_fkey") {
					return invalid("screen %d does not exist", e.Screen)
				}
				return err
			}
		}
		return s.audit(ctx, tx, a, "screen_scheme.save", fmt.Sprint(id), map[string]any{"name": in.Name})
	})
	if err != nil {
		return Scheme{}, err
	}
	all, err := s.ListSchemes(ctx)
	for _, x := range all {
		if x.ID == id {
			return x, err
		}
	}
	return Scheme{}, err
}

func (s *Service) ListSchemes(ctx context.Context) ([]Scheme, error) {
	rows, err := s.DB.Query(ctx, `SELECT c.id,c.name,e.operation,COALESCE(t.name,''),e.screen_id FROM screen_scheme c
		LEFT JOIN screen_scheme_entry e ON e.scheme_id=c.id LEFT JOIN issue_type t ON t.id=e.issue_type_id ORDER BY lower(c.name),c.id,e.operation,t.name NULLS FIRST`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Scheme{}
	for rows.Next() {
		var id int64
		var name string
		var op, typ *string
		var sc *int64
		if err := rows.Scan(&id, &name, &op, &typ, &sc); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != id {
			out = append(out, Scheme{ID: id, Name: name, Entries: []SchemeEntry{}})
		}
		if op != nil {
			c := &out[len(out)-1]
			c.Entries = append(c.Entries, SchemeEntry{Operation: *op, Type: *typ, Screen: *sc})
		}
	}
	return out, rows.Err()
}

func (s *Service) DeleteScheme(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM project WHERE screen_scheme_id=$1`, id).Scan(&n)
	if n > 0 {
		return invalid("%d project(s) use this screen scheme; switch them first", n)
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM screen_scheme WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetProjectScheme picks the screen scheme of a project; nil goes back to "every applicable field, one tab".
func (s *Service) SetProjectScheme(ctx context.Context, a Actor, projectKey string, scheme *int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	if scheme != nil {
		var n int
		s.DB.QueryRow(ctx, `SELECT count(*) FROM screen_scheme WHERE id=$1`, *scheme).Scan(&n)
		if n == 0 {
			return invalid("screen scheme %d does not exist", *scheme)
		}
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE project SET screen_scheme_id=$2 WHERE id=$1`, pid, scheme); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "project.screen_scheme", projectKey, map[string]any{"scheme": scheme})
	})
}

// ----- field configurations

func (s *Service) scopeIDs(ctx context.Context, q querier, project, typ string) (*string, *string, error) {
	var p, t *string
	if project != "" {
		var x string
		if err := q.QueryRow(ctx, `SELECT id::text FROM project WHERE key=$1`, strings.ToUpper(project)).Scan(&x); err != nil {
			return nil, nil, invalid("unknown project %q", project)
		}
		p = &x
	}
	if typ != "" {
		var x string
		if err := q.QueryRow(ctx, `SELECT id::text FROM issue_type WHERE lower(name)=lower($1)`, typ).Scan(&x); err != nil {
			return nil, nil, invalid("unknown issue type %q", typ)
		}
		t = &x
	}
	return p, t, nil
}

// SaveFieldConfig sets the overrides for one field at one scope (project and type, either may be empty = all).
func (s *Service) SaveFieldConfig(ctx context.Context, a Actor, in FieldCfgRow) (FieldCfgRow, error) {
	if err := adminOnly(a); err != nil {
		return in, err
	}
	in.Field = canonKey(in.Field)
	if err := s.validFieldKey(ctx, s.DB, in.Field); err != nil {
		return in, err
	}
	p, t, err := s.scopeIDs(ctx, s.DB, in.Project, in.Type)
	if err != nil {
		return in, err
	}
	in.Project = strings.ToUpper(in.Project)
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO field_config(project_id,issue_type_id,field_key,required,hidden,help_text) VALUES($1::uuid,$2::uuid,$3,$4,$5,$6)
			ON CONFLICT (COALESCE(project_id,'00000000-0000-0000-0000-000000000000'),COALESCE(issue_type_id,'00000000-0000-0000-0000-000000000000'),field_key)
			DO UPDATE SET required=EXCLUDED.required,hidden=EXCLUDED.hidden,help_text=EXCLUDED.help_text RETURNING id`, p, t, in.Field, in.Required, in.Hidden, in.HelpText).Scan(&in.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "field_config.save", fmt.Sprint(in.ID), in)
	})
	return in, err
}

func (s *Service) ListFieldConfigs(ctx context.Context) ([]FieldCfgRow, error) {
	rows, err := s.DB.Query(ctx, `SELECT c.id,COALESCE(p.key,''),COALESCE(t.name,''),c.field_key,c.required,c.hidden,c.help_text FROM field_config c
		LEFT JOIN project p ON p.id=c.project_id LEFT JOIN issue_type t ON t.id=c.issue_type_id ORDER BY c.field_key,p.key NULLS FIRST,t.name NULLS FIRST`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FieldCfgRow{}
	for rows.Next() {
		var r FieldCfgRow
		if err := rows.Scan(&r.ID, &r.Project, &r.Type, &r.Field, &r.Required, &r.Hidden, &r.HelpText); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) DeleteFieldConfig(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM field_config WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ----- behavior rules

var ruleOps = map[string]bool{"eq": true, "ne": true, "in": true, "empty": true, "notempty": true}

func (s *Service) SaveRule(ctx context.Context, a Actor, id int64, in Rule) (Rule, error) {
	if err := adminOnly(a); err != nil {
		return in, err
	}
	if in.WhenOp == "" {
		in.WhenOp = "eq"
	}
	if !ruleOps[in.WhenOp] {
		return in, invalid("a condition is eq, ne, in, empty or notempty")
	}
	if in.Action != "show" && in.Action != "hide" && in.Action != "require" {
		return in, invalid("an action is show, hide or require")
	}
	if in.WhenRole != "" && in.WhenRole != "viewer" && in.WhenRole != "member" && in.WhenRole != "admin" {
		return in, invalid("a role is viewer, member or admin")
	}
	in.Target = canonKey(in.Target)
	if err := s.validFieldKey(ctx, s.DB, in.Target); err != nil {
		return in, err
	}
	if in.WhenField != "" {
		in.WhenField = canonKey(in.WhenField)
		if in.WhenField == in.Target {
			return in, invalid("a rule cannot depend on the field it changes")
		}
		if err := s.validFieldKey(ctx, s.DB, in.WhenField); err != nil {
			return in, err
		}
	}
	p, t, err := s.scopeIDs(ctx, s.DB, in.Project, in.Type)
	if err != nil {
		return in, err
	}
	wv, _ := json.Marshal(in.WhenValue)
	if in.WhenValue == nil {
		wv = nil
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if id == 0 {
			if err := tx.QueryRow(ctx, `INSERT INTO behavior_rule(name,project_id,issue_type_id,position,enabled,when_field,when_op,when_value,when_role,action,target_field)
				VALUES($1,$2::uuid,$3::uuid,$4,$5,$6,$7,$8::jsonb,$9,$10,$11) RETURNING id`, in.Name, p, t, in.Position, in.Enabled, in.WhenField, in.WhenOp, wv, in.WhenRole, in.Action, in.Target).Scan(&id); err != nil {
				return err
			}
		} else {
			tag, err := tx.Exec(ctx, `UPDATE behavior_rule SET name=$2,project_id=$3::uuid,issue_type_id=$4::uuid,position=$5,enabled=$6,when_field=$7,when_op=$8,when_value=$9::jsonb,when_role=$10,action=$11,target_field=$12 WHERE id=$1`,
				id, in.Name, p, t, in.Position, in.Enabled, in.WhenField, in.WhenOp, wv, in.WhenRole, in.Action, in.Target)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
		return s.audit(ctx, tx, a, "behavior_rule.save", fmt.Sprint(id), in)
	})
	in.ID = id
	in.Project = strings.ToUpper(in.Project)
	return in, err
}

const ruleCols = `r.id,r.name,COALESCE(p.key,''),COALESCE(t.name,''),r.position,r.enabled,r.when_field,r.when_op,r.when_value,r.when_role,r.action,r.target_field`

func scanRules(rows pgx.Rows) ([]Rule, error) {
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var r Rule
		var raw []byte
		if err := rows.Scan(&r.ID, &r.Name, &r.Project, &r.Type, &r.Position, &r.Enabled, &r.WhenField, &r.WhenOp, &raw, &r.WhenRole, &r.Action, &r.Target); err != nil {
			return nil, err
		}
		if raw != nil {
			json.Unmarshal(raw, &r.WhenValue)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) ListRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+ruleCols+` FROM behavior_rule r LEFT JOIN project p ON p.id=r.project_id LEFT JOIN issue_type t ON t.id=r.issue_type_id ORDER BY r.position,r.id`)
	if err != nil {
		return nil, err
	}
	return scanRules(rows)
}

func (s *Service) DeleteRule(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM behavior_rule WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ----- evaluation

// FieldState is what the form says about one field after the screen, the field configuration and the rules.
type FieldState struct {
	Hidden   bool
	Required bool
}

// ruleHolds reports whether a rule's condition is met by the current values and role.
func ruleHolds(r Rule, vals map[string]any, role string) bool {
	if r.WhenRole != "" && r.WhenRole != role {
		return false
	}
	if r.WhenField == "" {
		return true
	}
	v := vals[r.WhenField]
	has := func(list any, x any) bool {
		for _, e := range asStrings(list) {
			if e == fmt.Sprint(x) {
				return true
			}
		}
		return false
	}
	switch r.WhenOp {
	case "empty":
		return isEmptyValue(v)
	case "notempty":
		return !isEmptyValue(v)
	case "eq":
		if isEmptyValue(v) {
			return false
		}
		if _, isList := v.([]any); isList {
			return has(v, r.WhenValue)
		}
		if _, isList := v.([]string); isList {
			return has(v, r.WhenValue)
		}
		return fmt.Sprint(v) == fmt.Sprint(r.WhenValue)
	case "ne":
		return !ruleHolds(Rule{WhenField: r.WhenField, WhenOp: "eq", WhenValue: r.WhenValue}, vals, "")
	case "in":
		if isEmptyValue(v) {
			return false
		}
		for _, e := range asStrings(v) {
			if has(r.WhenValue, e) {
				return true
			}
		}
		return false
	}
	return false
}

func asStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	default:
		return []string{fmt.Sprint(x)}
	}
}

func evalRules(rules []Rule, vals map[string]any, role string) map[string]FieldState {
	out := map[string]FieldState{}
	for _, r := range rules {
		if !r.Enabled || !ruleHolds(r, vals, role) {
			continue
		}
		st := out[r.Target]
		switch r.Action {
		case "show":
			st.Hidden = false
		case "hide":
			st.Hidden = true
		case "require":
			st.Required = true
		}
		out[r.Target] = st
	}
	return out
}

// formCtx is everything that applies to one project, issue type and operation, read once.
type formCtx struct {
	OnScreen map[string]bool // nil: no screen scheme, every applicable field is on the form
	Screen   *Screen
	Cfg      map[string]FieldCfgRow // resolved by specificity, per field
	Rules    []Rule
}

func (s *Service) loadForm(ctx context.Context, q querier, projectID, typeID, op string) (*formCtx, error) {
	fc := &formCtx{Cfg: map[string]FieldCfgRow{}}
	var schemeID *int64
	if err := q.QueryRow(ctx, `SELECT screen_scheme_id FROM project WHERE id::text=$1`, projectID).Scan(&schemeID); err != nil {
		return nil, err
	}
	if schemeID != nil && op != "" {
		var screenID int64
		err := q.QueryRow(ctx, `SELECT screen_id FROM screen_scheme_entry WHERE scheme_id=$1 AND operation=$2 AND (issue_type_id::text=$3 OR issue_type_id IS NULL)
			ORDER BY issue_type_id IS NULL LIMIT 1`, *schemeID, op, typeID).Scan(&screenID)
		if err == nil {
			rows, err := q.Query(ctx, `SELECT t.name,f.field_key FROM screen_tab t LEFT JOIN screen_field f ON f.tab_id=t.id WHERE t.screen_id=$1 ORDER BY t.position,t.id,f.position,f.id`, screenID)
			if err != nil {
				return nil, err
			}
			sc := &Screen{ID: screenID, Tabs: []ScreenTab{}}
			fc.OnScreen = map[string]bool{}
			for rows.Next() {
				var tn string
				var fk *string
				if err := rows.Scan(&tn, &fk); err != nil {
					rows.Close()
					return nil, err
				}
				if len(sc.Tabs) == 0 || sc.Tabs[len(sc.Tabs)-1].Name != tn {
					sc.Tabs = append(sc.Tabs, ScreenTab{Name: tn, Fields: []string{}})
				}
				if fk != nil {
					t := &sc.Tabs[len(sc.Tabs)-1]
					t.Fields = append(t.Fields, *fk)
					fc.OnScreen[*fk] = true
				}
			}
			rows.Close()
			fc.Screen = sc
		} else if err != pgx.ErrNoRows {
			return nil, err
		}
	}
	// field configuration: project+type beats project beats type beats global, attribute by attribute
	rows, err := q.Query(ctx, `SELECT field_key,required,hidden,help_text,
		(CASE WHEN project_id IS NOT NULL THEN 2 ELSE 0 END)+(CASE WHEN issue_type_id IS NOT NULL THEN 1 ELSE 0 END) AS spec
		FROM field_config WHERE (project_id IS NULL OR project_id::text=$1) AND (issue_type_id IS NULL OR issue_type_id::text=$2) ORDER BY spec`, projectID, typeID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r FieldCfgRow
		var spec int
		if err := rows.Scan(&r.Field, &r.Required, &r.Hidden, &r.HelpText, &spec); err != nil {
			rows.Close()
			return nil, err
		}
		cur := fc.Cfg[r.Field] // more specific rows come later and override what they set
		cur.Field = r.Field
		if r.Required != nil {
			cur.Required = r.Required
		}
		if r.Hidden != nil {
			cur.Hidden = r.Hidden
		}
		if r.HelpText != nil {
			cur.HelpText = r.HelpText
		}
		fc.Cfg[r.Field] = cur
	}
	rows.Close()
	rr, err := q.Query(ctx, `SELECT `+ruleCols+` FROM behavior_rule r LEFT JOIN project p ON p.id=r.project_id LEFT JOIN issue_type t ON t.id=r.issue_type_id
		WHERE r.enabled AND (r.project_id IS NULL OR r.project_id::text=$1) AND (r.issue_type_id IS NULL OR r.issue_type_id::text=$2) ORDER BY r.position,r.id`, projectID, typeID)
	if err != nil {
		return nil, err
	}
	if fc.Rules, err = scanRules(rr); err != nil {
		return nil, err
	}
	return fc, nil
}

func (fc *formCtx) onForm(key string) bool { return fc.OnScreen == nil || fc.OnScreen[key] }

// state is the final hidden and required flags for a field given the values.
func (fc *formCtx) state(key string, vals map[string]any, role string, ctxRequired bool) FieldState {
	st := FieldState{Required: ctxRequired}
	if c, ok := fc.Cfg[key]; ok {
		if c.Required != nil {
			st.Required = *c.Required || ctxRequired
		}
		if c.Hidden != nil {
			st.Hidden = *c.Hidden
		}
	}
	if rs, ok := evalRules(fc.Rules, vals, role)[key]; ok {
		// rules run after the configuration; show beats a configured hide only when a rule says so
		for _, r := range fc.Rules {
			if r.Target == key && r.Enabled && ruleHolds(r, vals, role) {
				switch r.Action {
				case "show":
					st.Hidden = false
				case "hide":
					st.Hidden = true
				}
			}
		}
		st.Required = st.Required || rs.Required
	}
	return st
}

// checkForm validates the touched fields (set to a value, or cleared when the value is nil) against the screen, the
// field configuration and the rules. vals are the values as they will be after the change. At create every required
// field on the form must be present; at edit only the touched fields and the fields a touched value made required are checked.
func (s *Service) checkForm(fc *formCtx, op, role string, names map[string]string, ctxRequired map[string]bool, vals map[string]any, touched map[string]any) error {
	name := func(k string) string {
		if n := names[k]; n != "" {
			return n
		}
		if n := builtinFields[k]; n != "" {
			return n
		}
		return k
	}
	keys := make([]string, 0, len(touched))
	for k := range touched {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := touched[k]
		if !isEmptyValue(v) {
			if !fc.onForm(k) {
				return invalid("%s is not on the %s screen of this project", name(k), op)
			}
			if st := fc.state(k, vals, role, ctxRequired[k]); st.Hidden {
				return invalid("%s is hidden for this issue", name(k))
			}
		}
	}
	check := map[string]bool{}
	if op == "create" {
		for k := range ctxRequired {
			check[k] = true
		}
		for k := range fc.Cfg {
			check[k] = true
		}
		for _, r := range fc.Rules {
			check[r.Target] = true
		}
		for k := range builtinFields {
			check[k] = true
		}
	} else {
		for k := range touched {
			check[k] = true
		}
		for _, r := range fc.Rules {
			if r.Action != "require" {
				continue
			}
			if _, ok := touched[r.WhenField]; ok {
				check[r.Target] = true
			}
		}
	}
	var missing []string
	for k := range check {
		if !fc.onForm(k) {
			continue
		}
		st := fc.state(k, vals, role, ctxRequired[k])
		if st.Required && !st.Hidden && isEmptyValue(vals[k]) {
			missing = append(missing, name(k))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return invalid("%s %s required", strings.Join(missing, ", "), map[bool]string{true: "is", false: "are"}[len(missing) == 1])
	}
	return nil
}

// ----- the form definition served to the interface

type FormField struct {
	Key      string        `json:"key"`
	Name     string        `json:"name"`
	Builtin  bool          `json:"builtin"`
	Type     string        `json:"type"`
	Required bool          `json:"required"`
	Hidden   bool          `json:"hidden"`
	HelpText string        `json:"help_text,omitempty"`
	Unit     string        `json:"unit,omitempty"`
	Default  any           `json:"default,omitempty"`
	Options  []FieldOption `json:"options,omitempty"`
}

type FormTab struct {
	Name   string      `json:"name"`
	Fields []FormField `json:"fields"`
}

type FormDef struct {
	Operation string    `json:"operation"`
	Role      string    `json:"role"`
	Screen    string    `json:"screen,omitempty"`
	Tabs      []FormTab `json:"tabs"`
	Rules     []Rule    `json:"rules"`
}

// Form is the screen for an operation: tabs of fields with their required and hidden flags (before rules, which the
// client re-evaluates as values change) and the rules that apply.
func (s *Service) Form(ctx context.Context, a Actor, projectKey, typeName, op string) (FormDef, error) {
	if !operations[op] {
		return FormDef{}, invalid("an operation is create, edit or view")
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return FormDef{}, err
	}
	role := s.role(ctx, a, pid)
	if role == "" && !a.Admin {
		return FormDef{}, ErrNotFound
	}
	var tid string
	if err := s.DB.QueryRow(ctx, `SELECT id::text FROM issue_type WHERE lower(name)=lower($1)`, typeName).Scan(&tid); err != nil {
		return FormDef{}, invalid("unknown issue type %q", typeName)
	}
	fc, err := s.loadForm(ctx, s.DB, pid, tid, op)
	if err != nil {
		return FormDef{}, err
	}
	cfgs, err := s.fieldsFor(ctx, s.DB, pid, tid)
	if err != nil {
		return FormDef{}, err
	}
	custom := map[string]FieldConfig{}
	for _, c := range cfgs {
		if roleIn(c.ViewRoles, role, a.Admin) {
			custom[c.Key] = c
		}
	}
	field := func(k string) (FormField, bool) {
		var f FormField
		if n, ok := builtinFields[k]; ok {
			f = FormField{Key: k, Name: n, Builtin: true, Type: map[string]string{"description": "textarea", "priority": "priority", "assignee": "user", "labels": "labels"}[k]}
		} else if c, ok := custom[k]; ok {
			f = FormField{Key: k, Name: c.Name, Type: c.Type, Required: c.Required, HelpText: c.HelpText, Unit: c.Unit, Default: c.Default, Options: c.Options}
		} else {
			return f, false // not applicable here, or not visible to this role
		}
		if x, ok := fc.Cfg[k]; ok {
			if x.Required != nil {
				f.Required = *x.Required || f.Required
			}
			if x.Hidden != nil {
				f.Hidden = *x.Hidden
			}
			if x.HelpText != nil && *x.HelpText != "" {
				f.HelpText = *x.HelpText
			}
		}
		return f, true
	}
	if a.Admin {
		role = "admin"
	}
	out := FormDef{Operation: op, Role: role, Tabs: []FormTab{}, Rules: fc.Rules}
	if out.Rules == nil {
		out.Rules = []Rule{}
	}
	if fc.Screen != nil {
		for _, t := range fc.Screen.Tabs {
			ft := FormTab{Name: t.Name, Fields: []FormField{}}
			for _, k := range t.Fields {
				if f, ok := field(k); ok {
					ft.Fields = append(ft.Fields, f)
				}
			}
			out.Tabs = append(out.Tabs, ft)
		}
		return out, nil
	}
	ft := FormTab{Name: "Details", Fields: []FormField{}}
	for _, k := range builtinOrder {
		if f, ok := field(k); ok {
			ft.Fields = append(ft.Fields, f)
		}
	}
	for _, c := range cfgs {
		if f, ok := field(c.Key); ok {
			ft.Fields = append(ft.Fields, f)
		}
	}
	out.Tabs = append(out.Tabs, ft)
	return out, nil
}

func nonEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// checkCreateForm applies the screen, field configuration and rules to a new issue (inside its transaction).
func (s *Service) checkCreateForm(ctx context.Context, tx pgx.Tx, pid, typeID, role string, in CreateIn, vals map[int64]any, cfgs []FieldConfig) error {
	fc, err := s.loadForm(ctx, tx, pid, typeID, "create")
	if err != nil {
		return err
	}
	if fc.OnScreen == nil && len(fc.Cfg) == 0 && len(fc.Rules) == 0 {
		return nil // nothing configured: the common case costs three queries
	}
	names, req := map[string]string{}, map[string]bool{}
	byName := map[string]string{}
	all := map[string]any{"description": nonEmpty(in.Description), "priority": nonEmpty(in.Priority), "assignee": nonEmpty(in.Assignee)}
	if len(in.Labels) > 0 {
		all["labels"] = in.Labels
	}
	touched := map[string]any{}
	for k, v := range all {
		if !isEmptyValue(v) {
			touched[k] = v
		}
	}
	for _, c := range cfgs {
		names[c.Key], req[c.Key] = c.Name, c.Required
		byName[strings.ToLower(c.Name)] = c.Key
		if v, ok := vals[c.ID]; ok {
			all[c.Key] = v
		}
	}
	for k, v := range in.Custom {
		key := strings.ToLower(k)
		if n, ok := byName[key]; ok {
			key = n
		}
		if !isEmptyValue(v) {
			touched[key] = v
		}
	}
	return s.checkForm(fc, "create", role, names, req, all, touched)
}

// checkEditForm applies them to a change of an existing issue. The values after the change are the current values
// with the change laid over them.
func (s *Service) checkEditForm(ctx context.Context, a Actor, is Issue, role, pid string, ch Changes) error {
	var typeID string
	s.DB.QueryRow(ctx, `SELECT id::text FROM issue_type WHERE lower(name)=lower($1)`, is.Type).Scan(&typeID)
	fc, err := s.loadForm(ctx, s.DB, pid, typeID, "edit")
	if err != nil {
		return err
	}
	if fc.OnScreen == nil && len(fc.Cfg) == 0 && len(fc.Rules) == 0 {
		return nil
	}
	cfgs, err := s.fieldsFor(ctx, s.DB, pid, typeID)
	if err != nil {
		return err
	}
	names, req, byName := map[string]string{}, map[string]bool{}, map[string]string{}
	for _, c := range cfgs {
		names[c.Key], req[c.Key] = c.Name, c.Required
		byName[strings.ToLower(c.Name)] = c.Key
	}
	vals := map[string]any{"description": nonEmpty(is.Description), "priority": nonEmpty(is.Priority)}
	if is.Assignee != nil {
		vals["assignee"] = is.Assignee.ID
	}
	if len(is.Labels) > 0 {
		vals["labels"] = is.Labels
	}
	if cur, err := s.CustomValues(ctx, Actor{Admin: true}, is.ID, pid, typeID, "admin"); err == nil {
		for k, v := range cur {
			vals[k] = v
		}
	}
	touched := map[string]any{}
	if ch.Priority != nil {
		touched["priority"], vals["priority"] = nonEmpty(*ch.Priority), nonEmpty(*ch.Priority)
	}
	if ch.Assignee != nil {
		touched["assignee"], vals["assignee"] = nonEmpty(*ch.Assignee), nonEmpty(*ch.Assignee)
	}
	if ch.Labels != nil {
		var l any
		if len(*ch.Labels) > 0 {
			l = *ch.Labels
		}
		touched["labels"], vals["labels"] = l, l
	}
	for k, v := range ch.Custom {
		key := strings.ToLower(k)
		if n, ok := byName[key]; ok {
			key = n
		}
		if isEmptyValue(v) {
			v = nil
		}
		touched[key], vals[key] = v, v
	}
	return s.checkForm(fc, "edit", role, names, req, vals, touched)
}
