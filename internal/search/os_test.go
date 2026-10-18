package search_test

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil/osmock"
)

type osRig struct {
	*fx
	m      *osmock.Server
	c      *search.OSClient
	ix2    *search.OSIndex
	osEng  *search.OpenSearch
	router *search.Router
}

func newOS(t *testing.T) *osRig {
	f := newFx(t)
	m := osmock.New()
	t.Cleanup(m.Close)
	r := &osRig{fx: f, m: m, c: &search.OSClient{BaseURL: m.URL, HTTP: egress.Policy{AllowPrivate: true}.Client()}}
	r.ix2 = &search.OSIndex{DB: f.Store.Pool, C: r.c, Now: f.Clock.Now, Sleep: func(time.Duration) {}}
	if err := r.ix2.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	r.osEng = &search.OpenSearch{C: r.c, PG: f.eng, Now: f.Clock.Now}
	r.router = &search.Router{PG: f.eng, OS: r.osEng, Index: r.ix2, Now: f.Clock.Now}
	return r
}

func (r *osRig) sync() {
	r.T.Helper()
	for {
		n, err := r.ix2.RunOnce(ctx)
		if err != nil {
			r.T.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
}

func (r *osRig) both() { r.index(); r.sync() } // both engines caught up

func keysOf(res *search.Result) []string {
	var k []string
	for _, x := range res.Rows {
		k = append(k, x.Key)
	}
	return k
}

func (r *osRig) via(eng search.Searcher, a domain.Actor, q string) *search.Result {
	r.T.Helper()
	res, err := eng.Search(ctx, r.scope(a), q, search.Page{Limit: 200})
	if err != nil {
		r.T.Fatalf("%q: %v", q, err)
	}
	return res
}

func sorted(s []string) []string { c := append([]string(nil), s...); sort.Strings(c); return c }

// dataset: a few issues with the features the translator has to get right
func (r *osRig) load() map[string]string {
	e := r.Env
	k := map[string]string{}
	mkIssue := func(name, project, typ, summary, desc string, labels []string, prio string) string {
		is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: project, Type: typ, Summary: summary, Description: desc, Labels: labels, Priority: prio})
		if err != nil {
			r.T.Fatal(err)
		}
		k[name] = is.Key
		return is.Key
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Summary: "warmup"}); err != nil {
		r.T.Fatal(err)
	}
	mkIssue("checkout", "ABC", "Bug", "Checkout fails on 3DS challenge", "Steps: use the test card\n\nExpected: order created", []string{"payments"}, "high")
	mkIssue("login", "ABC", "Task", "Login page slow", "profile the auth service", []string{"auth"}, "medium")
	mkIssue("export", "ABC", "Task", "Export CSV broken", "unicode in the header row", nil, "low")
	mkIssue("story", "ABC", "Story", "Story about onboarding", "", []string{"payments", "auth"}, "highest")
	e.Svc.AddComment(ctx, e.Alice, k["login"], "investigating the webhook retry", false)
	e.Svc.AddComment(ctx, e.Alice, k["login"], "secret-launch-plan is internal only", true)
	me := e.Alice.ID
	e.Svc.UpdateFields(ctx, e.Alice, k["checkout"], nil, false, domain.Changes{Assignee: &me})
	v, _ := e.Svc.View(ctx, e.Alice, k["export"])
	for _, tr := range v.Transitions {
		if tr.To == "Done" {
			e.Svc.Transition(ctx, e.Alice, k["export"], tr.ID, nil)
		}
	}
	xi, _ := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "XYZ", Type: "Bug", Summary: "Xray checkout bug", Description: "only carl may see this"})
	k["xray"] = xi.Key
	return k
}

