package domain_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func undoable(t *testing.T) *testutil.Env {
	t.Helper()
	e := testutil.New(t)
	e.Project()
	return e
}

// takeUndo runs fn with an undo slot and returns the token it left.
func takeUndo(t *testing.T, fn func(c context.Context) error) string {
	t.Helper()
	c, sl := domain.WithUndo(ctx)
	if err := fn(c); err != nil {
		t.Fatal(err)
	}
	if sl.Token == "" {
		t.Fatal("the action left no undo token")
	}
	return sl.Token
}

func auditCount(t *testing.T, e *testutil.Env, action, target string) int {
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action=$1 AND target=$2`, action, target).Scan(&n)
	return n
}

// UNDO-01, UNDO-05, UNDO-09: a transition, an archive and a delete are each put back by their token; a transition goes back to the
// previous status even when the workflow has no transition back and a condition would refuse it, without running conditions or
// post-functions; history and audit record the undo with its actor.
func TestUNDO01_05_TransitionArchiveAndDeleteAreUndone(t *testing.T) {
	e := undoable(t)
	key := e.Issue("undo me", "").Key
	start := transitionID(t, e, "Start progress")
	if _, err := e.Svc.Transition(ctx, e.Alice, key, start, nil); err != nil {
		t.Fatal(err)
	}
	// In Progress -> Done; the workflow has no way from Done back to In Progress, and post-functions run on the way in
	done := transitionID(t, e, "Done")
	if err := e.Svc.SetTransitionRules(ctx, e.Admin, done, domain.TRules{PostFunctions: []domain.Spec{{"kind": "set_field", "field": "priority", "value": "low"}}}); err != nil {
		t.Fatal(err)
	}
	reopen := transitionID(t, e, "Reopen") // and the only way out of Done is refused to Alice by a condition
	if err := e.Svc.SetTransitionRules(ctx, e.Admin, reopen, domain.TRules{Conditions: []domain.Spec{{"kind": "role", "roles": []string{"admin"}}}}); err != nil {
		t.Fatal(err)
	}
	tok := takeUndo(t, func(c context.Context) error { _, err := e.Svc.Transition(c, e.Alice, key, done, nil); return err })
	v, _ := e.Svc.View(ctx, e.Alice, key)
	if v.Issue.Status != "Done" || v.Issue.Resolved == nil {
		t.Fatalf("%+v", v.Issue)
	}
	res, err := e.Svc.Undo(ctx, e.Alice, tok)
	if err != nil || len(res.Undone) != 1 || res.Undone[0] != key {
		t.Fatalf("%+v %v", res, err)
	}
	v, _ = e.Svc.View(ctx, e.Alice, key)
	if v.Issue.Status != "In Progress" || v.Issue.Resolved != nil {
		t.Fatalf("not put back: %s resolved %v", v.Issue.Status, v.Issue.Resolved)
	}
	if v.Issue.Priority != "low" {
		t.Fatalf("automation that already ran is not reversed, and the undo runs no post-function: priority %s", v.Issue.Priority)
	}
	hs, _ := e.Svc.History(ctx, e.Alice, key)
	last := hs[len(hs)-1]
	if last.Field != "status" || *last.Before != "Done" || *last.After != "In Progress" || last.Actor != e.Alice.Label {
		t.Fatalf("history: %+v", last)
	}
	if auditCount(t, e, "issue.undo", key) != 1 {
		t.Fatal("the undo is not in the audit log")
	}
	// outside the undo, the same move is refused as usual (no Done -> In Progress transition, and Reopen is not Alice's)
	if _, err := e.Svc.Transition(ctx, e.Alice, key, done, nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, key); len(v.Transitions) != 0 {
		t.Fatalf("Alice should have no way out of Done: %+v", v.Transitions)
	}

	// archive (the issue is Done now)
	atok := takeUndo(t, func(c context.Context) error { _, err := e.Svc.ArchiveIssue(c, e.Alice, key); return err })
	if res, err := e.Svc.Undo(ctx, e.Alice, atok); err != nil || len(res.Undone) != 1 {
		t.Fatalf("archive undo: %+v %v", res, err)
	}
	if v, _ := e.Svc.View(ctx, e.Alice, key); v.Issue.Archived != nil {
		t.Fatal("still archived")
	}
	// delete to the trash: put back with its key
	dtok := takeUndo(t, func(c context.Context) error {
		_, err := e.Svc.Lifecycle(c, e.Alice, key, domain.LifecycleIn{Op: "delete"})
		return err
	})
	if _, err := e.Svc.View(ctx, e.Alice, key); !errors.Is(err, domain.ErrGone) {
		t.Fatalf("not in the trash: %v", err)
	}
	if res, err := e.Svc.Undo(ctx, e.Alice, dtok); err != nil || len(res.Undone) != 1 {
		t.Fatalf("delete undo: %+v %v", res, err)
	}
	if v, err := e.Svc.View(ctx, e.Alice, key); err != nil || v.Issue.Key != key {
		t.Fatalf("not restored under its key: %v", err)
	}
	if auditCount(t, e, "issue.undo", key) != 3 {
		t.Fatalf("each undo is audited: %d", auditCount(t, e, "issue.undo", key))
	}
	// a dry run leaves no token
	c, sl := domain.WithUndo(ctx)
	if _, err := e.Svc.Lifecycle(c, e.Alice, key, domain.LifecycleIn{Op: "delete", DryRun: true}); err != nil || sl.Token != "" {
		t.Fatalf("dry run: %v %q", err, sl.Token)
	}
	// and an action without a slot leaves none at all
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM undo_token`).Scan(&n)
	if _, err := e.Svc.Transition(ctx, e.Alice, e.Issue("no slot", "").Key, transitionID(t, e, "Resolve"), nil); err != nil {
		t.Fatal(err)
	}
	var n2 int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM undo_token`).Scan(&n2)
	if n2 != n {
		t.Fatal("a token was written without being asked for")
	}
}

// UNDO-03: single use; expired, another person's and an unknown token are all "not found"; the same person in another session may use it.
func TestUNDO03_TokenRules(t *testing.T) {
	e := undoable(t)
	resolve := transitionID(t, e, "Resolve")
	k1 := e.Issue("one", "").Key
	tok := takeUndo(t, func(c context.Context) error { _, err := e.Svc.Transition(c, e.Alice, k1, resolve, nil); return err })
	if _, err := e.Svc.Undo(ctx, e.Bob, tok); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another person used it: %v", err)
	}
	if _, err := e.Svc.Undo(ctx, e.Alice, tok); err != nil { // Alice from another tab: tokens belong to the person, not the session
		t.Fatal(err)
	}
	if _, err := e.Svc.Undo(ctx, e.Alice, tok); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("used twice: %v", err)
	}
	k2 := e.Issue("two", "").Key
	tok2 := takeUndo(t, func(c context.Context) error { _, err := e.Svc.Transition(c, e.Alice, k2, resolve, nil); return err })
	e.Clock.Advance(domain.UndoWindow + time.Second)
	if _, err := e.Svc.Undo(ctx, e.Alice, tok2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := e.Svc.Undo(ctx, e.Alice, "un_unknown"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := e.Svc.Undo(ctx, domain.Actor{}, tok2); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("anonymous: %v", err)
	}
}

// UNDO-04: a change by anyone between the action and Undo (a field, a comment, another transition, the description) stops it, and
// nothing is reverted.
func TestUNDO04_AChangeSinceStopsTheUndo(t *testing.T) {
	e := undoable(t)
	resolve := transitionID(t, e, "Resolve")
	for name, change := range map[string]func(key string){
		"field": func(key string) {
			p := "low"
			if _, err := e.Svc.UpdateFields(ctx, e.Bob, key, nil, false, domain.Changes{Priority: &p}); err != nil {
				t.Fatal(err)
			}
		},
		"comment": func(key string) {
			if _, err := e.Svc.AddComment(ctx, e.Bob, key, "meanwhile", false); err != nil {
				t.Fatal(err)
			}
		},
		"transition": func(key string) {
			if _, err := e.Svc.Transition(ctx, e.Bob, key, transitionID(t, e, "Reopen"), nil); err != nil {
				t.Fatal(err)
			}
		},
		"description": func(key string) {
			v, _ := e.Svc.View(ctx, e.Bob, key)
			if _, err := e.Svc.SaveText(ctx, e.Bob, key, domain.SaveIn{Field: "description", Body: "rewritten", BaseSeq: v.Issue.DescSeq, Mode: "save", SaveID: "s-" + key}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		key := e.Issue("changed "+name, "orig").Key
		tok := takeUndo(t, func(c context.Context) error { _, err := e.Svc.Transition(c, e.Alice, key, resolve, nil); return err })
		time.Sleep(5 * time.Millisecond) // the database's own clock stamps rows: the change is later than the action
		change(key)
		before, _ := e.Svc.View(ctx, e.Alice, key)
		_, err := e.Svc.Undo(ctx, e.Alice, tok)
		var ch *domain.UndoChanged
		if !errors.As(err, &ch) {
			t.Fatalf("%s: want a changed-since refusal, got %v", name, err)
		}
		after, _ := e.Svc.View(ctx, e.Alice, key)
		if after.Issue.Status != before.Issue.Status || after.Issue.Version != before.Issue.Version {
			t.Fatalf("%s: something was reverted: %s v%d -> %s v%d", name, before.Issue.Status, before.Issue.Version, after.Issue.Status, after.Issue.Version)
		}
	}
	// watching is not a change
	key := e.Issue("watched", "").Key
	tok := takeUndo(t, func(c context.Context) error { _, err := e.Svc.Transition(c, e.Alice, key, resolve, nil); return err })
	if _, err := e.Svc.SetWatch(ctx, e.Bob, key, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Undo(ctx, e.Alice, tok); err != nil {
		t.Fatalf("watching counted as a change: %v", err)
	}
}

// UNDO-09: the action and its undo each leave an event in the outbox, in order; the undo is marked as one.
func TestUNDO09_EventsForTheActionAndItsUndo(t *testing.T) {
	e := undoable(t)
	key := e.Issue("events", "").Key
	tok := takeUndo(t, func(c context.Context) error {
		_, err := e.Svc.Transition(c, e.Alice, key, transitionID(t, e, "Resolve"), nil)
		return err
	})
	if _, err := e.Svc.Undo(ctx, e.Alice, tok); err != nil {
		t.Fatal(err)
	}
	rows, err := e.Store.Pool.Query(ctx, `SELECT type, COALESCE(payload->>'undo','') FROM outbox WHERE aggregate=$1 ORDER BY id`, "issue:"+key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var typ, undo string
		rows.Scan(&typ, &undo)
		got = append(got, typ+":"+undo)
	}
	if len(got) < 2 || got[len(got)-2] != "issue.transitioned:" || got[len(got)-1] != "issue.updated:true" {
		t.Fatalf("outbox: %v", got)
	}
}
