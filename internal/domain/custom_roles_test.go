package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func mkRole(t *testing.T, e *testutil.Env, name, base string, grants ...string) domain.CustomRole {
	t.Helper()
	r, err := e.Svc.CreateCustomRole(ctx, e.Admin, domain.CustomRoleIn{Name: name, Base: base, Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPERM02_CustomRolesAreManagedByAdministratorsWithinTheirOrganizationAndCannotBeDeletedWhileInUse(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	if _, err := e.Svc.CreateCustomRole(ctx, e.Alice, domain.CustomRoleIn{Name: "X", Base: "viewer"}); err != domain.ErrForbidden {
		t.Fatalf("a member made a role: %v", err)
	}
	for _, bad := range []domain.CustomRoleIn{
		{Name: "", Base: "viewer"}, {Name: strings.Repeat("x", 61), Base: "viewer"}, {Name: "Member", Base: "viewer"}, {Name: "administrator", Base: "viewer"},
		{Name: "Ok", Base: "admin"}, {Name: "Ok", Base: "viewer", Grants: []string{"delete_everything"}},
	} {
		if _, err := e.Svc.CreateCustomRole(ctx, e.Admin, bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	r := mkRole(t, e, "Reporter", "viewer", "create", "comment", "comment") // a repeated grant counts once
	if len(r.Grants) != 2 || r.Grants[0] != "comment" || r.Grants[1] != "create" {
		t.Fatalf("%v", r.Grants)
	}
	if _, err := e.Svc.CreateCustomRole(ctx, e.Admin, domain.CustomRoleIn{Name: "reporter", Base: "member"}); err == nil {
		t.Fatal("a second role with the same name (in another case) was accepted")
	}
	// anyone in the organization can list; names are what a project administrator chooses from
	if l, _ := e.Svc.ListCustomRoles(ctx, e.Alice); len(l) != 1 || l[0].Name != "Reporter" || l[0].InUse != 0 {
		t.Fatalf("%+v", l)
	}
	// another organization sees nothing of it and cannot use its name
	ob, err := e.Svc.CreateOrganization(ctx, e.Admin, "Globex", "boss@globex.example", "Boss", "password123")
	if err != nil {
		t.Fatal(err)
	}
	ul, _ := e.Svc.ListUsersFor(ctx, e.Admin)
	var boss domain.Actor
	for _, u := range ul {
		if u.Email == "boss@globex.example" {
			boss, _ = e.Svc.ActorFor(ctx, u.ID)
		}
	}
	if l, _ := e.Svc.ListCustomRoles(ctx, boss); len(l) != 0 {
		t.Fatalf("another organization's roles are visible: %+v", l)
	}
	if err := e.Svc.UpdateCustomRole(ctx, boss, r.ID, domain.CustomRoleIn{Name: "Hijack", Base: "member"}); err != domain.ErrNotFound {
		t.Fatalf("another organization's administrator changed the role: %v", err)
	}
	if err := e.Svc.DeleteCustomRole(ctx, boss, r.ID); err != domain.ErrNotFound {
		t.Fatalf("another organization's administrator deleted the role: %v", err)
	}
	if _, err := e.Svc.CreateProject(ctx, boss, "GLX", "Globex"); err != nil {
		t.Fatal(err)
	}
	u2, _ := e.Svc.CreateUserAs(ctx, boss, "gina@globex.example", "Gina", "password123", false, "")
	if err := e.Svc.AddMember(ctx, boss, "GLX", u2.ID, "Reporter"); err == nil {
		t.Fatal("another organization's role was given in this one")
	}
	_ = ob
	// a role in use cannot be deleted; unused ones can; everything is audited
	vera := e.NewUser("vera@example.com", "Vera", false)
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "Reporter"); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.Svc.ListCustomRoles(ctx, e.Alice); l[0].InUse != 1 {
		t.Fatalf("%+v", l)
	}
	if err := e.Svc.DeleteCustomRole(ctx, e.Admin, r.ID); err == nil || !strings.Contains(err.Error(), "still given") {
		t.Fatalf("a role in use was deleted: %v", err)
	}
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.DeleteCustomRole(ctx, e.Admin, r.ID); err != nil {
		t.Fatalf("an unused role could not be deleted: %v", err)
	}
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "Reporter"); err == nil {
		t.Fatal("a deleted role was given")
	}
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "nonsense"); err == nil {
		t.Fatal("an unknown role was given")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action IN ('role.created','role.deleted')`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d audit records", n)
	}
}

func TestPERM02_ACustomRoleGrantsOnlyWhatItNamesAndRolesWithDifferentGrantsAreIndependent(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	is := e.Issue("custom roles", "text")
	reporter, triager, talker, mover := e.NewUser("rep@example.com", "Rita", false), e.NewUser("tri@example.com", "Tom", false), e.NewUser("tal@example.com", "Tess", false), e.NewUser("mov@example.com", "Max", false)
	mkRole(t, e, "Reporter", "viewer", "create", "comment")
	mkRole(t, e, "Triager", "viewer", "comment", "transition", "assign")
	mkRole(t, e, "Talker", "viewer", "comment")
	mkRole(t, e, "Mover", "viewer", "transition")
	for who, role := range map[domain.Actor]string{reporter: "Reporter", triager: "Triager", talker: "Talker", mover: "Mover"} {
		if err := e.Svc.AddMember(ctx, e.Admin, "ABC", who.ID, role); err != nil {
			t.Fatal(err)
		}
	}
	can := func(a domain.Actor, what string) bool {
		var err error
		switch what {
		case "read":
			_, err = e.Svc.View(ctx, a, is.Key)
		case "create":
			_, err = e.Svc.CreateIssue(ctx, a, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "by " + a.Label})
		case "comment":
			_, err = e.Svc.AddComment(ctx, a, is.Key, "hello", false)
		case "transition":
			if tid, ok := trans(t, e, e.Admin, is.Key, "Start progress"); ok {
				_, err = e.Svc.Transition(ctx, a, is.Key, tid, nil)
				if err == nil { // put it back for the next person
					back, _ := trans(t, e, e.Admin, is.Key, "Back to To Do")
					e.Svc.Transition(ctx, e.Admin, is.Key, back, nil)
				}
			}
		case "assign":
			bob := e.Bob.ID
			_, err = e.Svc.UpdateFields(ctx, a, is.Key, nil, false, domain.Changes{Assignee: &bob})
		case "edit":
			hi := "high"
			_, err = e.Svc.UpdateFields(ctx, a, is.Key, nil, false, domain.Changes{Priority: &hi})
		}
		if err != nil && err != domain.ErrForbidden {
			t.Fatalf("%s as %s: %v", what, a.Label, err)
		}
		return err == nil
	}
	want := map[domain.Actor]map[string]bool{
		reporter: {"read": true, "create": true, "comment": true, "transition": false, "assign": false, "edit": false},
		triager:  {"read": true, "create": false, "comment": true, "transition": true, "assign": true, "edit": false},
		talker:   {"read": true, "create": false, "comment": true, "transition": false, "assign": false, "edit": false},
		mover:    {"read": true, "create": false, "comment": false, "transition": true, "assign": false, "edit": false},
	}
	for who, row := range want {
		for what, w := range row {
			if got := can(who, what); got != w {
				t.Errorf("%s may %s: %v, want %v", who.Label, what, got, w)
			}
		}
	}
	// a plain viewer, for contrast, may only read
	vera := e.NewUser("vera@example.com", "Vera", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "viewer")
	for _, what := range []string{"create", "comment", "transition", "assign", "edit"} {
		if can(vera, what) {
			t.Errorf("a plain viewer may %s", what)
		}
	}
	// an outsider with no role still sees the project as nonexistent, whatever roles exist
	if _, err := e.Svc.View(ctx, e.NewUser("out@example.com", "Out", false), is.Key); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	// the role's grant is explicit: it holds even where the project's scheme asks for more, and the scheme still governs members
	if err := e.Svc.SetPermissionScheme(ctx, e.Admin, "ABC", map[string]string{"comment": "admin"}); err != nil {
		t.Fatal(err)
	}
	if !can(talker, "comment") {
		t.Error("a granted permission was lost to the scheme")
	}
	if can(e.Bob, "comment") {
		t.Error("the scheme no longer limits members")
	}
}

func TestPERM02_RolesFromGroupsAndFromTheirOwnAddUpAndEditsApplyAtOnce(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	is := e.Issue("group roles", "text")
	talker := mkRole(t, e, "Talker", "viewer", "comment")
	mkRole(t, e, "Mover", "viewer", "transition")
	vera := e.NewUser("vera@example.com", "Vera", false)
	// a person with one custom role directly and another through a group has both sets of grants
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "Talker"); err != nil {
		t.Fatal(err)
	}
	g, _ := e.Svc.CreateGroup(ctx, e.Admin, "Release managers")
	e.Svc.AddGroupMember(ctx, e.Admin, g.ID, vera.ID)
	if err := e.Svc.SetProjectGroup(ctx, e.Admin, "ABC", g.ID, "Mover"); err != nil {
		t.Fatal(err)
	}
	move := func() error {
		tid, _ := trans(t, e, e.Admin, is.Key, "Start progress")
		_, err := e.Svc.Transition(ctx, vera, is.Key, tid, nil)
		if err == nil {
			back, _ := trans(t, e, e.Admin, is.Key, "Back to To Do")
			e.Svc.Transition(ctx, e.Admin, is.Key, back, nil)
		}
		return err
	}
	if _, err := e.Svc.AddComment(ctx, vera, is.Key, "from the direct role", false); err != nil {
		t.Fatalf("the direct role's grant is missing: %v", err)
	}
	if err := move(); err != nil {
		t.Fatalf("the group role's grant is missing: %v", err)
	}
	if pg, _ := e.Svc.ProjectGroups(ctx, e.Admin, "ABC"); len(pg) != 1 || pg[0]["role"] != "viewer" {
		t.Fatalf("the group's stored base: %+v", pg)
	}
	// the highest base wins and the grants still add up: a direct member who also has a group custom role
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "member"); err != nil {
		t.Fatal(err)
	}
	hi := "high"
	if _, err := e.Svc.UpdateFields(ctx, vera, is.Key, nil, false, domain.Changes{Priority: &hi}); err != nil {
		t.Fatalf("a member could not edit: %v", err)
	}
	if err := e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "Talker"); err != nil {
		t.Fatal(err)
	}
	// editing the role takes effect for everyone who has it at once: take the comment grant away
	if err := e.Svc.UpdateCustomRole(ctx, e.Admin, talker.ID, domain.CustomRoleIn{Name: "Talker", Base: "viewer", Grants: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, vera, is.Key, "after", false); err != domain.ErrForbidden {
		t.Fatalf("a removed grant still worked: %v", err)
	}
	// and a role's base can be raised: everyone with it becomes a member
	if err := e.Svc.UpdateCustomRole(ctx, e.Admin, talker.ID, domain.CustomRoleIn{Name: "Talker", Base: "member", Grants: []string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, vera, is.Key, "now a member", false); err != nil {
		t.Fatalf("raising the base had no effect: %v", err)
	}
	var base string
	e.Store.Pool.QueryRow(ctx, `SELECT role FROM project_member WHERE user_id::text=$1`, vera.ID).Scan(&base)
	if base != "member" {
		t.Fatalf("the stored role is %s", base)
	}
	// and rename and re-assign: the audit keeps what the role was
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='role.updated' AND detail->>'was_base'='viewer'`).Scan(&n)
	if n < 1 {
		t.Fatalf("an update did not record what the role was")
	}
	// project administrators (not only instance administrators) may give roles; members may not
	if err := e.Svc.AddMember(ctx, e.Alice, "ABC", vera.ID, "Talker"); err == nil {
		t.Fatal("a member gave a role")
	}
}
