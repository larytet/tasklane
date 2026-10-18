package git

import "regexp"

// Redact removes what looks like a secret from text that is about to be stored (SPEC 18.4, data minimization): a commit
// subject or a pull request title is stored and shown, and people do paste tokens into them. The patterns are specific to
// well-known credential shapes, so ordinary words, hashes and issue keys are left alone.
func Redact(s string) string {
	for _, p := range secretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

const redacted = "[redacted]"

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*`), redacted},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}\b`), redacted},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{40,}\b`), redacted},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}\b`), redacted},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), redacted},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), redacted},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`), redacted},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), redacted},
	// credentials inside a URL: keep the host, drop the user and password
	{regexp.MustCompile(`(?i)(https?://)[^\s/@:]+:[^\s/@]+@`), "${1}" + redacted + "@"},
	// "Authorization: Bearer abc123": the scheme stays, the credential after it goes
	{regexp.MustCompile(`(?i)\b(authorization)\b(\s*[:=]\s*)(bearer|basic|token)\s+[^\s"']{6,}`), "${1}${2}${3} " + redacted},
	// "password = hunter2", "token: abc123def": keep the word, drop the value
	{regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|authorization)\b(\s*[:=]\s*)["']?[^\s"']{6,}`), "${1}${2}" + redacted},
}
