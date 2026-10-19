package domain_test

import (
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

func solve(ch domain.PortalChallenge) string { return solveBits(ch.Challenge, ch.Bits) }

func solveBits(challenge string, want int) string {
	for n := 0; ; n++ {
		s := strconv.Itoa(n)
		sum := sha256.Sum256([]byte(challenge + ":" + s))
		z := 0
		for _, b := range sum {
			if b == 0 {
				z += 8
				continue
			}
			for m := byte(0x80); m != 0 && b&m == 0; m >>= 1 {
				z++
			}
			break
		}
		if z >= want {
			return s
		}
	}
}

// PORT-05 (check): the sign-in check is off by default; when on, a code is mailed only for a solved, unexpired, unused challenge of this portal
// at the portal's difficulty; a failed check costs the address nothing and says so; the honeypot is still silent.
func TestPORT05_SignInProofOfWork(t *testing.T) {
	p := newPortal(t)
	if ch, err := p.Svc.NewPortalChallenge(ctx, "ABC"); err != nil || ch.Required {
		t.Fatalf("off by default: %+v %v", ch, err)
	}
	if info, _ := p.Svc.PortalInfoFor(ctx, "ABC"); info.Check {
		t.Fatal("the portal says it asks for a check")
	}
	for _, bad := range []int{1, 7, 25} {
		if _, err := p.Svc.SetPortalPolicy(ctx, p.Admin, "ABC", domain.PortalPolicy{Enabled: true, CaptchaBits: bad}); !isInvalid(err) {
			t.Fatalf("difficulty %d accepted: %v", bad, err)
		}
	}
	p.policy(t, domain.PortalPolicy{Enabled: true, CaptchaBits: 10})
	if info, _ := p.Svc.PortalInfoFor(ctx, "ABC"); !info.Check {
		t.Fatal("the portal does not say it asks for a check")
	}
	addr := "cora@acme.example"
	ask := func(c *domain.CaptchaIn) error {
		return p.Svc.PortalRequestCodeChecked(ctx, "ABC", addr, "", c, domain.ClientMeta{IP: "198.51.100.9"})
	}
	if err := ask(nil); !isInvalid(err) {
		t.Fatalf("no check: %v", err)
	}
	ch, err := p.Svc.NewPortalChallenge(ctx, "ABC")
	if err != nil || !ch.Required || ch.Bits != 10 || ch.Challenge == "" {
		t.Fatalf("challenge: %+v %v", ch, err)
	}
	sol := solve(ch)
	if err := ask(&domain.CaptchaIn{Challenge: ch.Challenge, Solution: sol + "x"}); !isInvalid(err) {
		t.Fatalf("a wrong solution: %v", err)
	}
	forged := strings.Replace(ch.Challenge, ".", "A.", 1)
	if err := ask(&domain.CaptchaIn{Challenge: forged, Solution: sol}); !isInvalid(err) {
		t.Fatalf("a tampered challenge: %v", err)
	}
	if err := ask(&domain.CaptchaIn{Challenge: "x.y", Solution: "1"}); !isInvalid(err) {
		t.Fatalf("a made-up challenge: %v", err)
	}
	if p.mails(t, addr) != 0 {
		t.Fatal("a code was sent for a failed check")
	}
	if err := ask(&domain.CaptchaIn{Challenge: ch.Challenge, Solution: sol}); err != nil {
		t.Fatalf("a solved check: %v", err)
	}
	if p.mails(t, addr) != 1 {
		t.Fatalf("%d mails after a solved check", p.mails(t, addr))
	}
	if err := ask(&domain.CaptchaIn{Challenge: ch.Challenge, Solution: sol}); !isInvalid(err) {
		t.Fatalf("a challenge used twice: %v", err)
	}
	// failed checks did not use up the address's allowance
	for i := 0; i < 8; i++ {
		ask(&domain.CaptchaIn{Challenge: ch.Challenge, Solution: "nope"})
	}
	ch2, _ := p.Svc.NewPortalChallenge(ctx, "ABC")
	if err := ask(&domain.CaptchaIn{Challenge: ch2.Challenge, Solution: solve(ch2)}); err != nil {
		t.Fatalf("after failed attempts: %v", err)
	}
	// expiry
	old, _ := p.Svc.NewPortalChallenge(ctx, "ABC")
	osol := solve(old)
	p.Clock.T = p.Clock.T.Add(6 * time.Minute)
	if err := ask(&domain.CaptchaIn{Challenge: old.Challenge, Solution: osol}); !isInvalid(err) {
		t.Fatalf("an expired challenge: %v", err)
	}
	// a challenge made before the difficulty was raised no longer does
	easy, _ := p.Svc.NewPortalChallenge(ctx, "ABC")
	esol := solve(easy)
	p.policy(t, domain.PortalPolicy{Enabled: true, CaptchaBits: 14})
	if err := ask(&domain.CaptchaIn{Challenge: easy.Challenge, Solution: esol}); !isInvalid(err) {
		t.Fatalf("an easier challenge after the difficulty was raised: %v", err)
	}
	// even with work done to the new difficulty, the old challenge itself is not accepted
	if err := ask(&domain.CaptchaIn{Challenge: easy.Challenge, Solution: solveBits(easy.Challenge, 14)}); !isInvalid(err) {
		t.Fatalf("a challenge issued at the old difficulty: %v", err)
	}
	// a challenge the server never issued, solved honestly, is not accepted: only its own signature makes one
	payload := "ABC|" + strconv.FormatInt(p.Clock.T.Add(time.Minute).Unix(), 10) + "|mine|14"
	made := base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".AAAA"
	if err := ask(&domain.CaptchaIn{Challenge: made, Solution: solveBits(made, 14)}); !isInvalid(err) {
		t.Fatalf("a challenge made by the client: %v", err)
	}
	// the honeypot is as silent as ever, and turning the check off removes the demand
	if err := p.Svc.PortalRequestCodeChecked(ctx, "ABC", "bot@x.example", "i-am-a-bot", nil, meta); err != nil {
		t.Fatalf("the honeypot answered differently: %v", err)
	}
	p.policy(t, domain.PortalPolicy{Enabled: true})
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "dave@acme.example", "", meta); err != nil {
		t.Fatalf("off again: %v", err)
	}
}
