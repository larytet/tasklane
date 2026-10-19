package richtext

import (
	"regexp"
	"strconv"
	"strings"
)

// FromMarkdown reads the dialect (and, loosely, ordinary Markdown) into a document.
func FromMarkdown(s string) Node {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return Normalize(Node{Type: Doc, Content: parseBlocks(strings.Split(s, "\n"))})
}

var (
	reHeading = regexp.MustCompile(`^(#{1,6})( (.*))?$`)
	reOrdered = regexp.MustCompile(`^(\d+)\. `)
	reFence   = regexp.MustCompile("^(`{3,})([^`]*)$") // the info string of a fence has no backticks: a line that begins with a code span is not a fence
	reSep     = regexp.MustCompile(`^\|( *:?-+:? *\|)+$`)
)

func isBlank(l string) bool { return strings.TrimSpace(l) == "" }

func listMarker(l string) (kind string, width int, start int) {
	if strings.HasPrefix(l, "- ") {
		return "bullet", 2, 0
	}
	if m := reOrdered.FindStringSubmatch(l); m != nil {
		n, _ := strconv.Atoi(m[1])
		return "ordered", len(m[0]), n
	}
	return "", 0, 0
}

func startsBlock(l string) bool {
	if reFence.MatchString(l) || reHeading.MatchString(l) || l == "---" || strings.HasPrefix(l, ">") {
		return true
	}
	k, _, _ := listMarker(l)
	return k != ""
}

func parseBlocks(lines []string) []Node {
	var out []Node
	for i := 0; i < len(lines); {
		l := lines[i]
		switch {
		case isBlank(l):
			i++
		case reFence.MatchString(l):
			m := reFence.FindStringSubmatch(l)
			f, info := m[1], strings.TrimSpace(m[2])
			j := i + 1
			var body []string
			for ; j < len(lines) && lines[j] != f; j++ {
				body = append(body, lines[j])
			}
			text := strings.Join(body, "\n")
			if info == "adf-opaque" {
				out = append(out, Node{Type: Opaque, Raw: text})
			} else {
				out = append(out, Node{Type: CodeBlock, Lang: info, Text: text})
			}
			i = j + 1
		case reHeading.MatchString(l):
			m := reHeading.FindStringSubmatch(l)
			out = append(out, Node{Type: Heading, Level: len(m[1]), Content: parseInline(m[3])})
			i++
		case l == "---":
			out = append(out, Node{Type: Rule})
			i++
		case strings.HasPrefix(l, ">"):
			var inner []string
			for ; i < len(lines) && strings.HasPrefix(lines[i], ">"); i++ {
				x := strings.TrimPrefix(lines[i], ">")
				inner = append(inner, strings.TrimPrefix(x, " "))
			}
			out = append(out, Node{Type: Quote, Content: parseBlocks(inner)})
		case strings.HasPrefix(l, "|") && i+1 < len(lines) && reSep.MatchString(strings.TrimRight(lines[i+1], " ")):
			rowOf := func(line string, typ string) Node {
				r := Node{Type: Row}
				for _, c := range splitRow(line) {
					r.Content = append(r.Content, Node{Type: typ, Content: []Node{{Type: Paragraph, Content: parseInlineCell(c)}}})
				}
				return r
			}
			rows := []Node{rowOf(lines[i], HeaderCell)}
			i += 2
			for ; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
				rows = append(rows, rowOf(lines[i], Cell))
			}
			out = append(out, Node{Type: Table, Content: rows})
		default:
			if kind, _, start := listMarker(l); kind != "" {
				list := Node{Type: BulletList}
				if kind == "ordered" {
					list = Node{Type: OrderedList, Start: start}
				}
				for i < len(lines) {
					k, w, _ := listMarker(lines[i])
					if k != kind {
						break
					}
					item := []string{lines[i][w:]}
					i++
					for i < len(lines) {
						if isBlank(lines[i]) {
							// blank lines belong to the item only when an indented line follows them
							j := i
							for j < len(lines) && isBlank(lines[j]) {
								j++
							}
							if j < len(lines) && strings.HasPrefix(lines[j], strings.Repeat(" ", w)) {
								for ; i < j; i++ {
									item = append(item, "")
								}
								continue
							}
							break
						}
						if !strings.HasPrefix(lines[i], strings.Repeat(" ", w)) {
							break
						}
						item = append(item, lines[i][w:])
						i++
					}
					list.Content = append(list.Content, Node{Type: ListItem, Content: parseBlocks(item)})
				}
				out = append(out, list)
				continue
			}
			// a paragraph: the lines up to a blank line or the start of another block
			para := []string{l}
			i++
			for i < len(lines) && !isBlank(lines[i]) && !startsBlock(lines[i]) {
				para = append(para, lines[i])
				i++
			}
			if c := parseInline(strings.Join(para, "\n")); len(c) > 0 {
				out = append(out, Node{Type: Paragraph, Content: c})
			}
		}
	}
	return out
}

