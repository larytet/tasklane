package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

const csvFixture = "\xef\xbb\xbfID,Title,Details,Kind,State,Prio,Owner,Tags,Opened,Closed,Parent,Note 1,Note 2\r\n" +
	"T-1,\"Login fails, sometimes\",\"Line one\nline two, with a comma\",Defect,Open,P1,alice@example.com,\"auth;urgent\",2024-03-01,,,first note,second note\r\n" +
	"T-2,Add export,,Feature,Closed,P3,newperson@example.com,export,2024-03-02 10:30,2024-03-05,,,\r\n" +
	"T-3,Subtask of one,,Sub,Open,,,,2024-03-03,,T-1,,\r\n" +
	"T-4,,no summary,Defect,Open,P2,,,2024-03-03,,,,\r\n" +
	"T-5,Bad date,,Defect,Open,P2,,,not-a-date,,,,\r\n"

func csvMap() domain.CSVMapping {
	return domain.CSVMapping{
		Columns: map[string]string{"ID": "key", "Title": "summary", "Details": "description", "Kind": "type", "State": "status", "Prio": "priority", "Owner": "assignee", "Tags": "labels", "Opened": "created", "Closed": "resolved", "Parent": "parent", "Note 1": "comment", "Note 2": "comment"},
		TypeMap: map[string]string{"Defect": "Bug", "Feature": "Story", "Sub": "Subtask"}, StatusMap: map[string]string{"Open": "To Do", "Closed": "Done"},
		PriorityMap: map[string]string{"P1": "highest", "P2": "high", "P3": "low"},
	}
}

func TestIMP09_GenericCSVWithMapping(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	existing := mk(t, e, e.Alice, "Task", "already here", "") // adding to a project that already has issues
	in := domain.CSVImportIn{Project: "ABC", Mapping: csvMap(), DryRun: true}
	dry, err := e.Svc.ImportCSV(ctx, e.Admin, []byte(csvFixture), in)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Counts["issues_created"] != 3 || dry.Counts["rows_rejected"] != 2 {
		t.Fatalf("dry run: %+v %v", dry.Counts, dry.Errors)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n)
	if n != 1 {
		t.Fatalf("a dry run committed %d issues", n-1)
	}
	in.DryRun = false
	rep, err := e.Svc.ImportCSV(ctx, e.Admin, []byte(csvFixture), in)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts["issues_created"] != dry.Counts["issues_created"] || rep.Counts["comments_created"] != 2 {
		t.Fatalf("%+v", rep.Counts)
	}
	// rejected rows are listed with their row number and reason, and as a CSV to fix and re-run
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "row 5: the summary is empty") || !strings.Contains(joined, "row 6: created: cannot read") || !strings.HasPrefix(rep.ErrorCSV, "row,problem,ID,Title") {
		t.Fatalf("errors: %v\n%s", rep.Errors, rep.ErrorCSV)
	}
	k := rep.KeyMap["T-1"]
	v, err := e.Svc.View(ctx, e.Admin, k)
	if err != nil {
		t.Fatal(err)
	}
	i := v.Issue
	if i.Summary != "Login fails, sometimes" || i.Description != "Line one\nline two, with a comma" || i.Type != "Bug" || i.Priority != "highest" || i.Status != "To Do" ||
		i.Assignee == nil || i.Assignee.Email != "alice@example.com" || strings.Join(i.Labels, ",") != "auth,urgent" || i.Created.UTC().Format("2006-01-02") != "2024-03-01" || len(v.Comments) != 2 {
		t.Fatalf("%+v comments %d", i, len(v.Comments))
	}
	if v2, _ := e.Svc.View(ctx, e.Admin, rep.KeyMap["T-2"]); v2.Issue.Status != "Done" || v2.Issue.Resolved == nil || v2.Issue.Created.UTC().Format("2006-01-02 15:04") != "2024-03-02 10:30" ||
		v2.Issue.Assignee == nil || v2.Issue.Assignee.Email != "newperson@example.com" {
		t.Fatalf("%+v", v2.Issue)
	}
	if sub, _ := e.Svc.View(ctx, e.Admin, rep.KeyMap["T-3"]); sub.Issue.Parent != k || sub.Issue.Type != "Subtask" { // the parent link follows the file's own keys
		t.Fatalf("%+v", sub.Issue)
	}
	if !strings.HasSuffix(k, "-2") || existing != "ABC-1" { // numbering continued after the existing issue
		t.Fatalf("numbering did not continue after the existing issue: %s", k)
	}
	// idempotent: the same file again changes nothing
	again, _ := e.Svc.ImportCSV(ctx, e.Admin, []byte(csvFixture), in)
	if again.Counts["issues_created"] != 0 || again.Counts["comments_created"] != 0 || again.Counts["issues_updated"] != 0 {
		t.Fatalf("re-import: %+v", again.Counts)
	}
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n)
	if n != 4 {
		t.Fatalf("%d issues", n)
	}
	// the unknown owner became an inactive placeholder
	var active bool
	e.Store.Pool.QueryRow(ctx, `SELECT active FROM app_user WHERE email='newperson@example.com'`).Scan(&active)
	if active {
		t.Fatal("a placeholder user is active")
	}
}

