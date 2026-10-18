package search

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/larytet/tasklane/internal/search/tql"
)

// FieldDef is what the compiler needs to know about a custom field.
type FieldDef struct {
	ID         int64
	Name       string
	Type       string
	Restricted bool // viewing is limited to some roles: only administrators may search it
}

func isMultiType(t string) bool {
	return t == "multi_select" || t == "checkbox_group" || t == "multi_user" || t == "labels"
}

func isNumType(t string) bool {
	return t == "number" || t == "integer" || t == "float" || t == "duration" || t == "rating"
}

// fieldMap returns custom fields by lower-case name, cf[id] and customfield_id. The table is small, so it is read on every
// search: a field created a moment ago is searchable at once.
func (e *Postgres) fieldMap(ctx context.Context) map[string]FieldDef {
	m := map[string]FieldDef{}
	rows, err := e.DB.Query(ctx, `SELECT id,name,type,cardinality(view_roles)>0 FROM custom_field WHERE trashed_at IS NULL AND searchable`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var f FieldDef
		rows.Scan(&f.ID, &f.Name, &f.Type, &f.Restricted)
		m[strings.ToLower(f.Name)] = f
		m["cf["+strconv.FormatInt(f.ID, 10)+"]"] = f
		m["customfield_"+strconv.FormatInt(f.ID, 10)] = f
	}
	return m
}

// customClause compiles a clause on a custom field. A missing value never matches a positive test, and negated tests
// include issues without a value (the same NULL-safe rule as the built-in fields).
func (c *compiler) customClause(x tql.Cmp, f FieldDef) (string, error) {
	if f.Restricted && !c.sc.All { // a clause on a field the caller may not view is refused, exactly as if it did not exist
		return "", errAt(x.Pos, "unknown field %q", x.Field)
	}
	ex := func(cond string) string {
		return "EXISTS (SELECT 1 FROM issue_field_value v WHERE v.issue_id=i.id AND v.field_id=" + c.arg(f.ID) + " AND " + cond + ")"
	}
	any := func() string { // evaluated only where used: an unused parameter makes the statement invalid
		return "EXISTS (SELECT 1 FROM issue_field_value v WHERE v.issue_id=i.id AND v.field_id=" + c.arg(f.ID) + ")"
	}
	isNeg := false
	switch x.Op {
	case "!=", "notin", "!~", "isnot":
		isNeg = true
	}
	not := func(s string) string { return "(NOT " + s + ")" }
	switch x.Op {
	case "is":
		return "(NOT " + any() + ")", nil
	case "isnot":
		return "(" + any() + ")", nil
	case "was", "wasnot", "changed":
		return c.customHistory(x, f)
	}
	val := func(v tql.Value) (string, error) {
		if v.Empty {
			return "", errAt(v.Pos, "use IS EMPTY to test for no value")
		}
		if v.Fn == "currentuser" && (f.Type == "user" || f.Type == "multi_user") {
			return strings.ToLower(c.sc.UserID), nil
		}
		if v.Fn != "" {
			return "", errAt(v.Pos, "function %s() cannot be used with %s", v.Fn, f.Name)
		}
		return strings.TrimSpace(v.S), nil
	}
	switch {
	case isNumType(f.Type):
		if x.Op == "~" || x.Op == "!~" {
			return "", errAt(x.Pos, "%s is a number: use =, !=, <, >, <=, >= or IN", f.Name)
		}
		conv := func(v tql.Value) (float64, error) {
			s, err := val(v)
			if err != nil {
				return 0, err
			}
			if f.Type == "duration" {
				if n, ok := durationSeconds(s); ok {
					return n, nil
				}
			} else if n, err := strconv.ParseFloat(s, 64); err == nil {
				return n, nil
			}
			return 0, errAt(v.Pos, "%q is not a valid value for %s", s, f.Name)
		}
		switch x.Op {
		case "=", "!=", "in", "notin":
			var ns []float64
			for _, v := range x.Vals {
				n, err := conv(v)
				if err != nil {
					return "", err
				}
				ns = append(ns, n)
			}
			cond := ex("v.num_value = ANY(" + c.arg(ns) + "::numeric[])")
			if isNeg {
				return not(cond), nil
			}
			return cond, nil
		case ">", "<", ">=", "<=":
			n, err := conv(x.Vals[0])
			if err != nil {
				return "", err
			}
			return ex("v.num_value " + x.Op + " " + c.arg(n)), nil
		}
	case f.Type == "date" || f.Type == "datetime":
		switch x.Op {
		case "=", "!=", ">", "<", ">=", "<=":
			t, day, err := c.date(x.Vals[0])
			if err != nil {
				return "", err
			}
			if day && (x.Op == "=" || x.Op == "!=") {
				cond := ex("v.date_value >= " + c.arg(t) + " AND v.date_value < " + c.arg(t.AddDate(0, 0, 1)))
				if x.Op == "!=" {
					return not(cond), nil
				}
				return cond, nil
			}
			cond := ex("v.date_value " + map[string]string{"=": "=", "!=": "=", ">": ">", "<": "<", ">=": ">=", "<=": "<="}[x.Op] + " " + c.arg(t))
			if x.Op == "!=" {
				return not(cond), nil
			}
			return cond, nil
		}
	case isMultiType(f.Type):
		switch x.Op {
		case "=", "!=", "in", "notin":
			var vs []string
			for _, v := range x.Vals {
				s, err := val(v)
				if err != nil {
					return "", err
				}
				if f.Type == "multi_user" && v.Fn == "" {
					s = "" // resolved below by name or email
					ids, err := c.userIDs(v.S)
					if err != nil {
						return "", err
					}
					vs = append(vs, ids...)
					continue
				}
				vs = append(vs, strings.ToLower(s))
			}
			cond := ex("EXISTS (SELECT 1 FROM jsonb_array_elements_text(v.value) e WHERE lower(e) = ANY(" + c.arg(vs) + "::text[]))")
			if isNeg {
				return not(cond), nil
			}
			return cond, nil
		case "~", "!~":
			s, err := val(x.Vals[0])
			if err != nil {
				return "", err
			}
			cond := ex("v.text_value LIKE '%' || lower(" + c.arg(s) + ") || '%'")
			if isNeg {
				return not(cond), nil
			}
			return cond, nil
		}
	default: // text, textarea, url, select, radio, user, issue_picker
		switch x.Op {
		case "=", "!=", "in", "notin":
			var vs []string
			for _, v := range x.Vals {
				if f.Type == "user" && v.Fn == "" && !v.Empty {
					ids, err := c.userIDs(v.S)
					if err != nil {
						return "", err
					}
					vs = append(vs, ids...)
					continue
				}
				s, err := val(v)
				if err != nil {
					return "", err
				}
				if f.Type == "issue_picker" {
					s = strings.ToUpper(s)
				}
				vs = append(vs, strings.ToLower(s))
			}
			cond := ex("v.text_value = ANY(" + c.arg(vs) + "::text[])")
			if isNeg {
				return not(cond), nil
			}
			return cond, nil
		case "~", "!~":
			s, err := val(x.Vals[0])
			if err != nil {
				return "", err
			}
			cond := ex("v.text_value LIKE '%' || lower(" + c.arg(s) + ") || '%'")
			if isNeg {
				return not(cond), nil
			}
			return cond, nil
		}
	}
	return "", errAt(x.Pos, "operator %s cannot be used with %s", opName(x.Op), f.Name)
}

