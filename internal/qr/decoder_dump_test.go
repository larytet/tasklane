package qr_test

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/qr"
)

// QR_DUMP=dir writes codes of many lengths and their text for an independent decoder (scripts/qr_decode.py, OpenCV): the unit tests check the encoder against the standard, this checks that a real reader reads what it writes
func TestDumpForIndependentDecoder(t *testing.T) {
	dir := os.Getenv("QR_DUMP")
	if dir == "" {
		t.Skip()
	}
	r := rand.New(rand.NewSource(1))
	n := 0
	emit := func(s string) {
		c, err := qr.Encode(s)
		if err != nil {
			return
		}
		os.WriteFile(fmt.Sprintf("%s/%03d.svg", dir, n), []byte(c.SVG()), 0o644)
		os.WriteFile(fmt.Sprintf("%s/%03d.txt", dir, n), []byte(s), 0o644)
		n++
	}
	emit("otpauth://totp/Tasklane:arkady.m%40qspark.co?algorithm=SHA1&digits=6&issuer=Tasklane&period=30&secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP")
	for l := 1; l <= 666; l += 5 { // every version from 1 to 20 gets codes at and around its capacity
		var b strings.Builder
		for b.Len() < l {
			b.WriteByte("abcdefghijklmnopqrstuvwxyz0123456789:/?&=%._-"[r.Intn(44)])
		}
		emit(b.String())
	}
	for i := 0; i < 40; i++ {
		emit(fmt.Sprintf("otpauth://totp/Tasklane:user%d%%40example.com?secret=%s&issuer=Tasklane", i, strings.Repeat("ABCDEFGH", 1+i%5)))
	}
}
