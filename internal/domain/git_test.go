package domain_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
)

type gitRig struct {
	e      *testutil.Env
	conn   domain.GitConnection
	secret string
	repo   domain.GitRepo
}

func newGit(t *testing.T, provider string, projects ...string) *gitRig {
	e := testutil.New(t)
	e.Project()
	if len(projects) == 0 {
		projects = []string{"ABC"}
	}
	r := &gitRig{e: e}
	var err error
	full := "acme/shop"
	base := ""
	if provider == "gitlab" {
		base = "https://gitlab.example.com"
	}
	r.conn, r.secret, err = e.Svc.CreateGitConnection(ctx, e.Admin, domain.GitConnIn{Name: provider + "-main", Provider: provider, BaseURL: base, Token: "tok-" + provider})
	if err != nil {
		t.Fatal(err)
	}
	if r.repo, err = e.Svc.MapGitRepo(ctx, e.Admin, r.conn.ID, full, projects); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *gitRig) deliver(t *testing.T, h gitmock.Hook) domain.WebhookResult {
	t.Helper()
	res, err := r.e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1234", h.Header, h.Body)
	if err != nil {
		t.Fatalf("delivery %q: %v", h.Header.Get("X-GitHub-Delivery"), err)
	}
	if _, err := r.e.Svc.ProcessGitInbox(ctx, 100); err != nil {
		t.Fatal(err)
	}
	return res
}

func dev(t *testing.T, e *testutil.Env, a domain.Actor, key string) domain.DevPanel {
	d, err := e.Svc.Development(ctx, a, key)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var t0 = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)

func TestGIT_WebhookSignaturesReplaysSizeAndSourceAddress(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	is := e.Issue("shop checkout", "")
	hook := func(secret, delivery string) gitmock.Hook {
		return gitmock.GitHubHook(secret, "pull_request", delivery, gitmock.GHPR("acme/shop", 1, is.Key+" fix", "", "feature", "aaa111", "open", false, t0))
	}
	// a wrong secret, a missing signature and a signature scheme we do not accept are all refused and store nothing
	bad := hook("not-the-secret", "d1")
	if _, err := e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1", bad.Header, bad.Body); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	none := hook(r.secret, "d1")
	none.Header.Del("X-Hub-Signature-256")
	if _, err := e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1", none.Header, none.Body); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	sha1 := hook(r.secret, "d1")
	sha1.Header.Set("X-Hub-Signature-256", strings.Replace(sha1.Header.Get("X-Hub-Signature-256"), "sha256=", "sha1=", 1))
	if _, err := e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1", sha1.Header, sha1.Body); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	tampered := hook(r.secret, "d1")
	tampered.Body = append(tampered.Body, ' ')
	if _, err := e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1", tampered.Header, tampered.Body); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a changed body verified: %v", err)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_inbox`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d deliveries stored from refused requests", n)
	}
	// a good delivery is stored and acknowledged; the same delivery again is a duplicate and changes nothing
	if res := r.deliver(t, hook(r.secret, "d1")); res.Duplicate {
		t.Fatal("first delivery marked duplicate")
	}
	if res := r.deliver(t, hook(r.secret, "d1")); !res.Duplicate {
		t.Fatal("a replay was not recognised")
	}
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_object`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d objects", n)
	}
	// GitLab sends a token, and older versions no delivery id: the content hash makes a replay a duplicate
	lr := newGit(t, "gitlab")
	lh := gitmock.GitLabHook(lr.secret, "Merge Request Hook", "", gitmock.GLMR("acme/shop", 3, "t", "", "b", "ccc", "opened", t0))
	if res := lr.deliver(t, lh); res.Duplicate {
		t.Fatal()
	}
	if res := lr.deliver(t, lh); !res.Duplicate {
		t.Fatal("a GitLab replay without an id was not recognised")
	}
	wrong := gitmock.GitLabHook("nope", "Merge Request Hook", "u", gitmock.GLMR("acme/shop", 3, "t", "", "b", "ccc", "opened", t0))
	if _, err := lr.e.Svc.ReceiveGitWebhook(ctx, lr.conn.ID, "192.0.2.1:1", wrong.Header, wrong.Body); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	// size limit, unknown connection
	big := hook(r.secret, "big")
	big.Body = make([]byte, domain.MaxWebhookBytes+1)
	var ptl *domain.PayloadTooLarge
	if _, err := e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1", big.Header, big.Body); !errors.As(err, &ptl) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.ReceiveGitWebhook(ctx, "00000000-0000-0000-0000-000000000000", "192.0.2.1:1", bad.Header, bad.Body); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// source-address allowlist
	c2, s2, _ := e.Svc.CreateGitConnection(ctx, e.Admin, domain.GitConnIn{Name: "locked", Provider: "github", AllowedCIDRs: []string{"192.30.252.0/22"}})
	h2 := gitmock.GitHubHook(s2, "ping", "p1", gitmock.M{})
	if _, err := e.Svc.ReceiveGitWebhook(ctx, c2.ID, "203.0.113.5:443", h2.Header, h2.Body); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an address outside the allowlist was accepted: %v", err)
	}
	if _, err := e.Svc.ReceiveGitWebhook(ctx, c2.ID, "192.30.253.9:443", h2.Header, h2.Body); err != nil {
		t.Fatalf("an address inside the allowlist was refused: %v", err)
	}
}

