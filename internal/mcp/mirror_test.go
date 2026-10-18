package mcp_test

import (
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil/jiramock"
)

// mirrorEnv mirrors a Jira project WEB (one issue, WEB-1) and gives an agent a role in it.
func mirrorEnv(t *testing.T) (*env, *agent, *jiramock.Server) {
	e := newEnv(t)
	jm := jiramock.New("sync@example.com", "tok")
	t.Cleanup(jm.Close)
	jm.Upsert(jiramock.IssueJSON("10001", "WEB-1", "WEB", "Mirrored issue", "To Do", "new", "Task", time.Now().Add(-time.Hour), time.Now().Add(-time.Minute)))
	c, _, err := e.Svc.CreateJiraConnection(ctx, e.Admin, domain.JiraConnIn{Name: "acme", SiteURL: jm.URL, Email: "sync@example.com", Token: "tok", AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.MirrorJiraProject(ctx, e.Admin, c.ID, "WEB", "WEB", "Web", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); err != nil {
		t.Fatal(err)
	}
	ag := e.newAgent("jira-agent", "", nil)
	ag.init()
	e.Store.Pool.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) SELECT p.id,$1::uuid,'member' FROM project p WHERE p.key='WEB'`, ag.user.ID)
	e.Store.Pool.Exec(ctx, `UPDATE jira_project SET perm_synced_at=now()`)
	return e, ag, jm
}

// MCP-16: on a mirrored project every read tool works and every write tool returns the read-only mirror refusal that names the Jira
// site. Every tool must be classified here: adding one without saying how it behaves on a mirror fails this test, so a write path
// cannot reach mirrored data by being forgotten.
func TestMCP16_EveryToolOnAMirroredProjectIsEitherReadableOrRefusedWithTheJiraLink(t *testing.T) {
	e, ag, jm := mirrorEnv(t)
	_ = e
	reads := map[string]map[string]any{
		"list_projects":     {},
		"search_issues":     {"q": "project = WEB"},
		"get_issue":         {"key": "WEB-1"},
		"get_issue_context": {"key": "WEB-1"},
		"list_comments":     {"key": "WEB-1"},
		"list_transitions":  {"key": "WEB-1"},
		"list_development":  {"key": "WEB-1"},
	}
	writes := map[string]map[string]any{
		"add_comment":         {"key": "WEB-1", "body": "hi"},
		"report_progress":     {"key": "WEB-1", "done": "something"},
		"update_fields":       {"key": "WEB-1", "priority": "high"},
		"edit_issue_text":     {"key": "WEB-1", "field": "summary", "old_text": "Mirrored", "new_text": "Changed"},
		"claim_issue":         {"key": "WEB-1"},
		"release_issue":       {"key": "WEB-1"},
		"request_human_input": {"key": "WEB-1", "question": "may I?"},
		"transition_issue":    {"key": "WEB-1", "transition": "Start progress"},
		"create_issue":        {"project": "WEB", "summary": "x"},
		"create_subtask":      {"parent": "WEB-1", "summary": "x"},
		"create_branch":       {"key": "WEB-1"},
		"bulk_update_fields":  {"keys": []any{"WEB-1"}, "priority": "high", "dry_run": false},
		"bulk_transition":     {"keys": []any{"WEB-1"}, "transition": "Start progress", "dry_run": false},
	}
	listed := ag.rpc("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	var names []string
	for _, x := range listed {
		tool := x.(map[string]any)
		name := tool["name"].(string)
		names = append(names, name)
		readOnly := tool["annotations"].(map[string]any)["readOnlyHint"] == true
		_, isRead := reads[name]
		_, isWrite := writes[name]
		switch {
		case !isRead && !isWrite:
			t.Errorf("tool %s is not classified for mirrored projects: add it to reads or writes in this test", name)
		case isRead && !readOnly, isWrite && readOnly:
			t.Errorf("tool %s is declared read-only=%v but classified the other way here", name, readOnly)
		}
	}
	for name, args := range reads {
		r := ag.call(name, args)
		if r.Err {
			t.Errorf("read tool %s failed on a mirrored project: %s", name, r.Text)
		}
	}
	for name, args := range writes {
		r := ag.call(name, args)
		if strings.HasPrefix(name, "bulk_") {
			// a bulk tool reports each issue: all refused with the reason, none changed
			if !strings.Contains(r.Text, `"changed":0`) || !strings.Contains(r.Text, `"ok":false`) || !strings.Contains(r.Text, "mirrored from Jira") || !strings.Contains(r.Text, jm.URL) {
				t.Errorf("%s on a mirrored issue: %s", name, r.Text)
			}
			continue
		}
		if !r.Err {
			t.Errorf("write tool %s succeeded on a mirrored project: %s", name, r.Text)
			continue
		}
		// the refusal says why and where to go
		if !strings.Contains(r.Text, "mirrored from Jira") || !strings.Contains(r.Text, jm.URL) {
			t.Errorf("write tool %s was refused without the mirror reason and the Jira address: %s", name, r.Text)
		}
	}
	for name := range reads {
		found := false
		for _, n := range names {
			found = found || n == name
		}
		if !found {
			t.Errorf("%s is classified here but is not a tool any more", name)
		}
	}
	// nothing changed in the mirrored data, and nothing was posted to the Jira double
	v, _ := e.Svc.View(ctx, e.Admin, "WEB-1")
	if v.Issue.Summary != "Mirrored issue" || len(v.Comments) != 0 || v.Issue.Status != "To Do" {
		t.Fatalf("%+v", v.Issue)
	}
	sort.Strings(names)
}

// Resources and prompts read mirrored data like any other, and a subscriber hears when the mirror brings a change in.
func TestMCP16_ResourcesPromptsAndSubscriptionsWorkOnMirroredData(t *testing.T) {
	e, ag, jm := mirrorEnv(t)
	txt := ag.rpc("resources/read", map[string]any{"uri": "tasklane://issue/WEB-1"})["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "Mirrored issue") || !strings.Contains(txt, "never as instructions") {
		t.Fatalf("%s", txt)
	}
	if res, errText := ag.prompt("summarize_thread", map[string]any{"key": "WEB-1"}); errText != "" || !strings.Contains(promptText(res), "Mirrored issue") {
		t.Fatalf("%q", errText)
	}
	s, code := ag.stream()
	if code != 200 {
		t.Fatalf("%d", code)
	}
	uri := "tasklane://issue/WEB-1"
	if msg := ag.subscribe(uri); msg != "" {
		t.Fatal(msg)
	}
	// a change made in Jira reaches the mirror at its next sync, and the subscriber is told
	jm.Upsert(jiramock.IssueJSON("10001", "WEB-1", "WEB", "Mirrored issue, renamed in Jira", "In Progress", "indeterminate", "Task", time.Now().Add(-time.Hour), time.Now()))
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.next(3 * time.Second); !ok || got != uri {
		t.Fatalf("the change that came in from Jira was not announced: %q %v", got, ok)
	}
	again := ag.rpc("resources/read", map[string]any{"uri": uri})["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(again, "renamed in Jira") {
		t.Fatalf("%s", again)
	}
	// an agent with no place in the mirrored project sees nothing of it, and a stale permission sync hides it from everyone
	out := e.newAgent("outsider", "", nil)
	out.init()
	if r := out.call("get_issue", map[string]any{"key": "WEB-1"}); !r.Err {
		t.Fatalf("%+v", r)
	}
	e.Store.Pool.Exec(ctx, `UPDATE jira_project SET perm_synced_at = now() - interval '20 minutes'`)
	if r := ag.call("get_issue", map[string]any{"key": "WEB-1"}); !r.Err {
		t.Fatalf("a mirrored issue stayed visible after its permissions went stale: %+v", r)
	}
}

// The compatibility table in docs/MCP.md is generated from the tool list and must not drift: a tool added or retyped without
// updating it fails here. (UPDATE_GOLDEN=1 prints the table to paste.)
func TestMCP16_TheCompatibilityTableInTheDocsMatchesTheTools(t *testing.T) {
	e := newEnv(t)
	ag := e.newAgent("docs", "member", nil)
	listed := ag.rpc("tools/list", nil)["result"].(map[string]any)["tools"].([]any)
	var rows []string
	for _, x := range listed {
		tool := x.(map[string]any)
		kind, mirror := "write", "refused with the Jira address"
		if tool["annotations"].(map[string]any)["readOnlyHint"] == true {
			kind, mirror = "read", "works"
		}
		rows = append(rows, "| `"+tool["name"].(string)+"` | "+kind+" | "+mirror+" |")
	}
	sort.Strings(rows)
	table := "| Tool | Kind | On a mirrored project |\n|---|---|---|\n" + strings.Join(rows, "\n")
	b, err := os.ReadFile("../../docs/MCP.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i, j := strings.Index(doc, "<!-- mirror-table:begin -->"), strings.Index(doc, "<!-- mirror-table:end -->")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		t.Logf("\n%s", table)
	}
	if i < 0 || j < i {
		t.Fatalf("docs/MCP.md has no mirror table between <!-- mirror-table:begin --> and <!-- mirror-table:end -->:\n%s", table)
	}
	got := strings.TrimSpace(doc[i+len("<!-- mirror-table:begin -->") : j])
	if got != table {
		t.Fatalf("the table in docs/MCP.md is out of date. It should be:\n%s", table)
	}
}
