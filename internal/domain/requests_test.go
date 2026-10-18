package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

// signIn makes the customer (as the portal would) and returns them with their session's organization.
func (p *portal) signIn(t *testing.T, email string) domain.Customer {
	t.Helper()
	if err := p.Svc.PortalRequestCode(ctx, "ABC", email, "", domain.ClientMeta{IP: "203.0.113." + string(rune('1'+len(email)%8))}); err != nil {
		t.Fatal(err)
	}
	code, _ := p.lastCode(t, email)
	tok, _, err := p.Svc.PortalVerify(ctx, "ABC", email, code, meta)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := p.Svc.CustomerByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (p *portal) field(t *testing.T, name, typ string, opts ...string) domain.FieldDef {
	t.Helper()
	f, err := p.Svc.CreateField(ctx, p.Admin, domain.FieldIn{Name: name, Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.AddContext(ctx, p.Admin, f.ID, domain.ContextIn{Global: true, Options: opts}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (p *portal) rtype(t *testing.T, name string, fields ...domain.RequestField) domain.RequestType {
	t.Helper()
	rt, err := p.Svc.CreateRequestType(ctx, p.Admin, "ABC", domain.RequestTypeIn{Name: &name, Fields: &fields})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// REQ01: request types are forms managed by project administrators; the fields they ask for must apply; a subtask is not a request.
func TestREQ01_RequestTypeAdministration(t *testing.T) {
	p := newPortal(t)
	order := p.field(t, "Order number", "text")
	n := "Billing question"
	if _, err := p.Svc.CreateRequestType(ctx, p.Alice, "ABC", domain.RequestTypeIn{Name: &n}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member made a form: %v", err)
	}
	rt := p.rtype(t, "Billing question", domain.RequestField{Key: order.Key, Label: "Your order", Required: true})
	if rt.IssueType != "Task" || len(rt.Fields) != 1 || rt.Fields[0].Label != "Your order" || !rt.Visible {
		t.Fatalf("new form: %+v", rt)
	}
	dup := "billing QUESTION"
	if _, err := p.Svc.CreateRequestType(ctx, p.Admin, "ABC", domain.RequestTypeIn{Name: &dup}); !isInvalid(err) {
		t.Fatalf("duplicate name: %v", err)
	}
	sub := "Sub"
	st := "Subtask"
	if _, err := p.Svc.CreateRequestType(ctx, p.Admin, "ABC", domain.RequestTypeIn{Name: &sub, IssueType: &st}); !isInvalid(err) {
		t.Fatalf("a subtask form: %v", err)
	}
	bad := []domain.RequestField{{Key: "customfield_99999"}}
	if _, err := p.Svc.UpdateRequestType(ctx, p.Admin, rt.ID, domain.RequestTypeIn{Fields: &bad}); !isInvalid(err) {
		t.Fatalf("a field that does not exist: %v", err)
	}
	twice := []domain.RequestField{{Key: order.Key}, {Key: order.Key}}
	if _, err := p.Svc.UpdateRequestType(ctx, p.Admin, rt.ID, domain.RequestTypeIn{Fields: &twice}); !isInvalid(err) {
		t.Fatalf("a field twice: %v", err)
	}
	user := p.field(t, "Owner", "user")
	uf := []domain.RequestField{{Key: user.Key}}
	if _, err := p.Svc.UpdateRequestType(ctx, p.Admin, rt.ID, domain.RequestTypeIn{Fields: &uf}); !isInvalid(err) {
		t.Fatalf("a person field on a customer form: %v", err)
	}
	// Bob is a member: he can read the list; someone outside the project cannot
	if got, err := p.Svc.ListRequestTypes(ctx, p.Bob, "ABC"); err != nil || len(got) != 1 {
		t.Fatalf("list: %+v %v", got, err)
	}
	out := p.NewUser("out@example.com", "Outsider", false)
	if _, err := p.Svc.ListRequestTypes(ctx, out, "ABC"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider listed forms: %v", err)
	}
	// a hidden form is not offered to customers; deleting a form leaves its requests
	c := p.signIn(t, "cora@example.com")
	hide := false
	if _, err := p.Svc.UpdateRequestType(ctx, p.Admin, rt.ID, domain.RequestTypeIn{Visible: &hide}); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Svc.PortalRequestTypes(ctx, c, "ABC"); len(got) != 0 {
		t.Fatalf("a hidden form is offered: %+v", got)
	}
	if err := p.Svc.DeleteRequestType(ctx, p.Alice, rt.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member deleted a form: %v", err)
	}
	if err := p.Svc.DeleteRequestType(ctx, p.Admin, rt.ID); err != nil {
		t.Fatal(err)
	}
}

// REQ02: a request is an ordinary issue raised through a form: required fields, only the form's fields, the honeypot, the rate limit.
func TestREQ02_CreatingRequests(t *testing.T) {
	p := newPortal(t)
	order := p.field(t, "Order number", "text")
	hidden := p.field(t, "Internal score", "number")
	rt := p.rtype(t, "Billing question", domain.RequestField{Key: order.Key, Label: "Your order", Required: true, Help: "On the invoice"})
	c := p.signIn(t, "cora@example.com")
	forms, err := p.Svc.PortalRequestTypes(ctx, c, "ABC")
	if err != nil || len(forms) != 1 || forms[0].IssueType != "" || forms[0].Fields[0].Type != "text" {
		t.Fatalf("the forms offered: %+v %v", forms, err)
	}
	mk := func(in domain.PortalRequestIn) (domain.PortalRequest, error) {
		return p.Svc.PortalCreateRequest(ctx, c, "ABC", in, meta)
	}
	if _, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "", Fields: map[string]any{order.Key: "A-1"}}); !isInvalid(err) {
		t.Fatalf("no summary: %v", err)
	}
	if _, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "Charged twice"}); !isInvalid(err) {
		t.Fatalf("a required field left out: %v", err)
	}
	if _, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "Charged twice", Fields: map[string]any{order.Key: "A-1", hidden.Key: 5}}); !isInvalid(err) {
		t.Fatalf("a field the form does not ask for: %v", err)
	}
	if _, err := mk(domain.PortalRequestIn{Type: "no-such-form", Summary: "x"}); !isInvalid(err) {
		t.Fatalf("an unknown form: %v", err)
	}
	r, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "Charged twice", Description: "I was <script>alert(1)</script>charged two times", Fields: map[string]any{order.Key: "A-1"}})
	if err != nil || r.Key == "" || r.Status == "" || r.Type != "Billing question" {
		t.Fatalf("create: %+v %v", r, err)
	}
	if len(r.Fields) != 1 || r.Fields[0].Label != "Your order" || r.Fields[0].Value != "A-1" {
		t.Fatalf("the form's fields: %+v", r.Fields)
	}
	if !strings.Contains(r.Description, "charged two times") { // text as typed: the portal page shows it with textContent
		t.Fatalf("description: %q", r.Description)
	}
	// it is an ordinary issue: staff see it with who raised it, and the hidden reporter is not in any list of people
	v, err := p.Svc.View(ctx, p.Alice, r.Key)
	if err != nil || v.Request == nil || v.Request.Email != "cora@example.com" || v.Request.Type != "Billing question" {
		t.Fatalf("staff view: %+v %v", v.Request, err)
	}
	users, _ := p.Svc.ListUsers(ctx)
	for _, u := range users {
		if u.Kind == "portal" || strings.Contains(u.Email, "portal.invalid") {
			t.Fatalf("the portal account is in the list of people: %+v", u)
		}
	}
	// the honeypot drops a request and makes no issue
	var before, after int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&before)
	if _, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "spam", Fields: map[string]any{order.Key: "x"}, Website: "http://x"}); err != nil {
		t.Fatal(err)
	}
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&after)
	if after != before {
		t.Fatalf("the honeypot made an issue")
	}
	// ten an hour, then a limit that says so
	for i := 0; i < 9; i++ {
		if _, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "more", Fields: map[string]any{order.Key: "A"}}); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	var locked *domain.LockedError
	if _, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "one too many", Fields: map[string]any{order.Key: "A"}}); !errors.As(err, &locked) {
		t.Fatalf("the eleventh request: %v", err)
	}
	p.Clock.T = p.Clock.T.Add(61 * time.Minute)
	if _, err := mk(domain.PortalRequestIn{Type: rt.ID, Summary: "later", Fields: map[string]any{order.Key: "A"}}); err != nil {
		t.Fatalf("after the hour: %v", err)
	}
	// a customer of another organization cannot use this portal
	o2, err := p.Svc.CreateOrganization(ctx, p.Admin, "Other", "boss@other.example", "Boss", "password123")
	if err != nil {
		t.Fatal(err)
	}
	foreign := c
	foreign.OrgID = o2.ID
	if _, err := p.Svc.PortalRequestTypes(ctx, foreign, "ABC"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a customer of another organization: %v", err)
	}
}

