package tql

import (
	"strings"
	"testing"
)

func TestFromText(t *testing.T) {
	cases := []struct{ in, want string }{
		{``, ``},
		{`   `, ``},
		{`Alan`, `any ~ "Alan"`},
		{`Alan AND (Arkady OR Dana)`, `any ~ "Alan" AND (any ~ "Arkady" OR any ~ "Dana")`},
		{`alan and (arkady or dana)`, `any ~ "alan" AND (any ~ "arkady" OR any ~ "dana")`},
		{`Alan Dana`, `any ~ "Alan" AND any ~ "Dana"`}, // side by side: all must match
		{`Alan OR Dana Arkady`, `any ~ "Alan" OR any ~ "Dana" AND any ~ "Arkady"`},
		{`"Alan Polevoi"`, `any ~ "Alan Polevoi"`},
		{`NOT draft`, `NOT any ~ "draft"`},
		{`login -draft`, `any ~ "login" AND NOT any ~ "draft"`},
		{`-(a OR b)`, `NOT (any ~ "a" OR any ~ "b")`},
		{`e-mail`, `any ~ "e-mail"`},
		{`ABC-12`, `any ~ "ABC-12"`},
		{`say "he said \"hi\""`, `any ~ "say" AND any ~ "he said \"hi\""`},
		{`back\slash`, `any ~ "back\\slash"`},
		{`status = Done`, `any ~ "status" AND any ~ "=" AND any ~ "Done"`}, // TQL typed in text mode is just words
		{`日本語 ünï`, `any ~ "日本語" AND any ~ "ünï"`},
	}
	for _, c := range cases {
		got, err := FromText(c.in)
		if err != nil || got != c.want {
			t.Errorf("FromText(%q) = %q, %v; want %q", c.in, got, err, c.want)
			continue
		}
		if _, err := Parse(got); err != nil { // whatever comes out is valid TQL
			t.Errorf("FromText(%q) = %q does not parse: %v", c.in, got, err)
		}
	}
}

func TestFromTextErrors(t *testing.T) {
	cases := []struct {
		in, msg string
		pos     int
	}{
		{`Alan AND`, "expected a word after AND", 8},
		{`OR Alan`, "unexpected OR", 0},
		{`(Alan`, "expected ')'", 5},
		{`Alan)`, "unexpected ')'", 4},
		{`say "hi`, "unterminated quote", 4},
		{`NOT`, "expected a word after NOT", 3},
		{strings.Repeat("(", MaxDepth+2) + "a", "nested too deeply", -1},
		{strings.Repeat("a ", MaxTerms+1), "too many words", -1},
	}
	for _, c := range cases {
		_, err := FromText(c.in)
		e, ok := err.(*Error)
		if !ok || !strings.Contains(e.Msg, c.msg) || (c.pos >= 0 && e.Pos != c.pos) {
			t.Errorf("FromText(%q) = %v; want %q at %d", c.in, err, c.msg, c.pos)
		}
	}
}

func FuzzFromText(f *testing.F) {
	for _, s := range []string{`Alan AND (Arkady OR Dana)`, `-"x y" OR`, `((`, `"\`, `ü-`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out, err := FromText(s)
		if err != nil {
			return
		}
		if _, err := Parse(out); err != nil {
			t.Fatalf("FromText(%q) = %q, which does not parse: %v", s, out, err)
		}
	})
}
