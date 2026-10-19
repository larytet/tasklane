package richtext_test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/richtext"
)

// ---- a generator of documents in canonical form: what the editor can make and what a round trip must give back

type gen struct{ r *rand.Rand }

func (g gen) pick(s ...string) string { return s[g.r.Intn(len(s))] }

// words includes every character the Markdown dialect treats as special, so that escaping is exercised all the time.
func (g gen) word() string {
	pieces := []string{"alpha", "beta", "a_b", "x*y", "[z]", "(p)", "# h", "- d", "1. n", "2) m", "~~t", "`c`", "\\", "<br>", "|", "!", "@who", "---", "> q", "ünï", "日本", "tab\tx", "  ", " ", "a  b", "**not bold**", "_not em_", "~~not struck~~", "***", "``", "`", "[a](b)", "![i](attachment:x)", "@[m](user:u)", "[K-1](issue:K-1)"}
	n := 1 + g.r.Intn(3)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(pieces[g.r.Intn(len(pieces))])
		if g.r.Intn(2) == 0 {
			b.WriteString(" ")
		}
	}
	return b.String()
}

func (g gen) href() string {
	return "https://example.com/" + g.pick("a", "b c", "p(q)", "x\\y", "z)", "?q=1&r=2")
}

func (g gen) marks() []richtext.Mark {
	var ms []richtext.Mark
	if g.r.Intn(4) == 0 {
		ms = append(ms, richtext.Mark{Type: richtext.MLink, Href: g.href()})
	}
	for _, t := range []string{richtext.MStrong, richtext.MEm, richtext.MStrike, richtext.MCode} {
		if g.r.Intn(5) == 0 {
			ms = append(ms, richtext.Mark{Type: t})
		}
	}
	return ms
}

func (g gen) inlines(cell bool) []richtext.Node { return g.inl(cell, true) }

func (g gen) inl(cell, breaks bool) []richtext.Node {
	var out []richtext.Node
	for i, n := 0, 1+g.r.Intn(5); i < n; i++ {
		switch k := g.r.Intn(12); {
		case k < 8:
			out = append(out, richtext.Node{Type: richtext.Text, Text: g.word(), Marks: g.marks()})
		case k == 8 && breaks && i > 0 && i < n-1: // never first or last: a break at the end of a paragraph is nothing
			out = append(out, richtext.Node{Type: richtext.HardBreak})
		case k == 9:
			out = append(out, richtext.Node{Type: richtext.Mention, ID: g.pick("u1", "id with space", "a)b"), Text: g.word()})
		case k == 10:
			out = append(out, richtext.Node{Type: richtext.IssueLink, Text: fmt.Sprintf("%s-%d", g.pick("ABC", "XYZ", "QA"), 1+g.r.Intn(999))})
		default:
			out = append(out, richtext.Node{Type: richtext.Attachment, ID: g.pick("att1", "a b", "c(d)"), Text: g.pick("shot.png", "my [file].txt", "x")})
		}
	}
	// a paragraph must say something visible
	out = append(out, richtext.Node{Type: richtext.Text, Text: "end"})
	return richtext.Normalize(richtext.Node{Type: richtext.Paragraph, Content: out}).Content
}

func (g gen) block(depth int) richtext.Node {
	k := g.r.Intn(11)
	if depth >= 3 && k >= 4 && k <= 7 {
		k = 0
	}
	switch k {
	case 0, 1:
		return richtext.Node{Type: richtext.Paragraph, Content: g.inlines(false)}
	case 2:
		return richtext.Node{Type: richtext.Heading, Level: 1 + g.r.Intn(6), Content: g.inl(false, false)} // a heading is one line
	case 3:
		return richtext.Node{Type: richtext.Rule}
	case 4, 5:
		typ, start := richtext.BulletList, 0
		if k == 5 {
			typ, start = richtext.OrderedList, 1+g.r.Intn(120)
		}
		l := richtext.Node{Type: typ, Start: start}
		for i, n := 0, 1+g.r.Intn(3); i < n; i++ {
			item := richtext.Node{Type: richtext.ListItem}
			for j, m := 0, 1+g.r.Intn(2); j < m; j++ {
				item.Content = append(item.Content, g.block(depth+1))
			}
			l.Content = append(l.Content, item)
		}
		return l
	case 6:
		q := richtext.Node{Type: richtext.Quote}
		for j, m := 0, 1+g.r.Intn(2); j < m; j++ {
			q.Content = append(q.Content, g.block(depth+1))
		}
		return q
	case 7:
		t := richtext.Node{Type: richtext.Table}
		cols := 1 + g.r.Intn(3)
		for ri, rows := 0, 2+g.r.Intn(2); ri < rows; ri++ {
			row := richtext.Node{Type: richtext.Row}
			for c := 0; c < cols; c++ {
				typ := richtext.Cell
				if ri == 0 {
					typ = richtext.HeaderCell
				}
				row.Content = append(row.Content, richtext.Node{Type: typ, Content: []richtext.Node{{Type: richtext.Paragraph, Content: g.inlines(true)}}})
			}
			t.Content = append(t.Content, row)
		}
		return t
	case 8, 9:
		lines := []string{"func main() {", "", "  fmt.Println(\"```\")", "}", "", ""}
		text := strings.Join(lines[:1+g.r.Intn(len(lines))], "\n")
		if g.r.Intn(4) == 0 {
			text = ""
		}
		return richtext.Node{Type: richtext.CodeBlock, Lang: g.pick("", "go", "js"), Text: text}
	}
	raw, _ := json.Marshal(map[string]any{"type": "panel", "attrs": map[string]any{"panelType": "info"}, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "kept as it was"}}}}})
	return richtext.Node{Type: richtext.Opaque, Raw: string(raw)}
}

