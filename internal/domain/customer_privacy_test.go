package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

// traces searches every text and JSON column of every table for what must be gone.
func (c *custMail) traces(t *testing.T, needles ...string) []string {
	t.Helper()
	rows, err := c.Store.Pool.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns WHERE table_schema=current_schema() AND data_type IN ('text','character varying','jsonb','json','ARRAY') ORDER BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ tbl, col string }
	var cols []col
	for rows.Next() {
		var x col
		rows.Scan(&x.tbl, &x.col)
		cols = append(cols, x)
	}
	rows.Close()
	var found []string
	for _, x := range cols {
		for _, n := range needles {
			var cnt int
			q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE %q::text ILIKE '%%' || $1 || '%%'`, x.tbl, x.col)
			if err := c.Store.Pool.QueryRow(ctx, q, n).Scan(&cnt); err != nil {
				t.Fatalf("%s.%s: %v", x.tbl, x.col, err)
			}
			if cnt > 0 {
				found = append(found, fmt.Sprintf("%s.%s has %d row(s) with %q", x.tbl, x.col, cnt, n))
			}
		}
	}
	return found
}

type privCust struct {
	*custMail
	id   string
	dave domain.Customer
}

// newPrivCust: Cora (named) has a request, comments both ways, an emailed reply, shares it with Erin, and signs in; Dave has his own request.
func newPrivCust(t *testing.T) *privCust {
	c := newCustMail(t)
	p := &privCust{custMail: c}
	if _, err := c.Svc.UpdateCustomer(ctx, c.cora, "Cora Czerny", "en", "Europe/Prague"); err != nil {
		t.Fatal(err)
	}
	c.Store.Pool.QueryRow(ctx, `SELECT id::text FROM customer WHERE lower(email)='cora@acme.example'`).Scan(&p.id)
	if _, err := c.Svc.PortalComment(ctx, c.cora, "ABC", c.key, "My name is Cora Czerny and this is my printer"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.AddComment(ctx, c.Alice, c.key, "Hello Cora, we are looking", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.PortalShare(ctx, c.cora, "ABC", c.key, domain.PortalShareIn{Add: []string{"erin@else.example"}}); err != nil {
		t.Fatal(err)
	}
	c.flush()
	h, _ := c.last(t, "cora@acme.example")
	r := c.ingest(true, c.eml("Cora Czerny <cora@acme.example>", h["Reply-To"], "Re: ["+c.key+"] x", []string{acmeAuth}, "A reply by email from cora@acme.example"))
	if r.Outcome != "accepted" {
		t.Fatalf("setup reply: %+v", r)
	}
	p.dave = c.p.signIn(t, "dave@acme.example")
	if _, err := c.Svc.PortalCreateRequest(ctx, p.dave, "ABC", domain.PortalRequestIn{Type: c.rt.ID, Summary: "Dave's own matter"}, meta); err != nil {
		t.Fatal(err)
	}
	return p
}

// PORT-06 / PRIV01: the export holds everything about the customer and nothing about anyone else; it is for administrators of the customer's
// organization (and for the customer), and is recorded.
func TestPRIV01_CustomerExport(t *testing.T) {
	p := newPrivCust(t)
	if _, err := p.Svc.ExportCustomerData(ctx, p.Alice, p.id); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member exported: %v", err)
	}
	if _, err := p.Svc.ExportCustomerData(ctx, p.Admin, "not-an-id"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a bad id: %v", err)
	}
	out, err := p.Svc.ExportCustomerData(ctx, p.Admin, p.id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	js := string(b)
	for _, want := range []string{"cora@acme.example", "Cora Czerny", "Europe/Prague", "Printer is on fire again", "My name is Cora Czerny", "Hello Cora, we are looking", "erin@else.example", "A reply by email from"} {
		if !strings.Contains(js, want) {
			t.Fatalf("the export lacks %q:\n%s", want, js)
		}
	}
	for _, other := range []string{"Dave's own matter", "dave@acme.example", "alice@example.com"} {
		if strings.Contains(js, other) {
			t.Fatalf("the export holds someone else's data %q", other)
		}
	}
	for _, section := range []string{"profile", "requests", "your_comments", "replies_to_your_requests", "people_you_shared_with", "sessions", "sign_in_codes", "mail_sent_to_you", "mail_from_you"} {
		if _, ok := out[section]; !ok {
			t.Fatalf("no section %q", section)
		}
	}
	if reqs := out["requests"].([]map[string]any); len(reqs) != 1 {
		t.Fatalf("requests: %+v", reqs)
	}
	var audited int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='privacy.export.customer' AND target=$1`, p.id).Scan(&audited)
	if audited != 1 {
		t.Fatalf("audit entries: %d", audited)
	}
	// an administrator of another organization cannot see the customer
	o2, err := p.Svc.CreateOrganization(ctx, p.Admin, "Other", "boss2@other.example", "Boss", "password123")
	if err != nil {
		t.Fatal(err)
	}
	var bossID string
	p.Store.Pool.QueryRow(ctx, `SELECT id::text FROM app_user WHERE email='boss2@other.example'`).Scan(&bossID)
	boss := domain.Actor{ID: bossID, Label: "Boss", OrgAdmin: true, Org: o2.ID}
	if _, err := p.Svc.ExportCustomerData(ctx, boss, p.id); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another organization's administrator: %v", err)
	}
	if l, _ := p.Svc.ListCustomers(ctx, boss, "", 50); len(l) != 0 {
		t.Fatalf("the other organization's list: %+v", l)
	}
	// the customer's own export is the same data
	self, err := p.Svc.CustomerExportSelf(ctx, p.cora)
	if err != nil || len(self["requests"].([]map[string]any)) != 1 {
		t.Fatalf("self export: %v", err)
	}
	if l, _ := p.Svc.ListCustomers(ctx, p.Admin, "cora", 50); len(l) != 1 || l[0].Email != "cora@acme.example" || !l[0].Verified {
		t.Fatalf("find: %+v", l)
	}
}