// splitRow splits "| a | b |" on the pipes that are not escaped, and takes one space off each side of a cell.
func splitRow(line string) []string {
	line = strings.TrimPrefix(line, "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) && line[i+1] == '|' { // only an escaped pipe is special: a backslash before another character belongs to the cell
			cur.WriteByte(line[i])
			cur.WriteByte(line[i+1])
			i++
			continue
		}
		if line[i] == '|' {
			cells = append(cells, trimOne(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(line[i])
	}
	if strings.TrimSpace(cur.String()) != "" {
		cells = append(cells, trimOne(cur.String()))
	}
	return cells
}

func trimOne(s string) string {
	s = strings.TrimPrefix(s, " ")
	return strings.TrimSuffix(s, " ")
}

func parseInlineCell(s string) []Node { return (&inl{cell: true}).parse(s) }

// inl is the inline parser; in a table cell a pipe in a code span is written \| and read back as |.
type inl struct{ cell bool }

func parseInline(s string) []Node { return (&inl{}).parse(s) }

func (p *inl) parse(s string) []Node {
	nodes, _, _ := p.until(s, 0, "", nil)
	return Normalize(Node{Type: Paragraph, Content: nodes}).Content
}

func isPunct(c byte) bool {
	return strings.IndexByte(punct, c) >= 0 || strings.IndexByte("#+-.() ", c) >= 0
}

// inlineUntil reads inline content from i until term (which is consumed), carrying the marks in force. It reports where it stopped and whether the
// terminator was found; with no terminator it reads to the end.
func (p *inl) until(s string, i int, term string, marks []Mark) ([]Node, int, bool) {
	var out []Node
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, Node{Type: Text, Text: lit.String(), Marks: append([]Mark(nil), marks...)})
			lit.Reset()
		}
	}
	for i < len(s) {
		if term != "" && strings.HasPrefix(s[i:], term) {
			flush()
			return out, i + len(term), true
		}
		c := s[i]
		switch {
		case p.cell && strings.HasPrefix(s[i:], "<br>"):
			flush()
			out = append(out, Node{Type: HardBreak})
			i += 4
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			flush()
			out = append(out, Node{Type: HardBreak})
			i += 2
		case c == '\\' && i+1 < len(s) && isPunct(s[i+1]):
			lit.WriteByte(s[i+1])
			i += 2
		case c == '`':
			n := 0
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			run := strings.Repeat("`", n)
			end := -1
			for j := i + n; j < len(s); j++ {
				if strings.HasPrefix(s[j:], run) && (j+n >= len(s) || s[j+n] != '`') && s[j-1] != '`' {
					end = j
					break
				}
			}
			if end < 0 {
				lit.WriteString(run)
				i += n
				break
			}
			flush()
			content := s[i+n : end]
			if p.cell {
				content = strings.ReplaceAll(content, "\\|", "|")
			}
			if len(content) >= 2 && content[0] == ' ' && content[len(content)-1] == ' ' {
				content = content[1 : len(content)-1]
			}
			out = append(out, Node{Type: Text, Text: content, Marks: append(append([]Mark(nil), marks...), Mark{Type: MCode})})
			i = end + n
		case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "~~"), c == '_':
			d := string(c)
			if c != '_' {
				d = s[i : i+2]
			}
			typ := map[string]string{"**": MStrong, "~~": MStrike, "_": MEm}[d]
			inner, next, ok := p.until(s, i+len(d), d, append(append([]Mark(nil), marks...), Mark{Type: typ}))
			if !ok {
				lit.WriteString(d)
				i += len(d)
				break
			}
			flush()
			out = append(out, inner...)
			i = next
		case c == '[' || (c == '!' && i+1 < len(s) && s[i+1] == '[') || (c == '@' && i+1 < len(s) && s[i+1] == '['):
			start := i
			if c != '[' {
				i++
			}
			inner, next, ok := p.until(s, i+1, "]", nil)
			if !ok || next >= len(s) || s[next] != '(' {
				lit.WriteString(s[start : start+1])
				i = start + 1
				break
			}
			href, after, ok := readHref(s, next+1)
			if !ok {
				lit.WriteString(s[start : start+1])
				i = start + 1
				break
			}
			flush()
			label := plain(inner)
			switch {
			case c == '@' && strings.HasPrefix(href, "user:"):
				out = append(out, Node{Type: Mention, Text: label, ID: strings.TrimPrefix(href, "user:")})
			case c == '!' && strings.HasPrefix(href, "attachment:"):
				out = append(out, Node{Type: Attachment, Text: label, ID: strings.TrimPrefix(href, "attachment:")})
			case c == '[' && strings.HasPrefix(href, "issue:"):
				out = append(out, Node{Type: IssueLink, Text: strings.TrimPrefix(href, "issue:")})
			case c == '[':
				for _, n := range inner {
					if n.Type == Text {
						n.Marks = append(append(append([]Mark(nil), marks...), Mark{Type: MLink, Href: href}), n.Marks...)
					}
					out = append(out, n)
				}
			default:
				lit.WriteString(s[start:after]) // an image or mention in a form this dialect does not know stays as text
			}
			i = after
		default:
			lit.WriteByte(c)
			i++
		}
	}
	flush()
	return out, i, term == ""
}

func plain(ns []Node) string {
	var b strings.Builder
	for _, n := range ns {
		b.WriteString(n.Text)
	}
	return b.String()
}

// readHref reads up to the closing parenthesis, honouring backslash escapes.
func readHref(s string, i int) (string, int, bool) {
	var b strings.Builder
	for ; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i++
		case s[i] == ')':
			return b.String(), i + 1, true
		default:
			b.WriteByte(s[i])
		}
	}
	return "", i, false
}