func (g gen) doc() richtext.Node {
	d := richtext.Node{Type: richtext.Doc}
	for i, n := 0, 1+g.r.Intn(5); i < n; i++ {
		d.Content = append(d.Content, g.block(0))
	}
	return richtext.Normalize(d)
}

func dump(n richtext.Node) string { b, _ := json.MarshalIndent(n, "", " "); return string(b) }

// diffPath names the first node at which two documents differ, with both nodes in short form.
func diffPath(a, b richtext.Node, path string) string {
	if a.Type != b.Type || a.Text != b.Text || a.Level != b.Level || a.Start != b.Start || a.Lang != b.Lang || a.ID != b.ID || a.Raw != b.Raw || fmt.Sprint(a.Marks) != fmt.Sprint(b.Marks) {
		return fmt.Sprintf("%s: %s %q %v  !=  %s %q %v", path, a.Type, a.Text, a.Marks, b.Type, b.Text, b.Marks)
	}
	for i := 0; i < len(a.Content) && i < len(b.Content); i++ {
		if d := diffPath(a.Content[i], b.Content[i], fmt.Sprintf("%s/%s[%d]", path, a.Type, i)); d != "" {
			return d
		}
	}
	if len(a.Content) != len(b.Content) {
		return fmt.Sprintf("%s/%s: %d children != %d", path, a.Type, len(a.Content), len(b.Content))
	}
	return ""
}

// firstDiff shows the first place two dumps differ, with a little context.
func firstDiff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			lo := i - 6
			if lo < 0 {
				lo = 0
			}
			hi := i + 3
			if hi > len(x) {
				hi = len(x)
			}
			if hi > len(y) {
				hi = len(y)
			}
			return "before:\n" + strings.Join(x[lo:hi], "\n") + "\nafter:\n" + strings.Join(y[lo:hi], "\n")
		}
	}
	return "one is a prefix of the other"
}

// LIVE-06 (Markdown): any document comes back from its Markdown as it was.
func TestLIVE06_MarkdownRoundTripProperty(t *testing.T) {
	g := gen{rand.New(rand.NewSource(3))}
	for i := 0; i < 3000; i++ {
		d := g.doc()
		md := richtext.ToMarkdown(d)
		back := richtext.FromMarkdown(md)
		if !richtext.Equal(d, back) {
			t.Fatalf("round %d lost something at %s\nmarkdown:\n%s", i, diffPath(richtext.Normalize(d), back, ""), md)
		}
		// and the Markdown itself is stable
		if again := richtext.ToMarkdown(back); again != md {
			t.Fatalf("round %d: the Markdown changed on a second pass\n%s\n---\n%s", i, md, again)
		}
	}
}

// LIVE-06 (ADF): any document comes back from ADF, through JSON as Jira would send it.
func TestLIVE06_ADFRoundTripProperty(t *testing.T) {
	g := gen{rand.New(rand.NewSource(2))}
	for i := 0; i < 3000; i++ {
		d := g.doc()
		b, err := json.Marshal(richtext.ToADF(d))
		if err != nil {
			t.Fatal(err)
		}
		var v any
		json.Unmarshal(b, &v)
		back := richtext.FromADF(v)
		if !richtext.Equal(d, back) {
			t.Fatalf("round %d lost something\nadf: %s\n%s", i, b, firstDiff(dump(richtext.Normalize(d)), dump(back)))
		}
	}
}

// and across: Markdown to ADF to Markdown, and the other way, with nothing changing on the way
func TestLIVE06_MarkdownAndADFAgree(t *testing.T) {
	g := gen{rand.New(rand.NewSource(3))}
	for i := 0; i < 1000; i++ {
		d := g.doc()
		viaADF := richtext.FromADF(jsonRound(t, richtext.ToADF(richtext.FromMarkdown(richtext.ToMarkdown(d)))))
		if !richtext.Equal(d, viaADF) {
			t.Fatalf("round %d\n%s", i, firstDiff(dump(richtext.Normalize(d)), dump(viaADF)))
		}
	}
}

