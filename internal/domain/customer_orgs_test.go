package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

// ORG01: customer organizations are managed by a portal's administrators, members are made by address, the lists apply, deleting keeps people.
func TestORG01_CustomerOrganizations(t *testing.T) {
	p := newPortal(t)
	if _, err := p.Svc.CreateCustomerOrg(ctx, p.Alice, "ABC", "Acme"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member made one: %v", err)
	}
	o, err := p.Svc.CreateCustomerOrg(ctx, p.Admin, "ABC", "Acme")
	if err != nil || o.Name != "Acme" {
		t.Fatalf("create: %+v %v", o, err)
	}
	if _, err := p.Svc.CreateCustomerOrg(ctx, p.Admin, "ABC", "ACME"); !isInvalid(err) {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := p.Svc.CreateCustomerOrg(ctx, p.Admin, "ABC", " "); !isInvalid(err) {
		t.Fatalf("no name: %v", err)
	}
	o, err = p.Svc.SetCustomerOrgMembers(ctx, p.Admin, "ABC", o.ID, []string{"Cora@Acme.example", "dave@acme.example"}, nil)
	if err != nil || len(o.Members) != 2 || o.Members[0] != "cora@acme.example" {
		t.Fatalf("members: %+v %v", o, err)
	}
	if _, err := p.Svc.SetCustomerOrgMembers(ctx, p.Admin, "ABC", o.ID, []string{"not an address"}, nil); !isInvalid(err) {
		t.Fatalf("a bad address: %v", err)
	}
	// the portal's block list applies to members too
	p.policy(t, domain.PortalPolicy{Enabled: true, Block: []string{"@bad.example"}})
	if _, err := p.Svc.SetCustomerOrgMembers(ctx, p.Admin, "ABC", o.ID, []string{"x@bad.example"}, nil); !isInvalid(err) {
		t.Fatalf("a blocked address: %v", err)
	}
	o, _ = p.Svc.SetCustomerOrgMembers(ctx, p.Admin, "ABC", o.ID, nil, []string{"dave@acme.example"})
	if len(o.Members) != 1 {
		t.Fatalf("remove: %+v", o)
	}
	// a stranger's organization id, and a malformed one, are not found
	if _, err := p.Svc.SetCustomerOrgMembers(ctx, p.Admin, "ABC", "not-an-id", []string{"a@b.example"}, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a bad id: %v", err)
	}
	out := p.NewUser("out2@example.com", "Outsider", false)
	if _, err := p.Svc.ListCustomerOrgs(ctx, out, "ABC"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider: %v", err)
	}
	// deleting the organization leaves its people as customers with no organization
	if err := p.Svc.DeleteCustomerOrg(ctx, p.Admin, "ABC", o.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	p.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM customer WHERE lower(email)='cora@acme.example' AND customer_org_id IS NULL`).Scan(&n)
	if n != 1 {
		t.Fatalf("deleting the organization removed the person: %d", n)
	}
}

type shareRig struct {
	*portal
	rt          domain.RequestType
	org         domain.CustomerOrg
	cora, dave  domain.Customer
	erin, loner domain.Customer
	key         string
}

// newShare: Acme has Cora and Dave; Erin is a customer of nobody; Cora raised a request and has a second, private one.
func newShare(t *testing.T) *shareRig {
	p := newPortal(t)
	r := &shareRig{portal: p, rt: p.rtype(t, "General")}
	var err error
	if r.org, err = p.Svc.CreateCustomerOrg(ctx, p.Admin, "ABC", "Acme"); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Svc.SetCustomerOrgMembers(ctx, p.Admin, "ABC", r.org.ID, []string{"cora@acme.example", "dave@acme.example"}, nil); err != nil {
		t.Fatal(err)
	}
	r.cora, r.dave, r.erin = p.signIn(t, "cora@acme.example"), p.signIn(t, "dave@acme.example"), p.signIn(t, "erin@elsewhere.example")
	pr, err := p.Svc.PortalCreateRequest(ctx, r.cora, "ABC", domain.PortalRequestIn{Type: r.rt.ID, Summary: "Shared problem", Description: "secret details"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	r.key = pr.Key
	return r
}

func (r *shareRig) sees(t *testing.T, c domain.Customer) bool {
	t.Helper()
	_, err := r.Svc.PortalGetRequest(ctx, c, "ABC", r.key)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// PORT-03 / ORG02: a request is private to its owner until they share it, with their customer organization or with named people; sharing can
// be taken back at once; colleagues see the conversation and nothing else of the owner.
func TestORG02_SharedRequests(t *testing.T) {
	r := newShare(t)
	if r.sees(t, r.dave) || r.sees(t, r.erin) {
		t.Fatal("a private request is visible to someone else")
	}
	// Cora has a second request that is never shared
	other, err := r.Svc.PortalCreateRequest(ctx, r.cora, "ABC", domain.PortalRequestIn{Type: r.rt.ID, Summary: "Private matter"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	// share with the company
	yes := true
	got, err := r.Svc.PortalShare(ctx, r.cora, "ABC", r.key, domain.PortalShareIn{Organization: &yes})
	if err != nil || got.Sharing == nil || !got.Sharing.SharedWithOrg || got.Sharing.Organization != "Acme" {
		t.Fatalf("share: %+v %v", got.Sharing, err)
	}
	if !r.sees(t, r.dave) {
		t.Fatal("a colleague cannot see a request shared with the company")
	}
	if r.sees(t, r.erin) {
		t.Fatal("someone outside the company sees it")
	}
	dv, _ := r.Svc.PortalGetRequest(ctx, r.dave, "ABC", r.key)
	if dv.Mine || dv.Sharing != nil || dv.RaisedBy != "cora@acme.example" {
		t.Fatalf("a colleague's view: %+v", dv)
	}
	if list, _ := r.Svc.PortalMyRequests(ctx, r.dave, "ABC"); len(list) != 1 || list[0].Key != r.key {
		t.Fatalf("a colleague's list holds more than the shared request: %+v", list)
	}
	if _, err := r.Svc.PortalGetRequest(ctx, r.dave, "ABC", other.Key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a colleague reads the private request: %v", err)
	}
	// the colleague joins the conversation; each sees the other by name and themselves as You
	if _, err := r.Svc.PortalComment(ctx, r.dave, "ABC", r.key, "Same here"); err != nil {
		t.Fatal(err)
	}
	cv, _ := r.Svc.PortalGetRequest(ctx, r.cora, "ABC", r.key)
	dv, _ = r.Svc.PortalGetRequest(ctx, r.dave, "ABC", r.key)
	if len(cv.Comments) != 1 || cv.Comments[0].Author != "dave@acme.example" || len(dv.Comments) != 1 || dv.Comments[0].Author != "You" {
		t.Fatalf("authors: %+v / %+v", cv.Comments, dv.Comments)
	}
	// a colleague cannot change who sees it
	if _, err := r.Svc.PortalShare(ctx, r.dave, "ABC", r.key, domain.PortalShareIn{Add: []string{"erin@elsewhere.example"}}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a colleague shared it: %v", err)
	}
	// nothing the colleague is shown names staff or the other request
	b, _ := json.Marshal(dv)
	for _, leak := range []string{"Private matter", other.Key, "alice@", "Alice"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("leak %q: %s", leak, b)
		}
	}
	// taking it back is immediate
	no := false
	if _, err := r.Svc.PortalShare(ctx, r.cora, "ABC", r.key, domain.PortalShareIn{Organization: &no}); err != nil {
		t.Fatal(err)
	}
	if r.sees(t, r.dave) {
		t.Fatal("a colleague still sees a request that is no longer shared")
	}
	// named people: Erin, who is outside the company, is added to this one request
	got, err = r.Svc.PortalShare(ctx, r.cora, "ABC", r.key, domain.PortalShareIn{Add: []string{"ERIN@elsewhere.example", "cora@acme.example", "newbie@elsewhere.example"}})
	if err != nil || len(got.Sharing.People) != 2 {
		t.Fatalf("add: %+v %v", got.Sharing, err)
	}
	if !r.sees(t, r.erin) {
		t.Fatal("a named person cannot see the request")
	}
	if _, err := r.Svc.PortalComment(ctx, r.erin, "ABC", r.key, "I can help"); err != nil {
		t.Fatalf("a named person cannot comment: %v", err)
	}
	if list, _ := r.Svc.PortalMyRequests(ctx, r.erin, "ABC"); len(list) != 1 {
		t.Fatalf("the named person's list: %+v", list)
	}
	// staff see who it is shared with
	v, _ := r.Svc.View(ctx, r.Alice, r.key)
	if v.Request == nil || v.Request.Company != "Acme" || len(v.Request.With) != 2 {
		t.Fatalf("staff view: %+v", v.Request)
	}
	got, _ = r.Svc.PortalShare(ctx, r.cora, "ABC", r.key, domain.PortalShareIn{Remove: []string{"erin@elsewhere.example"}})
	if r.sees(t, r.erin) || len(got.Sharing.People) != 1 {
		t.Fatalf("remove: %+v", got.Sharing)
	}
	// the lists apply to people added; a customer with no company cannot share with one; limits
	r.policy(t, domain.PortalPolicy{Enabled: true, Block: []string{"@bad.example"}})
	if _, err := r.Svc.PortalShare(ctx, r.cora, "ABC", r.key, domain.PortalShareIn{Add: []string{"x@bad.example"}}); !isInvalid(err) {
		t.Fatalf("a blocked address: %v", err)
	}
	pr, err := r.Svc.PortalCreateRequest(ctx, r.erin, "ABC", domain.PortalRequestIn{Type: r.rt.ID, Summary: "Erin's"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Svc.PortalShare(ctx, r.erin, "ABC", pr.Key, domain.PortalShareIn{Organization: &yes}); !isInvalid(err) {
		t.Fatalf("sharing with no company: %v", err)
	}
	many := make([]string, 21)
	for i := range many {
		many[i] = strings.Repeat("a", i+1) + "@x.example"
	}
	if _, err := r.Svc.PortalShare(ctx, r.cora, "ABC", r.key, domain.PortalShareIn{Add: many}); !isInvalid(err) {
		t.Fatalf("too many at once: %v", err)
	}
}

// ORG03: leaving the company ends shared access at once; the share survives people leaving and returning only while they are members.
func TestORG03_LeavingTheCompany(t *testing.T) {
	r := newShare(t)
	yes := true
	if _, err := r.Svc.PortalShare(ctx, r.cora, "ABC", r.key, domain.PortalShareIn{Organization: &yes}); err != nil {
		t.Fatal(err)
	}
	if !r.sees(t, r.dave) {
		t.Fatal("not shared")
	}
	if _, err := r.Svc.SetCustomerOrgMembers(ctx, r.Admin, "ABC", r.org.ID, nil, []string{"dave@acme.example"}); err != nil {
		t.Fatal(err)
	}
	if r.sees(t, r.dave) {
		t.Fatal("a person who left the company still sees what was shared with it")
	}
	if _, err := r.Svc.SetCustomerOrgMembers(ctx, r.Admin, "ABC", r.org.ID, []string{"dave@acme.example"}, nil); err != nil {
		t.Fatal(err)
	}
	if !r.sees(t, r.dave) {
		t.Fatal("a person who rejoined does not see it")
	}
	// deleting the company ends it for everyone but the owner
	if err := r.Svc.DeleteCustomerOrg(ctx, r.Admin, "ABC", r.org.ID); err != nil {
		t.Fatal(err)
	}
	if r.sees(t, r.dave) || !r.sees(t, r.cora) {
		t.Fatal("after the company was deleted")
	}
}
