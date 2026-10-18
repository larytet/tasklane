// Package text implements block-level three-way merge for the conflict dialog (SPEC 4.5):
// a block (paragraph) changed by only one side takes that side; a block changed by both
// is merged at word level when the edits do not overlap, otherwise it is reported as a conflict.
package text

import (
	"regexp"
	"strings"
)

// Conflict is one block changed incompatibly by both sides.
type Conflict struct {
	Block  int    `json:"block"` // index in the merged block list
	Base   string `json:"base"`
	Theirs string `json:"theirs"`
	Mine   string `json:"mine"`
}

// Result of a merge. Merged contains Mine's version of every conflicting block as a
// default, so it is always a complete, saveable document the user can still edit.
type Result struct {
	Merged       string     `json:"merged"`
	Conflicts    []Conflict `json:"conflicts"`
	TheirChanged int        `json:"their_changes"`
	MineChanged  int        `json:"your_changes"`
}

func Blocks(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n\n")
}

func Join(b []string) string { return strings.Join(b, "\n\n") }

// Merge performs the three-way merge of theirs and mine against base.
func Merge(base, theirs, mine string) Result {
	r := Result{Conflicts: []Conflict{}} // never nil: it serializes as [] and clients map over it
	bb, tb, mb := Blocks(base), Blocks(theirs), Blocks(mine)
	r.TheirChanged = countChanges(bb, tb)
	r.MineChanged = countChanges(bb, mb)
	if theirs == mine {
		r.Merged = mine
		return r
	}
	if base == theirs {
		r.Merged = mine
		return r
	}
	if base == mine {
		r.Merged = theirs
		return r
	}
	var out []string
	chunks := diff3(bb, tb, mb)
	for _, c := range chunks {
		switch c.kind {
		case ckStable, ckTheirs, ckMine, ckSame:
			out = append(out, c.out...)
		case ckConflict:
			// Adjacent edits by different people land in one diff3 span. When all three sides still have the same
			// number of blocks there, align them position by position and conflict only on blocks both changed.
			if len(c.base) == len(c.a) && len(c.a) == len(c.b) {
				for i := range c.base {
					bs, as, ms := c.base[i], c.a[i], c.b[i]
					switch {
					case as == ms || bs == ms:
						out = append(out, as)
					case bs == as:
						out = append(out, ms)
					default:
						if m, ok := mergeTokens(bs, as, ms); ok {
							out = append(out, m)
							continue
						}
						r.Conflicts = append(r.Conflicts, Conflict{Block: len(out), Base: bs, Theirs: as, Mine: ms})
						out = append(out, ms) // default to mine
					}
				}
				continue
			}
			start := len(out)
			out = append(out, c.b...) // default to mine
			r.Conflicts = append(r.Conflicts, Conflict{
				Block: start, Base: Join(c.base), Theirs: Join(c.a), Mine: Join(c.b),
			})
		}
	}
	r.Merged = Join(out)
	return r
}

func countChanges(base, other []string) int {
	n := 0
	for _, c := range diff2(base, other) {
		if !c.same {
			n++
		}
	}
	return n
}

var tokRe = regexp.MustCompile(`\s+|[^\s]+`)

func mergeTokens(base, a, b string) (string, bool) {
	bt, at, mt := tokRe.FindAllString(base, -1), tokRe.FindAllString(a, -1), tokRe.FindAllString(b, -1)
	var out []string
	for _, c := range diff3(bt, at, mt) {
		if c.kind == ckConflict {
			return "", false
		}
		out = append(out, c.out...)
	}
	return strings.Join(out, ""), true
}

type chunkKind int

const (
	ckStable chunkKind = iota
	ckTheirs
	ckMine
	ckSame
	ckConflict
)

type chunk struct {
	kind       chunkKind
	base, a, b []string
	out        []string
}

type seg struct {
	same         bool
	baseLo, bHi  int
	otherLo, oHi int
}

// lcsMatch returns for each index of x the matched index in y, or -1.
func lcsMatch(x, y []string) []int {
	n, m := len(x), len(y)
	match := make([]int, n)
	for i := range match {
		match[i] = -1
	}
	if n == 0 || m == 0 {
		return match
	}
	if n*m > 40_000_000 { // too large: treat as unmatched, which yields a conflict
		return match
	}
	// trim common prefix/suffix to keep the table small
	p := 0
	for p < n && p < m && x[p] == y[p] {
		match[p] = p
		p++
	}
	s := 0
	for s < n-p && s < m-p && x[n-1-s] == y[m-1-s] {
		match[n-1-s] = m - 1 - s
		s++
	}
	xi, yi := x[p:n-s], y[p:m-s]
	n2, m2 := len(xi), len(yi)
	if n2 == 0 || m2 == 0 {
		return match
	}
	t := make([][]int32, n2+1)
	for i := range t {
		t[i] = make([]int32, m2+1)
	}
	for i := n2 - 1; i >= 0; i-- {
		for j := m2 - 1; j >= 0; j-- {
			if xi[i] == yi[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n2 && j < m2 {
		if xi[i] == yi[j] {
			match[p+i] = p + j
			i++
			j++
		} else if t[i+1][j] >= t[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return match
}

// diff2 segments base against other into same / changed runs (used only for counting).
func diff2(base, other []string) []seg {
	match := lcsMatch(base, other)
	var segs []seg
	bi, oi := 0, 0
	for bi < len(base) || oi < len(other) {
		if bi < len(base) && match[bi] == oi {
			for bi < len(base) && match[bi] == oi {
				bi++
				oi++
			}
			segs = append(segs, seg{same: true})
			continue
		}
		nb, no := bi, oi
		for nb < len(base) && match[nb] == -1 {
			nb++
		}
		if nb < len(base) {
			no = match[nb]
		} else {
			no = len(other)
		}
		if no < oi {
			no = oi
		}
		segs = append(segs, seg{same: false})
		bi, oi = nb, no
	}
	return segs
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diff3 aligns a and b to base on the elements both sides left alone and classifies the gaps.
func diff3(base, a, b []string) []chunk {
	ma, mb := lcsMatch(base, a), lcsMatch(base, b)
	var chunks []chunk
	bi, ai, bj := 0, 0, 0 // cursors in base, a, b
	flush := func(bHi, aHi, bjHi int) {
		cb, ca, cm := base[bi:bHi], a[ai:aHi], b[bj:bjHi]
		if len(cb) == 0 && len(ca) == 0 && len(cm) == 0 {
			return
		}
		switch {
		case eq(ca, cm):
			chunks = append(chunks, chunk{kind: ckSame, out: ca})
		case eq(cb, ca):
			chunks = append(chunks, chunk{kind: ckMine, out: cm})
		case eq(cb, cm):
			chunks = append(chunks, chunk{kind: ckTheirs, out: ca})
		default:
			chunks = append(chunks, chunk{kind: ckConflict, base: cb, a: ca, b: cm})
		}
	}
	for i := 0; i < len(base); i++ {
		if ma[i] >= 0 && mb[i] >= 0 && ma[i] >= ai && mb[i] >= bj {
			flush(i, ma[i], mb[i])
			chunks = append(chunks, chunk{kind: ckStable, out: []string{base[i]}})
			bi, ai, bj = i+1, ma[i]+1, mb[i]+1
		}
	}
	flush(len(base), len(a), len(b))
	return chunks
}
