package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func standardWF(t *testing.T, e *testutil.Env) string {
	t.Helper()
	var id string
	if err := e.Store.Pool.QueryRow(ctx, `SELECT workflow_id::text FROM project WHERE key='ABC'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func draftOf(t *testing.T, e *testutil.Env, id string) domain.WDef {
	t.Helper()
	v, err := e.Svc.StartWorkflowDraft(ctx, e.Admin, id)
	if err != nil || v.Draft == nil {
		t.Fatalf("%v %+v", err, v)
	}
	return *v.Draft
}

func TestLIFE03_CheckingADraft(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	wf := standardWF(t, e)
	d := draftOf(t, e, wf)
	if len(d.Statuses) != 3 || len(d.Transitions) != 5 {
		t.Fatalf("%+v", d)
	}
	bad := domain.WDef{
		Statuses:    []domain.WStatus{{Name: "A", Category: "done"}, {Name: "a", Category: "bogus"}, {Name: "Island", Category: "in_progress"}},
		Transitions: []domain.WTransition{{Name: "go", From: "A", To: "Nowhere"}, {Name: "go", From: "A", To: "Nowhere"}, {Name: "", To: "A"}},
	}
	if _, err := e.Svc.SaveWorkflowDraft(ctx, e.Admin, wf, bad); err != nil {
		t.Fatal(err)
	}
	plan, err := e.Svc.PlanWorkflowPublish(ctx, e.Admin, wf, nil)
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(plan.Errors, "\n")
	for _, want := range []string{`two statuses are called "a"`, "category is todo", "To Do status", `"Nowhere", which is not a status`, "listed twice", "needs a name"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing error %q in:\n%s", want, all)
		}
	}
	if _, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, nil, ""); err == nil {
		t.Fatal("an invalid draft was published")
	} else {
		var refused *domain.PublishRefused
		if !errors.As(err, &refused) {
			t.Fatalf("%v", err)
		}
	}
	// warnings do not block: an unreachable status and a dead end
	ok := domain.WDef{
		Statuses:    []domain.WStatus{{Name: "Open", Category: "todo"}, {Name: "Stuck", Category: "in_progress"}, {Name: "Island", Category: "in_progress"}, {Name: "Done", Category: "done"}},
		Transitions: []domain.WTransition{{Name: "Stick", From: "Open", To: "Stuck"}, {Name: "Finish", From: "Open", To: "Done"}},
	}
	e.Svc.SaveWorkflowDraft(ctx, e.Admin, wf, ok)
	plan, _ = e.Svc.PlanWorkflowPublish(ctx, e.Admin, wf, nil)
	w := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(w, `"Island" cannot be reached`) || !strings.Contains(w, `"Stuck" has no way out`) {
		t.Fatalf("%q errors %v", w, plan.Errors)
	}
	// only administrators edit workflows
	if _, err := e.Svc.GetWorkflow(ctx, e.Alice, wf); err == nil {
		t.Fatal("a member read the editor")
	}
	if err := e.Svc.AbandonWorkflowDraft(ctx, e.Admin, wf); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.AbandonWorkflowDraft(ctx, e.Admin, wf); err == nil {
		t.Fatal("abandoning twice succeeded")
	}
}

func TestLIFE03_PublishRenamesSwapsAndAddsKeepingIds(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	wf := standardWF(t, e)
	key := mk(t, e, e.Alice, "Task", "t", "")
	before, _ := e.Svc.GetWorkflow(ctx, e.Admin, wf)
	ids := map[string]string{}
	for _, st := range before.Live.Statuses {
		ids[st.Name] = st.ID
	}
	d := draftOf(t, e, wf)
	// rename To Do to Backlog, add In Review between In Progress and Done, add a transition
	for i := range d.Statuses {
		if d.Statuses[i].Name == "To Do" {
			d.Statuses[i].Name = "Backlog"
		}
	}
	d.Statuses = append(d.Statuses[:2], append([]domain.WStatus{{Name: "In Review", Category: "in_progress"}}, d.Statuses[2:]...)...)
	for i := range d.Transitions {
		switch d.Transitions[i].Name {
		case "Start progress", "Resolve":
			if d.Transitions[i].From == "To Do" {
				d.Transitions[i].From = "Backlog"
			}
		case "Reopen", "Back to To Do":
			d.Transitions[i].To = "Backlog"
		}
	}
	d.Transitions = append(d.Transitions, domain.WTransition{Name: "Request review", From: "In Progress", To: "In Review"}, domain.WTransition{Name: "Approve", From: "In Review", To: "Done"})
	e.Svc.SaveWorkflowDraft(ctx, e.Admin, wf, d)
	plan, err := e.Svc.PlanWorkflowPublish(ctx, e.Admin, wf, nil)
	if err != nil || len(plan.Errors) != 0 || len(plan.Added) != 1 || len(plan.Renamed) != 1 || plan.Renamed[0] != "To Do → Backlog" {
		t.Fatalf("%+v %v", plan, err)
	}
	res, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, nil, "add review")
	if err != nil || res.Version != 2 || len(res.Migrations) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	after, _ := e.Svc.GetWorkflow(ctx, e.Admin, wf)
	for _, st := range after.Live.Statuses {
		if old, ok := ids[st.Name]; ok && old != st.ID && st.Name != "Backlog" {
			t.Errorf("%s changed id", st.Name)
		}
		if st.Name == "Backlog" && st.ID != ids["To Do"] {
			t.Errorf("the renamed status got a new id")
		}
	}
	if after.Draft != nil || after.Version != 2 {
		t.Fatalf("%+v", after.WorkflowInfo)
	}
	// the issue follows the rename and can use the new transitions
	v, _ := e.Svc.View(ctx, e.Alice, key)
	if v.Issue.Status != "Backlog" {
		t.Fatalf("status %q", v.Issue.Status)
	}
	start, _ := trans(t, e, e.Alice, key, "Start progress")
	if _, err := e.Svc.Transition(ctx, e.Alice, key, start, nil); err != nil {
		t.Fatal(err)
	}
	rev, ok := trans(t, e, e.Alice, key, "Request review")
	if !ok {
		t.Fatal("the new transition is not offered")
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, key, rev, nil); err != nil {
		t.Fatal(err)
	}
	// a stale draft cannot overwrite a newer publish
	d2 := draftOf(t, e, wf)
	e.Svc.SaveWorkflowDraft(ctx, e.Admin, wf, d2)
	e.Store.Pool.Exec(ctx, `UPDATE workflow SET version=version+1 WHERE id::text=$1`, wf) // someone else published
	if _, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, nil, ""); err == nil || !strings.Contains(err.Error(), "since this draft was started") {
		t.Fatalf("%v", err)
	}
	// swapping two names in one publish works
	e.Svc.AbandonWorkflowDraft(ctx, e.Admin, wf)
	e.Store.Pool.Exec(ctx, `UPDATE workflow SET version=version-1 WHERE id::text=$1`, wf)
	d3 := draftOf(t, e, wf)
	var a, b int = -1, -1
	for i, st := range d3.Statuses {
		if st.Name == "In Progress" {
			a = i
		}
		if st.Name == "In Review" {
			b = i
		}
	}
	d3.Statuses[a].Name, d3.Statuses[b].Name = "In Review", "In Progress"
	for i := range d3.Transitions {
		t3 := &d3.Transitions[i]
		for _, f := range []*string{&t3.From, &t3.To} {
			switch *f {
			case "In Progress":
				*f = "In Review"
			case "In Review":
				*f = "In Progress"
			}
		}
	}
	e.Svc.SaveWorkflowDraft(ctx, e.Admin, wf, d3)
	if _, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, nil, "swap"); err != nil {
		t.Fatal(err)
	}
}

func seedInStatus(t *testing.T, e *testutil.Env, status string, n int) {
	t.Helper()
	_, err := e.Store.Pool.Exec(ctx, `
INSERT INTO issue(project_id, number, key, issue_type_id, status_id, priority, reporter_id, labels, rank, summary, created_at, updated_at)
SELECT p.id, 1000+g, p.key||'-'||(1000+g), (SELECT id FROM issue_type WHERE name='Task'), (SELECT id FROM status WHERE workflow_id=p.workflow_id AND name=$2),
  'medium', $1::uuid, '{}', lpad(g::text, 8, '0'), 'seed '||g, now(), now()
FROM project p, generate_series(1,$3) g WHERE p.key='ABC'`, e.Alice.ID, status, n)
	if err != nil {
		t.Fatal(err)
	}
	e.Store.Pool.Exec(ctx, `UPDATE project SET next_number=next_number+$1 WHERE key='ABC'`, n+1000)
}

func TestLIFE03_RemovingAStatusMigratesInFlightIssues(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	wf := standardWF(t, e)
	live := mk(t, e, e.Alice, "Task", "in flight", "")
	start, _ := trans(t, e, e.Alice, live, "Start progress")
	e.Svc.Transition(ctx, e.Alice, live, start, nil)
	seedInStatus(t, e, "In Progress", 25)

	d := draftOf(t, e, wf)
	var st []domain.WStatus
	for _, s := range d.Statuses {
		if s.Name != "In Progress" {
			st = append(st, s)
		}
	}
	d.Statuses = st
	var tr []domain.WTransition
	for _, x := range d.Transitions {
		if x.From != "In Progress" && x.To != "In Progress" {
			tr = append(tr, x)
		}
	}
	d.Transitions = tr
	e.Svc.SaveWorkflowDraft(ctx, e.Admin, wf, d)
	// the plan counts the issues and asks for a mapping
	plan, _ := e.Svc.PlanWorkflowPublish(ctx, e.Admin, wf, nil)
	if !plan.NeedsInput || len(plan.Removed) != 1 || plan.Removed[0].Issues != 26 || len(plan.Errors) != 1 || !strings.Contains(plan.Errors[0], "choose a status") {
		t.Fatalf("%+v", plan)
	}
	if _, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, map[string]string{"In Progress": "Nowhere"}, ""); err == nil {
		t.Fatal("a mapping to a missing status was accepted")
	}
	if _, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, nil, ""); err == nil {
		t.Fatal("published without a mapping")
	}
	res, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, map[string]string{"In Progress": "To Do"}, "drop progress")
	if err != nil || len(res.Migrations) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// nothing has moved yet, but the issue works: it is offered what the status it is mapped to offers, and new issues start in To Do
	if got := statusOf(t, e, live); got != "In Progress (retired)" {
		t.Fatalf("status %q", got)
	}
	if _, ok := trans(t, e, e.Alice, live, "Resolve"); !ok {
		t.Fatal("an issue in a retired status is stuck")
	}
	fresh := mk(t, e, e.Alice, "Task", "new", "")
	if got := statusOf(t, e, fresh); got != "To Do" {
		t.Fatalf("a new issue starts in %q", got)
	}
	// editors keep working while the job runs in small batches; progress is visible and the job resumes
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		hi := "high"
		for i := 0; i < 5; i++ {
			if _, err := e.Svc.UpdateFields(ctx, e.Alice, live, nil, false, domain.Changes{Priority: &hi}); err != nil {
				t.Errorf("an edit failed during the migration: %v", err)
			}
			lo := "low"
			e.Svc.UpdateFields(ctx, e.Alice, live, nil, false, domain.Changes{Priority: &lo})
		}
	}()
	moved, err := e.Svc.ProcessWorkflowMigrations(ctx, 10, 1)
	if err != nil || moved != 10 {
		t.Fatalf("%d %v", moved, err)
	}
	v, _ := e.Svc.GetWorkflow(ctx, e.Admin, wf)
	if len(v.Migrations) != 1 || v.Migrations[0].Done != 10 || v.Migrations[0].Total != 26 || v.Migrations[0].State != "pending" || !v.Migrating {
		t.Fatalf("%+v", v.Migrations)
	}
	moved, err = e.Svc.ProcessWorkflowMigrations(ctx, 10, 10)
	wg.Wait()
	if err != nil || moved != 16 {
		t.Fatalf("%d %v", moved, err)
	}
	v, _ = e.Svc.GetWorkflow(ctx, e.Admin, wf)
	if v.Migrations[0].State != "done" || v.Migrations[0].Done != 26 || v.Migrating {
		t.Fatalf("%+v", v.Migrations)
	}
	if got := statusOf(t, e, live); got != "To Do" {
		t.Fatalf("after the migration: %q", got)
	}
	var left, hist int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM status WHERE name LIKE 'In Progress%'`).Scan(&left)
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_history WHERE field='status' AND actor_label='workflow migration'`).Scan(&hist)
	if left != 0 || hist != 26 {
		t.Fatalf("retired statuses left %d, history rows %d", left, hist)
	}
	// nothing is left to do
	if n, _ := e.Svc.ProcessWorkflowMigrations(ctx, 10, 10); n != 0 {
		t.Fatalf("%d", n)
	}
}

func TestLIFE03_TenThousandIssuesMigrate(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	e := testutil.New(t)
	e.Project()
	wf := standardWF(t, e)
	seedInStatus(t, e, "In Progress", 10000)
	d := draftOf(t, e, wf)
	var st []domain.WStatus
	for _, s := range d.Statuses {
		if s.Name != "In Progress" {
			st = append(st, s)
		}
	}
	d.Statuses = st
	var tr []domain.WTransition
	for _, x := range d.Transitions {
		if x.From != "In Progress" && x.To != "In Progress" {
			tr = append(tr, x)
		}
	}
	d.Transitions = tr
	e.Svc.SaveWorkflowDraft(ctx, e.Admin, wf, d)
	if _, err := e.Svc.PublishWorkflow(ctx, e.Admin, wf, map[string]string{"In Progress": "Done"}, ""); err != nil {
		t.Fatal(err)
	}
	moved, err := e.Svc.ProcessWorkflowMigrations(ctx, 500, 100)
	if err != nil || moved != 10000 {
		t.Fatalf("%d %v", moved, err)
	}
	var open, resolved int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE status_id NOT IN (SELECT id FROM status)`).Scan(&open)
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue i JOIN status s ON s.id=i.status_id WHERE s.name='Done' AND i.resolved_at IS NOT NULL`).Scan(&resolved)
	if open != 0 || resolved != 10000 {
		t.Fatalf("%d dangling, %d resolved", open, resolved)
	}
}

func TestLIFE03_TemplatesImportExport(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	for _, name := range domain.WorkflowTemplateNames() {
		w, err := e.Svc.CreateWorkflow(ctx, e.Admin, "T "+name, name, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, err := e.Svc.ExportWorkflow(ctx, e.Admin, w.ID)
		if err != nil || len(d.Statuses) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		again, err := e.Svc.CreateWorkflow(ctx, e.Admin, "Copy of "+name, "", &d)
		if err != nil {
			t.Fatalf("import %s: %v", name, err)
		}
		d2, _ := e.Svc.ExportWorkflow(ctx, e.Admin, again.ID)
		a, b := fmt.Sprintf("%+v", d), fmt.Sprintf("%+v", d2)
		if a != b {
			t.Errorf("%s does not round trip:\n%s\n%s", name, a, b)
		}
	}
	if _, err := e.Svc.CreateWorkflow(ctx, e.Admin, "T simple", "simple", nil); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := e.Svc.CreateWorkflow(ctx, e.Admin, "x", "nonsense", nil); err == nil {
		t.Fatal("unknown template accepted")
	}
	if _, err := e.Svc.CreateWorkflow(ctx, e.Alice, "y", "simple", nil); err == nil {
		t.Fatal("a member created a workflow")
	}
}

func TestLIFE03_SwapWorkflowOnlyForAnEmptyProject(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	w, err := e.Svc.CreateWorkflow(ctx, e.Admin, "Other", "software", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.SetProjectWorkflow(ctx, e.Admin, "ABC", w.ID); err != nil {
		t.Fatal(err)
	}
	key := mk(t, e, e.Alice, "Task", "first", "")
	if _, ok := trans(t, e, e.Alice, key, "Start progress"); !ok {
		t.Fatal("the new workflow's transition is not offered")
	}
	if _, ok := trans(t, e, e.Alice, key, "Resolve"); ok {
		t.Fatal("the project still uses the old workflow")
	}
	if err := e.Svc.SetProjectWorkflow(ctx, e.Admin, "ABC", w.ID); err == nil || !strings.Contains(err.Error(), "edit its workflow instead") {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.SetProjectWorkflow(ctx, e.Alice, "ABC", w.ID); err == nil {
		t.Fatal("a member swapped a workflow")
	}
}
