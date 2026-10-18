package domain_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// rich builds a source project that uses most of the model.
func rich(t *testing.T, e *testutil.Env) {
	e.WithBlobs()
	e.Project()
	xyz(t, e)
	epic := mk(t, e, e.Alice, "Epic", "the epic", "")
	story := mk(t, e, e.Alice, "Story", "a story with everything", epic)
	sub := mk(t, e, e.Bob, "Subtask", "its subtask", story)
	bug := mk(t, e, e.Bob, "Bug", "a bug <b>with markup</b> & ünïcode 日本語", "")
	e.Svc.SaveText(ctx, e.Alice, story, domain.SaveIn{Field: "description", Body: "Steps:\n\n1. one\n\n2. two", BaseSeq: 1})
	e.Svc.AddComment(ctx, e.Bob, story, "a public comment", false)
	e.Svc.AddComment(ctx, e.Alice, story, "an internal note", true)
	e.Svc.AddLink(ctx, e.Alice, story, "blocks", bug)
	hi, bob := "high", e.Bob.ID
	lab := []string{"payments", "urgent"}
	e.Svc.UpdateFields(ctx, e.Alice, story, nil, false, domain.Changes{Priority: &hi, Assignee: &bob, Labels: &lab})
	v, _ := e.Svc.View(ctx, e.Alice, story)
	e.Svc.Transition(ctx, e.Alice, story, v.Transitions[0].ID, nil)
	e.Svc.AttachStream(ctx, e.Alice, story, "notes.txt", "text/plain", strings.NewReader("attachment bytes that must survive"))
	e.Svc.AttachStream(ctx, e.Bob, story, "shot.png", "image/png", bytes.NewReader(pngBytes(40, 30)))
	// an alias (a move) and removed values (a conversion that detached a child)
	moved := mk(t, e, e.Alice, "Task", "moved away", "")
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, moved, domain.LifecycleIn{Op: "move", Project: "XYZ"}); err != nil {
		t.Fatal(err)
	}
	conv := mk(t, e, e.Alice, "Task", "convertible", "")
	mk(t, e, e.Alice, "Subtask", "child of convertible", conv)
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, conv, domain.LifecycleIn{Op: "convert", Type: "Epic"}); err != nil {
		t.Fatal(err)
	}
	trashed := mk(t, e, e.Alice, "Task", "in the trash", "")
	e.Svc.Lifecycle(ctx, e.Alice, trashed, domain.LifecycleIn{Op: "delete"})
	_ = sub
}

func exportBytes(t *testing.T, e *testutil.Env, a domain.Actor, o domain.ExportOpts) ([]byte, *domain.BManifest) {
	t.Helper()
	var buf bytes.Buffer
	man, err := e.Svc.Export(ctx, a, &buf, o)
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), man
}

func importBytes(t *testing.T, e *testutil.Env, b []byte, o domain.ImportOpts) *domain.ImportReport {
	t.Helper()
	rep, err := e.Svc.Import(ctx, e.Admin, bytes.NewReader(b), int64(len(b)), o)
	if err != nil {
		t.Fatalf("import: %v (report %+v)", err, rep)
	}
	return rep
}

type snap struct {
	Issue       domain.Issue
	Comments    []string
	Links       []string
	Attachments []string
}

// snapshotOf reads an issue the way a user would, for comparing two instances.
func snapshotOf(t *testing.T, e *testutil.Env, a domain.Actor, key string) snap {
	t.Helper()
	v, err := e.Svc.View(ctx, a, key)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	s := snap{Issue: v.Issue}
	for _, c := range v.Comments {
		s.Comments = append(s.Comments, fmt.Sprintf("%s|%s|%v|%s", c.Author.Email, c.Body, c.Internal, c.Created.UTC().Format(time.RFC3339)))
	}
	for _, l := range v.Links {
		s.Links = append(s.Links, fmt.Sprintf("%s|%s|%s", l.Type, l.Dir, l.Key))
	}
	for _, at := range v.Attachments {
		s.Attachments = append(s.Attachments, fmt.Sprintf("%s|%s|%d", at.Name, at.SHA256, at.Size))
	}
	return s
}

