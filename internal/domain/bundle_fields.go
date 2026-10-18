package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Custom fields in bundles (SPEC 18.2): field definitions with their contexts and options, and each issue's values,
// both carried by field name so they match across instances. People in user fields travel as email addresses.

type BContext struct {
	Name     string   `json:"name,omitempty"`
	Global   bool     `json:"global"`
	Projects []string `json:"projects,omitempty"`
	Types    []string `json:"types,omitempty"`
	Required bool     `json:"required,omitempty"`
	Default  any      `json:"default,omitempty"`
	Options  []string `json:"options,omitempty"`
}

type BField struct {
	Name         string     `json:"name"`
	Description  string     `json:"description,omitempty"`
	HelpText     string     `json:"help_text,omitempty"`
	Type         string     `json:"type"`
	Unit         string     `json:"unit,omitempty"`
	Searchable   bool       `json:"searchable"`
	ShowOnCard   bool       `json:"show_on_card,omitempty"`
	Unique       bool       `json:"unique,omitempty"`
	Min          *float64   `json:"min,omitempty"`
	Max          *float64   `json:"max,omitempty"`
	Regex        string     `json:"regex,omitempty"`
	CopyOnClone  bool       `json:"copy_on_clone,omitempty"`
	TrackHistory bool       `json:"track_history"`
	ViewRoles    []string   `json:"view_roles,omitempty"`
	EditRoles    []string   `json:"edit_roles,omitempty"`
	Contexts     []BContext `json:"contexts"`
}

