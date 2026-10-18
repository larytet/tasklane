package domain_test

import (
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

type twoOrg struct {
	*testutil.Env
	boss, gina domain.Actor // organization B: its administrator and a member
	orgB       domain.Organization
}

func newTwoOrgs(t *testing.T) *twoOrg {
	e := testutil.New(t)
	e.Project()
	o, err := e.Svc.CreateOrganization(ctx, e.Admin, "Globex", "boss@globex.example", "Boss", "password123")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	e.Store.Pool.QueryRow(ctx, `SELECT id FROM app_user WHERE email='boss@globex.example'`).Scan(&id)
	boss, _ := e.Svc.ActorFor(ctx, id)
	if _, err := e.Svc.CreateProject(ctx, boss, "GLX", "Globex"); err != nil {
		t.Fatal(err)
	}
	gu, err := e.Svc.CreateUserAs(ctx, boss, "gina@globex.example", "Gina", "password123", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.AddMember(ctx, boss, "GLX", gu.ID, "member"); err != nil {
		t.Fatal(err)
	}
	gina, _ := e.Svc.ActorFor(ctx, gu.ID)
	return &twoOrg{Env: e, boss: boss, gina: gina, orgB: o}
}

func TestSEC09_OrganizationsAreCreatedByTheInstanceAdministratorOnly(t *testing.T) {
	e := newTwoOrgs(t)
	if _, err := e.Svc.CreateOrganization(ctx, e.boss, "Initech", "x@initech.example", "X", "password123"); err == nil {
		t.Fatal("an organization administrator created an organization")
	}
	if _, err := e.Svc.CreateOrganization(ctx, e.Alice, "Initech", "x@initech.example", "X", "password123"); err == nil {
		t.Fatal("a member created an organization")
	}
	if _, err := e.Svc.CreateOrganization(ctx, e.Admin, "globex", "y@globex.example", "Y", "password123"); err == nil {
		t.Fatal("a duplicate name (case aside) was accepted")
	}
	// no organization is left without its administrator when the administrator cannot be made
	n := func() (c int) { e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM organization`).Scan(&c); return }
	before := n()
	if _, err := e.Svc.CreateOrganization(ctx, e.Admin, "Hooli", "not-an-address", "Z", "password123"); err == nil || n() != before {
		t.Fatalf("an organization was left behind: %v", err)
	}
	if l, err := e.Svc.ListOrganizations(ctx, e.Admin); err != nil || len(l) != 2 {
		t.Fatalf("%v %v", l, err)
	}
	if _, err := e.Svc.ListOrganizations(ctx, e.boss); err == nil {
		t.Fatal("an organization administrator listed all organizations")
	}
	// the new administrator is an organization administrator, not an instance administrator
	if !e.boss.OrgAdmin || e.boss.Admin || e.boss.Org != e.orgB.ID {
		t.Fatalf("%+v", e.boss)
	}
	// the existing administrator (and a fresh install) are unchanged: instance administrator of the Default organization
	adm, _ := e.Svc.ActorFor(ctx, e.Admin.ID)
	if !adm.Admin || !adm.OrgAdmin || adm.OrgID() != domain.DefaultOrg {
		t.Fatalf("%+v", adm)
	}
}

func TestSEC09_NothingCrossesTheLineInMembershipListsOrAdministration(t *testing.T) {
	e := newTwoOrgs(t)
	// a person of B cannot be made a member of A's project, and the refusal is the same as for a person who does not exist
	err1 := e.Svc.AddMember(ctx, e.Admin, "ABC", e.gina.ID, "viewer")
	err2 := e.Svc.AddMember(ctx, e.Admin, "ABC", "00000000-0000-0000-0000-000000000000", "viewer")
	if err1 == nil || err2 == nil || err1.Error() != err2.Error() {
		t.Fatalf("%v / %v", err1, err2)
	}
	// and the database itself refuses a row that says otherwise
	var pid string
	e.Store.Pool.QueryRow(ctx, `SELECT id FROM project WHERE key='ABC'`).Scan(&pid)
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1,$2,'admin')`, pid, e.gina.ID); err == nil || !strings.Contains(err.Error(), "another organization") {
		t.Fatalf("the database accepted a cross-organization member: %v", err)
	}
	g, err := e.Svc.CreateGroup(ctx, e.Admin, "Engineers")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO user_group_member(group_id,user_id) VALUES($1,$2)`, g.ID, e.gina.ID); err == nil {
		t.Fatal("the database accepted a person of B in a group of A")
	}
	var glx string
	e.Store.Pool.QueryRow(ctx, `SELECT id FROM project WHERE key='GLX'`).Scan(&glx)
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO project_group_role(project_id,group_id,role) VALUES($1,$2,'admin')`, glx, g.ID); err == nil {
		t.Fatal("the database accepted a group of A with a role in a project of B")
	}
	// even if such a membership row existed, the view that decides access does not see it
	e.Store.Pool.Exec(ctx, `ALTER TABLE project_member DISABLE TRIGGER project_member_same_org`)
	e.Store.Pool.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1,$2,'admin')`, pid, e.gina.ID)
	e.Store.Pool.Exec(ctx, `ALTER TABLE project_member ENABLE TRIGGER project_member_same_org`)
	is := e.Issue("org A secret", "text")
	if _, err := e.Svc.View(ctx, e.gina, is.Key); err == nil {
		t.Fatal("a stray membership row let a person of B read A's issue")
	}
	if ps, _ := e.Svc.ListProjects(ctx, e.gina); len(ps) != 1 || ps[0].Key != "GLX" {
		t.Fatalf("%+v", ps)
	}
	e.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id=$1 AND project_id=$2`, e.gina.ID, pid)

	// lists
	for name, a := range map[string]domain.Actor{"member of B": e.gina, "administrator of B": e.boss} {
		us, _ := e.Svc.ListUsersFor(ctx, a)
		for _, u := range us {
			if !strings.HasSuffix(u.Email, "@globex.example") {
				t.Errorf("%s sees %s", name, u.Email)
			}
		}
		if len(us) != 2 {
			t.Errorf("%s sees %d people", name, len(us))
		}
		if _, err := e.Svc.View(ctx, a, is.Key); err == nil {
			t.Errorf("%s read an issue of A", name)
		}
		if _, err := e.Svc.AddComment(ctx, a, is.Key, "from B", false); err == nil {
			t.Errorf("%s commented on an issue of A", name)
		}
		if err := e.Svc.AddMember(ctx, a, "ABC", a.ID, "admin"); err == nil {
			t.Errorf("%s made themselves a member of A's project", name)
		}
		all, readable, _, _ := e.Svc.SearchScope(ctx, a)
		if all || len(readable) != 1 {
			t.Errorf("%s: search scope %v %v", name, all, readable)
		}
		if _, err := e.Svc.CreateIssue(ctx, a, domain.CreateIn{Project: "ABC", Summary: "planted"}); err == nil {
			t.Errorf("%s created an issue in A", name)
		}
	}
	// the operator sees everyone and everything, as before
	if all, _, _, _ := e.Svc.SearchScope(ctx, e.Admin); !all {
		t.Fatal("the instance administrator's search scope narrowed")
	}
	if us, _ := e.Svc.ListUsersFor(ctx, e.Admin); len(us) < 5 {
		t.Fatalf("%d", len(us))
	}
	// the organization administrator administers their own organization
	if ps, _ := e.Svc.ListProjects(ctx, e.boss); len(ps) != 1 || ps[0].Key != "GLX" {
		t.Fatalf("%+v", ps)
	}
	if _, err := e.Svc.CreateProject(ctx, e.boss, "GLX2", "More"); err != nil {
		t.Fatal(err)
	}
	var org string
	e.Store.Pool.QueryRow(ctx, `SELECT org_id::text FROM project WHERE key='GLX2'`).Scan(&org)
	if org != e.orgB.ID {
		t.Fatalf("the project went to %s", org)
	}
	// and not the instance: global settings and the other organization's settings are the instance administrator's
	if err := e.Svc.SetStorageQuota(ctx, e.boss, "GLX", nil); err == nil {
		t.Fatal("an organization administrator used an instance-level setting")
	}
	if err := e.Svc.SetStorageQuota(ctx, e.boss, "ABC", nil); err == nil {
		t.Fatal("an organization administrator changed a setting of another organization's project")
	}
}

