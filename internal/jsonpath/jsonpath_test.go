package jsonpath

import (
	"encoding/json"
	"strings"
	"testing"
)

func doc(t *testing.T, s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEvalSubset(t *testing.T) {
	d := doc(t, `{"data":{"items":[{"id":1,"f":{"t":"a","tags":["x","y"]}},{"id":2.5,"f":{"t":"b"}}]},"a b":"sp"}`)
	cases := []struct {
		path string
		want string
	}{
		{"$.data.items[*].id", "1,2.5"},
		{"data.items[0].f.t", "a"},
		{"$.data.items[-1].f.t", "b"},
		{"$['a b']", "sp"},
		{"$.data.items[*].f.tags[*]", "x,y"},
		{"$..t", "a,b"},
		{"$.data.items[5]", ""},
		{"$.missing.deeper", ""},
		{"$.data.items.*", ""}, // objects are not scalars
	}
	for _, c := range cases {
		p, err := Compile(c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		var got []string
		for _, v := range p.Eval(d) {
			if s := String(v); s != "" {
				got = append(got, s)
			}
		}
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s: got %v, want %q", c.path, got, c.want)
		}
	}
	p, _ := Compile("$.data.items.*")
	if n := len(p.Eval(d)); n != 2 {
		t.Errorf("wildcard over an array: %d", n)
	}
}

func TestCompileRefusals(t *testing.T) {
	for _, bad := range []string{"$.a[", "$.a[?(@.x)]", "$..", "$.a[1:2]", "$.a.", "$" + strings.Repeat(".a", 17)} {
		if _, err := Compile(bad); err == nil {
			t.Errorf("%q compiled", bad)
		}
	}
	if p, err := Compile("$"); err != nil || len(p.Eval(1.0)) != 1 {
		t.Error("the root path selects the document")
	}
}

func TestDescendantIsBounded(t *testing.T) {
	// a deeply nested document must not make a path run away
	var v any = map[string]any{"t": 1.0}
	for i := 0; i < 5000; i++ {
		v = map[string]any{"c": v, "t": 1.0}
	}
	p, _ := Compile("$..t")
	if n := len(p.Eval(v)); n == 0 || n > maxNodes {
		t.Errorf("%d results", n)
	}
}