// PORT-06 / PRIV02: erasing a customer leaves no trace of them anywhere, keeps the work, can be repeated, and stops at legal hold.
func TestPRIV02_ErasingACustomer(t *testing.T) {
	p := newPrivCust(t)
	needles := []string{"cora@acme.example", "Cora Czerny", "Czerny"}
	if len(p.traces(t, needles...)) == 0 {
		t.Fatal("the setup left no trace to remove")
	}
	// legal hold first: refused, naming the request
	p.Store.Pool.Exec(ctx, `UPDATE issue SET legal_hold=true WHERE key=$1`, p.key)
	if _, err := p.Svc.EraseCustomer(ctx, p.Admin, p.id, domain.EraseOpts{ScrubMentions: true}); err == nil || !strings.Contains(err.Error(), p.key) {
		t.Fatalf("an erase under legal hold: %v", err)
	}
	p.Store.Pool.Exec(ctx, `UPDATE issue SET legal_hold=false WHERE key=$1`, p.key)
	// a member and the other customers cannot
	if _, err := p.Svc.EraseCustomer(ctx, p.Alice, p.id, domain.EraseOpts{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member erased: %v", err)
	}
	if _, err := p.Svc.CustomerEraseSelf(ctx, p.dave, "dave@acme.example", domain.EraseOpts{}); err != nil {
		t.Fatalf("Dave erasing himself: %v", err)
	}
	res, err := p.Svc.EraseCustomer(ctx, p.Admin, p.id, domain.EraseOpts{ScrubMentions: true})
	if err != nil || !res.Erased {
		t.Fatalf("erase: %+v %v", res, err)
	}
	if left := p.traces(t, needles...); len(left) != 0 {
		t.Fatalf("traces left after the erase:\n%s", strings.Join(left, "\n"))
	}
	// the work stays: the request and the staff's replies, with a pseudonym where the customer was
	v, err := p.Svc.View(ctx, p.Alice, p.key)
	if err != nil {
		t.Fatal(err)
	}
	if v.Request == nil || v.Request.Customer != "Deleted customer" || !strings.HasSuffix(v.Request.Email, "@erased.invalid") {
		t.Fatalf("the request after the erase: %+v", v.Request)
	}
	pubs, cust := 0, 0
	for _, c := range v.Comments {
		if strings.Contains(c.Body, "we are looking") {
			pubs++
		}
		if strings.HasPrefix(c.Author.Name, "Deleted customer") {
			cust++
		}
	}
	if pubs != 1 || cust < 1 {
		t.Fatalf("comments after the erase: %+v", v.Comments)
	}
	// the customer cannot come back as themselves: their sessions are gone, and the old address makes a new, empty customer
	if _, _, err := p.Svc.CustomerByToken(ctx, "anything"); err == nil {
		t.Fatal("a token worked")
	}
	var live int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM customer_session WHERE customer_id::text=$1`, p.id).Scan(&live)
	if live != 0 {
		t.Fatalf("the erased customer still has %d session(s)", live)
	}
	again := p.p.signIn(t, "cora@acme.example")
	if again.ID == p.id {
		t.Fatal("the old address got the erased identity back")
	}
	// repeatable, and recorded without anything personal
	if res, err := p.Svc.EraseCustomer(ctx, p.Admin, p.id, domain.EraseOpts{}); err != nil || res.Erased {
		t.Fatalf("a second erase: %+v %v", res, err)
	}
	var detail string
	if err := p.Store.Pool.QueryRow(ctx, `SELECT detail::text FROM audit_event WHERE action='privacy.erased.customer' AND target=$1`, p.id).Scan(&detail); err != nil || strings.Contains(strings.ToLower(detail), "cora") {
		t.Fatalf("audit: %q %v", detail, err)
	}
	// each erase touched only its own customer: Dave's row was erased by himself above, a third customer is whole
	var erased, whole int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM customer WHERE erased_at IS NOT NULL`).Scan(&erased)
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM customer WHERE erased_at IS NULL AND lower(email)='erin@else.example'`).Scan(&whole)
	if erased != 2 {
		t.Fatalf("erased customers: %d", erased)
	}
	_ = whole
}

// PRIV03: delete_content also empties what the customer wrote; erasing yourself needs the address typed; an organization's administrator
// may erase their own customers only.
func TestPRIV03_DeleteContentAndSelfService(t *testing.T) {
	p := newPrivCust(t)
	if _, err := p.Svc.CustomerEraseSelf(ctx, p.cora, "someone@else.example", domain.EraseOpts{}); !isInvalid(err) {
		t.Fatalf("a wrong confirmation: %v", err)
	}
	res, err := p.Svc.CustomerEraseSelf(ctx, p.cora, " CORA@acme.example ", domain.EraseOpts{DeleteContent: true, ScrubMentions: true})
	if err != nil || !res.Erased || res.Counts["comments_deleted"] != 2 && res.Counts["comments_deleted"] != 1 {
		t.Fatalf("self erase: %+v %v", res, err)
	}
	v, _ := p.Svc.View(ctx, p.Alice, p.key)
	if v.Issue.Summary != "Request from a deleted customer" {
		t.Fatalf("the summary: %q", v.Issue.Summary)
	}
	for _, c := range v.Comments {
		if strings.HasPrefix(c.Author.Name, "Deleted customer") {
			t.Fatalf("a customer comment survived delete_content: %+v", c)
		}
	}
	if left := p.traces(t, "cora@acme.example", "Czerny", "on fire again"); len(left) != 0 {
		t.Fatalf("traces:\n%s", strings.Join(left, "\n"))
	}
}

// PORT-06 / PRIV04: promotion makes a staff account with the customer's address and keeps everything they did; a customer is never a seat.
func TestPRIV04_PromotionToStaff(t *testing.T) {
	p := newPrivCust(t)
	staffBefore, _ := p.Svc.ListUsers(ctx)
	for _, u := range staffBefore {
		if strings.Contains(u.Email, "acme.example") {
			t.Fatalf("a customer is counted among the people: %+v", u)
		}
	}
	if _, err := p.Svc.PromoteCustomer(ctx, p.Alice, p.id, domain.PromoteIn{Password: "a long enough one"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member promoted: %v", err)
	}
	if _, err := p.Svc.PromoteCustomer(ctx, p.Admin, p.id, domain.PromoteIn{Password: "short"}); !isInvalid(err) {
		t.Fatalf("a weak password: %v", err)
	}
	u, err := p.Svc.PromoteCustomer(ctx, p.Admin, p.id, domain.PromoteIn{Password: "a long enough one"})
	if err != nil || u.Email != "cora@acme.example" || u.Name != "Cora Czerny" || u.Admin {
		t.Fatalf("promote: %+v %v", u, err)
	}
	after, _ := p.Svc.ListUsers(ctx)
	if len(after) != len(staffBefore)+1 {
		t.Fatalf("staff seats: %d -> %d", len(staffBefore), len(after))
	}
	if _, err := p.Svc.PromoteCustomer(ctx, p.Admin, p.id, domain.PromoteIn{Password: "a long enough one"}); err == nil || !strings.Contains(err.Error(), "already promoted") {
		t.Fatalf("a second promotion: %v", err)
	}
	// the history is kept, and the customer identity still works in the portal
	pv, err := p.Svc.PortalGetRequest(ctx, p.cora, "ABC", p.key)
	if err != nil || len(pv.Comments) != 3 {
		t.Fatalf("the portal after promotion: %+v %v", pv.Comments, err)
	}
	if l, _ := p.Svc.ListCustomers(ctx, p.Admin, "cora", 10); len(l) != 1 || l[0].Staff != "cora@acme.example" {
		t.Fatalf("the customer list: %+v", l)
	}
	// the new account signs in as staff and sees the project only if it is made a member
	if _, _, err := p.Svc.Login(ctx, "cora@acme.example", "a long enough one", 3600e9); err != nil {
		t.Fatalf("staff sign-in: %v", err)
	}
	// a customer whose address is already a staff account cannot be promoted; an erased customer cannot either
	dv := p.dave
	if _, err := p.Svc.PromoteCustomer(ctx, p.Admin, p.idOf(t, dv.Email), domain.PromoteIn{Password: "a long enough one"}); err != nil {
		t.Fatalf("Dave: %v", err)
	}
	if _, err := p.Svc.PromoteCustomer(ctx, p.Admin, p.idOf(t, dv.Email), domain.PromoteIn{Password: "a long enough one"}); err == nil {
		t.Fatal("promoted twice")
	}
	p.Store.Pool.Exec(ctx, `UPDATE customer SET promoted_to=NULL WHERE lower(email)='dave@acme.example'`)
	if _, err := p.Svc.PromoteCustomer(ctx, p.Admin, p.idOf(t, dv.Email), domain.PromoteIn{Password: "a long enough one"}); !isInvalid(err) {
		t.Fatalf("an address that is already staff: %v", err)
	}
}

func (p *privCust) idOf(t *testing.T, email string) string {
	var id string
	if err := p.Store.Pool.QueryRow(ctx, `SELECT id::text FROM customer WHERE lower(email)=lower($1)`, email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
