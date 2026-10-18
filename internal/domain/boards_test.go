package domain_test

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func boardKeys(v domain.BoardView, col string) []string {
	for _, c := range v.Columns {
		if c.Name == col {
			var out []string
			for _, k := range c.Cards {
				out = append(out, k.Key)
			}
			return out
		}
	}
	return nil
}

func newBoard(t *testing.T, e *testutil.Env, kind string) domain.Board {
	b, err := e.Svc.CreateBoard(ctx, e.Admin, "ABC", "Team board", kind)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBOARD_CreateColumnsAndPermissions(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	if _, err := e.Svc.CreateBoard(ctx, e.Alice, "ABC", "x", "kanban"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member made a board: %v", err)
	}
	out := e.Svc.AddMember
	_ = out
	outsider, _ := e.Svc.CreateUser(ctx, "out@example.com", "Out", "password123", false)
	oa := domain.Actor{ID: outsider.ID, Label: "Out", Kind: "human"}
	if _, err := e.Svc.CreateBoard(ctx, oa, "ABC", "x", "kanban"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider learned the project exists: %v", err)
	}
	if _, err := e.Svc.CreateBoard(ctx, e.Admin, "ABC", "x", "nonsense"); err == nil {
		t.Fatal("a bad kind was accepted")
	}
	b := newBoard(t, e, "scrum")
	if _, err := e.Svc.CreateBoard(ctx, e.Admin, "ABC", "Team board", "kanban"); err == nil || !strings.Contains(err.Error(), "already has a board") {
		t.Fatalf("duplicate name: %v", err)
	}
	k1 := mk(t, e, e.Alice, "Task", "one", "")
	v, err := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Columns) != 3 || v.Columns[0].Name != "To Do" || v.Columns[2].Name != "Done" { // one column per workflow status, in order
		t.Fatalf("%+v", v.Columns)
	}
	if got := boardKeys(v, "To Do"); len(got) != 1 || got[0] != k1 {
		t.Fatalf("%v", got)
	}
	if _, err := e.Svc.GetBoard(ctx, oa, b.ID, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an outsider read a board: %v", err)
	}
	if _, err := e.Svc.GetBoard(ctx, e.Alice, "00000000-0000-0000-0000-000000000000", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("unknown board")
	}
	if _, err := e.Svc.GetBoard(ctx, e.Alice, "not-a-uuid", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("malformed id")
	}
	name := "Renamed"
	if _, err := e.Svc.UpdateBoard(ctx, e.Alice, b.ID, domain.BoardPatch{Name: &name}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member edited the board: %v", err)
	}
	bad := "weekly"
	if _, err := e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Swimlane: &bad}); err == nil {
		t.Fatal("a bad swimlane was accepted")
	}
	if nb, err := e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Name: &name}); err != nil || nb.Name != "Renamed" {
		t.Fatalf("%v %+v", err, nb)
	}
	if bs, _ := e.Svc.ListBoards(ctx, e.Alice, "ABC"); len(bs) != 1 || bs[0].Name != "Renamed" {
		t.Fatalf("%+v", bs)
	}
}