// userIDs resolves a name or email to user ids (lower-case), at least one placeholder so a miss matches nothing.
func (c *compiler) userIDs(ref string) ([]string, error) {
	if c.users == nil {
		return []string{"00000000-0000-0000-0000-000000000000"}, nil
	}
	ids := c.users(strings.ToLower(strings.TrimSpace(ref)))
	if len(ids) == 0 {
		ids = []string{"00000000-0000-0000-0000-000000000000"}
	}
	return ids, nil
}

func (c *compiler) customHistory(x tql.Cmp, f FieldDef) (string, error) {
	field := "customfield_" + strconv.FormatInt(f.ID, 10)
	h := "EXISTS (SELECT 1 FROM issue_history h WHERE h.issue_id=i.id AND h.field=" + c.arg(field)
	un := func(col string) string { return "lower(trim(both '\"' from " + col + "))" }
	switch x.Op {
	case "was", "wasnot":
		v := x.Vals[0]
		if v.Fn != "" {
			return "", errAt(v.Pos, "functions are not supported with WAS")
		}
		a := c.arg(strings.ToLower(strings.TrimSpace(v.S)))
		w := h + " AND (" + un("h.before") + "=" + a + " OR " + un("h.after") + "=" + a + "))"
		if x.Op == "wasnot" {
			return "(NOT " + w + ")", nil
		}
		return w, nil
	case "changed":
		cond := h
		if x.From != nil {
			cond += " AND " + un("h.before") + "=" + c.arg(strings.ToLower(x.From.S))
		}
		if x.To != nil {
			cond += " AND " + un("h.after") + "=" + c.arg(strings.ToLower(x.To.S))
		}
		if x.By != nil {
			if x.By.Fn == "currentuser" {
				cond += " AND h.actor_id=" + c.arg(c.sc.UserID) + "::uuid"
			} else {
				a := c.arg(strings.ToLower(x.By.S))
				cond += " AND h.actor_id IN (SELECT id FROM app_user WHERE lower(email)=" + a + " OR lower(name)=" + a + ")"
			}
		}
		return cond + ")", nil
	}
	return "", errAt(x.Pos, "unsupported history operator")
}

func durationSeconds(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n, true
	}
	total, num := 0.0, ""
	for _, r := range strings.ToLower(s) + " " {
		switch {
		case r >= '0' && r <= '9' || r == '.':
			num += string(r)
		case r == ' ':
		default:
			n, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			switch r {
			case 'w':
				total += n * 40 * 3600
			case 'd':
				total += n * 8 * 3600
			case 'h':
				total += n * 3600
			case 'm':
				total += n * 60
			case 's':
				total += n
			default:
				return 0, false
			}
			num = ""
		}
	}
	return total, num == ""
}

// customOrder sorts by a custom field's typed value.
func (c *compiler) customOrder(f FieldDef) string {
	id := strconv.FormatInt(f.ID, 10)
	switch {
	case isNumType(f.Type):
		return "(SELECT num_value FROM issue_field_value WHERE issue_id=i.id AND field_id=" + id + ")"
	case f.Type == "date" || f.Type == "datetime":
		return "(SELECT date_value FROM issue_field_value WHERE issue_id=i.id AND field_id=" + id + ")"
	}
	return "(SELECT text_value FROM issue_field_value WHERE issue_id=i.id AND field_id=" + id + ")"
}

var _ = fmt.Sprint
