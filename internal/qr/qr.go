// Package qr encodes text as a QR code (ISO/IEC 18004) in byte mode at error-correction level M, versions 1–20,
// and draws it as SVG. It exists for the two-factor setup screen, so an authenticator can scan the otpauth address
// without the secret leaving the server for a third-party image service.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Code is a square of modules; Modules[y][x] is true for a dark module.
type Code struct {
	Version int
	Size    int
	Modules [][]bool
}

// ErrTooLong is returned for text that does not fit version 20 at level M (666 bytes).
var ErrTooLong = errors.New("qr: text too long")

// level M block structure per version: EC codewords per block, then (blocks, data codewords) for each group
var blocksM = [21]struct{ ec, n1, d1, n2, d2 int }{
	{},
	{10, 1, 16, 0, 0}, {16, 1, 28, 0, 0}, {26, 1, 44, 0, 0}, {18, 2, 32, 0, 0}, {24, 2, 43, 0, 0},
	{16, 4, 27, 0, 0}, {18, 4, 31, 0, 0}, {22, 2, 38, 2, 39}, {22, 3, 36, 2, 37}, {26, 4, 43, 1, 44},
	{30, 1, 50, 4, 51}, {22, 6, 36, 2, 37}, {22, 8, 37, 1, 38}, {24, 4, 40, 5, 41}, {24, 5, 41, 5, 42},
	{28, 7, 45, 3, 46}, {28, 10, 46, 1, 47}, {26, 9, 43, 4, 44}, {26, 3, 44, 11, 45}, {26, 3, 41, 13, 42},
}

func dataCapacity(ver int) int { b := blocksM[ver]; return b.n1*b.d1 + b.n2*b.d2 }

// Encode picks the smallest version that holds text and the mask with the lowest penalty.
func Encode(text string) (*Code, error) {
	data := []byte(text)
	ver := 0
	for v := 1; v <= 20; v++ {
		countBits := 8
		if v >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) <= 8*dataCapacity(v) {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, ErrTooLong
	}
	codewords := interleave(ver, dataCodewords(ver, data))
	var best *Code
	bestPenalty := -1
	for mask := 0; mask < 8; mask++ {
		c := newCode(ver)
		c.drawCodewords(codewords)
		c.applyMask(mask)
		c.drawFormat(mask)
		if p := c.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = c.Code, p
		}
	}
	return best, nil
}

