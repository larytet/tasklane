package domain_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

type portal struct{ *testutil.Env }

func newPortal(t *testing.T) *portal {
	e := testutil.New(t)
	e.Project()
	e.Svc.Mail = &domain.MailConfig{From: "Tasklane <t@example.com>", Domain: "tasklane.example.com", BaseURL: "https://tasklane.example.com"}
	p := &portal{e}
	p.policy(t, domain.PortalPolicy{Enabled: true, Welcome: "How can we help?"})
	return p
}

func (p *portal) policy(t *testing.T, in domain.PortalPolicy) {
	t.Helper()
	if _, err := p.Svc.SetPortalPolicy(ctx, p.Admin, "ABC", in); err != nil {
		t.Fatal(err)
	}
}

var meta = domain.ClientMeta{IP: "203.0.113.7"}

// lastCode reads the newest sign-in mail to the address: the six digit code and the link token.
func (p *portal) lastCode(t *testing.T, email string) (code, token string) {
	t.Helper()
	var body string
	if err := p.Store.Pool.QueryRow(ctx, `SELECT body_text FROM mail_message WHERE kind='portal-code' AND lower(recipient)=lower($1) ORDER BY id DESC LIMIT 1`, email).Scan(&body); err != nil {
		t.Fatalf("no sign-in mail to %s: %v", email, err)
	}
	code = regexp.MustCompile(`is (\d{6})`).FindStringSubmatch(body)[1]
	token = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindStringSubmatch(body)[1]
	return
}

func (p *portal) mails(t *testing.T, email string) int {
	var n int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_message WHERE kind='portal-code' AND lower(recipient)=lower($1)`, email).Scan(&n)
	return n
}

// PORT-01: a code signs in once; a link signs in once; wrong tries spoil the code; it expires; nothing says whether an address is known.
func TestPORT01_CodeAndLinkSignIn(t *testing.T) {
	p := newPortal(t)
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "Cora@Example.com", "", meta); err != nil {
		t.Fatal(err)
	}
	code, _ := p.lastCode(t, "cora@example.com")
	if _, _, err := p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", "000000", meta); !isInvalid(err) {
		t.Fatalf("a wrong code: %v", err)
	}
	tok, c, err := p.Svc.PortalVerify(ctx, "ABC", "CORA@example.com", code, meta)
	if err != nil || tok == "" || c.Email != "cora@example.com" {
		t.Fatalf("verify: %+v %v", c, err)
	}
	if _, _, err := p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", code, meta); !isInvalid(err) {
		t.Fatalf("a code worked twice: %v", err)
	}
	if got, _, err := p.Svc.CustomerByToken(ctx, tok); err != nil || got.ID != c.ID {
		t.Fatalf("session: %+v %v", got, err)
	}
	// the link works once, and asking again spoils the earlier code
	p.Svc.PortalRequestCode(ctx, "ABC", "cora@example.com", "", meta)
	code1, _ := p.lastCode(t, "cora@example.com")
	p.Svc.PortalRequestCode(ctx, "ABC", "cora@example.com", "", meta)
	code2, link2 := p.lastCode(t, "cora@example.com")
	if _, _, err := p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", code1, meta); !isInvalid(err) {
		t.Fatalf("an older code survived a newer request: %v", err)
	}
	_ = code2
	tok2, c2, err := p.Svc.PortalVerifyLink(ctx, link2, meta)
	if err != nil || c2.ID != c.ID || tok2 == tok {
		t.Fatalf("link: %+v %v", c2, err)
	}
	if _, _, err := p.Svc.PortalVerifyLink(ctx, link2, meta); !isInvalid(err) {
		t.Fatalf("a link worked twice: %v", err)
	}
	// five wrong tries spoil a code, even for the right one afterwards
	p.Svc.PortalRequestCode(ctx, "ABC", "cora@example.com", "", meta)
	good, _ := p.lastCode(t, "cora@example.com")
	for i := 0; i < 5; i++ {
		p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", "111111", meta)
	}
	if _, _, err := p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", good, meta); err == nil {
		t.Fatal("the right code worked after five wrong tries")
	}
	// expiry
	p.Svc.PortalRequestCode(ctx, "ABC", "dana@example.com", "", meta)
	dcode, _ := p.lastCode(t, "dana@example.com")
	p.Clock.T = p.Clock.T.Add(11 * time.Minute)
	if _, _, err := p.Svc.PortalVerify(ctx, "ABC", "dana@example.com", dcode, meta); !isInvalid(err) {
		t.Fatalf("an expired code: %v", err)
	}
	// a made-up address, a bad address, a portal that is not there
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "not an address", "", meta); !isInvalid(err) {
		t.Fatalf("bad address: %v", err)
	}
	if err := p.Svc.PortalRequestCode(ctx, "ZZZ", "x@example.com", "", meta); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no such portal: %v", err)
	}
	// a portal that is switched off is the same as no portal
	p.policy(t, domain.PortalPolicy{Enabled: false})
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "x@example.com", "", meta); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a closed portal: %v", err)
	}
	if _, err := p.Svc.PortalInfoFor(ctx, "ABC"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a closed portal's page: %v", err)
	}
}

// PORT-02: a customer session opens nothing of the staff side and staff sessions open nothing of the portal: they are different stores.
func TestPORT02_SessionsAreSeparate(t *testing.T) {
	p := newPortal(t)
	p.Svc.PortalRequestCode(ctx, "ABC", "cora@example.com", "", meta)
	code, _ := p.lastCode(t, "cora@example.com")
	tok, _, err := p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", code, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.Authenticate(ctx, tok); err == nil {
		t.Fatal("a customer session authenticated as staff")
	}
	staff, err := p.Svc.CreateAPIToken(ctx, p.Alice.ID, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Svc.CustomerByToken(ctx, staff); err == nil {
		t.Fatal("a staff token opened the portal")
	}
	st, _, err := p.Svc.Login(ctx, "alice@example.com", "password123", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Svc.CustomerByToken(ctx, st); err == nil {
		t.Fatal("a staff session opened the portal")
	}
	// a customer is not a staff account: no row among the people, and an address that is also staff stays two identities
	var n int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE lower(email)='cora@example.com'`).Scan(&n)
	if n != 0 {
		t.Fatalf("a customer became a staff account: %d", n)
	}
	// logout ends the session; an expired one is gone
	p.Svc.PortalLogout(ctx, tok)
	if _, _, err := p.Svc.CustomerByToken(ctx, tok); err == nil {
		t.Fatal("a session survived logout")
	}
}

