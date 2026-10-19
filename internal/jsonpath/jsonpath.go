// Package jsonpath evaluates the small, safe subset of JSONPath the generic JSON importer needs: $ for the root, .name and ['name'] for a
// member, [n] (negative counts from the end) for an array element, [*] and .* for every member or element, and ..name for a member at any
// depth. There are no filters or scripts: a path is data from an administrator and must not be able to run anything or run long.
package jsonpath

import (
	"fmt"
	"strconv"
	"strings"
)

type step struct {
	name  string
	index int
	kind  byte // 'n' name, 'i' index, '*' every child, 'd' descendant name
}

// Path is a compiled expression.
type Path struct {
	src   string
	steps []step
}

const maxSteps = 16

// Compile parses an expression; the leading $ is optional.
func Compile(expr string) (*Path, error) {
	src := strings.TrimSpace(expr)
	s := strings.TrimPrefix(src, "$")
	p := &Path{src: src}
	if s != "" && s[0] != '.' && s[0] != '[' {
		s = "." + s // "fields.title" reads as "$.fields.title"
	}
	for s != "" {
		if len(p.steps) >= maxSteps {
			return nil, fmt.Errorf("path %q has more than %d steps", src, maxSteps)
		}
		switch {
		case strings.HasPrefix(s, ".."):
			name, rest := ident(s[2:])
			if name == "" {
				return nil, fmt.Errorf("path %q: a name must follow ..", src)
			}
			p.steps = append(p.steps, step{kind: 'd', name: name})
			s = rest
		case s[0] == '.':
			name, rest := ident(s[1:])
			if name == "" {
				return nil, fmt.Errorf("path %q: a name must follow .", src)
			}
			if name == "*" {
				p.steps = append(p.steps, step{kind: '*'})
			} else {
				p.steps = append(p.steps, step{kind: 'n', name: name})
			}
			s = rest
		case s[0] == '[':
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q: unclosed [", src)
			}
			in := strings.TrimSpace(s[1:end])
			s = s[end+1:]
			switch {
			case in == "*":
				p.steps = append(p.steps, step{kind: '*'})
			case len(in) >= 2 && (in[0] == '\'' || in[0] == '"') && in[len(in)-1] == in[0]:
				p.steps = append(p.steps, step{kind: 'n', name: in[1 : len(in)-1]})
			default:
				n, err := strconv.Atoi(in)
				if err != nil {
					return nil, fmt.Errorf("path %q: %q is not a name, an index or *", src, in)
				}
				p.steps = append(p.steps, step{kind: 'i', index: n})
			}
		default:
			return nil, fmt.Errorf("path %q: unexpected %q", src, s)
		}
	}
	return p, nil
}

func ident(s string) (string, string) {
	i := 0
	for i < len(s) && s[i] != '.' && s[i] != '[' {
		i++
	}
	return s[:i], s[i:]
}

const maxNodes = 200000

// Eval returns every value the path selects, in document order. A path that selects nothing returns an empty list, not an error.
func (p *Path) Eval(doc any) []any {
	cur := []any{doc}
	for _, st := range p.steps {
		var next []any
		for _, v := range cur {
			switch st.kind {
			case 'n':
				if m, ok := v.(map[string]any); ok {
					if x, ok := m[st.name]; ok {
						next = append(next, x)
					}
				}
			case 'i':
				if a, ok := v.([]any); ok {
					i := st.index
					if i < 0 {
						i += len(a)
					}
					if i >= 0 && i < len(a) {
						next = append(next, a[i])
					}
				}
			case '*':
				switch c := v.(type) {
				case []any:
					next = append(next, c...)
				case map[string]any:
					for _, k := range sortedKeys(c) {
						next = append(next, c[k])
					}
				}
			case 'd':
				budget := maxNodes
				descend(v, st.name, &next, &budget)
			}
			if len(next) > maxNodes {
				return next[:maxNodes]
			}
		}
		cur = next
	}
	return cur
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	// insertion sort: maps here are small and this avoids importing sort for one call
	for i := 1; i < len(ks); i++ {
		for j := i; j > 0 && ks[j] < ks[j-1]; j-- {
			ks[j], ks[j-1] = ks[j-1], ks[j]
		}
	}
	return ks
}

func descend(v any, name string, out *[]any, budget *int) {
	if *budget <= 0 {
		return
	}
	*budget--
	switch c := v.(type) {
	case map[string]any:
		if x, ok := c[name]; ok {
			*out = append(*out, x)
		}
		for _, k := range sortedKeys(c) {
			descend(c[k], name, out, budget)
		}
	case []any:
		for _, x := range c {
			descend(x, name, out, budget)
		}
	}
}

// First returns the first selected value.
func (p *Path) First(doc any) (any, bool) {
	r := p.Eval(doc)
	if len(r) == 0 {
		return nil, false
	}
	return r[0], true
}

// String renders a selected scalar as text: strings as they are, numbers without a needless fraction, booleans as true or false; null and
// structures give "".
func String(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}