// TQL-08: exact consumers and history queries use PostgreSQL; search consumers use the index when it is on.
func TestTQL08_Routing(t *testing.T) {
	r := newOS(t)
	r.load()
	r.both()
	searches := func() int { return r.m.Count("POST", "_search") }
	before := searches()
	if _, err := r.router.Keys(ctx, r.scope(r.Alice), `project = ABC`, 100); err != nil {
		t.Fatal(err)
	}
	if searches() != before {
		t.Fatal("an exact consumer (Keys) read the index")
	}
	res := r.via(r.router, r.Alice, `text ~ "checkout"`)
	if searches() != before+1 || res.Engine != "opensearch" {
		t.Fatalf("a search box did not use the index: %s (%d requests)", res.Engine, searches()-before)
	}
	before = searches()
	if res := r.via(r.router, r.Alice, `status was "To Do"`); searches() != before || res.Engine == "opensearch" {
		t.Fatal("a history query was sent to the index")
	}
	if res := r.via(r.router, r.Alice, `inputrequested is EMPTY`); searches() != before || res.Engine == "opensearch" { // a field the index does not carry
		t.Fatal("a query the index cannot answer was sent to it")
	}
	if _, err := r.router.Search(ctx, r.scope(r.Alice), `project = (`, search.Page{}); err == nil {
		t.Fatal("a bad query did not fail")
	} else if _, ok := search.IsQueryError(err); !ok {
		t.Fatalf("a bad query gave a non-query error: %v", err)
	}
	off := &search.Router{PG: r.eng}
	if res := r.via(off, r.Alice, `text ~ "checkout"`); res.Engine != "" {
		t.Fatalf("%s", res.Engine)
	}
	if rows, err := r.router.Typeahead(ctx, r.scope(r.Alice), "ABC-", 5); err != nil || len(rows) == 0 || r.m.Count("POST", "_search") == before {
		t.Fatalf("%v %d", err, len(rows))
	}
}

// The translator must give the PostgreSQL engine's answers: the same queries run on both and the key sets are compared.
func TestTQL_TranslatorParityWithPostgres(t *testing.T) {
	r := newOS(t)
	k := r.load()
	r.both()
	queries := []string{
		`project = ABC`, `project = XYZ`, `project in (ABC, XYZ)`, `project = ABC AND status = "To Do"`, `status != Done`, `status = Done`, `statusCategory = Done`, `statusCategory != Done`,
		`assignee = currentUser()`, `assignee != currentUser()`, `assignee is EMPTY`, `assignee is not EMPTY`, `assignee = "Alice K."`, `reporter = currentUser()`,
		`labels = payments`, `labels in (payments, auth)`, `labels != payments`, `labels is EMPTY`, `labels is not EMPTY`,
		`priority = high`, `priority in (highest, low)`, `type = Bug`, `type != Bug`, `type in (Bug, Story)`,
		`text ~ "checkout"`, `text ~ "webhook"`, `text ~ "test card"`, `text ~ "onboarding"`, `text !~ "checkout"`, `text ~ "` + k["checkout"] + `"`,
		`summary ~ "login"`, `summary ~ "heckou"`, `description ~ "unicode"`, `comment ~ "investigating"`, `comment ~ "retry"`,
		`key = ` + k["login"], `key in (` + k["login"] + `, ` + k["export"] + `)`, `key != ` + k["login"],
		`created >= -1d`, `created < -1d`, `updated >= startOfDay()`, `resolved is EMPTY`, `resolved is not EMPTY`,
		`parent is EMPTY`,
		`NOT labels = auth`, `labels = payments OR labels = auth`, `(labels = payments AND priority = high) OR type = Task`,
		`project = ABC ORDER BY priority DESC, key`, `project = ABC ORDER BY key DESC`, `project = ABC ORDER BY created DESC, key`,
	}
	for _, who := range []struct {
		name string
		a    domain.Actor
	}{{"admin", r.Admin}, {"alice", r.Alice}, {"dave (viewer)", r.dave}, {"carl (xyz only)", r.carl}} {
		for _, q := range queries {
			pg, os := r.via(r.eng, who.a, q), r.via(r.osEng, who.a, q)
			a, b := keysOf(pg), keysOf(os)
			if !strings.Contains(q, "ORDER BY") {
				a, b = sorted(a), sorted(b)
			}
			if !reflect.DeepEqual(a, b) {
				t.Errorf("%s: %q\n  postgres:   %v\n  opensearch: %v", who.name, q, a, b)
			}
		}
	}
}