// PORT-05: limits per address and per network address, the honeypot, the block list and the allow list, and failures lock.
func TestPORT05_AbuseControls(t *testing.T) {
	p := newPortal(t)
	// per address: five codes an hour, then a limit that says so
	for i := 0; i < 5; i++ {
		if err := p.Svc.PortalRequestCode(ctx, "ABC", "flood@example.com", "", domain.ClientMeta{IP: "198.51.100.1"}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	var locked *domain.LockedError
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "flood@example.com", "", domain.ClientMeta{IP: "198.51.100.2"}); !errors.As(err, &locked) {
		t.Fatalf("the sixth code: %v", err)
	}
	// per network address: thirty an hour across addresses
	for i := 0; i < 30; i++ {
		p.Svc.PortalRequestCode(ctx, "ABC", strings.Repeat("a", i+1)+"@example.com", "", domain.ClientMeta{IP: "192.0.2.9"})
	}
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "next@example.com", "", domain.ClientMeta{IP: "192.0.2.9"}); !errors.As(err, &locked) {
		t.Fatalf("the thirty-first from one address: %v", err)
	}
	// the limit lifts with time
	p.Clock.T = p.Clock.T.Add(61 * time.Minute)
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "next@example.com", "", domain.ClientMeta{IP: "192.0.2.9"}); err != nil {
		t.Fatalf("after the hour: %v", err)
	}
	// honeypot: success on the surface, no mail
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "bot@example.com", "http://spam", meta); err != nil || p.mails(t, "bot@example.com") != 0 {
		t.Fatalf("honeypot: %v, %d mails", err, p.mails(t, "bot@example.com"))
	}
	// block and allow lists: the same answer as success, and no mail
	p.policy(t, domain.PortalPolicy{Enabled: true, Block: []string{"@spam.example", "evil@example.com"}})
	for _, a := range []string{"x@spam.example", "EVIL@example.com"} {
		if err := p.Svc.PortalRequestCode(ctx, "ABC", a, "", meta); err != nil || p.mails(t, a) != 0 {
			t.Fatalf("%s: %v, %d mails", a, err, p.mails(t, a))
		}
	}
	if err := p.Svc.PortalRequestCode(ctx, "ABC", "fine@example.com", "", meta); err != nil || p.mails(t, "fine@example.com") != 1 {
		t.Fatalf("an allowed address: %v", err)
	}
	p.policy(t, domain.PortalPolicy{Enabled: true, Allow: []string{"@partner.example", "vip@example.com"}, Block: []string{"bad@partner.example"}})
	for a, want := range map[string]int{"x@partner.example": 1, "vip@example.com": 1, "other@example.com": 0, "bad@partner.example": 0} {
		p.Svc.PortalRequestCode(ctx, "ABC", a, "", meta)
		if got := p.mails(t, a); got != want {
			t.Fatalf("allow list, %s: %d mails, want %d", a, got, want)
		}
	}
	if _, err := p.Svc.SetPortalPolicy(ctx, p.Admin, "ABC", domain.PortalPolicy{Enabled: true, Block: []string{"two@@bad"}}); !isInvalid(err) {
		t.Fatalf("a bad entry: %v", err)
	}
	// ten wrong codes in an hour lock verification for the address, whatever the code
	p.policy(t, domain.PortalPolicy{Enabled: true})
	p.Svc.PortalRequestCode(ctx, "ABC", "locked@example.com", "", meta)
	good, _ := p.lastCode(t, "locked@example.com")
	for i := 0; i < 10; i++ {
		p.Svc.PortalVerify(ctx, "ABC", "locked@example.com", "222222", meta)
	}
	if _, _, err := p.Svc.PortalVerify(ctx, "ABC", "locked@example.com", good, meta); !errors.As(err, &locked) {
		t.Fatalf("after ten failures: %v", err)
	}
	// only a project administrator sets the policy
	if _, err := p.Svc.SetPortalPolicy(ctx, p.Alice, "ABC", domain.PortalPolicy{Enabled: true}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member set the policy: %v", err)
	}
	// blocking a customer ends their sessions and stops new ones
	p.Svc.PortalRequestCode(ctx, "ABC", "cora@example.com", "", meta)
	cc, _ := p.lastCode(t, "cora@example.com")
	tok, _, err := p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", cc, meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Svc.BlockCustomer(ctx, p.Admin, "ABC", "cora@example.com", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Svc.CustomerByToken(ctx, tok); err == nil {
		t.Fatal("a blocked customer's session lived on")
	}
	before := p.mails(t, "cora@example.com")
	p.Svc.PortalRequestCode(ctx, "ABC", "cora@example.com", "", meta)
	if p.mails(t, "cora@example.com") != before {
		t.Fatal("a blocked customer was sent a code")
	}
}

