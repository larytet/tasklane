package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

func mk(t *testing.T, e *testutil.Env, a domain.Actor, typ, summary, parent string) string {
	t.Helper()
	is, err := e.Svc.CreateIssue(ctx, a, domain.CreateIn{Project: "ABC", Type: typ, Summary: summary, Description: "desc of " + summary, Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	return is.Key
}

func life(t *testing.T, e *testutil.Env, a domain.Actor, key string, in domain.LifecycleIn) *domain.Plan {
	t.Helper()
	p, err := e.Svc.Lifecycle(ctx, a, key, in)
	if err != nil {
		t.Fatalf("%s %s: %v (plan %+v)", in.Op, key, err, p)
	}
	return p
}

func xyz(t *testing.T, e *testutil.Env) {
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Xray"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []domain.Actor{e.Alice, e.Bob} {
		e.Svc.AddMember(ctx, e.Admin, "XYZ", a.ID, "member")
	}
}

func TestLIFE_HierarchyRulesAtCreation(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	epic := mk(t, e, e.Alice, "Epic", "e", "")
	story := mk(t, e, e.Alice, "Story", "s", epic)
	mk(t, e, e.Alice, "Subtask", "t", story)
	for name, in := range map[string]domain.CreateIn{
		"subtask without a parent":   {Project: "ABC", Type: "Subtask", Summary: "x"},
		"subtask under an epic":      {Project: "ABC", Type: "Subtask", Summary: "x", Parent: epic},
		"story under a story":        {Project: "ABC", Type: "Story", Summary: "x", Parent: story},
		"epic under an epic":         {Project: "ABC", Type: "Epic", Summary: "x", Parent: epic},
		"parent that does not exist": {Project: "ABC", Type: "Story", Summary: "x", Parent: "ABC-999"},
	} {
		if _, err := e.Svc.CreateIssue(ctx, e.Alice, in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestLIFE09_DeleteToTrash(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	ix := &search.Indexer{DB: e.Store.Pool}
	epic := mk(t, e, e.Alice, "Epic", "epic", "")
	s1 := mk(t, e, e.Alice, "Story", "story one", epic)
	s2 := mk(t, e, e.Alice, "Story", "story two", epic)
	sub := mk(t, e, e.Alice, "Subtask", "subtask", s1)
	other := mk(t, e, e.Alice, "Task", "linker", "")
	if err := e.Svc.AddLink(ctx, e.Alice, other, "relates", s1); err != nil {
		t.Fatal(err)
	}
	e.Svc.SaveDraft(ctx, e.Alice, s1, "description", "my unsaved words", 1)
	// the plan changes nothing
	p := life(t, e, e.Alice, s1, domain.LifecycleIn{Op: "delete", DryRun: true})
	if p.Executed || p.Counts["issues"] != 2 || p.Counts["subtasks_deleted"] != 1 {
		t.Fatalf("plan: %+v", p)
	}
	if _, err := e.Svc.View(ctx, e.Alice, s1); err != nil {
		t.Fatalf("a dry run deleted the issue: %v", err)
	}
	// execute: the result equals the plan
	r := life(t, e, e.Alice, s1, domain.LifecycleIn{Op: "delete"})
	if !r.Executed || r.Counts["issues"] != p.Counts["issues"] || r.Counts["subtasks_deleted"] != p.Counts["subtasks_deleted"] {
		t.Fatalf("plan %+v, result %+v", p.Counts, r.Counts)
	}
	for _, k := range []string{s1, sub} { // gone for people who can see the project, and told so with 410
		if _, err := e.Svc.View(ctx, e.Alice, k); !errors.Is(err, domain.ErrGone) {
			t.Fatalf("%s: %v", k, err)
		}
	}
	u, _ := e.Svc.CreateUser(ctx, "eve@example.com", "Eve", "password123", false)
	if _, err := e.Svc.View(ctx, domain.Actor{ID: u.ID}, s1); !errors.Is(err, domain.ErrNotFound) { // an outsider is not told it ever existed
		t.Fatalf("outsider: %v", err)
	}
	// an open editor: its save after the delete is refused (410) and the draft written before is still there
	if _, err := e.Svc.SaveText(ctx, e.Alice, s1, domain.SaveIn{Field: "description", Body: "late", BaseSeq: 1}); !errors.Is(err, domain.ErrGone) {
		t.Fatalf("save after delete: %v", err)
	}
	var drafts int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM draft WHERE body='my unsaved words'`).Scan(&drafts)
	if drafts != 1 {
		t.Fatal("the draft was lost")
	}
	// invisible in lists, search and TQL, and the key is reserved
	for { // the indexer removes it
		n, _ := ix.RunOnce(ctx)
		if n == 0 {
			break
		}
	}
	all, rd, wr, _ := e.Svc.SearchScope(ctx, e.Admin)
	sc := search.Scope{UserID: e.Admin.ID, All: all, Readable: rd, Writable: wr, Now: time.Now}
	eng := &search.Postgres{DB: e.Store.Pool}
	for _, q := range []string{"", "key = " + s1, "text ~ story", "project = ABC", "summary ~ subtask"} {
		res, err := eng.Search(ctx, sc, q, search.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range res.Rows {
			if row.Key == s1 || row.Key == sub {
				t.Errorf("query %q still returns the trashed %s", q, row.Key)
			}
		}
	}
	next := mk(t, e, e.Alice, "Task", "after the delete", "")
	if next != "ABC-6" { // numbers are never reused: 1..5 were used, the trashed ones stay reserved
		t.Fatalf("next key %s", next)
	}
	// a link to a deleted issue shows only a placeholder
	v, _ := e.Svc.View(ctx, e.Alice, other)
	if len(v.Links) != 1 || !v.Links[0].Deleted || v.Links[0].Summary != "" || v.Links[0].Key != s1 {
		t.Fatalf("links: %+v", v.Links)
	}
	// deleting an epic detaches its children instead of deleting them
	pe := life(t, e, e.Alice, epic, domain.LifecycleIn{Op: "delete"})
	if pe.Counts["children_detached"] != 1 || pe.Counts["issues"] != 1 { // s1 is already in the trash; s2 is detached
		t.Fatalf("%+v", pe.Counts)
	}
	if v, err := e.Svc.View(ctx, e.Alice, s2); err != nil || v.Issue.Parent != "" {
		t.Fatalf("child after epic delete: %v %+v", err, v.Issue)
	}
}

func TestLIFE09_WhoMayDelete(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	key := mk(t, e, e.Alice, "Task", "alice's", "")
	vu, _ := e.Svc.CreateUser(ctx, "vera@example.com", "Vera", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	for name, a := range map[string]domain.Actor{"viewer": {ID: vu.ID, Label: "Vera"}, "another member": e.Bob} {
		if _, err := e.Svc.Lifecycle(ctx, a, key, domain.LifecycleIn{Op: "delete"}); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, key, domain.LifecycleIn{Op: "delete"}); err != nil { // the reporter
		t.Fatal(err)
	}
	// restore: the deleter or an administrator, not another member
	if _, err := e.Svc.Lifecycle(ctx, e.Bob, key, domain.LifecycleIn{Op: "restore"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Admin, key, domain.LifecycleIn{Op: "restore"}); err != nil {
		t.Fatal(err)
	}
}

func TestLIFE10_RestoreBringsBackEverything(t *testing.T) {
	e := testutil.New(t)
	e.WithBlobs()
	e.Project()
	story := mk(t, e, e.Alice, "Story", "restorable", "")
	sub := mk(t, e, e.Alice, "Subtask", "child", story)
	e.Svc.AddComment(ctx, e.Bob, story, "a comment", false)
	e.Svc.AttachStream(ctx, e.Alice, story, "a.txt", "text/plain", strings.NewReader("attached"))
	v, _ := e.Svc.View(ctx, e.Alice, story)
	e.Svc.Transition(ctx, e.Alice, story, v.Transitions[0].ID, nil) // In Progress
	life(t, e, e.Alice, story, domain.LifecycleIn{Op: "delete"})
	if p := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "restore", DryRun: true}); p.Counts["issues"] != 2 || p.Counts["subtasks_restored"] != 1 {
		t.Fatalf("%+v", p)
	}
	r := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "restore"})
	if r.Counts["issues"] != 2 {
		t.Fatalf("%+v", r)
	}
	v, err := e.Svc.View(ctx, e.Alice, story)
	if err != nil || v.Issue.Status != "In Progress" || len(v.Comments) != 1 || len(v.Attachments) != 1 || v.Issue.Key != story {
		t.Fatalf("%v %+v", err, v.Issue)
	}
	if _, err := e.Svc.View(ctx, e.Alice, sub); err != nil { // the subtask deleted with it comes back with it
		t.Fatal(err)
	}
	// restoring something that is not in the trash is a blocker, not a change
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, story, domain.LifecycleIn{Op: "restore"}); err == nil {
		t.Fatal("restored a live issue")
	}
}

func TestLIFE11_PurgeAndRetention(t *testing.T) {
	e := testutil.New(t)
	fs := e.WithBlobs()
	e.Svc.Blobs.Retention = 0
	e.Project()
	key := mk(t, e, e.Alice, "Task", "to purge", "")
	att, _ := e.Svc.AttachStream(ctx, e.Alice, key, "p.txt", "text/plain", strings.NewReader("purge me please"))
	e.Svc.AddComment(ctx, e.Alice, key, "c", false)
	e.Svc.SaveDraft(ctx, e.Alice, key, "description", "d", 1)
	// not in the trash: blocked; not for agents; not for ordinary members; not under legal hold
	if _, err := e.Svc.Lifecycle(ctx, e.Admin, key, domain.LifecycleIn{Op: "purge"}); err == nil {
		t.Fatal("purged a live issue")
	}
	life(t, e, e.Alice, key, domain.LifecycleIn{Op: "delete"})
	agent := domain.Actor{ID: e.Admin.ID, Label: "Agent: claude", Admin: true, Kind: "agent"}
	if _, err := e.Svc.Lifecycle(ctx, agent, key, domain.LifecycleIn{Op: "purge"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an agent purged: %v", err)
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Bob, key, domain.LifecycleIn{Op: "purge"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member purged: %v", err)
	}
	e.Svc.SetLegalHold(ctx, e.Admin, key, true)
	if _, err := e.Svc.Lifecycle(ctx, e.Admin, key, domain.LifecycleIn{Op: "purge"}); err == nil || !strings.Contains(err.Error(), "legal hold") {
		t.Fatalf("legal hold did not block: %v", err)
	}
	e.Svc.SetLegalHold(ctx, e.Admin, key, false)
	// purge expired respects the retention date: nothing yet, then everything after it
	if n, _ := e.Svc.PurgeExpired(ctx); n != 0 {
		t.Fatal("purged inside the retention window")
	}
	e.Clock.Advance(31 * 24 * time.Hour)
	if n, err := e.Svc.PurgeExpired(ctx); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	var rows int
	for _, q := range []string{`SELECT count(*) FROM issue WHERE key='ABC-1'`, `SELECT count(*) FROM comment`, `SELECT count(*) FROM draft`, `SELECT count(*) FROM issue_history`, `SELECT count(*) FROM text_version`, `SELECT count(*) FROM attachment`} {
		e.Store.Pool.QueryRow(ctx, q).Scan(&rows)
		if rows != 0 {
			t.Errorf("%s: %d rows left", q, rows)
		}
	}
	if _, err := e.Svc.View(ctx, e.Alice, key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	// the blob is released and collected, and the audit trail of the deletion and the purge stays
	if n, err := e.Svc.GC(ctx); err != nil || n != 1 {
		t.Fatalf("gc %d %v", n, err)
	}
	if f, err := fs.Open(att.SHA256); err == nil {
		f.Close()
		t.Fatal("the blob bytes are still stored")
	}
	var audits int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action IN ('issue.delete','issue.purge')`).Scan(&audits)
	if audits != 2 {
		t.Fatalf("%d audit events", audits)
	}
	// a legal hold stops the scheduled purge too
	k2 := mk(t, e, e.Alice, "Task", "held", "")
	life(t, e, e.Alice, k2, domain.LifecycleIn{Op: "delete"})
	e.Svc.SetLegalHold(ctx, e.Admin, k2, true)
	e.Clock.Advance(40 * 24 * time.Hour)
	if n, _ := e.Svc.PurgeExpired(ctx); n != 0 {
		t.Fatal("scheduled purge ignored the legal hold")
	}
}

func TestLIFE04_ConvertType(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	story := mk(t, e, e.Alice, "Story", "convertible", "")
	sub := mk(t, e, e.Alice, "Subtask", "its subtask", story)
	// a Story with a subtask can become a Task (still level 0): nothing is dropped
	p := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "convert", Type: "Task", DryRun: true})
	if len(p.Drops) != 0 || p.Counts["children_detached"] != 0 {
		t.Fatalf("%+v", p)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, story); v.Issue.Type != "Story" {
		t.Fatal("a dry run converted the issue")
	}
	life(t, e, e.Alice, story, domain.LifecycleIn{Op: "convert", Type: "Task"})
	// Task -> Epic: its subtask cannot sit under an Epic, so the plan says it is detached, and the result matches the plan
	plan := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "convert", Type: "Epic", DryRun: true})
	if plan.Counts["children_detached"] != 1 || len(plan.Drops) != 1 || !strings.Contains(plan.Drops[0], sub) {
		t.Fatalf("%+v", plan)
	}
	res := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "convert", Type: "Epic"})
	if res.Counts["children_detached"] != plan.Counts["children_detached"] || len(res.Drops) != len(plan.Drops) {
		t.Fatalf("plan %+v result %+v", plan, res)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, sub); v.Issue.Parent != "" {
		t.Fatal("the subtask is still attached to an epic")
	}
	// converting back restores what was removed
	back := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "convert", Type: "Task"})
	if back.Counts["restored"] != 1 {
		t.Fatalf("%+v", back)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, sub); v.Issue.Parent != story {
		t.Fatalf("subtask parent after converting back: %q", v.Issue.Parent)
	}
	// hierarchy: a subtask needs a parent; key, comments and history are untouched; same type is a blocker
	lone := mk(t, e, e.Alice, "Task", "lone", "")
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, lone, domain.LifecycleIn{Op: "convert", Type: "Subtask"}); err == nil || !strings.Contains(err.Error(), "needs a parent") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, lone, domain.LifecycleIn{Op: "convert", Type: "Task"}); err == nil {
		t.Fatal("converted to the same type")
	}
	// a Story under an Epic that becomes an Epic itself leaves its parent
	epic := mk(t, e, e.Alice, "Epic", "parent epic", "")
	child := mk(t, e, e.Alice, "Story", "child story", epic)
	p2 := life(t, e, e.Alice, child, domain.LifecycleIn{Op: "convert", Type: "Epic"})
	if p2.Counts["detached"] != 1 {
		t.Fatalf("%+v", p2)
	}
	h, _ := e.Svc.History(ctx, e.Alice, child)
	found := false
	for _, x := range h {
		if x.Field == "type" && *x.Before == "Story" && *x.After == "Epic" {
			found = true
		}
	}
	if !found {
		t.Fatal("no history entry for the conversion")
	}
	// viewers cannot convert
	vu, _ := e.Svc.CreateUser(ctx, "vera@example.com", "Vera", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	if _, err := e.Svc.Lifecycle(ctx, domain.Actor{ID: vu.ID}, lone, domain.LifecycleIn{Op: "convert", Type: "Bug"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
}

func TestLIFE05_06_MoveWithAliases(t *testing.T) {
	e := testutil.New(t)
	e.WithBlobs()
	e.Project()
	xyz(t, e)
	ix := &search.Indexer{DB: e.Store.Pool}
	epic := mk(t, e, e.Alice, "Epic", "stays behind", "")
	story := mk(t, e, e.Alice, "Story", "moving story", epic)
	sub := mk(t, e, e.Alice, "Subtask", "moves along", story)
	e.Svc.AddComment(ctx, e.Bob, story, "a comment", false)
	e.Svc.AttachStream(ctx, e.Alice, story, "a.txt", "text/plain", strings.NewReader("attachment"))
	e.Svc.SaveDraft(ctx, e.Bob, story, "description", "bob is typing", 1)
	// dry run predicts the keys; the real move produces exactly them
	plan := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "move", Project: "XYZ", DryRun: true})
	if plan.NewKeys[story] != "XYZ-1" || plan.NewKeys[sub] != "XYZ-2" || plan.Counts["subtasks_moved"] != 1 || plan.Counts["parent_dropped"] != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, story); v.Issue.Key != story {
		t.Fatal("a dry run moved the issue")
	}
	res := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "move", Project: "XYZ", ExpectKey: story})
	if res.NewKeys[story] != plan.NewKeys[story] || res.NewKeys[sub] != plan.NewKeys[sub] || res.Counts["issues"] != 2 {
		t.Fatalf("plan %+v result %+v", plan, res)
	}
	// the new key works and carries everything; the old key resolves everywhere
	v, err := e.Svc.View(ctx, e.Alice, "XYZ-1")
	if err != nil || v.Issue.Project != "XYZ" || v.Issue.Parent != "" || len(v.Comments) != 1 || len(v.Attachments) != 1 || v.Alias != "" {
		t.Fatalf("%v %+v", err, v.Issue)
	}
	old, err := e.Svc.View(ctx, e.Alice, story) // the old key, in REST
	if err != nil || old.Issue.Key != "XYZ-1" || old.Alias != strings.ToUpper(story) {
		t.Fatalf("old key: %v %+v alias %q", err, old.Issue, old.Alias)
	}
	if k, err := e.Svc.ResolveKey(ctx, story); err != nil || k != "XYZ-1" {
		t.Fatalf("%q %v", k, err)
	}
	if v, err := e.Svc.View(ctx, e.Alice, sub); err != nil || v.Issue.Key != "XYZ-2" || v.Issue.Parent != "XYZ-1" {
		t.Fatalf("subtask: %v %+v", err, v.Issue)
	}
	// a draft written before the move is still there, and a save through the old key follows the issue
	if ds, _ := e.Svc.GetDrafts(ctx, e.Bob, story); len(ds) != 1 || ds[0].Body != "bob is typing" {
		t.Fatalf("drafts: %+v", ds)
	}
	if _, err := e.Svc.SaveText(ctx, e.Bob, story, domain.SaveIn{Field: "description", Body: "saved through the old key", BaseSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, "XYZ-1"); v.Issue.Description != "saved through the old key" {
		t.Fatal("the save did not follow the move")
	}
	// TQL: the old key finds it, the new key finds it, both in lists
	for {
		if n, _ := ix.RunOnce(ctx); n == 0 {
			break
		}
	}
	all, rd, wr, _ := e.Svc.SearchScope(ctx, e.Alice)
	sc := search.Scope{UserID: e.Alice.ID, All: all, Readable: rd, Writable: wr, Now: time.Now}
	eng := &search.Postgres{DB: e.Store.Pool}
	for _, q := range []string{"key = " + story, "key = XYZ-1", "key in (" + story + ", ZZZ-9)", "text ~ \"" + "XYZ-1" + "\""} {
		res, err := eng.Search(ctx, sc, q, search.Page{Limit: 10})
		if err != nil || len(res.Rows) != 1 || res.Rows[0].Key != "XYZ-1" {
			t.Errorf("%q: %v %+v", q, err, res)
		}
	}
	// history records the key change; the old number is never reused by its project
	h, _ := e.Svc.History(ctx, e.Alice, "XYZ-1")
	got := false
	for _, x := range h {
		if x.Field == "key" && *x.Before == strings.ToUpper(story) && *x.After == "XYZ-1" {
			got = true
		}
	}
	if !got {
		t.Fatal("no key change in the history")
	}
	if next := mk(t, e, e.Alice, "Task", "next in ABC", ""); next != "ABC-4" {
		t.Fatalf("an old number was reused: %s", next)
	}
	// moving again chains: every earlier key still resolves
	life(t, e, e.Alice, "XYZ-1", domain.LifecycleIn{Op: "move", Project: "ABC"})
	for _, k := range []string{story, "XYZ-1"} {
		if v, err := e.Svc.View(ctx, e.Alice, k); err != nil || v.Issue.Project != "ABC" {
			t.Fatalf("%s after the second move: %v", k, err)
		}
	}
}

