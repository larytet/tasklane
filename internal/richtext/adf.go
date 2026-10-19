package richtext

import (
	"encoding/json"
	"strings"
)

// FromADF reads an Atlassian Document Format value (decoded JSON). Blocks it does not understand, and blocks that contain something it cannot hold
// (a colour, an emoji, a status lozenge, a table with merged cells), become opaque nodes that keep the original JSON, so ToADF writes them back
// unchanged. An empty paragraph is dropped.
func FromADF(v any) Node {
	n, _ := v.(map[string]any)
	d := Node{Type: Doc}
	for _, c := range kids(n) {
		d.Content = append(d.Content, adfBlock(c)...)
	}
	return Normalize(d)
}

func kids(n map[string]any) []any { k, _ := n["content"].([]any); return k }

func attrs(n map[string]any) map[string]any { a, _ := n["attrs"].(map[string]any); return a }

func attrStr(n map[string]any, k string) string {
	if s, ok := attrs(n)[k].(string); ok {
		return s
	}
	return ""
}

func attrInt(n map[string]any, k string) int {
	if f, ok := attrs(n)[k].(float64); ok {
		return int(f)
	}
	return 0
}

func opaque(v any) []Node {
	b, _ := json.Marshal(v)
	return []Node{{Type: Opaque, Raw: string(b)}}
}

func adfBlock(v any) []Node {
	n, _ := v.(map[string]any)
	if n == nil {
		return nil
	}
	typ, _ := n["type"].(string)
	switch typ {
	case "paragraph", "heading":
		in, ok := adfInlines(kids(n))
		if !ok {
			return opaque(v)
		}
		for len(in) > 0 && in[len(in)-1].Type == HardBreak {
			in = in[:len(in)-1] // a break at the end of a block shows nothing
		}
		if blankInlines(in) {
			return nil
		}
		if typ == "heading" {
			for _, c := range in {
				if c.Type == HardBreak {
					return opaque(v) // a heading is one line
				}
			}
			lv := attrInt(n, "level")
			if lv < 1 || lv > 6 {
				return opaque(v)
			}
			return []Node{{Type: Heading, Level: lv, Content: in}}
		}
		return []Node{{Type: Paragraph, Content: in}}
	case "bulletList", "orderedList":
		out := Node{Type: BulletList}
		if typ == "orderedList" {
			out = Node{Type: OrderedList, Start: 1}
			if s := attrInt(n, "order"); s > 1 {
				out.Start = s
			}
		}
		for _, li := range kids(n) {
			m, _ := li.(map[string]any)
			if m == nil || m["type"] != "listItem" {
				return opaque(v)
			}
			item := Node{Type: ListItem}
			for _, c := range kids(m) {
				item.Content = append(item.Content, adfBlock(c)...)
			}
			if len(item.Content) == 0 {
				return opaque(v)
			}
			out.Content = append(out.Content, item)
		}
		return []Node{out}
	case "blockquote":
		q := Node{Type: Quote}
		for _, c := range kids(n) {
			q.Content = append(q.Content, adfBlock(c)...)
		}
		if len(q.Content) == 0 {
			return opaque(v)
		}
		return []Node{q}
	case "codeBlock":
		var b strings.Builder
		for _, c := range kids(n) {
			m, _ := c.(map[string]any)
			if m == nil || m["type"] != "text" || len(m) > 2 {
				return opaque(v)
			}
			t, _ := m["text"].(string)
			b.WriteString(t)
		}
		return []Node{{Type: CodeBlock, Lang: attrStr(n, "language"), Text: b.String()}}
	case "rule":
		return []Node{{Type: Rule}}
	case "table":
		t := Node{Type: Table}
		for ri, r := range kids(n) {
			rm, _ := r.(map[string]any)
			if rm == nil || rm["type"] != "tableRow" {
				return opaque(v)
			}
			row := Node{Type: Row}
			for _, c := range kids(rm) {
				cm, _ := c.(map[string]any)
				want := "tableCell"
				cellType := Cell
				if ri == 0 {
					want, cellType = "tableHeader", HeaderCell
				}
				if cm == nil || cm["type"] != want || len(attrs(cm)) > 0 {
					return opaque(v)
				}
				ps := kids(cm)
				if len(ps) != 1 {
					return opaque(v)
				}
				p, _ := ps[0].(map[string]any)
				if p == nil || p["type"] != "paragraph" {
					return opaque(v)
				}
				in, ok := adfInlines(kids(p))
				if !ok {
					return opaque(v)
				}
				row.Content = append(row.Content, Node{Type: cellType, Content: []Node{{Type: Paragraph, Content: in}}})
			}
			if len(row.Content) == 0 {
				return opaque(v)
			}
			t.Content = append(t.Content, row)
		}
		if len(t.Content) == 0 {
			return opaque(v)
		}
		return []Node{t}
	}
	return opaque(v)
}

func blankInlines(ns []Node) bool {
	for _, n := range ns {
		if n.Type != Text || strings.TrimSpace(n.Text) != "" {
			return false
		}
	}
	return true
}

