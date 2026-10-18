package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestPERM01_GroupsGrantProjectRolesAndTheHighestRoleWins(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Other"); err != nil {
		t.Fatal(err)
	}
	carol := e.NewUser("carol@example.com", "Carol", false)
	xi, _ := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Type: "Task", Summary: "in the other project"})
	// carol sees nothing of XYZ until a group she is in is given a role there
	if _, err := e.Svc.View(ctx, carol, xi.Key); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	g, err := e.Svc.CreateGroup(ctx, e.Admin, "Support")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.AddGroupMember(ctx, e.Admin, g.ID, "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.View(ctx, carol, xi.Key); err != domain.ErrNotFound {
		t.Fatal("a group with no role in the project gave access")
	}
	if err := e.Svc.SetProjectGroup(ctx, e.Admin, "XYZ", g.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	v, err := e.Svc.View(ctx, carol, xi.Key)
	if err != nil || v.Role != "viewer" {
		t.Fatalf("%v %v", v.Role, err)
	}
	if _, err := e.Svc.AddComment(ctx, carol, xi.Key, "x", false); err == nil {
		t.Fatal("a group viewer commented")
	}
	// search, lists and notifications all follow the same membership
	all, readable, _, _ := e.Svc.SearchScope(ctx, carol)
	var xp string
	e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM project WHERE key='XYZ'`).Scan(&xp)
	if all || len(readable) != 1 || readable[0] != xp {
		t.Fatalf("%v %v", all, readable)
	}
	if ps, _ := e.Svc.ListProjects(ctx, carol); len(ps) != 1 || ps[0].Key != "XYZ" {
		t.Fatalf("%+v", ps)
	}
	// the highest role wins: a direct viewer grant plus a group member grant is member, a group admin grant is admin
	e.Svc.AddMember(ctx, e.Admin, "XYZ", carol.ID, "viewer")
	e.Svc.SetProjectGroup(ctx, e.Admin, "XYZ", g.ID, "member")
	if v, _ := e.Svc.View(ctx, carol, xi.Key); v.Role != "member" {
		t.Fatalf("%s", v.Role)
	}
	if _, err := e.Svc.AddComment(ctx, carol, xi.Key, "now allowed", false); err != nil {
		t.Fatal(err)
	}
	e.Svc.SetProjectGroup(ctx, e.Admin, "XYZ", g.ID, "admin")
	grp, _ := e.Svc.ProjectGroups(ctx, carol, "XYZ")
	if len(grp) != 1 || grp[0]["role"] != "admin" || grp[0]["members"] != 1 {
		t.Fatalf("a group administrator could not see the project's groups: %v", grp)
	}
	// she can be assigned work through the group
	if _, err := e.Svc.UpdateFields(ctx, e.Admin, xi.Key, nil, false, domain.Changes{Assignee: &carol.ID}); err != nil {
		t.Fatalf("a group member could not be assigned: %v", err)
	}
	// removing her from the group takes the group's role away, her direct role stays; deleting the group drops its roles
	if err := e.Svc.RemoveGroupMember(ctx, e.Admin, g.ID, carol.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Svc.View(ctx, carol, xi.Key); v.Role != "viewer" {
		t.Fatalf("%s", v.Role)
	}
	if err := e.Svc.RemoveGroupMember(ctx, e.Admin, g.ID, carol.ID); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	e.Svc.AddGroupMember(ctx, e.Admin, g.ID, carol.ID)
	if err := e.Svc.DeleteGroup(ctx, e.Admin, g.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Svc.View(ctx, carol, xi.Key); v.Role != "viewer" {
		t.Fatalf("%s", v.Role)
	}
	e.Svc.AddMember(ctx, e.Admin, "XYZ", carol.ID, "viewer")
	// permissions to manage groups
	if _, err := e.Svc.CreateGroup(ctx, e.Alice, "Nope"); err == nil {
		t.Fatal("a member made a group")
	}
	if _, err := e.Svc.CreateGroup(ctx, e.Admin, "support"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.CreateGroup(ctx, e.Admin, "SUPPORT"); err == nil {
		t.Fatal("a duplicate group name")
	}
	if err := e.Svc.AddGroupMember(ctx, e.Admin, "00000000-0000-0000-0000-000000000000", carol.ID); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.AddGroupMember(ctx, e.Admin, g.ID, "nobody@example.com"); err == nil {
		t.Fatal("an unknown person was added")
	}
	_ = search.Postgres{}
	_ = time.Now
}

func TestPERM01_GroupAdministrationByProjectAdministratorsOnly(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	g, _ := e.Svc.CreateGroup(ctx, e.Admin, "Reviewers")
	if err := e.Svc.SetProjectGroup(ctx, e.Alice, "ABC", g.ID, "member"); err != domain.ErrForbidden {
		t.Fatalf("a member gave a group a role: %v", err)
	}
	outsider := e.NewUser("outsider@example.com", "Out", false)
	if err := e.Svc.SetProjectGroup(ctx, outsider, "ABC", g.ID, "member"); err != domain.ErrNotFound {
		t.Fatalf("an outsider learned something: %v", err)
	}
	e.Svc.AddMember(ctx, e.Admin, "ABC", e.Bob.ID, "admin")
	if err := e.Svc.SetProjectGroup(ctx, e.Bob, "ABC", g.ID, "member"); err != nil {
		t.Fatalf("a project administrator could not: %v", err)
	}
	if err := e.Svc.SetProjectGroup(ctx, e.Bob, "ABC", g.ID, "owner"); err == nil {
		t.Fatal("a made-up role")
	}
	if err := e.Svc.SetProjectGroup(ctx, e.Bob, "ABC", "00000000-0000-0000-0000-000000000000", "member"); err == nil {
		t.Fatal("an unknown group")
	}
	if err := e.Svc.SetProjectGroup(ctx, e.Bob, "ABC", g.ID, ""); err != nil {
		t.Fatal(err)
	}
	gg, _ := e.Svc.GetGroup(ctx, e.Admin, g.ID)
	if len(gg.Roles) != 0 {
		t.Fatalf("%+v", gg)
	}
}

func TestPERM01_PermissionSchemeChangesWhoMayDoWhat(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	viewer := e.NewUser("vera@example.com", "Vera", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "scheme"})
	// the defaults: viewers read, members work, only administrators manage sprints
	if _, err := e.Svc.AddComment(ctx, viewer, is.Key, "x", false); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.CreateIssue(ctx, viewer, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "v"}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	pt, _ := e.Svc.PermissionScheme(ctx, e.Alice, "ABC")
	if pt.Effective["comment"] != "member" || pt.Effective["manage_sprints"] != "admin" || len(pt.Overrides) != 0 {
		t.Fatalf("%+v", pt)
	}
	// open comment and create to viewers (a reporter-style role)
	if err := e.Svc.SetPermissionScheme(ctx, e.Admin, "ABC", map[string]string{"comment": "viewer", "create": "viewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, viewer, is.Key, "a reporter's comment", false); err != nil {
		t.Fatalf("%v", err)
	}
	vi, err := e.Svc.CreateIssue(ctx, viewer, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "from a reporter"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	// but not edit, transition or assign: those need edit rights, which a viewer never has
	hi := "high"
	if _, err := e.Svc.UpdateFields(ctx, viewer, is.Key, nil, false, domain.Changes{Priority: &hi}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if tid, ok := trans(t, e, e.Admin, is.Key, "Start progress"); ok {
		if _, err := e.Svc.Transition(ctx, viewer, is.Key, tid, nil); err != domain.ErrForbidden {
			t.Fatalf("%v", err)
		}
	}
	// archived work stays read-only whatever the scheme says
	rid, _ := trans(t, e, e.Admin, vi.Key, "Resolve")
	e.Svc.Transition(ctx, e.Alice, vi.Key, rid, nil)
	if _, err := e.Svc.ArchiveIssue(ctx, e.Alice, vi.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, viewer, vi.Key, "too late", false); err == nil {
		t.Fatal("a viewer commented on archived work")
	}
	// raise a permission above the default: members can no longer create, administrators still can
	if err := e.Svc.SetPermissionScheme(ctx, e.Admin, "ABC", map[string]string{"create": "admin", "transition": "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "m"}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "a"}); err != nil {
		t.Fatal(err)
	}
	if tid, ok := trans(t, e, e.Admin, is.Key, "Start progress"); ok {
		if _, err := e.Svc.Transition(ctx, e.Alice, is.Key, tid, nil); err != domain.ErrForbidden {
			t.Fatalf("a member transitioned although the scheme says administrators: %v", err)
		}
		if _, err := e.Svc.Transition(ctx, e.Admin, is.Key, tid, nil); err != nil {
			t.Fatal(err)
		}
	}
	// assign needs its permission on top of edit rights
	e.Svc.SetPermissionScheme(ctx, e.Admin, "ABC", map[string]string{"assign": "admin"})
	bob := e.Bob.ID
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, is.Key, nil, false, domain.Changes{Assignee: &bob}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, is.Key, nil, false, domain.Changes{Priority: &hi}); err != nil {
		t.Fatalf("other edits are unaffected: %v", err)
	}
	// sprints: opened to members
	b, _ := e.Svc.CreateBoard(ctx, e.Admin, "ABC", "Scrum", "scrum")
	if _, err := e.Svc.CreateSprint(ctx, e.Alice, b.ID, domain.SprintIn{}); err != domain.ErrForbidden {
		t.Fatalf("%v", err)
	}
	e.Svc.SetPermissionScheme(ctx, e.Admin, "ABC", map[string]string{"manage_sprints": "member"})
	if _, err := e.Svc.CreateSprint(ctx, e.Alice, b.ID, domain.SprintIn{}); err != nil {
		t.Fatalf("members could not manage sprints after the scheme allowed it: %v", err)
	}
	// resetting a permission goes back to the default, and the scheme is per project
	e.Svc.SetPermissionScheme(ctx, e.Admin, "ABC", map[string]string{"create": "", "transition": "", "assign": "", "comment": ""})
	if pt, _ := e.Svc.PermissionScheme(ctx, e.Alice, "ABC"); pt.Overrides["create"] != "" || pt.Overrides["manage_sprints"] != "member" {
		t.Fatalf("%+v", pt)
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "m again"}); err != nil {
		t.Fatalf("%v", err)
	}
	// validation and who may change a scheme
	for name, bad := range map[string]map[string]string{"unknown permission": {"delete_everything": "member"}, "unknown role": {"comment": "owner"},
		"viewers cannot edit": {"transition": "viewer"}, "viewers cannot assign": {"assign": "viewer"}, "viewers cannot manage sprints": {"manage_sprints": "viewer"}} {
		if err := e.Svc.SetPermissionScheme(ctx, e.Admin, "ABC", bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := e.Svc.SetPermissionScheme(ctx, e.Alice, "ABC", map[string]string{"comment": "viewer"}); err != domain.ErrForbidden {
		t.Fatalf("a member changed the scheme: %v", err)
	}
	if _, err := e.Svc.PermissionScheme(ctx, e.NewUser("o@example.com", "O", false), "ABC"); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	var audited int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='permission_scheme.save'`).Scan(&audited)
	if audited != 5 {
		t.Fatalf("%d audit rows", audited)
	}
	_ = strings.ToLower
}
