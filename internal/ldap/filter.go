package ldap

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Filter choices (RFC 4511 4.5.1).
const (
	FilterAnd        = 0
	FilterOr         = 1
	FilterNot        = 2
	FilterEquality   = 3
	FilterSubstrings = 4
	FilterGreater    = 5
	FilterLess       = 6
	FilterPresent    = 7
	FilterApprox     = 8
	FilterExtensible = 9
)

var ErrFilter = errors.New("ldap: invalid search filter")

// EscapeFilter makes a value safe inside a filter (RFC 4515): what the person typed can never change the filter's shape.
func EscapeFilter(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\', '*', '(', ')', 0:
			fmt.Fprintf(&b, "\\%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ParseFilter turns the string form of a filter (RFC 4515) into its BER form.
func ParseFilter(s string) (*Packet, error) {
	s = strings.TrimSpace(s)
	p, rest, err := parseFilter(s, 0)
	if err != nil {
		return nil, err
	}
	if rest != "" {
		return nil, fmt.Errorf("%w: unexpected %q after the filter", ErrFilter, rest)
	}
	return p, nil
}

func parseFilter(s string, depth int) (*Packet, string, error) {
	if depth > maxDepth {
		return nil, "", fmt.Errorf("%w: nested too deeply", ErrFilter)
	}
	if !strings.HasPrefix(s, "(") {
		return nil, "", fmt.Errorf("%w: a filter starts with (", ErrFilter)
	}
	s = s[1:]
	if s == "" {
		return nil, "", fmt.Errorf("%w: unexpected end", ErrFilter)
	}
	switch s[0] {
	case '&', '|':
		tag := FilterAnd
		if s[0] == '|' {
			tag = FilterOr
		}
		p := Seq(ClassContext, tag)
		s = s[1:]
		for strings.HasPrefix(s, "(") {
			c, rest, err := parseFilter(s, depth+1)
			if err != nil {
				return nil, "", err
			}
			p.Children = append(p.Children, c)
			s = rest
		}
		if len(p.Children) == 0 || !strings.HasPrefix(s, ")") {
			return nil, "", fmt.Errorf("%w: & and | take one or more filters", ErrFilter)
		}
		return p, s[1:], nil
	case '!':
		c, rest, err := parseFilter(s[1:], depth+1)
		if err != nil {
			return nil, "", err
		}
		if !strings.HasPrefix(rest, ")") {
			return nil, "", fmt.Errorf("%w: ! takes one filter", ErrFilter)
		}
		return Seq(ClassContext, FilterNot, c), rest[1:], nil
	}
	end := strings.IndexByte(s, ')')
	if end < 0 {
		return nil, "", fmt.Errorf("%w: missing )", ErrFilter)
	}
	p, err := parseItem(s[:end])
	return p, s[end+1:], err
}

func parseItem(s string) (*Packet, error) {
	if strings.ContainsAny(s, "(") {
		return nil, fmt.Errorf("%w: unescaped ( in %q", ErrFilter, s)
	}
	if i := strings.Index(s, ":="); i >= 0 { // extensible: attr[:dn][:rule]:=value
		lhs, val := strings.Split(s[:i], ":"), s[i+2:]
		v, err := unescape(val)
		if err != nil {
			return nil, err
		}
		p := Seq(ClassContext, FilterExtensible)
		attr, rule, dn := lhs[0], "", false
		for _, x := range lhs[1:] {
			if strings.EqualFold(x, "dn") {
				dn = true
			} else if x != "" && rule == "" {
				rule = x
			} else {
				return nil, fmt.Errorf("%w: %q", ErrFilter, s)
			}
		}
		if attr == "" && rule == "" {
			return nil, fmt.Errorf("%w: an extensible match needs an attribute or a rule", ErrFilter)
		}
		if rule != "" {
			p.Children = append(p.Children, Str(ClassContext, 1, rule))
		}
		if attr != "" {
			p.Children = append(p.Children, Str(ClassContext, 2, attr))
		}
		p.Children = append(p.Children, &Packet{Class: ClassContext, Tag: 3, Value: v})
		if dn {
			p.Children = append(p.Children, &Packet{Class: ClassContext, Tag: 4, Value: []byte{0xff}})
		}
		return p, nil
	}
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return nil, fmt.Errorf("%w: %q has no attribute=value", ErrFilter, s)
	}
	attr, val, tag := s[:i], s[i+1:], FilterEquality
	switch attr[len(attr)-1] {
	case '>':
		tag, attr = FilterGreater, attr[:len(attr)-1]
	case '<':
		tag, attr = FilterLess, attr[:len(attr)-1]
	case '~':
		tag, attr = FilterApprox, attr[:len(attr)-1]
	}
	if attr == "" || strings.ContainsAny(attr, " *\\") {
		return nil, fmt.Errorf("%w: bad attribute in %q", ErrFilter, s)
	}
	if tag == FilterEquality && val == "*" {
		return Str(ClassContext, FilterPresent, attr), nil
	}
	if tag == FilterEquality && strings.Contains(val, "*") {
		parts := strings.Split(val, "*")
		subs := Seq(ClassUniversal, TagSequence)
		for k, part := range parts {
			if part == "" {
				continue
			}
			v, err := unescape(part)
			if err != nil {
				return nil, err
			}
			t := 1 // any
			if k == 0 {
				t = 0 // initial
			} else if k == len(parts)-1 {
				t = 2 // final
			}
			subs.Children = append(subs.Children, &Packet{Class: ClassContext, Tag: t, Value: v})
		}
		return Seq(ClassContext, FilterSubstrings, Str(ClassUniversal, TagOctetString, attr), subs), nil
	}
	v, err := unescape(val)
	if err != nil {
		return nil, err
	}
	return Seq(ClassContext, tag, Str(ClassUniversal, TagOctetString, attr), &Packet{Class: ClassUniversal, Tag: TagOctetString, Value: v}), nil
}

func unescape(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		if i+3 > len(s) {
			return nil, fmt.Errorf("%w: a \\ is followed by two hex digits", ErrFilter)
		}
		n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("%w: a \\ is followed by two hex digits", ErrFilter)
		}
		out = append(out, byte(n))
		i += 2
	}
	return out, nil
}