func TestGIT_CredentialsAreEncryptedAndShownAsFingerprints(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	cs, err := e.Svc.ListGitConnections(ctx, e.Admin)
	if err != nil || len(cs) != 1 || cs[0].TokenFP == "" || cs[0].WebhookFP == "" || cs[0].TokenFP == "tok-github" {
		t.Fatalf("%v %+v", err, cs)
	}
	var tok, wh []byte
	e.Store.Pool.QueryRow(ctx, `SELECT token_enc,webhook_secret_enc FROM git_connection`).Scan(&tok, &wh)
	if strings.Contains(string(tok), "tok-github") || strings.Contains(string(wh), r.secret) || len(tok) < 20 {
		t.Fatal("a credential is stored in the clear")
	}
	if len(r.secret) < 32 {
		t.Fatal("a short webhook secret")
	}
	for _, f := range []func() error{
		func() error { _, err := e.Svc.ListGitConnections(ctx, e.Alice); return err },
		func() error {
			_, _, err := e.Svc.CreateGitConnection(ctx, e.Alice, domain.GitConnIn{Name: "x", Provider: "github"})
			return err
		},
		func() error { _, err := e.Svc.MapGitRepo(ctx, e.Alice, r.conn.ID, "a/b", []string{"ABC"}); return err },
		func() error { _, err := e.Svc.ListGitRepos(ctx, e.Alice); return err },
		func() error { _, err := e.Svc.SyncGitRepo(ctx, e.Alice, r.repo.ID); return err },
		func() error { return e.Svc.DeleteGitConnection(ctx, e.Alice, r.conn.ID) },
	} {
		if err := f(); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("a member managed the integration: %v", err)
		}
	}
	for name, in := range map[string]domain.GitConnIn{
		"no name":       {Provider: "github"},
		"bad provider":  {Name: "x", Provider: "svn"},
		"gitlab no url": {Name: "x", Provider: "gitlab"},
		"http url":      {Name: "x", Provider: "github", BaseURL: "http://api.example.com"},
		"bad cidr":      {Name: "x", Provider: "github", AllowedCIDRs: []string{"not-a-cidr"}},
	} {
		if _, _, err := e.Svc.CreateGitConnection(ctx, e.Admin, in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := e.Svc.MapGitRepo(ctx, e.Admin, r.conn.ID, "no-slash", []string{"ABC"}); err == nil {
		t.Error("a bad repository name was accepted")
	}
	if _, err := e.Svc.MapGitRepo(ctx, e.Admin, r.conn.ID, "a/b", []string{"NOPE"}); err == nil {
		t.Error("an unknown project was accepted")
	}
	// the key encrypting them is not the stored ciphertext's key: a different instance key cannot read them
	e.Svc.SecretKey = make([]byte, 32)
	if _, err := e.Svc.ReceiveGitWebhook(ctx, r.conn.ID, "192.0.2.1:1", http.Header{"X-Hub-Signature-256": {"sha256=00"}, "X-Github-Event": {"ping"}}, []byte("{}")); err == nil || errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a different key decrypted the secret: %v", err)
	}
}

