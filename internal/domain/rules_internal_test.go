package domain

import "testing"

func TestRuleConditions(t *testing.T) {
	cases := []struct {
		name string
		r    Rule
		vals map[string]any
		role string
		want bool
	}{
		{"always", Rule{}, nil, "member", true},
		{"eq", Rule{WhenField: "a", WhenOp: "eq", WhenValue: "x"}, map[string]any{"a": "x"}, "", true},
		{"eq differs", Rule{WhenField: "a", WhenOp: "eq", WhenValue: "x"}, map[string]any{"a": "y"}, "", false},
		{"eq on empty is false", Rule{WhenField: "a", WhenOp: "eq", WhenValue: "x"}, map[string]any{}, "", false},
		{"eq on a list is membership", Rule{WhenField: "a", WhenOp: "eq", WhenValue: "x"}, map[string]any{"a": []string{"w", "x"}}, "", true},
		{"eq number", Rule{WhenField: "a", WhenOp: "eq", WhenValue: 3.0}, map[string]any{"a": 3.0}, "", true},
		{"ne", Rule{WhenField: "a", WhenOp: "ne", WhenValue: "x"}, map[string]any{"a": "y"}, "", true},
		{"ne on empty", Rule{WhenField: "a", WhenOp: "ne", WhenValue: "x"}, map[string]any{}, "", true},
		{"ne equal", Rule{WhenField: "a", WhenOp: "ne", WhenValue: "x"}, map[string]any{"a": "x"}, "", false},
		{"in", Rule{WhenField: "a", WhenOp: "in", WhenValue: []any{"x", "y"}}, map[string]any{"a": "y"}, "", true},
		{"in none", Rule{WhenField: "a", WhenOp: "in", WhenValue: []any{"x", "y"}}, map[string]any{"a": "z"}, "", false},
		{"in with a list value", Rule{WhenField: "a", WhenOp: "in", WhenValue: []any{"x"}}, map[string]any{"a": []any{"q", "x"}}, "", true},
		{"empty", Rule{WhenField: "a", WhenOp: "empty"}, map[string]any{"a": ""}, "", true},
		{"notempty", Rule{WhenField: "a", WhenOp: "notempty"}, map[string]any{"a": "v"}, "", true},
		{"notempty on empty list", Rule{WhenField: "a", WhenOp: "notempty"}, map[string]any{"a": []string{}}, "", false},
		{"role matches", Rule{WhenRole: "admin"}, nil, "admin", true},
		{"role differs", Rule{WhenRole: "admin"}, nil, "member", false},
	}
	for _, c := range cases {
		if got := ruleHolds(c.r, c.vals, c.role); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	st := evalRules([]Rule{
		{Enabled: true, Action: "hide", Target: "f"},
		{Enabled: true, Action: "show", Target: "f"},
		{Enabled: true, Action: "require", Target: "g"},
		{Enabled: false, Action: "require", Target: "h"},
	}, nil, "member")
	if st["f"].Hidden || !st["g"].Required || st["h"].Required {
		t.Fatalf("%+v", st)
	}
}
