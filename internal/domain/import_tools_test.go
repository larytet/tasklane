package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

const ghFile = `{"issues":[
 {"number":1,"title":"Login fails on Safari","body":"Steps to repro","state":"open","html_url":"https://github.com/o/r/issues/1","user":{"login":"octocat"},"assignee":{"login":"hubot"},
  "labels":[{"name":"bug"},{"name":"priority: high"},{"name":"help wanted"}],"milestone":{"title":"v1.0","due_on":"2026-12-01T00:00:00Z","state":"open"},"created_at":"2026-01-02T10:00:00Z","updated_at":"2026-01-05T10:00:00Z"},
 {"number":2,"title":"Add dark mode","body":"","state":"closed","state_reason":"completed","user":{"login":"hubot"},"labels":[{"name":"enhancement"}],"milestone":{"title":"v1.0","due_on":"2026-12-01T00:00:00Z","state":"open"},
  "created_at":"2026-01-03T10:00:00Z","updated_at":"2026-01-09T10:00:00Z","closed_at":"2026-01-09T10:00:00Z"},
 {"number":3,"title":"Fix typo (PR)","state":"open","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/3"},"user":{"login":"octocat"},"created_at":"2026-01-04T10:00:00Z"},
 {"number":4,"title":"","state":"open","user":{"login":"octocat"},"created_at":"2026-01-04T10:00:00Z"}],
 "comments":[{"id":101,"issue_url":"https://api.github.com/repos/o/r/issues/1","body":"I can reproduce this","user":{"login":"hubot"},"created_at":"2026-01-02T12:00:00Z"},
  {"id":102,"issue_url":"https://api.github.com/repos/o/r/issues/1","body":"Working on it","user":{"login":"stranger"},"created_at":"2026-01-03T12:00:00Z"}]}`

const trelloFile = `{"name":"Roadmap","lists":[{"id":"l1","name":"To Do"},{"id":"l2","name":"Doing"},{"id":"l3","name":"Done"}],
 "members":[{"id":"m1","username":"ana","fullName":"Ana Novak"},{"id":"m2","username":"bo","fullName":"Bo Berg"}],
 "checklists":[{"id":"ch1","name":"Steps","checkItems":[{"id":"ci1","name":"Write the spec","state":"complete"},{"id":"ci2","name":"Review it","state":"incomplete"}]}],
 "cards":[
  {"id":"5f5e1000aaaa","name":"Plan the launch","desc":"Everything for the launch","idList":"l2","closed":false,"due":"2026-03-01T00:00:00.000Z","dateLastActivity":"2026-02-01T10:00:00.000Z","shortUrl":"https://trello.com/c/abc",
   "idMembers":["m1","m2"],"idChecklists":["ch1"],"labels":[{"name":"launch","color":"green"},{"name":"","color":"red"}],
   "attachments":[{"name":"Brief","url":"https://example.com/brief","isUpload":false},{"name":"deck.pdf","url":"https://trello.com/1/cards/x/attachments/y/download/deck.pdf","isUpload":true}]},
  {"id":"5f5e1000bbbb","name":"Ship it","desc":"","idList":"l3","closed":false,"dateLastActivity":"2026-02-02T10:00:00.000Z","idMembers":[],"idChecklists":[],"labels":[],"attachments":[]},
  {"id":"5f5e1000cccc","name":"Old idea","desc":"","idList":"l1","closed":true,"dateLastActivity":"2026-01-01T10:00:00.000Z","idMembers":[],"idChecklists":[],"labels":[],"attachments":[]}],
 "actions":[{"id":"a1","type":"commentCard","date":"2026-02-01T11:00:00.000Z","data":{"text":"Kicked off","card":{"id":"5f5e1000aaaa"}},"memberCreator":{"username":"ana","fullName":"Ana Novak"}},
  {"id":"a2","type":"updateCard","date":"2026-02-01T12:00:00.000Z","data":{"card":{"id":"5f5e1000aaaa"}},"memberCreator":{"username":"ana"}}]}`