func TestSEC09_PeopleAreCreatedInTheCreatorsOrganizationAndNeverBecomeInstanceAdministrators(t *testing.T) {
	e := newTwoOrgs(t)
	// an organization administrator may make another administrator, of their own organization only
	u, err := e.Svc.CreateUserAs(ctx, e.boss, "deputy@globex.example", "Deputy", "password123", true, e.OrgA())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := e.Svc.ActorFor(ctx, u.ID)
	if d.Org != e.orgB.ID || !d.OrgAdmin || d.Admin {
		t.Fatalf("an organization administrator created %+v (the organization they asked for is ignored)", d)
	}
	// a member creates no one
	if _, err := e.Svc.CreateUserAs(ctx, e.gina, "x@globex.example", "X", "password123", false, ""); err == nil {
		t.Fatal("a member created a person")
	}
	// the instance administrator may name the organization; an administrator made in another organization is not an instance administrator
	u2, err := e.Svc.CreateUserAs(ctx, e.Admin, "ops@globex.example", "Ops", "password123", true, e.orgB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := e.Svc.ActorFor(ctx, u2.ID); a.Admin || !a.OrgAdmin || a.Org != e.orgB.ID {
		t.Fatalf("%+v", a)
	}
	if _, err := e.Svc.CreateUserAs(ctx, e.Admin, "n@x.example", "N", "password123", false, "00000000-0000-0000-0000-00000000dead"); err == nil {
		t.Fatal("an unknown organization was accepted")
	}
	// an inactive placeholder of the Default organization (from an import) is not claimed by an administrator of another one
	e.Store.Pool.Exec(ctx, `INSERT INTO app_user(email,name,kind,active) VALUES('ghost@example.com','Ghost','human',false)`)
	if _, err := e.Svc.CreateUserAs(ctx, e.boss, "ghost@example.com", "Ghost", "password123", false, ""); err == nil {
		t.Fatal("a placeholder of another organization was claimed")
	}
	// a person's actor carries their organization through authentication
	tok, _ := e.Svc.CreateAPIToken(ctx, e.gina.ID, "t")
	if a, err := e.Svc.Authenticate(ctx, tok); err != nil || a.Org != e.orgB.ID || a.Admin || a.OrgAdmin {
		t.Fatalf("%+v %v", a, err)
	}
	// the same sentence about issues created by email: a person of B writing to an address of A's project is quarantined, not created
}

// OrgA is the Default organization's id.
func (e *twoOrg) OrgA() string { return domain.DefaultOrg }
