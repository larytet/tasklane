package git_test

import (
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/git"
)

// GIT-17: secret-looking strings are removed from text before it is stored; ordinary text is not touched.
func TestGIT17_RedactRemovesCredentialsAndLeavesOrdinaryTextAlone(t *testing.T) {
	secrets := map[string]string{
		"a GitHub token":        "fix auth, token ghp_" + strings.Repeat("a1B2c3", 7) + " was leaked",
		"a fine-grained token":  "use github_pat_" + strings.Repeat("A1b2C3d4E5", 5) + "_xyz now",
		"a GitLab token":        "glpat-" + strings.Repeat("x9Y8", 6),
		"an AWS key":            "key AKIAIOSFODNN7EXAMPLE in config",
		"a Slack token":         "xoxb-1234567890-abcdefghij",
		"an API key":            "sk-" + strings.Repeat("Ab3", 10),
		"a JWT":                 "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"a private key":         "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA...",
		"a password assignment": "set password = hunter2hunter2 for the db",
		"a token assignment":    "api_key: 0123456789abcdef",
		"a header":              "Authorization: Bearer abcdef123456",
		"credentials in a URL":  "clone https://deploy:s3cr3tpass@git.example.com/acme/shop.git",
	}
	for name, in := range secrets {
		out := git.Redact(in)
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("%s was not redacted: %q", name, out)
		}
		for _, leak := range []string{"hunter2hunter2", "s3cr3tpass", "0123456789abcdef", "AKIAIOSFODNN7EXAMPLE", "xoxb-1234567890", "abcdef123456", "MIIEowIBAAKCAQEA", "dBjftJeZ4CVPmB92K27uhbUJU1p1r"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: %q still contains %q", name, out, leak)
			}
		}
	}
	// the word and the surrounding text survive, so the message still reads
	if got := git.Redact("set password = hunter2hunter2 for the db"); got != "set password = [redacted] for the db" {
		t.Errorf("%q", got)
	}
	if got := git.Redact("clone https://deploy:s3cr3tpass@git.example.com/x"); got != "clone https://[redacted]@git.example.com/x" {
		t.Errorf("%q", got)
	}
	// text that merely looks technical is left as it is
	for _, in := range []string{
		"ABC-123 fix token expiry bug", "bump UTF-8 handling", "revert 3f786850e387550fdab836ed7e6dc881de23001b", "rotate the password policy",
		"add secret scanning docs", "feat: authorization header parsing", "ghp is not a token", "support https://example.com/a/b?x=1", "the sk- prefix", "token=short",
		"user:pass is a pattern, not a secret", "release 1.2.3-rc.1 build abcdef0123456789",
	} {
		if out := git.Redact(in); out != in {
			t.Errorf("%q was changed to %q", in, out)
		}
	}
}