const linearFile = "ID,Team,Title,Description,Status,Estimate,Priority,Project,Creator,Assignee,Labels,Cycle Number,Created,Updated,Completed,Canceled,Parent issue\n" +
	"ENG-1,Eng,Fast search,Make it fast,In Progress,3,High,Search,ana@x.example,bo@x.example,\"perf, backend\",4,2026-01-02T10:00:00.000Z,2026-01-05T10:00:00.000Z,,,\n" +
	"ENG-2,Eng,Index the titles,,Done,1,Medium,Search,ana@x.example,,,4,2026-01-03T10:00:00.000Z,2026-01-06T10:00:00.000Z,2026-01-06T10:00:00.000Z,,ENG-1\n" +
	"ENG-3,Eng,Drop the old API,,Canceled,,No priority,,Ana,,,,2026-01-04T10:00:00.000Z,2026-01-07T10:00:00.000Z,,2026-01-07T10:00:00.000Z,\n" +
	"ENG-4,Eng,,no title,Todo,,Urgent,,,,,,2026-01-04T10:00:00.000Z,,,,\n"

const asanaFile = "Task ID,Created At,Completed At,Last Modified,Name,Section/Column,Assignee,Assignee Email,Due Date,Tags,Notes,Parent task\n" +
	"1001,2026-01-02,,2026-01-05,Prepare the report,Doing,Ana Novak,ana@x.example,2026-02-01,\"finance, q1\",Numbers for Q1,\n" +
	"1002,2026-01-02,2026-01-04,2026-01-04,Collect the invoices,Done,Bo Berg,bo@x.example,,,,Prepare the report\n" +
	"1003,2026-01-03,,2026-01-03,Archive the files,Backlog,,,,,,A task that is not in the file\n"

type impRig struct{ *testutil.Env }

func newImp(t *testing.T) *impRig {
	e := testutil.New(t)
	e.Project()
	return &impRig{e}
}

func (r *impRig) run(t *testing.T, tool, data string, in domain.ToolImportIn) *domain.ImportReport {
	t.Helper()
	in.Project = "ABC"
	rep, err := r.Svc.ImportTool(ctx, r.Admin, tool, []byte(data), in)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return rep
}

