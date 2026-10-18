package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
)

func qin(name, tql string) domain.QueueIn { return domain.QueueIn{Name: &name, TQL: &tql} }

// QUE01: queues are managed by project administrators, their queries must compile, names are unique, and outsiders learn nothing.
func TestQUE01_QueueAdministration(t *testing.T) {
	d := newDash(t)
	if _, err := d.Svc.CreateQueue(ctx, d.Alice, "ABC", qin("Open", "statusCategory != Done")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member made a queue: %v", err)
	}
	q, err := d.Svc.CreateQueue(ctx, d.Admin, "ABC", qin("Open", "statusCategory != Done ORDER BY updated DESC"))
	if err != nil || q.Count == 0 {
		t.Fatalf("create: %+v %v", q, err)
	}
	for name, in := range map[string]domain.QueueIn{
		"duplicate": qin("open", "priority = high"),
		"no query":  qin("Empty", "  "),
		"bad query": qin("Bad", "status ="),
		"unknown":   qin("Unknown", "nonsense = 1"),
		"no name":   qin(" ", "priority = high"),
	} {
		if _, err := d.Svc.CreateQueue(ctx, d.Admin, "ABC", in); !isInvalid(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	bad := "status ="
	if _, err := d.Svc.UpdateQueue(ctx, d.Admin, q.ID, domain.QueueIn{TQL: &bad}); !isInvalid(err) {
		t.Fatalf("an update to a bad query: %v", err)
	}
	if _, err := d.Svc.UpdateQueue(ctx, d.Alice, q.ID, domain.QueueIn{TQL: func() *string { s := "priority = high"; return &s }()}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member changed a queue: %v", err)
	}
	// anyone in the project sees the queue; someone outside it, nothing
	if got, err := d.Svc.ListQueues(ctx, d.Bob, "ABC"); err != nil || len(got) != 1 {
		t.Fatalf("list: %+v %v", got, err)
	}
	if _, err := d.Svc.ListQueues(ctx, d.Bob, "XYZ"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider listed queues: %v", err)
	}
	if _, err := d.Svc.QueueIssues(ctx, d.NewUser("eve3@example.com", "Eve", false), q.ID, 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider read a queue: %v", err)
	}
	if err := d.Svc.DeleteQueue(ctx, d.Alice, q.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member deleted a queue: %v", err)
	}
	if err := d.Svc.DeleteQueue(ctx, d.Admin, q.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Svc.QueueIssues(ctx, d.Admin, q.ID, 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a deleted queue: %v", err)
	}
}

// QUE02: a queue never reaches beyond its project, counts and lists agree, and a query that stops running is shown as such.
func TestQUE02_QueuesStayInTheirProject(t *testing.T) {
	d := newDash(t)
	// the query names the other project as well: the queue still holds ABC only
	q, err := d.Svc.CreateQueue(ctx, d.Admin, "ABC", qin("Everything", "project = XYZ OR project = ABC"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Svc.QueueIssues(ctx, d.Alice, q.ID, 50) // Alice can read both projects
	if err != nil || res.Total == 0 {
		t.Fatalf("issues: %+v %v", res, err)
	}
	for _, r := range res.Rows {
		if !strings.HasPrefix(r.Key, "ABC-") {
			t.Fatalf("a queue of ABC holds %s", r.Key)
		}
	}
	list, _ := d.Svc.ListQueues(ctx, d.Alice, "ABC")
	if len(list) != 1 || list[0].Count != res.Total {
		t.Fatalf("count %+v, list total %d", list, res.Total)
	}
	// a priority queue is a subset, and order follows position
	hi, _ := d.Svc.CreateQueue(ctx, d.Admin, "ABC", domain.QueueIn{Name: sptr("High"), TQL: sptr("priority = high"), Position: func() *int { n := -1; return &n }()})
	list, _ = d.Svc.ListQueues(ctx, d.Bob, "ABC")
	if len(list) != 2 || list[0].ID != hi.ID || list[0].Count == 0 || list[0].Count >= list[1].Count {
		t.Fatalf("order and counts: %+v", list)
	}
	// a query that no longer compiles is reported in its own row; the others still answer
	d.Store.Pool.Exec(ctx, `UPDATE queue SET tql='status =' WHERE id::text=$1`, hi.ID)
	list, _ = d.Svc.ListQueues(ctx, d.Bob, "ABC")
	if list[0].Error == "" || list[0].Count != 0 || list[1].Error != "" {
		t.Fatalf("a broken queue: %+v", list)
	}
}

// QUE03: canned responses are for people who can comment; administrators manage them; names are unique.
func TestQUE03_CannedResponses(t *testing.T) {
	d := newDash(t)
	n, b := "Thanks", "Thank you for contacting us about {{issue.key}}, {{customer}}."
	if _, err := d.Svc.CreateCanned(ctx, d.Alice, "ABC", domain.CannedIn{Name: &n, Body: &b}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member made one: %v", err)
	}
	c, err := d.Svc.CreateCanned(ctx, d.Admin, "ABC", domain.CannedIn{Name: &n, Body: &b})
	if err != nil || c.Project != "ABC" || c.Body != b {
		t.Fatalf("create: %+v %v", c, err)
	}
	dup := "THANKS"
	if _, err := d.Svc.CreateCanned(ctx, d.Admin, "ABC", domain.CannedIn{Name: &dup, Body: &b}); !isInvalid(err) {
		t.Fatalf("duplicate: %v", err)
	}
	empty := " "
	if _, err := d.Svc.CreateCanned(ctx, d.Admin, "ABC", domain.CannedIn{Name: &dup, Body: &empty}); !isInvalid(err) {
		t.Fatalf("empty text: %v", err)
	}
	if got, err := d.Svc.ListCanned(ctx, d.Bob, "ABC"); err != nil || len(got) != 1 {
		t.Fatalf("list for a member: %+v %v", got, err)
	}
	if _, err := d.Svc.ListCanned(ctx, d.Bob, "XYZ"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider: %v", err)
	}
	// a viewer may read the project but cannot comment, so has no use for them
	viewer := d.NewUser("viewer3@example.com", "Vera", false)
	if err := d.Svc.AddMember(ctx, d.Admin, "ABC", viewer.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Svc.ListCanned(ctx, viewer, "ABC"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a viewer listed them: %v", err)
	}
	nb := "Changed {{agent}}"
	if u, err := d.Svc.UpdateCanned(ctx, d.Admin, c.ID, domain.CannedIn{Body: &nb}); err != nil || u.Body != nb || u.Name != "Thanks" {
		t.Fatalf("update: %+v %v", u, err)
	}
	if err := d.Svc.DeleteCanned(ctx, d.Bob, c.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member deleted one: %v", err)
	}
	if err := d.Svc.DeleteCanned(ctx, d.Admin, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Svc.UpdateCanned(ctx, d.Admin, c.ID, domain.CannedIn{Body: &nb}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a deleted one: %v", err)
	}
}
