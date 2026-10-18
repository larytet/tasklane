package domain_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
)

// autoRig is a GitHub repository mapped to ABC, with the software workflow (To Do, In Progress, In Review, Done).
func newCodeRig(t *testing.T) *gitRig {
	r := newGit(t, "github")
	wf, err := r.e.Svc.CreateWorkflow(ctx, r.e.Admin, "Software flow", "software", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.e.Svc.SetProjectWorkflow(ctx, r.e.Admin, "ABC", wf.ID); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *gitRig) status(key string) string {
	var st string
	r.e.Store.Pool.QueryRow(ctx, `SELECT st.name FROM issue i JOIN status st ON st.id=i.status_id WHERE i.key=$1`, key).Scan(&st)
	return st
}

func (r *gitRig) rule(t *testing.T, in domain.GitRule) domain.GitRule {
	t.Helper()
	v, err := r.e.Svc.SaveGitRule(ctx, r.e.Admin, "ABC", in)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (r *gitRig) runs(t *testing.T) []domain.GitRuleRun {
	v, err := r.e.Svc.GitRuleRuns(ctx, r.e.Admin, "ABC", 100)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (r *gitRig) actors(key string) []string {
	h, _ := r.e.Svc.History(ctx, r.e.Admin, key)
	var out []string
	for _, x := range h {
		if x.Field == "status" {
			out = append(out, x.Actor)
		}
	}
	return out
}

var deliveryN int

func (r *gitRig) push(t *testing.T, branch, sha string, commits ...gitmock.M) {
	t.Helper()
	deliveryN++
	r.deliver(t, gitmock.GitHubHook(r.secret, "push", fmt.Sprint("push", deliveryN), gitmock.GHPush("acme/shop", branch, sha, commits, t0)))
}

func (r *gitRig) pr(t *testing.T, n int, title, body, state string, merged bool, at time.Time) {
	t.Helper()
	deliveryN++
	r.deliver(t, gitmock.GitHubHook(r.secret, "pull_request", fmt.Sprint("pr", deliveryN), gitmock.GHPR("acme/shop", n, title, body, "feature-"+fmt.Sprint(n), fmt.Sprintf("%08d", n), state, merged, at)))
}

func TestGIT08_RulesMoveIssuesFromCodeEventsAndSayWhoDidIt(t *testing.T) {
	r := newCodeRig(t)
	e := r.e
	is := e.Issue("the checkout", "")
	r.rule(t, domain.GitRule{Name: "branch starts work", Trigger: "branch_created", Transition: "Start progress", Mode: "on"})
	r.rule(t, domain.GitRule{Name: "pr asks for review", Trigger: "pr_opened", Transition: "Request review", Mode: "on"})
	r.rule(t, domain.GitRule{Name: "merge finishes", Trigger: "pr_merged", Transition: "Approve", Comment: "Merged, thank you.", Mode: "on"})
	r.rule(t, domain.GitRule{Name: "red build", Trigger: "build_failed", Comment: "The build is failing.", Mode: "on"})

	r.push(t, is.Key+"-fix", "b1b1b1b1")
	if r.status(is.Key) != "In Progress" {
		t.Fatalf("a new branch left the issue in %q", r.status(is.Key))
	}
	r.pr(t, 12, is.Key+" fix the checkout", "", "open", false, t0)
	if r.status(is.Key) != "In Review" {
		t.Fatalf("an opened pull request left the issue in %q", r.status(is.Key))
	}
	// a failing build on the pull request's head commit is reported once, on the issue
	deliveryN++
	r.deliver(t, gitmock.GitHubHook(r.secret, "check_run", fmt.Sprint("chk", deliveryN), gitmock.GHCheck("acme/shop", "ci", "00000012", "completed", "failure", t0.Add(time.Minute))))
	r.pr(t, 12, is.Key+" fix the checkout", "", "closed", true, t0.Add(2*time.Minute))
	if r.status(is.Key) != "Done" {
		t.Fatalf("a merged pull request left the issue in %q", r.status(is.Key))
	}
	// every move says what caused it, and the comments are there once each
	want := []string{"GitHub, branch " + is.Key + "-fix", "GitHub, PR #12", "GitHub, PR #12"}
	if got := r.actors(is.Key); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q, want %q", got, want)
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	var bodies []string
	for _, c := range v.Comments {
		bodies = append(bodies, strings.SplitN(c.Body, "\n", 2)[0])
	}
	if strings.Join(bodies, "|") != "The build is failing.|Merged, thank you." {
		t.Fatalf("%q", bodies)
	}
	// the panel shows what the rules did
	d := dev(t, e, e.Alice, is.Key)
	applied := 0
	for _, a := range d.Automation {
		if a.Source == "rule" && a.Outcome == "applied" {
			applied++
		}
	}
	if applied != 4 {
		t.Fatalf("%d applied runs in the panel: %+v", applied, d.Automation)
	}
}

func TestGIT08_ADisallowedMoveIsSkippedAndReportedNeverForced(t *testing.T) {
	r := newCodeRig(t)
	e := r.e
	is := e.Issue("not started", "")
	r.rule(t, domain.GitRule{Name: "merge finishes", Trigger: "pr_merged", Transition: "Approve", Mode: "on"})
	// the issue is in To Do: "Approve" belongs to In Review, so the merge cannot finish it
	r.pr(t, 3, is.Key+" early merge", "", "closed", true, t0)
	if r.status(is.Key) != "To Do" {
		t.Fatalf("the workflow was forced: %q", r.status(is.Key))
	}
	runs := r.runs(t)
	if len(runs) != 1 || runs[0].Outcome != "skipped" || !strings.Contains(runs[0].Detail, `"Approve" is not available from "To Do"`) {
		t.Fatalf("%+v", runs)
	}
	// a rule is refused when the workflow has no such transition, and a rule with nothing to do or a bad trigger
	for name, in := range map[string]domain.GitRule{
		"unknown transition": {Name: "x", Trigger: "pr_merged", Transition: "Teleport", Mode: "on"},
		"bad trigger":        {Name: "x", Trigger: "tag_pushed", Transition: "Approve", Mode: "on"},
		"nothing to do":      {Name: "x", Trigger: "pr_merged", Mode: "on"},
		"bad mode":           {Name: "x", Trigger: "pr_merged", Transition: "Approve", Mode: "always"},
		"no name":            {Trigger: "pr_merged", Transition: "Approve", Mode: "on"},
	} {
		if _, err := e.Svc.SaveGitRule(ctx, e.Admin, "ABC", in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// only someone who administers the project manages its rules
	if _, err := e.Svc.SaveGitRule(ctx, e.Alice, "ABC", domain.GitRule{Name: "x", Trigger: "pr_merged", Transition: "Approve"}); err == nil {
		t.Fatal("a member saved a rule")
	}
	if _, err := e.Svc.ListGitRules(ctx, e.Alice, "ABC"); err == nil {
		t.Fatal("a member listed rules")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='git.rule.save'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d audit rows", n)
	}
}

func TestGIT08_ARuleRunsOncePerCodeObjectAndIssueSoNothingLoops(t *testing.T) {
	r := newCodeRig(t)
	e := r.e
	is := e.Issue("looping", "")
	r.rule(t, domain.GitRule{Name: "branch starts work", Trigger: "branch_created", Transition: "Start progress", Mode: "on"})
	r.rule(t, domain.GitRule{Name: "pr asks for review", Trigger: "pr_opened", Transition: "Request review", Comment: "Review please.", Mode: "on"})
	r.push(t, is.Key+"-x", "a1a1a1a1")
	r.pr(t, 5, is.Key+" x", "", "open", false, t0)
	// the same deliveries again, the pull request closed and reopened, a draft made ready, an edit: no rule runs a second time
	r.push(t, is.Key+"-x", "a1a1a1a1")
	r.pr(t, 5, is.Key+" x", "", "open", false, t0)
	r.pr(t, 5, is.Key+" x", "", "closed", false, t0.Add(time.Minute))
	r.pr(t, 5, is.Key+" x", "", "open", false, t0.Add(2*time.Minute))
	r.pr(t, 5, is.Key+" x (edited)", "", "draft", false, t0.Add(3*time.Minute))
	r.pr(t, 5, is.Key+" x (edited)", "", "open", false, t0.Add(4*time.Minute))
	// someone moves the issue back by hand: another event does not undo that
	back, ok := trans(t, e, e.Alice, is.Key, "Changes needed")
	if !ok {
		t.Fatal("no Changes needed transition")
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, is.Key, back, nil); err != nil {
		t.Fatal(err)
	}
	r.pr(t, 5, is.Key+" x (edited again)", "", "open", false, t0.Add(5*time.Minute))
	if r.status(is.Key) != "In Progress" {
		t.Fatalf("a replayed event re-ran a rule: %q", r.status(is.Key))
	}
	if n := len(r.runs(t)); n != 2 {
		t.Fatalf("%d runs for two rules on one branch and one pull request", n)
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if len(v.Comments) != 1 {
		t.Fatalf("%d comments", len(v.Comments))
	}
}

func TestGIT08_DryRunOffAndBackfillChangeNothing(t *testing.T) {
	r := newCodeRig(t)
	e := r.e
	is := e.Issue("careful", "")
	// a new rule starts in dry run: it says what it would do and does nothing
	dry := r.rule(t, domain.GitRule{Name: "branch starts work", Trigger: "branch_created", Transition: "Start progress", Comment: "Started."})
	if dry.Mode != "dry_run" {
		t.Fatalf("%q", dry.Mode)
	}
	r.rule(t, domain.GitRule{Name: "disabled", Trigger: "branch_created", Transition: "Start progress", Mode: "off"})
	r.push(t, is.Key+"-a", "c1c1c1c1")
	if r.status(is.Key) != "To Do" {
		t.Fatalf("a dry run moved the issue to %q", r.status(is.Key))
	}
	runs := r.runs(t)
	if len(runs) != 1 || runs[0].Outcome != "dry_run" || !strings.Contains(runs[0].Detail, `would move `+is.Key+` from "To Do" to "In Progress"`) || !strings.Contains(runs[0].Detail, "comment on") || !strings.Contains(runs[0].Detail, "GitHub, branch") {
		t.Fatalf("%+v", runs)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, is.Key); len(v.Comments) != 0 {
		t.Fatal("a dry run commented")
	}
	// switched on, it acts on the next branch, not on the one that was already seen
	dry.Mode = "on"
	if _, err := e.Svc.SaveGitRule(ctx, e.Admin, "ABC", dry); err != nil {
		t.Fatal(err)
	}
	two := e.Issue("second", "")
	r.push(t, two.Key+"-b", "c2c2c2c2")
	if r.status(two.Key) != "In Progress" {
		t.Fatalf("%q", r.status(two.Key))
	}
	if err := e.Svc.DeleteGitRule(ctx, e.Admin, "ABC", dry.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.DeleteGitRule(ctx, e.Admin, "ABC", dry.ID); err == nil {
		t.Fatal("deleted twice")
	}
}

func TestGIT08_ReplayingHistoryNeverRunsRules(t *testing.T) {
	r, api := backfillRig(t, "github", true)
	e := r.e
	wf, _ := e.Svc.CreateWorkflow(ctx, e.Admin, "Software flow", "software", nil)
	e.Svc.SetProjectWorkflow(ctx, e.Admin, "ABC", wf.ID)
	is := e.Issue("old work", "")
	if _, err := e.Svc.SaveGitRule(ctx, e.Admin, "ABC", domain.GitRule{Name: "any branch", Trigger: "branch_created", Transition: "Start progress", Mode: "on"}); err != nil {
		t.Fatal(err)
	}
	upd := func(d time.Duration) string { return e.Svc.Now().Add(-d).UTC().Format(time.RFC3339) }
	api.Branches = []gitmock.M{{"name": is.Key + "-old", "commit": gitmock.M{"sha": "bbbb0001"}}}
	api.PRs = []gitmock.M{{"number": float64(9), "title": is.Key + " long ago", "body": "", "state": "closed", "merged": true, "html_url": "https://x/9", "user": gitmock.M{"login": "octo"},
		"head": gitmock.M{"ref": "old", "sha": "aaaa0009"}, "base": gitmock.M{"ref": "main"}, "updated_at": upd(48 * time.Hour)}}
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	if d := dev(t, e, e.Alice, is.Key); len(d.Branches) != 1 || len(d.PRs) != 1 {
		t.Fatalf("the backfill did not link: %+v", d)
	}
	var st string
	e.Store.Pool.QueryRow(ctx, `SELECT st.name FROM issue i JOIN status st ON st.id=i.status_id WHERE i.key=$1`, is.Key).Scan(&st)
	var runs int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM git_rule_run`).Scan(&runs)
	if st != "To Do" || runs != 0 {
		t.Fatalf("a backfill ran a rule: %q, %d runs", st, runs)
	}
}

// ---------- smart commits ----------

func (r *gitRig) smart(t *testing.T, mode string) {
	t.Helper()
	if err := r.e.Svc.SetSmartCommits(ctx, r.e.Admin, "ABC", mode); err != nil {
		t.Fatal(err)
	}
}

func (r *gitRig) link(t *testing.T, login string, u domain.Actor) {
	t.Helper()
	if _, err := r.e.Svc.LinkGitIdentity(ctx, r.e.Admin, "github", login, u.ID); err != nil {
		t.Fatal(err)
	}
}

func (r *gitRig) smartRuns(key string) (out []string) {
	rows, _ := r.e.Store.Pool.Query(ctx, `SELECT command||' '||outcome||' '||reason FROM git_smart_run WHERE issue_key=$1 ORDER BY idx`, key)
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return
}

func (r *gitRig) comments(key string) []domain.Comment {
	v, _ := r.e.Svc.View(ctx, r.e.Alice, key)
	return v.Comments
}

func TestGIT09_SmartCommitsAreOffByDefaultAndRunAsTheMappedCommitter(t *testing.T) {
	r := newCodeRig(t)
	e := r.e
	is := e.Issue("redirect", "")
	start, _ := trans(t, e, e.Alice, is.Key, "Start progress")
	e.Svc.Transition(ctx, e.Alice, is.Key, start, nil)
	msg := is.Key + " #comment fixed the redirect #request-review"
	r.link(t, "octo", e.Alice)
	// disabled by default: the command is just text
	r.push(t, "main", "d1d1d1d1", gitmock.GHCommitBy("d1d1d1d1", msg, "octo", "octo@example.com", t0))
	if len(r.comments(is.Key)) != 0 || r.status(is.Key) != "In Progress" || len(r.smartRuns(is.Key)) != 0 {
		t.Fatal("a command ran with smart commits off")
	}
	r.smart(t, "default_branch")
	r.push(t, "main", "d2d2d2d2", gitmock.GHCommitBy("d2d2d2d2", msg, "octo", "octo@example.com", t0.Add(time.Minute)))
	cs := r.comments(is.Key)
	if len(cs) != 1 || cs[0].Author.ID != e.Alice.ID || !strings.HasPrefix(cs[0].Body, "fixed the redirect") {
		t.Fatalf("%+v", cs)
	}
	if r.status(is.Key) != "In Review" {
		t.Fatalf("%q", r.status(is.Key))
	}
	// the move is attributed to the person, and to the commit's source
	if got := r.actors(is.Key); len(got) != 2 || !strings.HasPrefix(got[1], "Alice K.") || !strings.Contains(got[1], "GitHub") {
		t.Fatalf("%q", got)
	}
	if got := r.smartRuns(is.Key); len(got) != 2 || !strings.HasPrefix(got[0], "#comment applied") || !strings.HasPrefix(got[1], "#request-review applied") {
		t.Fatalf("%q", got)
	}
	var audits int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='git.smart_commit'`).Scan(&audits)
	if audits != 2 {
		t.Fatalf("%d audit rows: every command is audited", audits)
	}
	// the same commit delivered again (or reconciled): nothing runs twice
	r.push(t, "main", "d2d2d2d2", gitmock.GHCommitBy("d2d2d2d2", msg, "octo", "octo@example.com", t0.Add(time.Minute)))
	if len(r.comments(is.Key)) != 1 || len(r.smartRuns(is.Key)) != 2 {
		t.Fatal("a replay ran a command again")
	}
	// the panel says what happened
	d := dev(t, e, e.Alice, is.Key)
	smart := 0
	for _, a := range d.Automation {
		if a.Source == "smart_commit" && a.Outcome == "applied" {
			smart++
		}
	}
	if smart != 2 {
		t.Fatalf("%+v", d.Automation)
	}
}

func TestGIT09_SmartCommitsIgnoreAndReportWhatTheyMustNotRun(t *testing.T) {
	r := newCodeRig(t)
	e := r.e
	is := e.Issue("careful", "")
	start, _ := trans(t, e, e.Alice, is.Key, "Start progress")
	e.Svc.Transition(ctx, e.Alice, is.Key, start, nil)
	r.smart(t, "default_branch")
	r.link(t, "octo", e.Alice)
	commit := func(sha, branch, login, email, msg string) {
		r.push(t, branch, sha, gitmock.GHCommitBy(sha, msg, login, email, t0))
	}
	last := func() string {
		got := r.smartRuns(is.Key)
		if len(got) == 0 {
			return ""
		}
		return got[len(got)-1]
	}
	assertIgnored := func(what, want string) {
		t.Helper()
		if l := last(); !strings.Contains(l, " ignored ") || !strings.Contains(l, want) {
			t.Errorf("%s: %q does not say ignored and %q", what, l, want)
		}
		if len(r.comments(is.Key)) != 0 || r.status(is.Key) != "In Progress" {
			t.Fatalf("%s: something ran (%d comments, %q)", what, len(r.comments(is.Key)), r.status(is.Key))
		}
	}
	// an author nobody linked: shown by the provider's name, the command is reported and not run
	commit("e1e1e1e1", "main", "stranger", "stranger@example.org", is.Key+" #comment hello")
	assertIgnored("unlinked author", "not linked to a Tasklane user (stranger)")
	// an email that is Alice's, written by someone without a provider account the provider vouches for: that is not Alice
	commit("e2e2e2e2", "main", "", "alice@example.com", is.Key+" #comment as alice")
	assertIgnored("an unverified email", "no provider account asserted")
	// the default branch only
	commit("e3e3e3e3", "feature", "octo", "octo@example.com", is.Key+" #comment on a branch")
	assertIgnored("a feature branch", "only on the default branch (main)")
	// unknown and unsupported commands, and a comment without text
	commit("e4e4e4e4", "main", "octo", "octo@example.com", is.Key+" #frobnicate now")
	assertIgnored("an unknown command", "unknown command #frobnicate")
	commit("e5e5e5e5", "main", "octo", "octo@example.com", is.Key+" #time 2h")
	assertIgnored("time", "time tracking is not built yet")
	commit("e6e6e6e6", "main", "octo", "octo@example.com", is.Key+" #comment")
	assertIgnored("an empty comment", "needs some text")
	// a transition the workflow does not offer from here
	commit("e7e7e7e7", "main", "octo", "octo@example.com", is.Key+" #approve")
	assertIgnored("a transition that does not apply", "no transition of that name")
	// commands in a pull request description are never run, by anyone; the title's run when it merges
	r.pr(t, 21, is.Key+" tidy up", is.Key+" #comment from the description", "closed", true, t0)
	assertIgnored("a pull request description", "commands in a pull request description are never run")
	// a person who may not comment (a viewer) is not given the power by writing a commit
	viewer, _ := e.Svc.CreateUser(ctx, "vince@example.com", "Vince", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	r.link(t, "vince-gh", domain.Actor{ID: viewer.ID})
	commit("e8e8e8e8", "main", "vince-gh", "vince@example.com", is.Key+" #comment let me in")
	assertIgnored("a viewer", "not permitted")
	// several keys and several commands in one line apply to each
	two := e.Issue("second", "")
	commit("e9e9e9e9", "main", "octo", "octo@example.com", is.Key+" "+two.Key+" #comment both #back-to-to-do")
	if len(r.comments(is.Key)) != 1 || len(r.comments(two.Key)) != 1 || r.status(is.Key) != "To Do" {
		t.Fatalf("%d %d %q", len(r.comments(is.Key)), len(r.comments(two.Key)), r.status(is.Key))
	}
	// every ignored command was audited too
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='git.smart_commit' AND detail->>'outcome'='ignored'`).Scan(&n)
	if n < 8 {
		t.Fatalf("%d ignored commands audited", n)
	}
	// "any" runs on a feature branch, as the person
	r.smart(t, "any")
	commit("eaeaeaea", "feature", "octo", "octo@example.com", two.Key+" #comment from a branch")
	if len(r.comments(two.Key)) != 2 {
		t.Fatalf("%d comments", len(r.comments(two.Key)))
	}
	// a merged pull request's title runs as the pull request's author
	r.pr(t, 22, two.Key+" #start-progress", "", "closed", true, t0.Add(time.Hour))
	if r.status(two.Key) != "In Progress" {
		t.Fatalf("%q", r.status(two.Key))
	}
	if got := r.actors(two.Key); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "Alice K.") {
		t.Fatalf("%q", got)
	}
}

func TestGIT09_ReplayingHistoryNeverRunsCommands(t *testing.T) {
	r, api := backfillRig(t, "github", true)
	e := r.e
	is := e.Issue("old", "")
	e.Svc.SetSmartCommits(ctx, e.Admin, "ABC", "any")
	e.Svc.LinkGitIdentity(ctx, e.Admin, "github", "octo", e.Alice.ID)
	api.Commits = []gitmock.M{{"sha": "cccc0001", "html_url": "https://x/c", "commit": gitmock.M{"message": is.Key + " #comment from the past", "author": gitmock.M{"name": "Octo", "email": "alice@example.com", "date": e.Svc.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)}}}}
	if _, err := e.Svc.SyncGitRepo(ctx, e.Admin, r.repo.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, is.Key); len(v.Comments) != 0 {
		t.Fatal("a reconcile ran a command")
	}
}

func TestGIT09_10_GitLabPushesAreAttributedToThePusherOnlyWhenTheCommitIsTheirs(t *testing.T) {
	r := newGit(t, "gitlab")
	e := r.e
	wf, _ := e.Svc.CreateWorkflow(ctx, e.Admin, "Software flow", "software", nil)
	e.Svc.SetProjectWorkflow(ctx, e.Admin, "ABC", wf.ID)
	is := e.Issue("gitlab work", "")
	e.Svc.SetSmartCommits(ctx, e.Admin, "ABC", "default_branch")
	if _, err := e.Svc.LinkGitIdentity(ctx, e.Admin, "gitlab", "tanuki", e.Alice.ID); err != nil {
		t.Fatal(err)
	}
	push := func(n int, pusher, pusherEmail, authorEmail, msg string) {
		deliveryN++
		r.deliver(t, gitmock.GitLabHook(r.secret, "Push Hook", fmt.Sprint("gl", deliveryN), gitmock.GLPush("acme/shop", "main", fmt.Sprintf("%08d", n), pusher, pusherEmail,
			[]gitmock.M{gitmock.GLCommit(fmt.Sprintf("%08d", n), msg, "Some Body", authorEmail, t0)}, t0)))
	}
	// someone else's name on a commit pushed by tanuki: the commit email is not the pusher's, so it is not attributed to tanuki
	push(1, "tanuki", "tanuki@example.com", "someone.else@example.com", is.Key+" #comment forged author")
	if len(r.comments(is.Key)) != 0 {
		t.Fatal("a commit with another person's email ran as the pusher")
	}
	// the pusher's own commit runs as the person their account is linked to
	push(2, "tanuki", "tanuki@example.com", "tanuki@example.com", is.Key+" #comment from gitlab")
	cs := r.comments(is.Key)
	if len(cs) != 1 || cs[0].Author.ID != e.Alice.ID {
		t.Fatalf("%+v", cs)
	}
	var author *string
	e.Store.Pool.QueryRow(ctx, `SELECT author_user::text FROM git_object WHERE ext_id='00000001'`).Scan(&author)
	if author != nil {
		t.Fatal("the forged commit was attributed to a person")
	}
	e.Store.Pool.QueryRow(ctx, `SELECT author_user::text FROM git_object WHERE ext_id='00000002'`).Scan(&author)
	if author == nil || *author != e.Alice.ID {
		t.Fatalf("%v", author)
	}
}

func TestGIT10_IdentitiesAreLinkedByAnAdministratorAndMapOnlyWhatTheProviderVouchesFor(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	is := e.Issue("who did it", "")
	// a commit whose email is Bob's but which the provider does not tie to an account: shown by name, attributed to nobody
	r.push(t, "main", "f1f1f1f1", gitmock.GHCommitBy("f1f1f1f1", is.Key+" unverified", "", "bob@example.com", t0))
	// the provider says this account owns the email (GitHub fills in the username only then): attributed through the verified email
	r.push(t, "main", "f2f2f2f2", gitmock.GHCommitBy("f2f2f2f2", is.Key+" verified", "bobby", "bob@example.com", t0.Add(time.Minute)))
	// an account an administrator linked wins, whatever email the commit carries
	if _, err := e.Svc.LinkGitIdentity(ctx, e.Admin, "github", "Carol-GH", e.Alice.ID); err != nil {
		t.Fatal(err)
	}
	r.push(t, "main", "f3f3f3f3", gitmock.GHCommitBy("f3f3f3f3", is.Key+" linked", "carol-gh", "carol@elsewhere.example", t0.Add(2*time.Minute)))
	who := func(sha string) (author string, user *string) {
		e.Store.Pool.QueryRow(ctx, `SELECT author,author_user::text FROM git_object WHERE ext_id=$1`, sha).Scan(&author, &user)
		return
	}
	if a, u := who("f1f1f1f1"); u != nil || a != "Some Body" {
		t.Fatalf("an unverified email was attributed: %q %v", a, u)
	}
	if a, u := who("f2f2f2f2"); u == nil || *u != e.Bob.ID || a != "bobby" {
		t.Fatalf("%q %v", a, u)
	}
	if a, u := who("f3f3f3f3"); u == nil || *u != e.Alice.ID || a != "carol-gh" {
		t.Fatalf("%q %v", a, u)
	}
	// linking is an administrator's act, audited, and one account is one person
	if _, err := e.Svc.LinkGitIdentity(ctx, e.Alice, "github", "mine", e.Alice.ID); err == nil {
		t.Fatal("a member linked an account")
	}
	if _, err := e.Svc.LinkGitIdentity(ctx, e.Admin, "github", "carol-gh", e.Bob.ID); err == nil {
		t.Fatal("an account was linked to two people")
	}
	for name, in := range map[string][3]string{"provider": {"bitbucket", "x", e.Bob.ID}, "login": {"github", "no spaces allowed", e.Bob.ID}, "person": {"github", "ok", "00000000-0000-0000-0000-000000000000"}} {
		if _, err := e.Svc.LinkGitIdentity(ctx, e.Admin, in[0], in[1], in[2]); err == nil {
			t.Errorf("a bad %s was accepted", name)
		}
	}
	l, _ := e.Svc.ListGitIdentities(ctx, e.Admin)
	if len(l) != 1 || l[0].Login != "carol-gh" {
		t.Fatalf("%+v", l)
	}
	if err := e.Svc.UnlinkGitIdentity(ctx, e.Admin, l[0].ID); err != nil {
		t.Fatal(err)
	}
	r.push(t, "main", "f4f4f4f4", gitmock.GHCommitBy("f4f4f4f4", is.Key+" after unlink", "carol-gh", "carol@elsewhere.example", t0.Add(3*time.Minute)))
	if _, u := who("f4f4f4f4"); u != nil {
		t.Fatal("an unlinked account is still attributed")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action LIKE 'git.identity.%'`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d audit rows", n)
	}
}

// GIT-03 regression: keys separated by a single space both link (the matcher used to take the separator with the first key), and a
// key inside a longer word or number still does not.
func TestGIT03_KeysSeparatedByOneSpaceAllLinkAndLongerWordsStillDoNot(t *testing.T) {
	r := newGit(t, "github")
	e := r.e
	one, two := e.Issue("one", ""), e.Issue("two", "")
	r.pr(t, 31, one.Key+" "+two.Key+" both together", "", "open", false, t0)
	for _, is := range []domain.Issue{one, two} {
		if d := dev(t, e, e.Alice, is.Key); len(d.PRs) != 1 {
			t.Fatalf("%s: %d pull requests linked", is.Key, len(d.PRs))
		}
	}
	three := e.Issue("three", "")
	r.pr(t, 32, "x"+three.Key+" and "+three.Key+"7 and "+three.Key+"x", "", "open", false, t0)
	if d := dev(t, e, e.Alice, three.Key); len(d.PRs) != 0 {
		t.Fatalf("a key inside a longer word or number linked: %d", len(d.PRs))
	}
	r.pr(t, 33, "see ("+three.Key+"), "+one.Key+".", "", "open", false, t0)
	if d := dev(t, e, e.Alice, three.Key); len(d.PRs) != 1 {
		t.Fatal("a key in punctuation did not link")
	}
}
