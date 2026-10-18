package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Custom fields (SPEC 8). A field has a type, optional validation, and one or more contexts; a context says for which
// projects and issue types the field applies, with its own options, default and required flag. Values are stored typed.

var fieldTypes = map[string]bool{
	"text": true, "textarea": true, "number": true, "integer": true, "float": true, "date": true, "datetime": true,
	"select": true, "radio": true, "multi_select": true, "checkbox_group": true, "user": true, "multi_user": true,
	"url": true, "labels": true, "duration": true, "rating": true, "issue_picker": true,
}

func isOptionType(t string) bool {
	return t == "select" || t == "radio" || t == "multi_select" || t == "checkbox_group"
}
func isMulti(t string) bool {
	return t == "multi_select" || t == "checkbox_group" || t == "multi_user" || t == "labels"
}

const (
	maxFieldsPerInstance = 500
	maxOptionsPerContext = 1000
)

type FieldDef struct {
	ID           int64    `json:"id"`
	Key          string   `json:"key"` // customfield_<id>
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	HelpText     string   `json:"help_text"`
	Type         string   `json:"type"`
	Unit         string   `json:"unit,omitempty"`
	Searchable   bool     `json:"searchable"`
	ShowOnCard   bool     `json:"show_on_card"`
	Unique       bool     `json:"unique"`
	Min          *float64 `json:"min,omitempty"`
	Max          *float64 `json:"max,omitempty"`
	Regex        string   `json:"regex,omitempty"`
	CopyOnClone  bool     `json:"copy_on_clone"`
	TrackHistory bool     `json:"track_history"`
	ViewRoles    []string `json:"view_roles"`
	EditRoles    []string `json:"edit_roles"`
	Trashed      bool     `json:"trashed,omitempty"`
}

type FieldOption struct {
	ID       int64  `json:"id"`
	Value    string `json:"value"`
	Color    string `json:"color,omitempty"`
	Position int    `json:"position"`
	Archived bool   `json:"archived,omitempty"`
}

type FieldContext struct {
	ID       int64         `json:"id"`
	FieldID  int64         `json:"field_id"`
	Name     string        `json:"name"`
	Global   bool          `json:"global"`
	Projects []string      `json:"projects"`
	Types    []string      `json:"types"`
	Required bool          `json:"required"`
	Default  any           `json:"default,omitempty"`
	Position int           `json:"position"`
	Options  []FieldOption `json:"options,omitempty"`
}

// FieldConfig is a field as it applies to one project and issue type: what a form needs.
type FieldConfig struct {
	FieldDef
	Required bool          `json:"required"`
	Default  any           `json:"default,omitempty"`
	Options  []FieldOption `json:"options,omitempty"`
	Context  int64         `json:"context"`
}

func fieldKey(id int64) string { return "customfield_" + strconv.FormatInt(id, 10) }

var fieldKeyRe = regexp.MustCompile(`^(?:customfield_|cf\[)(\d+)\]?$`)

func parseFieldKey(k string) (int64, bool) {
	m := fieldKeyRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(k)))
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil
}

type FieldIn struct {
	Name         string   `json:"name"`
	Description  *string  `json:"description"`
	HelpText     *string  `json:"help_text"`
	Type         string   `json:"type"`
	Unit         *string  `json:"unit"`
	Searchable   *bool    `json:"searchable"`
	ShowOnCard   *bool    `json:"show_on_card"`
	Unique       *bool    `json:"unique"`
	Min          *float64 `json:"min"`
	Max          *float64 `json:"max"`
	Regex        *string  `json:"regex"`
	CopyOnClone  *bool    `json:"copy_on_clone"`
	TrackHistory *bool    `json:"track_history"`
	ViewRoles    []string `json:"view_roles"`
	EditRoles    []string `json:"edit_roles"`
}

func validRoles(rs []string) error {
	for _, r := range rs {
		if r != "viewer" && r != "member" && r != "admin" {
			return invalid("roles are viewer, member or admin (got %q)", r)
		}
	}
	return nil
}

const fieldCols = `id,name,description,help_text,type,unit,searchable,show_on_card,unique_values,min_value,max_value,regex,copy_on_clone,track_history,view_roles,edit_roles,trashed_at IS NOT NULL`

func scanField(row pgx.Row) (FieldDef, error) {
	var f FieldDef
	var min, max *float64
	err := row.Scan(&f.ID, &f.Name, &f.Description, &f.HelpText, &f.Type, &f.Unit, &f.Searchable, &f.ShowOnCard, &f.Unique, &min, &max, &f.Regex, &f.CopyOnClone, &f.TrackHistory, &f.ViewRoles, &f.EditRoles, &f.Trashed)
	if err == pgx.ErrNoRows {
		return f, ErrNotFound
	}
	f.Key, f.Min, f.Max = fieldKey(f.ID), min, max
	if f.ViewRoles == nil {
		f.ViewRoles = []string{}
	}
	if f.EditRoles == nil {
		f.EditRoles = []string{}
	}
	return f, err
}

