package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/jsonpath"
)

// The generic JSON importer (SPEC 18.2, "any JSON array via a JSONPath mapping"): the administrator says where the issues are and which
// path inside each issue holds each field. It builds the same bundle as the other importers, so the dry run, per-item errors, updating in
// place on a second run and rollback are shared. Paths are the small safe subset in internal/jsonpath.

// JSONMapping is the whole configuration of the generic JSON importer.
type JSONMapping struct {
	Items         string            `json:"items"`          // where the issues are: a path to an array (or to several), such as $.data.issues
	Fields        map[string]string `json:"fields"`         // field -> path inside one item: key, summary (required), description, type, status, priority, assignee, reporter, labels, parent, created, updated, resolved, estimate
	Comments      string            `json:"comments"`       // path inside one item to its comments
	CommentFields map[string]string `json:"comment_fields"` // id, author, body (required), created: paths inside one comment
	LabelSep      string            `json:"label_sep"`      // splits a labels value that is one string (default ",")
	DefaultType   string            `json:"default_type"`   // when the type is empty or unmapped (default Task)
}

var jsonFields = map[string]bool{"key": true, "summary": true, "description": true, "type": true, "status": true, "priority": true, "assignee": true,
	"reporter": true, "labels": true, "parent": true, "created": true, "updated": true, "resolved": true, "estimate": true}
var jsonCommentFields = map[string]bool{"id": true, "author": true, "body": true, "created": true}

func compileFields(what string, in map[string]string, known map[string]bool) (map[string]*jsonpath.Path, error) {
	out := map[string]*jsonpath.Path{}
	names := make([]string, 0, len(in))
	for k := range in {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !known[k] {
			return nil, invalid("%s: %q is not a field the importer knows", what, k)
		}
		if strings.TrimSpace(in[k]) == "" {
			continue
		}
		p, err := jsonpath.Compile(in[k])
		if err != nil {
			return nil, invalid("%s %s: %v", what, k, err)
		}
		out[k] = p
	}
	return out, nil
}

// jsonValues reads every value a field's path selects and turns scalars into text; structures are ignored.
func jsonText(p *jsonpath.Path, item any) string {
	if p == nil {
		return ""
	}
	v, ok := p.First(item)
	if !ok {
		return ""
	}
	return strings.TrimSpace(jsonpath.String(v))
}

func jsonList(p *jsonpath.Path, item any, sep string) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, v := range p.Eval(item) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				if s := strings.TrimSpace(jsonpath.String(e)); s != "" {
					out = append(out, s)
				}
			}
		case string:
			for _, e := range strings.Split(x, sep) {
				if e = strings.TrimSpace(e); e != "" {
					out = append(out, e)
				}
			}
		default:
			if s := strings.TrimSpace(jsonpath.String(v)); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// jsonTime reads a date from text, or a number as Unix seconds (or milliseconds when it is that large).
func jsonTime(s string) (time.Time, bool) {
	if t, ok := toolTime(s); ok {
		return t, true
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		if n > 1e11 {
			return time.UnixMilli(int64(n)).UTC(), true
		}
		return time.Unix(int64(n), 0).UTC(), true
	}
	return time.Time{}, false
}

