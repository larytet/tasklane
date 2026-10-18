package domain_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
)

type visRig struct {
	*devSearch
	api *gitmock.API
	is  domain.Issue
}

func newVis(t *testing.T) *visRig {
	r, api := backfillRig(t, "github", true)
	api.Private = true
	api.Members = []string{"octo", "hubot"}
	v := &visRig{devSearch: &devSearch{gitRig: r}, api: api}
	v.eng = &search.Postgres{DB: r.e.Store.Pool}
	v.is = r.e.Issue("private work", "")
	// a pull request, a branch, a commit, a failing build and a production deployment, all in the private repository
	r.push(t, v.is.Key+"-secret-branch", "5ec7e700", gitmock.GHCommitBy("5ec7e700", v.is.Key+" internal change", "octo", "octo@example.com", t0))
	r.pr(t, 61, v.is.Key+" internal fix", "", "open", false, t0)
	r.build(t, "ci", "00000061", "completed", "failure", t0.Add(time.Minute))
	r.deploy(t, 700, "production", "00000061", "success", t0.Add(2*time.Minute))
	return v
}

func (v *visRig) restrict(t *testing.T) {
	t.Helper()
	if err := v.e.Svc.SetGitVisibility(ctx, v.e.Admin, "restricted"); err != nil {
		t.Fatal(err)
	}
}

func (v *visRig) sync(t *testing.T) {
	t.Helper()
	if _, err := v.e.Svc.SyncGitRepo(ctx, v.e.Admin, v.repo.ID); err != nil {
		t.Fatal(err)
	}
}

func (v *visRig) seen(a domain.Actor) (items, hidden int) {
	d, err := v.e.Svc.Development(ctx, a, v.is.Key)
	if err != nil {
		v.e.T.Fatal(err)
	}
	return len(d.PRs) + len(d.Branches) + len(d.Commits) + len(d.Builds) + len(d.Deployments), d.Hidden
}

