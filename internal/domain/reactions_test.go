package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func reactEnv(t *testing.T) (*testutil.Env, string, string) {
	e := testutil.New(t)
	e.Project()
	key := e.Issue("react", "").Key
	c, err := e.Svc.AddComment(ctx, e.Alice, key, "a comment", false)
	if err != nil {
		t.Fatal(err)
	}
	return e, key, c.ID
}

// REACT-01: each of the eight on and off; one per person per emoji; several people; everything outside the set refused.
func TestREACT01_TheEightOnePerPersonNothingElse(t *testing.T) {
	e, key, cid := reactEnv(t)
	for _, em := range domain.Reactions {
		rs, err := e.Svc.React(ctx, e.Alice, key, cid, em, true)
		if err != nil {
			t.Fatalf("%s: %v", em, err)
		}
		if _, err := e.Svc.React(ctx, e.Alice, key, cid, em, true); err != nil { // the same emoji twice by one person is still one
			t.Fatal(err)
		}
		found := false
		for _, r := range rs {
			if r.Emoji == em && r.Count == 1 && r.Mine {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: %+v", em, rs)
		}
	}
	rs, _ := e.Svc.React(ctx, e.Bob, key, cid, "👍", true)
	if rs[0].Emoji != "👍" || rs[0].Count != 2 || rs[0].Mine != true || len(rs) != 8 || strings.Join(rs[0].Names, ",") != "Alice K.,Bob R." {
		t.Fatalf("two people: %+v", rs[0])
	}
	rs, _ = e.Svc.React(ctx, e.Alice, key, cid, "👍", false)
	if rs[0].Count != 1 || strings.Join(rs[0].Names, ",") != "Bob R." {
		t.Fatalf("taken back: %+v", rs[0])
	}
	if _, err := e.Svc.React(ctx, e.Alice, key, cid, "👍", false); err != nil { // taking back what is not there is fine
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, key)
	if len(v.Comments[0].Reactions) != 8 || v.Comments[0].Reactions[0].Mine {
		t.Fatalf("view: %+v", v.Comments[0].Reactions)
	}
	for _, bad := range []string{"", "+1", "👍🏽", "❤", "🦄", "👍👍", "<b>x</b>", "👍‍"} {
		var inv *domain.InvalidError
		if _, err := e.Svc.React(ctx, e.Alice, key, cid, bad, true); !errors.As(err, &inv) {
			t.Errorf("%q: want invalid, got %v", bad, err)
		}
	}
}

// REACT-02: only people who can see the comment react: a viewer may; nobody outside the project; an internal comment only for
// those who read internal comments; archived issues are read-only; agents do not react.
func TestREACT02_WhoMayReact(t *testing.T) {
	e, key, cid := reactEnv(t)
	vera, err := e.Svc.CreateUser(ctx, "vera@example.com", "Vera V.", "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	e.Svc.AddMember(ctx, e.Admin, "ABC", vera.ID, "viewer")
	viewer := domain.Actor{ID: vera.ID, Label: "Vera V.", Kind: "human"}
	if _, err := e.Svc.React(ctx, viewer, key, cid, "👀", true); err != nil {
		t.Fatalf("a viewer who sees the comment: %v", err)
	}
	stranger, _ := e.Svc.CreateUser(ctx, "s@example.com", "Stranger", "password123", false)
	if _, err := e.Svc.React(ctx, domain.Actor{ID: stranger.ID, Label: "Stranger", Kind: "human"}, key, cid, "👀", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("someone outside the project: %v", err)
	}
	in, _ := e.Svc.AddComment(ctx, e.Alice, key, "staff only", true)
	if _, err := e.Svc.React(ctx, viewer, key, in.ID, "👀", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a viewer reacting to an internal comment: %v", err)
	}
	if _, err := e.Svc.React(ctx, e.Bob, key, in.ID, "👀", true); err != nil {
		t.Fatalf("staff on an internal comment: %v", err)
	}
	if _, err := e.Svc.React(ctx, e.Alice, key, "00000000-0000-0000-0000-000000000000", "👀", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unknown comment: %v", err)
	}
	other := e.Issue("other", "").Key
	if _, err := e.Svc.React(ctx, e.Alice, other, cid, "👀", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a comment of another issue: %v", err)
	}
	if _, err := e.Svc.React(ctx, domain.Actor{ID: e.Alice.ID, Label: "agent", Kind: "agent"}, key, cid, "👀", true); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an agent: %v", err)
	}
	resolve := transitionID(t, e, "Resolve")
	if _, err := e.Svc.Transition(ctx, e.Alice, key, resolve, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ArchiveIssue(ctx, e.Alice, key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.React(ctx, e.Alice, key, cid, "🎉", true); err == nil {
		t.Fatal("reacted on an archived issue")
	}
}

// REACT-04: a reaction changes nothing on the issue (version, updated time, history) and leaves nothing to notify anyone with.
func TestREACT04_NoVersionNoUpdatedNoNotification(t *testing.T) {
	e, key, cid := reactEnv(t)
	before, _ := e.Svc.View(ctx, e.Alice, key)
	var outbox, hist int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outbox)
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_history`).Scan(&hist)
	if _, err := e.Svc.React(ctx, e.Bob, key, cid, "🚀", true); err != nil {
		t.Fatal(err)
	}
	after, _ := e.Svc.View(ctx, e.Alice, key)
	if after.Issue.Version != before.Issue.Version || !after.Issue.Updated.Equal(before.Issue.Updated) {
		t.Fatalf("the issue changed: v%d -> v%d, %v -> %v", before.Issue.Version, after.Issue.Version, before.Issue.Updated, after.Issue.Updated)
	}
	var outbox2, hist2 int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outbox2)
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_history`).Scan(&hist2)
	if outbox2 != outbox || hist2 != hist {
		t.Fatalf("outbox %d -> %d, history %d -> %d", outbox, outbox2, hist, hist2)
	}
}

// REACT-05: reactions go with their comment (deleted with it, kept through a move and the trash) and with their person (in the
// export, gone after erasure).
func TestREACT05_ReactionsFollowCommentAndPerson(t *testing.T) {
	e, key, cid := reactEnv(t)
	e.Svc.React(ctx, e.Bob, key, cid, "❤️", true)
	e.Svc.React(ctx, e.Alice, key, cid, "😄", true)
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Xray"); err != nil {
		t.Fatal(err)
	}
	e.Svc.AddMember(ctx, e.Admin, "XYZ", e.Alice.ID, "admin")
	e.Svc.AddMember(ctx, e.Admin, "XYZ", e.Bob.ID, "member")
	plan, err := e.Svc.Lifecycle(ctx, e.Alice, key, domain.LifecycleIn{Op: "move", Project: "XYZ"})
	if err != nil {
		t.Fatal(err)
	}
	moved := plan.Key2
	if moved == "" {
		moved = plan.NewKeys[key]
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, moved, domain.LifecycleIn{Op: "delete"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Lifecycle(ctx, e.Alice, moved, domain.LifecycleIn{Op: "restore"}); err != nil {
		t.Fatal(err)
	}
	v, err := e.Svc.View(ctx, e.Bob, moved)
	if err != nil || len(v.Comments[0].Reactions) != 2 {
		t.Fatalf("after move, trash and restore: %v %+v", err, v.Comments)
	}
	d, err := e.Svc.ExportPersonalData(ctx, e.Admin, e.Bob.ID)
	if b, _ := json.Marshal(d["reactions"]); err != nil || !strings.Contains(string(b), moved) || !strings.Contains(string(b), "❤️") {
		t.Fatalf("export: %v %v", err, d["reactions"])
	}
	if _, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{}); err != nil {
		t.Fatal(err)
	}
	v, _ = e.Svc.View(ctx, e.Alice, moved)
	if len(v.Comments[0].Reactions) != 1 || v.Comments[0].Reactions[0].Emoji != "😄" {
		t.Fatalf("after erasure: %+v", v.Comments[0].Reactions)
	}
	e.Store.Pool.Exec(ctx, `DELETE FROM comment WHERE id=$1`, cid)
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM comment_reaction`).Scan(&n)
	if n != 0 {
		t.Fatalf("reactions outlived their comment: %d", n)
	}
}
