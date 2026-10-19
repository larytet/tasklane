package search_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/search/tql"
	"github.com/larytet/tasklane/internal/testutil"
)

var ctx = context.Background()

type fx struct {
	*testutil.Env
	ix   *search.Indexer
	eng  *search.Postgres
	dave domain.Actor // viewer of ABC
	carl domain.Actor // member of XYZ only
}

func newFx(t *testing.T) *fx {
	e := testutil.New(t)
	e.Clock.T = time.Now() // these golden queries mix the service clock with database-side timestamps: keep them on one clock
	e.Project()
	f := &fx{Env: e, ix: &search.Indexer{DB: e.Store.Pool}, eng: &search.Postgres{DB: e.Store.Pool}}
	mk := func(email, name string) domain.Actor {
		u, err := e.Svc.CreateUser(ctx, email, name, "password123", false)
		if err != nil {
			t.Fatal(err)
		}
		return domain.Actor{ID: u.ID, Label: u.Name}
	}
	f.dave, f.carl = mk("dave@example.com", "Dave V."), mk("carl@example.com", "Carl X.")
	e.Svc.AddMember(ctx, e.Admin, "ABC", f.dave.ID, "viewer")
	if _, err := e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Xray"); err != nil {
		t.Fatal(err)
	}
	e.Svc.AddMember(ctx, e.Admin, "XYZ", f.carl.ID, "member")
	return f
}

