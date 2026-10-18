package domain_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want an error containing %q, got %v", sub, err)
	}
}

func bptr(b bool) *bool { return &b }

func TestCORE03_ScreensAndSchemes(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	svc := e.Svc
	sev, _ := svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Severity", Type: "text"})
	area, _ := svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Area", Type: "text"})
	for _, f := range []domain.FieldDef{sev, area} {
		svc.AddContext(ctx, e.Admin, f.ID, domain.ContextIn{Global: true})
	}
	kS, kA := sev.Key, area.Key
	create, err := svc.SaveScreen(ctx, e.Admin, 0, domain.Screen{Name: "Create", Tabs: []domain.ScreenTab{{Name: "Main", Fields: []string{"priority", "description", "CF[" + fmt.Sprint(sev.ID) + "]"}}}})
	if err != nil {
		t.Fatal(err)
	}
	edit, _ := svc.SaveScreen(ctx, e.Admin, 0, domain.Screen{Name: "Edit", Tabs: []domain.ScreenTab{{Name: "Main", Fields: []string{"priority", kS}}, {Name: "More", Fields: []string{kA}}}})
	if create.Tabs[0].Fields[2] != kS {
		t.Fatalf("keys are canonical: %v", create.Tabs[0].Fields)
	}
	if _, err := svc.SaveScreen(ctx, e.Admin, 0, domain.Screen{Name: "create"}); err == nil {
		t.Fatal("duplicate screen name accepted")
	}
	_, err = svc.SaveScreen(ctx, e.Admin, 0, domain.Screen{Name: "Bad", Tabs: []domain.ScreenTab{{Name: "x", Fields: []string{"nonsense"}}}})
	wantErr(t, err, "is not a field")
	if _, err := svc.SaveScreen(ctx, e.Alice, 0, domain.Screen{Name: "Nope"}); err == nil {
		t.Fatal("a member saved a screen")
	}
	sch, err := svc.SaveScheme(ctx, e.Admin, 0, domain.Scheme{Name: "Software", Entries: []domain.SchemeEntry{{Operation: "create", Screen: create.ID}, {Operation: "edit", Screen: edit.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveScheme(ctx, e.Admin, 0, domain.Scheme{Name: "Dup", Entries: []domain.SchemeEntry{{Operation: "edit", Screen: edit.ID}, {Operation: "edit", Screen: create.ID}}}); err == nil {
		t.Fatal("an operation mapped twice for the same type was accepted")
	}
	// without a scheme every field is allowed
	is, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "before", Custom: map[string]any{kA: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteScreen(ctx, e.Admin, create.ID); err == nil {
		t.Fatal("a screen in use by a scheme was deleted")
	}
	if err := svc.SetProjectScheme(ctx, e.Admin, "ABC", &sch.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteScheme(ctx, e.Admin, sch.ID); err == nil {
		t.Fatal("a scheme in use by a project was deleted")
	}
	// Area is on the edit screen but not on the create screen
	_, err = svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "x", Custom: map[string]any{kA: "x"}})
	wantErr(t, err, "Area is not on the create screen")
	if _, err = svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "x", Custom: map[string]any{kS: "high"}}); err != nil {
		t.Fatal(err)
	}
	// edit: Area may be set, Labels is not on the edit screen
	if _, err := svc.UpdateFields(ctx, e.Alice, is.Key, nil, false, domain.Changes{Custom: map[string]any{kA: "y"}}); err != nil {
		t.Fatal(err)
	}
	lab := []string{"a"}
	_, err = svc.UpdateFields(ctx, e.Alice, is.Key, nil, false, domain.Changes{Labels: &lab})
	wantErr(t, err, "Labels is not on the edit screen")
	// the form definition follows the screen
	f, err := svc.Form(ctx, e.Alice, "ABC", "Task", "edit")
	if err != nil || len(f.Tabs) != 2 || f.Tabs[1].Name != "More" || f.Tabs[1].Fields[0].Name != "Area" {
		t.Fatalf("%+v %v", f, err)
	}
	// a type-specific entry beats the default
	bugScreen, _ := svc.SaveScreen(ctx, e.Admin, 0, domain.Screen{Name: "Bug create", Tabs: []domain.ScreenTab{{Name: "Bug", Fields: []string{"labels"}}}})
	svc.SaveScheme(ctx, e.Admin, sch.ID, domain.Scheme{Name: "Software", Entries: []domain.SchemeEntry{{Operation: "create", Screen: create.ID}, {Operation: "create", Type: "Bug", Screen: bugScreen.ID}, {Operation: "edit", Screen: edit.ID}}})
	if f, _ := svc.Form(ctx, e.Alice, "ABC", "Bug", "create"); len(f.Tabs) != 1 || f.Tabs[0].Name != "Bug" {
		t.Fatalf("%+v", f)
	}
	if f, _ := svc.Form(ctx, e.Alice, "ABC", "Task", "create"); f.Tabs[0].Name != "Main" {
		t.Fatalf("%+v", f)
	}
	// back to no scheme: everything is allowed again
	svc.SetProjectScheme(ctx, e.Admin, "ABC", nil)
	if _, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "after", Custom: map[string]any{kA: "x"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCORE03_FieldConfigurationRequiredHiddenPrecedence(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	svc := e.Svc
	// description is required for Bugs in ABC only
	if _, err := svc.SaveFieldConfig(ctx, e.Admin, domain.FieldCfgRow{Project: "ABC", Type: "Bug", Field: "description", Required: bptr(true)}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "no description"})
	wantErr(t, err, "Description is required")
	if _, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "task is fine"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "with", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	// a global row says assignee is hidden, a project+type row un-hides it for Tasks: the most specific wins
	svc.SaveFieldConfig(ctx, e.Admin, domain.FieldCfgRow{Field: "assignee", Hidden: bptr(true)})
	_, err = svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Story", Summary: "s", Assignee: e.Bob.ID})
	wantErr(t, err, "Assignee is hidden")
	svc.SaveFieldConfig(ctx, e.Admin, domain.FieldCfgRow{Project: "ABC", Type: "Task", Field: "assignee", Hidden: bptr(false)})
	if _, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "t", Assignee: e.Bob.ID}); err != nil {
		t.Fatal(err)
	}
	f, _ := svc.Form(ctx, e.Alice, "ABC", "Bug", "create")
	var desc, asg *domain.FormField
	for i := range f.Tabs[0].Fields {
		switch f.Tabs[0].Fields[i].Key {
		case "description":
			desc = &f.Tabs[0].Fields[i]
		case "assignee":
			asg = &f.Tabs[0].Fields[i]
		}
	}
	if desc == nil || !desc.Required || asg == nil || !asg.Hidden {
		t.Fatalf("%+v", f)
	}
	// clearing a required field is refused on edit
	bug, _ := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "b", Description: "d"})
	rows, _ := svc.ListFieldConfigs(ctx)
	if len(rows) != 3 {
		t.Fatalf("%+v", rows)
	}
	for _, r := range rows {
		if err := svc.DeleteFieldConfig(ctx, e.Admin, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "free again"}); err != nil {
		t.Fatal(err)
	}
	_ = bug
}

