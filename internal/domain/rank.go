package domain

import "strings"

const rankDigits = "0123456789abcdefghijklmnopqrstuvwxyz"

func rankIdx(c byte) int { return strings.IndexByte(rankDigits, c) }

// rankBetween returns a string that sorts (bytewise) strictly between lo and hi; "" means no bound on that side.
// Strings use the digits 0-9a-z and never end in '0', so there is always room between two distinct ranks.
func rankBetween(lo, hi string) (string, bool) {
	if hi != "" && lo >= hi {
		return "", false
	}
	var out []byte
	hiOpen := hi == ""
	for i := 0; i < 200; i++ {
		l := 0
		if i < len(lo) {
			l = rankIdx(lo[i])
		}
		h := 36
		if !hiOpen {
			if i < len(hi) {
				h = rankIdx(hi[i])
			} else { // hi is a prefix of what we built: cannot happen while lo < hi
				return "", false
			}
		}
		if h-l > 1 {
			return string(append(out, rankDigits[(l+h)/2])), true
		}
		out = append(out, rankDigits[l])
		if h-l == 1 {
			hiOpen = true // we are already below hi; the rest is bounded only by lo
		}
	}
	return "", false
}

// spacedRank gives the i-th (0-based) of n evenly spread ranks of fixed width, used to rebalance a column.
func spacedRank(i int) string {
	v := (i + 1) * 46656 // 36^3
	b := make([]byte, 8)
	for p := 7; p >= 0; p-- {
		b[p] = rankDigits[v%36]
		v /= 36
	}
	return string(b)
}
