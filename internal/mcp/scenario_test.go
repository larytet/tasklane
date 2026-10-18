package mcp_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
)

// MCP-19: a scripted agent does a whole piece of work through the tools, the way a deployed one would: it picks the next ready issue,
// claims it, plans, splits it, starts a branch, sees the pull request that arrives, records its test results, sends the work to
// review, is stopped from closing it itself, asks a person, and the person approves. Afterwards everything the people and the
// system would look at is right: the history says who did what, the owner was told, every call is in the log, the lease is gone, and
// the agent stayed within a small budget of calls and characters.
func TestMCP19_AScriptedAgentDoesAPieceOfWorkFromPickToApproval(t *testing.T) {
	e := newEnv(t)
	git := gitmock.New("github", "tok-mcp")
	git.AllowWrites = true
	t.Cleanup(git.Close)
	e.Svc.BaseURL = "https://tasklane.example.com"
	conn, secret, err := e.Svc.CreateGitConnection(ctx, e.Admin, domain.GitConnIn{Name: "gh", Provider: "github", BaseURL: git.URL, Token: "tok-mcp", AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.MapGitRepo(ctx, e.Admin, conn.ID, "acme/shop", []string{"ABC"}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if err := e.Svc.SetGitConnectionOptions(ctx, e.Admin, conn.ID, domain.GitConnOptions{WriteEnabled: &yes}); err != nil {
		t.Fatal(err)
	}
	// the workflow: In Progress -> In Review (anyone) -> Done (a person only); closing straight from In Progress is a person's too
	wf := ""
	e.Store.Pool.QueryRow(ctx, `SELECT workflow_id::text FROM project WHERE key='ABC'`).Scan(&wf)
	var inProg, done, review string
	e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM status WHERE workflow_id::text=$1 AND name='In Progress'`, wf).Scan(&inProg)
	e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM status WHERE workflow_id::text=$1 AND name='Done'`, wf).Scan(&done)
	if err := e.Store.Pool.QueryRow(ctx, `INSERT INTO status(workflow_id,name,category,position) VALUES($1,'In Review','in_progress',3) RETURNING id::text`, wf).Scan(&review); err != nil {
		t.Fatal(err)
	}
	e.Store.Pool.Exec(ctx, `INSERT INTO transition(workflow_id,name,from_status,to_status) VALUES($1,'Send to review',$2,$3),($1,'Approve',$3,$4)`, wf, inProg, review, done)
	var gate []string
	rows, _ := e.Store.Pool.Query(ctx, `SELECT id::text FROM transition WHERE workflow_id::text=$1 AND to_status::text=$2`, wf, done)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		gate = append(gate, id)
	}
	rows.Close()
	for _, id := range gate {
		if err := e.Svc.SetTransitionHuman(ctx, e.Admin, id, true); err != nil {
			t.Fatal(err)
		}
	}

	// the people: Alice files the work and labels what the agent may take; Bob files something that is not ready
	ready, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Story", Summary: "Add a retry button to the payment screen", Description: "When the card needs a challenge and the person closes the dialog, show a Retry button.", Labels: []string{"agent-ready"}})
	if err != nil {
		t.Fatal(err)
	}
	notReady, err := e.Svc.CreateIssue(ctx, e.Bob, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "Not ready yet", Labels: []string{"needs-spec"}})
	if err != nil {
		t.Fatal(err)
	}
	key := ready.Key
	ag := e.newAgent("builder", "member", []string{"read", "claim", "comment", "create", "transition", "branch"})
	ag.init()
	calls, chars := 0, 0
	step := func(tool string, args map[string]any) result {
		t.Helper()
		calls++
		r := ag.call(tool, args)
		chars += len(r.Text)
		return r
	}
	must := func(tool string, args map[string]any) result {
		t.Helper()
		r := step(tool, args)
		if r.Err {
			t.Fatalf("%s: %s", tool, r.Text)
		}
		return r
	}

	// 1. pick from "Agent ready"
	pick := must("search_issues", map[string]any{"q": `project = ABC AND labels = agent-ready AND status = "To Do" ORDER BY created ASC`})
	rowsOut := pick.Data["issues"].([]any)
	if len(rowsOut) != 1 || rowsOut[0].(map[string]any)["key"] != key {
		t.Fatalf("the agent's queue holds %v, want only %s", rowsOut, key)
	}
	// 2. claim it: assigned to the agent, moved to In Progress, with a lease
	claim := must("claim_issue", map[string]any{"key": key})
	if !strings.Contains(claim.Text, "In Progress") && claim.Data["status"] != "In Progress" {
		t.Logf("claim: %s", claim.Text)
	}
	ctxr := must("get_issue_context", map[string]any{"key": key})
	if ctxr.Data["status"] != "In Progress" || ctxr.Data["assignee"] != "builder" || ctxr.Data["lease"] == nil {
		t.Fatalf("after claiming: %v", ctxr.Data)
	}
	// someone else cannot take it meanwhile
	rival := e.newAgent("rival", "member", nil)
	rival.init()
	if r := rival.call("claim_issue", map[string]any{"key": key}); !r.Err || !strings.Contains(r.Text, "claimed by") {
		t.Fatalf("a second agent claimed work that is held: %+v", r)
	}
	// 3. plan, in a comment; split it into subtasks
	must("add_comment", map[string]any{"key": key, "body": "Plan: 1) add the Retry button component, 2) wire it to the challenge callback, 3) test the flow on the second attempt."})
	for _, s := range []string{"Retry button component", "Wire the button to the challenge callback"} {
		must("create_subtask", map[string]any{"parent": key, "summary": s})
	}
	// 4. a branch, and the pull request that then arrives from the provider
	br := must("create_branch", map[string]any{"key": key})
	branch, _ := br.Data["branch"].(string)
	if branch == "" || !strings.Contains(branch, key) {
		t.Fatalf("the branch: %v", br.Data)
	}
	if posts := git.PostsMade(); len(posts) != 1 || !strings.Contains(posts[0], key) {
		t.Fatalf("the branch was not created at the provider: %v", posts)
	}
	hook := gitmock.GitHubHook(secret, "pull_request", "scenario-pr", gitmock.GHPR("acme/shop", 12, key+" Add a retry button", "", branch, "99999999", "open", false, time.Now()))
	if _, err := e.Svc.ReceiveGitWebhook(ctx, conn.ID, "192.0.2.1:1", hook.Header, hook.Body); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ProcessGitInbox(ctx, 10); err != nil {
		t.Fatal(err)
	}
	dev := must("list_development", map[string]any{"key": key})
	prs, _ := dev.Data["pull_requests"].([]any)
	if len(prs) != 1 || prs[0].(map[string]any)["state"] != "open" {
		t.Fatalf("the agent does not see the pull request that arrived: %v", dev.Data)
	}
	// 5. record the test results
	must("report_progress", map[string]any{"key": key, "done": "Implemented the button and the callback; 14 tests passed, 0 failed.", "next": "Waiting for review.", "blockers": "none"})
	// 6. send it to review; closing it itself is refused with the reason
	must("transition_issue", map[string]any{"key": key, "transition": "Send to review"})
	if r := step("transition_issue", map[string]any{"key": key, "transition": "Approve"}); !r.Err || !strings.Contains(r.Text, "needs a person") {
		t.Fatalf("the agent was allowed to approve its own work: %+v", r)
	}
	// 7. ask a person
	must("request_human_input", map[string]any{"key": key, "question": "Ready for review: PR #12. Please approve or say what to change."})
	if fv, _ := e.Svc.View(ctx, e.Alice, key); fv.Input == nil || !strings.Contains(fv.Input.Question, "PR #12") {
		t.Fatalf("the issue is not flagged as waiting for a person: %+v", fv.Input)
	}
	flagged := ag.ok("search_issues", map[string]any{"q": "inputrequested is not EMPTY"})
	calls++
	if len(flagged.Data["issues"].([]any)) != 1 {
		t.Fatalf("the flagged issue is not findable: %v", flagged.Data)
	}
	// 8. the person approves: a reply clears the flag, then the approval is theirs
	if _, err := e.Svc.AddComment(ctx, e.Alice, key, "Looks good, approving.", false); err != nil {
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, key)
	var approve string
	for _, tr := range v.Transitions {
		if tr.Name == "Approve" {
			approve = tr.ID
		}
	}
	if approve == "" {
		t.Fatalf("the person is not offered Approve: %+v", v.Transitions)
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, key, approve, nil); err != nil {
		t.Fatal(err)
	}

	// ---- what everyone sees afterwards
	end := must("get_issue_context", map[string]any{"key": key})
	if end.Data["status"] != "Done" || end.Data["input_requested"] != nil {
		t.Fatalf("the end state: status %v, flag %v", end.Data["status"], end.Data["input_requested"])
	}
	if end.Data["lease"] != nil {
		t.Fatalf("the agent still holds a lease on finished work: %v", end.Data["lease"])
	}
	// the history says who did what
	h, _ := e.Svc.History(ctx, e.Alice, key)
	var byAgent, byPerson []string
	for _, x := range h {
		if x.Field == "status" && x.After != nil {
			if strings.HasPrefix(x.Actor, "Agent: builder") {
				byAgent = append(byAgent, *x.After)
			} else {
				byPerson = append(byPerson, *x.After)
			}
		}
	}
	if fmt.Sprint(byAgent) != "[In Progress In Review]" || fmt.Sprint(byPerson) != "[Done]" {
		t.Fatalf("status changes by the agent %v and by a person %v", byAgent, byPerson)
	}
	subs := 0
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue i JOIN issue p ON p.id=i.parent_id WHERE p.key=$1`, key).Scan(&subs)
	if subs != 2 {
		t.Fatalf("%d subtasks", subs)
	}
	// the owner was told: Alice has notifications from the agent
	e.Svc.NotifyOnce(ctx)
	var toldAlice int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM notification WHERE user_id=$1 AND actor_label LIKE 'Agent: builder%'`, e.Alice.ID).Scan(&toldAlice)
	if toldAlice < 3 {
		t.Fatalf("Alice received %d notices of the agent's work", toldAlice)
	}
	// every call is in the log, with the agent, the tool and the outcome, and the refused approval is recorded as such
	var logged, denied int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE outcome<>'ok') FROM mcp_call_log WHERE agent_id=$1 AND tool<>'initialize'`, ag.user.ID).Scan(&logged, &denied)
	if logged != calls || denied != 1 {
		t.Fatalf("%d calls made, %d logged, %d of them not ok", calls, logged, denied)
	}
	// and the whole loop was cheap
	if calls > 16 || chars > 30000 {
		t.Fatalf("the loop took %d calls and %d characters (~%d tokens)", calls, chars, chars/4)
	}
	t.Logf("the whole loop: %d calls, %d characters (~%d tokens)", calls, chars, chars/4)
	// the agent cannot touch what it was not given: the other issue is untouched and still unclaimed
	other, _ := e.Svc.Lease(ctx, e.Admin, notReady.Key)
	if other != nil {
		t.Fatalf("another issue was claimed: %v", other)
	}
}
