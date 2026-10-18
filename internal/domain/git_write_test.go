package domain_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
)

// writeRig is a repository with a REST double that accepts writes, and a connection with write actions OFF.
func writeRig(t *testing.T, provider string) (*gitRig, *gitmock.API) {
	r, api := backfillRig(t, provider, true)
	api.AllowWrites = true
	r.e.Svc.BaseURL = "https://tasklane.example.com"
	return r, api
}

func (r *gitRig) writeOn(t *testing.T, on bool) {
	t.Helper()
	if err := r.e.Svc.SetGitConnectionOptions(ctx, r.e.Admin, r.conn.ID, domain.GitConnOptions{WriteEnabled: &on}); err != nil {
		t.Fatal(err)
	}
}

func (r *gitRig) conn1(t *testing.T) domain.GitConnection {
	cs, err := r.e.Svc.ListGitConnections(ctx, r.e.Admin)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%v %d", err, len(cs))
	}
	return cs[0]
}

func TestGIT14_CreateABranchFromAnIssue(t *testing.T) {
	r, api := writeRig(t, "github")
	e := r.e
	is := e.Issue("Fix the checkout flow!", "")
	// off until an administrator turns it on: nothing is sent, and the message says what to do
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{}); err == nil || !strings.Contains(err.Error(), "write actions turned on") {
		t.Fatalf("%v", err)
	}
	if len(api.Requests()) != 0 || len(api.PostsMade()) != 0 {
		t.Fatalf("a connection with write actions off was contacted: %v", api.Requests())
	}
	if err := e.Svc.SetGitConnectionOptions(ctx, e.Alice, r.conn.ID, domain.GitConnOptions{}); err == nil {
		t.Fatal("a member changed connection options")
	}
	r.writeOn(t, true)
	b, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{})
	if err != nil {
		t.Fatal(err)
	}
	want := is.Key + "-fix-the-checkout-flow"
	if b.Branch != want || b.Base != "main" || b.Repo != "acme/shop" || b.Command != "git fetch origin "+want+" && git switch "+want {
		t.Fatalf("%+v", b)
	}
	posts := api.PostsMade()
	if len(posts) != 1 || !strings.Contains(posts[0], "/repos/acme/shop/git/refs") || !strings.Contains(posts[0], `"ref":"refs/heads/`+want+`"`) || !strings.Contains(posts[0], "0123456789abcdef") {
		t.Fatalf("%v", posts)
	}
	// the new branch arrives by webhook like any other and links to the issue by its name
	r.deliver(t, gitmock.GitHubHook(r.secret, "push", "p-new", gitmock.GHPush("acme/shop", want, "abcabcab", nil, t0)))
	if d := dev(t, e, e.Alice, is.Key); len(d.Branches) != 1 || d.Branches[0].Title != want {
		t.Fatalf("%+v", d.Branches)
	}
	// the generated name is taken: a numbered one is made. A name the person chose that is taken is an error, not a surprise.
	api.ExistingBranches = map[string]bool{want: true, want + "-2": true}
	b2, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{})
	if err != nil || b2.Branch != want+"-3" {
		t.Fatalf("%+v %v", b2, err)
	}
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{Name: is.Key + "-mine"}); err != nil {
		t.Fatal(err)
	}
	api.ExistingBranches[is.Key+"-mine"] = true
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{Name: is.Key + "-mine"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("%v", err)
	}
	// a base branch can be chosen
	before := len(api.Requests())
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{Name: is.Key + "-from-release", Base: "release/1.2"}); err != nil {
		t.Fatal(err)
	}
	if rq := strings.Join(api.Requests()[before:], " "); !strings.Contains(rq, "/git/ref/heads/release%2F1.2") {
		t.Fatalf("%s", rq)
	}
	// names that are not branch names, or that would never link to the issue, are refused before anything is sent
	posts = api.PostsMade()
	for name, in := range map[string]domain.BranchIn{
		"no key":        {Name: "my-branch"},
		"dots":          {Name: is.Key + "-a..b"},
		"a space":       {Name: is.Key + " x"},
		"a lock suffix": {Name: is.Key + "-x.lock"},
		"leading dash":  {Name: "-" + is.Key},
		"unknown repo":  {Repo: "other/repo"},
	} {
		if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if len(api.PostsMade()) != len(posts) {
		t.Fatal("a refused request reached the provider")
	}
	// who may: a viewer and an outsider may not, and the log and the audit record what was done
	vu, _ := e.Svc.CreateUser(ctx, "vv@example.com", "VV", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	if _, err := e.Svc.CreateGitBranch(ctx, domain.Actor{ID: vu.ID, Label: "VV"}, is.Key, domain.BranchIn{}); err == nil {
		t.Fatal("a viewer created a branch")
	}
	eve, _ := e.Svc.CreateUser(ctx, "eve2@example.com", "Eve", "password123", false)
	if _, err := e.Svc.CreateGitBranch(ctx, domain.Actor{ID: eve.ID, Label: "Eve"}, is.Key, domain.BranchIn{}); err == nil {
		t.Fatal("an outsider created a branch")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='git.branch.create'`).Scan(&n)
	var logged int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_write_log WHERE kind='branch' AND outcome='done'`).Scan(&logged)
	if n != 4 || logged != 4 {
		t.Fatalf("%d audited, %d logged", n, logged)
	}
}

func TestGIT14_SeveralRepositoriesNeedAChoiceAndGitLabWorksToo(t *testing.T) {
	r, api := writeRig(t, "gitlab")
	e := r.e
	is := e.Issue("Gitlab work", "")
	r.writeOn(t, true)
	b, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{Base: "develop"})
	if err != nil {
		t.Fatal(err)
	}
	post := strings.Join(api.PostsMade(), " ")
	if b.Branch != is.Key+"-gitlab-work" || !strings.Contains(post, "/api/v4/projects/acme%2Fshop/repository/branches?branch="+is.Key+"-gitlab-work&ref=develop") || !strings.Contains(b.URL, "gitlab.example") {
		t.Fatalf("%+v %s", b, post)
	}
	api.ExistingBranches = map[string]bool{is.Key + "-gitlab-work": true}
	if b2, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{}); err != nil || b2.Branch != is.Key+"-gitlab-work-2" {
		t.Fatalf("%+v %v", b2, err)
	}
	// a second repository: the person must say which
	if _, err := e.Svc.MapGitRepo(ctx, e.Admin, r.conn.ID, "acme/other", []string{"ABC"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{}); err == nil || !strings.Contains(err.Error(), "name one of: acme/other, acme/shop") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{Repo: "acme/other", Name: is.Key + "-o"}); err != nil {
		t.Fatal(err)
	}
}