func TestTQL09_10_IndexerVersionsRetriesAndDeadLetters(t *testing.T) {
	r := newOS(t)
	k := r.load()
	// before the indexer runs, the backlog and its lag are visible
	r.Clock.Advance(30 * time.Second)
	b, _ := r.ix2.Backlog(ctx)
	if b.Events == 0 || b.Lag < 29*time.Second { // event times come from the database clock, the lag from the service clock
		t.Fatalf("%+v", b)
	}
	r.sync()
	if b, _ = r.ix2.Backlog(ctx); b.Events != 0 || b.Lag != 0 {
		t.Fatalf("%+v", b)
	}
	docs := r.m.Docs("tasklane-issues")
	if len(docs) != 5+1 { // the five issues, and XYZ's warm-up issue
		t.Fatalf("%d documents", len(docs))
	}
	// replaying everything changes nothing: the cursor passed it, and the index would refuse older versions anyway
	r.Env.Store.Pool.Exec(ctx, `UPDATE outbox_cursor SET last_id=0 WHERE consumer='search_os'`)
	r.sync()
	if after := r.m.Docs("tasklane-issues"); !reflect.DeepEqual(after, docs) {
		t.Fatal("a replay changed the index")
	}
	// an older write after a newer one is refused by the index (external versioning), the document keeps the newer state
	id := ""
	for did, d := range docs {
		if d["key"] == k["login"] {
			id = did
		}
	}
	r.Svc.UpdateFields(ctx, r.Alice, k["login"], nil, false, domain.Changes{Labels: &[]string{"fresh"}})
	r.sync()
	res, err := r.c.Bulk(ctx, []search.BulkOp{{Index: "tasklane-issues", ID: id, Version: 1, Doc: docs[id]}})
	if err != nil || res.Conflicts != 1 || res.Written != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if labels := r.m.Docs("tasklane-issues")[id]["labels"]; fmt.Sprint(labels) != "[fresh]" {
		t.Fatalf("an old write regressed the document: %v", labels)
	}
	// an outage in the middle of applying events: nothing advances, the backlog grows, and it drains afterwards
	r.Svc.UpdateFields(ctx, r.Alice, k["login"], nil, false, domain.Changes{Labels: &[]string{"during-outage"}})
	r.m.Fail(1000, 503, func(rq *http.Request) bool { return strings.Contains(rq.URL.Path, "_bulk") })
	if _, err := r.ix2.RunOnce(ctx); err == nil {
		t.Fatal("an outage looked like success")
	}
	r.Svc.UpdateFields(ctx, r.Alice, k["export"], nil, false, domain.Changes{Labels: &[]string{"during-outage-2"}})
	if b, _ = r.ix2.Backlog(ctx); b.Events < 2 {
		t.Fatalf("the backlog did not grow: %+v", b)
	}
	r.m.ClearFaults()
	r.sync()
	if b, _ = r.ix2.Backlog(ctx); b.Events != 0 {
		t.Fatalf("%+v", b)
	}
	if labels := r.m.Docs("tasklane-issues")[id]["labels"]; fmt.Sprint(labels) != "[during-outage]" {
		t.Fatalf("%v", labels)
	}
	// a document the index rejects is retried, then parked as a dead letter, and the rest still flows
	poison := ""
	for did, d := range r.m.Docs("tasklane-issues") {
		if d["key"] == k["export"] {
			poison = did
		}
	}
	r.m.Reject = func(i string) bool { return i == poison }
	r.Svc.UpdateFields(ctx, r.Alice, k["export"], nil, false, domain.Changes{Priority: ptr("highest")})
	r.Svc.UpdateFields(ctx, r.Alice, k["login"], nil, false, domain.Changes{Priority: ptr("lowest")})
	for i := 0; i < 2; i++ {
		if _, err := r.ix2.RunOnce(ctx); err == nil {
			t.Fatal("a rejected write passed silently")
		}
	}
	if _, err := r.ix2.RunOnce(ctx); err != nil {
		t.Fatalf("after three failures the batch should be parked: %v", err)
	}
	if n, _ := r.ix2.DeadLetters(ctx); n == 0 {
		t.Fatal("no dead letter was recorded")
	}
	if b, _ = r.ix2.Backlog(ctx); b.Events != 0 {
		t.Fatalf("a poison document blocked the queue: %+v", b)
	}
	if p := r.m.Docs("tasklane-issues")[id]["priority"]; p != "lowest" {
		t.Fatalf("a healthy document in the same batch was not written: %v", p)
	}
}