// PORT-03: a customer sees only their own requests and only what is meant for them; everything else answers as if it were not there.
func TestPORT03_VisibilityBetweenCustomersAndWhatIsHidden(t *testing.T) {
	p := newPortal(t)
	order := p.field(t, "Order number", "text")
	score := p.field(t, "Internal score", "number")
	rt := p.rtype(t, "Billing question", domain.RequestField{Key: order.Key, Label: "Your order"})
	cora, dave := p.signIn(t, "cora@example.com"), p.signIn(t, "dave@customer.example")
	mine, err := p.Svc.PortalCreateRequest(ctx, cora, "ABC", domain.PortalRequestIn{Type: rt.ID, Summary: "Cora's problem", Description: "details", Fields: map[string]any{order.Key: "C-1"}}, meta)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := p.Svc.PortalCreateRequest(ctx, dave, "ABC", domain.PortalRequestIn{Type: rt.ID, Summary: "Dave's problem", Fields: map[string]any{order.Key: "D-1"}}, meta)
	if err != nil {
		t.Fatal(err)
	}
	// staff work on Cora's request: an internal field and note, a public reply, an assignment, a link, a worklog-like private comment
	if _, err := p.Svc.UpdateFields(ctx, p.Alice, mine.Key, nil, false, domain.Changes{Custom: map[string]any{score.Key: 9}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.AddComment(ctx, p.Alice, mine.Key, "SECRET internal note", true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.AddComment(ctx, p.Alice, mine.Key, "Hello Cora, we are looking", false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.UpdateFields(ctx, p.Alice, mine.Key, nil, false, domain.Changes{Assignee: &p.Alice.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.PortalComment(ctx, cora, "ABC", mine.Key, "Thank you"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.PortalComment(ctx, dave, "ABC", mine.Key, "I am not the owner"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("someone else's request: %v", err)
	}
	got, err := p.Svc.PortalGetRequest(ctx, cora, "ABC", mine.Key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	for _, leak := range []string{"SECRET", "Alice", "alice@", "Bob", "internal", "Internal score", "portal.invalid", "Customer portal", "assignee", "reporter"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("the customer's view leaks %q: %s", leak, b)
		}
	}
	if len(got.Comments) != 2 || got.Comments[0].Author != "Support" || got.Comments[1].Author != "You" || got.Comments[0].Body != "Hello Cora, we are looking" {
		t.Fatalf("comments: %+v", got.Comments)
	}
	// Dave's view of his own request has none of Cora's comments, and he cannot read hers; a key that does not exist answers the same
	if _, err := p.Svc.PortalGetRequest(ctx, dave, "ABC", mine.Key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Dave read Cora's request: %v", err)
	}
	if _, err := p.Svc.PortalGetRequest(ctx, dave, "ABC", "ABC-99999"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a missing key: %v", err)
	}
	list, _ := p.Svc.PortalMyRequests(ctx, dave, "ABC")
	if len(list) != 1 || list[0].Key != theirs.Key {
		t.Fatalf("Dave's list: %+v", list)
	}
	// a request raised by staff, an issue in the project that no customer raised, is nobody's
	staffIssue := mk(t, p.Env, p.Alice, "Task", "staff only", "")
	if _, err := p.Svc.PortalGetRequest(ctx, cora, "ABC", staffIssue); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an issue no customer raised: %v", err)
	}
	// staff see the customer's comment in the thread, labelled
	v, _ := p.Svc.View(ctx, p.Alice, mine.Key)
	found := false
	for _, c := range v.Comments {
		if strings.HasSuffix(c.Author.Name, "(customer)") && c.Body == "Thank you" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the customer's comment is not in the staff thread: %+v", v.Comments)
	}
	// a deleted issue disappears from the portal
	if _, err := p.Svc.Lifecycle(ctx, p.Admin, mine.Key, domain.LifecycleIn{Op: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.PortalGetRequest(ctx, cora, "ABC", mine.Key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a deleted request is still there: %v", err)
	}
	if list, _ := p.Svc.PortalMyRequests(ctx, cora, "ABC"); len(list) != 0 {
		t.Fatalf("a deleted request is listed: %+v", list)
	}
}

// REQ03: a customer's comment tells the people who watch the issue, once, as a comment from the customer.
func TestREQ03_CustomerCommentNotifiesStaff(t *testing.T) {
	p := newPortal(t)
	rt := p.rtype(t, "General")
	c := p.signIn(t, "cora@example.com")
	r, err := p.Svc.PortalCreateRequest(ctx, c, "ABC", domain.PortalRequestIn{Type: rt.ID, Summary: "Hello"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.SetWatch(ctx, p.Alice, r.Key, true); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Svc.PortalComment(ctx, c, "ABC", r.Key, "Any news?"); err != nil {
		t.Fatal(err)
	}
	notify(t, p.Env)
	var found int
	for _, n := range inboxOf(t, p.Env, p.Alice, "") {
		if n.Kind == "comment" && strings.Contains(n.What, "(customer) commented") && n.Body == "Any news?" {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("notices for Alice: %+v", inboxOf(t, p.Env, p.Alice, ""))
	}
	if _, err := p.Svc.PortalComment(ctx, c, "ABC", r.Key, "   "); !isInvalid(err) {
		t.Fatalf("an empty comment: %v", err)
	}
}