// CreateField defines a custom field (administrators). A field with no context applies nowhere until one is added.
func (s *Service) CreateField(ctx context.Context, a Actor, in FieldIn) (FieldDef, error) {
	if err := adminOnly(a); err != nil {
		return FieldDef{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return FieldDef{}, invalid("a field needs a name of up to 100 characters")
	}
	if !fieldTypes[in.Type] {
		return FieldDef{}, invalid("unknown field type %q", in.Type)
	}
	if err := validRoles(in.ViewRoles); err != nil {
		return FieldDef{}, err
	}
	if err := validRoles(in.EditRoles); err != nil {
		return FieldDef{}, err
	}
	if in.Regex != nil && *in.Regex != "" {
		if _, err := regexp.Compile(*in.Regex); err != nil {
			return FieldDef{}, invalid("the validation pattern is not a valid regular expression: %v", err)
		}
	}
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM custom_field WHERE trashed_at IS NULL`).Scan(&n)
	if n >= maxFieldsPerInstance {
		return FieldDef{}, invalid("this instance has the maximum of %d custom fields", maxFieldsPerInstance)
	}
	b := func(p *bool, d bool) bool {
		if p == nil {
			return d
		}
		return *p
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	row := s.DB.QueryRow(ctx, `INSERT INTO custom_field(name,description,help_text,type,unit,searchable,show_on_card,unique_values,min_value,max_value,regex,copy_on_clone,track_history,view_roles,edit_roles)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING `+fieldCols,
		in.Name, str(in.Description), str(in.HelpText), in.Type, str(in.Unit), b(in.Searchable, true), b(in.ShowOnCard, false), b(in.Unique, false), in.Min, in.Max, str(in.Regex), b(in.CopyOnClone, false), b(in.TrackHistory, true),
		orEmpty(in.ViewRoles), orEmpty(in.EditRoles))
	f, err := scanField(row)
	if err != nil && strings.Contains(err.Error(), "custom_field_name_live") {
		return f, invalid("a custom field named %q already exists", in.Name)
	}
	return f, err
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// UpdateField changes a field's settings; the type never changes (replace the field instead).
func (s *Service) UpdateField(ctx context.Context, a Actor, id int64, in FieldIn) (FieldDef, error) {
	if err := adminOnly(a); err != nil {
		return FieldDef{}, err
	}
	f, err := s.GetField(ctx, id)
	if err != nil {
		return f, err
	}
	if in.Type != "" && in.Type != f.Type {
		return f, invalid("a field's type cannot change; create a new field and replace this one")
	}
	if in.Name != "" {
		f.Name = strings.TrimSpace(in.Name)
		if len(f.Name) > 100 {
			return f, invalid("a field name is up to 100 characters")
		}
	}
	if in.Description != nil {
		f.Description = *in.Description
	}
	if in.HelpText != nil {
		f.HelpText = *in.HelpText
	}
	if in.Unit != nil {
		f.Unit = *in.Unit
	}
	if in.Searchable != nil {
		f.Searchable = *in.Searchable
	}
	if in.ShowOnCard != nil {
		f.ShowOnCard = *in.ShowOnCard
	}
	if in.Unique != nil {
		f.Unique = *in.Unique
	}
	if in.Min != nil {
		f.Min = in.Min
	}
	if in.Max != nil {
		f.Max = in.Max
	}
	if in.Regex != nil {
		if *in.Regex != "" {
			if _, err := regexp.Compile(*in.Regex); err != nil {
				return f, invalid("the validation pattern is not a valid regular expression: %v", err)
			}
		}
		f.Regex = *in.Regex
	}
	if in.CopyOnClone != nil {
		f.CopyOnClone = *in.CopyOnClone
	}
	if in.TrackHistory != nil {
		f.TrackHistory = *in.TrackHistory
	}
	if in.ViewRoles != nil {
		if err := validRoles(in.ViewRoles); err != nil {
			return f, err
		}
		f.ViewRoles = in.ViewRoles
	}
	if in.EditRoles != nil {
		if err := validRoles(in.EditRoles); err != nil {
			return f, err
		}
		f.EditRoles = in.EditRoles
	}
	_, err = s.DB.Exec(ctx, `UPDATE custom_field SET name=$2,description=$3,help_text=$4,unit=$5,searchable=$6,show_on_card=$7,unique_values=$8,min_value=$9,max_value=$10,regex=$11,copy_on_clone=$12,track_history=$13,view_roles=$14,edit_roles=$15 WHERE id=$1`,
		id, f.Name, f.Description, f.HelpText, f.Unit, f.Searchable, f.ShowOnCard, f.Unique, f.Min, f.Max, f.Regex, f.CopyOnClone, f.TrackHistory, f.ViewRoles, f.EditRoles)
	if err != nil && strings.Contains(err.Error(), "custom_field_name_live") {
		return f, invalid("a custom field named %q already exists", f.Name)
	}
	return f, err
}

func (s *Service) GetField(ctx context.Context, id int64) (FieldDef, error) {
	return scanField(s.DB.QueryRow(ctx, `SELECT `+fieldCols+` FROM custom_field WHERE id=$1`, id))
}

// ListFields returns every field (trashed ones too when asked), with their contexts for administrators.
func (s *Service) ListFields(ctx context.Context, trashed bool) ([]FieldDef, error) {
	q := `SELECT ` + fieldCols + ` FROM custom_field`
	if !trashed {
		q += ` WHERE trashed_at IS NULL`
	}
	rows, err := s.DB.Query(ctx, q+` ORDER BY lower(name),id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FieldDef{}
	for rows.Next() {
		f, err := scanField(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// TrashField hides a field (and its values from every form and search) for the retention period; RestoreField undoes it.
func (s *Service) TrashField(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `UPDATE custom_field SET trashed_at=$2 WHERE id=$1 AND trashed_at IS NULL`, id, s.Now())
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Service) RestoreField(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	var days = 30
	var at *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT trashed_at FROM custom_field WHERE id=$1`, id).Scan(&at); err != nil || at == nil {
		return ErrNotFound
	}
	if s.Now().Sub(*at) > time.Duration(days)*24*time.Hour {
		return invalid("the retention period has passed")
	}
	_, err := s.DB.Exec(ctx, `UPDATE custom_field SET trashed_at=NULL WHERE id=$1`, id)
	if err != nil && strings.Contains(err.Error(), "custom_field_name_live") {
		return invalid("another field now has this name: rename one of them first")
	}
	return err
}

type ContextIn struct {
	Name     string   `json:"name"`
	Global   bool     `json:"global"`
	Projects []string `json:"projects"`
	Types    []string `json:"types"`
	Required bool     `json:"required"`
	Default  any      `json:"default"`
	Options  []string `json:"options"`
}

// AddContext adds a context to a field: the projects and issue types it applies to, its options, default and required flag.
func (s *Service) AddContext(ctx context.Context, a Actor, fieldID int64, in ContextIn) (FieldContext, error) {
	if err := adminOnly(a); err != nil {
		return FieldContext{}, err
	}
	f, err := s.GetField(ctx, fieldID)
	if err != nil {
		return FieldContext{}, err
	}
	if !in.Global && len(in.Projects) == 0 {
		return FieldContext{}, invalid("a context applies to every project (global) or to a list of projects")
	}
	if len(in.Options) > maxOptionsPerContext {
		return FieldContext{}, invalid("a context has at most %d options", maxOptionsPerContext)
	}
	if len(in.Options) > 0 && !isOptionType(f.Type) {
		return FieldContext{}, invalid("only select, radio, multi select and checkbox fields have options")
	}
	out := FieldContext{FieldID: fieldID, Name: in.Name, Global: in.Global, Projects: []string{}, Types: []string{}, Required: in.Required}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var pos int
		tx.QueryRow(ctx, `SELECT COALESCE(max(position)+1,0) FROM custom_field_context WHERE field_id=$1`, fieldID).Scan(&pos)
		out.Position = pos
		if err := tx.QueryRow(ctx, `INSERT INTO custom_field_context(field_id,name,is_global,required,position) VALUES($1,$2,$3,$4,$5) RETURNING id`, fieldID, in.Name, in.Global, in.Required, pos).Scan(&out.ID); err != nil {
			return err
		}
		for _, k := range in.Projects {
			k = strings.ToUpper(strings.TrimSpace(k))
			tag, err := tx.Exec(ctx, `INSERT INTO custom_field_context_project(context_id,project_id) SELECT $1,id FROM project WHERE key=$2 ON CONFLICT DO NOTHING`, out.ID, k)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return invalid("there is no project %q", k)
			}
			out.Projects = append(out.Projects, k)
		}
		for _, tname := range in.Types {
			tag, err := tx.Exec(ctx, `INSERT INTO custom_field_context_type(context_id,issue_type_id) SELECT $1,id FROM issue_type WHERE lower(name)=lower($2) ON CONFLICT DO NOTHING`, out.ID, strings.TrimSpace(tname))
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return invalid("there is no issue type %q", tname)
			}
			out.Types = append(out.Types, tname)
		}
		for i, o := range in.Options {
			o = strings.TrimSpace(o)
			if o == "" || len(o) > 200 {
				return invalid("an option is 1 to 200 characters")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO custom_field_option(context_id,value,position) VALUES($1,$2,$3)`, out.ID, o, i); err != nil {
				if strings.Contains(err.Error(), "custom_field_option_context_id_value_key") {
					return invalid("the option %q is listed twice", o)
				}
				return err
			}
		}
		if in.Default != nil {
			// a default must be valid for the field as the context will define it
			opts, _ := s.contextOptions(ctx, tx, out.ID)
			v, err := s.normalize(ctx, tx, f, opts, in.Default, "")
			if err != nil {
				return invalid("the default value: %v", err)
			}
			raw, _ := json.Marshal(v)
			if _, err := tx.Exec(ctx, `UPDATE custom_field_context SET default_value=$2 WHERE id=$1`, out.ID, raw); err != nil {
				return err
			}
			out.Default = v
		}
		return nil
	})
	return out, err
}