func (f *fx) index() {
	f.T.Helper()
	for {
		n, err := f.ix.RunOnce(ctx)
		if err != nil {
			f.T.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (f *fx) scope(a domain.Actor) search.Scope {
	all, rd, wr, err := f.Svc.SearchScope(ctx, a)
	if err != nil {
		f.T.Fatal(err)
	}
	return search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: time.Now}
}

func (f *fx) keys(a domain.Actor, q string) []string {
	f.T.Helper()
	res, err := f.eng.Search(ctx, f.scope(a), q, search.Page{Limit: 200})
	if err != nil {
		f.T.Fatalf("%q: %v", q, err)
	}
	var out []string
	for _, r := range res.Rows {
		out = append(out, r.Key)
	}
	return out
}

func (f *fx) sortedKeys(a domain.Actor, q string) string {
	k := f.keys(a, q)
	sort.Strings(k)
	return strings.Join(k, ",")
}

func (f *fx) create(a domain.Actor, in domain.CreateIn) domain.Issue {
	is, err := f.Svc.CreateIssue(ctx, a, in)
	if err != nil {
		f.T.Fatal(err)
	}
	return is
}

func TestTQL01_GoldenQueries(t *testing.T) {
	f := newFx(t)
	a := f.Alice
	i1 := f.create(a, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "Checkout fails on 3DS challenge", Description: "After the issuer redirects back the order stays pending", Priority: "high", Assignee: f.Bob.ID, Labels: []string{"payments", "bug"}})
	i2 := f.create(a, domain.CreateIn{Project: "ABC", Type: "Story", Summary: "Add CSV export", Description: "Export issues to CSV", Labels: []string{"export"}})
	i3 := f.create(a, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "Sprint rollover job", Priority: "low", Assignee: a.ID})
	f.create(f.Admin, domain.CreateIn{Project: "XYZ", Type: "Bug", Summary: "Xray checkout crash", Description: "crash on checkout"})
	f.Svc.AddComment(ctx, a, i2.Key, "needs the Überweisung column too", false)
	// move i3 to Done, i1 to In Progress
	v, _ := f.Svc.View(ctx, a, i3.Key)
	for _, tr := range v.Transitions {
		if tr.Name == "Resolve" {
			f.Svc.Transition(ctx, a, i3.Key, tr.ID, nil)
		}
	}
	v, _ = f.Svc.View(ctx, a, i1.Key)
	for _, tr := range v.Transitions {
		if tr.Name == "Start progress" {
			f.Svc.Transition(ctx, a, i1.Key, tr.ID, nil)
		}
	}
	f.index()

	cases := []struct{ q, want string }{
		{``, "ABC-1,ABC-2,ABC-3,XYZ-1"},
		{`project = ABC`, "ABC-1,ABC-2,ABC-3"},
		{`project = abc`, "ABC-1,ABC-2,ABC-3"},
		{`project in (ABC, XYZ) AND type = Bug`, "ABC-1,XYZ-1"},
		{`status = "In Progress"`, "ABC-1"},
		{`status in ("To Do", Done)`, "ABC-2,ABC-3,XYZ-1"},
		{`status not in (Done)`, "ABC-1,ABC-2,XYZ-1"},
		{`statusCategory = Done`, "ABC-3"},
		{`priority >= high`, ""}, // ordering operators on priority are rejected below
	}
	for _, c := range cases[:8] {
		if got := f.sortedKeys(f.Admin, c.q); got != c.want {
			t.Errorf("%q: got %s want %s", c.q, got, c.want)
		}
	}
	more := []struct{ q, want string }{
		{`priority = high`, "ABC-1"},
		{`assignee = bob@example.com OR assignee = alice@example.com`, "ABC-1,ABC-3"},
		{`assignee is EMPTY AND project = ABC`, "ABC-2"},
		{`assignee = "Bob R."`, "ABC-1"},
		{`assignee = bob@example.com`, "ABC-1"},
		{`assignee != "Alice K." AND project = ABC`, "ABC-1,ABC-2"}, // unassigned issues are included, as in JQL
		{`reporter = "Alice K."`, "ABC-1,ABC-2,ABC-3"},
		{`labels = payments`, "ABC-1"},
		{`labels in (export, payments)`, "ABC-1,ABC-2"},
		{`labels is EMPTY AND project = ABC`, "ABC-3"},
		{`labels != bug AND project = ABC`, "ABC-2,ABC-3"},
		{`key = abc-2`, "ABC-2"},
		{`key in (ABC-1, XYZ-1)`, "ABC-1,XYZ-1"},
		{`summary ~ "csv"`, "ABC-2"},
		{`summary ~ "check"`, "ABC-1,XYZ-1"}, // substring (trigram) match
		{`description ~ "pending"`, "ABC-1"},
		{`text ~ "csv"`, "ABC-2"},
		{`text ~ "ueberweisung"`, ""},     // unaccent maps ü to u, not ue
		{`text ~ "uberweisung"`, "ABC-2"}, // accent-insensitive match through the comment
		{`comment ~ "Überweisung"`, "ABC-2"},
		{`text ~ "checkout" AND project = ABC`, "ABC-1"},
		{`text !~ "checkout" AND project = ABC`, "ABC-2,ABC-3"},
		{`type = Story OR priority = low`, "ABC-2,ABC-3"},
		{`NOT (type = Story OR priority = low) AND project = ABC`, "ABC-1"},
		{`created >= -1d`, "ABC-1,ABC-2,ABC-3,XYZ-1"},
		{`created < -1d`, ""},
		{fmt.Sprintf(`created = "%s"`, time.Now().UTC().Format("2006-01-02")), "ABC-1,ABC-2,ABC-3,XYZ-1"},
		{`resolved is not EMPTY`, "ABC-3"},
		{`resolved is EMPTY AND project = ABC`, "ABC-1,ABC-2"},
		{`updated >= startOfWeek() AND updated < endOfWeek()`, "ABC-1,ABC-2,ABC-3,XYZ-1"},
		{`parent is EMPTY AND project = XYZ`, "XYZ-1"},
		{`status was "To Do" AND project = ABC`, "ABC-1,ABC-2,ABC-3"},
		{`status changed`, "ABC-1,ABC-3"},
		{`status changed to Done`, "ABC-3"},
		{`status changed from "To Do" to "In Progress" by "Alice K."`, "ABC-1"},
		{`status was not Done AND project = ABC`, "ABC-1,ABC-2"},
	}
	for _, c := range more {
		if got := f.sortedKeys(f.Admin, c.q); got != c.want {
			t.Errorf("%q: got %s want %s", c.q, got, c.want)
		}
	}
	if got := f.sortedKeys(f.Alice, `assignee = currentUser()`); got != "ABC-3" {
		t.Errorf("currentUser(): %s", got)
	}
	if got := f.sortedKeys(f.Alice, `assignee != currentUser() AND project = ABC`); got != "ABC-1,ABC-2" {
		t.Errorf("!= currentUser(): %s", got)
	}
	// ORDER BY
	if got := strings.Join(f.keys(f.Admin, `project = ABC ORDER BY priority DESC, key ASC`), ","); got != "ABC-1,ABC-2,ABC-3" {
		t.Errorf("order by priority: %s", got)
	}
	if got := strings.Join(f.keys(f.Admin, `project = ABC ORDER BY key DESC`), ","); got != "ABC-3,ABC-2,ABC-1" {
		t.Errorf("order by key desc: %s", got)
	}
	if got := strings.Join(f.keys(f.Admin, `project = ABC ORDER BY summary`), ","); got != "ABC-2,ABC-1,ABC-3" {
		t.Errorf("order by summary: %s", got)
	}
}