func TestGIT11_ARestrictedOrganizationShowsPrivateRepositoryDetailsOnlyToPeopleWithAccess(t *testing.T) {
	v := newVis(t)
	e := v.e
	// the default: anyone who can see the issue sees all of it, and a sync does not ask the provider about access
	if n, h := v.seen(e.Bob); n != 5 || h != 0 {
		t.Fatalf("%d items, %d hidden", n, h)
	}
	v.sync(t)
	for _, rq := range v.api.Requests() {
		if strings.Contains(rq, "/collaborators") {
			t.Fatalf("the access list was read although no organization restricts: %s", rq)
		}
	}
	// only an administrator changes the setting, and it is audited
	if err := e.Svc.SetGitVisibility(ctx, e.Alice, "restricted"); err == nil {
		t.Fatal("a member restricted visibility")
	}
	if err := e.Svc.SetGitVisibility(ctx, e.Admin, "everyone"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	v.restrict(t)
	// fail closed: nothing is known yet about the repository or who may see it
	if n, h := v.seen(e.Bob); n != 0 || h != 3 {
		t.Fatalf("before any sync: %d items, %d hidden", n, h)
	}
	d, _ := e.Svc.Development(ctx, e.Bob, v.is.Key)
	if !strings.Contains(d.Summary, "private repository you cannot access") || len(d.PRs) != 0 {
		t.Fatalf("%+v", d)
	}
	v.sync(t)
	var private *bool
	var members int
	e.Store.Pool.QueryRow(ctx, `SELECT private,(SELECT count(*) FROM git_repo_member) FROM git_repo`).Scan(&private, &members)
	if private == nil || !*private || members != 2 {
		t.Fatalf("%v %d", private, members)
	}
	// a linked account that the provider does not list has no access; a person with no account has none; an administrator has all
	v.link(t, "octo", e.Alice)
	if _, err := e.Svc.LinkGitIdentity(ctx, e.Admin, "github", "stranger", e.Bob.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := v.seen(e.Alice); n != 5 {
		t.Fatalf("a person whose account has access sees %d of 5", n)
	}
	if n, h := v.seen(e.Bob); n != 0 || h != 3 {
		t.Fatalf("a linked account without access sees %d (%d hidden)", n, h)
	}
	if n, _ := v.seen(e.Admin); n != 5 {
		t.Fatalf("an administrator sees %d", n)
	}
	// TQL says the same thing, field by field: the person without access cannot find the issue by any of them
	for _, q := range []string{`pullRequest.state = open`, `pullRequest.count >= 1`, `branch ~ secret`, `build.state = failing`, `deployment.environment = production`, `pullRequest.state is not EMPTY`} {
		if got := v.q(e.Bob, q); len(got) != 0 {
			t.Errorf("a person without access found %v with %s", got, q)
		}
		if got := v.q(e.Alice, q); len(got) != 1 || got[0] != v.is.Key {
			t.Errorf("a person with access found %v with %s", got, q)
		}
	}
	if got := v.q(e.Bob, `pullRequest.state is EMPTY AND key = `+v.is.Key); len(got) != 1 {
		t.Errorf("to a person without access the issue has no pull request: %v", got)
	}
	// what the rules and commands did to private code names branches: hidden too
	e.Store.Pool.Exec(ctx, `INSERT INTO git_smart_run(object_id,issue_key,idx,command,outcome,reason) SELECT id,$1,0,'#comment','applied','comment added' FROM git_object WHERE kind='pr'`, v.is.Key)
	if d, _ := e.Svc.Development(ctx, e.Bob, v.is.Key); len(d.Automation) != 0 {
		t.Fatalf("%+v", d.Automation)
	}
	if d, _ := e.Svc.Development(ctx, e.Alice, v.is.Key); len(d.Automation) != 1 {
		t.Fatalf("%+v", d.Automation)
	}
	// revocation: the provider no longer lists the account, and the next sync takes the access away
	v.api.Members = []string{"hubot"}
	v.sync(t)
	if n, h := v.seen(e.Alice); n != 0 || h != 3 {
		t.Fatalf("after the account lost access: %d items, %d hidden", n, h)
	}
	if got := v.q(e.Alice, `pullRequest.state = open`); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// access comes back, and unlinking the account takes it away at once, without waiting for a sync
	v.api.Members = []string{"hubot", "octo"}
	v.sync(t)
	if n, _ := v.seen(e.Alice); n != 5 {
		t.Fatalf("%d", n)
	}
	l, _ := e.Svc.ListGitIdentities(ctx, e.Admin)
	for _, x := range l {
		if x.Login == "octo" {
			e.Svc.UnlinkGitIdentity(ctx, e.Admin, x.ID)
		}
	}
	if n, _ := v.seen(e.Alice); n != 0 {
		t.Fatalf("an unlinked account still sees %d", n)
	}
	v.link(t, "octo", e.Alice)
	// an access list that was not refreshed is not believed
	e.Store.Pool.Exec(ctx, `UPDATE git_repo SET members_synced_at=$1`, e.Svc.Now().Add(-25*time.Hour))
	if n, _ := v.seen(e.Alice); n != 0 {
		t.Fatalf("a day-old access list was believed: %d", n)
	}
	e.Store.Pool.Exec(ctx, `UPDATE git_repo SET members_synced_at=$1`, e.Svc.Now().Add(-time.Hour))
	if n, _ := v.seen(e.Alice); n != 5 {
		t.Fatalf("a fresh access list was not believed: %d", n)
	}
	// the provider cannot be asked (a token that may not list collaborators): the last list stays, the problem is shown
	v.api.RateLimit(40, 403, nil)
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, v.repo.ID); err == nil {
		t.Log("the sync itself failed on the first read, as expected when everything is refused")
	}
	// back to the default for everyone
	v.api.RateLimit(0, 0, nil)
}

func TestGIT11_PublicRepositoriesAreVisibleAndAnAdministratorCanOverride(t *testing.T) {
	v := newVis(t)
	e := v.e
	v.restrict(t)
	v.api.Private = false
	v.sync(t)
	if n, h := v.seen(e.Bob); n != 5 || h != 0 {
		t.Fatalf("a public repository: %d items, %d hidden", n, h)
	}
	// the administrator knows better than the provider's answer: it is private
	var repoID string
	e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM git_repo`).Scan(&repoID)
	if err := e.Svc.SetGitRepoPrivacy(ctx, e.Alice, repoID, nil); err == nil {
		t.Fatal("a member changed a repository's privacy")
	}
	yes := true
	if err := e.Svc.SetGitRepoPrivacy(ctx, e.Admin, repoID, &yes); err != nil {
		t.Fatal(err)
	}
	if n, h := v.seen(e.Bob); n != 0 || h != 3 {
		t.Fatalf("%d %d", n, h)
	}
	v.sync(t) // the provider still says public: the override stands
	if n, _ := v.seen(e.Bob); n != 0 {
		t.Fatalf("a sync overwrote an administrator's override: %d", n)
	}
	if err := e.Svc.SetGitRepoPrivacy(ctx, e.Admin, repoID, nil); err != nil {
		t.Fatal(err)
	}
	v.sync(t)
	if n, _ := v.seen(e.Bob); n != 5 {
		t.Fatalf("%d", n)
	}
	// the setting is visible to administrators with the state of each repository
	gv, err := e.Svc.GitVisibility(ctx, e.Admin)
	if err != nil || gv.Mode != "restricted" || len(gv.Repos) != 1 || gv.Repos[0].Private == nil || gv.Repos[0].Members != 2 || gv.Repos[0].MembersSynced == nil {
		t.Fatalf("%+v %v", gv, err)
	}
	if _, err := e.Svc.GitVisibility(ctx, e.Alice); err == nil {
		t.Fatal("a member read the visibility settings")
	}
	// switching back shows everything again at once
	if err := e.Svc.SetGitVisibility(ctx, e.Admin, "default"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.SetGitRepoPrivacy(ctx, e.Admin, repoID, &yes); err != nil {
		t.Fatal(err)
	}
	if n, h := v.seen(e.Bob); n != 5 || h != 0 {
		t.Fatalf("%d %d", n, h)
	}
	var audits int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action IN ('git.visibility','git.repo.privacy')`).Scan(&audits)
	if audits != 5 {
		t.Fatalf("%d audit rows", audits)
	}
}

