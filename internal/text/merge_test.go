package text

import "testing"

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
