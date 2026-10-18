package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func resolveIssue(t *testing.T, r *fieldRig, key string) {
	t.Helper()
	id, ok := trans(t, r.e, r.e.Alice, key, "Resolve")
	if !ok {
		t.Fatal("no Resolve transition")
	}
	if _, err := r.e.Svc.Transition(ctx, r.e.Alice, key, id, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLIFE12_ArchiveIsReadOnlyHiddenFindableAndReversible(t *testing.T) {
	r := newFields(t)
	e := r.e
	done := mk(t, e, e.Alice, "Task", "finished work", "")
	open := mk(t, e, e.Alice, "Task", "still open", "")
	// only finished work is archived
	if _, err := e.Svc.ArchiveIssue(ctx, e.Alice, open); err == nil || !strings.Contains(err.Error(), "only finished work") {
		t.Fatalf("%v", err)
	}
	resolveIssue(t, r, done)
	if _, err := e.Svc.ArchiveIssue(ctx, e.Alice, done); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ArchiveIssue(ctx, e.Alice, done); err == nil || !strings.Contains(err.Error(), "already archived") {
		t.Fatalf("%v", err)
	}
	v, err := e.Svc.View(ctx, e.Alice, done)
	if err != nil || v.Issue.Archived == nil || v.Role != "viewer" {
		t.Fatalf("an archived issue is shown read-only: %+v %v", v.Role, err)
	}
	// read-only on every write path
	hi := "high"
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, done, nil, false, domain.Changes{Priority: &hi}); err == nil {
		t.Fatal("an archived issue was edited")
	}
	if _, err := e.Svc.AddComment(ctx, e.Alice, done, "late comment", false); err == nil {
		t.Fatal("an archived issue was commented on")
	}
	if tid, ok := trans(t, e, e.Alice, done, "Reopen"); ok {
		if _, err := e.Svc.Transition(ctx, e.Alice, done, tid, nil); err == nil {
			t.Fatal("an archived issue was transitioned")
		}
	}
	if _, err := e.Svc.SaveText(ctx, e.Alice, done, domain.SaveIn{Field: "description", Body: "changed", BaseSeq: 1}); err == nil {
		t.Fatal("an archived issue's text was saved")
	}
	for _, op := range []string{"move", "convert"} {
		if _, err := e.Svc.Lifecycle(ctx, e.Alice, done, domain.LifecycleIn{Op: op, Project: "ABC", Type: "Bug"}); err == nil || !strings.Contains(err.Error(), "archived") {
			t.Fatalf("%s: %v", op, err)
		}
	}
	// hidden by default, found by asking
	if got := r.keys(t, e.Alice, `project = ABC`); strings.Contains(got, done) || !strings.Contains(got, open) {
		t.Fatalf("default search: %q", got)
	}
	for q, want := range map[string]string{
		`archived = true`:                  done,
		`archived = false`:                 open,
		`archived is not EMPTY`:            done,
		`archived is EMPTY`:                open,
		`archived = yes AND project = ABC`: done,
		`archived != true`:                 open,
	} {
		if got := r.keys(t, e.Alice, q); got != want {
			t.Errorf("%q: got %q, want %q", q, got, want)
		}
	}
	if got := r.keys(t, e.Alice, `archived = true OR archived = false`); got != done+" "+open && got != open+" "+done {
		t.Errorf("both: %q", got)
	}
	if err := r.tryKeys(e.Alice, `archived = maybe`); err == nil {
		t.Error("a bad archived value was accepted")
	}
	// the board and the backlog leave it out, typeahead too, but its key still opens
	b, _ := e.Svc.CreateBoard(ctx, e.Admin, "ABC", "Main", "kanban")
	bv, err := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bv.Columns {
		for _, card := range c.Cards {
			if card.Key == done {
				t.Fatal("an archived issue is on the board")
			}
		}
	}
	// reversible, by a member
	if _, err := e.Svc.UnarchiveIssue(ctx, e.Bob, done); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.UnarchiveIssue(ctx, e.Bob, done); err == nil {
		t.Fatal("unarchiving twice succeeded")
	}
	if got := r.keys(t, e.Alice, `project = ABC`); !strings.Contains(got, done) {
		t.Fatalf("restored issue missing: %q", got)
	}
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, done, nil, false, domain.Changes{Priority: &hi}); err != nil {
		t.Fatalf("a restored issue cannot be edited: %v", err)
	}
	// history says who did what
	h, _ := e.Svc.History(ctx, e.Alice, done)
	var seen []string
	for _, x := range h {
		if x.Field == "archived" {
			seen = append(seen, x.Actor)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("history: %v", seen)
	}
}

func TestLIFE12_ViewersAndOutsidersCannotArchive(t *testing.T) {
	r := newFields(t)
	e := r.e
	key := mk(t, e, e.Alice, "Task", "x", "")
	resolveIssue(t, r, key)
	viewer := e.NewUser("viewer@example.com", "Vera", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	if _, err := e.Svc.ArchiveIssue(ctx, viewer, key); err == nil {
		t.Fatal("a viewer archived")
	}
	out := e.NewUser("out@example.com", "Out", false)
	if _, err := e.Svc.ArchiveIssue(ctx, out, key); err == nil || err != domain.ErrNotFound {
		t.Fatalf("an outsider learned something: %v", err)
	}
	e.Svc.ArchiveIssue(ctx, e.Alice, key)
	if _, err := e.Svc.UnarchiveIssue(ctx, viewer, key); err == nil {
		t.Fatal("a viewer unarchived")
	}
}

func TestLIFE12_AutoArchiveByProjectPolicy(t *testing.T) {
	r := newFields(t)
	e := r.e
	old := mk(t, e, e.Alice, "Task", "resolved long ago", "")
	recent := mk(t, e, e.Alice, "Task", "resolved yesterday", "")
	open := mk(t, e, e.Alice, "Task", "open", "")
	resolveIssue(t, r, old)
	resolveIssue(t, r, recent)
	e.Store.Pool.Exec(ctx, `UPDATE issue SET resolved_at=$2 WHERE key=$1`, old, e.Svc.Now().AddDate(0, 0, -40))
	e.Store.Pool.Exec(ctx, `UPDATE issue SET resolved_at=$2 WHERE key=$1`, recent, e.Svc.Now().AddDate(0, 0, -1))
	// off by default
	if n, err := e.Svc.AutoArchive(ctx, 100); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	zero := 0
	if err := e.Svc.SetArchivePolicy(ctx, e.Admin, "ABC", &zero); err == nil {
		t.Fatal("0 days accepted")
	}
	if err := e.Svc.SetArchivePolicy(ctx, e.Alice, "ABC", nil); err == nil {
		t.Fatal("a member set the policy")
	}
	days := 30
	if err := e.Svc.SetArchivePolicy(ctx, e.Admin, "ABC", &days); err != nil {
		t.Fatal(err)
	}
	if n, err := e.Svc.AutoArchive(ctx, 100); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := r.keys(t, e.Alice, `archived = true`); got != old {
		t.Fatalf("archived: %q", got)
	}
	// a day later than the recent one's 30th day it follows; idempotent in between
	e.Clock.T = e.Clock.T.AddDate(0, 0, 31)
	if n, _ := e.Svc.AutoArchive(ctx, 100); n != 1 {
		t.Fatalf("%d", n)
	}
	if n, _ := e.Svc.AutoArchive(ctx, 100); n != 0 {
		t.Fatalf("%d", n)
	}
	if got := r.keys(t, e.Alice, `archived = true`); !strings.Contains(got, recent) || strings.Contains(got, open) {
		t.Fatalf("%q", got)
	}
	d, _ := e.Svc.ArchivePolicy(ctx, "ABC")
	if d == nil || *d != 30 {
		t.Fatalf("%v", d)
	}
	_ = time.Now
}

func TestLIFE12_ArchiveSurvivesExportAndImport(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			src := testutil.New(t)
			src.Project()
			key := mk(t, src, src.Alice, "Task", "archived and exported", "")
			id, _ := trans(t, src, src.Alice, key, "Resolve")
			src.Svc.Transition(ctx, src.Alice, key, id, nil)
			if _, err := src.Svc.ArchiveIssue(ctx, src.Alice, key); err != nil {
				t.Fatal(err)
			}
			o := domain.DefaultExport()
			o.Format = format
			b, _ := exportBytes(t, src, src.Admin, o)
			dst := testutil.New(t)
			dst.WithBlobs()
			importBytes(t, dst, b, domain.ImportOpts{})
			v, err := dst.Svc.View(ctx, dst.Admin, key)
			if err != nil || v.Issue.Archived == nil {
				t.Fatalf("archived state lost in %s: %+v %v", format, v.Issue.Archived, err)
			}
		})
	}
}