func jsonRound(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

// A document as Jira sends it, with things the model has no place for: what it can hold is held, the rest is kept exactly as an opaque block, and
// writing it back as ADF gives the same document.
func TestLIVE06_JiraDocumentWithUnsupportedNodesIsKeptWhole(t *testing.T) {
	const jira = `{"version":1,"type":"doc","content":[
	  {"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Steps"}]},
	  {"type":"paragraph","content":[{"type":"text","text":"Use "},{"type":"text","text":"the test card","marks":[{"type":"strong"}]},{"type":"text","text":" and see "},{"type":"text","text":"the runbook","marks":[{"type":"link","attrs":{"href":"https://example.com/runbook"}}]}]},
	  {"type":"paragraph","content":[{"type":"text","text":"red text","marks":[{"type":"textColor","attrs":{"color":"#ff0000"}}]},{"type":"emoji","attrs":{"shortName":":smile:"}}]},
	  {"type":"orderedList","attrs":{"order":3},"content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"third"}]}]}]},
	  {"type":"panel","attrs":{"panelType":"warning"},"content":[{"type":"paragraph","content":[{"type":"text","text":"careful"}]}]},
	  {"type":"table","content":[{"type":"tableRow","content":[{"type":"tableHeader","attrs":{"colspan":2},"content":[{"type":"paragraph","content":[{"type":"text","text":"merged"}]}]}]}]},
	  {"type":"codeBlock","attrs":{"language":"sql"},"content":[{"type":"text","text":"select 1;"}]},
	  {"type":"paragraph"},
	  {"type":"mediaSingle","content":[{"type":"media","attrs":{"id":"x","type":"file","collection":"jira"}}]}
	]}`
	var in any
	if err := json.Unmarshal([]byte(jira), &in); err != nil {
		t.Fatal(err)
	}
	d := richtext.FromADF(in)
	var kinds []string
	opaque := 0
	for _, n := range d.Content {
		kinds = append(kinds, n.Type)
		if n.Type == richtext.Opaque {
			opaque++
		}
	}
	// heading, paragraph, [colour+emoji paragraph opaque], list, [panel opaque], [merged table opaque], code, [media opaque]; the empty paragraph is gone
	if got := strings.Join(kinds, ","); got != "heading,paragraph,opaque,orderedList,opaque,opaque,codeBlock,opaque" {
		t.Fatalf("%s", got)
	}
	if d.Content[3].Start != 3 || d.Content[6].Lang != "sql" {
		t.Fatalf("%+v %+v", d.Content[3], d.Content[6])
	}
	out := jsonRound(t, richtext.ToADF(d)).(map[string]any)["content"].([]any)
	src := in.(map[string]any)["content"].([]any)
	// every block that is kept comes back identical; the empty paragraph is the only one not written
	var want []any
	for _, b := range src {
		if m := b.(map[string]any); m["type"] == "paragraph" && m["content"] == nil {
			continue
		}
		want = append(want, b)
	}
	if len(out) != len(want) {
		t.Fatalf("%d blocks out, %d in", len(out), len(want))
	}
	for i := range want {
		a, _ := json.Marshal(want[i])
		b, _ := json.Marshal(out[i])
		if string(a) != string(b) {
			t.Fatalf("block %d changed:\n%s\n%s", i, a, b)
		}
	}
	// and through Markdown the opaque blocks survive too
	if back := richtext.FromMarkdown(richtext.ToMarkdown(d)); !richtext.Equal(d, back) {
		t.Fatalf("through Markdown:\n%s\n%s", dump(d), dump(back))
	}
}

// Ordinary Markdown that people paste is understood well enough.
func TestLIVE06_OrdinaryMarkdownIsRead(t *testing.T) {
	d := richtext.FromMarkdown("# Title\n\nSome **bold** and _italic_ with `code` and a [link](https://example.com).\n\n- one\n- two\n  - nested\n\n1. first\n2. second\n\n> quoted\n\n```go\nx := 1\n```\n\n| a | b |\n| --- | --- |\n| 1 | 2 |\n")
	var kinds []string
	for _, n := range d.Content {
		kinds = append(kinds, n.Type)
	}
	if got := strings.Join(kinds, ","); got != "heading,paragraph,bulletList,orderedList,blockquote,codeBlock,table" {
		t.Fatalf("%s\n%s", got, dump(d))
	}
	if bl := d.Content[2]; len(bl.Content) != 2 || bl.Content[1].Content[1].Type != richtext.BulletList {
		t.Fatalf("%s", dump(bl))
	}
}

// a heading with a break in it cannot be written as one line of Markdown: it is kept whole instead of being damaged
func TestLIVE06_HeadingWithABreakIsKeptWhole(t *testing.T) {
	const adf = `{"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"a"},{"type":"hardBreak"},{"type":"text","text":"b"}]}]}`
	var v any
	json.Unmarshal([]byte(adf), &v)
	d := richtext.FromADF(v)
	if len(d.Content) != 1 || d.Content[0].Type != richtext.Opaque {
		t.Fatalf("%s", dump(d))
	}
	if back := richtext.FromMarkdown(richtext.ToMarkdown(d)); !richtext.Equal(d, back) {
		t.Fatal("the opaque heading did not survive Markdown")
	}
}
