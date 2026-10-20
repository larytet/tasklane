package domain_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
)

type devSearch struct {
	*gitRig
	eng *search.Postgres
}

func (d *devSearch) q(a domain.Actor, query string) []string {
	d.e.T.Helper()
	all, rd, wr, err := d.e.Svc.SearchScope(ctx, a)
	if err != nil {
		d.e.T.Fatal(err)
	}
	res, err := d.eng.Search(ctx, search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: d.e.Svc.Now}, query, search.Page{Limit: 100}) // the service clock: access lists age by it
	if err != nil {
		d.e.T.Fatalf("%s: %v", query, err)
	}
	var keys []string
	for _, is := range res.Rows {
		keys = append(keys, is.Key)
	}
	sort.Strings(keys)
	return keys
}

func (d *devSearch) want(a domain.Actor, query string, keys ...string) {
	d.e.T.Helper()
	sort.Strings(keys)
	if got := d.q(a, query); strings.Join(got, ",") != strings.Join(keys, ",") {
		d.e.T.Errorf("%s\n   got  %v\n   want %v", query, got, keys)
	}
}

func (r *gitRig) build(t *testing.T, name, sha, status, conclusion string, at time.Time) {
	deliveryN++
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", fmt.Sprint("ck", deliveryN), gitmock.GHCheck("acme/shop", name, sha, status, conclusion, at)))
}

func (r *gitRig) deploy(t *testing.T, id int, env, sha, state string, at time.Time) {
	deliveryN++
	r.deliver(t, gitmock.GitHubHook(r.secret, "deployment_status", fmt.Sprint("dp", deliveryN), gitmock.GHDeploy("acme/shop", id, env, sha, state, at)))
}

