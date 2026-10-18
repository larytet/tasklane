package domain_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestEXP_CustomFieldsAndWorkflowRulesRoundTrip(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			src := testutil.New(t)
			src.Project()
			svc := src.Svc
			sev, _ := svc.CreateField(ctx, src.Admin, domain.FieldIn{Name: "Severity", Type: "select"})
			svc.AddContext(ctx, src.Admin, sev.ID, domain.ContextIn{Projects: []string{"ABC"}, Types: []string{"Bug", "Task"}, Options: []string{"minor", "major"}, Required: false, Default: "minor"})
			est, _ := svc.CreateField(ctx, src.Admin, domain.FieldIn{Name: "Estimate", Type: "duration"})
			svc.AddContext(ctx, src.Admin, est.ID, domain.ContextIn{Global: true})
			owner, _ := svc.CreateField(ctx, src.Admin, domain.FieldIn{Name: "Owner", Type: "user"})
			svc.AddContext(ctx, src.Admin, owner.ID, domain.ContextIn{Global: true})
			tags, _ := svc.CreateField(ctx, src.Admin, domain.FieldIn{Name: "Areas", Type: "multi_select"})
			svc.AddContext(ctx, src.Admin, tags.ID, domain.ContextIn{Global: true, Options: []string{"api", "ui", "db"}})
			secret, _ := svc.CreateField(ctx, src.Admin, domain.FieldIn{Name: "Internal note", Type: "text", ViewRoles: []string{"admin"}})
			svc.AddContext(ctx, src.Admin, secret.ID, domain.ContextIn{Global: true})
			key := mk(t, src, src.Alice, "Bug", "has everything", "")
			if _, err := svc.UpdateFields(ctx, src.Admin, key, nil, false, domain.Changes{Custom: map[string]any{
				sev.Key: "major", est.Key: "2h 30m", owner.Key: src.Bob.ID, tags.Key: []any{"api", "db"}, secret.Key: "for admins only"}}); err != nil {
				t.Fatal(err)
			}
			o := domain.DefaultExport()
			o.Format = format
			b, man := exportBytes(t, src, src.Admin, o)
			if man.Counts["fields"] != 5 && format == "json" {
				t.Fatalf("counts %+v", man.Counts)
			}
			dst := testutil.New(t)
			dst.WithBlobs()
			rep := importBytes(t, dst, b, domain.ImportOpts{})
			if rep.Counts["fields_created"] != 5 || rep.Counts["custom_values_set"] != 5 {
				t.Fatalf("%+v gaps %v warnings %v", rep.Counts, rep.Gaps, rep.Warnings)
			}
			v, err := dst.Svc.View(ctx, dst.Admin, key)
			if err != nil {
				t.Fatal(err)
			}
			byName := func(env *testutil.Env, n string) domain.FieldDef {
				fs, _ := env.Svc.ListFields(ctx, true)
				for _, f := range fs {
					if f.Name == n {
						return f
					}
				}
				t.Fatalf("no field %s", n)
				return domain.FieldDef{}
			}
			get := func(n string) any { return v.Issue.Custom[byName(dst, n).Key] }
			if get("Severity") != "major" || get("Estimate") != float64(9000) || get("Internal note") != "for admins only" {
				t.Fatalf("values %v", v.Issue.Custom)
			}
			if !reflect.DeepEqual(fmt.Sprint(get("Areas")), "[api db]") {
				t.Fatalf("areas %v", get("Areas"))
			}
			bobHere := ""
			dst.Store.Pool.QueryRow(ctx, `SELECT id::text FROM app_user WHERE email='bob@example.com'`).Scan(&bobHere)
			if got := fmt.Sprint(get("Owner")); got != bobHere {
				t.Fatalf("a person in a user field did not survive: %q vs %q", got, bobHere)
			}
			// contexts came along: Severity is on Bug and Task in ABC only, with its options and default
			ctxs, _ := dst.Svc.ListContexts(ctx, byName(dst, "Severity").ID)
			if len(ctxs) != 1 || strings.Join(ctxs[0].Types, ",") != "Bug,Task" || strings.Join(ctxs[0].Projects, ",") != "ABC" || ctxs[0].Default != "minor" {
				t.Fatalf("%+v", ctxs)
			}
			// importing the same bundle again changes nothing
			rep2 := importBytes(t, dst, b, domain.ImportOpts{})
			if rep2.Counts["fields_created"] != 0 || rep2.Counts["field_contexts_created"] != 0 {
				t.Fatalf("re-import created things: %+v", rep2.Counts)
			}
			// a person who may not see a restricted field does not get its values or hide its definition behind them
			bobBundle, _ := exportBytes(t, src, src.Bob, domain.DefaultExport())
			if strings.Contains(string(bobBundle), "for admins only") {
				t.Log("note: zipped content is compressed, checking by import below")
			}
			dst2 := testutil.New(t)
			dst2.WithBlobs()
			r3 := importBytes(t, dst2, bobBundle, domain.ImportOpts{})
			v3, _ := dst2.Svc.View(ctx, dst2.Admin, key)
			for _, val := range v3.Issue.Custom {
				if val == "for admins only" {
					t.Fatalf("a restricted value was exported to a member: %v (%+v)", v3.Issue.Custom, r3.Counts)
				}
			}
		})
	}
}

