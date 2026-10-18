package domain_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func i64(n int64) *int64 { return &n }

func TestLIVE01_CompareAndSetExactlyOneWinner(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "start")
	const users = 50
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: fmt.Sprintf("v%d", i), BaseSeq: 1, SaveID: fmt.Sprintf("s%d", i)})
			mu.Lock()
			defer mu.Unlock()
			var tc *domain.TextConflict
			switch {
			case err == nil:
				wins++
			case errors.As(err, &tc):
				conflicts++
				if tc.CurrentSeq != 2 || !strings.HasPrefix(tc.Current, "v") || tc.Base != "start" {
					t.Errorf("conflict payload: %+v", tc)
				}
			default:
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || conflicts != users-1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if v.Issue.DescSeq != 2 {
		t.Fatalf("seq %d", v.Issue.DescSeq)
	}
}

func TestLIVE01_SequenceHasNoGapsUnderRetriesToFreshSeq(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "")
	var wg sync.WaitGroup
	acked := make(chan string, 200)
	for u := 0; u < 8; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			for n := 0; n < 5; n++ {
				for { // each user retries on conflict from the seq it was told about, like the dialog does
					v, _ := e.Svc.View(ctx, e.Alice, is.Key)
					body := v.Issue.Description + fmt.Sprintf("[u%dn%d]", u, n)
					_, err := e.Svc.SaveText(ctx, e.Bob, is.Key, domain.SaveIn{Field: "description", Body: body, BaseSeq: v.Issue.DescSeq})
					if err == nil {
						acked <- fmt.Sprintf("[u%dn%d]", u, n)
						break
					}
					var tc *domain.TextConflict
					if !errors.As(err, &tc) {
						t.Error(err)
						return
					}
				}
			}
		}(u)
	}
	wg.Wait()
	close(acked)
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	n := 0
	for tag := range acked {
		n++
		if !strings.Contains(v.Issue.Description, tag) { // E1: an acknowledged save must be present
			t.Fatalf("acknowledged save %s lost", tag)
		}
	}
	if n != 40 || v.Issue.DescSeq != 41 {
		t.Fatalf("acked %d, seq %d", n, v.Issue.DescSeq)
	}
	vs, _ := e.Svc.TextVersions(ctx, e.Alice, is.Key, "description")
	for i, tv := range vs { // seq is gapless and descending
		if tv.Seq != int64(41-i) {
			t.Fatalf("gap at %d: %d", i, tv.Seq)
		}
	}
}

func TestLIVE05_IdempotentRetry(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "a")
	in := domain.SaveIn{Field: "description", Body: "b", BaseSeq: 1, SaveID: "save-1"}
	r1, err := e.Svc.SaveText(ctx, e.Alice, is.Key, in)
	if err != nil || r1.Seq != 2 {
		t.Fatal(err, r1)
	}
	// lost acknowledgement: client retries the very same save
	r2, err := e.Svc.SaveText(ctx, e.Alice, is.Key, in)
	if err != nil || r2.Seq != 2 || !r2.Retried {
		t.Fatalf("%v %+v", err, r2)
	}
	vs, _ := e.Svc.TextVersions(ctx, e.Alice, is.Key, "description")
	if len(vs) != 2 {
		t.Fatalf("applied twice: %d versions", len(vs))
	}
	// a new save id from the stale base is a conflict, not a replay
	in.SaveID = "save-2"
	if _, err := e.Svc.SaveText(ctx, e.Alice, is.Key, in); err == nil {
		t.Fatal("stale save with new id accepted")
	}
}

