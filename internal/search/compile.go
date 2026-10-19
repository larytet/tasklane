// Package search compiles TQL to SQL and runs it against PostgreSQL (the default engine, SPEC 10.1).
package search

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Scope is who is searching and what they may see. Every compiled query is constrained by it;
// there is no code path that builds a query without one (TQL-06, TQL-13).
type Scope struct {
	UserID     string
	All        bool                       // global administrator: every project
	Readable   []string                   // project ids the user may read
	Writable   []string                   // project ids where internal comments are visible
	OnlyKeys   []string                   // when not nil, only these issue keys can match (used for the own-edit overlay)
	Fields     map[string]FieldDef        // custom fields by lower-case name, cf[id] and customfield_id (filled by the engine)
	UserLookup func(name string) []string // resolves a name or email to user ids, for user-typed custom fields
	WeekStart  time.Weekday
	Now        func() time.Time
}

type compiled struct {
	where   string
	args    []any
	order   string
	rankArg int // 1-based arg index of the text query used for ranking, 0 if none
}

type compiler struct {
	sc    Scope
	args  []any
	rank  int
	nowFn func() time.Time
	users func(string) []string
	// sawArchived: the query says something about archived work, so the default exclusion of archived issues is not applied
	sawArchived bool
}

func (c *compiler) arg(v any) string {
	c.args = append(c.args, v)
	return "$" + strconv.Itoa(len(c.args))
}

// compile builds the WHERE clause (always starting with the scope and the trash filter) and ORDER BY.
func compile(q *tql.Query, sc Scope) (*compiled, error) {
	c := &compiler{sc: sc, nowFn: sc.Now, users: sc.UserLookup}
	if c.nowFn == nil {
		c.nowFn = time.Now
	}
	var where []string
	where = append(where, "i.deleted_at IS NULL")
	if !sc.All {
		where = append(where, "NOT i.restricted") // Jira security levels Tasklane cannot evaluate: administrators only
	}
	if !sc.All {
		if len(sc.Readable) == 0 {
			where = append(where, "false")
		} else {
			where = append(where, "i.project_id = ANY("+c.arg(sc.Readable)+"::uuid[])")
		}
	}
	if sc.OnlyKeys != nil {
		where = append(where, "i.key = ANY("+c.arg(sc.OnlyKeys)+"::text[])")
	}
	if q.Where != nil {
		w, err := c.node(q.Where)
		if err != nil {
			return nil, err
		}
		where = append(where, "("+w+")")
	}
	if !c.sawArchived {
		where = append(where, "i.archived_at IS NULL") // archived work stays out of the way unless the query asks for it
	}
	ord, err := c.order(q.Order)
	if err != nil {
		return nil, err
	}
	return &compiled{where: strings.Join(where, " AND "), args: c.args, order: ord, rankArg: c.rank}, nil
}

func (c *compiler) node(n tql.Node) (string, error) {
	switch x := n.(type) {
	case tql.Bool:
		l, err := c.node(x.L)
		if err != nil {
			return "", err
		}
		r, err := c.node(x.R)
		if err != nil {
			return "", err
		}
		op := " AND "
		if x.Or {
			op = " OR "
		}
		return "(" + l + op + r + ")", nil
	case tql.Not:
		s, err := c.node(x.X)
		if err != nil {
			return "", err
		}
		return "(NOT " + s + ")", nil
	case tql.Cmp:
		return c.cmp(x)
	}
	return "", &tql.Error{Msg: "internal: unknown node"}
}

// neg wraps a condition in a NULL-safe NOT: "assignee != x" must include unassigned issues (JQL semantics),
// and NOT(NULL) is NULL, which would silently drop them.
func negate(isNeg bool, cond string) string {
	if !isNeg {
		return "(" + cond + ")"
	}
	return "(NOT COALESCE((" + cond + "), false))"
}

func errAt(pos int, f string, a ...any) error { return &tql.Error{Pos: pos, Msg: fmt.Sprintf(f, a...)} }