func TestGIT_LinkingRules(t *testing.T) {
	r := newGit(t, "github", "ABC")
	e := r.e
	one, two := e.Issue("one", ""), e.Issue("two", "")
	e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Other")
	other, _ := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Type: "Task", Summary: "unmapped project"})
	jana, _ := e.Svc.CreateUser(ctx, "octo@example.com", "Octo", "password123", false)
	_ = jana
	ghc := func(sha, msg string) gitmock.M { return gitmock.GHCommit(sha, msg, "octo@example.com", t0) }
	// branch name, commit messages, PR title and description, in any case, several keys, with noise that must not link
	r.deliver(t, gitmock.GitHubHook(r.secret, "push", "p1", gitmock.GHPush("acme/shop", "feature/"+strings.ToLower(one.Key)+"-redirect", "bbb222", []gitmock.M{
		ghc("c1c1c1c1", "fix the redirect ("+two.Key+")\n\nsee UTF-8 and "+other.Key+" and ABC-9999"), ghc("c2c2c2c2", "unrelated work")}, t0)))
	d1, d2 := dev(t, e, e.Alice, one.Key), dev(t, e, e.Alice, two.Key)
	if len(d1.Branches) != 1 || len(d1.Commits) != 0 {
		t.Fatalf("branch name link: %+v", d1)
	}
	if len(d2.Commits) != 1 || d2.Commits[0].Title != "fix the redirect ("+two.Key+")" || len(d2.Branches) != 0 {
		t.Fatalf("commit message link: %+v", d2)
	}
	if d := dev(t, e, e.Admin, other.Key); len(d.Commits) != 0 { // a project the repository is not mapped to never links
		t.Fatalf("%+v", d)
	}
	// a PR names two issues in its title and one in its description
	pr := gitmock.GHPR("acme/shop", 7, "Handle "+one.Key+" and "+two.Key, "Closes "+strings.ToLower(one.Key)+".", "x", "d3d3d3", "open", false, t0)
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "pr1", pr))
	for _, k := range []string{one.Key, two.Key} {
		if d := dev(t, e, e.Alice, k); len(d.PRs) != 1 {
			t.Fatalf("%s: %+v", k, d.PRs)
		}
	}
	// editing the title removes an automatic link and adds another; a manual link stays through the edit
	if err := e.Svc.AddGitLink(ctx, e.Alice, two.Key, "https://github.com/acme/shop/pull/7"); err != nil {
		t.Fatal(err)
	}
	pr = gitmock.GHPR("acme/shop", 7, "Handle only "+one.Key, "", "x", "d3d3d3", "open", false, t0.Add(time.Minute))
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "pr2", pr))
	if d := dev(t, e, e.Alice, one.Key); len(d.PRs) != 1 {
		t.Fatalf("%+v", d.PRs)
	}
	if d := dev(t, e, e.Alice, two.Key); len(d.PRs) != 1 || !d.PRs[0].Manual { // still there, now only because a person linked it
		t.Fatalf("%+v", d.PRs)
	}
	if err := e.Svc.RemoveGitLink(ctx, e.Alice, two.Key, dev(t, e, e.Alice, two.Key).PRs[0].ID); err != nil {
		t.Fatal(err)
	}
	if d := dev(t, e, e.Alice, two.Key); len(d.PRs) != 0 {
		t.Fatalf("%+v", d.PRs)
	}
	// an issue that was moved keeps linking through its old key
	moved := life(t, e, e.Admin, one.Key, domain.LifecycleIn{Op: "move", Project: "XYZ"})
	_ = moved
	// (the old key still resolves, but XYZ is not mapped: the link was made before, and stays on the moved issue)
	if d := dev(t, e, e.Admin, moved.Key2); len(d.PRs) != 1 {
		t.Fatalf("a moved issue lost its links: %+v", d.PRs)
	}
	// a trashed issue is never linked, and a missing key is ignored
	life(t, e, e.Admin, two.Key, domain.LifecycleIn{Op: "delete"})
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "pr3", gitmock.GHPR("acme/shop", 8, "Mentions "+two.Key+" and ABC-424242", "", "y", "e4e4e4", "open", false, t0)))
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_link l JOIN git_object o ON o.id=l.object_id WHERE o.ext_id='8'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d links to a trashed or missing issue", n)
	}
	// the author is matched to a person by email
	var uid *string
	e.Store.Pool.QueryRow(ctx, `SELECT author_user::text FROM git_object WHERE ext_id='c1c1c1c1'`).Scan(&uid)
	if uid == nil {
		t.Fatal("the committer was not matched to the user with that email")
	}
	// an unknown repository's events are ignored, not errors
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "pr4", gitmock.GHPR("someone/else", 1, one.Key, "", "z", "f5f5f5", "open", false, t0)))
	var failed int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_inbox WHERE error<>''`).Scan(&failed)
	if failed != 0 {
		t.Fatalf("%d deliveries failed", failed)
	}
	// repository case does not matter, and a link survives a PR being re-sent
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "pr5", gitmock.GHPR("ACME/Shop", 7, "Handle only "+one.Key, "", "x", "d3d3d3", "open", false, t0.Add(2*time.Minute))))
	if d := dev(t, e, e.Admin, moved.Key2); len(d.PRs) != 1 { // the old key in the text still finds the moved issue
		t.Fatalf("%+v", d.PRs)
	}
}

func TestGIT_DevelopmentPanelSummaryBuildsAndDeployments(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	is := e.Issue("ship it", "")
	sha := "abcdef1234567890"
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "1", gitmock.GHPR("acme/shop", 12, is.Key+": ship", "", "feature", sha, "open", false, t0)))
	r.deliver(t, gitmock.GitHubHook(r.secret, "push", "2", gitmock.GHPush("acme/shop", "feature", sha, []gitmock.M{
		gitmock.GHCommit(sha, is.Key+" first", "a@example.com", t0), gitmock.GHCommit("1111111111", is.Key+" second", "a@example.com", t0.Add(time.Second))}, t0)))
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", "3", gitmock.GHCheck("acme/shop", "ci/build", sha, "completed", "success", t0)))
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", "4", gitmock.GHCheck("acme/shop", "ci/lint", sha, "in_progress", "", t0)))
	d := dev(t, e, e.Alice, is.Key)
	if d.Build != "running" || len(d.Builds) != 2 {
		t.Fatalf("%+v", d)
	}
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", "5", gitmock.GHCheck("acme/shop", "ci/lint", sha, "completed", "success", t0.Add(time.Minute))))
	r.deliver(t, gitmock.GitHubHook(r.secret, "deployment_status", "6", gitmock.GHDeploy("acme/shop", 99, "staging", sha, "success", t0.Add(2*time.Minute))))
	d = dev(t, e, e.Alice, is.Key)
	if d.Summary != "1 PR open, 2 commits, build passing, deployed to staging" { // the spec's own example, in this wording
		t.Fatalf("%q", d.Summary)
	}
	if len(d.PRs) != 1 || d.PRs[0].State != "open" || d.PRs[0].Branch != "feature" || len(d.Deployments) != 1 || d.Deployments[0].Env != "staging" {
		t.Fatalf("%+v", d)
	}
	// a failing check turns the build red; a merged PR changes the wording
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", "7", gitmock.GHCheck("acme/shop", "ci/e2e", sha, "completed", "failure", t0.Add(3*time.Minute))))
	if d = dev(t, e, e.Alice, is.Key); d.Build != "failing" || !strings.Contains(d.Summary, "build failing") {
		t.Fatalf("%q", d.Summary)
	}
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "8", gitmock.GHPR("acme/shop", 12, is.Key+": ship", "", "feature", sha, "closed", true, t0.Add(4*time.Minute))))
	if d = dev(t, e, e.Alice, is.Key); !strings.HasPrefix(d.Summary, "1 PR merged") || d.PRs[0].State != "merged" {
		t.Fatalf("%q", d.Summary)
	}
	// the review state comes with approvals and survives later updates of the PR
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request_review", "9", gitmock.M{"repository": gitmock.M{"full_name": "acme/shop"},
		"review":       gitmock.M{"state": "approved", "submitted_at": t0.Add(5 * time.Minute).Format(time.RFC3339)},
		"pull_request": gitmock.GHPR("acme/shop", 12, "", "", "feature", sha, "open", false, t0.Add(4*time.Minute))["pull_request"]}))
	if d = dev(t, e, e.Alice, is.Key); d.PRs[0].Review != "approved" || d.PRs[0].State != "merged" {
		t.Fatalf("a review rewrote the PR: %+v", d.PRs[0])
	}
	// who may see it: the same people who may see the issue
	out, _ := e.Svc.CreateUser(ctx, "out@example.com", "Out", "password123", false)
	if _, err := e.Svc.Development(ctx, domain.Actor{ID: out.ID, Kind: "human"}, is.Key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider saw the panel: %v", err)
	}
	if _, err := e.Svc.Development(ctx, e.Alice, "ABC-9999"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// an issue with nothing linked has an empty panel and no summary
	if d := dev(t, e, e.Alice, e.Issue("bare", "").Key); d.Summary != "" || len(d.PRs)+len(d.Commits) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestGIT_OutOfOrderEventsConverge(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	is := e.Issue("order", "")
	sha := "0a0a0a0a0a"
	// the merged event is delivered first and the older "open" event second: the stored state is the provider's latest
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "late", gitmock.GHPR("acme/shop", 5, is.Key+" new title", "", "b", sha, "closed", true, t0.Add(10*time.Minute))))
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", "early", gitmock.GHPR("acme/shop", 5, is.Key+" old title", "", "b", sha, "open", false, t0)))
	if d := dev(t, e, e.Alice, is.Key); d.PRs[0].State != "merged" || d.PRs[0].Title != is.Key+" new title" {
		t.Fatalf("an old event overwrote a newer one: %+v", d.PRs[0])
	}
	// builds: a running event that is older than a finished one does not bring the build back to running
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", "done", gitmock.GHCheck("acme/shop", "ci", sha, "completed", "success", t0.Add(10*time.Minute))))
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", "start", gitmock.GHCheck("acme/shop", "ci", sha, "in_progress", "", t0.Add(5*time.Minute))))
	if d := dev(t, e, e.Alice, is.Key); d.Build != "passing" {
		t.Fatalf("%q", d.Build)
	}
	// the same set of events in any order gives the same final state
	final := func(order []int) string {
		rr := newGit(t, "github")
		is := rr.e.Issue("x", "")
		evs := []gitmock.Hook{
			gitmock.GitHubHook(rr.secret, "pull_request", "a", gitmock.GHPR("acme/shop", 1, is.Key+" v1", "", "b", "s1s1s1", "draft", false, t0)),
			gitmock.GitHubHook(rr.secret, "pull_request", "b", gitmock.GHPR("acme/shop", 1, is.Key+" v2", "", "b", "s2s2s2", "open", false, t0.Add(time.Minute))),
			gitmock.GitHubHook(rr.secret, "pull_request", "c", gitmock.GHPR("acme/shop", 1, is.Key+" v3", "", "b", "s3s3s3", "closed", true, t0.Add(2*time.Minute))),
		}
		for _, i := range order {
			rr.deliver(t, evs[i])
		}
		d := dev(t, rr.e, rr.e.Alice, is.Key)
		return fmt.Sprintf("%s|%s|%s", d.PRs[0].State, d.PRs[0].Title[len(is.Key):], d.PRs[0].SHA)
	}
	want := final([]int{0, 1, 2})
	for _, o := range [][]int{{2, 1, 0}, {1, 2, 0}, {2, 0, 1}, {1, 0, 2}, {0, 2, 1}} {
		if got := final(o); got != want {
			t.Fatalf("order %v ended in %q, in order it ends in %q", o, got, want)
		}
	}
	// a deleted branch leaves the panel; the pull request stays
	r.deliver(t, gitmock.GitHubHook(r.secret, "push", "p", gitmock.GHPush("acme/shop", is.Key+"-gone", "9f9f9f", nil, t0)))
	if d := dev(t, e, e.Alice, is.Key); len(d.Branches) != 1 {
		t.Fatalf("%+v", d.Branches)
	}
	del := gitmock.GHPush("acme/shop", is.Key+"-gone", "0000", nil, t0)
	del["deleted"] = true
	r.deliver(t, gitmock.GitHubHook(r.secret, "push", "pd", del))
	if d := dev(t, e, e.Alice, is.Key); len(d.Branches) != 0 || len(d.PRs) != 1 {
		t.Fatalf("%+v", d)
	}
}

func TestGIT_LargePushIsCappedAndBadPayloadsAreKept(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	is := e.Issue("big push", "")
	var cs []gitmock.M
	for i := 0; i < 250; i++ {
		cs = append(cs, gitmock.GHCommit(fmt.Sprintf("%040d", i), fmt.Sprintf("%s change %d", is.Key, i), "a@example.com", t0.Add(time.Duration(i)*time.Second)))
	}
	r.deliver(t, gitmock.GitHubHook(r.secret, "push", "big", gitmock.GHPush("acme/shop", "main", fmt.Sprintf("%040d", 249), cs, t0)))
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_object WHERE kind='commit'`).Scan(&n)
	var note int
	e.Store.Pool.QueryRow(ctx, `SELECT COALESCE((meta->>'commits_not_expanded')::int,0) FROM git_object WHERE kind='branch'`).Scan(&note)
	if n != 100 || note != 150 {
		t.Fatalf("%d commits stored, %d summarized", n, note)
	}
	// a delivery that cannot be applied is retried a few times and then stays visible with its error
	hk := gitmock.GitHubHook(r.secret, "pull_request", "bad2", gitmock.M{"repository": gitmock.M{"full_name": "acme/shop"}}) // a PR event without a PR
	r.deliver(t, hk)
	for i := 0; i < 3; i++ {
		e.Svc.ProcessGitInbox(ctx, 10)
	}
	var attempts int
	var msg string
	e.Store.Pool.QueryRow(ctx, `SELECT attempts,error FROM git_inbox WHERE delivery_id='bad2'`).Scan(&attempts, &msg)
	if attempts < 3 || msg == "" {
		t.Fatalf("%d %q", attempts, msg)
	}
	// processed deliveries keep no payload: commit messages and file lists are not retained
	var kept int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_inbox WHERE processed_at IS NOT NULL AND length(payload)>0`).Scan(&kept)
	if kept != 0 {
		t.Fatalf("%d processed deliveries still hold their payload", kept)
	}
}

func backfillRig(t *testing.T, provider string, allowPrivate bool) (*gitRig, *gitmock.API) {
	api := gitmock.New(provider, "tok-backfill")
	t.Cleanup(api.Close)
	e := testutil.New(t)
	e.Project()
	conn, secret, err := e.Svc.CreateGitConnection(ctx, e.Admin, domain.GitConnIn{Name: provider, Provider: provider, BaseURL: api.URL, Token: "tok-backfill", AllowPrivate: true}) // plain http needs it to be created
	if err == nil && !allowPrivate {
		_, err = e.Store.Pool.Exec(ctx, `UPDATE git_connection SET allow_private=false`)
	}
	if err != nil {
		t.Fatal(err)
	}
	repo, err := e.Svc.MapGitRepo(ctx, e.Admin, conn.ID, "acme/shop", []string{"ABC"})
	if err != nil {
		t.Fatal(err)
	}
	return &gitRig{e: e, conn: conn, secret: secret, repo: repo}, api
}

func TestGIT_BackfillGitHubReadsOnlyAndRespectsRateLimitsAndETags(t *testing.T) {
	r, api := backfillRig(t, "github", true)
	e := r.e
	is := e.Issue("backfilled", "")
	upd := func(d time.Duration) string { return e.Svc.Now().Add(-d).UTC().Format(time.RFC3339) }
	pr := func(n int, title, state string, d time.Duration, sha string) gitmock.M {
		return gitmock.M{"number": float64(n), "title": title, "body": "", "state": state, "draft": false, "html_url": fmt.Sprintf("https://github.com/acme/shop/pull/%d", n), "user": gitmock.M{"login": "octo"},
			"head": gitmock.M{"ref": "f" + fmt.Sprint(n), "sha": sha}, "base": gitmock.M{"ref": "main"}, "updated_at": upd(d)}
	}
	api.PRs = []gitmock.M{pr(3, is.Key+" newest", "open", time.Hour, "aaaa0003"), pr(2, is.Key+" older", "closed", 24*time.Hour, "aaaa0002"), pr(1, "ancient "+is.Key, "closed", 200*24*time.Hour, "aaaa0001")}
	api.Branches = []gitmock.M{{"name": is.Key + "-branch", "commit": gitmock.M{"sha": "bbbb0001"}}, {"name": "main", "commit": gitmock.M{"sha": "bbbb0002"}}}
	api.Commits = []gitmock.M{{"sha": "cccc0001", "html_url": "https://github.com/acme/shop/commit/cccc0001", "commit": gitmock.M{"message": is.Key + " backfilled commit\n\nbody", "author": gitmock.M{"name": "Octo", "email": "o@example.com", "date": upd(2 * time.Hour)}}}}
	api.Checks = []gitmock.M{{"id": float64(1), "name": "ci", "head_sha": "aaaa0003", "status": "completed", "conclusion": "success", "html_url": "https://x", "completed_at": upd(30 * time.Minute)}}
	n, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := dev(t, e, e.Alice, is.Key)
	if len(d.PRs) != 2 || len(d.Branches) != 1 || len(d.Commits) != 1 || d.Build != "passing" { // the ancient PR is older than 90 days: not read
		t.Fatalf("%d events %+v", n, d)
	}
	for _, rq := range api.Requests() {
		if !strings.HasPrefix(rq, "GET ") {
			t.Fatalf("a non-read request was sent: %s", rq)
		}
	}
	// the second sync sends the stored validator and a 304 changes nothing; the panel is unchanged
	api.SetETag(`"v1"`)
	before := len(api.Requests())
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	var cond int
	for _, rq := range api.Requests()[before:] {
		if strings.Contains(rq, "/pulls") {
			cond++
		}
	}
	if cond != 1 { // one conditional request for the first page, no further pages
		t.Fatalf("%d pull request calls on the second sync: %v", cond, api.Requests()[before:])
	}
	if d2 := dev(t, e, e.Alice, is.Key); len(d2.PRs) != 2 {
		t.Fatalf("%+v", d2)
	}
	// a primary rate limit that resets at once is waited out; the sync completes
	api.SetETag(`"v2"`)
	api.RateLimit(1, 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(e.Clock.Now().Add(-time.Minute).Unix())})
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatalf("a rate limit that has already reset stopped the sync: %v", err)
	}
	// a limit that lasts an hour is recorded, not slept through, and the previous data stays
	api.RateLimit(1, 429, map[string]string{"Retry-After": "3600"})
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("%v", err)
	}
	repos, _ := e.Svc.ListGitRepos(ctx, e.Admin)
	if !strings.Contains(repos[0].SyncError, "rate limit") || repos[0].LastSync == nil {
		t.Fatalf("%+v", repos[0])
	}
	if d3 := dev(t, e, e.Alice, is.Key); len(d3.PRs) != 2 {
		t.Fatal("a failed sync removed data")
	}
	// the next good sync clears the error
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	if repos, _ = e.Svc.ListGitRepos(ctx, e.Admin); repos[0].SyncError != "" {
		t.Fatalf("%+v", repos[0])
	}
	// a wrong token is a clear failure, never a partial import
	e.Store.Pool.Exec(ctx, `UPDATE git_connection SET token_enc=$1`, mustSeal(t, e, "wrong"))
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("%v", err)
	}
}

func mustSeal(t *testing.T, e *testutil.Env, v string) []byte {
	b, err := e.Svc.SealForTest(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGIT_BackfillGitLab(t *testing.T) {
	r, api := backfillRig(t, "gitlab", true)
	e := r.e
	is := e.Issue("gl", "")
	ts := func(d time.Duration) string { return e.Svc.Now().Add(-d).UTC().Format(time.RFC3339) }
	api.PRs = []gitmock.M{{"iid": float64(4), "title": is.Key + " mr", "description": "", "state": "opened", "draft": false, "web_url": "https://gitlab.example/acme/shop/-/merge_requests/4",
		"author": gitmock.M{"username": "tanuki"}, "sha": "dddd0004", "source_branch": "s", "target_branch": "main", "updated_at": ts(time.Hour)}}
	api.Branches = []gitmock.M{{"name": "feature/" + is.Key, "commit": gitmock.M{"id": "dddd0004"}}}
	api.Commits = []gitmock.M{{"id": "dddd0004", "message": is.Key + " change", "author_name": "Tan", "author_email": "t@example.com", "web_url": "https://gitlab.example/c/dddd0004", "created_at": ts(time.Hour)}}
	api.Pipelines = []gitmock.M{{"id": float64(77), "sha": "dddd0004", "ref": "s", "status": "failed", "web_url": "https://gitlab.example/p/77", "updated_at": ts(30 * time.Minute)}}
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	d := dev(t, e, e.Alice, is.Key)
	if len(d.PRs) != 1 || d.PRs[0].State != "open" || len(d.Branches) != 1 || d.Build != "failing" {
		t.Fatalf("%+v", d)
	}
	for _, rq := range api.Requests() {
		if !strings.Contains(rq, "/api/v4/projects/acme%2Fshop/") {
			t.Fatalf("the project path was not encoded: %s", rq)
		}
	}
}

func TestGIT_EgressPolicyBlocksPrivateHostsUnlessAllowed(t *testing.T) {
	r, api := backfillRig(t, "github", false)
	_ = api
	_, err := r.e.Svc.SyncGitRepo(ctx, r.e.Admin, r.repo.ID)
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("a loopback provider was reachable without allow_private: %v", err)
	}
	if api.Hits != 0 {
		t.Fatalf("%d requests reached a blocked host", api.Hits)
	}
	// plain http is only for private hosts the connection allows
	if _, _, err := r.e.Svc.CreateGitConnection(ctx, r.e.Admin, domain.GitConnIn{Name: "plain", Provider: "github", BaseURL: "http://example.com"}); err == nil {
		t.Fatal("plain http to a public host was accepted")
	}
}
