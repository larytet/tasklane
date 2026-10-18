package tql

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestParseGood(t *testing.T) { // TQL-01 grammar
	good := []string{
		``,
		`project = ABC`,
		`project = ABC AND status in ("To Do", "In Progress") AND assignee = currentUser()`,
		`labels = payments OR (text ~ "3DS" AND NOT priority = low)`,
		`assignee is EMPTY ORDER BY priority DESC, updated ASC`,
		`status not in (Done, Closed)`,
		`updated >= -7d AND created < startOfWeek()`,
		`status was "To Do"`,
		`status was not Done`,
		`status changed`,
		`status changed from "To Do" to "In Progress" by currentUser()`,
		`summary !~ "flaky"`,
		`ORDER BY updated`,
		`key = ABC-123`,
		`reporter is not null`,
		`text ~ "ünïcode 日本語"`,
		`PROJECT = abc and STATUS = done order by KEY desc`,
	}
	for _, g := range good {
		if _, err := Parse(g); err != nil {
			t.Errorf("%q: %v", g, err)
		}
	}
	q, _ := Parse(`a = 1 OR b = 2 AND c = 3`)
	or, ok := q.Where.(Bool)
	if !ok || !or.Or { // AND binds tighter than OR
		t.Fatalf("precedence: %+v", q.Where)
	}
}

func TestParseErrorsArePositionedAndClear(t *testing.T) { // TQL-03: clear error, no silent wrong result
	bad := map[string]string{
		`project =`:           "expected a value",
		`project ABC`:         "expected an operator",
		`status in (a, b`:     "expected ',' or ')'",
		`status in a`:         "expected '(' after IN",
		`(status = a`:         "expected ')'",
		`status = "open`:      "unterminated string",
		`status = a AND`:      "expected a field name",
		`status = a ORDER`:    "expected BY",
		`status = a ORDER BY`: "expected a field name after ORDER BY",
		`status is open`:      "IS must be followed by EMPTY or NULL",
		`status = and`:        "keyword",
		`status = a b`:        "unexpected",
		`status = a ) `:       "unexpected",
		`status ! a`:          "unexpected '!'",
		`status = a # b`:      "unexpected character",
		`status not = a`:      "expected IN after NOT",
	}
	for q, want := range bad {
		_, err := Parse(q)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", q, err, want)
		}
		if e, ok := err.(*Error); ok && (e.Pos < 0 || e.Pos > len(q)) {
			t.Errorf("%q: position %d out of range", q, e.Pos)
		}
	}
}

func TestLimits(t *testing.T) {
	if _, err := Parse(strings.Repeat("(", 5000)); err == nil {
		t.Fatal("deep nesting accepted")
	}
	if _, err := Parse(strings.Repeat("(", 100) + "a=1" + strings.Repeat(")", 100)); err == nil {
		t.Fatal("nesting beyond the limit accepted")
	}
	if _, err := Parse("a = 1" + strings.Repeat(" AND a = 1", 300)); err == nil {
		t.Fatal("too many clauses accepted")
	}
	if _, err := Parse(strings.Repeat("a", MaxLen+1)); err == nil {
		t.Fatal("overlong query accepted")
	}
}

// TQL-02: random and mutated input never panics or hangs.
func TestFuzzRandomInput(t *testing.T) {
	seeds := []string{`project = ABC AND status in ("a","b") ORDER BY updated DESC`, `status changed from a to b by x`, `text ~ "q" OR NOT (a=1)`}
	alphabet := []string{"(", ")", ",", "=", "!=", "~", "!~", ">", "<", ">=", "AND", "OR", "NOT", "in", "is", "was", "changed", "ORDER", "BY", "EMPTY", `"`, `'`, "a", "-7d", "currentUser()", " ", "\x00", "é", "日", "\\"}
	rng := rand.New(rand.NewSource(1))
	deadline := time.Now().Add(2 * time.Second)
	n := 0
	for time.Now().Before(deadline) {
		var s string
		if rng.Intn(2) == 0 {
			b := []byte(seeds[rng.Intn(len(seeds))])
			for k := 0; k < 1+rng.Intn(4); k++ {
				if len(b) > 0 {
					b[rng.Intn(len(b))] = byte(rng.Intn(256))
				}
			}
			s = string(b)
		} else {
			var sb strings.Builder
			for k := 0; k < rng.Intn(30); k++ {
				sb.WriteString(alphabet[rng.Intn(len(alphabet))])
				sb.WriteByte(' ')
			}
			s = sb.String()
		}
		done := make(chan struct{})
		go func() { defer close(done); Parse(s) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("parser hung on %q", s)
		}
		n++
	}
	t.Logf("%d inputs", n)
}

func FuzzParse(f *testing.F) {
	f.Add(`project = ABC AND status in ("a") ORDER BY updated`)
	f.Fuzz(func(t *testing.T, s string) { Parse(s) })
}