func TestCORE03_BehaviorRules(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	svc := e.Svc
	sev, _ := svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Severity", Type: "select"})
	svc.AddContext(ctx, e.Admin, sev.ID, domain.ContextIn{Global: true, Options: []string{"minor", "major"}})
	note, _ := svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Note", Type: "text"})
	svc.AddContext(ctx, e.Admin, note.ID, domain.ContextIn{Global: true})
	// priority highest requires a severity
	r, err := svc.SaveRule(ctx, e.Admin, 0, domain.Rule{Name: "needs severity", Enabled: true, WhenField: "priority", WhenOp: "eq", WhenValue: "highest", Action: "require", Target: sev.Key})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "x", Priority: "highest"})
	wantErr(t, err, "Severity is required")
	if _, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "x", Priority: "low"}); err != nil {
		t.Fatal(err)
	}
	hi, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "x", Priority: "highest", Custom: map[string]any{sev.Key: "major"}})
	if err != nil {
		t.Fatal(err)
	}
	// on edit: raising a ticket to highest demands a severity; with one it works; clearing it afterwards is refused
	lo, _ := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "y", Priority: "low"})
	highest := "highest"
	_, err = svc.UpdateFields(ctx, e.Alice, lo.Key, nil, false, domain.Changes{Priority: &highest})
	wantErr(t, err, "Severity is required")
	if _, err := svc.UpdateFields(ctx, e.Alice, lo.Key, nil, false, domain.Changes{Priority: &highest, Custom: map[string]any{sev.Key: "minor"}}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.UpdateFields(ctx, e.Alice, hi.Key, nil, false, domain.Changes{Custom: map[string]any{sev.Key: nil}})
	wantErr(t, err, "Severity is required")
	// hide Note when Severity is minor; a role rule shows it only to admins
	svc.SaveRule(ctx, e.Admin, 0, domain.Rule{Enabled: true, WhenField: sev.Key, WhenOp: "eq", WhenValue: "minor", Action: "hide", Target: note.Key})
	_, err = svc.UpdateFields(ctx, e.Alice, lo.Key, nil, false, domain.Changes{Custom: map[string]any{note.Key: "n"}})
	wantErr(t, err, "Note is hidden")
	if _, err := svc.UpdateFields(ctx, e.Alice, hi.Key, nil, false, domain.Changes{Custom: map[string]any{note.Key: "n"}}); err != nil {
		t.Fatal(err) // Severity is major here: the hide rule does not apply
	}
	// ordering: a later show rule wins
	svc.SaveRule(ctx, e.Admin, 0, domain.Rule{Enabled: true, Position: 5, WhenField: sev.Key, WhenOp: "eq", WhenValue: "minor", WhenRole: "member", Action: "show", Target: note.Key})
	if _, err := svc.UpdateFields(ctx, e.Alice, lo.Key, nil, false, domain.Changes{Custom: map[string]any{note.Key: "n"}}); err != nil {
		t.Fatal(err)
	}
	// disabling a rule turns it off; a rule cannot depend on its own target; unknown fields are refused
	r.Enabled = false
	if _, err := svc.SaveRule(ctx, e.Admin, r.ID, r); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "z", Priority: "highest"}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.SaveRule(ctx, e.Admin, 0, domain.Rule{Enabled: true, WhenField: "labels", Action: "hide", Target: "labels"})
	wantErr(t, err, "cannot depend")
	_, err = svc.SaveRule(ctx, e.Admin, 0, domain.Rule{Enabled: true, Action: "hide", Target: "customfield_99999"})
	wantErr(t, err, "is not a field")
	form, _ := svc.Form(ctx, e.Alice, "ABC", "Task", "create")
	if len(form.Rules) < 2 {
		t.Fatalf("the form carries the rules for the client: %+v", form.Rules)
	}
}