// SVG draws the code with a four-module quiet zone, one path of dark modules on a white square.
func (c *Code) SVG() string {
	n := c.Size + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y, row := range c.Modules {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+4, y+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

type bitBuffer []bool

func (b *bitBuffer) put(v, n int) {
	for i := n - 1; i >= 0; i-- {
		*b = append(*b, v>>i&1 == 1)
	}
}

// dataCodewords lays out mode, count, bytes, terminator and padding.
func dataCodewords(ver int, data []byte) []byte {
	capBits := 8 * dataCapacity(ver)
	var bb bitBuffer
	bb.put(0b0100, 4)
	if ver >= 10 {
		bb.put(len(data), 16)
	} else {
		bb.put(len(data), 8)
	}
	for _, d := range data {
		bb.put(int(d), 8)
	}
	bb.put(0, min(4, capBits-len(bb)))
	bb.put(0, (8-len(bb)%8)%8)
	for pad := 0xEC; len(bb) < capBits; pad ^= 0xEC ^ 0x11 {
		bb.put(pad, 8)
	}
	out := make([]byte, len(bb)/8)
	for i, bit := range bb {
		if bit {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

// interleave splits data into blocks, adds Reed–Solomon codewords and interleaves both, column by column.
func interleave(ver int, data []byte) []byte {
	b := blocksM[ver]
	gen := rsGenerator(b.ec)
	var blocks, ecs [][]byte
	for i, off := 0, 0; i < b.n1+b.n2; i++ {
		n := b.d1
		if i >= b.n1 {
			n = b.d2
		}
		blk := data[off : off+n]
		off += n
		blocks = append(blocks, blk)
		ecs = append(ecs, rsRemainder(blk, gen))
	}
	var out []byte
	for i := 0; i < max(b.d1, b.d2); i++ {
		for _, blk := range blocks {
			if i < len(blk) {
				out = append(out, blk[i])
			}
		}
	}
	for i := 0; i < b.ec; i++ {
		for _, e := range ecs {
			out = append(out, e[i])
		}
	}
	return out
}

// GF(256) with the QR polynomial x^8+x^4+x^3+x^2+1
func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		z = z<<1 ^ (z>>7)*0x1D
		z ^= (y >> i & 1) * x
	}
	return z
}

// rsGenerator returns the coefficients of (x-α^0)…(x-α^(degree-1)), highest power first, leading 1 dropped.
func rsGenerator(degree int) []byte {
	g := make([]byte, degree)
	g[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range g {
			g[j] = gfMul(g[j], root)
			if j+1 < len(g) {
				g[j] ^= g[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return g
}

func rsRemainder(data, gen []byte) []byte {
	r := make([]byte, len(gen))
	for _, d := range data {
		f := d ^ r[0]
		copy(r, r[1:])
		r[len(r)-1] = 0
		for i, g := range gen {
			r[i] ^= gfMul(g, f)
		}
	}
	return r
}

// builder holds a code under construction and which modules belong to function patterns.
type builder struct {
	*Code
	fn [][]bool
}

func newCode(ver int) *builder {
	size := 17 + 4*ver
	c := &builder{Code: &Code{Version: ver, Size: size}}
	c.Modules, c.fn = make([][]bool, size), make([][]bool, size)
	for i := range c.Modules {
		c.Modules[i], c.fn[i] = make([]bool, size), make([]bool, size)
	}
	for i := 0; i < size; i++ {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	c.finder(3, 3)
	c.finder(size-4, 3)
	c.finder(3, size-4)
	pos := alignmentPositions(ver)
	for i, x := range pos {
		for j, y := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue // the finders are there
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.set(x+dx, y+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserves the format areas; redrawn once the mask is known
	if ver >= 7 {
		rem := ver
		for i := 0; i < 12; i++ {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		bits := ver<<12 | rem
		for i := 0; i < 18; i++ {
			a, b := size-11+i%3, i/3
			c.set(a, b, bits>>i&1 == 1)
			c.set(b, a, bits>>i&1 == 1)
		}
	}
	return c
}

func (c *builder) set(x, y int, dark bool) { c.Modules[y][x] = dark; c.fn[y][x] = true }

// finder draws a finder pattern centred on (x, y) with its separator.
func (c *builder) finder(x, y int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			xx, yy := x+dx, y+dy
			if xx >= 0 && xx < c.Size && yy >= 0 && yy < c.Size {
				d := max(abs(dx), abs(dy))
				c.set(xx, yy, d != 2 && d != 4)
			}
		}
	}
}

func alignmentPositions(ver int) []int {
	if ver == 1 {
		return nil
	}
	n := ver/7 + 2
	step := (ver*4 + n*2 + 1) / (n*2 - 2) * 2
	pos := make([]int, n)
	pos[0] = 6
	for i, p := n-1, ver*4+10; i >= 1; i, p = i-1, p-step {
		pos[i] = p
	}
	return pos
}

func (c *builder) drawFormat(mask int) {
	data := mask // level M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := 0; i <= 5; i++ {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		c.set(c.Size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, c.Size-15+i, bit(i))
	}
	c.set(8, c.Size-8, true) // the dark module
}

// drawCodewords places the bits in the two-column zigzag from the bottom-right corner.
func (c *builder) drawCodewords(cw []byte) {
	i := 0
	for right := c.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // skip the vertical timing pattern
		}
		for vert := 0; vert < c.Size; vert++ {
			for j := 0; j < 2; j++ {
				x, y := right-j, vert
				if (right+1)&2 == 0 {
					y = c.Size - 1 - vert
				}
				if !c.fn[y][x] && i < len(cw)*8 {
					c.Modules[y][x] = cw[i/8]>>(7-i%8)&1 == 1
					i++
				}
			}
		}
	}
}

func (c *builder) applyMask(mask int) {
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			var inv bool
			switch mask {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv && !c.fn[y][x] {
				c.Modules[y][x] = !c.Modules[y][x]
			}
		}
	}
}

// penalty scores a masked code by the four rules of the standard: runs, 2×2 blocks, finder look-alikes, balance.
func (c *builder) penalty() int {
	n, p, dark := c.Size, 0, 0
	at := func(x, y int, col bool) bool {
		if col {
			return c.Modules[x][y]
		}
		return c.Modules[y][x]
	}
	for _, col := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 0
			for x := 0; x < n; x++ {
				if x > 0 && at(x, y, col) == at(x-1, y, col) {
					run++
				} else {
					run = 1
				}
				if run == 5 {
					p += 3
				} else if run > 5 {
					p++
				}
				// 1:1:3:1:1 with four light modules on either side
				if x+10 < n {
					pat := [11]bool{true, false, true, true, true, false, true, false, false, false, false}
					fwd, back := true, true
					for k := 0; k < 11; k++ {
						v := at(x+k, y, col)
						fwd = fwd && v == pat[k]
						back = back && v == pat[10-k]
					}
					if fwd {
						p += 40
					}
					if back {
						p += 40
					}
				}
			}
		}
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c.Modules[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				v := c.Modules[y][x]
				if v == c.Modules[y][x+1] && v == c.Modules[y+1][x] && v == c.Modules[y+1][x+1] {
					p += 3
				}
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10)+total-1)/total - 1
	return p + max(k, 0)*10
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
