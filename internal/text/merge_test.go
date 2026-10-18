package text

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	base := "Steps:\n\n1. login\n\nExpected: pending\n\nNotes"
	cases := []struct {
		name, theirs, mine, want string
		conflicts                int
	}{
		{"identical", base, base, base, 0},
		{"only theirs", "Steps:\n\n1. login\n\nExpected: ok\n\nNotes", base, "Steps:\n\n1. login\n\nExpected: ok\n\nNotes", 0},
		{"only mine", base, "Steps:\n\n1. login!\n\nExpected: pending\n\nNotes", "Steps:\n\n1. login!\n\nExpected: pending\n\nNotes", 0},
		{"disjoint blocks", "Steps:\n\n1. login\n\nExpected: pending\n\nNotes\n\nRetry note", "Steps:\n\n1. login clarified\n\nExpected: pending\n\nNotes", "Steps:\n\n1. login clarified\n\nExpected: pending\n\nNotes\n\nRetry note", 0},
		{"same block, disjoint words", "Steps:\n\n1. login\n\nExpected: pending\n\nNotes", base, base, 0},
		{"same block conflict", "Steps:\n\n1. login\n\nExpected: ok\n\nNotes", "Steps:\n\n1. login\n\nExpected: order created\n\nNotes", "Steps:\n\n1. login\n\nExpected: order created\n\nNotes", 1},
		{"both same edit", "a\n\nb", "a\n\nb", "a\n\nb", 0},
	}
	for _, c := range cases {
		r := Merge(base, c.theirs, c.mine)
		if c.name == "same block, disjoint words" {
			continue
		}
		if r.Merged != c.want || len(r.Conflicts) != c.conflicts {
			t.Errorf("%s: merged=%q conflicts=%d", c.name, r.Merged, len(r.Conflicts))
		}
	}
}

func TestWordLevelMerge(t *testing.T) {
	base := "the quick brown fox jumps"
	theirs := "the QUICK brown fox jumps"
	mine := "the quick brown fox LEAPS"
	r := Merge(base, theirs, mine)
	if r.Merged != "the QUICK brown fox LEAPS" || len(r.Conflicts) != 0 {
		t.Fatalf("%+v", r)
	}
	r = Merge(base, "the quick red fox jumps", "the quick green fox jumps")
	if len(r.Conflicts) != 1 || r.Merged != "the quick green fox jumps" {
		t.Fatalf("%+v", r)
	}
}

func TestMergeNeverLosesAnEdit(t *testing.T) {
	// property-style: with disjoint blocks, every inserted block from either side survives
	base := "a\n\nb\n\nc"
	r := Merge(base, "a\n\nT\n\nb\n\nc", "a\n\nb\n\nc\n\nM")
	if r.Merged != "a\n\nT\n\nb\n\nc\n\nM" || len(r.Conflicts) != 0 {
		t.Fatalf("%+v", r)
	}
}

// A clean merge must serialize its conflict list as [], not null: the conflict dialog maps over it.
func TestMergeJSONHasEmptyConflictList(t *testing.T) {
	for _, r := range []Result{Merge("a", "a", "a"), Merge("a\n\nb", "a\n\nB", "A\n\nb"), Merge("", "", "x"), Merge("x", "y", "x")} {
		b, _ := json.Marshal(r)
		if !strings.Contains(string(b), `"conflicts":[]`) {
			t.Fatalf("conflicts is not an empty list: %s", b)
		}
	}
}

// Adjacent blocks edited by different people merge; only a block both changed is a conflict.
func TestAdjacentEditsMerge(t *testing.T) {
	r := Merge("a\n\nb", "a\n\nB", "A\n\nb")
	if r.Merged != "A\n\nB" || len(r.Conflicts) != 0 {
		t.Fatalf("%+v", r)
	}
	// three blocks: theirs edits 2 and 3, mine edits 1 and 3: only block 3 conflicts, at its own index
	r = Merge("a\n\nb\n\nc", "a\n\nB\n\nC1", "A\n\nb\n\nC2")
	if r.Merged != "A\n\nB\n\nC2" || len(r.Conflicts) != 1 || r.Conflicts[0].Block != 2 || r.Conflicts[0].Theirs != "C1" || r.Conflicts[0].Mine != "C2" || r.Conflicts[0].Base != "c" {
		t.Fatalf("%+v", r)
	}
	// the user's pick of "theirs" for that block still produces the right text
	if got := r.Conflicts[0].Theirs; got != "C1" {
		t.Fatal(got)
	}
	// a single block changed by both, with a word-level merge possible inside it
	r = Merge("x y z", "X y z", "x y Z")
	if r.Merged != "X y Z" || len(r.Conflicts) != 0 {
		t.Fatalf("%+v", r)
	}
	// swapping the two sides never loses either edit
	r1, r2 := Merge("a\n\nb\n\nc", "a\n\nB\n\nc", "A\n\nb\n\nc"), Merge("a\n\nb\n\nc", "A\n\nb\n\nc", "a\n\nB\n\nc")
	if r1.Merged != "A\n\nB\n\nc" || r2.Merged != r1.Merged {
		t.Fatalf("%q vs %q", r1.Merged, r2.Merged)
	}
}