func TestLIVE07_APIWrites(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "alpha beta gamma beta")
	bot := domain.Actor{ID: e.Alice.ID, Label: "Agent: claude", Kind: "agent"}
	// anchored patch: unique match applies
	if _, err := e.Svc.PatchText(ctx, bot, is.Key, "description", "alpha", "ALPHA"); err != nil {
		t.Fatal(err)
	}
	// ambiguous and missing fail with the current context
	for old, want := range map[string]int{"beta": 2, "zzz": 0} {
		_, err := e.Svc.PatchText(ctx, bot, is.Key, "description", old, "x")
		var pm *domain.PatchMismatch
		if !errors.As(err, &pm) || pm.Matches != want || pm.Current != "ALPHA beta gamma beta" || pm.CurrentSeq != 2 {
			t.Fatalf("%q: %v", old, err)
		}
	}
	// edits elsewhere in the field are preserved by a patch
	e.Svc.ReplaceText(ctx, e.Bob, is.Key, "description", "ALPHA beta gamma beta tail", nil)
	if _, err := e.Svc.PatchText(ctx, bot, is.Key, "description", "gamma", "G"); err != nil {
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if v.Issue.Description != "ALPHA beta G beta tail" {
		t.Fatal(v.Issue.Description)
	}
	// conditional replace: stale seq -> 412 with current text
	_, err := e.Svc.ReplaceText(ctx, bot, is.Key, "description", "x", i64(1))
	var tp *domain.TextPrecondition
	if !errors.As(err, &tp) || tp.CurrentSeq != v.Issue.DescSeq || tp.Current != v.Issue.Description {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.ReplaceText(ctx, bot, is.Key, "description", "fresh", i64(v.Issue.DescSeq)); err != nil {
		t.Fatal(err)
	}
	// bare replace: last write wins, and the overwritten text is in history and restorable
	before, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if _, err := e.Svc.ReplaceText(ctx, bot, is.Key, "description", "bare", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.RestoreText(ctx, e.Alice, is.Key, "description", before.Issue.DescSeq); err != nil {
		t.Fatal(err)
	}
	after, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if after.Issue.Description != "fresh" {
		t.Fatal(after.Issue.Description)
	}
}

func TestConflictFlow_KeepMineIsRestorableAndRecheckedAgainstLatest(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	base := "Steps:\n\n1. login\n\nExpected: pending\n\nNotes"
	is := e.Issue("s", base)
	theirs := "Steps:\n\n1. login\n\nExpected: pending\n\nNotes\n\nRetry note"
	if _, err := e.Svc.SaveText(ctx, e.Bob, is.Key, domain.SaveIn{Field: "description", Body: theirs, BaseSeq: 1}); err != nil {
		t.Fatal(err)
	}
	mine := "Steps:\n\n1. login clarified\n\nExpected: pending\n\nNotes"
	_, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: mine, BaseSeq: 1})
	var tc *domain.TextConflict
	if !errors.As(err, &tc) {
		t.Fatalf("want conflict: %v", err)
	}
	if tc.By != "Bob R." || tc.Base != base || tc.Current != theirs || len(tc.Merge.Conflicts) != 0 {
		t.Fatalf("%+v", tc)
	}
	want := "Steps:\n\n1. login clarified\n\nExpected: pending\n\nNotes\n\nRetry note"
	if tc.Merge.Merged != want || tc.Merge.TheirChanged != 1 || tc.Merge.MineChanged != 1 {
		t.Fatalf("merge: %+v", tc.Merge)
	}
	// someone saves again while the dialog is open: saving the merge against the old current seq repeats the dialog
	e.Svc.SaveText(ctx, e.Bob, is.Key, domain.SaveIn{Field: "description", Body: theirs + "!", BaseSeq: tc.CurrentSeq})
	_, err = e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: tc.Merge.Merged, BaseSeq: tc.CurrentSeq, Mode: "merge"})
	if !errors.As(err, &tc) {
		t.Fatalf("dialog must repeat: %v", err)
	}
	// keep mine against the newest seq succeeds and the overwritten version stays in history
	r, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: mine, BaseSeq: tc.CurrentSeq, Mode: "keep_mine"})
	if err != nil {
		t.Fatal(err)
	}
	vs, _ := e.Svc.TextVersions(ctx, e.Alice, is.Key, "description")
	found := false
	for _, v := range vs {
		if v.Body == theirs+"!" {
			found = true
		}
	}
	if !found || vs[0].Mode != "keep_mine" || vs[0].Seq != r.Seq {
		t.Fatalf("overwritten version not kept: %+v", vs)
	}
}

func TestLIVE13_Sanitization(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "")
	r, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", BaseSeq: 1,
		Body: "see [docs](javascript:alert(1)) and [ok](https://example.com) and [m](mailto:a@b.c)\x00\x07"})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if v.Issue.Description != "see docs and [ok](https://example.com) and [m](mailto:a@b.c)" {
		t.Fatalf("%q", v.Issue.Description)
	}
	if len(r.Removed) != 2 { // reported, never silent
		t.Fatalf("removed: %v", r.Removed)
	}
	big := strings.Repeat("x", 1<<20+1)
	if _, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", BaseSeq: 2, Body: big}); err == nil {
		t.Fatal("oversized text accepted")
	}
	if _, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "summary", BaseSeq: 1, Body: "two\nlines"}); err == nil {
		t.Fatal("multi-line summary accepted")
	}
}