func (s *Service) contextOptions(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, contextID int64) ([]FieldOption, error) {
	rows, err := q.Query(ctx, `SELECT id,value,color,position,archived FROM custom_field_option WHERE context_id=$1 ORDER BY position,id`, contextID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FieldOption
	for rows.Next() {
		var o FieldOption
		rows.Scan(&o.ID, &o.Value, &o.Color, &o.Position, &o.Archived)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Service) RemoveContext(ctx context.Context, a Actor, fieldID, contextID int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM custom_field_context WHERE id=$1 AND field_id=$2`, contextID, fieldID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ListContexts returns a field's contexts with their options.
func (s *Service) ListContexts(ctx context.Context, fieldID int64) ([]FieldContext, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,is_global,required,default_value,position FROM custom_field_context WHERE field_id=$1 ORDER BY position,id`, fieldID)
	if err != nil {
		return nil, err
	}
	var out []FieldContext
	for rows.Next() {
		c := FieldContext{FieldID: fieldID, Projects: []string{}, Types: []string{}}
		var def []byte
		rows.Scan(&c.ID, &c.Name, &c.Global, &c.Required, &def, &c.Position)
		if len(def) > 0 {
			json.Unmarshal(def, &c.Default)
		}
		out = append(out, c)
	}
	rows.Close()
	for i := range out {
		pr, _ := s.DB.Query(ctx, `SELECT p.key FROM custom_field_context_project cp JOIN project p ON p.id=cp.project_id WHERE cp.context_id=$1 ORDER BY p.key`, out[i].ID)
		for pr.Next() {
			var k string
			pr.Scan(&k)
			out[i].Projects = append(out[i].Projects, k)
		}
		pr.Close()
		tr, _ := s.DB.Query(ctx, `SELECT t.name FROM custom_field_context_type ct JOIN issue_type t ON t.id=ct.issue_type_id WHERE ct.context_id=$1 ORDER BY t.name`, out[i].ID)
		for tr.Next() {
			var k string
			tr.Scan(&k)
			out[i].Types = append(out[i].Types, k)
		}
		tr.Close()
		out[i].Options, _ = s.contextOptions(ctx, s.DB, out[i].ID)
	}
	return out, nil
}

// AddOption, ArchiveOption and MergeOptions manage a context's options.
func (s *Service) AddOption(ctx context.Context, a Actor, fieldID, contextID int64, value, color string) (FieldOption, error) {
	if err := adminOnly(a); err != nil {
		return FieldOption{}, err
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 200 {
		return FieldOption{}, invalid("an option is 1 to 200 characters")
	}
	var o FieldOption
	err := s.DB.QueryRow(ctx, `INSERT INTO custom_field_option(context_id,value,color,position) SELECT c.id,$3,$4,COALESCE((SELECT max(position)+1 FROM custom_field_option WHERE context_id=c.id),0)
		FROM custom_field_context c JOIN custom_field f ON f.id=c.field_id WHERE c.id=$1 AND c.field_id=$2 AND f.type IN ('select','radio','multi_select','checkbox_group') RETURNING id,value,color,position,archived`,
		contextID, fieldID, value, color).Scan(&o.ID, &o.Value, &o.Color, &o.Position, &o.Archived)
	if err == pgx.ErrNoRows {
		return o, invalid("that context does not exist, or its field has no options")
	}
	if err != nil && strings.Contains(err.Error(), "custom_field_option_context_id_value_key") {
		return o, invalid("the option %q already exists", value)
	}
	return o, err
}

func (s *Service) ArchiveOption(ctx context.Context, a Actor, optionID int64, archived bool) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `UPDATE custom_field_option SET archived=$2 WHERE id=$1`, optionID, archived)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// MergeOptions rewrites every issue's value from one option to another, then removes the first.
func (s *Service) MergeOptions(ctx context.Context, a Actor, fieldID int64, from, into string) (int, error) {
	if err := adminOnly(a); err != nil {
		return 0, err
	}
	f, err := s.GetField(ctx, fieldID)
	if err != nil {
		return 0, err
	}
	if !isOptionType(f.Type) || strings.EqualFold(from, into) {
		return 0, invalid("merge two different options of a select, radio, multi select or checkbox field")
	}
	n := 0
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var ok int
		tx.QueryRow(ctx, `SELECT count(*) FROM custom_field_option o JOIN custom_field_context c ON c.id=o.context_id WHERE c.field_id=$1 AND o.value=$2`, fieldID, into).Scan(&ok)
		if ok == 0 {
			return invalid("the option %q does not exist", into)
		}
		rows, err := tx.Query(ctx, `SELECT issue_id::text,value FROM issue_field_value WHERE field_id=$1`, fieldID)
		if err != nil {
			return err
		}
		type rv struct {
			id  string
			raw []byte
		}
		var all []rv
		for rows.Next() {
			var x rv
			rows.Scan(&x.id, &x.raw)
			all = append(all, x)
		}
		rows.Close()
		for _, x := range all {
			var v any
			json.Unmarshal(x.raw, &v)
			changed := false
			switch t := v.(type) {
			case string:
				if t == from {
					v, changed = into, true
				}
			case []any:
				seen := map[string]bool{}
				var out []any
				for _, e := range t {
					es, _ := e.(string)
					if es == from {
						es, changed = into, true
					}
					if !seen[es] {
						seen[es] = true
						out = append(out, es)
					}
				}
				v = out
			}
			if changed {
				if err := s.storeValue(ctx, tx, x.id, f, v); err != nil {
					return err
				}
				n++
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM custom_field_option o USING custom_field_context c WHERE c.id=o.context_id AND c.field_id=$1 AND o.value=$2`, fieldID, from)
		return err
	})
	return n, err
}

// ReplaceField moves every value from one field to another of the same type (a bulk migration), then trashes the first.
func (s *Service) ReplaceField(ctx context.Context, a Actor, from, to int64) (int, error) {
	if err := adminOnly(a); err != nil {
		return 0, err
	}
	ff, err := s.GetField(ctx, from)
	if err != nil {
		return 0, err
	}
	tf, err := s.GetField(ctx, to)
	if err != nil {
		return 0, err
	}
	if ff.Type != tf.Type || from == to {
		return 0, invalid("a field can be replaced only by a different field of the same type")
	}
	n := 0
	err = s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO issue_field_value(issue_id,field_id,value,text_value,num_value,date_value,updated_at)
			SELECT issue_id,$2,value,text_value,num_value,date_value,$3 FROM issue_field_value WHERE field_id=$1 ON CONFLICT (issue_id,field_id) DO NOTHING`, from, to, s.Now())
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		if _, err := tx.Exec(ctx, `DELETE FROM issue_field_value WHERE field_id=$1`, from); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE custom_field SET trashed_at=$2 WHERE id=$1`, from, s.Now())
		return err
	})
	return n, err
}

// FieldWarnings lists fields that are unused or apply nowhere.
func (s *Service) FieldWarnings(ctx context.Context, a Actor) ([]string, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT f.name,
		NOT EXISTS (SELECT 1 FROM custom_field_context c WHERE c.field_id=f.id) AS nocontext,
		NOT EXISTS (SELECT 1 FROM issue_field_value v WHERE v.field_id=f.id) AS unused
		FROM custom_field f WHERE f.trashed_at IS NULL ORDER BY f.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		var nc, un bool
		rows.Scan(&n, &nc, &un)
		if nc {
			out = append(out, fmt.Sprintf("%s has no context, so it applies to no issue", n))
		} else if un {
			out = append(out, fmt.Sprintf("%s is not used by any issue", n))
		}
	}
	return out, rows.Err()
}

// ---------- resolution ----------

// FieldsFor returns the fields that apply to a project and issue type, each with the settings of the most specific
// matching context (a project-specific context wins over a global one, then the lowest position).
func (s *Service) FieldsFor(ctx context.Context, projectID, typeID string) ([]FieldConfig, error) {
	return s.fieldsFor(ctx, s.DB, projectID, typeID)
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Service) fieldsFor(ctx context.Context, q querier, projectID, typeID string) ([]FieldConfig, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (f.id) f.id,c.id,c.required,c.default_value
		FROM custom_field f JOIN custom_field_context c ON c.field_id=f.id
		WHERE f.trashed_at IS NULL
		  AND (c.is_global OR EXISTS (SELECT 1 FROM custom_field_context_project cp WHERE cp.context_id=c.id AND cp.project_id::text=$1))
		  AND (NOT EXISTS (SELECT 1 FROM custom_field_context_type ct WHERE ct.context_id=c.id) OR EXISTS (SELECT 1 FROM custom_field_context_type ct WHERE ct.context_id=c.id AND ct.issue_type_id::text=$2))
		ORDER BY f.id, (NOT c.is_global) DESC, (EXISTS (SELECT 1 FROM custom_field_context_type ct WHERE ct.context_id=c.id)) DESC, c.position, c.id`, projectID, typeID)
	if err != nil {
		return nil, err
	}
	type pick struct {
		fid, cid int64
		req      bool
		def      []byte
	}
	var picks []pick
	for rows.Next() {
		var p pick
		rows.Scan(&p.fid, &p.cid, &p.req, &p.def)
		picks = append(picks, p)
	}
	rows.Close()
	out := []FieldConfig{}
	for _, p := range picks {
		f, err := scanField(q.QueryRow(ctx, `SELECT `+fieldCols+` FROM custom_field WHERE id=$1`, p.fid))
		if err != nil {
			return nil, err
		}
		c := FieldConfig{FieldDef: f, Required: p.req, Context: p.cid}
		if len(p.def) > 0 {
			json.Unmarshal(p.def, &c.Default)
		}
		if isOptionType(f.Type) {
			if c.Options, err = s.contextOptions(ctx, q, p.cid); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// ProjectFields is the form definition for creating or editing an issue of a type in a project.
func (s *Service) ProjectFields(ctx context.Context, a Actor, projectKey, typeName string) ([]FieldConfig, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	role := s.role(ctx, a, pid)
	if role == "" {
		return nil, ErrNotFound
	}
	var tid string
	if typeName == "" {
		typeName = "Task"
	}
	if err := s.DB.QueryRow(ctx, `SELECT id::text FROM issue_type WHERE lower(name)=lower($1)`, typeName).Scan(&tid); err != nil {
		return nil, invalid("unknown issue type %q", typeName)
	}
	all, err := s.FieldsFor(ctx, pid, tid)
	if err != nil {
		return nil, err
	}
	out := []FieldConfig{}
	for _, f := range all {
		if roleIn(f.ViewRoles, role, a.Admin) {
			out = append(out, f)
		}
	}
	return out, nil
}

// roleIn reports whether a field's role restriction lets this role through (empty means everyone).
func roleIn(roles []string, role string, admin bool) bool {
	if admin || len(roles) == 0 {
		return true
	}
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// ---------- values ----------

var durRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(w|d|h|m|s)`)

func parseDuration(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, true
	}
	ms := durRe.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 || len(strings.Join(strings.Fields(durRe.ReplaceAllString(s, "")), "")) > 0 {
		return 0, false
	}
	total := 0.0
	for _, m := range ms {
		n, _ := strconv.ParseFloat(m[1], 64)
		switch strings.ToLower(m[2]) {
		case "w":
			total += n * 5 * 8 * 3600 // a working week of five 8-hour days, as in Jira
		case "d":
			total += n * 8 * 3600
		case "h":
			total += n * 3600
		case "m":
			total += n * 60
		case "s":
			total += n
		}
	}
	return total, true
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

func toStrings(v any) ([]string, bool) {
	switch x := v.(type) {
	case []string:
		return x, true
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// normalize validates a value against a field and returns its canonical form. skipIssue excludes one issue from the
// uniqueness check (the issue being edited).
func (s *Service) normalize(ctx context.Context, q querier, f FieldDef, opts []FieldOption, raw any, skipIssue string) (any, error) {
	numRange := func(n float64) error {
		if f.Min != nil && n < *f.Min {
			return fmt.Errorf("must be at least %v", *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Errorf("must be at most %v", *f.Max)
		}
		return nil
	}
	var out any
	switch f.Type {
	case "text", "textarea", "url":
		str, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("must be text")
		}
		str = strings.TrimSpace(str)
		limit := 255
		if f.Type == "textarea" {
			limit = 32768
		}
		if len([]rune(str)) > limit {
			return nil, fmt.Errorf("is limited to %d characters", limit)
		}
		if f.Type != "textarea" && strings.ContainsAny(str, "\n\r") {
			return nil, fmt.Errorf("must be a single line")
		}
		if f.Type == "url" {
			u, err := url.Parse(str)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto") || (u.Host == "" && u.Scheme != "mailto") {
				return nil, fmt.Errorf("must be an http, https or mailto address")
			}
		}
		if f.Regex != "" {
			if re, err := regexp.Compile(f.Regex); err == nil && !re.MatchString(str) {
				return nil, fmt.Errorf("does not match the required pattern %s", f.Regex)
			}
		}
		out = str
	case "number", "float", "integer":
		n, ok := toFloat(raw)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		if f.Type == "integer" && n != math.Trunc(n) {
			return nil, fmt.Errorf("must be a whole number")
		}
		if err := numRange(n); err != nil {
			return nil, err
		}
		out = n
	case "duration":
		var secs float64
		var ok bool
		if str, isStr := raw.(string); isStr {
			secs, ok = parseDuration(str)
		} else {
			secs, ok = toFloat(raw)
		}
		if !ok || secs < 0 {
			return nil, fmt.Errorf("must be a duration such as 90m, 2h 30m or 1d (seconds are accepted too)")
		}
		if err := numRange(secs); err != nil {
			return nil, err
		}
		out = math.Round(secs)
	case "rating":
		n, ok := toFloat(raw)
		if !ok || n != math.Trunc(n) || n < 0 || n > 5 {
			return nil, fmt.Errorf("must be a whole number from 0 to 5")
		}
		out = n
	case "date":
		str, _ := raw.(string)
		if _, err := time.Parse("2006-01-02", str); err != nil {
			return nil, fmt.Errorf("must be a date such as 2026-10-19")
		}
		out = str
	case "datetime":
		str, _ := raw.(string)
		t, err := time.Parse(time.RFC3339, str)
		if err != nil {
			return nil, fmt.Errorf("must be a date and time such as 2026-10-19T09:30:00Z")
		}
		out = t.UTC().Format(time.RFC3339)
	case "select", "radio":
		str, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("must be one of the options")
		}
		o, ok := findOption(opts, str)
		if !ok {
			return nil, fmt.Errorf("%q is not one of the options", str)
		}
		out = o
	case "multi_select", "checkbox_group":
		list, ok := toStrings(raw)
		if !ok {
			return nil, fmt.Errorf("must be a list of options")
		}
		seen := map[string]bool{}
		res := []string{}
		for _, e := range list {
			o, ok := findOption(opts, e)
			if !ok {
				return nil, fmt.Errorf("%q is not one of the options", e)
			}
			if !seen[o] {
				seen[o] = true
				res = append(res, o)
			}
		}
		out = res
	case "labels":
		list, ok := toStrings(raw)
		if !ok {
			return nil, fmt.Errorf("must be a list of labels")
		}
		l, err := normLabels(list)
		if err != nil {
			return nil, err
		}
		out = l
	case "user", "multi_user":
		var list []string
		if f.Type == "user" {
			str, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("must be a user")
			}
			list = []string{str}
		} else {
			var ok bool
			if list, ok = toStrings(raw); !ok {
				return nil, fmt.Errorf("must be a list of users")
			}
		}
		ids := []string{}
		for _, ref := range list {
			var id string
			if err := q.QueryRow(ctx, `SELECT id::text FROM app_user WHERE active AND (id::text=$1 OR lower(email)=lower($1))`, strings.TrimSpace(ref)).Scan(&id); err != nil {
				return nil, fmt.Errorf("%q is not an active user", ref)
			}
			ids = append(ids, id)
		}
		if f.Type == "user" {
			out = ids[0]
		} else {
			out = ids
		}
	case "issue_picker":
		str, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("must be an issue key")
		}
		var key string
		if err := q.QueryRow(ctx, `SELECT i.key FROM issue i WHERE i.deleted_at IS NULL AND (i.key=upper($1) OR i.id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=upper($1)))`, strings.TrimSpace(str)).Scan(&key); err != nil {
			return nil, fmt.Errorf("%q is not an issue", str)
		}
		out = key
	default:
		return nil, fmt.Errorf("unknown field type %q", f.Type)
	}
	if f.Unique {
		if tv := textOf(f, out); tv != "" {
			var n int
			q.QueryRow(ctx, `SELECT count(*) FROM issue_field_value WHERE field_id=$1 AND text_value=$2 AND issue_id::text<>$3`, f.ID, tv, skipIssue).Scan(&n)
			if n > 0 {
				return nil, fmt.Errorf("must be unique: another issue already has this value")
			}
		}
	}
	return out, nil
}

func findOption(opts []FieldOption, v string) (string, bool) {
	for _, o := range opts {
		if !o.Archived && strings.EqualFold(o.Value, strings.TrimSpace(v)) {
			return o.Value, true
		}
	}
	return "", false
}

// textOf is the lower-cased text a value is searched and sorted by.
func textOf(f FieldDef, v any) string {
	switch x := v.(type) {
	case string:
		return strings.ToLower(x)
	case []string:
		return strings.ToLower(strings.Join(x, " "))
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.ToLower(strings.Join(parts, " "))
	}
	return ""
}

// storeValue writes a canonical value with its typed search columns; nil removes it.
func (s *Service) storeValue(ctx context.Context, tx pgx.Tx, issueID string, f FieldDef, v any) error {
	if v == nil {
		_, err := tx.Exec(ctx, `DELETE FROM issue_field_value WHERE issue_id::text=$1 AND field_id=$2`, issueID, f.ID)
		return err
	}
	raw, _ := json.Marshal(v)
	var num any
	var date any
	switch f.Type {
	case "number", "float", "integer", "duration", "rating":
		if n, ok := toFloat(v); ok {
			num = n
		}
	case "date":
		if t, err := time.Parse("2006-01-02", fmt.Sprint(v)); err == nil {
			date = t
		}
	case "datetime":
		if t, err := time.Parse(time.RFC3339, fmt.Sprint(v)); err == nil {
			date = t
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO issue_field_value(issue_id,field_id,value,text_value,num_value,date_value,updated_at) VALUES($1::uuid,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (issue_id,field_id) DO UPDATE SET value=EXCLUDED.value,text_value=EXCLUDED.text_value,num_value=EXCLUDED.num_value,date_value=EXCLUDED.date_value,updated_at=EXCLUDED.updated_at`,
		issueID, f.ID, raw, textOf(f, v), num, date, s.Now())
	return err
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	}
	return false
}

// prepareCreate validates the custom values for a new issue of a type in a project: defaults fill what is missing,
// required fields must have a value, and a value for a field that does not apply is refused.
func (s *Service) prepareCreate(ctx context.Context, tx pgx.Tx, projectID, typeID string, role string, admin bool, provided map[string]any) (map[int64]any, []FieldConfig, error) {
	cfg, err := s.fieldsFor(ctx, tx, projectID, typeID)
	if err != nil {
		return nil, nil, err
	}
	byKey := map[string]FieldConfig{}
	for _, c := range cfg {
		byKey[c.Key] = c
		byKey[strings.ToLower(c.Name)] = c
	}
	for k := range provided {
		if _, ok := byKey[strings.ToLower(k)]; !ok {
			return nil, nil, invalid("the field %q does not apply to this project and issue type", k)
		}
	}
	vals := map[int64]any{}
	for _, c := range cfg {
		raw, given := provided[c.Key]
		if !given {
			raw, given = provided[c.Name]
		}
		canEdit := roleIn(c.EditRoles, role, admin)
		switch {
		case given && !isEmptyValue(raw):
			if !canEdit {
				return nil, nil, invalid("%s: your role may not set this field", c.Name)
			}
			v, err := s.normalize(ctx, tx, c.FieldDef, c.Options, raw, "")
			if err != nil {
				return nil, nil, invalid("%s: %v", c.Name, err)
			}
			vals[c.ID] = v
		case c.Default != nil:
			v, err := s.normalize(ctx, tx, c.FieldDef, c.Options, c.Default, "")
			if err == nil {
				vals[c.ID] = v
			} else if c.Required {
				return nil, nil, invalid("%s: the default value is no longer valid: %v", c.Name, err)
			}
		case c.Required:
			return nil, nil, invalid("%s is required", c.Name)
		}
	}
	return vals, cfg, nil
}

// CustomValues returns an issue's custom field values the caller may see, by field key.
func (s *Service) CustomValues(ctx context.Context, a Actor, issueID, projectID, typeID, role string) (map[string]any, error) {
	rows, err := s.DB.Query(ctx, `SELECT f.id,f.view_roles,v.value FROM issue_field_value v JOIN custom_field f ON f.id=v.field_id WHERE v.issue_id::text=$1 AND f.trashed_at IS NULL`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]any{}
	for rows.Next() {
		var id int64
		var vr []string
		var raw []byte
		rows.Scan(&id, &vr, &raw)
		if !roleIn(vr, role, a.Admin) {
			continue
		}
		var v any
		json.Unmarshal(raw, &v)
		out[fieldKey(id)] = v
	}
	return out, rows.Err()
}
