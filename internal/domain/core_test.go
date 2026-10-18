package domain_test

import (
	"context"
	"errors"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

var ctx = context.Background()

func TestCreateAndKeys(t *testing.T) { // CORE-01
	e := testutil.New(t)
	e.Project()
	a := e.Issue("first", "d")
	b := e.Issue("second", "")
	if a.Key != "ABC-1" || b.Key != "ABC-2" {
		t.Fatalf("keys %s %s", a.Key, b.Key)
	}
	if a.Status != "To Do" || a.Version != 1 || a.DescSeq != 1 || a.SummarySeq != 1 {
		t.Fatalf("%+v", a)
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "  "}); err == nil {
		t.Fatal("empty summary accepted")
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "x", Type: "Nope"}); err == nil {
		t.Fatal("unknown type accepted")
	}
}

func TestKeysAreUniqueUnderConcurrency(t *testing.T) { // CORE-01
	e := testutil.New(t)
	e.Project()
	const n = 25
	keys := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "c"})
			if err != nil {
				t.Error(err)
				keys <- ""
				return
			}
			keys <- is.Key
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		k := <-keys
		if seen[k] {
			t.Fatalf("duplicate key %s", k)
		}
		seen[k] = true
	}
}

func TestWorkflowTransitions(t *testing.T) { // CORE-02
	e := testutil.New(t)
	e.Project()
	is := e.Issue("w", "")
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	names := map[string]string{}
	for _, tr := range v.Transitions {
		names[tr.Name] = tr.ID
	}
	if _, ok := names["Start progress"]; !ok || len(v.Transitions) != 2 {
		t.Fatalf("transitions from To Do: %+v", v.Transitions)
	}
	is2, err := e.Svc.Transition(ctx, e.Alice, is.Key, names["Start progress"], nil)
	if err != nil || is2.Status != "In Progress" || is2.Version != 2 {
		t.Fatalf("%v %+v", err, is2)
	}
	// the same transition id is no longer valid from In Progress
	if _, err := e.Svc.Transition(ctx, e.Alice, is.Key, names["Start progress"], nil); err == nil {
		t.Fatal("invalid transition accepted")
	}
	v, _ = e.Svc.View(ctx, e.Alice, is.Key)
	for _, tr := range v.Transitions {
		if tr.Name == "Done" {
			d, err := e.Svc.Transition(ctx, e.Alice, is.Key, tr.ID, nil)
			if err != nil || d.Resolved == nil || d.StatusCat != "done" {
				t.Fatalf("%v %+v", err, d)
			}
		}
	}
}

func TestScalarFieldsMergeAcrossFields(t *testing.T) { // LIVE-02
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "")
	base := is.Version
	hi, lab := "high", []string{"payments", "bug"}
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, is.Key, &base, false, domain.Changes{Priority: &hi}); err != nil {
		t.Fatal(err)
	}
	// Bob's change to a different field, based on the same old version, merges without a dialog
	got, err := e.Svc.UpdateFields(ctx, e.Bob, is.Key, &base, false, domain.Changes{Labels: &lab})
	if err != nil || got.Priority != "high" || len(got.Labels) != 2 || got.Version != 3 {
		t.Fatalf("%v %+v", err, got)
	}
	// the same field again from the stale base is a conflict and applies nothing
	low := "low"
	_, err = e.Svc.UpdateFields(ctx, e.Bob, is.Key, &base, false, domain.Changes{Priority: &low})
	var vc *domain.VersionConflict
	if !errors.As(err, &vc) || vc.Fields[0] != "priority" {
		t.Fatalf("want conflict, got %v", err)
	}
	cur, _ := e.Svc.View(ctx, e.Bob, is.Key)
	if cur.Issue.Priority != "high" {
		t.Fatal("conflicting write was applied")
	}
	// strict If-Match: any stale version is rejected
	_, err = e.Svc.UpdateFields(ctx, e.Bob, is.Key, &base, true, domain.Changes{Priority: &low})
	if !errors.As(err, &vc) {
		t.Fatalf("strict: %v", err)
	}
}

func TestPermissions(t *testing.T) { // SEC-01..03
	e := testutil.New(t)
	e.Project()
	is := e.Issue("secret", "")
	carol := e.Admin
	_ = carol
	out, err := e.Svc.CreateUser(ctx, "eve@example.com", "Eve", "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	eve := domain.Actor{ID: out.ID, Label: "Eve"}
	if _, err := e.Svc.View(ctx, eve, is.Key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-member sees issue: %v", err)
	}
	if ps, _ := e.Svc.ListProjects(ctx, eve); len(ps) != 0 {
		t.Fatal("non-member lists project")
	}
	e.Svc.AddMember(ctx, e.Admin, "ABC", eve.ID, "viewer")
	if _, err := e.Svc.View(ctx, eve, is.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, eve, is.Key, "hi", false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("viewer commented: %v", err)
	}
	if _, err := e.Svc.CreateProject(ctx, e.Alice, "XYZ", "n"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("non-admin created project")
	}
	if _, err := e.Svc.CreateIssue(ctx, eve, domain.CreateIn{Project: "ABC", Summary: "x"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("viewer created issue")
	}
}

func TestAuthAndTokens(t *testing.T) { // SEC-05
	e := testutil.New(t)
	tok, u, err := e.Svc.Login(ctx, "ALICE@example.com", "password123", 1e9*3600)
	if err != nil || u.Name != "Alice K." {
		t.Fatal(err)
	}
	a, err := e.Svc.Authenticate(ctx, tok)
	if err != nil || a.Label != "Alice K." {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.Login(ctx, "alice@example.com", "wrong-password", 1e9); err == nil {
		t.Fatal("wrong password accepted")
	}
	e.Svc.Logout(ctx, tok)
	if _, err := e.Svc.Authenticate(ctx, tok); err == nil {
		t.Fatal("session survived logout")
	}
	api, _ := e.Svc.CreateAPIToken(ctx, u.ID, "ci-bot")
	b, err := e.Svc.Authenticate(ctx, api)
	if err != nil || b.Label != "API: ci-bot" {
		t.Fatalf("%v %+v", err, b)
	}
	// only a hash is stored
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM api_token WHERE encode(token_hash,'hex')=$1`, api).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored")
	}
}

// SEC-03: an internal comment is never shown to someone who cannot edit the issue.
func TestSEC03_InternalCommentsHiddenFromViewers(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "")
	vu, _ := e.Svc.CreateUser(ctx, "vera@example.com", "Vera", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	e.Svc.AddComment(ctx, e.Alice, is.Key, "public words", false)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "internal words", true)
	vv, _ := e.Svc.View(ctx, domain.Actor{ID: vu.ID, Label: "Vera"}, is.Key)
	if len(vv.Comments) != 1 || vv.Comments[0].Body != "public words" {
		t.Fatalf("viewer saw %+v", vv.Comments)
	}
	if mv, _ := e.Svc.View(ctx, e.Bob, is.Key); len(mv.Comments) != 2 {
		t.Fatalf("a member lost the internal comment: %d", len(mv.Comments))
	}
}