func (r *impRig) issue(t *testing.T, rep *domain.ImportReport, ext string) domain.IssueView {
	t.Helper()
	k, ok := rep.KeyMap[ext]
	if !ok {
		t.Fatalf("no issue for %q in the key map %v", ext, rep.KeyMap)
	}
	v, err := r.Svc.View(ctx, r.Admin, k)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (r *impRig) count(t *testing.T) (n int) {
	r.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id=(SELECT id FROM project WHERE key='ABC')`).Scan(&n)
	return
}

// IMP01: GitHub Issues: state, labels (bug is also the type, priority: x a priority), assignees through the user map, comments, milestones as
// versions, pull requests skipped, a bad row reported without stopping the others; a dry run changes nothing; the same file again updates.
func TestIMP01_GitHub(t *testing.T) {
	r := newImp(t)
	um := map[string]string{"hubot": "hubot@x.example", "octocat": "octo@x.example"}
	dry := r.run(t, "github", ghFile, domain.ToolImportIn{DryRun: true, UserMap: um})
	if r.count(t) != 0 || dry.Counts["issues_created"] != 2 {
		t.Fatalf("a dry run: %d issues made, report %+v", r.count(t), dry.Counts)
	}
	rep := r.run(t, "github", ghFile, domain.ToolImportIn{UserMap: um})
	if r.count(t) != 2 || rep.Counts["issues_created"] != 2 {
		t.Fatalf("import: %d issues, %+v errors %v", r.count(t), rep.Counts, rep.Errors)
	}
	v := r.issue(t, rep, "gh:1")
	if v.Issue.Type != "Bug" || v.Issue.Priority != "high" || v.Issue.Status != "To Do" || v.Issue.Summary != "Login fails on Safari" {
		t.Fatalf("issue 1: %+v", v.Issue)
	}
	if v.Issue.Assignee == nil || v.Issue.Assignee.Email != "hubot@x.example" || v.Issue.Reporter.Email != "octo@x.example" {
		t.Fatalf("people: %+v %+v", v.Issue.Assignee, v.Issue.Reporter)
	}
	if !strings.Contains(v.Issue.Description, "Steps to repro") || !strings.Contains(v.Issue.Description, "https://github.com/o/r/issues/1") {
		t.Fatalf("description: %q", v.Issue.Description)
	}
	if strings.Join(v.Issue.Labels, ",") != "bug,help-wanted" { // bug is also the type; "priority: high" became the priority and is not a label
		t.Fatalf("labels: %v", v.Issue.Labels)
	}
	if len(v.Comments) != 2 || v.Comments[0].Body != "I can reproduce this" {
		t.Fatalf("comments: %+v", v.Comments)
	}
	if v.Issue.Created.Year() != 2026 || v.Issue.Created.Month() != 1 || v.Issue.Created.Day() != 2 {
		t.Fatalf("the creation date was not kept: %v", v.Issue.Created)
	}
	c := r.issue(t, rep, "gh:2")
	if c.Issue.Status != "Done" || c.Issue.Resolved == nil || c.Issue.Type != "Task" {
		t.Fatalf("closed issue: %+v", c.Issue)
	}
	// milestones became a version, released state kept (open here), both issues in it
	if len(v.Versions.Fix) != 1 || v.Versions.Fix[0] != "v1.0" || len(c.Versions.Fix) != 1 {
		t.Fatalf("versions: %+v %+v", v.Versions, c.Versions)
	}
	vs, _ := r.Svc.ListVersions(ctx, r.Admin, "ABC", false)
	if len(vs) != 1 || vs[0].ReleaseDate != "2026-12-01" || vs[0].Released {
		t.Fatalf("the version: %+v", vs)
	}
	// reported: the pull request skipped, the empty title, the stranger with no address
	all := strings.Join(append(append(rep.Warnings, rep.Gaps...), rep.Errors...), "\n")
	for _, want := range []string{"1 pull request", "#4: the title is empty", `"stranger"`} {
		if !strings.Contains(all, want) {
			t.Fatalf("the report lacks %q:\n%s", want, all)
		}
	}
	// the same file again updates in place
	again := r.run(t, "github", ghFile, domain.ToolImportIn{UserMap: um})
	if r.count(t) != 2 || again.KeyMap["gh:1"] != rep.KeyMap["gh:1"] {
		t.Fatalf("a second import made %d issues, keys %v", r.count(t), again.KeyMap)
	}
	if v2 := r.issue(t, again, "gh:1"); len(v2.Comments) != 2 {
		t.Fatalf("comments doubled: %d", len(v2.Comments))
	}
	// the plain array of the issues list works too, and without a user map the people are left out and said so
	arr := ghFile[strings.Index(ghFile, `"issues":[`)+9 : strings.Index(ghFile, "],\n \"comments\"")+1]
	rep3 := r.run(t, "github", arr, domain.ToolImportIn{})
	if !strings.Contains(strings.Join(rep3.Gaps, "\n"), `"hubot" has no address`) {
		t.Fatalf("gaps: %v", rep3.Gaps)
	}
	// rollback by import id
	if n, err := r.Svc.RollbackImport(ctx, r.Admin, rep.JobID); err != nil || n < 0 {
		t.Fatalf("rollback: %d %v", n, err)
	}
}

// IMP02: Trello: lists to statuses, cards to issues, checklist items to subtasks, labels, the first member as the assignee, comments, links
// and files in the description, archived cards done.
func TestIMP02_Trello(t *testing.T) {
	r := newImp(t)
	rep := r.run(t, "trello", trelloFile, domain.ToolImportIn{UserMap: map[string]string{"ana": "ana@x.example"}})
	if len(rep.Errors) != 0 {
		t.Fatalf("errors: %v", rep.Errors)
	}
	card := r.issue(t, rep, "trello:5f5e1000aaaa")
	if card.Issue.Status != "In Progress" || card.Issue.Summary != "Plan the launch" || card.Issue.Assignee == nil || card.Issue.Assignee.Email != "ana@x.example" {
		t.Fatalf("card: %+v", card.Issue)
	}
	for _, want := range []string{"Everything for the launch", "https://trello.com/c/abc", "Due: 2026-03-01", "Attachment: Brief https://example.com/brief", "Attachment (file not copied): deck.pdf", "Other members: bo"} {
		if !strings.Contains(card.Issue.Description, want) {
			t.Fatalf("description lacks %q:\n%s", want, card.Issue.Description)
		}
	}
	if len(card.Issue.Labels) != 2 || card.Issue.Labels[0] != "launch" || card.Issue.Labels[1] != "red" {
		t.Fatalf("labels: %v", card.Issue.Labels)
	}
	if len(card.Comments) != 1 || card.Comments[0].Body != "Kicked off" {
		t.Fatalf("comments: %+v", card.Comments)
	}
	// the checklist: two subtasks, one done
	sub1, sub2 := r.issue(t, rep, "trello:ci1"), r.issue(t, rep, "trello:ci2")
	if sub1.Issue.Type != "Subtask" || sub1.Issue.Parent != card.Issue.Key || sub1.Issue.Status != "Done" || sub2.Issue.Status != "To Do" {
		t.Fatalf("subtasks: %+v / %+v", sub1.Issue, sub2.Issue)
	}
	if done := r.issue(t, rep, "trello:5f5e1000bbbb"); done.Issue.Status != "Done" || done.Issue.Resolved == nil {
		t.Fatalf("a card in the Done list: %+v", done.Issue)
	}
	if old := r.issue(t, rep, "trello:5f5e1000cccc"); old.Issue.Status != "Done" || len(old.Issue.Labels) != 1 || old.Issue.Labels[0] != "trello-archived" {
		t.Fatalf("an archived card: %+v", old.Issue)
	}
	// the creation time comes from the card id
	if card.Issue.Created.Year() != 2020 {
		t.Fatalf("created: %v", card.Issue.Created)
	}
	if !strings.Contains(strings.Join(rep.Gaps, "\n"), "1 attached file") {
		t.Fatalf("gaps: %v", rep.Gaps)
	}
	if n := r.count(t); n != 5 {
		t.Fatalf("issues: %d", n)
	}
	// the user map can be overridden by a status map
	r2 := newImp(t)
	rep2 := r2.run(t, "trello", trelloFile, domain.ToolImportIn{StatusMap: map[string]string{"Doing": "To Do", "To Do": "Nowhere"}})
	if c := r2.issue(t, rep2, "trello:5f5e1000aaaa"); c.Issue.Status != "To Do" {
		t.Fatalf("status map: %+v", c.Issue)
	}
	if !strings.Contains(strings.Join(rep2.Gaps, "\n"), `"Nowhere"`) {
		t.Fatalf("a status the workflow lacks: %v", rep2.Gaps)
	}
	if _, err := r2.Svc.ImportTool(ctx, r2.Admin, "trello", []byte(`{"cards":[]}`), domain.ToolImportIn{Project: "ABC"}); !isInvalid(err) {
		t.Fatalf("an empty board: %v", err)
	}
}

// IMP03: Linear: priorities, states, estimate, labels, cycles as labels, projects as epics, sub-issues as subtasks; a bad row does not stop the rest.
func TestIMP03_Linear(t *testing.T) {
	r := newImp(t)
	rep := r.run(t, "linear", linearFile, domain.ToolImportIn{})
	a := r.issue(t, rep, "linear:ENG-1")
	if a.Issue.Status != "In Progress" || a.Issue.Priority != "high" || a.Issue.Estimate == nil || *a.Issue.Estimate != 3 || a.Issue.Assignee.Email != "bo@x.example" {
		t.Fatalf("ENG-1: %+v", a.Issue)
	}
	if got := strings.Join(a.Issue.Labels, ","); got != "perf,backend,cycle-4" {
		t.Fatalf("labels: %s", got)
	}
	epic := r.issue(t, rep, "linear-project:Search")
	if epic.Issue.Type != "Epic" || epic.Issue.Summary != "Search" || a.Issue.Parent != epic.Issue.Key {
		t.Fatalf("the project epic: %+v, parent %q", epic.Issue, a.Issue.Parent)
	}
	b := r.issue(t, rep, "linear:ENG-2")
	if b.Issue.Type != "Subtask" || b.Issue.Parent != a.Issue.Key || b.Issue.Status != "Done" || b.Issue.Resolved == nil {
		t.Fatalf("ENG-2: %+v", b.Issue)
	}
	c := r.issue(t, rep, "linear:ENG-3")
	if c.Issue.Status != "Done" || c.Issue.Priority != "medium" || c.Issue.Parent != "" {
		t.Fatalf("ENG-3 (canceled): %+v", c.Issue)
	}
	if !strings.Contains(strings.Join(rep.Errors, "\n"), "ENG-4") || !strings.Contains(strings.Join(rep.Gaps, "\n"), "cycles are kept as labels") {
		t.Fatalf("report: errors %v gaps %v", rep.Errors, rep.Gaps)
	}
	if !strings.Contains(strings.Join(rep.Gaps, "\n"), `"Ana"`) {
		t.Fatalf("a creator with no address: %v", rep.Gaps)
	}
	// without epics, and with the priority map overridden
	r2 := newImp(t)
	no := false
	rep2 := r2.run(t, "linear", linearFile, domain.ToolImportIn{Epics: &no, PriorityMap: map[string]string{"High": "highest"}})
	if _, ok := rep2.KeyMap["linear-project:Search"]; ok || r2.issue(t, rep2, "linear:ENG-1").Issue.Priority != "highest" {
		t.Fatalf("options: %v", rep2.KeyMap)
	}
	if _, err := r.Svc.ImportTool(ctx, r.Admin, "linear", []byte("Name,Value\nx,y\n"), domain.ToolImportIn{Project: "ABC"}); !isInvalid(err) {
		t.Fatalf("the wrong CSV: %v", err)
	}
}

// IMP04: Asana: sections, assignee by address, tags, completion, subtasks by parent name, due date kept in the description.
func TestIMP04_Asana(t *testing.T) {
	r := newImp(t)
	rep := r.run(t, "asana", asanaFile, domain.ToolImportIn{})
	a := r.issue(t, rep, "asana:1001")
	if a.Issue.Status != "In Progress" || a.Issue.Assignee == nil || a.Issue.Assignee.Email != "ana@x.example" || strings.Join(a.Issue.Labels, ",") != "finance,q1" || !strings.Contains(a.Issue.Description, "Due: 2026-02-01") {
		t.Fatalf("1001: %+v", a.Issue)
	}
	b := r.issue(t, rep, "asana:1002")
	if b.Issue.Type != "Subtask" || b.Issue.Parent != a.Issue.Key || b.Issue.Status != "Done" || b.Issue.Resolved == nil {
		t.Fatalf("1002: %+v", b.Issue)
	}
	c := r.issue(t, rep, "asana:1003")
	if c.Issue.Type != "Task" || c.Issue.Parent != "" || c.Issue.Status != "To Do" {
		t.Fatalf("1003: %+v", c.Issue)
	}
	if !strings.Contains(strings.Join(rep.Gaps, "\n"), "is not in the file") {
		t.Fatalf("gaps: %v", rep.Gaps)
	}
	// the same file again: no duplicates
	r.run(t, "asana", asanaFile, domain.ToolImportIn{})
	if n := r.count(t); n != 3 {
		t.Fatalf("issues after a second import: %d", n)
	}
}

// IMP05: who may import, into what, with what: project administrators only, never a mirrored project, a workflow with custom statuses is used.
func TestIMP05_PermissionsAndWorkflow(t *testing.T) {
	r := newImp(t)
	if _, err := r.Svc.ImportTool(ctx, r.Alice, "linear", []byte(linearFile), domain.ToolImportIn{Project: "ABC"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member imported: %v", err)
	}
	out := r.NewUser("out5@example.com", "Outsider", false)
	if _, err := r.Svc.ImportTool(ctx, out, "linear", []byte(linearFile), domain.ToolImportIn{Project: "ABC"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider imported: %v", err)
	}
	if _, err := r.Svc.ImportTool(ctx, r.Admin, "jira-cloud", nil, domain.ToolImportIn{Project: "ABC"}); !isInvalid(err) {
		t.Fatalf("an unknown tool: %v", err)
	}
	if _, err := r.Svc.ImportTool(ctx, r.Admin, "linear", []byte(linearFile), domain.ToolImportIn{Project: "NOPE"}); !isInvalid(err) {
		t.Fatalf("a missing project: %v", err)
	}
	if _, err := r.Svc.ImportTool(ctx, r.Admin, "github", []byte("not json"), domain.ToolImportIn{Project: "ABC"}); !isInvalid(err) {
		t.Fatalf("not JSON: %v", err)
	}
	// a project administrator (not the instance administrator) may
	if err := r.Svc.AddMember(ctx, r.Admin, "ABC", r.Bob.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Svc.ImportTool(ctx, r.Bob, "asana", []byte(asanaFile), domain.ToolImportIn{Project: "ABC"}); err != nil {
		t.Fatalf("a project administrator: %v", err)
	}
	// a workflow whose statuses are not the defaults: states land on the nearest status of their kind, and a mapped name is used as it is
	if _, err := r.Svc.CreateProject(ctx, r.Admin, "WRK", "Workflow"); err != nil {
		t.Fatal(err)
	}
	var wf string
	r.Store.Pool.QueryRow(ctx, `SELECT workflow_id::text FROM project WHERE key='WRK'`).Scan(&wf)
	r.Store.Pool.Exec(ctx, `UPDATE status SET name='Backlog' WHERE workflow_id::text=$1 AND category='todo'`, wf)
	r.Store.Pool.Exec(ctx, `UPDATE status SET name='Shipped' WHERE workflow_id::text=$1 AND category='done'`, wf)
	rep, err := r.Svc.ImportTool(ctx, r.Admin, "asana", []byte(asanaFile), domain.ToolImportIn{Project: "WRK"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := r.Svc.View(ctx, r.Admin, rep.KeyMap["asana:1002"])
	w, _ := r.Svc.View(ctx, r.Admin, rep.KeyMap["asana:1003"])
	if v.Issue.Status != "Shipped" || w.Issue.Status != "Backlog" {
		t.Fatalf("statuses in a custom workflow: %q %q", v.Issue.Status, w.Issue.Status)
	}
}

const genericFile = `{"meta":{"total":5},"data":{"tickets":[
 {"ref":"T-1","fields":{"title":"Printer jams","body":"Tray 2","kind":"Defect","state":"Working","prio":"P1","who":{"login":"ana"},"tags":["hw","urgent"],"opened":"2026-02-01T09:00:00Z","touched":1770000000},
  "notes":[{"nid":"n1","by":"ana","text":"Looking","at":"2026-02-02T09:00:00Z"},{"nid":"n2","by":"nobody","text":"Fixed the tray","at":"2026-02-03T09:00:00Z"},{"by":"ana","text":"A note with no id","at":"2026-02-03T10:00:00Z"}]},
 {"ref":"T-2","fields":{"title":"Replace toner","kind":"Chore","state":"Finished","prio":"P3","tags":"hw, supplies","opened":"2026-02-01T10:00:00Z","closed":"2026-02-04T10:00:00Z"},"parent":"T-1","notes":[]},
 {"ref":"T-3","fields":{"title":"","state":"New"}},
 {"fields":{"title":"No reference"}},
 "not an object"],
 "archive":[{"ref":"T-9","fields":{"title":"Old ticket","state":"Finished"}}]}}`

var genericMap = domain.JSONMapping{
	Items: "$.data.tickets", Comments: "notes",
	Fields: map[string]string{"key": "ref", "summary": "fields.title", "description": "fields.body", "type": "fields.kind", "status": "fields.state", "priority": "fields.prio",
		"assignee": "fields.who.login", "labels": "fields.tags", "created": "fields.opened", "updated": "fields.touched", "resolved": "fields.closed", "parent": "parent"},
	CommentFields: map[string]string{"id": "nid", "author": "by", "body": "text", "created": "at"},
}

// IMP10: the generic JSON importer: any JSON through paths. Nested fields, a label list and a label string, dates as text and as Unix seconds,
// a mapped type and status and priority, comments with authors through the user map, a parent by reference, bad items reported without
// stopping the others, a second run updating in place, and the mapping itself checked.
func TestIMP10_GenericJSONWithAPathMapping(t *testing.T) {
	r := newImp(t)
	m := genericMap
	in := domain.ToolImportIn{Mapping: &m, UserMap: map[string]string{"ana": "ana@x.example"},
		TypeMap: map[string]string{"Defect": "Bug", "Chore": "Task"}, StatusMap: map[string]string{"Working": "In Progress", "Finished": "Done"}, PriorityMap: map[string]string{"P1": "highest", "P3": "low"}}
	dry := in
	dry.DryRun = true
	rep := r.run(t, "json", genericFile, dry)
	if r.count(t) != 0 || !rep.DryRun {
		t.Fatalf("a dry run made %d issues", r.count(t))
	}
	rep = r.run(t, "json", genericFile, in)
	a := r.issue(t, rep, "json:T-1")
	if a.Issue.Type != "Bug" || a.Issue.Status != "In Progress" || a.Issue.Priority != "highest" || a.Issue.Assignee.Email != "ana@x.example" || a.Issue.Summary != "Printer jams" {
		t.Fatalf("T-1: %+v", a.Issue)
	}
	if strings.Join(a.Issue.Labels, ",") != "hw,urgent" || !strings.Contains(a.Issue.Description, "Tray 2") {
		t.Fatalf("labels %v description %q", a.Issue.Labels, a.Issue.Description)
	}
	if a.Issue.Created.Year() != 2026 || a.Issue.Created.Month() != 2 || !a.Issue.Updated.After(a.Issue.Created) || a.Issue.Updated.Day() != 2 { // 1770000000 is Feb 2026 as Unix seconds
		t.Fatalf("dates: %v %v", a.Issue.Created, a.Issue.Updated)
	}
	if len(a.Comments) != 3 || a.Comments[0].Body != "Looking" {
		t.Fatalf("comments: %+v", a.Comments)
	}
	b := r.issue(t, rep, "json:T-2")
	if b.Issue.Status != "Done" || b.Issue.Resolved == nil || b.Issue.Parent != a.Issue.Key || b.Issue.Type != "Subtask" || strings.Join(b.Issue.Labels, ",") != "hw,supplies" || b.Issue.Priority != "low" {
		t.Fatalf("T-2: %+v", b.Issue)
	}
	all := strings.Join(append(append(append([]string{}, rep.Warnings...), rep.Gaps...), rep.Errors...), "\n")
	for _, want := range []string{"item 3 (T-3): the summary is empty", "item 4: the key path found nothing", "item 5: not an object", `"nobody"`} {
		if !strings.Contains(all, want) {
			t.Fatalf("the report lacks %q:\n%s", want, all)
		}
	}
	if _, ok := rep.KeyMap["json:T-9"]; ok || r.count(t) != 2 {
		t.Fatalf("the archive array was not part of the items path: %d issues", r.count(t))
	}
	// the same file again updates in place; a path to two arrays reads both
	again := r.run(t, "json", genericFile, in)
	if r.count(t) != 2 || again.KeyMap["json:T-1"] != rep.KeyMap["json:T-1"] {
		t.Fatalf("a second run made %d issues", r.count(t))
	}
	if v := r.issue(t, again, "json:T-1"); len(v.Comments) != 3 {
		t.Fatalf("a second run duplicated comments: %d", len(v.Comments))
	}
	m2 := m
	m2.Items = "$..tickets"
	in2 := in
	in2.Mapping = &m2
	if rep2 := r.run(t, "json", `{"a":{"tickets":[{"ref":"X1","fields":{"title":"deep"}}]},"b":{"tickets":[{"ref":"X2","fields":{"title":"deeper"}}]}}`, in2); len(rep2.KeyMap) != 2 {
		t.Fatalf("a descendant path: %v", rep2.KeyMap)
	}
	// the mapping is checked before anything is read
	bad := func(mut func(*domain.JSONMapping), data string) {
		t.Helper()
		x := m
		x.Fields = map[string]string{}
		for k, v := range m.Fields {
			x.Fields[k] = v
		}
		mut(&x)
		_, err := r.Svc.ImportTool(ctx, r.Admin, "json", []byte(data), domain.ToolImportIn{Project: "ABC", Mapping: &x})
		if !isInvalid(err) {
			t.Fatalf("a bad mapping was accepted: %v", err)
		}
	}
	bad(func(x *domain.JSONMapping) { x.Items = "" }, genericFile)
	bad(func(x *domain.JSONMapping) { delete(x.Fields, "key") }, genericFile)
	bad(func(x *domain.JSONMapping) { x.Fields["summary"] = "fields[?(@.t)]" }, genericFile)
	bad(func(x *domain.JSONMapping) { x.Fields["colour"] = "c" }, genericFile)
	bad(func(x *domain.JSONMapping) { x.CommentFields = map[string]string{"id": "nid"} }, genericFile)
	bad(func(x *domain.JSONMapping) {}, "{not json")
	bad(func(x *domain.JSONMapping) { x.Items = "$.nothing" }, genericFile) // no issues found
	if _, err := r.Svc.ImportTool(ctx, r.Admin, "json", []byte(genericFile), domain.ToolImportIn{Project: "ABC"}); !isInvalid(err) {
		t.Fatalf("no mapping at all: %v", err)
	}
}
