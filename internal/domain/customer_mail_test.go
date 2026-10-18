package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

type custMail struct {
	*inboundEnv
	p    *portal
	rt   domain.RequestType
	cora domain.Customer
	key  string
}

// newCustMail: a portal on ABC, a customer (Cora) who has signed in and raised a request, the mail the system sent flushed.
func newCustMail(t *testing.T) *custMail {
	e := newInbound(t)
	p := &portal{e.Env}
	p.policy(t, domain.PortalPolicy{Enabled: true})
	c := &custMail{inboundEnv: e, p: p, rt: p.rtype(t, "General")}
	c.cora = p.signIn(t, "cora@acme.example")
	pr, err := p.Svc.PortalCreateRequest(ctx, c.cora, "ABC", domain.PortalRequestIn{Type: c.rt.ID, Summary: "Printer is on fire again"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	c.key = pr.Key
	e.flush()
	return c
}

// last is the newest message to the address that is not a sign-in code: its headers and text.
func (c *custMail) last(t *testing.T, to string) (map[string]string, string) {
	t.Helper()
	msgs := c.msgTo(to)
	for i := len(msgs) - 1; i >= 0; i-- {
		h, text, _ := readMsg(t, msgs[i].Raw)
		if strings.Contains(h.Get("Subject"), "sign-in code") {
			continue
		}
		return map[string]string{"Subject": h.Get("Subject"), "Reply-To": h.Get("Reply-To"), "In-Reply-To": h.Get("In-Reply-To"), "Message-Id": h.Get("Message-Id")}, text
	}
	t.Fatalf("no mail to %s", to)
	return nil, ""
}

func (c *custMail) count(to string) int {
	n := 0
	for _, m := range c.msgTo(to) {
		if !strings.Contains(string(m.Raw), "sign-in code") {
			n++
		}
	}
	return n
}

const acmeAuth = "Authentication-Results: mx.example.net; dkim=pass header.d=acme.example; spf=pass"

// PORT-07 / MAIL-22: a customer is emailed when the request is received, when staff reply publicly and when it is resolved, with only what
// the portal shows them and a link; nothing internal and no staff name ever.
func TestPORT07_EmailToCustomersCarriesOnlyWhatTheyMaySee(t *testing.T) {
	c := newCustMail(t)
	h, text := c.last(t, "cora@acme.example")
	if !strings.Contains(h["Subject"], c.key) || !strings.Contains(text, "We received your request") || !strings.Contains(text, "https://tasklane.example.com/portal/ABC#/r/"+c.key) {
		t.Fatalf("the confirmation: %v\n%s", h, text)
	}
	if !strings.HasPrefix(h["Reply-To"], "reply+") {
		t.Fatalf("no reply address: %v", h)
	}
	// an internal note sends nothing; a public reply sends the text and not the name
	if _, err := c.Svc.AddComment(ctx, c.Alice, c.key, "SECRET: the customer is on the red list", true); err != nil {
		t.Fatal(err)
	}
	c.flush()
	if n := c.count("cora@acme.example"); n != 1 {
		t.Fatalf("an internal note was mailed: %d messages", n)
	}
	if _, err := c.Svc.AddComment(ctx, c.Alice, c.key, "We are investigating the printer.", false); err != nil {
		t.Fatal(err)
	}
	c.flush()
	h2, text2 := c.last(t, "cora@acme.example")
	for _, want := range []string{"Support replied to your request", "We are investigating the printer.", "/portal/ABC#/r/" + c.key} {
		if !strings.Contains(text2, want) {
			t.Fatalf("the reply mail lacks %q:\n%s", want, text2)
		}
	}
	for _, leak := range []string{"Alice", "alice@", "SECRET", "red list", "internal"} {
		if strings.Contains(text2, leak) || strings.Contains(h2["Subject"], leak) {
			t.Fatalf("the reply mail leaks %q:\n%s", leak, text2)
		}
	}
	if h2["In-Reply-To"] == "" || h2["In-Reply-To"] == h2["Message-Id"] {
		t.Fatalf("not threaded: %v", h2)
	}
	// resolved
	id, _ := trans(t, c.Env, c.Alice, c.key, "Resolve")
	if _, err := c.Svc.Transition(ctx, c.Alice, c.key, id, nil); err != nil {
		t.Fatal(err)
	}
	c.flush()
	if _, text3 := c.last(t, "cora@acme.example"); !strings.Contains(text3, "marked Done") {
		t.Fatalf("the resolved mail: %s", text3)
	}
	// a customer who is blocked, or who no longer sees the request, is not mailed even if the message was already queued
	before := c.count("cora@acme.example")
	if _, err := c.Svc.AddComment(ctx, c.Alice, c.key, "One more thing", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.NotifyOnce(ctx); err != nil { // queued, not yet sent
		t.Fatal(err)
	}
	if err := c.Svc.BlockCustomer(ctx, c.Admin, "ABC", "cora@acme.example", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.SendMailOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if c.count("cora@acme.example") != before {
		t.Fatal("a blocked customer was mailed")
	}
	var supp int
	c.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_message WHERE kind='customer' AND status='suppressed'`).Scan(&supp)
	if supp != 1 {
		t.Fatalf("suppressed customer mail: %d", supp)
	}
}

// PORT-07: people a request is shared with are mailed too, but only once they have proved their address by signing in; a colleague's
// comment is mailed to the others and not back to the writer.
func TestPORT07_SharedRequestsMailOnlyVerifiedPeople(t *testing.T) {
	c := newCustMail(t)
	org, _ := c.Svc.CreateCustomerOrg(ctx, c.Admin, "ABC", "Acme")
	if _, err := c.Svc.SetCustomerOrgMembers(ctx, c.Admin, "ABC", org.ID, []string{"cora@acme.example", "dave@acme.example"}, nil); err != nil {
		t.Fatal(err)
	}
	cora := c.p.signIn(t, "cora@acme.example") // fresh: now in the organization
	yes := true
	if _, err := c.Svc.PortalShare(ctx, cora, "ABC", c.key, domain.PortalShareIn{Organization: &yes, Add: []string{"erin@else.example"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.AddComment(ctx, c.Alice, c.key, "We have a fix", false); err != nil {
		t.Fatal(err)
	}
	c.flush()
	if c.count("dave@acme.example") != 0 {
		t.Fatal("a colleague who never signed in was mailed an address nobody proved")
	}
	if c.count("erin@else.example") != 0 {
		t.Fatal("a person who was added and never signed in was mailed")
	}
	dave := c.p.signIn(t, "dave@acme.example")
	c.p.signIn(t, "erin@else.example")
	if _, err := c.Svc.AddComment(ctx, c.Alice, c.key, "Now deployed", false); err != nil {
		t.Fatal(err)
	}
	c.flush()
	for _, a := range []string{"dave@acme.example", "erin@else.example", "cora@acme.example"} {
		if _, text := c.last(t, a); !strings.Contains(text, "Now deployed") {
			t.Fatalf("%s was not told:\n%s", a, text)
		}
	}
	// Dave writes in the portal: Cora and Erin are told, Dave is not
	dBefore := c.count("dave@acme.example")
	if _, err := c.Svc.PortalComment(ctx, dave, "ABC", c.key, "Confirmed here too"); err != nil {
		t.Fatal(err)
	}
	c.flush()
	if _, text := c.last(t, "cora@acme.example"); !strings.Contains(text, "dave@acme.example wrote on a request you share") || !strings.Contains(text, "Confirmed here too") {
		t.Fatalf("Cora was not told of the colleague's comment:\n%s", text)
	}
	if c.count("dave@acme.example") != dBefore {
		t.Fatal("the writer was mailed their own comment")
	}
}

// PORT-07 / MAIL-22: a customer's emailed reply becomes their comment when it is proven and addressed to what we sent them; the rules of
// inbound mail apply: no proof, a forwarded address, someone who does not see the request, automatic mail.
func TestPORT07_RepliesByEmailBecomeTheCustomersComment(t *testing.T) {
	c := newCustMail(t)
	_, _ = c.Svc.AddComment(ctx, c.Alice, c.key, "Please send a photo", false)
	c.flush()
	h, _ := c.last(t, "cora@acme.example")
	reply := h["Reply-To"]
	commentsOf := func() []domain.Comment {
		v, _ := c.Svc.View(ctx, c.Alice, c.key)
		return v.Comments
	}
	customerComments := func() (n int) {
		for _, x := range commentsOf() {
			if strings.HasSuffix(x.Author.Name, "(customer)") {
				n++
			}
		}
		return
	}
	// not proven: quarantined, no comment
	r := c.ingest(false, c.eml("cora@acme.example", reply, "Re: ["+c.key+"] x", nil, "Here is the photo"+quoted))
	if r.Outcome != "quarantined" || !strings.Contains(r.Reason, "proof of origin") || customerComments() != 0 {
		t.Fatalf("an unproven reply: %+v", r)
	}
	// proven: the customer's comment, without the quoted part; a repeat is a duplicate
	raw := c.eml("Cora <cora@acme.example>", reply, "Re: ["+c.key+"] Printer is on fire again", []string{acmeAuth}, "It is the grey one."+quoted)
	r = c.ingest(true, raw)
	if r.Outcome != "accepted" {
		t.Fatalf("a proven reply: %+v", r)
	}
	if n := customerComments(); n != 1 {
		t.Fatalf("customer comments: %d", n)
	}
	for _, x := range commentsOf() {
		if strings.HasSuffix(x.Author.Name, "(customer)") && (x.Body != "It is the grey one." || x.Internal) {
			t.Fatalf("the comment: %+v", x)
		}
	}
	if r2 := c.ingest(true, raw); !r2.Duplicate || customerComments() != 1 {
		t.Fatalf("a repeat: %+v", r2)
	}
	// the portal shows it as theirs
	pv, _ := c.Svc.PortalGetRequest(ctx, c.cora, "ABC", c.key)
	found := false
	for _, x := range pv.Comments {
		found = found || (x.Author == "You" && x.Body == "It is the grey one.")
	}
	if !found {
		t.Fatalf("the portal: %+v", pv.Comments)
	}
	// by threading alone (no signed address) and by subject
	r = c.ingest(true, c.eml("cora@acme.example", "tasklane@example.com", "Re: x", []string{acmeAuth, "In-Reply-To: " + h["Message-Id"]}, "By threading"))
	if r.Outcome != "accepted" {
		t.Fatalf("threaded: %+v", r)
	}
	r = c.ingest(true, c.eml("cora@acme.example", "tasklane@example.com", "Re: ["+c.key+"] again", []string{acmeAuth}, "By subject"))
	if r.Outcome != "accepted" {
		t.Fatalf("by subject: %+v", r)
	}
	// a forwarded copy of the signed address: Dave, who is a customer but not on the request, is refused
	dave := c.p.signIn(t, "dave@acme.example")
	_ = dave
	r = c.ingest(true, c.eml("dave@acme.example", reply, "Fwd", []string{"Authentication-Results: mx; dkim=pass header.d=acme.example"}, "I got this by accident"))
	if r.Outcome != "quarantined" {
		t.Fatalf("a forwarded address: %+v", r)
	}
	r = c.ingest(true, c.eml("dave@acme.example", "tasklane@example.com", "Re: ["+c.key+"] x", []string{"Authentication-Results: mx; dkim=pass header.d=acme.example"}, "guessing the key"))
	if r.Outcome != "ignored" {
		t.Fatalf("a customer who does not see the request: %+v", r)
	}
	// automatic mail is ignored, an unknown sender is quarantined as before
	r = c.ingest(true, c.eml("cora@acme.example", reply, "Automatic reply: away", []string{acmeAuth, "Auto-Submitted: auto-replied"}, "I am away"))
	if r.Outcome != "ignored" {
		t.Fatalf("automatic mail: %+v", r)
	}
	r = c.ingest(true, c.eml("nobody@acme.example", reply, "Re: x", []string{acmeAuth}, "who am I"))
	if r.Outcome != "quarantined" {
		t.Fatalf("an unknown sender: %+v", r)
	}
	// a customer cannot open a request by email
	before := c.issueCount()
	c.ingest(true, c.eml("cora@acme.example", "ABC@tasklane.example.com", "A new problem", []string{acmeAuth}, "please help"))
	if c.issueCount() != before {
		t.Fatal("an email from a customer opened an issue")
	}
	// the blocked: nothing from a blocked customer is accepted
	if err := c.Svc.BlockCustomer(ctx, c.Admin, "ABC", "cora@acme.example", true); err != nil {
		t.Fatal(err)
	}
	n := customerComments()
	r = c.ingest(true, c.eml("cora@acme.example", reply, "Re: x", []string{acmeAuth}, "still here?"))
	if r.Outcome == "accepted" || customerComments() != n {
		t.Fatalf("a blocked customer's reply: %+v", r)
	}
}

// MAIL-22: a quarantined customer reply is released by an administrator into the customer's comment, if the customer still sees the request.
func TestPORT07_ReleasingAQuarantinedCustomerReply(t *testing.T) {
	c := newCustMail(t)
	_, _ = c.Svc.AddComment(ctx, c.Alice, c.key, "Any news?", false)
	c.flush()
	h, _ := c.last(t, "cora@acme.example")
	r := c.ingest(false, c.eml("cora@acme.example", h["Reply-To"], "Re: x", nil, "Released text"))
	if r.Outcome != "quarantined" {
		t.Fatalf("%+v", r)
	}
	if _, err := c.Svc.ReleaseInbound(ctx, c.Alice, r.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member released: %v", err)
	}
	if _, err := c.Svc.ReleaseInbound(ctx, c.Admin, r.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	pv, _ := c.Svc.PortalGetRequest(ctx, c.cora, "ABC", c.key)
	found := false
	for _, x := range pv.Comments {
		found = found || (x.Author == "You" && x.Body == "Released text")
	}
	if !found {
		t.Fatalf("the released reply is not in the thread: %+v", pv.Comments)
	}
}