// exportFields adds the field definitions to the bundle and each issue's values (by name). Administrators export every
// definition; anyone else gets the definitions of the fields whose values they export. A field restricted to roles is
// exported only to people in those roles. roles maps project key to the exporter's role there.
func (s *Service) exportFields(ctx context.Context, tx pgx.Tx, a Actor, d *BundleData, projectRoles map[string]string, trashCond string, userEmail func(id *string) string) error {
	idx := map[string]int{}
	for i, is := range d.Issues {
		idx[is.Key] = i
	}
	if len(idx) == 0 && !a.Admin {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT i.key,p.key,f.name,f.type,f.view_roles,v.value FROM issue_field_value v JOIN issue i ON i.id=v.issue_id JOIN project p ON p.id=i.project_id
		JOIN custom_field f ON f.id=v.field_id WHERE i.key = ANY($1) AND f.trashed_at IS NULL`+strings.ReplaceAll(trashCond, "i.deleted_at IS NULL", "i.deleted_at IS NULL"), keysOf(idx))
	if err != nil {
		return err
	}
	used := map[string]bool{}
	type val struct {
		key, pkey, name, typ string
		view                 []string
		raw                  []byte
	}
	var vals []val
	for rows.Next() {
		var x val
		if err := rows.Scan(&x.key, &x.pkey, &x.name, &x.typ, &x.view, &x.raw); err != nil {
			rows.Close()
			return err
		}
		vals = append(vals, x)
	}
	rows.Close() // the connection is free again for the user lookups below
	for _, x := range vals {
		if !roleIn(x.view, projectRoles[x.pkey], a.Admin) {
			continue
		}
		var v any
		json.Unmarshal(x.raw, &v)
		if x.typ == "user" || x.typ == "multi_user" {
			conv := func(y any) any {
				id, _ := y.(string)
				if em := userEmail(&id); em != "" {
					return em
				}
				return y
			}
			if l, ok := v.([]any); ok {
				for i := range l {
					l[i] = conv(l[i])
				}
			} else {
				v = conv(v)
			}
		}
		i := idx[x.key]
		if d.Issues[i].Custom == nil {
			d.Issues[i].Custom = map[string]any{}
		}
		d.Issues[i].Custom[x.name] = v
		used[x.name] = true
	}
	fr, err := tx.Query(ctx, `SELECT id,name,description,help_text,type,unit,searchable,show_on_card,unique_values,min_value,max_value,regex,copy_on_clone,track_history,view_roles,edit_roles FROM custom_field WHERE trashed_at IS NULL ORDER BY lower(name),id`)
	if err != nil {
		return err
	}
	type fid struct {
		id int64
		f  BField
	}
	var fs []fid
	for fr.Next() {
		var x fid
		if err := fr.Scan(&x.id, &x.f.Name, &x.f.Description, &x.f.HelpText, &x.f.Type, &x.f.Unit, &x.f.Searchable, &x.f.ShowOnCard, &x.f.Unique, &x.f.Min, &x.f.Max, &x.f.Regex, &x.f.CopyOnClone, &x.f.TrackHistory, &x.f.ViewRoles, &x.f.EditRoles); err != nil {
			fr.Close()
			return err
		}
		fs = append(fs, x)
	}
	fr.Close()
	for _, x := range fs {
		if !a.Admin && !used[x.f.Name] {
			continue
		}
		cr, err := tx.Query(ctx, `SELECT id,name,is_global,required,default_value FROM custom_field_context WHERE field_id=$1 ORDER BY position,id`, x.id)
		if err != nil {
			return err
		}
		type cid struct {
			id int64
			c  BContext
			df []byte
		}
		var cs []cid
		for cr.Next() {
			var c cid
			if err := cr.Scan(&c.id, &c.c.Name, &c.c.Global, &c.c.Required, &c.df); err != nil {
				cr.Close()
				return err
			}
			if c.df != nil {
				json.Unmarshal(c.df, &c.c.Default)
			}
			cs = append(cs, c)
		}
		cr.Close()
		for _, c := range cs {
			c.c.Projects, c.c.Types, c.c.Options = []string{}, []string{}, []string{}
			pr, _ := tx.Query(ctx, `SELECT p.key FROM custom_field_context_project cp JOIN project p ON p.id=cp.project_id WHERE cp.context_id=$1 ORDER BY p.key`, c.id)
			for pr.Next() {
				var k string
				pr.Scan(&k)
				c.c.Projects = append(c.c.Projects, k)
			}
			pr.Close()
			tr, _ := tx.Query(ctx, `SELECT t.name FROM custom_field_context_type ct JOIN issue_type t ON t.id=ct.issue_type_id WHERE ct.context_id=$1 ORDER BY t.name`, c.id)
			for tr.Next() {
				var k string
				tr.Scan(&k)
				c.c.Types = append(c.c.Types, k)
			}
			tr.Close()
			or, _ := tx.Query(ctx, `SELECT value FROM custom_field_option WHERE context_id=$1 AND NOT archived ORDER BY position,id`, c.id)
			for or.Next() {
				var k string
				or.Scan(&k)
				c.c.Options = append(c.c.Options, k)
			}
			or.Close()
			x.f.Contexts = append(x.f.Contexts, c.c)
		}
		d.Fields = append(d.Fields, x.f)
	}
	return nil
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ----- CSV files

func writeFieldsCSV(bw *bundleWriter, d *BundleData) error {
	rows := [][]string{}
	for _, f := range d.Fields {
		b, _ := json.Marshal(f)
		rows = append(rows, []string{f.Name, f.Type, string(b)})
	}
	if err := bw.csvFile("fields.csv", []string{"name", "type", "definition"}, rows); err != nil {
		return err
	}
	rows = nil
	for _, is := range d.Issues {
		names := make([]string, 0, len(is.Custom))
		for n := range is.Custom {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			b, _ := json.Marshal(is.Custom[n])
			rows = append(rows, []string{is.Key, n, string(b)})
		}
	}
	return bw.csvFile("custom_values.csv", []string{"issue", "field", "value"}, rows)
}

func readFieldsCSV(files bundleFiles, d *BundleData) error {
	rows, err := readCSV(files, "fields.csv")
	if err != nil {
		return err
	}
	for _, r := range rows {
		if len(r) < 3 {
			continue
		}
		var f BField
		if err := json.Unmarshal([]byte(r[2]), &f); err != nil {
			return invalid("fields.csv: %v", err)
		}
		d.Fields = append(d.Fields, f)
	}
	rows, err = readCSV(files, "custom_values.csv")
	if err != nil {
		return err
	}
	at := map[string]int{}
	for i, is := range d.Issues {
		at[is.Key] = i
	}
	for _, r := range rows {
		if len(r) < 3 {
			continue
		}
		i, ok := at[r[0]]
		if !ok {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(r[2]), &v); err != nil {
			return invalid("custom_values.csv: %v", err)
		}
		if d.Issues[i].Custom == nil {
			d.Issues[i].Custom = map[string]any{}
		}
		d.Issues[i].Custom[r[1]] = v
	}
	return nil
}

// ----- import

// customFields creates or matches the fields of a bundle (by name) and their contexts. A field here with the same name
// but another type is left alone and its values are skipped, with a note.
func (c *importCtx) customFields(ctx context.Context, d *BundleData) error {
	c.fieldIDs = map[string]int64{}
	for _, bf := range d.Fields {
		if !fieldTypes[bf.Type] {
			c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("custom field %q has the unknown type %q and was skipped", bf.Name, bf.Type))
			continue
		}
		var id int64
		var typ string
		err := c.tx.QueryRow(ctx, `SELECT id,type FROM custom_field WHERE lower(name)=lower($1) AND trashed_at IS NULL`, strings.TrimSpace(bf.Name)).Scan(&id, &typ)
		switch {
		case err == nil && typ != bf.Type:
			c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("custom field %q exists here as %s, the bundle has %s: its values were skipped", bf.Name, typ, bf.Type))
			continue
		case err == pgx.ErrNoRows:
			b := func(v bool) *bool { return &v }
			in := FieldIn{Name: bf.Name, Type: bf.Type, Description: &bf.Description, HelpText: &bf.HelpText, Unit: &bf.Unit, Searchable: b(bf.Searchable), ShowOnCard: b(bf.ShowOnCard), Unique: b(bf.Unique),
				Min: bf.Min, Max: bf.Max, Regex: &bf.Regex, CopyOnClone: b(bf.CopyOnClone), TrackHistory: b(bf.TrackHistory), ViewRoles: bf.ViewRoles, EditRoles: bf.EditRoles}
			f, err := c.s.createField(ctx, c.tx, in)
			if err != nil {
				return err
			}
			id = f.ID
			c.rep.add("fields_created")
		case err != nil:
			return err
		default:
			c.rep.add("fields_matched")
		}
		c.fieldIDs[strings.ToLower(bf.Name)] = id
		f, err := c.s.getFieldQ(ctx, c.tx, id)
		if err != nil {
			return err
		}
		for _, bc := range bf.Contexts {
			if exists, err := c.contextExists(ctx, id, bc); err != nil {
				return err
			} else if exists {
				continue
			}
			var projects []string
			for _, k := range bc.Projects {
				var one int
				if c.tx.QueryRow(ctx, `SELECT 1 FROM project WHERE key=$1`, strings.ToUpper(k)).Scan(&one) == nil {
					projects = append(projects, k)
				} else {
					c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("custom field %q: project %s is not here, so that part of its context was left out", bf.Name, k))
				}
			}
			if !bc.Global && len(projects) == 0 {
				continue
			}
			var types []string
			for _, t := range bc.Types {
				var one int
				if c.tx.QueryRow(ctx, `SELECT 1 FROM issue_type WHERE lower(name)=lower($1)`, t).Scan(&one) == nil {
					types = append(types, t)
				}
			}
			if _, err := c.s.addContext(ctx, c.tx, f, id, ContextIn{Name: bc.Name, Global: bc.Global, Projects: projects, Types: types, Required: bc.Required, Default: bc.Default, Options: bc.Options}); err != nil {
				return fmt.Errorf("custom field %q: %w", bf.Name, err)
			}
			c.rep.add("field_contexts_created")
		}
	}
	return nil
}

func (c *importCtx) contextExists(ctx context.Context, fieldID int64, bc BContext) (bool, error) {
	rows, err := c.tx.Query(ctx, `SELECT id,is_global FROM custom_field_context WHERE field_id=$1`, fieldID)
	if err != nil {
		return false, err
	}
	type cx struct {
		id int64
		g  bool
	}
	var cs []cx
	for rows.Next() {
		var x cx
		rows.Scan(&x.id, &x.g)
		cs = append(cs, x)
	}
	rows.Close()
	set := func(q string, id int64) string {
		r, _ := c.tx.Query(ctx, q, id)
		var out []string
		for r.Next() {
			var s string
			r.Scan(&s)
			out = append(out, strings.ToLower(s))
		}
		r.Close()
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	norm := func(l []string) string {
		o := make([]string, len(l))
		for i, s := range l {
			o[i] = strings.ToLower(s)
		}
		sort.Strings(o)
		return strings.Join(o, ",")
	}
	for _, x := range cs {
		if x.g == bc.Global &&
			set(`SELECT p.key FROM custom_field_context_project cp JOIN project p ON p.id=cp.project_id WHERE cp.context_id=$1`, x.id) == norm(bc.Projects) &&
			set(`SELECT t.name FROM custom_field_context_type ct JOIN issue_type t ON t.id=ct.issue_type_id WHERE ct.context_id=$1`, x.id) == norm(bc.Types) {
			return true, nil
		}
	}
	return false, nil
}

// customValues stores each imported issue's values, checked like any edit: a value that does not fit is reported and skipped.
func (c *importCtx) customValues(ctx context.Context, d *BundleData) error {
	for _, bi := range d.Issues {
		if len(bi.Custom) == 0 {
			continue
		}
		iid, ok := c.issues[bi.Key]
		if !ok {
			continue
		}
		var pid, tid string
		if err := c.tx.QueryRow(ctx, `SELECT project_id::text,issue_type_id::text FROM issue WHERE id=$1`, iid).Scan(&pid, &tid); err != nil {
			return err
		}
		cfgs, err := c.s.fieldsFor(ctx, c.tx, pid, tid)
		if err != nil {
			return err
		}
		byName := map[string]FieldConfig{}
		for _, f := range cfgs {
			byName[strings.ToLower(f.Name)] = f
		}
		names := make([]string, 0, len(bi.Custom))
		for n := range bi.Custom {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			f, ok := byName[strings.ToLower(n)]
			if !ok {
				c.rep.Gaps = append(c.rep.Gaps, fmt.Sprintf("%s: the field %q does not apply here, its value was skipped", bi.Key, n))
				continue
			}
			v, err := c.s.normalize(ctx, c.tx, f.FieldDef, f.Options, bi.Custom[n], iid)
			if err != nil {
				c.rep.Warnings = append(c.rep.Warnings, fmt.Sprintf("%s: %s: %v (skipped)", bi.Key, n, err))
				continue
			}
			if err := c.s.storeValue(ctx, c.tx, iid, f.FieldDef, v); err != nil {
				return err
			}
			c.rep.add("custom_values_set")
		}
	}
	return nil
}

func (s *Service) getFieldQ(ctx context.Context, q querier, id int64) (FieldDef, error) {
	return scanField(q.QueryRow(ctx, `SELECT `+fieldCols+` FROM custom_field WHERE id=$1`, id))
}