func sameSnap(t *testing.T, key string, a, b snap) {
	t.Helper()
	x, y := a.Issue, b.Issue
	em := func(u *domain.User) string {
		if u == nil {
			return ""
		}
		return u.Email
	}
	if x.Key != y.Key || x.Summary != y.Summary || x.Description != y.Description || x.Status != y.Status || x.Priority != y.Priority || x.Type != y.Type ||
		strings.Join(x.Labels, ",") != strings.Join(y.Labels, ",") || em(x.Assignee) != em(y.Assignee) || em(x.Reporter) != em(y.Reporter) || x.Parent != y.Parent ||
		!x.Created.Equal(y.Created) || (x.Resolved == nil) != (y.Resolved == nil) {
		t.Errorf("%s differs:\n  source %+v\n  target %+v", key, x, y)
	}
	for name, p := range map[string][2][]string{"comments": {a.Comments, b.Comments}, "links": {a.Links, b.Links}, "attachments": {a.Attachments, b.Attachments}} {
		if strings.Join(p[0], "\n") != strings.Join(p[1], "\n") {
			t.Errorf("%s %s differ:\n  source %v\n  target %v", key, name, p[0], p[1])
		}
	}
}

func TestEXP01_RoundTripJSONAndCSV(t *testing.T) {
	for _, format := range []string{"json", "csv"} {
		t.Run(format, func(t *testing.T) {
			src := testutil.New(t)
			rich(t, src)
			o := domain.DefaultExport()
			o.Format = format
			o.History = true
			b, man := exportBytes(t, src, src.Admin, o)
			if man.Format != domain.BundleFormat || man.Version != 1 || man.Kind != format || man.Counts["issues"] != 7 {
				t.Fatalf("manifest %+v", man)
			}
			dst := testutil.New(t)
			dst.WithBlobs()
			rep := importBytes(t, dst, b, domain.ImportOpts{})
			if rep.Counts["issues_created"] != 7 { // ABC-1..4, 6, 7 and XYZ-1: ABC-5 moved away and ABC-8 is in the trash
				t.Fatalf("created %d: %+v", rep.Counts["issues_created"], rep)
			}
			keys := []string{"ABC-1", "ABC-2", "ABC-3", "ABC-4", "ABC-6", "ABC-7", "XYZ-1"}
			for _, k := range keys {
				a, b := snapshotOf(t, src, src.Admin, k), snapshotOf(t, dst, dst.Admin, k)
				sameSnap(t, k, a, b)
			}
			// attachment bytes are identical, verified by checksum
			v, _ := dst.Svc.View(ctx, dst.Admin, "ABC-2")
			if len(v.Attachments) != 2 {
				t.Fatalf("%d attachments", len(v.Attachments))
			}
			for _, at := range v.Attachments {
				f, err := dst.Svc.OpenBlob(at.SHA256)
				if err != nil {
					t.Fatal(err)
				}
				data, _ := io.ReadAll(f)
				f.Close()
				if at.Name == "notes.txt" && string(data) != "attachment bytes that must survive" {
					t.Fatalf("attachment bytes changed: %q", data)
				}
			}
			// the old key from the move resolves in the target, and so does the project the issue moved to
			if v, err := dst.Svc.View(ctx, dst.Admin, "ABC-5"); err != nil || v.Issue.Key != "XYZ-1" {
				t.Fatalf("alias in the target: %v %+v", err, v.Issue)
			}
			// removed values survive: converting the epic back restores its detached child, in the target
			plan, err := dst.Svc.Lifecycle(ctx, dst.Admin, "ABC-6", domain.LifecycleIn{Op: "convert", Type: "Task"})
			if err != nil || plan.Counts["restored"] != 1 {
				t.Fatalf("convert back in the target: %v %+v", err, plan)
			}
			// the key map is complete and the numbers are preserved, so new issues continue the sequence
			if next := mk(t, dst, dst.Alice, "Task", "continues", ""); next != "ABC-9" {
				t.Fatalf("next key %s", next)
			}
			// the trash was not exported by default
			if _, err := dst.Svc.View(ctx, dst.Admin, "ABC-9x"); err == nil {
				t.Fatal("unexpected issue")
			}
			if _, err := dst.Svc.View(ctx, dst.Admin, "ABC-7"); err != nil {
				t.Fatalf("ABC-7: %v", err)
			}
			var inTrash int
			dst.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE deleted_at IS NOT NULL`).Scan(&inTrash)
			if inTrash != 0 {
				t.Fatal("the trash was imported although it was not exported")
			}
		})
	}
}

func TestLIFE17_TrashOptionalAndHistoryRoundTrip(t *testing.T) {
	src := testutil.New(t)
	rich(t, src)
	o := domain.DefaultExport()
	o.Trash, o.History = true, true
	b, _ := exportBytes(t, src, src.Admin, o)
	dst := testutil.New(t)
	dst.WithBlobs()
	importBytes(t, dst, b, domain.ImportOpts{})
	var inTrash int
	dst.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE deleted_at IS NOT NULL`).Scan(&inTrash)
	if inTrash != 1 {
		t.Fatalf("%d trashed issues after import", inTrash)
	}
	// history and text versions came along; the next save continues the sequence
	h, _ := dst.Svc.History(ctx, dst.Admin, "ABC-2")
	sh, _ := src.Svc.History(ctx, src.Admin, "ABC-2")
	if len(h) < len(sh)-1 {
		t.Fatalf("history: source %d entries, target %d", len(sh), len(h))
	}
	vs, _ := dst.Svc.TextVersions(ctx, dst.Admin, "ABC-2", "description")
	svs, _ := src.Svc.TextVersions(ctx, src.Admin, "ABC-2", "description")
	if len(vs) != len(svs) {
		t.Fatalf("text versions: %d vs %d", len(vs), len(svs))
	}
	v, _ := dst.Svc.View(ctx, dst.Admin, "ABC-2")
	if _, err := dst.Svc.SaveText(ctx, dst.Admin, "ABC-2", domain.SaveIn{Field: "description", Body: "saved after the import", BaseSeq: v.Issue.DescSeq}); err != nil {
		t.Fatalf("a save after the import: %v", err)
	}
}

// PAUSE-01: a snapshot export taken while 50 users save contains every save acknowledged before the marker and nothing after it,
// and no writer is held.
func TestPAUSE01_SnapshotExportWhileUsersSave(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	key := mk(t, e, e.Alice, "Task", "busy", "")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var acked, refused int64
	for u := 0; u < 50; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			a := e.Alice
			if u%2 == 1 {
				a = e.Bob
			}
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				v, err := e.Svc.View(ctx, a, key)
				if err != nil {
					return
				}
				_, err = e.Svc.SaveText(ctx, a, key, domain.SaveIn{Field: "description", Body: fmt.Sprintf("%s[%d.%d]", v.Issue.Description, u, n), BaseSeq: v.Issue.DescSeq})
				var pe *domain.ErrPaused
				switch {
				case err == nil:
					atomic.AddInt64(&acked, 1)
				case errors.As(err, &pe):
					atomic.AddInt64(&refused, 1)
				}
			}
		}(u)
	}
	time.Sleep(300 * time.Millisecond)
	o := domain.DefaultExport()
	o.History, o.Attachments = true, false
	ackBefore := atomic.LoadInt64(&acked)
	b, man := exportBytes(t, e, e.Admin, o)
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	if refused != 0 {
		t.Fatalf("a snapshot export held %d writers", refused)
	}
	zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	maxSeq := int64(0)
	for _, f := range zr.File {
		if f.Name != "text_versions.jsonl" {
			continue
		}
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var v domain.BTextVersion
			json.Unmarshal([]byte(line), &v)
			if v.Field == "description" && v.Seq > maxSeq {
				maxSeq = v.Seq
			}
		}
	}
	// exact boundary: the export holds precisely the description saves that the outbox recorded up to its marker
	var upTo int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE type='issue.text_saved' AND payload->>'field'='description' AND id<=$1`, man.Marker.OutboxID).Scan(&upTo)
	if maxSeq-1 != int64(upTo) {
		t.Fatalf("the export holds %d description saves, the outbox says %d were committed up to the marker", maxSeq-1, upTo)
	}
	if maxSeq-1 < ackBefore {
		t.Fatalf("a save acknowledged before the export began is missing: export has %d, %d were acknowledged", maxSeq-1, ackBefore)
	}
	var total int64
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE type='issue.text_saved' AND payload->>'field'='description'`).Scan(&total)
	if total <= int64(upTo) {
		t.Fatalf("the writers did not keep saving after the marker (%d total, %d at the marker)", total, upTo)
	}
	t.Logf("%d saves acknowledged, export holds exactly the %d committed at its marker, %d were saved after it", acked, upTo, total-int64(upTo))
	if man.Mode != "snapshot" || man.Marker.LSN == "" || man.Marker.OutboxID == 0 || man.Derived == "" { // PAUSE-12: the manifest records mode, marker and time
		t.Fatalf("manifest: %+v", man)
	}
}