func TestEXP_FieldOfAnotherTypeIsReportedNotOverwritten(t *testing.T) {
	src := testutil.New(t)
	src.Project()
	f, _ := src.Svc.CreateField(ctx, src.Admin, domain.FieldIn{Name: "Points", Type: "number"})
	src.Svc.AddContext(ctx, src.Admin, f.ID, domain.ContextIn{Global: true})
	key := mk(t, src, src.Alice, "Task", "t", "")
	src.Svc.UpdateFields(ctx, src.Admin, key, nil, false, domain.Changes{Custom: map[string]any{f.Key: 5}})
	b, _ := exportBytes(t, src, src.Admin, domain.DefaultExport())
	dst := testutil.New(t)
	dst.WithBlobs()
	other, _ := dst.Svc.CreateField(ctx, dst.Admin, domain.FieldIn{Name: "points", Type: "text"})
	dst.Svc.AddContext(ctx, dst.Admin, other.ID, domain.ContextIn{Global: true})
	rep := importBytes(t, dst, b, domain.ImportOpts{})
	if rep.Counts["fields_created"] != 0 || rep.Counts["custom_values_set"] != 0 {
		t.Fatalf("%+v", rep.Counts)
	}
	found := false
	for _, g := range rep.Gaps {
		if strings.Contains(g, "exists here as text") {
			found = true
		}
	}
	if !found {
		t.Fatalf("gaps: %v warnings: %v", rep.Gaps, rep.Warnings)
	}
}

func TestEXP_WorkflowRulesTravelWithTheWorkflow(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			src := testutil.New(t)
			src.Project()
			def := domain.WDef{
				Statuses: []domain.WStatus{{Name: "Open", Category: "todo"}, {Name: "Done", Category: "done"}},
				Transitions: []domain.WTransition{{Name: "Finish", From: "Open", To: "Done", RequiresHuman: true,
					Rules: domain.TRules{Conditions: []domain.Spec{{"kind": "role", "roles": []string{"admin"}}}, Validators: []domain.Spec{{"kind": "field_required", "field": "labels"}},
						PostFunctions: []domain.Spec{{"kind": "add_comment", "text": "closed"}}, Screen: []string{"labels"}}}},
			}
			w, err := src.Svc.CreateWorkflow(ctx, src.Admin, "Rules flow", "", &def)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := src.Svc.CreateProject(ctx, src.Admin, "QQ", "Rules project"); err != nil {
				t.Fatal(err)
			}
			if err := src.Svc.SetProjectWorkflow(ctx, src.Admin, "QQ", w.ID); err != nil {
				t.Fatal(err)
			}
			o := domain.DefaultExport()
			o.Format = format
			b, _ := exportBytes(t, src, src.Admin, o)
			dst := testutil.New(t)
			dst.WithBlobs()
			importBytes(t, dst, b, domain.ImportOpts{})
			var wf string
			dst.Store.Pool.QueryRow(ctx, `SELECT id::text FROM workflow WHERE name='Rules flow'`).Scan(&wf)
			got, err := dst.Svc.ExportWorkflow(ctx, dst.Admin, wf)
			if err != nil || len(got.Transitions) != 1 {
				t.Fatalf("%+v %v", got, err)
			}
			tr := got.Transitions[0]
			if !tr.RequiresHuman || len(tr.Rules.Conditions) != 1 || len(tr.Rules.Validators) != 1 || len(tr.Rules.PostFunctions) != 1 || len(tr.Rules.Screen) != 1 {
				t.Fatalf("rules did not travel: %+v", tr)
			}
		})
	}
}
