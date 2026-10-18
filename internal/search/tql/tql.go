// Package tql parses Tasklane Query Language, a JQL-compatible subset (SPEC 10):
//
//	project = ABC AND status in ("To Do", "In Progress") AND assignee = currentUser()
//	  AND (labels = payments OR text ~ "3DS") AND updated >= -7d ORDER BY priority DESC, updated
//
// The parser is total: any input yields an AST or a positioned error, never a panic.
package tql

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	MaxLen   = 4096
	MaxDepth = 32
	MaxTerms = 200
)

// Error is a parse or semantic error with the byte offset where it was detected.
type Error struct {
	Pos int
	Msg string
}

func (e *Error) Error() string { return fmt.Sprintf("%s (at position %d)", e.Msg, e.Pos) }

type tokKind int

const (
	tEOF tokKind = iota
	tWord
	tStr
	tLParen
	tRParen
	tComma
	tOp // = != ~ !~ > < >= <=
)

type token struct {
	kind   tokKind
	s      string
	pos    int
	quoted bool
}

func lex(src string) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			out = append(out, token{tLParen, "(", i, false})
			i++
		case c == ')':
			out = append(out, token{tRParen, ")", i, false})
			i++
		case c == ',':
			out = append(out, token{tComma, ",", i, false})
			i++
		case c == '"' || c == '\'':
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < len(src) {
				if src[i] == '\\' && i+1 < len(src) {
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				if src[i] == c {
					closed = true
					i++
					break
				}
				b.WriteByte(src[i])
				i++
			}
			if !closed {
				return nil, &Error{start, "unterminated string"}
			}
			out = append(out, token{tStr, b.String(), start, true})
		case c == '=' || c == '~' || c == '>' || c == '<' || c == '!':
			start := i
			op := string(c)
			i++
			if i < len(src) && src[i] == '=' && c != '=' && c != '~' {
				op += "="
				i++
			} else if c == '!' && i < len(src) && src[i] == '~' {
				op = "!~"
				i++
			}
			if op == "!" {
				return nil, &Error{start, "unexpected '!'"}
			}
			out = append(out, token{tOp, op, start, false})
		default:
			start := i
			for i < len(src) && isWord(rune(src[i])) {
				i++
			}
			if i == start {
				// non-ASCII letters and anything else: take a full rune
				r := []rune(src[i:])[0]
				if unicode.IsLetter(r) || unicode.IsDigit(r) {
					for i < len(src) {
						rr := []rune(src[i:])[0]
						if !(isWord(rr) || unicode.IsLetter(rr) || unicode.IsDigit(rr)) {
							break
						}
						i += len(string(rr))
					}
				} else {
					return nil, &Error{start, fmt.Sprintf("unexpected character %q", r)}
				}
			}
			out = append(out, token{tWord, src[start:i], start, false})
		}
	}
	out = append(out, token{tEOF, "", len(src), false})
	return out, nil
}

