package tql

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// FromText turns a plain search, the kind typed into Kibana's bar, into TQL:
//
//	Alan AND (Arkady OR "Dana K.") -draft
//
// Words and quoted phrases match anything (any ~ "..."): summary, description, labels, comments, key, assignee and
// reporter. AND, OR and NOT (any case) combine them, brackets group them, a leading - negates a word and words side by side
// must all match. Errors carry the position in src, not in the TQL that comes out. An empty search is an empty query.
func FromText(src string) (out string, err error) {
	defer func() { // like Parse: never take the server down
		if r := recover(); r != nil {
			out, err = "", &Error{0, "search could not be parsed"}
		}
	}()
	if len(src) > MaxLen {
		return "", &Error{MaxLen, "search is too long"}
	}
	toks, err := lexText(src)
	if err != nil {
		return "", err
	}
	p := &textParser{toks: toks}
	if p.peek().kind == xEOF {
		return "", nil
	}
	if out, err = p.or(0); err != nil {
		return "", err
	}
	if t := p.peek(); t.kind != xEOF {
		return "", &Error{t.pos, "unexpected ')'"}
	}
	return out, nil
}

const (
	xWord = iota
	xPhrase
	xAnd
	xOr
	xNot
	xLParen
	xRParen
	xEOF
)

type textTok struct {
	kind int
	s    string
	pos  int
}

func lexText(src string) ([]textTok, error) {
	var out []textTok
	i := 0
	for i < len(src) {
		r, n := utf8.DecodeRuneInString(src[i:])
		switch {
		case unicode.IsSpace(r):
			i += n
		case r == '(':
			out = append(out, textTok{xLParen, "(", i})
			i++
		case r == ')':
			out = append(out, textTok{xRParen, ")", i})
			i++
		case r == '-' && i+1 < len(src) && !strings.ContainsRune(" \t\r\n)", rune(src[i+1])):
			out = append(out, textTok{xNot, "-", i}) // -word at the start of a token, as in every search box; e-mail stays one word
			i++
		case r == '"':
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
				if src[i] == '"' {
					closed = true
					i++
					break
				}
				b.WriteByte(src[i])
				i++
			}
			if !closed {
				return nil, &Error{start, "unterminated quote"}
			}
			if s := strings.TrimSpace(b.String()); s != "" {
				out = append(out, textTok{xPhrase, s, start})
			}
		default:
			start := i
			for i < len(src) && !strings.ContainsRune(" \t\r\n()\"", rune(src[i])) {
				i++
			}
			w := src[start:i]
			kind := xWord
			switch strings.ToLower(w) {
			case "and", "&&":
				kind = xAnd
			case "or", "||":
				kind = xOr
			case "not", "!":
				kind = xNot
			}
			out = append(out, textTok{kind, w, start})
		}
	}
	return append(out, textTok{xEOF, "", len(src)}), nil
}

type textParser struct {
	toks  []textTok
	i     int
	terms int
}

func (p *textParser) peek() textTok { return p.toks[p.i] }
func (p *textParser) next() textTok { t := p.toks[p.i]; p.i++; return t }

func (p *textParser) or(depth int) (string, error) {
	l, err := p.and(depth)
	if err != nil {
		return "", err
	}
	for p.peek().kind == xOr {
		p.next()
		r, err := p.and(depth)
		if err != nil {
			return "", err
		}
		l = l + " OR " + r
	}
	return l, nil
}

// and also takes words side by side: "Alan Dana" is Alan AND Dana.
func (p *textParser) and(depth int) (string, error) {
	l, err := p.not(depth)
	if err != nil {
		return "", err
	}
	for {
		switch p.peek().kind {
		case xAnd:
			p.next()
		case xWord, xPhrase, xNot, xLParen:
		default:
			return l, nil
		}
		r, err := p.not(depth)
		if err != nil {
			return "", err
		}
		l = l + " AND " + r
	}
}

func (p *textParser) not(depth int) (string, error) {
	if depth > MaxDepth {
		return "", &Error{p.peek().pos, "search is nested too deeply"}
	}
	t := p.next()
	switch t.kind {
	case xNot:
		x, err := p.not(depth + 1)
		if err != nil {
			return "", err
		}
		return "NOT " + x, nil
	case xLParen:
		x, err := p.or(depth + 1)
		if err != nil {
			return "", err
		}
		if c := p.next(); c.kind != xRParen {
			return "", &Error{c.pos, "expected ')'"}
		}
		return "(" + x + ")", nil
	case xWord, xPhrase:
		if p.terms++; p.terms > MaxTerms {
			return "", &Error{t.pos, "search has too many words"}
		}
		return `any ~ "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(t.s) + `"`, nil
	case xEOF:
		return "", &Error{t.pos, "expected a word after " + strings.ToUpper(p.toks[max(p.i-2, 0)].s)}
	default:
		return "", &Error{t.pos, "unexpected " + strings.ToUpper(t.s)}
	}
}