func TestLIFE06_MovePermissionsAndBlockers(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	xyz(t, e)
	solo, _ := e.Svc.CreateUser(ctx, "solo@example.com", "Solo", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", solo.ID, "member") // member of ABC only
	vu, _ := e.Svc.CreateUser(ctx, "vera@example.com", "Vera", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "member")
	e.Svc.AddMember(ctx, e.Admin, "XYZ", vu.ID, "viewer")
	key := mk(t, e, e.Alice, "Task", "movable", "")
	// a target the caller cannot see answers like one that does not exist; a target they can only read is forbidden
	if _, err := e.Svc.Lifecycle(ctx, domain.Actor{ID: solo.ID}, key, domain.LifecycleIn{Op: "move", Project: "XYZ"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Lifecycle(ctx, domain.Actor{ID: vu.ID}, key, domain.LifecycleIn{Op: "move", Project: "XYZ"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, key, domain.LifecycleIn{Op: "move", Project: "ABC"}); err == nil {
		t.Fatal("moved into its own project")
	}
	// a subtask cannot move alone; a type that does not fit the target hierarchy is refused with the reason
	story := mk(t, e, e.Alice, "Story", "parent", "")
	sub := mk(t, e, e.Alice, "Subtask", "child", story)
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, sub, domain.LifecycleIn{Op: "move", Project: "XYZ"}); err == nil || !strings.Contains(err.Error(), "move with their parent") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, story, domain.LifecycleIn{Op: "move", Project: "XYZ", TypeMap: map[string]string{"Story": "Epic"}}); err == nil {
		t.Fatal("a type of a different level was accepted in a move")
	}
	if v, _ := e.Svc.View(ctx, e.Alice, story); v.Issue.Project != "ABC" {
		t.Fatal("a refused move changed the issue") // nothing is half applied
	}
	// the epic's children are detached with a record when the epic moves
	epic := mk(t, e, e.Alice, "Epic", "epic", "")
	kid := mk(t, e, e.Alice, "Story", "kid", epic)
	p := life(t, e, e.Alice, epic, domain.LifecycleIn{Op: "move", Project: "XYZ"})
	if p.Counts["children_detached"] != 1 {
		t.Fatalf("%+v", p)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, kid); v.Issue.Parent != "" || v.Issue.Project != "ABC" {
		t.Fatalf("%+v", v.Issue)
	}
}

func TestLIFE08_CloneOptionMatrix(t *testing.T) {
	e := testutil.New(t)
	e.WithBlobs()
	e.Project()
	xyz(t, e)
	story := mk(t, e, e.Alice, "Story", "original", "")
	for i := 1; i <= 3; i++ {
		mk(t, e, e.Alice, "Subtask", fmt.Sprint("sub ", i), story)
	}
	other := mk(t, e, e.Alice, "Task", "linked", "")
	e.Svc.AddLink(ctx, e.Alice, story, "blocks", other)
	e.Svc.AddComment(ctx, e.Bob, story, "a comment", false)
	e.Svc.AttachStream(ctx, e.Alice, story, "one.txt", "text/plain", strings.NewReader("first attachment"))
	e.Svc.AttachStream(ctx, e.Alice, story, "two.txt", "text/plain", strings.NewReader("second attachment"))
	e.Svc.SaveText(ctx, e.Alice, story, domain.SaveIn{Field: "description", Body: "text at clone time", BaseSeq: 1})
	v, _ := e.Svc.View(ctx, e.Alice, story)
	for _, tr := range v.Transitions { // done, assigned to Bob, high priority
		if tr.Name == "Resolve" {
			e.Svc.Transition(ctx, e.Alice, story, tr.ID, nil)
		}
	}
	hi, bob := "high", e.Bob.ID
	e.Svc.UpdateFields(ctx, e.Alice, story, nil, false, domain.Changes{Priority: &hi, Assignee: &bob})
	refs := func() int {
		n := 0
		e.Store.Pool.QueryRow(ctx, `SELECT COALESCE(sum(refcount),0) FROM blob`).Scan(&n)
		return n
	}
	before := refs()
	plan := life(t, e, e.Bob, story, domain.LifecycleIn{Op: "clone", DryRun: true})
	if refs() != before {
		t.Fatal("a dry run changed blob reference counts")
	}
	res := life(t, e, e.Bob, story, domain.LifecycleIn{Op: "clone"}) // Bob clones
	if res.NewKeys[story] != plan.NewKeys[story] || res.Counts["issues"] != plan.Counts["issues"] || res.Counts["attachments_shared"] != plan.Counts["attachments_shared"] || plan.Counts["attachments_shared"] != 2 {
		t.Fatalf("plan %+v result %+v", plan, res)
	}
	clone := res.Key2
	cv, err := e.Svc.View(ctx, e.Bob, clone)
	if err != nil {
		t.Fatal(err)
	}
	ci := cv.Issue
	if ci.Summary != "CLONE - original" || ci.Type != "Story" || ci.Priority != "high" || ci.Status != "To Do" || ci.Resolved != nil || ci.Reporter.ID != e.Bob.ID || ci.Assignee == nil || ci.Assignee.ID != e.Bob.ID {
		t.Fatalf("clone: %+v", ci)
	}
	if ci.Description != "text at clone time" || len(cv.Comments) != 0 || len(cv.Attachments) != 2 {
		t.Fatalf("clone content: %q comments %d attachments %d", ci.Description, len(cv.Comments), len(cv.Attachments))
	}
	// the description is an independent copy
	e.Svc.SaveText(ctx, e.Alice, story, domain.SaveIn{Field: "description", Body: "changed afterwards", BaseSeq: 2})
	if cv, _ := e.Svc.View(ctx, e.Bob, clone); cv.Issue.Description != "text at clone time" {
		t.Fatal("the clone's description follows the original")
	}
	// attachments are shared by reference: the same bytes, reference counts up by two, nothing copied
	ov, _ := e.Svc.View(ctx, e.Alice, story)
	if cv.Attachments[0].SHA256 != ov.Attachments[0].SHA256 || refs() != before+2 {
		t.Fatalf("blob references %d -> %d", before, refs())
	}
	// subtasks cloned recursively and attached to the clone, links not copied but the clones link is there, history is fresh
	subs := 0
	for k := range res.NewKeys {
		if k != story {
			subs++
		}
	}
	if subs != 3 || res.Counts["subtasks_cloned"] != 3 {
		t.Fatalf("subtasks: %+v", res)
	}
	hasClones, hasBlocks := false, false
	for _, l := range cv.Links {
		hasClones = hasClones || (l.Type == "clones" && l.Key == story && l.Dir == "out")
		hasBlocks = hasBlocks || l.Type == "blocks"
	}
	if !hasClones || hasBlocks {
		t.Fatalf("links: %+v", cv.Links)
	}
	if h, _ := e.Svc.History(ctx, e.Bob, clone); len(h) != 1 || h[0].Field != "created" {
		t.Fatalf("history was copied: %+v", h)
	}
	// options: no attachments, no subtasks, with comments and links, no clones link, keep the assignee, custom prefix
	off, prefix := false, "COPY: "
	r2 := life(t, e, e.Bob, story, domain.LifecycleIn{Op: "clone", Clone: domain.CloneOpts{Attachments: &off, Subtasks: &off, Comments: true, Links: true, NoCloneLink: true, KeepAssignee: true, Prefix: &prefix}})
	c2, _ := e.Svc.View(ctx, e.Bob, r2.Key2)
	if c2.Issue.Summary != "COPY: original" || len(c2.Attachments) != 0 || len(c2.Comments) != 1 || r2.Counts["issues"] != 1 || r2.Counts["links_copied"] != 1 {
		t.Fatalf("%+v %+v", c2.Issue, r2.Counts)
	}
	for _, l := range c2.Links {
		if l.Type == "clones" {
			t.Fatal("a clones link was created although it was switched off")
		}
	}
	if refs() != before+2 {
		t.Fatal("a clone without attachments changed blob references")
	}
	// into another project: a key in the target, viewers refused, a subtask alone refused
	r3 := life(t, e, e.Bob, story, domain.LifecycleIn{Op: "clone", Clone: domain.CloneOpts{Project: "XYZ"}})
	if !strings.HasPrefix(r3.Key2, "XYZ-") {
		t.Fatalf("%+v", r3)
	}
	var subKey string
	for k := range res.NewKeys {
		if k != story {
			subKey = k
		}
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Bob, subKey, domain.LifecycleIn{Op: "clone", Clone: domain.CloneOpts{Project: "XYZ"}}); err == nil {
		t.Fatal("cloned a subtask alone into another project")
	}
}

func TestLIFE08_CloneSubtaskCap(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	story := mk(t, e, e.Alice, "Story", "big", "")
	for i := 0; i < 55; i++ {
		mk(t, e, e.Alice, "Subtask", fmt.Sprint("s", i), story)
	}
	r := life(t, e, e.Alice, story, domain.LifecycleIn{Op: "clone"})
	if r.Counts["subtasks_cloned"] != 50 || len(r.Warnings) == 0 || !strings.Contains(strings.Join(r.Warnings, " "), "cap of 50") {
		t.Fatalf("%+v", r)
	}
}

func TestLIFE13_Races(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	xyz(t, e)
	// double move: exactly one wins, the other is told the issue moved
	key := mk(t, e, e.Alice, "Task", "contested", "")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflict := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Svc.Lifecycle(ctx, e.Alice, key, domain.LifecycleIn{Op: "move", Project: "XYZ", ExpectKey: key})
			var ce *domain.ConflictError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.As(err, &ce):
				conflict++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || conflict != 5 {
		t.Fatalf("ok=%d conflict=%d", ok, conflict)
	}
	v, err := e.Svc.View(ctx, e.Alice, key)
	if err != nil || v.Issue.Project != "XYZ" {
		t.Fatalf("%v %+v", err, v.Issue)
	}
	// move versus transition: both finish, the issue is in one place with a valid status, nothing is half applied
	for i := 0; i < 15; i++ {
		k := mk(t, e, e.Alice, "Task", fmt.Sprint("race ", i), "")
		vv, _ := e.Svc.View(ctx, e.Alice, k)
		var wg2 sync.WaitGroup
		wg2.Add(2)
		go func() {
			defer wg2.Done()
			e.Svc.Lifecycle(ctx, e.Alice, k, domain.LifecycleIn{Op: "move", Project: "XYZ"})
		}()
		go func() { defer wg2.Done(); e.Svc.Transition(ctx, e.Bob, k, vv.Transitions[0].ID, nil) }()
		wg2.Wait()
		after, err := e.Svc.View(ctx, e.Alice, k)
		if err != nil || after.Issue.Status != "In Progress" && after.Issue.Status != "Done" {
			t.Fatalf("race %d: %v %+v", i, err, after.Issue)
		}
	}
	// delete versus edit: the final state is deleted, and every acknowledged save is in the text versions
	for i := 0; i < 15; i++ {
		k := mk(t, e, e.Alice, "Task", fmt.Sprint("del race ", i), "")
		var wg3 sync.WaitGroup
		wg3.Add(2)
		var saved bool
		go func() {
			defer wg3.Done()
			_, err := e.Svc.SaveText(ctx, e.Alice, k, domain.SaveIn{Field: "description", Body: "edit", BaseSeq: 1})
			saved = err == nil
		}()
		go func() { defer wg3.Done(); e.Svc.Lifecycle(ctx, e.Alice, k, domain.LifecycleIn{Op: "delete"}) }()
		wg3.Wait()
		if _, err := e.Svc.View(ctx, e.Alice, k); !errors.Is(err, domain.ErrGone) {
			t.Fatalf("race %d: %v", i, err)
		}
		var n int
		e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM text_version WHERE issue_id=(SELECT id FROM issue WHERE key=$1) AND field='description' AND seq=2`, k).Scan(&n)
		if saved && n != 1 {
			t.Fatalf("race %d: an acknowledged save is missing", i)
		}
	}
}

func TestLIFE14_AuditAndEvents(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	xyz(t, e)
	var evs []domain.Event
	e.Svc.Publish = func(ev domain.Event) { evs = append(evs, ev) }
	k := mk(t, e, e.Alice, "Task", "audited", "")
	life(t, e, e.Alice, k, domain.LifecycleIn{Op: "clone"})
	life(t, e, e.Alice, k, domain.LifecycleIn{Op: "convert", Type: "Bug"})
	life(t, e, e.Alice, k, domain.LifecycleIn{Op: "move", Project: "XYZ"})
	life(t, e, e.Alice, "XYZ-1", domain.LifecycleIn{Op: "delete"})
	life(t, e, e.Alice, "XYZ-1", domain.LifecycleIn{Op: "restore"})
	life(t, e, e.Alice, "XYZ-1", domain.LifecycleIn{Op: "delete", DryRun: true})
	var actions []string
	rows, _ := e.Store.Pool.Query(ctx, `SELECT action FROM audit_event ORDER BY id`)
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	want := "issue.clone,issue.convert,issue.move,issue.delete,issue.restore"
	if strings.Join(actions, ",") != want { // every operation is audited, a dry run is not
		t.Fatalf("audit: %v", actions)
	}
	var types []string
	for _, ev := range evs {
		if ev.Type != "issue.created" || ev.Issue != k {
			types = append(types, ev.Type)
		}
	}
	got := strings.Join(types, ",")
	if !strings.Contains(got, "issue.updated,issue.moved,issue.deleted,issue.restored") || strings.Count(got, "issue.deleted") != 1 {
		t.Fatalf("events in order: %s", got)
	}
}