func TestGIT19_DevelopmentFieldsInTQLWithPermissionFiltering(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	d := &devSearch{gitRig: r, eng: &search.Postgres{DB: e.Store.Pool}}
	a, b, c, dd, ee := e.Issue("alpha", ""), e.Issue("beta", ""), e.Issue("gamma", ""), e.Issue("delta", ""), e.Issue("epsilon", "")
	sha := func(n int) string { return fmt.Sprintf("%08d", n) }
	// alpha: an open pull request, a branch, a failing build
	r.push(t, a.Key+"-login-page", "aaaaaaaa")
	r.pr(t, 1, a.Key+" login page", "", "open", false, t0)
	r.build(t, "ci", sha(1), "completed", "failure", t0.Add(time.Minute))
	// beta: a merged pull request, a passing build, deployed to production
	r.pr(t, 2, b.Key+" payment", "", "closed", true, t0)
	r.build(t, "ci", sha(2), "completed", "success", t0.Add(time.Minute))
	r.deploy(t, 100, "production", sha(2), "success", t0.Add(2*time.Minute))
	// gamma: a draft and an open pull request, deployed to staging and with a failed production deployment
	r.pr(t, 3, c.Key+" first try", "", "draft", false, t0)
	r.pr(t, 4, c.Key+" second try", "", "open", false, t0)
	r.deploy(t, 101, "staging", sha(4), "success", t0.Add(3*time.Minute))
	r.deploy(t, 102, "production", sha(4), "failure", t0.Add(4*time.Minute))
	// epsilon: two builds, one passing and one failing
	r.pr(t, 5, ee.Key+" flaky", "", "open", false, t0)
	r.build(t, "unit", sha(5), "completed", "success", t0.Add(time.Minute))
	r.build(t, "lint", sha(5), "completed", "failure", t0.Add(time.Minute))
	// delta: no code at all

	// pull requests
	d.want(e.Alice, `pullRequest.state = open`, a.Key, c.Key, ee.Key)
	d.want(e.Alice, `pullRequest.state = merged`, b.Key)
	d.want(e.Alice, `pullRequest.state in (draft, merged)`, b.Key, c.Key)
	d.want(e.Alice, `pullRequest.state != merged`, a.Key, c.Key, dd.Key, ee.Key) // no pull request at all is "not merged"
	d.want(e.Alice, `pullRequest.state is EMPTY`, dd.Key)
	d.want(e.Alice, `pullRequest.state is not EMPTY`, a.Key, b.Key, c.Key, ee.Key)
	d.want(e.Alice, `pullRequest.count >= 2`, c.Key)
	d.want(e.Alice, `pullRequest.count = 0`, dd.Key)
	d.want(e.Alice, `pullRequest.count > 0 AND pullRequest.count < 2`, a.Key, b.Key, ee.Key)
	// a review changes the review state
	deliveryN++
	rv := gitmock.GHPR("acme/shop", 1, a.Key+" login page", "", "feature-1", sha(1), "open", false, t0.Add(10*time.Minute))
	rv["review"] = gitmock.M{"state": "approved", "submitted_at": t0.Add(10 * time.Minute).UTC().Format(time.RFC3339)}
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request_review", fmt.Sprint("rv", deliveryN), rv))
	d.want(e.Alice, `pullRequest.review = approved`, a.Key)
	// branches
	d.want(e.Alice, `branch ~ login`, a.Key)
	d.want(e.Alice, `branch ~ "LOGIN-PAGE"`, a.Key) // case does not matter
	d.want(e.Alice, `branch = "`+a.Key+`-login-page"`, a.Key)
	d.want(e.Alice, `branch is EMPTY AND key = `+dd.Key, dd.Key)
	d.want(e.Alice, `branch ~ nothing-like-this`)
	// builds: the worst state across the issue's builds
	d.want(e.Alice, `build.state = failing`, a.Key, ee.Key)
	d.want(e.Alice, `build.state = passing`, b.Key)
	d.want(e.Alice, `build.state is EMPTY`, c.Key, dd.Key)
	// a re-run replaces the old result
	r.build(t, "ci", sha(1), "completed", "success", t0.Add(20*time.Minute))
	d.want(e.Alice, `build.state = failing`, ee.Key)
	d.want(e.Alice, `build.state in (passing, running)`, a.Key, b.Key)
	// deployments: where it is deployed now
	d.want(e.Alice, `deployment.environment = production`, b.Key)
	d.want(e.Alice, `deployment.environment = staging`, c.Key)
	d.want(e.Alice, `deployment.environment in (staging, production)`, b.Key, c.Key)
	r.deploy(t, 103, "production", sha(2), "failure", t0.Add(30*time.Minute)) // the newer production deployment failed
	d.want(e.Alice, `deployment.environment = production`)
	d.want(e.Alice, `deployment.environment is not EMPTY`, b.Key, c.Key)
	// combined with the rest of the language, and ordering still works
	d.want(e.Alice, `project = ABC AND pullRequest.state = open AND status = "To Do" AND build.state != failing`, a.Key, c.Key)
	if got := d.q(e.Alice, `pullRequest.count >= 1 ORDER BY key ASC`); len(got) != 4 {
		t.Fatalf("%v", got)
	}
	// bad queries say what is wrong
	for q, want := range map[string]string{
		`pullRequest.state = bogus`:  "closed, draft, merged, open",
		`pullRequest.count > many`:   "whole number",
		`build.state ~ failing`:      "cannot be used with build.state",
		`build.state = broken`:       "failing, passing, running",
		`branch ~ EMPTY`:             "needs",
		`deployment.environment > 1`: "cannot be used",
	} {
		all, rd, wr, _ := e.Svc.SearchScope(ctx, e.Alice)
		_, err := d.eng.Search(ctx, search.Scope{UserID: e.Alice.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: time.Now}, q, search.Page{Limit: 10})
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Errorf("%s: %v", q, err)
		}
	}
	// permission filtering: a person who cannot see the issues finds nothing, whatever the query, and a viewer sees what a member sees
	eve, _ := e.Svc.CreateUser(ctx, "eve3@example.com", "Eve", "password123", false)
	outsider := domain.Actor{ID: eve.ID, Label: "Eve"}
	for _, q := range []string{`pullRequest.count >= 0`, `pullRequest.state != merged`, `build.state = failing`, `branch ~ login`, `deployment.environment = staging`, `pullRequest.state is EMPTY`} {
		if got := d.q(outsider, q); len(got) != 0 {
			t.Errorf("an outsider found %v with %s", got, q)
		}
	}
	vu, _ := e.Svc.CreateUser(ctx, "vv3@example.com", "VV", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	d.want(domain.Actor{ID: vu.ID, Label: "VV"}, `build.state = failing`, ee.Key)
	// an issue moved to the trash, or one only administrators may see, is not returned
	e.Store.Pool.Exec(ctx, `UPDATE issue SET restricted=true WHERE key=$1`, ee.Key)
	d.want(e.Alice, `build.state = failing`)
	d.want(e.Admin, `build.state = failing`, ee.Key)
	life(t, e, e.Admin, c.Key, domain.LifecycleIn{Op: "delete"})
	d.want(e.Alice, `deployment.environment = staging`)
}
