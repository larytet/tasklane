// Package saml is a small, strict SAML 2.0 service provider: SP-initiated Web SSO (AuthnRequest by redirect, Response by POST), assertions
// signed with RSA (XML-DSig, exclusive canonicalization), standard library only. It accepts only what it can check completely and refuses
// the rest (encrypted assertions, SHA-1, comments in canonicalization, unsolicited responses, several assertions), because a SAML library
// that tries to understand everything is where signature-wrapping attacks live.
package saml

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

const (
	nsDS   = "http://www.w3.org/2000/09/xmldsig#"
	nsSAML = "urn:oasis:names:tc:SAML:2.0:assertion"
	nsP    = "urn:oasis:names:tc:SAML:2.0:protocol"
	nsMD   = "urn:oasis:names:tc:SAML:2.0:metadata"
	nsXML  = "http://www.w3.org/XML/1998/namespace"

	maxDepth = 64
	maxNodes = 20000
	MaxSize  = 1 << 20
)

type Attr struct{ Prefix, Local, NS, Value string }

// Node is an element (or a run of text). Names keep the prefix as written, because canonicalization needs it, and carry the resolved namespace.
type Node struct {
	Prefix, Local, NS string
	Attrs             []Attr
	Decls             map[string]string // namespace declarations made on this element: prefix ("" is the default) to URI
	Children          []*Node
	Text              string
	IsText            bool
	Parent            *Node
}

func (n *Node) is(ns, local string) bool {
	return n != nil && !n.IsText && n.NS == ns && n.Local == local
}

func (n *Node) attr(local string) (string, bool) {
	for _, a := range n.Attrs {
		if a.NS == "" && a.Prefix == "" && a.Local == local {
			return a.Value, true
		}
	}
	return "", false
}

// child returns the only child element with the name, an error when there are several, or nil when there is none.
func (n *Node) child(ns, local string) (*Node, error) {
	var found *Node
	for _, c := range n.Children {
		if c.is(ns, local) {
			if found != nil {
				return nil, fmt.Errorf("more than one %s element", local)
			}
			found = c
		}
	}
	return found, nil
}

func (n *Node) children(ns, local string) []*Node {
	var out []*Node
	for _, c := range n.Children {
		if c.is(ns, local) {
			out = append(out, c)
		}
	}
	return out
}

// text is the concatenated character data of the element's own text children, trimmed.
func (n *Node) text() string {
	var b strings.Builder
	for _, c := range n.Children {
		if c.IsText {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

func (n *Node) lookupNS(prefix string) (string, bool) {
	for x := n; x != nil; x = x.Parent {
		if v, ok := x.Decls[prefix]; ok {
			return v, true
		}
	}
	if prefix == "xml" {
		return nsXML, true
	}
	return "", false
}

// Parse reads a document. It refuses a DOCTYPE or any other directive (no entities are ever defined, expanded or fetched), processing
// instructions other than the XML declaration, unmatched tags, undeclared prefixes, and documents that are too deep or too large.
func Parse(data []byte) (*Node, error) {
	if len(data) > MaxSize {
		return nil, fmt.Errorf("the document is larger than %d bytes", MaxSize)
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	dec.Entity = nil
	dec.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		if strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "utf8") {
			return in, nil
		}
		return nil, fmt.Errorf("the document is not UTF-8")
	}
	var root, cur *Node
	nodes := 0
	for {
		tok, err := dec.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.Directive:
			return nil, fmt.Errorf("a document type declaration is not accepted")
		case xml.ProcInst:
			if t.Target != "xml" {
				return nil, fmt.Errorf("a processing instruction is not accepted")
			}
		case xml.Comment:
			// dropped: signatures here never include comments
		case xml.StartElement:
			if nodes++; nodes > maxNodes {
				return nil, fmt.Errorf("the document has too many elements")
			}
			n := &Node{Prefix: t.Name.Space, Local: t.Name.Local, Parent: cur, Decls: map[string]string{}}
			for _, a := range t.Attr {
				switch {
				case a.Name.Space == "" && a.Name.Local == "xmlns":
					n.Decls[""] = a.Value
				case a.Name.Space == "xmlns":
					n.Decls[a.Name.Local] = a.Value
				default:
					n.Attrs = append(n.Attrs, Attr{Prefix: a.Name.Space, Local: a.Name.Local, Value: a.Value})
				}
			}
			ns, ok := n.lookupNS(n.Prefix)
			if n.Prefix != "" && !ok {
				return nil, fmt.Errorf("the prefix %q is not declared", n.Prefix)
			}
			n.NS = ns
			for i := range n.Attrs {
				if p := n.Attrs[i].Prefix; p != "" {
					u, ok := n.lookupNS(p)
					if !ok {
						return nil, fmt.Errorf("the prefix %q is not declared", p)
					}
					n.Attrs[i].NS = u
				}
			}
			if cur == nil {
				if root != nil {
					return nil, fmt.Errorf("more than one root element")
				}
				root = n
			} else {
				cur.Children = append(cur.Children, n)
			}
			cur = n
			depth := 0
			for x := cur; x != nil; x = x.Parent {
				depth++
			}
			if depth > maxDepth {
				return nil, fmt.Errorf("the document is nested too deeply")
			}
		case xml.EndElement:
			if cur == nil || cur.Prefix != t.Name.Space || cur.Local != t.Name.Local {
				return nil, fmt.Errorf("mismatched end tag %s", t.Name.Local)
			}
			cur = cur.Parent
		case xml.CharData:
			if cur == nil {
				if len(bytes.TrimSpace(t)) > 0 {
					return nil, fmt.Errorf("text outside the document element")
				}
				continue
			}
			cur.Children = append(cur.Children, &Node{IsText: true, Text: string(t), Parent: cur})
		}
	}
	if root == nil || cur != nil {
		return nil, fmt.Errorf("the document is not complete")
	}
	return root, nil
}

// walk visits every element.
func (n *Node) walk(f func(*Node)) {
	if n.IsText {
		return
	}
	f(n)
	for _, c := range n.Children {
		c.walk(f)
	}
}
