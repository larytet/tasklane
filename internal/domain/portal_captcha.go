package domain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// The portal's check before it mails a sign-in code. It is a proof of work, not a picture puzzle, because the portal runs no third-party
// scripts and sends nothing about its visitors elsewhere (SPEC 15, privacy). The server hands out a challenge it can recognize without storing
// it (a signed project, expiry, random nonce and difficulty); the browser searches for a counter that makes SHA-256 of "challenge:counter"
// start with the required number of zero bits; the server checks the signature, the expiry, the difficulty and that the challenge has not been
// used before, and only then does the work of mailing a code. A failed check says so and costs the address nothing.

const captchaTTL = 5 * time.Minute

type CaptchaIn struct {
	Challenge string `json:"challenge"`
	Solution  string `json:"solution"`
}

type PortalChallenge struct {
	Required  bool   `json:"required"`
	Challenge string `json:"challenge,omitempty"`
	Bits      int    `json:"bits,omitempty"`
}

func (s *Service) captchaSign(ctx context.Context, payload string) (string, error) {
	key, err := s.secretKey(ctx)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("portal-captcha|" + payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}

// NewPortalChallenge issues a challenge for the portal (or says none is needed).
func (s *Service) NewPortalChallenge(ctx context.Context, projectKey string) (PortalChallenge, error) {
	_, _, set, _, err := s.portalOf(ctx, projectKey)
	if err != nil {
		return PortalChallenge{}, err
	}
	if set.CaptchaBits == 0 {
		return PortalChallenge{}, nil
	}
	payload := fmt.Sprintf("%s|%d|%s|%d", strings.ToUpper(projectKey), s.Now().Add(captchaTTL).Unix(), randURLToken(12), set.CaptchaBits)
	sig, err := s.captchaSign(ctx, payload)
	if err != nil {
		return PortalChallenge{}, err
	}
	return PortalChallenge{Required: true, Challenge: base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig, Bits: set.CaptchaBits}, nil
}

func leadingZeroBits(sum [32]byte) int {
	n := 0
	for _, b := range sum {
		if b == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(b)
	}
	return n
}

var errCaptcha = invalid("the check did not pass: please try again")

// verifyCaptcha accepts a solved challenge exactly once.
func (s *Service) verifyCaptcha(ctx context.Context, projectKey string, set PortalPolicy, in *CaptchaIn) error {
	if set.CaptchaBits == 0 {
		return nil
	}
	if in == nil || len(in.Challenge) > 300 || len(in.Solution) > 24 || in.Solution == "" {
		return errCaptcha
	}
	enc, sig, ok := strings.Cut(in.Challenge, ".")
	if !ok {
		return errCaptcha
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return errCaptcha
	}
	want, err := s.captchaSign(ctx, string(raw))
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errCaptcha
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 || parts[0] != strings.ToUpper(projectKey) {
		return errCaptcha
	}
	exp, err1 := strconv.ParseInt(parts[1], 10, 64)
	diff, err2 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil || s.Now().Unix() > exp || diff < set.CaptchaBits { // a challenge made before the difficulty was raised no longer does
		return errCaptcha
	}
	if leadingZeroBits(sha256.Sum256([]byte(in.Challenge+":"+in.Solution))) < set.CaptchaBits {
		return errCaptcha
	}
	nonce := parts[2]
	if s.portalCount(ctx, "captcha", nonce, captchaTTL+time.Minute) > 0 {
		return errCaptcha
	}
	s.portalNote(ctx, "captcha", nonce)
	return nil
}