var keyLike = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]+-\d+$`)

var statusCats = map[string]string{"to do": "todo", "todo": "todo", "in progress": "in_progress", "done": "done"}

func (c *compiler) cmp(x tql.Cmp) (string, error) {
	isNeg := false
	switch x.Op {
	case "!=", "notin", "wasnot", "!~", "isnot":
		isNeg = true
	}
	switch x.Field {
	case "text", "summary", "description", "comment", "any":
		return c.textClause(x)
	case "project", "key", "issuekey", "type", "issuetype", "status", "priority", "parent":
		col := map[string]string{"project": "upper(p.key)", "key": "i.key", "issuekey": "i.key", "type": "lower(t.name)", "issuetype": "lower(t.name)",
			"status": "lower(st.name)", "priority": "i.priority", "parent": "par.key"}[x.Field]
		upper := x.Field == "project" || x.Field == "key" || x.Field == "issuekey" || x.Field == "parent"
		switch x.Op {
		case "=", "!=", "in", "notin":
			var vs []string
			for _, v := range x.Vals {
				if v.Empty {
					if x.Field == "parent" {
						return negate(isNeg, "i.parent_id IS NULL"), nil
					}
					return "", errAt(v.Pos, "%s cannot be EMPTY", x.Field)
				}
				if v.Fn != "" {
					return "", errAt(v.Pos, "function %s() cannot be used with %s", v.Fn, x.Field)
				}
				s := strings.TrimSpace(v.S)
				if upper {
					s = strings.ToUpper(s)
				} else {
					s = strings.ToLower(s)
				}
				vs = append(vs, s)
			}
			arg := c.arg(vs)
			cond := col + " = ANY(" + arg + "::text[])"
			if x.Field == "key" || x.Field == "issuekey" { // an old key finds the issue forever
				cond = "(" + cond + " OR i.id IN (SELECT issue_id FROM issue_key_alias WHERE old_key = ANY(" + arg + "::text[])))"
			}
			return negate(isNeg, cond), nil
		case "is", "isnot":
			if x.Field == "parent" {
				if x.Op == "is" {
					return "(i.parent_id IS NULL)", nil
				}
				return "(i.parent_id IS NOT NULL)", nil
			}
			return "", errAt(x.Pos, "%s cannot be EMPTY", x.Field)
		case "was", "wasnot", "changed":
			return c.histClause(x)
		}
		return "", errAt(x.Pos, "operator %s cannot be used with %s", opName(x.Op), x.Field)
	case "archived":
		c.sawArchived = true
		switch x.Op {
		case "is":
			return "(i.archived_at IS NULL)", nil
		case "isnot":
			return "(i.archived_at IS NOT NULL)", nil
		case "=", "!=":
			var want bool
			switch strings.ToLower(x.Vals[0].S) {
			case "true", "yes":
				want = true
			case "false", "no":
			default:
				return "", errAt(x.Vals[0].Pos, "archived is true or false")
			}
			if (x.Op == "=") == want {
				return "(i.archived_at IS NOT NULL)", nil
			}
			return "(i.archived_at IS NULL)", nil
		}
		return "", errAt(x.Pos, "use archived = true, archived = false, or archived is EMPTY")
	case "statuscategory":
		if x.Op != "=" && x.Op != "!=" && x.Op != "in" && x.Op != "notin" {
			return "", errAt(x.Pos, "operator %s cannot be used with statusCategory", opName(x.Op))
		}
		var vs []string
		for _, v := range x.Vals {
			cat, ok := statusCats[strings.ToLower(v.S)]
			if !ok {
				return "", errAt(v.Pos, "unknown status category %q (use To Do, In Progress or Done)", v.S)
			}
			vs = append(vs, cat)
		}
		return negate(isNeg, "st.category = ANY("+c.arg(vs)+"::text[])"), nil
	case "assignee", "reporter":
		col := map[string]string{"assignee": "i.assignee_id", "reporter": "i.reporter_id"}[x.Field]
		switch x.Op {
		case "is", "isnot":
			if x.Field == "reporter" {
				return "", errAt(x.Pos, "reporter is never empty")
			}
			if x.Op == "is" {
				return "(" + col + " IS NULL)", nil
			}
			return "(" + col + " IS NOT NULL)", nil
		case "=", "!=", "in", "notin":
			var conds []string
			for _, v := range x.Vals {
				switch {
				case v.Empty:
					conds = append(conds, col+" IS NULL")
				case v.Fn == "currentuser":
					conds = append(conds, col+" = "+c.arg(c.sc.UserID)+"::uuid")
				case v.Fn != "":
					return "", errAt(v.Pos, "function %s() cannot be used with %s", v.Fn, x.Field)
				default:
					a := c.arg(strings.ToLower(strings.TrimSpace(v.S)))
					conds = append(conds, col+" IN (SELECT id FROM app_user WHERE lower(email)="+a+" OR lower(name)="+a+")")
				}
			}
			return negate(isNeg, strings.Join(conds, " OR ")), nil
		case "was", "wasnot", "changed":
			return c.histClause(x)
		}
		return "", errAt(x.Pos, "operator %s cannot be used with %s", opName(x.Op), x.Field)
	case "label", "labels":
		switch x.Op {
		case "=", "!=", "in", "notin":
			var vs []string
			for _, v := range x.Vals {
				if v.Empty {
					return negate(isNeg, "cardinality(i.labels)=0"), nil
				}
				vs = append(vs, v.S)
			}
			return negate(isNeg, "i.labels && "+c.arg(vs)+"::text[]"), nil
		case "is":
			return "(cardinality(i.labels)=0)", nil
		case "isnot":
			return "(cardinality(i.labels)>0)", nil
		}
		return "", errAt(x.Pos, "operator %s cannot be used with labels", opName(x.Op))
	case "fixversion", "affectedversion", "affectsversion":
		return c.versionClause(x, isNeg)
	case "sla.state", "sla.firstresponse.state", "sla.resolution.state":
		return c.slaClause(x, isNeg)
	case "inputrequested":
		switch x.Op {
		case "is":
			return "(i.input_requested_at IS NULL)", nil
		case "isnot":
			return "(i.input_requested_at IS NOT NULL)", nil
		}
		return "", errAt(x.Pos, "use inputrequested is EMPTY or inputrequested is not EMPTY")
	case "created", "updated", "resolved", "resolutiondate":
		col := map[string]string{"created": "i.created_at", "updated": "i.updated_at", "resolved": "i.resolved_at", "resolutiondate": "i.resolved_at"}[x.Field]
		switch x.Op {
		case "is":
			return "(" + col + " IS NULL)", nil
		case "isnot":
			return "(" + col + " IS NOT NULL)", nil
		case "=", "!=", ">", "<", ">=", "<=":
			t, day, err := c.date(x.Vals[0])
			if err != nil {
				return "", err
			}
			if day && (x.Op == "=" || x.Op == "!=") { // a bare date means the whole day
				a, b := c.arg(t), c.arg(t.AddDate(0, 0, 1))
				if x.Op == "=" {
					return "(" + col + " >= " + a + " AND " + col + " < " + b + ")", nil
				}
				return "(NOT (" + col + " >= " + a + " AND " + col + " < " + b + "))", nil
			}
			return "(" + col + " " + x.Op + " " + c.arg(t) + ")", nil
		}
		return "", errAt(x.Pos, "operator %s cannot be used with %s", opName(x.Op), x.Field)
	}
	if cond, ok, err := c.devClause(x); ok {
		return cond, err
	}
	if f, ok := c.sc.Fields[x.Field]; ok {
		return c.customClause(x, f)
	}
	return "", errAt(x.Pos, "unknown field %q", x.Field)
}

func opName(op string) string {
	return map[string]string{"in": "IN", "notin": "NOT IN", "is": "IS", "isnot": "IS NOT", "was": "WAS", "wasnot": "WAS NOT", "changed": "CHANGED"}[op] + opRaw(op)
}
func opRaw(op string) string {
	switch op {
	case "in", "notin", "is", "isnot", "was", "wasnot", "changed":
		return ""
	}
	return op
}

// textClause handles ~ and !~ on summary, description, comment and the combined text field.
func (c *compiler) textClause(x tql.Cmp) (string, error) {
	if x.Op != "~" && x.Op != "!~" {
		return "", errAt(x.Pos, "%s supports only ~ and !~", x.Field)
	}
	v := x.Vals[0]
	if v.Fn != "" || v.Empty {
		return "", errAt(v.Pos, "%s ~ needs a word or phrase", x.Field)
	}
	term := strings.TrimSpace(v.S)
	if term == "" {
		return "", errAt(v.Pos, "empty search text")
	}
	q := c.arg(term)
	qi := len(c.args) // index of the term argument, used for ranking
	tsq := "plainto_tsquery('simple', unaccent(" + q + "))"
	// One IN (subquery) over a UNION ALL of index-driven branches. An OR of EXISTS subqueries would
	// force a per-row subplan over the whole issue table and never use the GIN indexes (PERF-16).
	var branches []string
	switch x.Field {
	case "summary":
		branches = append(branches, "SELECT s.issue_id FROM issue_search s WHERE s.summary_norm LIKE '%' || lower(unaccent("+q+")) || '%'")
	case "description":
		branches = append(branches, "SELECT s.issue_id FROM issue_search s WHERE s.tsv @@ "+tsq)
	case "comment":
		branches = append(branches, c.commentBranch(tsq))
	case "any": // text, a summary substring, and people: the field the plain search box (tql.FromText) uses
		if keyLike.MatchString(term) {
			branches = append(branches, "SELECT k.id FROM issue k WHERE k.key = upper("+q+")")
		}
		branches = append(branches, "SELECT s.issue_id FROM issue_search s WHERE s.tsv @@ "+tsq, c.commentBranch(tsq))
		if utf8.RuneCountInString(term) >= 3 {
			branches = append(branches, "SELECT s.issue_id FROM issue_search s WHERE s.summary_norm LIKE '%' || lower(unaccent("+q+")) || '%'")
		}
		people := "SELECT u.id FROM app_user u WHERE " + peopleMatch("u", c.arg(likeEscape(term)))
		branches = append(branches, "SELECT k.id FROM issue k WHERE k.assignee_id IN ("+people+")", "SELECT k.id FROM issue k WHERE k.reporter_id IN ("+people+")")
		if c.rank == 0 && x.Op == "~" {
			c.rank = qi
		}
	default: // text: summary, labels, description and comments, plus an exact key match
		if keyLike.MatchString(term) {
			branches = append(branches, "SELECT k.id FROM issue k WHERE k.key = upper("+q+")")
		}
		branches = append(branches, "SELECT s.issue_id FROM issue_search s WHERE s.tsv @@ "+tsq, c.commentBranch(tsq))
		if c.rank == 0 && x.Op == "~" {
			c.rank = qi
		}
	}
	return negate(x.Op == "!~", "i.id IN ("+strings.Join(branches, " UNION ALL ")+")"), nil
}

// peopleMatch is the condition on app_user alias a for "a word of the name, or the email, starts with" the LIKE-escaped term
// in arg: Alan finds Alan Polevoi and alan@example.com, not Catalan.
func peopleMatch(a, arg string) string {
	return "((' ' || lower(unaccent(" + a + ".name))) LIKE '% ' || lower(unaccent(" + arg + ")) || '%' OR lower(" + a + ".email) LIKE lower(" + arg + ") || '%')"
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}

// commentBranch never matches internal comments for users who cannot see them.
func (c *compiler) commentBranch(tsq string) string {
	vis := "NOT cs.internal"
	if c.sc.All {
		vis = "true"
	} else if len(c.sc.Writable) > 0 {
		vis = "(NOT cs.internal OR cs.project_id = ANY(" + c.arg(c.sc.Writable) + "::uuid[]))"
	}
	return "SELECT cs.issue_id FROM comment_search cs WHERE " + vis + " AND cs.tsv @@ " + tsq
}

// histClause implements WAS and CHANGED from the issue history store.
func (c *compiler) histClause(x tql.Cmp) (string, error) {
	field := map[string]string{"status": "status", "priority": "priority", "assignee": "assignee", "project": "project"}[x.Field]
	if field == "" {
		return "", errAt(x.Pos, "history search (WAS, CHANGED) is not supported for %s", x.Field)
	}
	h := "EXISTS (SELECT 1 FROM issue_history h WHERE h.issue_id=i.id AND h.field=" + c.arg(field)
	switch x.Op {
	case "was", "wasnot":
		v := x.Vals[0]
		if v.Fn != "" {
			return "", errAt(v.Pos, "functions are not supported with WAS")
		}
		val := strings.ToLower(strings.TrimSpace(v.S))
		// "was X": X is the value before some change, or the value it was created with
		a := c.arg(val)
		w := h + " AND (lower(h.before)=" + a + " OR lower(h.after)=" + a + "))"
		if x.Field == "status" || x.Field == "priority" {
			// the creation status/priority is not in history rows, so the current value also counts as "was"
			cur := map[string]string{"status": "lower(st.name)", "priority": "i.priority"}[x.Field]
			w = "(" + w + " OR " + cur + " = " + a + ")"
		}
		if x.Op == "wasnot" {
			return "(NOT " + w + ")", nil
		}
		return w, nil
	case "changed":
		cond := h
		if x.From != nil {
			cond += " AND lower(h.before)=" + c.arg(strings.ToLower(x.From.S))
		}
		if x.To != nil {
			cond += " AND lower(h.after)=" + c.arg(strings.ToLower(x.To.S))
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

var relRe = regexp.MustCompile(`^([+-]?)(\d+)([mhdw])$`)

// date resolves a value to an instant; day reports whether it was a bare date (whole-day semantics).
func (c *compiler) date(v tql.Value) (t time.Time, day bool, err error) {
	now := c.nowFn().UTC()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch v.Fn {
	case "now":
		return now, false, nil
	case "startofday":
		return startOfDay, false, nil
	case "endofday":
		return startOfDay.AddDate(0, 0, 1), false, nil
	case "startofweek", "endofweek":
		back := (int(now.Weekday()) - int(c.sc.WeekStart) + 7) % 7
		start := startOfDay.AddDate(0, 0, -back)
		if v.Fn == "endofweek" {
			return start.AddDate(0, 0, 7), false, nil
		}
		return start, false, nil
	case "startofmonth":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), false, nil
	case "":
	default:
		return t, false, errAt(v.Pos, "function %s() is not supported for dates", v.Fn)
	}
	if m := relRe.FindStringSubmatch(v.S); m != nil {
		n, _ := strconv.Atoi(m[2])
		if n > 100000 {
			return t, false, errAt(v.Pos, "relative date is too large")
		}
		if m[1] == "-" || m[1] == "" {
			n = -n
		}
		switch m[3] {
		case "m":
			return now.Add(time.Duration(n) * time.Minute), false, nil
		case "h":
			return now.Add(time.Duration(n) * time.Hour), false, nil
		case "d":
			return now.AddDate(0, 0, n), false, nil
		case "w":
			return now.AddDate(0, 0, 7*n), false, nil
		}
	}
	for _, l := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if tt, e := time.Parse(l, v.S); e == nil {
			return tt, l == "2006-01-02", nil
		}
	}
	return t, false, errAt(v.Pos, "cannot read %q as a date (use 2026-10-19, 2026-10-19 14:30, -7d, -2w or a function such as startOfWeek())", v.S)
}

var orderCols = map[string]string{
	"key": "p.key|i.number", "issuekey": "p.key|i.number", "created": "i.created_at", "updated": "i.updated_at", "resolved": "i.resolved_at",
	"summary": "lower(i.summary)", "status": "st.position|st.name", "priority": "array_position(ARRAY['lowest','low','medium','high','highest'], i.priority)",
	"assignee": "asg.name", "reporter": "rp.name", "type": "t.name", "issuetype": "t.name", "rank": "i.rank",
}

func (c *compiler) order(os []tql.Order) (string, error) {
	if len(os) == 0 {
		return "", nil
	}
	var parts []string
	for _, o := range os {
		col, ok := orderCols[o.Field]
		if !ok {
			if f, isField := c.sc.Fields[o.Field]; isField {
				dir := " ASC"
				if o.Desc {
					dir = " DESC"
				}
				parts = append(parts, c.customOrder(f)+dir+" NULLS LAST")
				continue
			}
			return "", errAt(0, "cannot sort by %q", o.Field)
		}
		dir := " ASC"
		if o.Desc {
			dir = " DESC"
		}
		for _, cc := range strings.Split(col, "|") {
			if o.Field == "key" || o.Field == "issuekey" {
				parts = append(parts, cc+dir)
			} else {
				parts = append(parts, cc+dir+" NULLS LAST")
			}
		}
	}
	return strings.Join(parts, ", "), nil
}
