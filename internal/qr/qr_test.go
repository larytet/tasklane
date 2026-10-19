package qr

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The worked example "HELLO WORLD" at 1-M (thonky.com QR tutorial): its sixteen data codewords and their ten EC codewords.
func TestReedSolomonKnownVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := rsRemainder(data, rsGenerator(10)); !bytes.Equal(got, want) {
		t.Fatalf("EC codewords %v, want %v", got, want)
	}
}

// Every version's block table must fill exactly the modules left after the function patterns.
func TestBlockTableFillsTheSymbol(t *testing.T) {
	for v := 1; v <= 20; v++ {
		c, free := newCode(v), 0
		for y := range c.fn {
			for x := range c.fn[y] {
				if !c.fn[y][x] {
					free++
				}
			}
		}
		b := blocksM[v]
		if total := dataCapacity(v) + (b.n1+b.n2)*b.ec; total != free/8 {
			t.Errorf("version %d: table has %d codewords, the symbol holds %d", v, total, free/8)
		}
	}
}

func TestEncodePicksTheSmallestVersion(t *testing.T) {
	for _, tc := range []struct {
		n, ver int
	}{{1, 1}, {14, 1}, {15, 2}, {106, 6}, {107, 7}, {180, 9}, {181, 10}, {666, 20}} {
		c, err := Encode(strings.Repeat("x", tc.n))
		if err != nil || c.Version != tc.ver || c.Size != 17+4*tc.ver {
			t.Errorf("%d bytes: version %v err %v, want version %d", tc.n, c, err, tc.ver)
		}
	}
	if _, err := Encode(strings.Repeat("x", 667)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("667 bytes: %v, want ErrTooLong", err)
	}
}

func TestFunctionPatterns(t *testing.T) {
	c, err := Encode("otpauth://totp/Tasklane:a%40example.com?secret=JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	m, n := c.Modules, c.Size
	for _, o := range [][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} { // finder rings: dark, light, dark 3×3 core
		x, y := o[0], o[1]
		if !m[y][x] || m[y+1][x+1] || !m[y+2][x+2] || !m[y+3][x+3] {
			t.Fatalf("finder at %v is wrong", o)
		}
	}
	for i := 8; i < n-8; i++ {
		if m[6][i] != (i%2 == 0) || m[i][6] != (i%2 == 0) {
			t.Fatalf("timing pattern broken at %d", i)
		}
	}
	if !m[n-8][8] {
		t.Fatal("dark module missing")
	}
}

func TestSVG(t *testing.T) {
	c, _ := Encode("a")
	s := c.SVG()
	if !strings.HasPrefix(s, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 29 29"`) || !strings.HasSuffix(s, `"/></svg>`) || !strings.Contains(s, "M4 4h1v1h-1z") {
		t.Fatalf("unexpected SVG: %.120s", s)
	}
}