func ptr(s string) *string { return &s }

// TQL-13: nothing the caller cannot see comes back, even while the index is behind.
func TestTQL13_PermissionsHoldWhileTheIndexLags(t *testing.T) {
	r := newOS(t)
	k := r.load()
	r.both()
	for _, c := range []struct {
		who  string
		a    domain.Actor
		q    string
		want int
	}{
		{"carl, XYZ only, searches ABC text", r.carl, `text ~ "checkout"`, 1}, // only the XYZ issue
		{"carl searches an ABC key", r.carl, `key = ` + k["checkout"], 0},
		{"dave, a viewer, searches internal comment text", r.dave, `text ~ "secret-launch-plan"`, 0},
		{"dave searches comments", r.dave, `comment ~ "secret-launch-plan"`, 0},
		{"alice, a member, finds the internal comment", r.Alice, `text ~ "secret-launch-plan"`, 1},
		{"alice finds it in comments", r.Alice, `comment ~ "secret-launch-plan"`, 1},
	} {
		res := r.via(r.router, c.a, c.q)
		if res.Engine != "opensearch" || len(res.Rows) != c.want {
			t.Errorf("%s: %s %v", c.who, res.Engine, keysOf(res))
		}
	}
	// access revoked during lag: Bob leaves ABC, the index is not told, and his search shows nothing from ABC
	r.Svc.AddMember(ctx, r.Admin, "XYZ", r.Bob.ID, "member")
	if got := keysOf(r.via(r.router, r.Bob, `project = ABC`)); len(got) == 0 {
		t.Fatal("setup: bob should see ABC")
	}
	r.Env.Store.Pool.Exec(ctx, `DELETE FROM project_member WHERE user_id=$1 AND project_id=(SELECT id FROM project WHERE key='ABC')`, r.Bob.ID)
	if res := r.via(r.router, r.Bob, `project = ABC`); len(res.Rows) != 0 || res.Total != 0 {
		t.Fatalf("a revoked member still finds issues: %v total %d", keysOf(res), res.Total)
	}
	if res := r.via(r.router, r.Bob, `text ~ "checkout"`); len(res.Rows) != 1 || res.Rows[0].Key != k["xray"] {
		t.Fatalf("%v", keysOf(res))
	}
	// an issue moved to a project the caller cannot see, and one trashed, during lag (the index still has the old state)
	moved := lifeMove(t, r, k["checkout"], "XYZ")
	if res := r.via(r.router, r.Alice, `text ~ "3DS"`); len(res.Rows) != 0 {
		t.Fatalf("an issue moved to a restricted project appeared: %v (moved to %s)", keysOf(res), moved)
	}
	lifeDelete(t, r, k["login"])
	if res := r.via(r.router, r.Alice, `text ~ "login"`); len(res.Rows) != 0 {
		t.Fatalf("a trashed issue appeared: %v", keysOf(res))
	}
	// re-securing is the same mechanism: PostgreSQL's scope decides on the way out. Typeahead obeys it as well
	if rows, _ := r.router.Typeahead(ctx, r.scope(r.carl), "ABC", 10); len(rows) != 0 {
		t.Fatalf("typeahead leaked: %+v", rows)
	}
	if rows, _ := r.router.Typeahead(ctx, r.scope(r.Alice), k["login"], 10); len(rows) != 0 {
		t.Fatalf("typeahead returned a trashed issue: %+v", rows)
	}
}

func lifeMove(t *testing.T, r *osRig, key, project string) string {
	p, err := r.Svc.Lifecycle(ctx, r.Admin, key, domain.LifecycleIn{Op: "move", Project: project})
	if err != nil {
		t.Fatal(err)
	}
	return p.Key2
}

