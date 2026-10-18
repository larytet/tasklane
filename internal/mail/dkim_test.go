package mail_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/mail"
)

type dnsMap map[string]string

func (m dnsMap) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := m[name]; ok {
		return []string{v}, nil
	}
	return nil, errors.New("no such record")
}

func fixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/dkim/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dns(t *testing.T) dnsMap {
	var m dnsMap
	if err := json.Unmarshal(fixture(t, "dns.json"), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var now = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func verify(t *testing.T, raw []byte, d dnsMap) mail.DKIMResult {
	t.Helper()
	rs := mail.VerifyDKIM(context.Background(), raw, d, now)
	if len(rs) != 1 {
		t.Fatalf("%d results", len(rs))
	}
	return rs[0]
}

// The fixtures were signed by dkimpy, an independent implementation, with RSA-2048 and ed25519 keys.
func TestMAIL10_DKIMVerifiesMessagesSignedByAnotherImplementation(t *testing.T) {
	d := dns(t)
	for _, f := range []string{"simple_rr", "simple_ss", "simple_rs", "folded_rr", "folded_ss", "folded_rs", "simple_ed", "folded_ed"} {
		if r := verify(t, fixture(t, f+".eml"), d); !r.Pass || r.Domain != "example.com" {
			t.Errorf("%s: %+v", f, r)
		}
	}
	// the same messages with bare line feeds (a store that normalised them) still verify
	lf := []byte(strings.ReplaceAll(string(fixture(t, "folded_rr.eml")), "\r\n", "\n"))
	if r := verify(t, lf, d); !r.Pass {
		t.Errorf("LF endings: %+v", r)
	}
}

func TestMAIL10_DKIMRefusesEverythingThatIsNotProof(t *testing.T) {
	d := dns(t)
	good := string(fixture(t, "simple_rr.eml"))
	cases := []struct {
		name, want string
		raw        string
		dns        dnsMap
	}{
		{"body changed", "body hash", strings.Replace(good, "A plain body.", "A forged body.", 1), d},
		{"signed header changed", "does not verify", strings.Replace(good, "Subject: Hello there", "Subject: Hello there!", 1), d},
		{"from changed", "does not verify", strings.Replace(good, "bob@example.com", "eve@example.com", 1), d},
		{"signature value damaged", "does not verify", strings.Replace(good, "b=", "b=AAAA", 1), d},
		{"no key in DNS", "could not be found", good, dnsMap{}},
		{"key revoked", "revoked", good, dnsMap{"rsa._domainkey.example.com": "v=DKIM1; k=rsa; p="}},
		{"key in testing mode", "testing mode", good, dnsMap{"rsa._domainkey.example.com": d["rsa._domainkey.example.com"] + "; t=y"}},
		{"another domain's key", "key is ed25519", good, dnsMap{"rsa._domainkey.example.com": d["ed._domainkey.example.com"]}},
		{"expired", "expired", strings.Replace(good, "DKIM-Signature: ", "DKIM-Signature: x=1000; ", 1), d},
		{"partial body signature", "part of the body", strings.Replace(good, "DKIM-Signature: ", "DKIM-Signature: l=5; ", 1), d},
		{"does not cover From", "From header", strings.Replace(good, "h=from : to :", "h=to :", 1), d},
	}
	for _, c := range cases {
		rs := mail.VerifyDKIM(context.Background(), []byte(c.raw), c.dns, now)
		if len(rs) != 1 || rs[0].Pass || !strings.Contains(rs[0].Reason, c.want) {
			t.Errorf("%s: %+v (want %q)", c.name, rs, c.want)
		}
	}
	// no signature at all, and no resolver
	if rs := mail.VerifyDKIM(context.Background(), []byte("From: a@example.com\r\n\r\nx"), d, now); len(rs) != 0 {
		t.Errorf("%+v", rs)
	}
	if rs := mail.VerifyDKIM(context.Background(), []byte(good), nil, now); len(rs) != 1 || rs[0].Pass {
		t.Errorf("%+v", rs)
	}
}

func TestMAIL10_DKIMLimitsWorkAndAlignment(t *testing.T) {
	d := dns(t)
	one := string(fixture(t, "simple_rr.eml"))
	sig := one[:strings.Index(one, "\r\nFrom:")+2]
	// twenty signatures: only the first five are examined
	many := strings.Repeat(sig, 20) + one[len(sig):]
	if rs := mail.VerifyDKIM(context.Background(), []byte(many), d, now); len(rs) != 5 {
		t.Errorf("%d signatures examined", len(rs))
	}
	for _, c := range []struct {
		signing, from string
		ok            bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "mail.example.com", true},
		{"EXAMPLE.com", "Mail.Example.COM", true},
		{"example.com.", "example.com", true},
		{"example.com", "example.com.evil.net", false},
		{"example.com", "notexample.com", false},
		{"mail.example.com", "example.com", false}, // a subdomain does not speak for its parent
		{"", "example.com", false},
	} {
		if mail.Aligned(c.signing, c.from) != c.ok {
			t.Errorf("%+v", c)
		}
	}
}
