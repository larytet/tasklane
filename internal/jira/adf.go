// Package jira reads a Jira Cloud site for the one-way mirror (SPEC 18.1): a REST client that honours rate limits and the
// egress policy, and a reader for Atlassian Document Format that turns it into searchable text without dropping anything
// (the original document is kept alongside).
package jira

import (
	"fmt"
	"net/url"
	"strings"
)

// ADFText extracts plain text from an Atlassian Document Format value and counts nodes it does not understand. Unsupported
// nodes appear as "[unsupported content: <type>]" so a reader can see something was there; the caller keeps the original
// document, so nothing is lost. Link marks keep only http, https and mailto targets.
func ADFText(doc any) (text string, unsupported int) {
	var b strings.Builder
	walkBlock(&b, doc, 0, &unsupported)
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimRight(b.String(), "\n"), "\n\n\n", "\n\n")), unsupported
}

func node(v any) map[string]any { m, _ := v.(map[string]any); return m }
func kids(n map[string]any) []any {
	k, _ := n["content"].([]any)
	return k
}
func attr(n map[string]any, k string) string {
	a := node(n["attrs"])
	if a == nil {
		return ""
	}
	if s, ok := a[k].(string); ok {
		return s
	}
	if f, ok := a[k].(float64); ok {
		return fmt.Sprint(int(f))
	}
	return ""
}

func safeHref(h string) string {
	u, err := url.Parse(strings.TrimSpace(h))
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
		return u.String()
	}
	return ""
}

func inline(n map[string]any, unsup *int) string {
	switch n["type"] {
	case "text":
		t, _ := n["text"].(string)
		if marks, _ := n["marks"].([]any); marks != nil {
			for _, m := range marks {
				mm := node(m)
				if mm["type"] == "link" {
					if h := safeHref(attr(mm, "href")); h != "" && t != "" {
						return "[" + t + "](" + h + ")"
					}
				}
			}
		}
		return t
	case "hardBreak":
		return "\n"
	case "mention":
		if t := attr(n, "text"); t != "" {
			return t
		}
		return "@" + attr(n, "id")
	case "emoji":
		if t := attr(n, "text"); t != "" {
			return t
		}
		return attr(n, "shortName")
	case "status":
		return "[" + attr(n, "text") + "]"
	case "date":
		return attr(n, "timestamp")
	case "inlineCard":
		return safeHref(attr(n, "url"))
	case "media", "mediaInline":
		return "[attachment]"
	}
	*unsup++
	return "[unsupported content: " + fmt.Sprint(n["type"]) + "]"
}

func inlines(n map[string]any, unsup *int) string {
	var b strings.Builder
	for _, c := range kids(n) {
		if cn := node(c); cn != nil {
			b.WriteString(inline(cn, unsup))
		}
	}
	return b.String()
}

func walkBlock(b *strings.Builder, v any, depth int, unsup *int) {
	n := node(v)
	if n == nil {
		return
	}
	switch n["type"] {
	case "doc", "panel", "expand", "nestedExpand", "tableCell", "tableHeader", "mediaSingle", "mediaGroup", "layoutSection", "layoutColumn":
		if t := attr(n, "title"); t != "" {
			b.WriteString(t + "\n\n")
		}
		for _, c := range kids(n) {
			walkBlock(b, c, depth, unsup)
		}
	case "paragraph", "heading":
		if s := inlines(n, unsup); strings.TrimSpace(s) != "" {
			b.WriteString(s + "\n\n")
		}
	case "bulletList", "orderedList":
		for i, li := range kids(n) {
			marker := "- "
			if n["type"] == "orderedList" {
				marker = fmt.Sprintf("%d. ", i+1)
			}
			var item strings.Builder
			for _, c := range kids(node(li)) {
				walkBlock(&item, c, depth+1, unsup)
			}
			lines := strings.Split(strings.TrimSpace(item.String()), "\n")
			b.WriteString(strings.Repeat("  ", depth) + marker + lines[0] + "\n")
			for _, l := range lines[1:] {
				if strings.TrimSpace(l) != "" {
					b.WriteString(strings.Repeat("  ", depth) + "  " + l + "\n")
				}
			}
		}
		b.WriteString("\n")
	case "codeBlock":
		var code strings.Builder
		for _, c := range kids(n) {
			if cn := node(c); cn != nil {
				t, _ := cn["text"].(string)
				code.WriteString(t)
			}
		}
		b.WriteString("```\n" + code.String() + "\n```\n\n")
	case "blockquote":
		var q strings.Builder
		for _, c := range kids(n) {
			walkBlock(&q, c, depth, unsup)
		}
		for _, l := range strings.Split(strings.TrimSpace(q.String()), "\n") {
			b.WriteString("> " + l + "\n")
		}
		b.WriteString("\n")
	case "rule":
		b.WriteString("---\n\n")
	case "table":
		for _, row := range kids(n) {
			var cells []string
			for _, cell := range kids(node(row)) {
				var cs strings.Builder
				for _, c := range kids(node(cell)) {
					walkBlock(&cs, c, depth, unsup)
				}
				cells = append(cells, strings.Join(strings.Fields(cs.String()), " "))
			}
			b.WriteString(strings.Join(cells, " | ") + "\n")
		}
		b.WriteString("\n")
	case "blockCard", "embedCard":
		if h := safeHref(attr(n, "url")); h != "" {
			b.WriteString(h + "\n\n")
		}
	case "media":
		b.WriteString("[attachment]\n\n")
	case "text", "hardBreak", "mention", "emoji", "status", "date", "inlineCard", "mediaInline":
		b.WriteString(inline(n, unsup) + "\n\n")
	default:
		*unsup++
		b.WriteString("[unsupported content: " + fmt.Sprint(n["type"]) + "]\n\n")
	}
}