func lifeDelete(t *testing.T, r *osRig, key string) {
	if _, err := r.Svc.Lifecycle(ctx, r.Admin, key, domain.LifecycleIn{Op: "delete"}); err != nil {
		t.Fatal(err)
	}
}

func (r *osRig) docsMatchPostgres() {
	r.T.Helper()
	want := map[string]bool{}
	rows, _ := r.Store.Pool.Query(ctx, `SELECT id::text FROM issue WHERE deleted_at IS NULL`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		want[id] = true
	}
	rows.Close()
	have := r.m.Docs("tasklane-issues")
	if len(have) != len(want) {
		r.T.Fatalf("index has %d documents, PostgreSQL %d issues", len(have), len(want))
	}
	for id := range want {
		if have[id] == nil {
			r.T.Fatalf("issue %s is not in the index", id)
		}
	}
}

// TQL-11: an online rebuild with live writes (dual write), verification, atomic alias swap, resume and rollback.
func TestTQL11_OnlineReindex(t *testing.T) {
	r := newOS(t)
	k := r.load()
	for i := 0; i < 25; i++ {
		r.Svc.CreateIssue(ctx, r.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprintf("filler issue %d", i)})
	}
	r.sync()
	oldIndex := r.m.AliasTarget("tasklane-issues")
	// the first run is "killed" after one page of 10 documents; writes continue meanwhile
	small := *r.ix2
	rep, err := (&small).Rebuild(ctx, 1)
	if !search.IsPaused(err) {
		t.Fatalf("%v %+v", err, rep)
	}
	if r.m.AliasTarget("tasklane-issues") != oldIndex {
		t.Fatal("the alias moved before the new index was verified")
	}
	r.Svc.UpdateFields(ctx, r.Alice, k["login"], nil, false, domain.Changes{Labels: &[]string{"during-rebuild"}})
	newIs, _ := r.Svc.CreateIssue(ctx, r.Alice, domain.CreateIn{Project: "ABC", Summary: "created while rebuilding"})
	r.sync() // dual write: both the live and the building index get these
	building := ""
	for _, n := range r.m.Indexes() {
		if n != oldIndex {
			building = n
		}
	}
	if building == "" {
		t.Fatal("no building index")
	}
	if d := r.m.Docs(building); len(d) == 0 {
		t.Fatal("the building index received nothing")
	}
	// a fresh process resumes where the first stopped and finishes
	fresh := &search.OSIndex{DB: r.Store.Pool, C: r.c, Now: r.Clock.Now, Sleep: func(time.Duration) {}, Batch: 10}
	rep, err = fresh.Rebuild(ctx, 0)
	if err != nil || !rep.Resumed || !rep.Verified || !rep.Swapped || rep.Index != building {
		t.Fatalf("%v %+v", err, rep)
	}
	if got := r.m.AliasTarget("tasklane-issues"); got != building {
		t.Fatalf("the alias points at %s, want %s", got, building)
	}
	r.docsMatchPostgres()
	if d := r.m.Docs("tasklane-issues"); fmt.Sprint(d[idOf(t, r, k["login"])]["labels"]) != "[during-rebuild]" || d[idOf(t, r, newIs.Key)] == nil {
		t.Fatal("writes made during the rebuild are missing from the new index")
	}
	// searching follows the alias without a restart; the old index is the rollback window
	if res, err := r.osEng.Search(ctx, r.scope(r.Alice), `text ~ "rebuilding"`, search.Page{}); err != nil {
		t.Fatalf("the index query failed: %v", err)
	} else {
		sr, _ := r.c.Search(ctx, "tasklane-issues", map[string]any{"query": map[string]any{"match_all": map[string]any{}}, "size": 3, "_source": true})
		t.Logf("os total %d rows %d; index total %d; sample %v; docs %d", res.Total, len(res.Rows), sr.Total, sr.Hits, len(r.m.Docs("tasklane-issues")))
	}
	if res := r.via(r.router, r.Alice, `text ~ "rebuilding"`); len(res.Rows) != 1 || res.Engine != "opensearch" {
		t.Fatalf("%s %v", res.Engine, keysOf(res))
	}
	if err := r.ix2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if r.m.AliasTarget("tasklane-issues") != oldIndex {
		t.Fatal("rollback did not restore the old index")
	}
	if err := r.ix2.Rollback(ctx); err != nil {
		t.Fatal(err) // rolling forward again is the same swap
	}
	if err := r.ix2.DropPrevious(ctx); err != nil {
		t.Fatal(err)
	}
	// a rebuild that finds the new index wrong refuses to swap
	r.ix2.Ensure(ctx)
	r.m.Reject = nil
	// a project-scoped reindex repairs one project without touching the others
	other, _ := r.Svc.CreateIssue(ctx, r.Admin, domain.CreateIn{Project: "XYZ", Summary: "other project issue"})
	r.sync()
	r.m.Put("tasklane-issues", idOf(t, r, k["export"]), map[string]any{"id": "x", "key": "STALE"})
	if _, err := r.ix2.ReindexProject(ctx, "ABC"); err != nil {
		t.Fatal(err)
	}
	if d := r.m.Docs("tasklane-issues")[idOf(t, r, k["export"])]; d["key"] != k["export"] {
		t.Fatalf("%v", d["key"])
	}
	if d := r.m.Docs("tasklane-issues")[idOf(t, r, other.Key)]; d == nil {
		t.Fatal("a project reindex removed another project's document")
	}
}

