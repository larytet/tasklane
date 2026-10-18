package mail_test

import (
	"testing"

	"github.com/larytet/tasklane/internal/mail"
)

type quoteCase struct{ name, in, want string }

var corpus = []quoteCase{
	{"our marker, quoted by the client", "Done, deployed.\n\nOn Tue, 3 Mar 2026 at 09:00, Tasklane <t@example.com> wrote:\n> -- Reply above this line --\n> Alice assigned you\n> ABC-1: Printer", "Done, deployed."},
	{"our marker, unquoted (Outlook)", "Will do.\n\n-- Reply above this line --\n\nAlice assigned you\nABC-1: Printer", "Will do."},
	{"gmail", "Looks good to me.\n\nOn Mon, Mar 2, 2026 at 4:15 PM Bob R. <bob@example.com> wrote:\n\n> Can you check the build?\n> It fails on main.\n", "Looks good to me."},
	{"gmail, attribution wrapped over two lines", "Yes.\n\nOn Mon, Mar 2, 2026 at 4:15 PM Bob Roberts-Smithington <bob.roberts@example.com> wrote:\n> hi\n", "Yes."},
	{"apple mail", "Sounds fine.\n\nOn 2 Mar 2026, at 16:15, Bob <bob@example.com> wrote:\n\n> Please look.\n> Thanks", "Sounds fine."},
	{"outlook original message", "Please go ahead.\n\n-----Original Message-----\nFrom: Bob\nSent: Monday, March 2, 2026 4:15 PM\nTo: Alice\nSubject: build\n\nCan you check?", "Please go ahead."},
	{"outlook header block without divider", "Fine by me.\n\nFrom: Bob R. <bob@example.com>\nSent: Monday, March 2, 2026 4:15 PM\nTo: Alice\nSubject: RE: build\n\nCan you check?", "Fine by me."},
	{"german", "Ja, passt.\n\nAm 02.03.2026 um 16:15 schrieb Bob R. <bob@example.com>:\n> Kannst du prüfen?", "Ja, passt."},
	{"french", "D'accord.\n\nLe 2 mars 2026 à 16:15, Bob <bob@example.com> a écrit :\n> Tu peux vérifier ?", "D'accord."},
	{"spanish", "Vale.\n\nEl lun, 2 mar 2026 a las 16:15, Bob <bob@example.com> escribió:\n> ¿Puedes revisar?", "Vale."},
	{"chinese", "好的。\n\n2026年3月2日 16:15，Bob <bob@example.com> 写道：\n> 你能检查吗？", "好的。"},
	{"trailing quote block without attribution", "Agreed.\n\n> earlier text\n> more earlier text\n", "Agreed."},
	{"signature delimiter", "Thanks.\n\n-- \nAlice K.\nSenior Engineer\n+1 555 0100", "Thanks."},
	{"signature and quote", "Okay.\n\n-- \nAlice\n\nOn Mon, Bob <b@example.com> wrote:\n> hi", "Okay."},
	{"forwarded block", "FYI see below.\n\n---------- Forwarded message ---------\nFrom: Bob\nDate: Mon\nSubject: x\n\nbody of the forward", "FYI see below."},
	{"quote by CRLF", "Fine.\r\n\r\nOn Mon, Bob <b@example.com> wrote:\r\n> quoted\r\n", "Fine."},
	{"plain message, nothing to strip", "Just a comment with no history.", "Just a comment with no history."},
	{"multi paragraph new text", "First point.\n\nSecond point.\n\nThird point.\n\nOn Mon, Bob <b@example.com> wrote:\n> old", "First point.\n\nSecond point.\n\nThird point."},
	{"inline reply kept whole", "On Mon, Bob <b@example.com> wrote:\n> Is the build green?\nYes, since noon.\n> And the deploy?\nTomorrow morning.", "On Mon, Bob <b@example.com> wrote:\n> Is the build green?\nYes, since noon.\n> And the deploy?\nTomorrow morning."},
	{"inline reply without attribution kept whole", "> Is it fixed?\nYes.\n> Released?\nNot yet.\nI will tell you.", "> Is it fixed?\nYes.\n> Released?\nNot yet.\nI will tell you."},
	{"the word wrote in ordinary text", "Bob wrote: the build is green, I checked it again and it still is.\nThen it broke.", "Bob wrote: the build is green, I checked it again and it still is.\nThen it broke."},
	{"a line that merely starts with On", "On Tuesday we ship.\nPlease prepare.", "On Tuesday we ship.\nPlease prepare."},
	{"dashes in a list are not a signature", "Steps:\n--\nrestart\n--\ncheck", "Steps:\n--\nrestart\n--\ncheck"},
	{"a message that is only a quote keeps its text", "> only quoted text here", "> only quoted text here"},
	{"empty", "", ""},
}

func TestMAIL12_QuoteStrippingCorpus(t *testing.T) {
	exact, falseStrip := 0, 0
	for _, c := range corpus {
		got, _ := mail.StripQuote(c.in)
		if got == c.want {
			exact++
			continue
		}
		t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		if len(got) < len(c.want) { // new text was removed: the failure that matters most
			falseStrip++
		}
	}
	if pct := 100 * float64(exact) / float64(len(corpus)); pct < 98 {
		t.Errorf("exact %.1f%% (%d of %d), need 98%%", pct, exact, len(corpus))
	}
	if falseStrip > 0 {
		t.Errorf("%d replies lost text the person wrote", falseStrip)
	}
}

func TestMAIL12_ReportsWhetherAnythingWasRemoved(t *testing.T) {
	if _, cut := mail.StripQuote("hi\n\nOn Mon, Bob <b@example.com> wrote:\n> x"); !cut {
		t.Error("a removal was not reported")
	}
	if _, cut := mail.StripQuote("hi there"); cut {
		t.Error("a removal was reported for plain text")
	}
}