func isWord(r rune) bool {
	return r == '_' || r == '-' || r == '+' || r == '.' || r == ':' || r == '@' || r == '/' || r == '*' ||
		(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// ---- AST ----

type Node interface{ node() }

type Bool struct {
	Or   bool
	L, R Node
}
type Not struct{ X Node }

// Cmp is one clause. Op is one of = != ~ !~ > < >= <= in notin is isnot was wasnot changed.
type Cmp struct {
	Field string // lower-case field name
	Op    string
	Vals  []Value
	Pos   int
	// for "changed"
	From, To, By *Value
}

func (Bool) node() {}
func (Not) node()  {}
func (Cmp) node()  {}

type Value struct {
	S      string
	Fn     string   // function name when this is a call, lower-case
	Args   []string // function arguments
	Empty  bool     // EMPTY or NULL
	Quoted bool
	Pos    int
}

type Order struct {
	Field string
	Desc  bool
}

type Query struct {
	Where Node // nil = everything
	Order []Order
}

// Parse turns TQL text into a Query.
func Parse(src string) (q *Query, err error) {
	defer func() { // belt and braces: the parser must never take the server down
		if r := recover(); r != nil {
			q, err = nil, &Error{0, "query could not be parsed"}
		}
	}()
	if len(src) > MaxLen {
		return nil, &Error{MaxLen, fmt.Sprintf("query is too long (limit %d characters)", MaxLen)}
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	q = &Query{}
	if !p.isKw("order") && p.peek().kind != tEOF {
		if q.Where, err = p.parseOr(0); err != nil {
			return nil, err
		}
	}
	if p.isKw("order") {
		p.next()
		if !p.isKw("by") {
			return nil, &Error{p.peek().pos, "expected BY after ORDER"}
		}
		p.next()
		for {
			t := p.next()
			if t.kind != tWord {
				return nil, &Error{t.pos, "expected a field name after ORDER BY"}
			}
			o := Order{Field: strings.ToLower(t.s)}
			if p.isKw("desc") {
				o.Desc = true
				p.next()
			} else if p.isKw("asc") {
				p.next()
			}
			q.Order = append(q.Order, o)
			if p.peek().kind == tComma {
				p.next()
				continue
			}
			break
		}
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, &Error{t.pos, fmt.Sprintf("unexpected %q", t.s)}
	}
	return q, nil
}

type parser struct {
	toks  []token
	i     int
	terms int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tEOF {
		p.i++
	}
	return t
}
func (p *parser) isKw(k string) bool {
	t := p.peek()
	return t.kind == tWord && !t.quoted && strings.EqualFold(t.s, k)
}

func (p *parser) parseOr(depth int) (Node, error) {
	if depth > MaxDepth {
		return nil, &Error{p.peek().pos, "query is nested too deeply"}
	}
	l, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	for p.isKw("or") {
		p.next()
		r, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		l = Bool{Or: true, L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseAnd(depth int) (Node, error) {
	l, err := p.parseNot(depth)
	if err != nil {
		return nil, err
	}
	for p.isKw("and") {
		p.next()
		r, err := p.parseNot(depth)
		if err != nil {
			return nil, err
		}
		l = Bool{L: l, R: r}
	}
	return l, nil
}

func (p *parser) parseNot(depth int) (Node, error) {
	if depth > MaxDepth {
		return nil, &Error{p.peek().pos, "query is nested too deeply"}
	}
	if p.isKw("not") {
		p.next()
		x, err := p.parseNot(depth + 1)
		if err != nil {
			return nil, err
		}
		return Not{x}, nil
	}
	if p.peek().kind == tLParen {
		p.next()
		x, err := p.parseOr(depth + 1)
		if err != nil {
			return nil, err
		}
		if t := p.next(); t.kind != tRParen {
			return nil, &Error{t.pos, "expected ')'"}
		}
		return x, nil
	}
	return p.parseClause()
}

func (p *parser) parseClause() (Node, error) {
	p.terms++
	if p.terms > MaxTerms {
		return nil, &Error{p.peek().pos, "query has too many clauses"}
	}
	ft := p.next()
	if ft.kind != tWord && ft.kind != tStr {
		return nil, &Error{ft.pos, "expected a field name"}
	}
	c := Cmp{Field: strings.ToLower(ft.s), Pos: ft.pos}
	t := p.peek()
	switch {
	case t.kind == tOp:
		p.next()
		c.Op = t.s
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		c.Vals = []Value{v}
	case p.isKw("is"):
		p.next()
		c.Op = "is"
		if p.isKw("not") {
			p.next()
			c.Op = "isnot"
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		if !v.Empty {
			return nil, &Error{v.Pos, "IS must be followed by EMPTY or NULL"}
		}
		c.Vals = []Value{v}
	case p.isKw("in") || p.isKw("not"):
		c.Op = "in"
		if p.isKw("not") {
			p.next()
			c.Op = "notin"
			if !p.isKw("in") {
				return nil, &Error{p.peek().pos, "expected IN after NOT"}
			}
		}
		p.next()
		if t := p.next(); t.kind != tLParen {
			return nil, &Error{t.pos, "expected '(' after IN"}
		}
		for {
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			c.Vals = append(c.Vals, v)
			if len(c.Vals) > 500 {
				return nil, &Error{p.peek().pos, "too many values in list"}
			}
			t := p.next()
			if t.kind == tComma {
				continue
			}
			if t.kind != tRParen {
				return nil, &Error{t.pos, "expected ',' or ')'"}
			}
			break
		}
	case p.isKw("was"):
		p.next()
		c.Op = "was"
		if p.isKw("not") {
			p.next()
			c.Op = "wasnot"
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		c.Vals = []Value{v}
	case p.isKw("changed"):
		p.next()
		c.Op = "changed"
		for {
			var dst **Value
			switch {
			case p.isKw("from"):
				dst = &c.From
			case p.isKw("to"):
				dst = &c.To
			case p.isKw("by"):
				dst = &c.By
			}
			if dst == nil {
				break
			}
			p.next()
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			*dst = &v
		}
	default:
		return nil, &Error{t.pos, fmt.Sprintf("expected an operator after %q", ft.s)}
	}
	return c, nil
}

func (p *parser) parseValue() (Value, error) {
	t := p.next()
	switch t.kind {
	case tStr:
		return Value{S: t.s, Quoted: true, Pos: t.pos}, nil
	case tWord:
		lw := strings.ToLower(t.s)
		if lw == "empty" || lw == "null" {
			return Value{Empty: true, Pos: t.pos}, nil
		}
		if p.peek().kind == tLParen { // function call
			p.next()
			v := Value{Fn: lw, Pos: t.pos}
			if p.peek().kind != tRParen {
				for {
					a := p.next()
					if a.kind != tWord && a.kind != tStr {
						return Value{}, &Error{a.pos, "expected a function argument"}
					}
					v.Args = append(v.Args, a.s)
					if p.peek().kind == tComma {
						p.next()
						continue
					}
					break
				}
			}
			if e := p.next(); e.kind != tRParen {
				return Value{}, &Error{e.pos, "expected ')' to close function call"}
			}
			return v, nil
		}
		switch lw {
		case "and", "or", "not", "order", "in", "is", "was", "changed":
			return Value{}, &Error{t.pos, fmt.Sprintf("expected a value but found keyword %q (quote it to use it as text)", t.s)}
		}
		return Value{S: t.s, Pos: t.pos}, nil
	}
	return Value{}, &Error{t.pos, "expected a value"}
}