func idOf(t *testing.T, r *osRig, key string) string {
	var id string
	if err := r.Store.Pool.QueryRow(ctx, `SELECT id::text FROM issue WHERE key=$1`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestTQL11_RebuildThatDoesNotVerifyDoesNotSwap(t *testing.T) {
	r := newOS(t)
	r.load()
	r.sync()
	live := r.m.AliasTarget("tasklane-issues")
	// the cluster silently drops a document while building (a bulk that "succeeds" but loses data)
	small := &search.OSIndex{DB: r.Store.Pool, C: r.c, Now: r.Clock.Now, Sleep: func(time.Duration) {}, Batch: 3}
	if _, err := small.Rebuild(ctx, 1); !search.IsPaused(err) {
		t.Fatal(err)
	}
	for _, n := range r.m.Indexes() {
		if n != live {
			for id := range r.m.Docs(n) {
				r.m.Remove(n, id)
				break
			}
		}
	}
	if _, err := small.Rebuild(ctx, 0); err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("a damaged index passed verification: %v", err)
	}
	if r.m.AliasTarget("tasklane-issues") != live {
		t.Fatal("the alias moved to an index that failed verification")
	}
}

// TQL-12: drift is found and repaired: missing, stale and orphaned documents, including a purged issue.
func TestTQL12_ReconcileRepairsDrift(t *testing.T) {
	r := newOS(t)
	k := r.load()
	r.sync()
	if rep, err := r.ix2.Reconcile(ctx, ""); err != nil || !rep.Clean() || rep.Checked != 6 {
		t.Fatalf("%v %+v", err, rep)
	}
	missing, stale := idOf(t, r, k["login"]), idOf(t, r, k["export"])
	r.m.Remove("tasklane-issues", missing)
	src := r.m.Docs("tasklane-issues")[stale]
	src["summary"] = "tampered summary"
	src["h"] = "0000"
	r.m.Put("tasklane-issues", stale, src)
	r.m.Put("tasklane-issues", "ffffffff-0000-0000-0000-000000000000", map[string]any{"id": "ffffffff-0000-0000-0000-000000000000", "key": "ABC-777", "project": "ABC"}) // orphan: a purged issue
	rep, err := r.ix2.Reconcile(ctx, "ABC")
	if err != nil || len(rep.Missing) != 1 || rep.Missing[0] != k["login"] || len(rep.Stale) != 1 || rep.Stale[0] != k["export"] || len(rep.Orphans) != 1 || rep.Repaired < 3 {
		t.Fatalf("%v %+v", err, rep)
	}
	if rep, _ = r.ix2.Reconcile(ctx, ""); !rep.Clean() {
		t.Fatalf("drift remained: %+v", rep)
	}
	r.docsMatchPostgres()
	var saved string
	r.Store.Pool.QueryRow(ctx, `SELECT last_reconcile::text FROM search_os_state`).Scan(&saved)
	if saved == "" { // visible in Admin
		t.Fatal("the report was not kept")
	}
	// a purge removes the document through the outbox, by key, because no row remains to look up
	life, _ := r.Svc.Lifecycle(ctx, r.Admin, k["story"], domain.LifecycleIn{Op: "delete"})
	_ = life
	r.sync()
	if d := r.m.Docs("tasklane-issues")[idOf(t, r, k["story"])]; d != nil {
		t.Fatal("a trashed issue is still indexed")
	}
}

// TQL-16: the index being down never blocks writes or searches: PostgreSQL answers, with a warning, and the index catches up.
func TestTQL16_OutageFallsBackAndRecovers(t *testing.T) {
	r := newOS(t)
	k := r.load()
	r.both()
	r.m.Fail(100000, 503, nil)
	// writes are unaffected
	is, err := r.Svc.CreateIssue(ctx, r.Alice, domain.CreateIn{Project: "ABC", Summary: "written during the outage"})
	if err != nil {
		t.Fatalf("a write failed during an index outage: %v", err)
	}
	r.index() // the PostgreSQL indexer is independent of the cluster
	res := r.via(r.router, r.Alice, `text ~ "outage"`)
	if res.Engine == "opensearch" || len(res.Rows) != 1 || !strings.Contains(res.Warning, "search is limited") {
		t.Fatalf("%s %v %q", res.Engine, keysOf(res), res.Warning)
	}
	before := r.m.Count("POST", "_search")
	r.via(r.router, r.Alice, `text ~ "checkout"`) // within the cool-down the cluster is not hammered
	if r.m.Count("POST", "_search") != before {
		t.Fatal("a failing index was queried again inside its cool-down")
	}
	if rows, err := r.router.Typeahead(ctx, r.scope(r.Alice), "ABC", 5); err != nil || len(rows) == 0 {
		t.Fatalf("typeahead did not fall back: %v", err)
	}
	if _, err := r.ix2.RunOnce(ctx); err == nil {
		t.Fatal("the indexer did not notice the outage")
	}
	if b, _ := r.ix2.Backlog(ctx); b.Events == 0 {
		t.Fatal("the backlog did not build up")
	}
	// recovery: the cluster comes back, the cool-down passes, the backlog drains, searches use the index again
	r.m.ClearFaults()
	r.Clock.Advance(time.Minute)
	r.sync()
	res = r.via(r.router, r.Alice, `text ~ "outage"`)
	if res.Engine != "opensearch" || len(res.Rows) != 1 || res.Rows[0].Key != is.Key {
		t.Fatalf("%s %v %q", res.Engine, keysOf(res), res.Warning)
	}
	_ = k
}

// The lag indicator and the own-edit overlay (SPEC 10.1): a change you just made neither vanishes from nor lingers in a list.
func TestTQL09_LagWarningAndOwnEditOverlay(t *testing.T) {
	r := newOS(t)
	k := r.load()
	r.both()
	r.router.LagWarn = 5 * time.Second
	// not behind: no warning
	if res := r.via(r.router, r.Alice, `project = ABC`); res.Warning != "" {
		t.Fatalf("%q", res.Warning)
	}
	// Alice finishes an issue and creates one; the index has not heard yet
	v, _ := r.Svc.View(ctx, r.Alice, k["login"])
	for _, tr := range v.Transitions {
		if tr.To == "Done" {
			r.Svc.Transition(ctx, r.Alice, k["login"], tr.ID, nil)
		}
	}
	fresh, _ := r.Svc.CreateIssue(ctx, r.Alice, domain.CreateIn{Project: "ABC", Summary: "brand new thing"})
	r.Clock.Advance(20 * time.Second)
	res := r.via(r.router, r.Alice, `project = ABC AND status != Done`)
	got := map[string]bool{}
	for _, x := range res.Rows {
		got[x.Key] = true
	}
	if got[k["login"]] {
		t.Fatalf("the issue Alice just finished still shows in 'not done': %v", keysOf(res))
	}
	if !got[fresh.Key] {
		t.Fatalf("the issue Alice just created is missing from a list that matches it: %v", keysOf(res))
	}
	if !strings.Contains(res.Warning, "a few seconds behind") || res.LagSeconds < 15 {
		t.Fatalf("%q %v", res.Warning, res.LagSeconds)
	}
	// other people get the index's honest (older) answer, with the same warning
	bob := r.via(r.router, r.Bob, `project = ABC AND status != Done`)
	if bob.Engine != "opensearch" || !strings.Contains(bob.Warning, "behind") {
		t.Fatalf("%s %q", bob.Engine, bob.Warning)
	}
	r.sync()
	if res = r.via(r.router, r.Alice, `project = ABC AND status != Done`); res.Warning != "" || len(res.Rows) != len(keysOfRows(r.via(r.eng, r.Alice, `project = ABC AND status != Done`).Rows)) {
		t.Fatalf("%q", res.Warning)
	}
}

func keysOfRows(rows []search.Row) []string {
	var k []string
	for _, x := range rows {
		k = append(k, x.Key)
	}
	return k
}

// TQL-15: paging is stable and disjoint; very deep pages are PostgreSQL's job; the totals are marked when capped.
func TestTQL15_PagingAndTotals(t *testing.T) {
	r := newOS(t)
	r.load()
	for i := 0; i < 30; i++ {
		r.Svc.CreateIssue(ctx, r.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprintf("paged issue %02d", i)})
	}
	r.both()
	var all []string
	for off := 0; off < 40; off += 10 {
		res, err := r.router.Search(ctx, r.scope(r.Alice), `project = ABC ORDER BY key`, search.Page{Limit: 10, Offset: off})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != 34 || res.TotalCapped || res.Engine != "opensearch" {
			t.Fatalf("%d %v %s", res.Total, res.TotalCapped, res.Engine)
		}
		all = append(all, keysOf(res)...)
	}
	pg, _ := r.eng.Search(ctx, r.scope(r.Alice), `project = ABC ORDER BY key`, search.Page{Limit: 200})
	if !reflect.DeepEqual(all, keysOf(pg)) {
		t.Fatalf("paging through the index differs from the primary database\n%v\n%v", all, keysOf(pg))
	}
	if res, err := r.router.Search(ctx, r.scope(r.Alice), `project = ABC`, search.Page{Limit: 50, Offset: 9990}); err != nil || res.Engine == "opensearch" {
		t.Fatalf("a page beyond the index window: %v %v", err, res)
	}
}