func TestLIVE14_TextSavesNeverBumpVersion(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "")
	for i := 0; i < 10; i++ {
		if _, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: fmt.Sprint("v", i), BaseSeq: int64(1 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	hi := "high"
	got, err := e.Svc.UpdateFields(ctx, e.Bob, is.Key, i64(1), true, domain.Changes{Priority: &hi}) // strict If-Match on the original version: no false 412
	if err != nil || got.Version != 2 || got.DescSeq != 11 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestLIVE10_Drafts(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "orig")
	if err := e.Svc.SaveDraft(ctx, e.Alice, is.Key, "description", "typing...", 1); err != nil {
		t.Fatal(err)
	}
	ds, _ := e.Svc.GetDrafts(ctx, e.Alice, is.Key)
	if len(ds) != 1 || ds[0].Body != "typing..." || ds[0].Stale {
		t.Fatalf("%+v", ds)
	}
	if ds, _ := e.Svc.GetDrafts(ctx, e.Bob, is.Key); len(ds) != 0 { // private
		t.Fatal("draft visible to another user")
	}
	e.Svc.SaveText(ctx, e.Bob, is.Key, domain.SaveIn{Field: "description", Body: "bob", BaseSeq: 1})
	ds, _ = e.Svc.GetDrafts(ctx, e.Alice, is.Key)
	if !ds[0].Stale { // base moved: the client opens the conflict dialog
		t.Fatal("draft not marked stale")
	}
	// saving removes the saver's own draft
	e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: "typing...", BaseSeq: 2})
	if ds, _ := e.Svc.GetDrafts(ctx, e.Alice, is.Key); len(ds) != 0 {
		t.Fatal("draft survived save")
	}
}

func TestHIST_RecordsAndRestore(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "one")
	e.Svc.SaveText(ctx, e.Bob, is.Key, domain.SaveIn{Field: "description", Body: "two", BaseSeq: 1})
	e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "summary", Body: "renamed", BaseSeq: 1})
	h, _ := e.Svc.History(ctx, e.Alice, is.Key)
	var desc, sum *domain.HistoryEntry
	for i := range h {
		if h[i].Field == "description" {
			desc = &h[i]
		}
		if h[i].Field == "summary" {
			sum = &h[i]
		}
	}
	if desc == nil || *desc.Before != "one" || *desc.After != "two" || desc.Actor != "Bob R." {
		t.Fatalf("%+v", h)
	}
	if sum == nil || *sum.After != "renamed" || sum.Actor != "Alice K." {
		t.Fatalf("%+v", h)
	}
	if _, err := e.Svc.RestoreText(ctx, e.Alice, is.Key, "description", 1); err != nil {
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	vs, _ := e.Svc.TextVersions(ctx, e.Alice, is.Key, "description")
	if v.Issue.Description != "one" || v.Issue.DescSeq != 3 || vs[0].Mode != "restore" || len(vs) != 3 { // restore is a new change; nothing rewritten
		t.Fatalf("%+v %+v", v.Issue, vs)
	}
}

func TestPRES_AdvisoryPresence(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "")
	p := e.Svc.Presence
	p.Heartbeat(is.Key, e.Bob, "description", false)
	got := p.List(is.Key, e.Alice.ID)
	if len(got) != 1 || got[0].Name != "Bob R." || got[0].Editing != "description" {
		t.Fatalf("%+v", got)
	}
	e.Clock.Advance(125 * time.Second / 10) // 12.5 s
	if got := p.List(is.Key, e.Alice.ID); len(got) != 1 || got[0].EditingS != 12 {
		t.Fatalf("%+v", got)
	}
	// own entry is excluded; offline-in-editors users show up as viewers only (PRES-02)
	p.Heartbeat(is.Key, e.Alice, "", false)
	if got := p.List(is.Key, e.Alice.ID); len(got) != 1 {
		t.Fatal("own entry listed")
	}
	p.Heartbeat(is.Key, e.Bob, "description", true)
	if got := p.List(is.Key, e.Alice.ID); got[0].Editing != "" {
		t.Fatalf("offline user shown editing: %+v", got)
	}
	// expiry after 30 s without a heartbeat
	e.Clock.Advance(31 * time.Second)
	if got := p.List(is.Key, ""); len(got) != 0 {
		t.Fatalf("not expired: %+v", got)
	}
	// presence never blocks saving (PRES-03/04): a save works while someone else "is editing"
	p.Heartbeat(is.Key, e.Bob, "description", false)
	if _, err := e.Svc.SaveText(ctx, e.Alice, is.Key, domain.SaveIn{Field: "description", Body: "x", BaseSeq: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestComments(t *testing.T) { // CORE-07
	e := testutil.New(t)
	e.Project()
	is := e.Issue("s", "")
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := e.Alice
			if i%2 == 0 {
				a = e.Bob
			}
			if _, err := e.Svc.AddComment(ctx, a, is.Key, fmt.Sprint("c", i), false); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if len(v.Comments) != 10 { // comments never conflict
		t.Fatalf("%d comments", len(v.Comments))
	}
	c := v.Comments[0]
	author := e.Alice
	if c.Author.ID == e.Bob.ID {
		author = e.Bob
	}
	other := e.Bob
	if author.ID == e.Bob.ID {
		other = e.Alice
	}
	if err := e.Svc.EditComment(ctx, other, is.Key, c.ID, "hijack"); err == nil {
		t.Fatal("edited someone else's comment")
	}
	if err := e.Svc.EditComment(ctx, author, is.Key, c.ID, "edited"); err != nil {
		t.Fatal(err)
	}
}
