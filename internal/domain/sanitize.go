package domain

import (
	"strings"
	"unicode"
)

const maxTextBytes = 1 << 20 // 1 MiB per text field

func checkSize(s string) error {
	if len(s) > maxTextBytes {
		return invalid("text is too large (limit %d bytes)", maxTextBytes)
	}
	return nil
}

// Sanitize removes control characters and markdown links whose scheme is not http, https or
// mailto (SPEC 4.5, validation on save). It returns the cleaned text and a report of what was removed
// so the saver can be told; nothing is dropped silently.
func Sanitize(s string) (string, []string) { return sanitize(s, schemeOK) }

// dialectSchemeOK is schemeOK plus the three schemes of Tasklane's Markdown dialect (internal/richtext): a mention is
// @[Name](user:ID), an issue link [KEY](issue:KEY) and an attachment ![name](attachment:ID). The parser turns each into a node of its
// own, never a link, so none of them can carry a script; text that is Markdown for Jira (SPEC 18.1.1, WT-12) is cleaned with this.
func dialectSchemeOK(dest string) bool {
	u := strings.ToLower(strings.TrimSpace(dest))
	return schemeOK(dest) || strings.HasPrefix(u, "user:") || strings.HasPrefix(u, "issue:") || strings.HasPrefix(u, "attachment:")
}

// SanitizeDialect is Sanitize for text in the Markdown dialect: mentions, issue links and attachments are kept.
func SanitizeDialect(s string) (string, []string) { return sanitize(s, dialectSchemeOK) }

func sanitize(s string, ok func(string) bool) (string, []string) {
	var removed []string
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	dropped := 0
	for _, r := range s {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		} else {
			dropped++
		}
	}
	if dropped > 0 {
		removed = append(removed, "control characters")
	}
	s = strings.ReplaceAll(b.String(), "\r\n", "\n")
	s = stripBadLinks(s, &removed, ok)
	return s, removed
}

func schemeOK(dest string) bool {
	u := strings.ToLower(strings.TrimSpace(dest))
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "mailto:") ||
		strings.HasPrefix(u, "/") || strings.HasPrefix(u, "#")
}

// stripBadLinks scans markdown links [text](dest) with balanced parentheses in dest, so
// "javascript:alert(1)" cannot hide its tail from the scanner.
func stripBadLinks(s string, removed *[]string, ok func(string) bool) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		open := strings.Index(s[i:], "](")
		if open < 0 {
			break
		}
		open += i
		lb := strings.LastIndex(s[i:open], "[")
		if lb < 0 {
			out.WriteString(s[i : open+2])
			i = open + 2
			continue
		}
		lb += i
		depth, j := 1, open+2
		for j < len(s) && depth > 0 {
			switch s[j] {
			case '(':
				depth++
			case ')':
				depth--
			}
			j++
		}
		if depth != 0 { // unterminated: leave as plain text
			out.WriteString(s[i : open+2])
			i = open + 2
			continue
		}
		dest := s[open+2 : j-1]
		if f := strings.Fields(dest); len(f) > 0 {
			dest = f[0]
		}
		if ok(dest) {
			out.WriteString(s[i:j])
		} else {
			out.WriteString(s[i:lb])
			out.WriteString(s[lb+1 : open])
			*removed = append(*removed, "link to "+dest)
		}
		i = j
	}
	out.WriteString(s[i:])
	return out.String()
}