func TestGIT14_18_ATokenThatCannotWriteOrWasRevokedIsAWarningNotASilentFailure(t *testing.T) {
	r, api := writeRig(t, "github")
	e := r.e
	is := e.Issue("health", "")
	r.writeOn(t, true)
	if c := r.conn1(t); c.Health != "unknown" {
		t.Fatalf("%q", c.Health)
	}
	// the token reads but is not allowed to write
	api.WriteStatus = 403
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{}); err == nil || !strings.Contains(err.Error(), "not allowed to create the branch") || !strings.Contains(err.Error(), "write repository contents") {
		t.Fatalf("%v", err)
	}
	if c := r.conn1(t); c.Health != "no_write_permission" || !strings.Contains(c.HealthDetail, "can read but is not allowed to write") || c.HealthAt == nil {
		t.Fatalf("%+v", c)
	}
	var failed int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_write_log WHERE outcome='failed'`).Scan(&failed)
	if failed != 1 {
		t.Fatalf("%d failures logged", failed)
	}
	// reading keeps working and does not hide the standing problem
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	if c := r.conn1(t); c.Health != "no_write_permission" {
		t.Fatalf("a good read cleared a write problem: %q", c.Health)
	}
	// permission is given: the next write works and the warning goes
	api.WriteStatus = 0
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{}); err != nil {
		t.Fatal(err)
	}
	if c := r.conn1(t); c.Health != "ok" || c.HealthDetail != "" {
		t.Fatalf("%+v", c)
	}
	// the provider no longer accepts the token (revoked or expired): the sync fails, and the connection says why
	api.Token = "a-different-token"
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err == nil {
		t.Fatal("a revoked token synced")
	}
	if c := r.conn1(t); c.Health != "token_rejected" || !strings.Contains(c.HealthDetail, "revoked") {
		t.Fatalf("%+v", c)
	}
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{Name: is.Key + "-x"}); err == nil || !strings.Contains(err.Error(), "replace the token") {
		t.Fatalf("%v", err)
	}
	// the administrator's Check button reports it too, and says ok again once the token works
	if c, err := e.Svc.CheckGitConnection(ctx, e.Admin, r.conn.ID); err != nil || c.Health != "token_rejected" {
		t.Fatalf("%+v %v", c, err)
	}
	api.Token = "tok-backfill"
	if c, err := e.Svc.CheckGitConnection(ctx, e.Admin, r.conn.ID); err != nil || c.Health != "ok" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := e.Svc.CheckGitConnection(ctx, e.Alice, r.conn.ID); err == nil {
		t.Fatal("a member ran a connection check")
	}
}

func (r *gitRig) prEvent(t *testing.T, n int, title string, at time.Time) {
	deliveryN++
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", fmt.Sprint("bl", deliveryN), gitmock.GHPR("acme/shop", n, title, "", "feature-"+fmt.Sprint(n), fmt.Sprintf("%08d", n), "open", false, at)))
}

func TestGIT14_APullRequestGetsOneBackLinkAndNothingFromTheIssue(t *testing.T) {
	r, api := writeRig(t, "github")
	e := r.e
	is := e.Issue("A confidential summary", "secret description")
	// write actions off: no comment
	r.prEvent(t, 5, is.Key+" first", t0)
	if len(api.PostsMade()) != 0 {
		t.Fatal("a back-link was posted with write actions off")
	}
	r.writeOn(t, true)
	// a failing provider: logged, nothing remembered, so the next event tries again
	api.WriteStatus = 403
	r.prEvent(t, 5, is.Key+" first (edited)", t0.Add(time.Minute))
	if len(api.PostsMade()) != 0 {
		t.Fatal("a refused comment was recorded as posted")
	}
	api.WriteStatus = 0
	r.prEvent(t, 5, is.Key+" first (edited again)", t0.Add(2*time.Minute))
	posts := api.PostsMade()
	if len(posts) != 1 || !strings.Contains(posts[0], "/repos/acme/shop/issues/5/comments") || !strings.Contains(posts[0], "https://tasklane.example.com/#/issue/"+is.Key) {
		t.Fatalf("%v", posts)
	}
	if strings.Contains(posts[0], "confidential") || strings.Contains(posts[0], "secret") {
		t.Fatalf("something from the issue was posted to the provider: %s", posts[0])
	}
	// once per pull request and issue, however many events follow
	r.prEvent(t, 5, is.Key+" first (edited a third time)", t0.Add(3*time.Minute))
	if len(api.PostsMade()) != 1 {
		t.Fatalf("%d comments", len(api.PostsMade()))
	}
	// a backfill (not live) never writes
	two := e.Issue("another", "")
	api.PRs = []gitmock.M{{"number": float64(8), "title": two.Key + " via backfill", "body": "", "state": "open", "html_url": "https://x/8", "user": gitmock.M{"login": "octo"},
		"head": gitmock.M{"ref": "f8", "sha": "aaaa0008"}, "base": gitmock.M{"ref": "main"}, "updated_at": e.Svc.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}}
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	if len(api.PostsMade()) != 1 {
		t.Fatal("a backfill wrote to the provider")
	}
	// a second pull request mentioning two issues links both
	r.prEvent(t, 9, is.Key+" and "+two.Key, t0.Add(4*time.Minute))
	if len(api.PostsMade()) != 3 {
		t.Fatalf("%d comments after a pull request with two issues", len(api.PostsMade()))
	}
}

func TestGIT18_DisconnectKeepsLinksReadOnlyOrPurgesThem(t *testing.T) {
	r, api := writeRig(t, "github")
	e := r.e
	is := e.Issue("keep me", "")
	r.writeOn(t, true)
	r.prEvent(t, 3, is.Key+" work", t0)
	if d := dev(t, e, e.Alice, is.Key); len(d.PRs) != 1 {
		t.Fatalf("%+v", d)
	}
	if err := e.Svc.DisconnectGitConnection(ctx, e.Alice, r.conn.ID, "keep_read_only"); err == nil {
		t.Fatal("a member disconnected a connection")
	}
	if err := e.Svc.DisconnectGitConnection(ctx, e.Admin, r.conn.ID, "later"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := e.Svc.DisconnectGitConnection(ctx, e.Admin, r.conn.ID, "keep_read_only"); err != nil {
		t.Fatal(err)
	}
	// the links stay on the ticket
	if d := dev(t, e, e.Alice, is.Key); len(d.PRs) != 1 {
		t.Fatalf("the links were lost: %+v", d)
	}
	// nothing more comes in or goes out
	h := gitmock.GitHubHook(r.secret, "pull_request", "after-disconnect", gitmock.GHPR("acme/shop", 4, is.Key+" later", "", "f4", "44444444", "open", false, t0.Add(time.Hour)))
	if _, err := e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1", h.Header, h.Body); err == nil {
		t.Fatal("a disconnected connection accepted a webhook")
	}
	hits := api.Hits
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err == nil || api.Hits != hits {
		t.Fatalf("a disconnected connection synced: %v", err)
	}
	if _, err := e.Svc.CreateGitBranch(ctx, e.Alice, is.Key, domain.BranchIn{}); err == nil {
		t.Fatal("a disconnected connection wrote a branch")
	}
	c := r.conn1(t)
	if !c.Disabled || c.WriteEnabled || !strings.Contains(c.HealthDetail, "kept read-only") {
		t.Fatalf("%+v", c)
	}
	// purge removes the connection and everything it brought
	if err := e.Svc.DisconnectGitConnection(ctx, e.Admin, r.conn.ID, "purge"); err != nil {
		t.Fatal(err)
	}
	if d := dev(t, e, e.Alice, is.Key); len(d.PRs) != 0 {
		t.Fatalf("purge left links: %+v", d)
	}
	if err := e.Svc.DisconnectGitConnection(ctx, e.Admin, r.conn.ID, "purge"); err == nil {
		t.Fatal("purged twice")
	}
}