func TestIMP09_MappingValidationAndTemplates(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	bad := []struct {
		name string
		in   domain.CSVImportIn
		csv  string
	}{
		{"no summary column", domain.CSVImportIn{Project: "ABC", Mapping: domain.CSVMapping{Columns: map[string]string{"A": "description"}}}, "A\nx"},
		{"unknown field", domain.CSVImportIn{Project: "ABC", Mapping: domain.CSVMapping{Columns: map[string]string{"A": "summary", "B": "flavour"}}}, "A,B\nx,y"},
		{"column not in the file", domain.CSVImportIn{Project: "ABC", Mapping: domain.CSVMapping{Columns: map[string]string{"Nope": "summary"}}}, "A\nx"},
		{"unknown project", domain.CSVImportIn{Project: "NOPE", Mapping: domain.CSVMapping{Columns: map[string]string{"A": "summary"}}}, "A\nx"},
	}
	for _, c := range bad {
		if _, err := e.Svc.ImportCSV(ctx, e.Admin, []byte(c.csv), c.in); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	// members may not import; templates are saved once and reused
	if _, err := e.Svc.ImportCSV(ctx, e.Alice, []byte("A\nx"), domain.CSVImportIn{Project: "ABC", Mapping: domain.CSVMapping{Columns: map[string]string{"A": "summary"}}}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.SaveCSVTemplate(ctx, e.Admin, "tracker", csvMap()); err != nil {
		t.Fatal(err)
	}
	m, err := e.Svc.CSVTemplate(ctx, e.Admin, "tracker")
	if err != nil || m.Columns["Title"] != "summary" || m.PriorityMap["P1"] != "highest" {
		t.Fatalf("%v %+v", err, m)
	}
	if _, err := e.Svc.CSVTemplate(ctx, e.Admin, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if err := e.Svc.SaveCSVTemplate(ctx, e.Alice, "x", m); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.ImportCSV(ctx, e.Admin, []byte(csvFixture), domain.CSVImportIn{Project: "ABC", Mapping: m}); err != nil {
		t.Fatalf("a saved template did not import: %v", err)
	}
}

// A CSV export neutralizes formulas so a spreadsheet cannot run a cell as code, and the data still round-trips (SEC-05).
func TestSEC05_CSVExportFormulaInjection(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: `=HYPERLINK("http://evil","x")`, Description: "+cmd|' /C calc'!A0"})
	o := domain.DefaultExport()
	o.Format = "csv"
	b, _ := exportBytes(t, e, e.Admin, o)
	dst := testutil.New(t)
	dst.WithBlobs()
	importBytes(t, dst, b, domain.ImportOpts{})
	v, _ := dst.Svc.View(ctx, dst.Admin, "ABC-1")
	if v.Issue.Summary != `=HYPERLINK("http://evil","x")` || v.Issue.Description != "+cmd|' /C calc'!A0" {
		t.Fatalf("%q %q", v.Issue.Summary, v.Issue.Description)
	}
}