func TestBOARD_ColumnsWIPAndUnmapped(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := newBoard(t, e, "kanban")
	one, two := 1, 2
	cols := []domain.ColumnIn{
		{Name: "Backlog", Statuses: []string{"To Do"}, MinWIP: &one},
		{Name: "Doing", Statuses: []string{"In Progress"}, MaxWIP: &one},
	}
	if err := e.Svc.SetColumns(ctx, e.Alice, b.ID, cols); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	for name, bad := range map[string][]domain.ColumnIn{
		"none":           {},
		"duplicate name": {{Name: "A", Statuses: []string{"To Do"}}, {Name: "a"}},
		"status twice":   {{Name: "A", Statuses: []string{"To Do"}}, {Name: "B", Statuses: []string{"To Do"}}},
		"unknown status": {{Name: "A", Statuses: []string{"Nope"}}},
		"min above max":  {{Name: "A", MinWIP: &two, MaxWIP: &one}},
	} {
		if err := e.Svc.SetColumns(ctx, e.Admin, b.ID, bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := e.Svc.SetColumns(ctx, e.Admin, b.ID, cols); err != nil {
		t.Fatal(err)
	}
	k1, k2, k3 := mk(t, e, e.Alice, "Task", "a", ""), mk(t, e, e.Alice, "Task", "b", ""), mk(t, e, e.Alice, "Task", "c", "")
	for _, k := range []string{k1, k2} {
		v, _ := e.Svc.View(ctx, e.Alice, k)
		e.Svc.Transition(ctx, e.Alice, k, tidTo(t, v, "In Progress"), nil)
	}
	v3, _ := e.Svc.View(ctx, e.Alice, k3)
	e.Svc.Transition(ctx, e.Alice, k3, tidTo(t, v3, "Done"), nil) // Done is on no column
	bv, _ := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if bv.Columns[1].Breach != "over" || bv.Columns[1].Count != 2 { // two cards, limit one
		t.Fatalf("%+v", bv.Columns[1])
	}
	if bv.Columns[0].Breach != "under" { // no cards, minimum one
		t.Fatalf("%+v", bv.Columns[0])
	}
	if len(bv.Unmapped) != 1 || bv.Unmapped[0].Key != k3 {
		t.Fatalf("a card on an unmapped status vanished: %+v", bv.Unmapped)
	}
	// finished work older than done_days leaves the board but not the project
	e.Store.Pool.Exec(ctx, `UPDATE issue SET resolved_at = now() - interval '30 days' WHERE key=$1`, k3)
	if bv, _ = e.Svc.GetBoard(ctx, e.Alice, b.ID, nil); len(bv.Unmapped) != 0 {
		t.Fatalf("old done work still shown: %+v", bv.Unmapped)
	}
	// the filter keys (computed by the API from TQL) restrict the cards
	bv, _ = e.Svc.GetBoard(ctx, e.Alice, b.ID, []string{k1})
	if len(bv.Columns[1].Cards) != 1 || bv.Columns[1].Cards[0].Key != k1 {
		t.Fatalf("%+v", bv.Columns[1].Cards)
	}
	if bv, _ = e.Svc.GetBoard(ctx, e.Alice, b.ID, []string{}); bv.Columns[1].Count != 0 {
		t.Fatal("an empty filter result should show nothing")
	}
}

func tidTo(t *testing.T, v domain.IssueView, to string) string {
	for _, tr := range v.Transitions {
		if tr.To == to {
			return tr.ID
		}
	}
	t.Fatalf("no transition to %s in %+v", to, v.Transitions)
	return ""
}

func TestBOARD_MoveCardTransitionsAndRanks(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := newBoard(t, e, "kanban")
	var keys []string
	for _, s := range []string{"a", "b", "c", "d"} {
		keys = append(keys, mk(t, e, e.Alice, "Task", s, ""))
	}
	a, bb, c, d := keys[0], keys[1], keys[2], keys[3]
	// reorder inside a column, no transition
	r, err := e.Svc.MoveCard(ctx, e.Alice, b.ID, d, domain.MoveIn{Column: "To Do", Before: a})
	if err != nil || r.Transitioned {
		t.Fatalf("%v %+v", err, r)
	}
	v, _ := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if got := strings.Join(boardKeys(v, "To Do"), " "); got != strings.Join([]string{d, a, bb, c}, " ") {
		t.Fatalf("send to top: %s", got)
	}
	e.Svc.MoveCard(ctx, e.Alice, b.ID, d, domain.MoveIn{Column: "To Do", After: bb})
	v, _ = e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if got := strings.Join(boardKeys(v, "To Do"), " "); got != strings.Join([]string{a, bb, d, c}, " ") {
		t.Fatalf("after: %s", got)
	}
	e.Svc.MoveCard(ctx, e.Alice, b.ID, a, domain.MoveIn{Column: "To Do"}) // to the end
	v, _ = e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if got := strings.Join(boardKeys(v, "To Do"), " "); got != strings.Join([]string{bb, d, c, a}, " ") {
		t.Fatalf("end: %s", got)
	}
	// dragging to another column is a transition, and the order persists across reads
	if r, err = e.Svc.MoveCard(ctx, e.Alice, b.ID, c, domain.MoveIn{Column: "In Progress"}); err != nil || !r.Transitioned || r.TransitionVia != "Start progress" || r.Issue.Status != "In Progress" {
		t.Fatalf("%v %+v", err, r)
	}
	e.Svc.MoveCard(ctx, e.Alice, b.ID, a, domain.MoveIn{Column: "In Progress", Before: c})
	v, _ = e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if got := strings.Join(boardKeys(v, "In Progress"), " "); got != a+" "+c {
		t.Fatalf("%s", got)
	}
	if got := strings.Join(boardKeys(v, "To Do"), " "); got != bb+" "+d {
		t.Fatalf("%s", got)
	}
	if is, _ := e.Svc.View(ctx, e.Alice, a); is.Issue.Status != "In Progress" {
		t.Fatal("the card did not transition")
	}
	// a neighbour that is in another column is refused, and nothing changes
	if _, err := e.Svc.MoveCard(ctx, e.Alice, b.ID, d, domain.MoveIn{Column: "To Do", After: a}); err == nil || !strings.Contains(err.Error(), "is not in column") {
		t.Fatalf("%v", err)
	}
	// a move the workflow does not allow explains why and where the card can go
	e.Svc.MoveCard(ctx, e.Alice, b.ID, c, domain.MoveIn{Column: "Done"})
	_, err = e.Svc.MoveCard(ctx, e.Alice, b.ID, c, domain.MoveIn{Column: "In Progress"})
	if err == nil || !strings.Contains(err.Error(), "no transition from \"Done\"") || !strings.Contains(err.Error(), "To Do") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.MoveCard(ctx, e.Alice, b.ID, c, domain.MoveIn{Column: "Nowhere"}); err == nil {
		t.Fatal("unknown column")
	}
	// roles: viewers cannot move, outsiders learn nothing, other projects' issues are refused
	viewer, _ := e.Svc.CreateUser(ctx, "v@example.com", "V", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	va := domain.Actor{ID: viewer.ID, Label: "V", Kind: "human"}
	if _, err := e.Svc.MoveCard(ctx, va, b.ID, bb, domain.MoveIn{Column: "In Progress"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.GetBoard(ctx, va, b.ID, nil); err != nil {
		t.Fatalf("a viewer should read the board: %v", err)
	}
	e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Other")
	other, _ := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Type: "Task", Summary: "elsewhere"})
	if _, err := e.Svc.MoveCard(ctx, e.Admin, b.ID, other.Key, domain.MoveIn{Column: "To Do"}); err == nil || !strings.Contains(err.Error(), "not on this board") {
		t.Fatalf("%v", err)
	}
}

// Two boards over one project keep their own order (SPEC 6: rank is stored per board).
func TestBOARD_RankIsPerBoard(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b1 := newBoard(t, e, "kanban")
	b2, _ := e.Svc.CreateBoard(ctx, e.Admin, "ABC", "Second", "kanban")
	x, y := mk(t, e, e.Alice, "Task", "x", ""), mk(t, e, e.Alice, "Task", "y", "")
	e.Svc.MoveCard(ctx, e.Alice, b1.ID, y, domain.MoveIn{Column: "To Do", Before: x})
	v1, _ := e.Svc.GetBoard(ctx, e.Alice, b1.ID, nil)
	v2, _ := e.Svc.GetBoard(ctx, e.Alice, b2.ID, nil)
	if got := strings.Join(boardKeys(v1, "To Do"), " "); got != y+" "+x {
		t.Fatalf("board 1: %s", got)
	}
	if got := strings.Join(boardKeys(v2, "To Do"), " "); got != x+" "+y {
		t.Fatalf("board 2 changed with board 1: %s", got)
	}
}

// Hammering one spot grows the rank string; past a bound the column is spread out again and the order is kept.
func TestBOARD_RankRebalance(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := newBoard(t, e, "kanban")
	x, y, z := mk(t, e, e.Alice, "Task", "x", ""), mk(t, e, e.Alice, "Task", "y", ""), mk(t, e, e.Alice, "Task", "z", "")
	e.Svc.MoveCard(ctx, e.Alice, b.ID, x, domain.MoveIn{Column: "To Do"})
	e.Svc.MoveCard(ctx, e.Alice, b.ID, y, domain.MoveIn{Column: "To Do"})
	long := strings.Repeat("1", 40)
	e.Store.Pool.Exec(ctx, `UPDATE board_rank SET rank=$2 WHERE issue_id=(SELECT id FROM issue WHERE key=$1)`, x, "0000"+long)
	e.Store.Pool.Exec(ctx, `UPDATE board_rank SET rank=$2 WHERE issue_id=(SELECT id FROM issue WHERE key=$1)`, y, "0000"+long+"1")
	r, err := e.Svc.MoveCard(ctx, e.Alice, b.ID, z, domain.MoveIn{Column: "To Do", After: x})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rank) != 8 {
		t.Fatalf("the column was not rebalanced: %q", r.Rank)
	}
	v, _ := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	if got := strings.Join(boardKeys(v, "To Do"), " "); got != x+" "+z+" "+y {
		t.Fatalf("order after rebalance: %s", got)
	}
}

func TestBOARD_AgentsRespectHumanGatesAndLanesAndProgress(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := newBoard(t, e, "kanban")
	epic := mk(t, e, e.Alice, "Epic", "the epic", "")
	st := mk(t, e, e.Alice, "Story", "story", epic)
	sub := mk(t, e, e.Alice, "Subtask", "sub", st)
	_ = sub
	sw := "stories"
	e.Svc.UpdateBoard(ctx, e.Admin, b.ID, domain.BoardPatch{Swimlane: &sw})
	v, _ := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
	var card domain.Card
	for _, c := range v.Columns[0].Cards {
		if c.Key == st {
			card = c
		}
	}
	if card.Subtasks != 1 || card.SubtasksDone != 0 || card.Parent != epic || len(v.Lanes) == 0 {
		t.Fatalf("%+v lanes %+v", card, v.Lanes)
	}
	// an agent's drag obeys human gates
	ag, _ := e.Svc.CreateAgent(ctx, e.Admin, "bot")
	e.Svc.AddMember(ctx, e.Admin, "ABC", ag.ID, "member")
	aa := domain.Actor{ID: ag.ID, Label: "Agent: bot", Kind: "agent"}
	var tid string
	e.Store.Pool.QueryRow(ctx, `SELECT t.id FROM transition t JOIN status s ON s.id=t.to_status WHERE s.name='Done' AND t.name='Resolve'`).Scan(&tid)
	e.Svc.SetTransitionHuman(ctx, e.Admin, tid, true)
	if _, err := e.Svc.MoveCard(ctx, aa, b.ID, st, domain.MoveIn{Column: "Done"}); err == nil || !strings.Contains(err.Error(), "needs a person") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.MoveCard(ctx, e.Alice, b.ID, st, domain.MoveIn{Column: "Done"}); err != nil {
		t.Fatalf("a person may: %v", err)
	}
}

// PERF-04: a board of 500 issues is served in under 100 ms (p95) at the service layer, and a drag stays fast.
func TestPERF04_BoardOf500Issues(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	b := newBoard(t, e, "kanban")
	var keys []string
	for i := 0; i < 500; i++ {
		is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "card " + strings.Repeat("x", i%7), Labels: []string{"l"}})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, is.Key)
	}
	var ds []time.Duration
	for i := 0; i < 40; i++ {
		s := time.Now()
		v, err := e.Svc.GetBoard(ctx, e.Alice, b.ID, nil)
		ds = append(ds, time.Since(s))
		if err != nil || v.Columns[0].Count != 500 {
			t.Fatalf("%v %d", err, v.Columns[0].Count)
		}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p := ds[len(ds)*95/100]
	var md []time.Duration
	for i := 0; i < 30; i++ {
		s := time.Now()
		if _, err := e.Svc.MoveCard(ctx, e.Alice, b.ID, keys[len(keys)-1-i], domain.MoveIn{Column: "To Do", Before: keys[0]}); err != nil {
			t.Fatal(err)
		}
		md = append(md, time.Since(s))
	}
	sort.Slice(md, func(i, j int) bool { return md[i] < md[j] })
	t.Logf("board of 500: p95 %v; drag within a column p95 %v", p, md[len(md)*95/100])
	if p > 100*time.Millisecond {
		t.Fatalf("board p95 %v exceeds 100 ms", p)
	}
	if md[len(md)*95/100] > 150*time.Millisecond {
		t.Fatalf("drag p95 %v", md[len(md)*95/100])
	}
}
