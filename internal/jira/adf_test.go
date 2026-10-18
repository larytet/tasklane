package jira_test

import (
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/jira"
)

type m = map[string]any

func doc(content ...any) m  { return m{"type": "doc", "version": 1, "content": content} }
func para(content ...any) m { return m{"type": "paragraph", "content": content} }
func txt(s string, marks ...any) m {
	n := m{"type": "text", "text": s}
	if len(marks) > 0 {
		n["marks"] = marks
	}
	return n
}

// MIR-05: supported nodes read as text, unsupported ones are marked and counted, hostile content stays inert.
func TestMIR05_ADFNodes(t *testing.T) {
	d := doc(
		m{"type": "heading", "attrs": m{"level": 2}, "content": []any{txt("Title")}},
		para(txt("see "), txt("the spec", m{"type": "link", "attrs": m{"href": "https://example.com/a?b=c"}}), txt(" and "), m{"type": "mention", "attrs": m{"id": "acc-1", "text": "@Ann"}}, txt("!")),
		m{"type": "orderedList", "content": []any{
			m{"type": "listItem", "content": []any{para(txt("one"))}},
			m{"type": "listItem", "content": []any{para(txt("two")), m{"type": "bulletList", "content": []any{m{"type": "listItem", "content": []any{para(txt("nested"))}}}}}},
		}},
		m{"type": "blockquote", "content": []any{para(txt("quoted"))}},
		m{"type": "codeBlock", "attrs": m{"language": "go"}, "content": []any{txt("x := 1\ny := 2")}},
		m{"type": "table", "content": []any{
			m{"type": "tableRow", "content": []any{m{"type": "tableHeader", "content": []any{para(txt("A"))}}, m{"type": "tableHeader", "content": []any{para(txt("B"))}}}},
			m{"type": "tableRow", "content": []any{m{"type": "tableCell", "content": []any{para(txt("1"))}}, m{"type": "tableCell", "content": []any{para(txt("2"))}}}},
		}},
		m{"type": "panel", "attrs": m{"panelType": "info"}, "content": []any{para(txt("careful"))}},
		m{"type": "rule"},
		m{"type": "mediaSingle", "content": []any{m{"type": "media", "attrs": m{"id": "x", "type": "file"}}}},
		para(m{"type": "emoji", "attrs": m{"shortName": ":tada:", "text": "🎉"}}, m{"type": "hardBreak"}, m{"type": "status", "attrs": m{"text": "DONE"}}),
		m{"type": "somethingNew", "attrs": m{"x": 1}},
		para(m{"type": "inlineThing", "content": []any{}}),
	)
	text, unsup := jira.ADFText(d)
	for _, want := range []string{"Title", "see [the spec](https://example.com/a?b=c) and @Ann!", "1. one", "2. two", "  - nested", "> quoted", "```\nx := 1\ny := 2\n```", "A | B", "1 | 2", "careful", "---", "[attachment]", "🎉", "[DONE]"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if unsup != 2 || !strings.Contains(text, "[unsupported content: somethingNew]") || !strings.Contains(text, "[unsupported content: inlineThing]") {
		t.Fatalf("%d unsupported:\n%s", unsup, text)
	}
}

func TestMIR05_HostileContentStaysInert(t *testing.T) {
	d := doc(para(
		txt("click ", m{"type": "strong"}),
		txt("here", m{"type": "link", "attrs": m{"href": "javascript:alert(document.cookie)"}}),
		txt(" or "), txt("there", m{"type": "link", "attrs": m{"href": "data:text/html;base64,PHNjcmlwdD4="}}),
		txt(" <script>alert(1)</script> <img src=x onerror=alert(2)>"),
	), m{"type": "blockCard", "attrs": m{"url": "javascript:evil()"}})
	text, _ := jira.ADFText(d)
	if strings.Contains(text, "javascript:") || strings.Contains(text, "data:text") {
		t.Fatalf("a dangerous link target survived: %q", text)
	}
	if !strings.Contains(text, "click here or there") { // the words stay, the targets go
		t.Fatalf("%q", text)
	}
	if !strings.Contains(text, "<script>alert(1)</script>") { // markup is kept as inert text: the UI renders text, never HTML
		t.Fatalf("%q", text)
	}
	// nothing breaks on garbage
	for _, v := range []any{nil, "string", 42, m{}, m{"type": "doc"}, m{"type": "doc", "content": "no"}, []any{1, 2}, m{"type": "doc", "content": []any{nil, 3, "x", m{"type": 5}}}} {
		jira.ADFText(v)
	}
	// deeply nested lists do not overflow the stack
	var deep any = para(txt("leaf"))
	for i := 0; i < 400; i++ {
		deep = m{"type": "bulletList", "content": []any{m{"type": "listItem", "content": []any{deep}}}}
	}
	if text, _ := jira.ADFText(doc(deep)); !strings.Contains(text, "leaf") {
		t.Fatal("lost the leaf")
	}
}