func (b *toolBuild) generic(data []byte) error {
	m := b.in.Mapping
	if m == nil || strings.TrimSpace(m.Items) == "" {
		return invalid("the generic JSON importer needs a mapping: where the issues are (items) and the path of each field (fields)")
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return invalid("the file is not valid JSON: %v", err)
	}
	itemsPath, err := jsonpath.Compile(m.Items)
	if err != nil {
		return invalid("items: %v", err)
	}
	fp, err := compileFields("fields", m.Fields, jsonFields)
	if err != nil {
		return err
	}
	if fp["key"] == nil || fp["summary"] == nil {
		return invalid("the mapping needs a path for key and for summary")
	}
	cp, err := compileFields("comment_fields", m.CommentFields, jsonCommentFields)
	if err != nil {
		return err
	}
	var commentsPath *jsonpath.Path
	if strings.TrimSpace(m.Comments) != "" {
		if commentsPath, err = jsonpath.Compile(m.Comments); err != nil {
			return invalid("comments: %v", err)
		}
		if cp["body"] == nil {
			return invalid("comments are mapped but comment_fields has no path for body")
		}
	}
	sep := m.LabelSep
	if sep == "" {
		sep = ","
	}
	types := lowerMap(b.in.TypeMap)
	defType := strings.TrimSpace(m.DefaultType)
	if defType == "" {
		defType = "Task"
	}
	var items []any
	for _, v := range itemsPath.Eval(doc) {
		if a, ok := v.([]any); ok {
			items = append(items, a...)
		} else {
			items = append(items, v)
		}
	}
	for n, it := range items {
		if _, ok := it.(map[string]any); !ok {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("item %d: not an object", n+1))
			continue
		}
		id := jsonText(fp["key"], it)
		if id == "" {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("item %d: the key path found nothing", n+1))
			continue
		}
		key := "json:" + id
		i := BIssue{Key: key, Summary: jsonText(fp["summary"], it), Type: defType, Description: jsonText(fp["description"], it), Created: time.Now()}
		if i.Summary == "" {
			b.rep.Errors = append(b.rep.Errors, fmt.Sprintf("item %d (%s): the summary is empty", n+1, id))
			continue
		}
		if t := jsonText(fp["type"], it); t != "" {
			if mt, ok := types[strings.ToLower(t)]; ok {
				i.Type = mt
			} else {
				b.note("type:"+strings.ToLower(t), fmt.Sprintf("type %q is not in type_map: %s is used", t, defType))
			}
		}
		i.Priority = b.priority(jsonText(fp["priority"], it))
		if st := jsonText(fp["status"], it); st != "" {
			i.Status = b.status(st)
		} else {
			i.Status = b.status("todo")
		}
		if t, ok := jsonTime(jsonText(fp["created"], it)); ok {
			i.Created = t
		}
		if t, ok := jsonTime(jsonText(fp["updated"], it)); ok {
			i.Updated = t
		}
		if t, ok := jsonTime(jsonText(fp["resolved"], it)); ok {
			i.Resolved = &t
			if !b.isDone(i.Status) {
				i.Status = b.status("done")
			}
		} else if b.isDone(i.Status) {
			t := i.Updated
			if t.IsZero() {
				t = i.Created
			}
			i.Resolved = &t
		}
		if s := jsonText(fp["estimate"], it); s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 {
				i.Estimate = &f
			}
		}
		for _, l := range jsonList(fp["labels"], it, sep) {
			if l = b.label(l); l != "" {
				i.Labels = append(i.Labels, l)
			}
		}
		i.Assignee, i.Reporter = b.person(jsonText(fp["assignee"], it), "assignee"), b.person(jsonText(fp["reporter"], it), "reporter")
		if p := jsonText(fp["parent"], it); p != "" {
			i.Parent = "json:" + p
			if i.Type == defType && types["subtask"] == "" {
				i.Type = "Subtask"
			}
		}
		if !b.add(i) {
			continue
		}
		if commentsPath != nil {
			var cs []any
			for _, v := range commentsPath.Eval(it) {
				if a, ok := v.([]any); ok {
					cs = append(cs, a...)
				} else {
					cs = append(cs, v)
				}
			}
			for j, c := range cs {
				cid := jsonText(cp["id"], c)
				if cid == "" {
					cid = strconv.Itoa(j) // without an id, the position keeps a second run from duplicating
				}
				at := i.Created
				if t, ok := jsonTime(jsonText(cp["created"], c)); ok {
					at = t
				}
				b.comment(key, cid, b.person(jsonText(cp["author"], c), "comment author"), jsonText(cp["body"], c), at)
			}
		}
	}
	return nil
}