func adfInlines(items []any) ([]Node, bool) {
	var out []Node
	for _, it := range items {
		n, _ := it.(map[string]any)
		if n == nil {
			return nil, false
		}
		switch n["type"] {
		case "text":
			t, _ := n["text"].(string)
			marks, ok := adfMarks(n)
			if !ok {
				return nil, false
			}
			if ms, issue := issueTarget(marks); issue != "" {
				_ = ms
				out = append(out, Node{Type: IssueLink, Text: issue})
				continue
			}
			parts := strings.Split(t, "\n") // a newline inside a text node is a hard break
			for i, p := range parts {
				if i > 0 {
					out = append(out, Node{Type: HardBreak})
				}
				out = append(out, Node{Type: Text, Text: p, Marks: marks})
			}
		case "hardBreak":
			out = append(out, Node{Type: HardBreak})
		case "mention":
			out = append(out, Node{Type: Mention, ID: attrStr(n, "id"), Text: strings.TrimPrefix(attrStr(n, "text"), "@")})
		case "mediaInline":
			if attrStr(n, "collection") != "tasklane" {
				return nil, false
			}
			out = append(out, Node{Type: Attachment, ID: attrStr(n, "id"), Text: attrStr(n, "alt")})
		default:
			return nil, false
		}
	}
	return out, true
}

func issueTarget(marks []Mark) ([]Mark, string) {
	for _, m := range marks {
		if m.Type == MLink && strings.HasPrefix(m.Href, "issue:") {
			return marks, strings.TrimPrefix(m.Href, "issue:")
		}
	}
	return marks, ""
}

func adfMarks(n map[string]any) ([]Mark, bool) {
	var out []Mark
	ms, _ := n["marks"].([]any)
	for _, m := range ms {
		mm, _ := m.(map[string]any)
		switch mm["type"] {
		case "strong":
			out = append(out, Mark{Type: MStrong})
		case "em":
			out = append(out, Mark{Type: MEm})
		case "strike":
			out = append(out, Mark{Type: MStrike})
		case "code":
			out = append(out, Mark{Type: MCode})
		case "link":
			out = append(out, Mark{Type: MLink, Href: attrStr(mm, "href")})
		default:
			return nil, false
		}
	}
	return sortedMarks(out), true
}

// ToADF writes a document as Atlassian Document Format.
func ToADF(d Node) map[string]any {
	return map[string]any{"version": 1, "type": "doc", "content": adfOut(d.Content)}
}

func adfOut(ns []Node) []any {
	out := []any{}
	for _, n := range ns {
		switch n.Type {
		case Paragraph:
			out = append(out, map[string]any{"type": "paragraph", "content": adfInlinesOut(n.Content)})
		case Heading:
			out = append(out, map[string]any{"type": "heading", "attrs": map[string]any{"level": n.Level}, "content": adfInlinesOut(n.Content)})
		case BulletList, OrderedList:
			items := []any{}
			for _, it := range n.Content {
				items = append(items, map[string]any{"type": "listItem", "content": adfOut(it.Content)})
			}
			l := map[string]any{"type": string(n.Type), "content": items}
			if n.Type == OrderedList && n.Start > 1 {
				l["attrs"] = map[string]any{"order": n.Start}
			}
			out = append(out, l)
		case Quote:
			out = append(out, map[string]any{"type": "blockquote", "content": adfOut(n.Content)})
		case CodeBlock:
			c := map[string]any{"type": "codeBlock"}
			if n.Lang != "" {
				c["attrs"] = map[string]any{"language": n.Lang}
			}
			if n.Text != "" {
				c["content"] = []any{map[string]any{"type": "text", "text": n.Text}}
			}
			out = append(out, c)
		case Rule:
			out = append(out, map[string]any{"type": "rule"})
		case Table:
			rows := []any{}
			for _, r := range n.Content {
				cells := []any{}
				for _, c := range r.Content {
					typ := "tableCell"
					if c.Type == HeaderCell {
						typ = "tableHeader"
					}
					cells = append(cells, map[string]any{"type": typ, "content": adfOut(c.Content)})
				}
				rows = append(rows, map[string]any{"type": "tableRow", "content": cells})
			}
			out = append(out, map[string]any{"type": "table", "content": rows})
		case Opaque:
			var v any
			if json.Unmarshal([]byte(n.Raw), &v) == nil {
				out = append(out, v)
			}
		}
	}
	return out
}

func adfInlinesOut(ns []Node) []any {
	out := []any{}
	for _, n := range ns {
		switch n.Type {
		case Text:
			t := map[string]any{"type": "text", "text": n.Text}
			if ms := marksOut(n.Marks); len(ms) > 0 {
				t["marks"] = ms
			}
			out = append(out, t)
		case HardBreak:
			out = append(out, map[string]any{"type": "hardBreak"})
		case Mention:
			out = append(out, map[string]any{"type": "mention", "attrs": map[string]any{"id": n.ID, "text": "@" + n.Text}})
		case IssueLink:
			out = append(out, map[string]any{"type": "text", "text": n.Text, "marks": []any{map[string]any{"type": "link", "attrs": map[string]any{"href": "issue:" + n.Text}}}})
		case Attachment:
			out = append(out, map[string]any{"type": "mediaInline", "attrs": map[string]any{"id": n.ID, "collection": "tasklane", "type": "file", "alt": n.Text}})
		}
	}
	return out
}

func marksOut(ms []Mark) []any {
	var out []any
	for _, m := range sortedMarks(ms) {
		switch m.Type {
		case MLink:
			out = append(out, map[string]any{"type": "link", "attrs": map[string]any{"href": m.Href}})
		default:
			out = append(out, map[string]any{"type": m.Type})
		}
	}
	return out
}
