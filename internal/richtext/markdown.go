package richtext

import (
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- Markdown out

// ToMarkdown writes a document in the dialect described in the package comment.
func ToMarkdown(d Node) string { return renderBlocks(d.Content) }

func renderBlocks(ns []Node) string {
	var parts []string
	for _, n := range ns {
		parts = append(parts, renderBlock(n))
	}
	return strings.Join(parts, "\n\n")
}

func indentLines(s string, pad string, first string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		p := pad
		if i == 0 {
			p = first
		}
		if l == "" && i > 0 {
			continue // blank lines stay blank
		}
		lines[i] = p + l
	}
	return strings.Join(lines, "\n")
}

func renderBlock(n Node) string {
	switch n.Type {
	case Paragraph:
		return renderInlines(n.Content, false)
	case Heading:
		lv := n.Level
		if lv < 1 {
			lv = 1
		}
		if lv > 6 {
			lv = 6
		}
		return strings.Repeat("#", lv) + " " + renderInlines(n.Content, false)
	case BulletList:
		var items []string
		for _, it := range n.Content {
			items = append(items, indentLines(renderBlocks(it.Content), "  ", "- "))
		}
		return strings.Join(items, "\n")
	case OrderedList:
		start := n.Start
		if start < 1 {
			start = 1
		}
		var items []string
		for i, it := range n.Content {
			marker := strconv.Itoa(start+i) + ". "
			items = append(items, indentLines(renderBlocks(it.Content), strings.Repeat(" ", len(marker)), marker))
		}
		return strings.Join(items, "\n")
	case Quote:
		lines := strings.Split(renderBlocks(n.Content), "\n")
		for i, l := range lines {
			if l == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + l
			}
		}
		return strings.Join(lines, "\n")
	case CodeBlock:
		return fence(n.Lang, n.Text)
	case Rule:
		return "---"
	case Table:
		var b strings.Builder
		for ri, row := range n.Content {
			b.WriteString("|")
			for _, c := range row.Content {
				cell := ""
				if len(c.Content) == 1 {
					cell = renderCell(c.Content[0].Content)
				}
				b.WriteString(" " + cell + " |")
			}
			b.WriteString("\n")
			if ri == 0 {
				b.WriteString("|")
				for range row.Content {
					b.WriteString(" --- |")
				}
				b.WriteString("\n")
			}
		}
		return strings.TrimRight(b.String(), "\n")
	case Opaque:
		return fence("adf-opaque", n.Raw)
	}
	return ""
}

func renderCell(ns []Node) string { return renderInlines(ns, true) }

func fence(lang, text string) string {
	run := 3
	for _, l := range strings.Split(text, "\n") {
		if n := longestRun(l, '`'); n >= run {
			run = n + 1
		}
	}
	f := strings.Repeat("`", run)
	return f + lang + "\n" + text + "\n" + f
}

func longestRun(s string, c byte) (best int) {
	cur := 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return
}

const punct = "\\`*_[]<>|~!@"

func escapeText(s string, lineStart bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case strings.IndexByte(punct, c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
		case i == 0 && lineStart && (c == '#' || c == '+' || c == '-' || c == ' '): // a leading space would read as the indentation of a list item
			b.WriteByte('\\')
			b.WriteByte(c)
		case lineStart && isOrderedStart(s[:i], c):
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isOrderedStart: c is the dot or parenthesis after a run of digits that began the line.
func isOrderedStart(before string, c byte) bool {
	if (c != '.' && c != ')') || before == "" {
		return false
	}
	for i := 0; i < len(before); i++ {
		if before[i] < '0' || before[i] > '9' {
			return false
		}
	}
	return true
}

func escapeHref(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '(', ')', ' ':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func sortedMarks(ms []Mark) []Mark {
	out := append([]Mark(nil), ms...)
	sort.SliceStable(out, func(i, j int) bool { return markRank(out[i].Type) < markRank(out[j].Type) })
	return out
}

func markRank(t string) int {
	for i, m := range markOrder {
		if m == t {
			return i
		}
	}
	return len(markOrder)
}

func open(m Mark) string {
	switch m.Type {
	case MLink:
		return "["
	case MStrong:
		return "**"
	case MEm:
		return "_"
	case MStrike:
		return "~~"
	}
	return ""
}

func closing(m Mark) string {
	switch m.Type {
	case MLink:
		return "](" + escapeHref(m.Href) + ")"
	case MStrong:
		return "**"
	case MEm:
		return "_"
	case MStrike:
		return "~~"
	}
	return ""
}

func renderInlines(ns []Node, cell bool) string {
	var b strings.Builder
	var stack []Mark // the marks that are open, outermost first (code is never open: a code span is written whole)
	lineStart := true
	closeTo := func(keep int) {
		for len(stack) > keep {
			b.WriteString(closing(stack[len(stack)-1]))
			stack = stack[:len(stack)-1]
		}
	}
	for _, n := range ns {
		var want []Mark
		code := false
		if n.Type == Text {
			for _, m := range sortedMarks(n.Marks) {
				if m.Type == MCode {
					code = true
				} else {
					want = append(want, m)
				}
			}
		}
		common := 0
		for common < len(stack) && common < len(want) && stack[common] == want[common] {
			common++
		}
		closeTo(common)
		for _, m := range want[common:] {
			b.WriteString(open(m))
			stack = append(stack, m)
		}
		switch n.Type {
		case Text:
			if code {
				span := codeSpan(n.Text)
				if cell {
					span = strings.ReplaceAll(span, "|", "\\|")
				}
				b.WriteString(span)
			} else {
				b.WriteString(escapeText(n.Text, lineStart))
			}
			lineStart = false
		case HardBreak:
			closeTo(0)
			if cell {
				b.WriteString("<br>")
			} else {
				b.WriteString("\\\n")
			}
			lineStart = true
		case Mention:
			closeTo(0)
			b.WriteString("@[" + escapeText(n.Text, false) + "](user:" + escapeHref(n.ID) + ")")
			lineStart = false
		case IssueLink:
			closeTo(0)
			b.WriteString("[" + escapeText(n.Text, false) + "](issue:" + escapeHref(n.Text) + ")")
			lineStart = false
		case Attachment:
			closeTo(0)
			b.WriteString("![" + escapeText(n.Text, false) + "](attachment:" + escapeHref(n.ID) + ")")
			lineStart = false
		}
	}
	closeTo(0)
	return b.String()
}

func codeSpan(s string) string {
	run := longestRun(s, '`') + 1
	d := strings.Repeat("`", run)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") || strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") {
		return d + " " + s + " " + d
	}
	return d + s + d
}