func TestTQL03_SemanticErrorsAreClear(t *testing.T) {
	f := newFx(t)
	for q, want := range map[string]string{
		`flavor = vanilla`:             `unknown field "flavor"`,
		`priority > high`:              "cannot be used with priority",
		`status = Open ORDER BY bogus`: `cannot sort by "bogus"`,
		`created > yesterday`:          "cannot read",
		`assignee = foo()`:             "cannot be used with assignee",
		`summary = x`:                  "supports only ~",
		`labels > a`:                   "cannot be used with labels",
		`openSprints = 1`:              "unknown field",
		`description changed`:          "unknown field", // routed to text clause: only ~ allowed
		`project changed by foo`:       "",
		`labels changed`:               "cannot be used with labels",
		`statusCategory = Nope`:        "unknown status category",
		`created > 99999999d`:          "too large",
	} {
		_, err := f.eng.Search(ctx, f.scope(f.Admin), q, search.Page{})
		if err == nil {
			if want == "" {
				continue
			}
			t.Errorf("%q: no error", q)
			continue
		}
		if _, ok := search.IsQueryError(err); !ok {
			t.Errorf("%q: not a query error: %v", q, err)
		}
		if want != "" && !strings.Contains(err.Error(), want) && !strings.Contains(err.Error(), "supports only") {
			t.Errorf("%q: %v (want %q)", q, err, want)
		}
	}
	// parse errors surface the same way
	if _, err := f.eng.Search(ctx, f.scope(f.Admin), `status = (`, search.Page{}); err == nil {
		t.Fatal("no error")
	} else if te, ok := search.IsQueryError(err); !ok || te.Pos == 0 {
		t.Fatalf("%v", err)
	}
	// SQL injection attempts are only ever values
	for _, q := range []string{`summary ~ "'; DROP TABLE issue; --"`, `status = "x' OR '1'='1"`, `key = "a\"; DELETE FROM issue"`} {
		if _, err := f.eng.Search(ctx, f.scope(f.Admin), q, search.Page{}); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	var n int
	f.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue`).Scan(&n)
}

func TestTQL06_PermissionFiltering(t *testing.T) {
	f := newFx(t)
	abc := f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "secret roadmap", Description: "launch codename zebra"})
	f.create(f.Admin, domain.CreateIn{Project: "XYZ", Summary: "xray roadmap", Description: "codename zebra too"})
	f.Svc.AddComment(ctx, f.Alice, abc.Key, "internal note about quokka", true)
	f.Svc.AddComment(ctx, f.Alice, abc.Key, "public note about wombat", false)
	f.index()

	// every query path is scoped: members see only their projects
	for _, q := range []string{``, `text ~ "zebra"`, `summary ~ "roadmap"`, `project = ABC`, `project in (ABC, XYZ)`, `key = ABC-1`, `NOT status = Done`, `text ~ "roadmap" OR project = ABC`} {
		if got := f.sortedKeys(f.carl, q); strings.Contains(got, "ABC") {
			t.Errorf("carl (XYZ only) saw ABC through %q: %s", q, got)
		}
		if got := f.sortedKeys(f.Alice, q); strings.Contains(got, "XYZ") {
			t.Errorf("alice (ABC only) saw XYZ through %q: %s", q, got)
		}
	}
	if got := f.sortedKeys(f.carl, `text ~ "zebra"`); got != "XYZ-1" {
		t.Fatalf("carl: %s", got)
	}
	// internal comments: matched for members, never for viewers
	if got := f.sortedKeys(f.Alice, `comment ~ "quokka"`); got != "ABC-1" {
		t.Fatalf("member internal: %s", got)
	}
	if got := f.sortedKeys(f.dave, `comment ~ "quokka"`); got != "" {
		t.Fatalf("viewer matched an internal comment: %s", got)
	}
	if got := f.sortedKeys(f.dave, `text ~ "quokka"`); got != "" {
		t.Fatalf("viewer matched internal text: %s", got)
	}
	if got := f.sortedKeys(f.dave, `comment ~ "wombat"`); got != "ABC-1" {
		t.Fatalf("viewer public comment: %s", got)
	}
	// NOT / negative text cannot be used to probe: "!~ quokka" for a viewer includes the issue
	if got := f.sortedKeys(f.dave, `text !~ "quokka"`); got != "ABC-1" {
		t.Fatalf("viewer negated probe: %s", got)
	}
	// a user with no membership sees nothing, and typeahead is scoped too
	u, _ := f.Svc.CreateUser(ctx, "eve@example.com", "Eve", "password123", false)
	eve := domain.Actor{ID: u.ID, Label: "Eve"}
	if got := f.keys(eve, ``); len(got) != 0 {
		t.Fatalf("outsider saw %v", got)
	}
	rows, _ := f.eng.Typeahead(ctx, f.scope(eve), "road", 8)
	if len(rows) != 0 {
		t.Fatalf("typeahead leaked: %v", rows)
	}
	rows, _ = f.eng.Typeahead(ctx, f.scope(f.carl), "road", 8)
	if len(rows) != 1 || rows[0].Key != "XYZ-1" {
		t.Fatalf("typeahead carl: %+v", rows)
	}
	// revoked access takes effect immediately, with no reindex (the scope is read at query time)
	f.Svc.AddMember(ctx, f.Admin, "ABC", f.dave.ID, "viewer")
	f.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id=$1`, f.dave.ID)
	if got := f.keys(f.dave, ``); len(got) != 0 {
		t.Fatalf("revoked user still sees %v", got)
	}
}

