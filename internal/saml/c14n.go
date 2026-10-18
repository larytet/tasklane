package saml

import (
	"sort"
	"strings"
)

// Exclusive XML canonicalization (RFC 3741, without comments) of an element and what is under it, as XML-DSig uses it. skip is an element left
// out (the enveloped-signature transform removes the signature that is being checked); inclusive lists the prefixes whose declarations are
// kept even when unused (the InclusiveNamespaces PrefixList).
func canonicalize(n *Node, skip *Node, inclusive []string) []byte {
	var b strings.Builder
	c14nElement(&b, n, skip, inclusive, map[string]string{})
	return []byte(b.String())
}

func c14nElement(b *strings.Builder, n *Node, skip *Node, inclusive []string, rendered map[string]string) {
	if n == skip {
		return
	}
	// the prefixes this element visibly uses: its own, and those of its namespaced attributes
	used := map[string]bool{n.Prefix: true}
	for _, a := range n.Attrs {
		if a.Prefix != "" && a.Prefix != "xml" {
			used[a.Prefix] = true
		}
	}
	for _, p := range inclusive {
		if p == "#default" {
			p = ""
		}
		used[p] = true
	}
	type decl struct{ prefix, uri string }
	var decls []decl
	for p := range used {
		uri, ok := n.lookupNS(p)
		prev, was := rendered[p]
		switch {
		case ok && uri != "":
			if !was || prev != uri {
				decls = append(decls, decl{p, uri})
			}
		case p == "" && was && prev != "": // the default namespace is undeclared here, and an output ancestor declared one
			decls = append(decls, decl{"", ""})
		}
	}
	sort.Slice(decls, func(i, j int) bool { return decls[i].prefix < decls[j].prefix })
	child := make(map[string]string, len(rendered)+len(decls))
	for k, v := range rendered {
		child[k] = v
	}
	b.WriteByte('<')
	writeName(b, n.Prefix, n.Local)
	for _, d := range decls {
		if d.prefix == "" {
			b.WriteString(` xmlns="`)
		} else {
			b.WriteString(" xmlns:" + d.prefix + `="`)
		}
		b.WriteString(escAttr(d.uri))
		b.WriteByte('"')
		child[d.prefix] = d.uri
	}
	attrs := append([]Attr(nil), n.Attrs...)
	sort.SliceStable(attrs, func(i, j int) bool {
		if attrs[i].NS != attrs[j].NS {
			return attrs[i].NS < attrs[j].NS
		}
		return attrs[i].Local < attrs[j].Local
	})
	for _, a := range attrs {
		b.WriteByte(' ')
		writeName(b, a.Prefix, a.Local)
		b.WriteString(`="` + escAttr(a.Value) + `"`)
	}
	b.WriteByte('>')
	for _, c := range n.Children {
		if c.IsText {
			b.WriteString(escText(c.Text))
		} else {
			c14nElement(b, c, skip, inclusive, child)
		}
	}
	b.WriteString("</")
	writeName(b, n.Prefix, n.Local)
	b.WriteByte('>')
}

func writeName(b *strings.Builder, prefix, local string) {
	if prefix != "" {
		b.WriteString(prefix + ":")
	}
	b.WriteString(local)
}

var (
	textEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#xD;")
	attrEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", `"`, "&quot;", "\t", "&#x9;", "\n", "&#xA;", "\r", "&#xD;")
)

func escText(s string) string { return textEsc.Replace(s) }
func escAttr(s string) string { return attrEsc.Replace(s) }