// A caller who can read more projects than the filter allows is answered by PostgreSQL, with a warning, and sees no more.
func TestTQL_TermLimitFallsBackWithoutLeaking(t *testing.T) {
	r := newOS(t)
	r.load()
	r.both()
	r.osEng.TermLimit = 1
	r.Svc.AddMember(ctx, r.Admin, "XYZ", r.Alice.ID, "member")
	res := r.via(r.router, r.Alice, `text ~ "checkout"`)
	if res.Engine == "opensearch" || !strings.Contains(res.Warning, "very many projects") || len(res.Rows) != 2 {
		t.Fatalf("%s %q %v", res.Engine, res.Warning, keysOf(res))
	}
	if res := r.via(r.router, r.carl, `text ~ "checkout"`); res.Engine != "opensearch" || len(res.Rows) != 1 { // one project: within the limit
		t.Fatalf("%s %v", res.Engine, keysOf(res))
	}
}

// A client built without an explicit policy refuses private and loopback addresses: the safe default.
func TestOpenSearchClientDefaultsToTheEgressPolicy(t *testing.T) {
	m := osmock.New()
	defer m.Close()
	if _, err := (&search.OSClient{BaseURL: m.URL}).Health(ctx); err == nil || !strings.Contains(err.Error(), "private or loopback") {
		t.Fatalf("a loopback cluster was reachable by default: %v", err)
	}
	if h, err := (&search.OSClient{BaseURL: m.URL, HTTP: egress.Policy{AllowPrivate: true}.Client()}).Health(ctx); err != nil || h != "green" {
		t.Fatalf("%v %s", err, h)
	}
}