// PORT-01b: an optional password, with the policy, the same limits, and no word on whether the address exists.
func TestPORT01_OptionalPassword(t *testing.T) {
	p := newPortal(t)
	p.Svc.PortalRequestCode(ctx, "ABC", "cora@example.com", "", meta)
	code, _ := p.lastCode(t, "cora@example.com")
	tok, c, err := p.Svc.PortalVerify(ctx, "ABC", "cora@example.com", code, meta)
	if err != nil {
		t.Fatal(err)
	}
	if c.HasPass {
		t.Fatal("a new customer has a password")
	}
	if err := p.Svc.SetCustomerPassword(ctx, c, "short"); !isInvalid(err) {
		t.Fatalf("a weak password: %v", err)
	}
	if err := p.Svc.SetCustomerPassword(ctx, c, "a long enough secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Svc.PortalLoginPassword(ctx, "ABC", "cora@example.com", "a long enough secret", meta); err != nil {
		t.Fatalf("password sign-in: %v", err)
	}
	_, _, errWrong := p.Svc.PortalLoginPassword(ctx, "ABC", "cora@example.com", "not it at all", meta)
	_, _, errNone := p.Svc.PortalLoginPassword(ctx, "ABC", "nobody@example.com", "not it at all", meta)
	if errWrong == nil || errNone == nil || errWrong.Error() != errNone.Error() {
		t.Fatalf("the answers differ: %v / %v", errWrong, errNone)
	}
	_ = tok
	if err := p.Svc.SetCustomerPassword(ctx, c, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Svc.PortalLoginPassword(ctx, "ABC", "cora@example.com", "a long enough secret", meta); err == nil {
		t.Fatal("a removed password still works")
	}
	// the profile
	if _, err := p.Svc.UpdateCustomer(ctx, c, "Cora C.", "en", "Mars/Olympus"); !isInvalid(err) {
		t.Fatalf("a bad zone: %v", err)
	}
	up, err := p.Svc.UpdateCustomer(ctx, c, "Cora C.", "en", "Europe/Prague")
	if err != nil || up.Name != "Cora C." || up.Timezone != "Europe/Prague" {
		t.Fatalf("profile: %+v %v", up, err)
	}
}