func TestEXP03_ExportRespectsPermissions(t *testing.T) {
	e := testutil.New(t)
	rich(t, e)
	vu, _ := e.Svc.CreateUser(ctx, "vera@example.com", "Vera", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	ou, _ := e.Svc.CreateUser(ctx, "eve@example.com", "Eve", "password123", false)
	viewer, outsider := domain.Actor{ID: vu.ID, Label: "Vera"}, domain.Actor{ID: ou.ID, Label: "Eve"}
	hasInternal := func(b []byte) bool {
		zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		for _, f := range zr.File {
			if f.Name == "comments.jsonl" {
				rc, _ := f.Open()
				d, _ := io.ReadAll(rc)
				rc.Close()
				return strings.Contains(string(d), "an internal note")
			}
		}
		return false
	}
	o := domain.DefaultExport()
	o.Projects = []string{"ABC"}
	vb, _ := exportBytes(t, e, viewer, o)
	if hasInternal(vb) {
		t.Fatal("a viewer's export contains an internal comment")
	}
	mb, _ := exportBytes(t, e, e.Alice, o)
	if !hasInternal(mb) {
		t.Fatal("a member's export lacks the internal comment")
	}
	// an outsider asking for the project gets what a missing project gets; without a name they get nothing to export
	var buf bytes.Buffer
	if _, err := e.Svc.Export(ctx, outsider, &buf, o); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Export(ctx, outsider, &buf, domain.DefaultExport()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// the audited full export is for administrators only
	full := domain.DefaultExport()
	full.Full = true
	if _, err := e.Svc.Export(ctx, e.Alice, &buf, full); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	fb, _ := exportBytes(t, e, e.Admin, full)
	if !hasInternal(fb) {
		t.Fatal("the full export lacks internal comments")
	}
	var audits int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action IN ('export','export.full')`).Scan(&audits)
	if audits != 3 { // every export that ran is audited (the refused ones exported nothing)
		t.Fatalf("%d export audit events", audits)
	}
	// no credentials are ever in a bundle
	for _, b := range [][]byte{vb, mb, fb} {
		zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		for _, f := range zr.File {
			rc, _ := f.Open()
			d, _ := io.ReadAll(rc)
			rc.Close()
			if strings.Contains(strings.ToLower(string(d)), "password") || strings.Contains(string(d), "$2a$") {
				t.Fatalf("%s mentions credentials", f.Name)
			}
		}
	}
}

func tamper(t *testing.T, b []byte, edit func(name string, data []byte) ([]byte, bool)) []byte {
	zr, _ := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		rc, _ := f.Open()
		d, _ := io.ReadAll(rc)
		rc.Close()
		nd, keep := edit(f.Name, d)
		if !keep {
			continue
		}
		w, _ := zw.Create(f.Name)
		w.Write(nd)
	}
	zw.Close()
	return out.Bytes()
}

func TestEXP_CorruptBundlesAreRejectedWholesale(t *testing.T) {
	src := testutil.New(t)
	rich(t, src)
	b, _ := exportBytes(t, src, src.Admin, domain.DefaultExport())
	dst := testutil.New(t)
	dst.WithBlobs()
	for name, bad := range map[string][]byte{
		"not a zip":          []byte("hello"),
		"altered issue data": tamper(t, b, func(n string, d []byte) ([]byte, bool) { return append(d, ' '), n != "issues.jsonl" || true }),
		"missing file":       tamper(t, b, func(n string, d []byte) ([]byte, bool) { return d, n != "comments.jsonl" }),
		"no manifest":        tamper(t, b, func(n string, d []byte) ([]byte, bool) { return d, n != "manifest.json" }),
		"altered attachment": tamper(t, b, func(n string, d []byte) ([]byte, bool) { return append([]byte(nil), append(d, 'x')...), true }),
		"newer format version": tamper(t, b, func(n string, d []byte) ([]byte, bool) {
			if n == "manifest.json" {
				return []byte(strings.Replace(string(d), `"version": 1`, `"version": 99`, 1)), true
			}
			return d, true
		}),
	} {
		if _, err := dst.Svc.Import(ctx, dst.Admin, bytes.NewReader(bad), int64(len(bad)), domain.ImportOpts{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	var n int
	dst.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n)
	if n != 0 {
		t.Fatalf("a rejected bundle left %d issues behind", n)
	}
	// only administrators import
	if _, err := dst.Svc.Import(ctx, dst.Alice, bytes.NewReader(b), int64(len(b)), domain.ImportOpts{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
}

func TestIMP05_06_07_DryRunIdempotencyRollbackAndCollisions(t *testing.T) {
	src := testutil.New(t)
	rich(t, src)
	b, _ := exportBytes(t, src, src.Admin, domain.DefaultExport())
	dst := testutil.New(t)
	dst.WithBlobs()
	count := func(q string) int {
		var n int
		dst.Store.Pool.QueryRow(ctx, q).Scan(&n)
		return n
	}
	// IMP-05: the dry run predicts, and commits nothing
	dry := importBytes(t, dst, b, domain.ImportOpts{DryRun: true})
	if count(`SELECT count(*) FROM issue`) != 0 || count(`SELECT count(*) FROM project`) != 0 || count(`SELECT count(*) FROM blob`) != 0 {
		t.Fatal("a dry run committed something")
	}
	// IMP-06: the real run equals the prediction; running it again changes nothing
	real := importBytes(t, dst, b, domain.ImportOpts{})
	for _, k := range []string{"issues_created", "comments_created", "links_created", "attachments_created", "projects_created", "aliases_created"} {
		if dry.Counts[k] != real.Counts[k] {
			t.Errorf("%s: dry run %d, real %d", k, dry.Counts[k], real.Counts[k])
		}
	}
	issues, comments := count(`SELECT count(*) FROM issue`), count(`SELECT count(*) FROM comment`)
	again := importBytes(t, dst, b, domain.ImportOpts{})
	if again.Counts["issues_created"] != 0 || again.Counts["comments_created"] != 0 || again.Counts["issues_updated"] != 0 || again.Counts["issues_unchanged"] != issues {
		t.Fatalf("re-import: %+v", again.Counts)
	}
	if count(`SELECT count(*) FROM issue`) != issues || count(`SELECT count(*) FROM comment`) != comments {
		t.Fatal("re-importing the same bundle duplicated records")
	}
	// a change at the source updates in place
	v, _ := src.Svc.View(ctx, src.Admin, "ABC-4")
	src.Svc.SaveText(ctx, src.Alice, "ABC-4", domain.SaveIn{Field: "description", Body: "edited at the source", BaseSeq: v.Issue.DescSeq})
	b2, _ := exportBytes(t, src, src.Admin, domain.DefaultExport())
	upd := importBytes(t, dst, b2, domain.ImportOpts{Pause: true}) // updating existing data holds a project pause meanwhile
	if upd.Counts["issues_updated"] != 1 || upd.Counts["issues_created"] != 0 {
		t.Fatalf("%+v", upd.Counts)
	}
	if got, _ := dst.Svc.View(ctx, dst.Admin, "ABC-4"); got.Issue.Description != "edited at the source" || count(`SELECT count(*) FROM issue`) != issues {
		t.Fatal("the update was not applied in place")
	}
	if st, _ := dst.Svc.PauseStatus(ctx, dst.Admin); len(st) != 0 {
		t.Fatal("the import left its pause behind")
	}
	// a key clash with unrelated issues is refused with the reason; a rename resolves it and keeps the old keys as aliases
	other := testutil.New(t)
	other.WithBlobs()
	other.Project()
	mk(t, other, other.Alice, "Task", "someone else's issue", "")
	if _, err := other.Svc.Import(ctx, other.Admin, bytes.NewReader(b), int64(len(b)), domain.ImportOpts{}); err == nil || !strings.Contains(err.Error(), "rename_projects") {
		t.Fatalf("clash: %v", err)
	}
	if n := func() int { var n int; other.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n); return n }(); n != 1 {
		t.Fatalf("a refused import changed the instance: %d issues", n)
	}
	rep := importBytes(t, other, b, domain.ImportOpts{RenameProjects: map[string]string{"ABC": "ABD"}})
	if rep.KeyMap["ABC-2"] != "ABD-2" {
		t.Fatalf("key map: %v", rep.KeyMap)
	}
	if v, err := other.Svc.View(ctx, other.Admin, "ABC-2"); err != nil || v.Issue.Key != "ABD-2" { // the old key still opens it
		t.Fatalf("old key after a rename: %v %+v", err, v.Issue)
	}
	// IMP-07: rollback by import id removes only what that run created
	preIssues := func() int { var n int; other.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n); return n }()
	n, err := other.Svc.RollbackImport(ctx, other.Admin, rep.JobID)
	if err != nil || n != preIssues-1 {
		t.Fatalf("rollback removed %d (%v), want %d", n, err, preIssues-1)
	}
	if _, err := other.Svc.View(ctx, other.Admin, "ABD-2"); err == nil {
		t.Fatal("an imported issue survived the rollback")
	}
	var mine int
	other.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE summary='someone else''s issue'`).Scan(&mine)
	if mine != 1 {
		t.Fatal("the rollback removed an issue it did not create")
	}
	if _, err := other.Svc.RollbackImport(ctx, other.Admin, rep.JobID); err == nil {
		t.Fatal("rolled back twice")
	}
}
