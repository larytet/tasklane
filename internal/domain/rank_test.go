package domain

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestRankBetweenOrdersAndNeverRunsOut(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	list := []string{rankFor(1), rankFor(2), rankFor(3)}
	for i := 0; i < 3000; i++ {
		pos := rng.Intn(len(list) + 1) // insert anywhere, including both ends
		lo, hi := "", ""
		if pos > 0 {
			lo = list[pos-1]
		}
		if pos < len(list) {
			hi = list[pos]
		}
		r, ok := rankBetween(lo, hi)
		if !ok || (lo != "" && r <= lo) || (hi != "" && r >= hi) || strings.HasSuffix(r, "0") {
			t.Fatalf("between %q and %q gave %q %v", lo, hi, r, ok)
		}
		list = append(list[:pos], append([]string{r}, list[pos:]...)...)
	}
	if !sort.StringsAreSorted(list) {
		t.Fatal("ranks lost their order")
	}
	// hammering one spot grows the string by a digit now and then, never breaks
	lo, hi := "00000001", "00000002"
	for i := 0; i < 500; i++ {
		r, ok := rankBetween(lo, hi)
		if !ok || r <= lo || r >= hi {
			t.Fatalf("%q %q %q", lo, hi, r)
		}
		hi = r
	}
	if _, ok := rankBetween("b", "a"); ok {
		t.Fatal("an inverted range was accepted")
	}
	if _, ok := rankBetween("a", "a"); ok {
		t.Fatal("an empty range was accepted")
	}
	// spaced ranks are increasing and fixed width
	prev := ""
	for i := 0; i < 1000; i++ {
		r := spacedRank(i)
		if len(r) != 8 || r <= prev {
			t.Fatalf("%d %q", i, r)
		}
		prev = r
	}
}
