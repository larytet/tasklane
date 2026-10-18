package mail

import (
	"regexp"
	"strings"
)

// StripQuote returns what the person wrote: the quoted history, the reply-marker line and the signature are removed with
// conservative rules, and anything unclear is kept. An inline reply (answers written between quoted lines) is kept whole.
// The second result says whether anything was removed.
func StripQuote(text string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	cut := len(lines)
	// 1. the marker our own notifications carry: everything from it on is history
	for i, l := range lines {
		if reMarker.MatchString(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "> "))) {
			// the marker sits at the top of our notification (quoted or not), so the person's text is above it
			cut = i
			break
		}
	}
	// 2. an attribution line ("On ... wrote:") or an original-message block, followed only by quoted text and blanks
	if cut == len(lines) {
		for i, l := range lines {
			t := strings.TrimSpace(l)
			if (reWrote.MatchString(t) || reOrig.MatchString(t) || reHeaderBlock(lines, i)) && onlyQuoteAfter(lines[i+1:], reOrig.MatchString(t) || reHeaderBlock(lines, i)) {
				cut = i
				break
			}
		}
	}
	// 3. a trailing block of ">" lines (top-posting style leaves them at the end)
	if cut == len(lines) {
		j := len(lines)
		for j > 0 && (strings.TrimSpace(lines[j-1]) == "" || strings.HasPrefix(strings.TrimSpace(lines[j-1]), ">")) {
			j--
		}
		if j < len(lines) && hasQuote(lines[j:]) {
			cut = j
		}
	}
	kept := lines[:cut]
	for len(kept) > 0 && (strings.TrimSpace(kept[len(kept)-1]) == "" || (cut < len(lines) && reWrote.MatchString(strings.TrimSpace(kept[len(kept)-1])))) {
		kept = kept[:len(kept)-1] // the attribution line that introduces the quote goes with it
	}
	// 4. the signature delimiter, only when it is the last "-- " line
	for i := len(kept) - 1; i >= 0 && i >= len(kept)-12; i-- {
		if kept[i] == "-- " { // exactly dash, dash, space: a bare "--" is too often ordinary text, and when unsure the text stays
			kept = kept[:i]
			break
		}
	}
	out := strings.TrimSpace(strings.Join(kept, "\n"))
	if out == "" { // never return nothing when something was written: keep the original
		return strings.TrimSpace(text), false
	}
	return out, out != strings.TrimSpace(text)
}

func hasQuote(ls []string) bool {
	for _, l := range ls {
		if strings.HasPrefix(strings.TrimSpace(l), ">") {
			return true
		}
	}
	return false
}

var (
	reMarker = regexp.MustCompile(`^-{2,}\s*(Reply above this line|Responda acima desta linha|Antworten Sie oberhalb dieser Zeile|Répondez au-dessus de cette ligne)\s*-{2,}$`)
	// "On Tue, 3 Mar 2026 at 10:00, A <a@b> wrote:" in several languages, possibly wrapped over two lines
	reWrote = regexp.MustCompile(`(?i)^(on .{5,200} wrote:|le .{5,200} a écrit\s*:|am .{5,200} schrieb .{0,100}:|el .{5,200} escribió:|il .{5,200} ha scritto:|em .{5,200} escreveu:|.{5,200} schreef .{0,100}:|.{5,200} 写道[：:]|.{5,200} wrote:)$`)
	reOrig  = regexp.MustCompile(`(?i)^(-{2,}\s*original message\s*-{2,}|-{2,}\s*forwarded message\s*-{2,}|-{2,}\s*ursprüngliche nachricht\s*-{2,}|-{2,}\s*message d'origine\s*-{2,}|_{10,}|begin forwarded message:?)$`)
	reFrom  = regexp.MustCompile(`(?i)^(from|de|von|da|van)\s*:\s*\S`)
	reSent  = regexp.MustCompile(`(?i)^(sent|date|envoyé|gesendet|enviado|inviato|verzonden)\s*:\s*\S`)
)

// reHeaderBlock detects the Outlook block "From: ... / Sent: ... / To: ... / Subject: ..." (two consecutive header-like lines).
func reHeaderBlock(lines []string, i int) bool {
	if i+1 >= len(lines) {
		return false
	}
	return reFrom.MatchString(strings.TrimSpace(lines[i])) && reSent.MatchString(strings.TrimSpace(lines[i+1]))
}

// onlyQuoteAfter: after an attribution line the rest must be quoted ("> ...") or blank for the cut to be safe: if the person
// wrote below the attribution (an inline reply), the text is kept. After an original-message divider or an Outlook header
// block the rest is the original whatever it looks like.
func onlyQuoteAfter(rest []string, blockStyle bool) bool {
	if blockStyle {
		return true
	}
	seen := false
	for _, l := range rest {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
		case strings.HasPrefix(t, ">"):
			seen = true
		case !seen && len(t) < 200 && !strings.HasPrefix(t, ">") && reWrote.MatchString(t): // a wrapped second line
		default:
			return false
		}
	}
	return true
}
