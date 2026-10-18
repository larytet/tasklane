package web_test

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// UI-02: a contrast checker over the token pairs (DESIGN 1). A failing pair fails the build.

func hexRGB(s string) (r, g, b float64, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) == 3 { // #abc means #aabbcc
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	if len(s) != 6 {
		return
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return
	}
	return float64(v >> 16 & 255), float64(v >> 8 & 255), float64(v & 255), true
}

func lum(hex string) float64 {
	r, g, b, _ := hexRGB(hex)
	ch := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(r) + 0.7152*ch(g) + 0.0722*ch(b)
}

func ratio(a, b string) float64 {
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// tokens reads the custom properties of one theme block from app.css.
func tokens(t *testing.T, selector string) map[string]string {
	b, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	i := strings.Index(css, selector+"{")
	if i < 0 {
		t.Fatalf("no %s block", selector)
	}
	j := strings.Index(css[i:], "}")
	block := css[i+len(selector)+1 : i+j]
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`--([a-z-]+):\s*([^;]+);`).FindAllStringSubmatch(block, -1) {
		out[m[1]] = strings.TrimSpace(m[2])
	}
	return out
}

func TestUI02_TokenContrast(t *testing.T) {
	dark := tokens(t, ":root")
	light := tokens(t, ":root[data-theme=light]")
	// the light block overrides; anything it does not set is inherited from dark
	for k, v := range dark {
		if _, ok := light[k]; !ok {
			light[k] = v
		}
	}
	type pair struct {
		fg, bg string
		min    float64
		what   string
	}
	pairs := []pair{
		{"text", "surface", 4.5, "body text on a page"}, {"text", "sunken", 4.5, "body text on the canvas"}, {"text", "raised", 4.5, "text on a raised surface"}, {"text", "overlay", 4.5, "text in a dialog"},
		{"subtle", "surface", 4.5, "secondary text"}, {"subtle", "raised", 4.5, "secondary text on raised"}, {"subtle", "overlay", 4.5, "secondary text in a dialog"},
		{"subtlest", "surface", 4.5, "tertiary text"}, {"subtlest", "sunken", 4.5, "tertiary text on the canvas"}, {"subtlest", "overlay", 4.5, "tertiary text in a dialog"},
		{"accent", "surface", 4.5, "links"}, {"accent", "overlay", 4.5, "links in a dialog"},
		{"on-accent", "accent", 4.5, "primary button label"},
		{"success", "success-bg", 4.5, "Done lozenge"}, {"warning", "warning-bg", 4.5, "warning banner"}, {"danger", "danger-bg", 4.5, "error banner"},
		{"info", "info-bg", 4.5, "info banner and field note"}, {"accent", "accent-bg", 4.5, "In Progress lozenge"}, {"subtle", "todo-bg", 4.5, "To Do lozenge"},
		{"success", "surface", 4.5, "success text"}, {"danger", "surface", 4.5, "error text"}, {"warning", "surface", 4.5, "warning text"},
		{"border-input", "surface", 3.0, "input borders (UI component, 3:1)"}, {"border-input", "sunken", 3.0, "input borders on the canvas"},
	}
	for name, th := range map[string]map[string]string{"dark": dark, "light": light} {
		for _, p := range pairs {
			fg, bg := th[p.fg], th[p.bg]
			if _, _, _, ok := hexRGB(fg); !ok {
				t.Errorf("%s: token --%s = %q is not a hex colour", name, p.fg, fg)
				continue
			}
			if _, _, _, ok := hexRGB(bg); !ok {
				t.Errorf("%s: token --%s = %q is not a hex colour", name, p.bg, bg)
				continue
			}
			if r := ratio(fg, bg); r < p.min {
				t.Errorf("%s theme: %s (--%s %s on --%s %s) has contrast %.2f, needs %.1f", name, p.what, p.fg, fg, p.bg, bg, r, p.min)
			}
		}
	}
	// the checker itself is right: known reference values
	if r := ratio("#000000", "#ffffff"); math.Abs(r-21) > 0.01 {
		t.Fatalf("black on white is %.2f, want 21", r)
	}
	if r := ratio("#777777", "#ffffff"); r < 4.4 || r > 4.5 {
		t.Fatalf("#777 on white is %.2f, want about 4.48", r)
	}
	// the DESIGN figures for text on surface (12.3 dark, 16.2 light) are reproduced
	if r := ratio(dark["text"], dark["surface"]); r < 12.0 || r > 12.6 {
		t.Fatalf("dark text on surface %.2f, DESIGN says 12.3", r)
	}
	if r := ratio(light["text"], light["surface"]); r < 15.8 || r > 16.6 {
		t.Fatalf("light text on surface %.2f, DESIGN says 16.2", r)
	}
}

// A pair that fails must really fail the build: prove the checker rejects a bad palette.
func TestUI02_CheckerRejectsBadPairs(t *testing.T) {
	if ratio("#8d95a1", "#ffffff") >= 4.5 {
		t.Fatal("expected the dark-theme tertiary grey on white to fail 4.5:1")
	}
}