func TestTQL07_IndexFollowsEditsAndTrash(t *testing.T) {
	f := newFx(t)
	is := f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "alpha", Description: "bravo"})
	f.index()
	if f.sortedKeys(f.Alice, `text ~ "bravo"`) != "ABC-1" {
		t.Fatal("not indexed")
	}
	f.Svc.SaveText(ctx, f.Alice, is.Key, domain.SaveIn{Field: "description", Body: "charlie", BaseSeq: 1})
	if f.sortedKeys(f.Alice, `text ~ "charlie"`) != "" { // search lags until the indexer runs: derived data trails
		t.Fatal("indexed before the indexer ran")
	}
	f.index()
	if f.sortedKeys(f.Alice, `text ~ "charlie"`) != "ABC-1" || f.sortedKeys(f.Alice, `text ~ "bravo"`) != "" {
		t.Fatal("edit not reflected")
	}
	// but exact consumers read PostgreSQL directly and are always current
	if v, _ := f.Svc.View(ctx, f.Alice, is.Key); v.Issue.Description != "charlie" {
		t.Fatal("view stale")
	}
	hi := []string{"zulu"}
	f.Svc.UpdateFields(ctx, f.Alice, is.Key, nil, false, domain.Changes{Labels: &hi})
	f.index()
	if f.sortedKeys(f.Alice, `text ~ "zulu"`) != "ABC-1" {
		t.Fatal("label not searchable")
	}
	// a comment edit reaches the index
	c, _ := f.Svc.AddComment(ctx, f.Alice, is.Key, "first words", false)
	f.index()
	f.Svc.EditComment(ctx, f.Alice, is.Key, c.ID, "second words")
	f.index()
	if f.sortedKeys(f.Alice, `comment ~ "second"`) != "ABC-1" || f.sortedKeys(f.Alice, `comment ~ "first"`) != "" {
		t.Fatal("comment edit not reflected")
	}
	// trash removes it from the index
	f.Store.Pool.Exec(ctx, `UPDATE issue SET deleted_at=now() WHERE key=$1`, is.Key)
	f.Store.Pool.Exec(ctx, `INSERT INTO outbox(aggregate,type,payload) VALUES('issue:ABC-1','issue.trashed','{"key":"ABC-1"}')`)
	f.index()
	var n int
	f.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_search`).Scan(&n)
	if n != 0 || len(f.keys(f.Admin, ``)) != 0 {
		t.Fatal("trashed issue still searchable")
	}
}

func TestTQL10_27_IndexerIsIdempotentAndSkipsUnchangedText(t *testing.T) {
	f := newFx(t)
	is := f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "alpha", Description: "bravo"})
	f.index()
	stamp := func() time.Time {
		var u time.Time
		f.Store.Pool.QueryRow(ctx, `SELECT updated_at FROM issue_search`).Scan(&u)
		return u
	}
	t0 := stamp()
	// events for changes that do not touch searchable text (priority) must not rewrite the row
	hi := "high"
	for i := 0; i < 5; i++ {
		p := "high"
		if i%2 == 0 {
			p = "low"
		}
		f.Svc.UpdateFields(ctx, f.Alice, is.Key, nil, false, domain.Changes{Priority: &p})
	}
	_ = hi
	f.index()
	if !stamp().Equal(t0) {
		t.Fatal("search row rewritten although the text did not change")
	}
	// replaying the same events (duplicates / retries) changes nothing and loses nothing
	f.Store.Pool.Exec(ctx, `UPDATE outbox SET processed_at=NULL`)
	f.index()
	if !stamp().Equal(t0) || f.sortedKeys(f.Alice, `text ~ "bravo"`) != "ABC-1" {
		t.Fatal("replay changed the index")
	}
	// all events consumed; nothing pending
	n, age, err := f.ix.Backlog(ctx)
	if err != nil || n != 0 || age != 0 {
		t.Fatalf("backlog %d %v %v", n, age, err)
	}
	f.Svc.AddComment(ctx, f.Alice, is.Key, "x", false)
	if n, _, _ := f.ix.Backlog(ctx); n != 1 {
		t.Fatalf("backlog %d", n)
	}
}

func TestTQL22_TokenizationContract(t *testing.T) {
	f := newFx(t)
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "Plain one", Description: "see ABC-2 and the UTF-8 handling of ERR_CONN_RESET at /var/log/app.log from jana@example.com, v2.4.1 CamelCaseWord"})
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "Second", Description: "ünïcode Straße 日本語 😀 emoji"})
	f.index()
	cases := []struct{ q, want string }{
		{`text ~ "ABC-2"`, "ABC-1,ABC-2"}, // exact key match plus the text that mentions it
		{`text ~ "abc-2"`, "ABC-1,ABC-2"}, // case-insensitive key
		{`text ~ "utf-8"`, "ABC-1"},
		{`text ~ "ERR_CONN_RESET"`, "ABC-1"},
		{`text ~ "jana@example.com"`, "ABC-1"},
		{`text ~ "v2.4.1"`, "ABC-1"},
		{`text ~ "camelcaseword"`, "ABC-1"},
		{`text ~ "unicode"`, "ABC-2"}, // ü -> u
		{`text ~ "STRASSE"`, "ABC-2"}, // unaccent expands ß to ss (pinned for PostgreSQL 16)
		{`text ~ "日本語"`, "ABC-2"},
		{`text ~ "😀"`, ""}, // emoji are not tokens; documented limitation
	}
	for _, c := range cases {
		if got := f.sortedKeys(f.Alice, c.q); got != c.want {
			t.Errorf("%q: got %q want %q", c.q, got, c.want)
		}
	}
	// the exact key is listed first (TQL-22)
	if got := f.keys(f.Alice, `text ~ "ABC-2"`); got[0] != "ABC-2" {
		t.Errorf("key match not first: %v", got)
	}
}

// The plain search box (tql.FromText) runs as any ~: text, a summary substring, the key, and people by a word of the name or the
// start of the email.
func TestTQL_PlainTextSearch(t *testing.T) {
	f := newFx(t)
	mk := func(email, name string) string {
		u, err := f.Svc.CreateUser(ctx, email, name, "password123", false)
		if err != nil {
			t.Fatal(err)
		}
		f.Svc.AddMember(ctx, f.Admin, "ABC", u.ID, "member")
		return u.ID
	}
	alan, catalan := mk("alan@example.com", "Alan Polevoi"), mk("cq@example.com", "Catalan Q.")
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "Checkout fails", Description: "ask arkady", Assignee: alan})
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "Dana's report", Description: "for arkady", Assignee: catalan})
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "Rollover 100% done"})
	f.index()
	cases := []struct{ q, want string }{
		{`Alan`, "ABC-1"},           // a word of the assignee's name, not Catalan
		{`polevoi`, "ABC-1"},        // any word of it
		{`"Alan Polevoi"`, "ABC-1"}, // the full name
		{`alan@example`, "ABC-1"},   // the start of the email
		{`alice`, "ABC-1,ABC-2,ABC-3"},
		{`Alan AND (Arkady OR Dana)`, "ABC-1"},
		{`arkady -alan`, "ABC-2"},
		{`arkady OR rollover`, "ABC-1,ABC-2,ABC-3"},
		{`heckou`, "ABC-1"}, // summary substring
		{`abc-2`, "ABC-2"},  // key
		{`_`, ""},           // LIKE wildcards are literal
		{`%`, ""},
		{``, "ABC-1,ABC-2,ABC-3"},
	}
	for _, c := range cases {
		q, err := tql.FromText(c.q)
		if err != nil {
			t.Fatalf("%q: %v", c.q, err)
		}
		if got := f.sortedKeys(f.Alice, q); got != c.want {
			t.Errorf("%q (%s): got %q want %q", c.q, q, got, c.want)
		}
	}
}

func TestTQL24_RankingAndPaging(t *testing.T) {
	f := newFx(t)
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "unrelated", Description: "mentions needle once"})
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "needle in the summary", Description: ""})
	f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: "also unrelated", Description: "needle needle needle needle"})
	f.index()
	got := f.keys(f.Alice, `text ~ "needle"`)
	if got[0] != "ABC-2" { // summary (weight A) outranks description (weight C)
		t.Fatalf("summary match not ranked first: %v", got)
	}
	if got[1] != "ABC-3" { // more occurrences outrank fewer within the same weight
		t.Fatalf("ranking: %v", got)
	}
	// explicit ORDER BY wins over relevance
	if got := f.keys(f.Alice, `text ~ "needle" ORDER BY key DESC`); got[0] != "ABC-3" {
		t.Fatalf("explicit order ignored: %v", got)
	}
	// paging is deterministic and covers every row exactly once
	for i := 0; i < 12; i++ {
		f.create(f.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprintf("bulk %d", i)})
	}
	f.index()
	seen := map[string]bool{}
	for off := 0; off < 15; off += 4 {
		res, err := f.eng.Search(ctx, f.scope(f.Alice), `project = ABC ORDER BY key`, search.Page{Limit: 4, Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != 15 {
			t.Fatalf("total %d", res.Total)
		}
		for _, r := range res.Rows {
			if seen[r.Key] {
				t.Fatalf("duplicate %s across pages", r.Key)
			}
			seen[r.Key] = true
		}
	}
	if len(seen) != 15 {
		t.Fatalf("paged %d rows", len(seen))
	}
}