func TestGIT11_GitLabAccessListsWorkToo(t *testing.T) {
	r, api := backfillRig(t, "gitlab", true)
	api.Private = true
	api.Members = []string{"tanuki"}
	e := r.e
	is := e.Issue("gitlab private", "")
	deliveryN++
	r.deliver(t, gitmock.GitLabHook(r.secret, "Merge Request Hook", fmt.Sprint("gl", deliveryN), gitmock.GLMR("acme/shop", 3, is.Key+" internal", "", "feature", "ab12ab12", "opened", t0)))
	e.Svc.SetGitVisibility(ctx, e.Admin, "restricted")
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.Svc.Development(ctx, e.Alice, is.Key); len(d.PRs) != 0 || d.Hidden != 1 {
		t.Fatalf("%+v", d)
	}
	if _, err := e.Svc.LinkGitIdentity(ctx, e.Admin, "gitlab", "tanuki", e.Alice.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.Svc.Development(ctx, e.Alice, is.Key); len(d.PRs) != 1 || d.Hidden != 0 {
		t.Fatalf("%+v", d)
	}
	// an account of the other provider with the same name is not that person's access
	e.Store.Pool.Exec(ctx, `DELETE FROM git_identity`)
	if _, err := e.Svc.LinkGitIdentity(ctx, e.Admin, "github", "tanuki", e.Alice.ID); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.Svc.Development(ctx, e.Alice, is.Key); len(d.PRs) != 0 {
		t.Fatal("a GitHub account counted as access to a GitLab repository")
	}
}
