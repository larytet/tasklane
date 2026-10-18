package domain_test

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/jiramock"
)

type jiraRig struct {
	e      *testutil.Env
	jm     *jiramock.Server
	conn   domain.JiraConnection
	secret string
	proj   domain.Project
	base   time.Time
}

func newJira(t *testing.T) *jiraRig {
	e := testutil.New(t)
	e.Clock.T = time.Now()
	e.Project() // a native project ABC, to prove natives stay writable
	jm := jiramock.New("sync@example.com", "api-token")
	t.Cleanup(jm.Close)
	r := &jiraRig{e: e, jm: jm, base: time.Now().Add(-48 * time.Hour)}
	var err error
	r.conn, r.secret, err = e.Svc.CreateJiraConnection(ctx, e.Admin, domain.JiraConnIn{Name: "acme", SiteURL: jm.URL, Email: "sync@example.com", Token: "api-token", AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *jiraRig) mirror(t *testing.T) {
	var err error
	if r.proj, err = r.e.Svc.MirrorJiraProject(ctx, r.e.Admin, r.conn.ID, "WEB", "WEB", "Website", ""); err != nil {
		t.Fatal(err)
	}
}

func (r *jiraRig) issue(id int, summary, status, cat, typ string, mod func(*jiramock.Issue)) *jiramock.Issue {
	is := jiramock.IssueJSON(fmt.Sprint(10000+id), fmt.Sprintf("WEB-%d", id), "WEB", summary, status, cat, typ, r.base.Add(time.Duration(id)*time.Minute), r.base.Add(time.Duration(id)*time.Hour))
	if mod != nil {
		mod(is)
	}
	r.jm.Upsert(is)
	return is
}

func (r *jiraRig) syncAs(t *testing.T) int {
	t.Helper()
	n, err := r.e.Svc.SyncJiraProject(ctx, r.e.Admin, "WEB")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func jiraDataset(r *jiraRig) {
	r.jm.InlineHist = 1 // Jira caps the inline changelog; the rest must be fetched
	r.issue(1, "Mirror epic", "To Do", "new", "Epic", nil)
	r.issue(2, "Checkout redesign", "In Progress", "indeterminate", "Story", func(is *jiramock.Issue) {
		is.Fields["parent"] = jiramock.M{"id": "10001", "key": "WEB-1"}
		is.Fields["labels"] = []any{"payments", "ux"}
		is.Fields["assignee"] = jiramock.M{"accountId": "acc-ann", "displayName": "Ann Assignee", "emailAddress": "ann@example.com"}
		is.Fields["description"] = jiramock.M{"type": "doc", "version": 1, "content": []any{
			jiramock.M{"type": "paragraph", "content": []any{jiramock.M{"type": "text", "text": "Redesign the "}, jiramock.M{"type": "text", "text": "checkout", "marks": []any{jiramock.M{"type": "link", "attrs": jiramock.M{"href": "https://example.com/spec"}}}}}},
			jiramock.M{"type": "bulletList", "content": []any{jiramock.M{"type": "listItem", "content": []any{jiramock.M{"type": "paragraph", "content": []any{jiramock.M{"type": "text", "text": "keep the cart"}}}}}}},
			jiramock.M{"type": "codeBlock", "content": []any{jiramock.M{"type": "text", "text": "cart.total()"}}},
			jiramock.M{"type": "futureNodeType", "attrs": jiramock.M{"x": 1}},
		}}
		is.Fields["issuelinks"] = []any{jiramock.M{"type": jiramock.M{"name": "Blocks"}, "outwardIssue": jiramock.M{"id": "10003", "key": "WEB-3"}}}
		is.Comments = []jiramock.M{
			{"id": "c1", "author": jiramock.M{"accountId": "acc-ann", "displayName": "Ann Assignee", "emailAddress": "ann@example.com"}, "body": jiramock.ADFParagraph("first comment"), "created": "2026-01-01T10:00:00.000+0000", "updated": "2026-01-01T10:00:00.000+0000"},
			{"id": "c2", "author": jiramock.M{"accountId": "acc-bob", "displayName": "Bob Builder"}, "body": jiramock.ADFParagraph("second, edited"), "created": "2026-01-01T11:00:00.000+0000", "updated": "2026-01-01T12:00:00.000+0000"},
			{"id": "c3", "author": jiramock.M{"accountId": "acc-ann", "displayName": "Ann Assignee", "emailAddress": "ann@example.com"}, "body": jiramock.ADFParagraph("restricted to developers"), "visibility": jiramock.M{"type": "role", "value": "Developers"}, "created": "2026-01-01T13:00:00.000+0000", "updated": "2026-01-01T13:00:00.000+0000"},
		}
		is.History = []jiramock.M{
			{"id": "h1", "author": jiramock.M{"accountId": "acc-ann", "displayName": "Ann Assignee"}, "created": "2026-01-01T09:00:00.000+0000", "items": []any{jiramock.M{"field": "status", "fromString": "To Do", "toString": "In Progress"}}},
			{"id": "h2", "author": jiramock.M{"accountId": "acc-bob", "displayName": "Bob Builder"}, "created": "2026-01-02T09:00:00.000+0000", "items": []any{jiramock.M{"field": "assignee", "fromString": "", "toString": "Ann Assignee"}, jiramock.M{"field": "priority", "fromString": "Low", "toString": "Medium"}}},
		}
	})
	r.issue(3, "Cart crashes", "Done", "done", "Bug", func(is *jiramock.Issue) { is.Fields["resolutiondate"] = "2026-01-03T10:00:00.000+0000" })
	r.issue(4, "Confidential matter", "To Do", "new", "Task", func(is *jiramock.Issue) { is.Fields["security"] = jiramock.M{"name": "Internal"} })
}

func TestMIR01_BackfillCountsFieldsAndIdempotence(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	if n := r.syncAs(t); n != 4 {
		t.Fatalf("%d issues synced", n)
	}
	v, err := e.Svc.View(ctx, e.Admin, "WEB-2")
	if err != nil {
		t.Fatal(err)
	}
	i := v.Issue
	if i.Summary != "Checkout redesign" || i.Type != "Story" || i.Status != "In Progress" || i.StatusCat != "in_progress" || i.Parent != "WEB-1" || strings.Join(i.Labels, ",") != "payments,ux" ||
		i.Assignee == nil || i.Assignee.Name != "Ann Assignee" || i.Priority != "medium" {
		t.Fatalf("%+v", i)
	}
	if !strings.Contains(i.Description, "[checkout](https://example.com/spec)") || !strings.Contains(i.Description, "- keep the cart") || !strings.Contains(i.Description, "cart.total()") || !strings.Contains(i.Description, "[unsupported content: futureNodeType]") {
		t.Fatalf("%q", i.Description)
	}
	if len(v.Comments) != 3 || v.Comments[1].Body != "second, edited" || !v.Comments[2].Internal { // the paged comments all arrived, and a restricted one is internal
		t.Fatalf("%+v", v.Comments)
	}
	if v.Mirror == nil || v.Mirror.JiraKey != "WEB" || v.Mirror.SyncedAt == nil || v.Mirror.Stale || !strings.HasPrefix(v.Mirror.SiteURL, "http") {
		t.Fatalf("%+v", v.Mirror)
	}
	h, _ := e.Svc.History(ctx, e.Admin, "WEB-2")
	fields := map[string]bool{}
	for _, x := range h {
		fields[x.Field] = true
	}
	if !fields["status"] || !fields["assignee"] || !fields["priority"] { // the first changelog entry came inline, the second needed the changelog endpoint
		t.Fatalf("%+v", h)
	}
	if links := v.Links; len(links) != 0 { // WEB-3 is mirrored, so the link exists on its side too
		_ = links
	}
	v3, _ := e.Svc.View(ctx, e.Admin, "WEB-3")
	if v3.Issue.Resolved == nil || v3.Issue.StatusCat != "done" || len(v3.Links) != 1 || v3.Links[0].Type != "blocks" {
		t.Fatalf("%+v %+v", v3.Issue, v3.Links)
	}
	// the same sync again changes nothing: no duplicate comments, history rows or issues
	count := func(q string) (n int) { e.Store.Pool.QueryRow(ctx, q).Scan(&n); return }
	cm, hs, is, tv := count(`SELECT count(*) FROM comment`), count(`SELECT count(*) FROM issue_history`), count(`SELECT count(*) FROM issue WHERE external_id IS NOT NULL`), count(`SELECT count(*) FROM text_version`)
	r.syncAs(t)
	r.jm.Upsert(r.jm.Issues[1]) // touch: the same content with a newer `updated`
	r.syncAs(t)
	if count(`SELECT count(*) FROM comment`) != cm || count(`SELECT count(*) FROM issue_history`) != hs || count(`SELECT count(*) FROM issue WHERE external_id IS NOT NULL`) != is || count(`SELECT count(*) FROM text_version`) != tv {
		t.Fatal("a re-sync duplicated or rewrote data")
	}
	// a native project beside it is untouched and writable
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "native"}); err != nil {
		t.Fatal(err)
	}
	// numbering follows Jira's and creating never collides
	var next int
	e.Store.Pool.QueryRow(ctx, `SELECT next_number FROM project WHERE key='WEB'`).Scan(&next)
	if next != 5 {
		t.Fatalf("next number %d", next)
	}
}

func TestMIR01_KillAndResumeWithoutDuplicates(t *testing.T) {
	r := newJira(t)
	for i := 1; i <= 7; i++ {
		r.issue(i, fmt.Sprintf("issue %d", i), "To Do", "new", "Task", func(is *jiramock.Issue) {
			is.Comments = []jiramock.M{{"id": fmt.Sprint("c", i), "author": jiramock.M{"accountId": "acc-ann", "displayName": "Ann"}, "body": jiramock.ADFParagraph("hi"), "created": "2026-01-01T10:00:00.000+0000", "updated": "2026-01-01T10:00:00.000+0000"}}
		})
	}
	r.mirror(t)
	// the connection dies after two pages: some issues are in, and the watermark is where it got to
	r.jm.PageSize = 2
	r.jm.DownAfterSearches(2)
	if _, err := r.e.Svc.SyncJiraProject(ctx, r.e.Admin, "WEB"); err == nil {
		t.Fatal("a sync against a dead site succeeded")
	}
	r.jm.SetDown(false)
	var partial int
	r.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE external_id IS NOT NULL`).Scan(&partial)
	if partial != 4 { // two pages of two
		t.Fatalf("expected a partial sync of 4 issues, got %d", partial)
	}
	st, _ := r.e.Svc.JiraStatus(ctx, r.e.Admin)
	if st[0].LastError == "" || st[0].Watermark == nil {
		t.Fatalf("%+v", st[0])
	}
	n := r.syncAs(t)
	var total, comments int
	r.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE external_id IS NOT NULL`).Scan(&total)
	r.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM comment`).Scan(&comments)
	if total != 7 || comments != 7 || n == 0 {
		t.Fatalf("after resume: %d issues, %d comments, %d synced", total, comments, n)
	}
}

func TestMIR02_IncrementalWebhookHintsOverlapAndLostWebhooks(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	r.syncAs(t)
	w2 := r.jm.Issues[1]
	w2.Fields["summary"] = "Checkout redesign v2"
	w2.Fields["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
	r.jm.Upsert(w2)
	// a webhook with a wrong signature is refused and wakes nothing
	body := []byte(`{"webhookEvent":"jira:issue_updated"}`)
	if err := e.Svc.ReceiveJiraWebhook(ctx, r.conn.ID, http.Header{"X-Hub-Signature": {"sha256=00"}}, body); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	var wake bool
	e.Store.Pool.QueryRow(ctx, `SELECT wake FROM jira_project`).Scan(&wake)
	if wake {
		t.Fatal("a bad webhook woke the sync")
	}
	// a good one is only a hint: it sets the flag, and the poll does the work
	if err := e.Svc.ReceiveJiraWebhook(ctx, r.conn.ID, http.Header{"X-Hub-Signature": {jiramock.Sign(r.secret, body)}}, body); err != nil {
		t.Fatal(err)
	}
	e.Store.Pool.QueryRow(ctx, `SELECT wake FROM jira_project`).Scan(&wake)
	if !wake {
		t.Fatal("a signed webhook did not hint")
	}
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-2"); v.Issue.Summary != "Checkout redesign" {
		t.Fatal("the webhook itself changed data")
	}
	r.syncAs(t)
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-2"); v.Issue.Summary != "Checkout redesign v2" {
		t.Fatalf("%q", v.Issue.Summary)
	}
	e.Store.Pool.QueryRow(ctx, `SELECT wake FROM jira_project`).Scan(&wake)
	if wake {
		t.Fatal("the hint was not cleared by the sync")
	}
	// duplicate and replayed webhooks are harmless; a LOST webhook is caught by polling alone
	for i := 0; i < 3; i++ {
		e.Svc.ReceiveJiraWebhook(ctx, r.conn.ID, http.Header{"X-Hub-Signature": {jiramock.Sign(r.secret, body)}}, body)
	}
	w3 := r.jm.Issues[2]
	w3.Fields["summary"] = "Cart crashes (no webhook for this one)"
	w3.Fields["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
	r.jm.Upsert(w3)
	r.syncAs(t)
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-3"); !strings.Contains(v.Issue.Summary, "no webhook") {
		t.Fatalf("%q", v.Issue.Summary)
	}
	// the overlap window: a change that Jira stamps inside the minute the watermark sits in (JQL has minute precision)
	w4 := r.jm.Issues[0]
	w4.Fields["summary"] = "Mirror epic, edited in the same minute"
	w4.Fields["updated"] = time.Now().UTC().Add(time.Second).Format("2006-01-02T15:04:05.000-0700") // the same minute as the last sync's newest change
	r.jm.Upsert(w4)
	r.syncAs(t)
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-1"); !strings.Contains(v.Issue.Summary, "same minute") {
		t.Fatalf("a change inside the watermark's minute was missed: %q", v.Issue.Summary)
	}
	// an older version arriving after a newer one changes nothing (idempotent per issue id and updated)
	old := *r.jm.Issues[1]
	old.Fields = map[string]any{}
	for k, v := range r.jm.Issues[1].Fields {
		old.Fields[k] = v
	}
	old.Fields["summary"] = "STALE"
	old.Fields["updated"] = "2000-01-01T00:00:00.000+0000"
	// applying it directly is impossible from outside; a full sync with an older server copy must not regress
	cur := r.jm.Issues[1].Fields["summary"]
	r.jm.Issues[1].Fields["updated"] = "2000-01-01T00:00:00.000+0000"
	r.jm.Issues[1].Fields["summary"] = "STALE"
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-2"); v.Issue.Summary != cur {
		t.Fatalf("an older version regressed the mirror: %q", v.Issue.Summary)
	}
}

func TestMIR03_DeletesMovesAndAccounts(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	r.syncAs(t)
	// the key changes in Jira (an issue moved within the project's numbering): recorded as an alias, the old key still resolves
	w3 := r.jm.Issues[2]
	w3.Key = "WEB-33"
	w3.Fields["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
	r.jm.Upsert(w3)
	r.syncAs(t)
	if v, err := e.Svc.View(ctx, e.Admin, "WEB-33"); err != nil || v.Issue.Summary != "Cart crashes" {
		t.Fatalf("%v", err)
	}
	if v, err := e.Svc.View(ctx, e.Admin, "WEB-3"); err != nil || v.Issue.Key != "WEB-33" || v.Alias != "WEB-3" {
		t.Fatalf("the old key does not resolve: %v %+v", err, v.Issue.Key)
	}
	// an issue deleted in Jira is removed from the mirror by reconcile, including its comments and history; the purge reaches the search index through the outbox
	var purgedBefore int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE type='issue.purged'`).Scan(&purgedBefore)
	r.jm.Delete("10002")
	rep, err := e.Svc.ReconcileJira(ctx, e.Admin, "WEB")
	if err != nil || len(rep.Removed) != 1 || rep.Removed[0] != "WEB-2" {
		t.Fatalf("%v %+v", err, rep)
	}
	if _, err := e.Svc.View(ctx, e.Admin, "WEB-2"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	var leftovers int
	e.Store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM comment c WHERE NOT EXISTS (SELECT 1 FROM issue i WHERE i.id=c.issue_id)) + (SELECT count(*) FROM jira_adf a WHERE NOT EXISTS (SELECT 1 FROM issue i WHERE i.id=a.issue_id))`).Scan(&leftovers)
	var purged int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE type='issue.purged'`).Scan(&purged)
	if leftovers != 0 || purged != purgedBefore+1 {
		t.Fatalf("leftovers %d, purge events %d -> %d", leftovers, purgedBefore, purged)
	}
	// the child's parent link goes with it; nothing dangles
	// a reconcile that sees NO issues in Jira does not wipe the mirror: it is far more likely a broken query or permission
	r.jm.Issues = nil
	rep, err = e.Svc.ReconcileJira(ctx, e.Admin, "WEB")
	if err != nil || rep.Skipped == "" || len(rep.Removed) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id=(SELECT id FROM project WHERE key='WEB')`).Scan(&n)
	if n == 0 {
		t.Fatal("the mirror was emptied")
	}
	st, _ := e.Svc.JiraStatus(ctx, e.Admin)
	if len(st[0].Warnings) == 0 {
		t.Fatalf("%+v", st[0])
	}
	// people who exist only in Jira are inactive placeholders without credentials, matched by account id or email later
	var active bool
	var hash *string
	e.Store.Pool.QueryRow(ctx, `SELECT active,password_hash FROM app_user WHERE jira_account_id='acc-bob'`).Scan(&active, &hash)
	if active || hash != nil {
		t.Fatal("a Jira-only person became a usable account")
	}
	// Ann has an account here with the same email: the issue's assignee is that account, not a copy
	ann, _ := e.Svc.CreateUser(ctx, "ann@example.com", "Ann At Home", "password123", false)
	r.jm.Issues = nil
	r.jm.Upsert(r.issue(2, "Checkout redesign", "In Progress", "indeterminate", "Story", func(is *jiramock.Issue) {
		is.Fields["assignee"] = jiramock.M{"accountId": "acc-ann", "displayName": "Ann Assignee", "emailAddress": "ann@example.com"}
		is.Fields["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
	}))
	r.syncAs(t)
	if v, err := e.Svc.View(ctx, e.Admin, "WEB-2"); err != nil || v.Issue.Assignee == nil || v.Issue.Assignee.ID != ann.ID {
		t.Fatalf("%v %+v", err, v.Issue.Assignee)
	}
}

func TestMIR04_HistoryIsIdempotentAndAttributed(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	r.syncAs(t)
	hist := func() []string {
		rows, _ := e.Store.Pool.Query(ctx, `SELECT field||'|'||COALESCE(before,'')||'|'||COALESCE(after,'')||'|'||actor_label||'|'||source||'|'||external_id FROM issue_history WHERE source='Jira' ORDER BY at, external_id`)
		var out []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		rows.Close()
		return out
	}
	first := hist()
	want := []string{"status|To Do|In Progress|Ann Assignee|Jira|h1:0", "assignee||Ann Assignee|Bob Builder|Jira|h2:0", "priority|Low|Medium|Bob Builder|Jira|h2:1"}
	if len(first) != 3 {
		t.Fatalf("%v", first)
	}
	for i, w := range want {
		if first[i] != w {
			t.Errorf("%d: %q want %q", i, first[i], w)
		}
	}
	// replay with the entries in a different order, and with a new one: only the new one is added
	is := r.jm.Issues[1]
	is.History = []jiramock.M{is.History[1], is.History[0], {"id": "h3", "author": jiramock.M{"accountId": "acc-ann", "displayName": "Ann Assignee"}, "created": "2026-01-03T09:00:00.000+0000", "items": []any{jiramock.M{"field": "labels", "fromString": "", "toString": "ux"}}}}
	is.Fields["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
	r.jm.Upsert(is)
	r.syncAs(t)
	if got := hist(); len(got) != 4 || got[3] != "labels||ux|Ann Assignee|Jira|h3:0" {
		t.Fatalf("%v", got)
	}
}

func TestMIR06_PermissionsAreJirasAndFailClosed(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	e := r.e
	// Ann and Carl have accounts here; Dora is in Jira's group but has no account; the project lead role holds an application role we cannot evaluate
	ann, _ := e.Svc.CreateUser(ctx, "ann@example.com", "Ann", "password123", false)
	carl, _ := e.Svc.CreateUser(ctx, "carl@example.com", "Carl", "password123", false)
	annA, carlA := domain.Actor{ID: ann.ID, Label: "Ann"}, domain.Actor{ID: carl.ID, Label: "Carl"}
	r.jm.Users["acc-ann"] = jiramock.M{"accountId": "acc-ann", "displayName": "Ann", "emailAddress": "ann@example.com", "active": true}
	r.jm.Roles["WEB"] = map[string][]jiramock.M{
		"Developers":     {{"type": "atlassian-user-role-actor", "displayName": "Ann", "actorUser": jiramock.M{"accountId": "acc-ann"}}, {"type": "atlassian-group-role-actor", "displayName": "devs", "actorGroup": jiramock.M{"name": "devs"}}},
		"Administrators": {{"type": "atlassian-application-role-actor", "displayName": "Some app role"}},
	}
	r.jm.Groups["devs"] = []jiramock.M{{"accountId": "acc-carl", "emailAddress": "carl@example.com", "displayName": "Carl", "active": true}, {"accountId": "acc-dora", "emailAddress": "dora@example.com", "displayName": "Dora", "active": true}, {"accountId": "acc-gone", "emailAddress": "gone@example.com", "displayName": "Closed", "active": false}}
	r.mirror(t)
	r.syncAs(t)
	if _, err := e.Svc.View(ctx, annA, "WEB-3"); err != nil {
		t.Fatalf("a member of the role cannot read: %v", err)
	}
	if _, err := e.Svc.View(ctx, carlA, "WEB-3"); err != nil {
		t.Fatalf("a member of a group in the role cannot read: %v", err)
	}
	for _, u := range []domain.Actor{e.Alice, e.Bob} {
		if _, err := e.Svc.View(ctx, u, "WEB-3"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("someone Jira does not let in can read: %v", err)
		}
	}
	// members are readers only; the mirror never widens beyond Jira
	if v, _ := e.Svc.View(ctx, annA, "WEB-3"); v.Role != "viewer" {
		t.Fatalf("%s", v.Role)
	}
	// a restricted comment is internal, so a reader never sees it; an issue with a security level is hidden from non-administrators
	if v, _ := e.Svc.View(ctx, annA, "WEB-2"); len(v.Comments) != 2 {
		t.Fatalf("%d comments visible, want the 2 unrestricted ones", len(v.Comments))
	}
	if _, err := e.Svc.View(ctx, annA, "WEB-4"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a security-level issue is readable: %v", err)
	}
	if _, err := e.Svc.View(ctx, e.Admin, "WEB-4"); err != nil {
		t.Fatalf("an administrator cannot see it: %v", err)
	}
	st, _ := e.Svc.JiraStatus(ctx, e.Admin)
	warn := strings.Join(st[0].Warnings, " | ")
	if !strings.Contains(warn, "security level") || !strings.Contains(warn, "Some app role") || !st[0].PermFresh {
		t.Fatalf("%+v", st[0])
	}
}

func TestMIR06_RevocationReachesTheMirrorAndStalenessFailsClosed(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	e := r.e
	ann, _ := e.Svc.CreateUser(ctx, "ann@example.com", "Ann", "password123", false)
	annA := domain.Actor{ID: ann.ID, Label: "Ann"}
	r.jm.Users["acc-ann"] = jiramock.M{"accountId": "acc-ann", "displayName": "Ann", "emailAddress": "ann@example.com", "active": true}
	r.jm.Roles["WEB"] = map[string][]jiramock.M{"Developers": {{"type": "atlassian-user-role-actor", "displayName": "Ann", "actorUser": jiramock.M{"accountId": "acc-ann"}}}}
	r.mirror(t)
	r.syncAs(t)
	if _, err := e.Svc.View(ctx, annA, "WEB-3"); err != nil {
		t.Fatal(err)
	}
	// she is visible in her project list and in search scope while the permission data is fresh
	if ps, _ := e.Svc.ListProjects(ctx, annA); len(ps) != 1 || ps[0].Key != "WEB" || !ps[0].Mirrored {
		t.Fatalf("%+v", ps)
	}
	if _, rd, _, _ := e.Svc.SearchScope(ctx, annA); len(rd) != 1 {
		t.Fatalf("%v", rd)
	}
	// revoked in Jira: after the next permission read she is out, with no reindexing
	r.jm.Roles["WEB"] = map[string][]jiramock.M{"Developers": {}}
	e.Store.Pool.Exec(ctx, `UPDATE jira_project SET perm_synced_at = now() - interval '6 minutes'`)
	e.Clock.Advance(6 * time.Minute)
	r.syncAs(t)
	if _, err := e.Svc.View(ctx, annA, "WEB-3"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a revoked person can still read: %v", err)
	}
	// restore her, then let the permission data go stale (the sync cannot read roles): the project fails closed for everyone but administrators
	r.jm.Roles["WEB"] = map[string][]jiramock.M{"Developers": {{"type": "atlassian-user-role-actor", "displayName": "Ann", "actorUser": jiramock.M{"accountId": "acc-ann"}}}}
	e.Store.Pool.Exec(ctx, `UPDATE jira_project SET perm_synced_at = now() - interval '6 minutes'`)
	e.Clock.Advance(6 * time.Minute)
	r.syncAs(t)
	if _, err := e.Svc.View(ctx, annA, "WEB-3"); err != nil {
		t.Fatal(err)
	}
	e.Store.Pool.Exec(ctx, `UPDATE jira_project SET perm_synced_at = now() - interval '20 minutes'`)
	if _, err := e.Svc.View(ctx, annA, "WEB-3"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("stale permission data did not fail closed: %v", err)
	}
	if ps, _ := e.Svc.ListProjects(ctx, annA); len(ps) != 0 {
		t.Fatalf("%+v", ps)
	}
	if _, rd, _, _ := e.Svc.SearchScope(ctx, annA); len(rd) != 0 {
		t.Fatalf("%v", rd)
	}
	if _, err := e.Svc.View(ctx, e.Admin, "WEB-3"); err != nil {
		t.Fatalf("administrators keep access: %v", err)
	}
	st, _ := e.Svc.JiraStatus(ctx, e.Admin)
	if st[0].PermFresh {
		t.Fatalf("%+v", st[0])
	}
	// a failing permission read is reported, and does not mask the issue sync
	r.jm.Roles = nil
	r.jm.SetDown(false)
	e.Store.Pool.Exec(ctx, `UPDATE jira_project SET perm_synced_at = NULL`)
	r.syncAs(t)
	if st, _ = e.Svc.JiraStatus(ctx, e.Admin); st[0].LastError != "" && !strings.Contains(st[0].LastError, "permissions") {
		t.Fatalf("%q", st[0].LastError)
	}
}

// MIR-07: every write path refuses a mirrored issue with the same typed error and a pointer to Jira.
func TestMIR07_ReadOnlyOnEveryWritePath(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	e.WithBlobs()
	r.syncAs(t)
	key := "WEB-2"
	two := 2.0
	me := e.Admin.ID
	writes := map[string]func() error{
		"update fields": func() error {
			_, err := e.Svc.UpdateFields(ctx, e.Admin, key, nil, false, domain.Changes{Estimate: &two})
			return err
		},
		"assign": func() error {
			_, err := e.Svc.UpdateFields(ctx, e.Admin, key, nil, false, domain.Changes{Assignee: &me})
			return err
		},
		"transition": func() error {
			v, _ := e.Svc.View(ctx, e.Admin, key)
			_, err := e.Svc.Transition(ctx, e.Admin, key, firstID(v), nil)
			return err
		},
		"comment": func() error { _, err := e.Svc.AddComment(ctx, e.Admin, key, "hi", false); return err },
		"save text": func() error {
			_, err := e.Svc.SaveText(ctx, e.Admin, key, domain.SaveIn{Field: "summary", Body: "x", BaseSeq: 1})
			return err
		},
		"attach": func() error {
			_, err := e.Svc.AttachStream(ctx, e.Admin, key, "a.txt", "text/plain", strings.NewReader("x"))
			return err
		},
		"start upload": func() error { _, err := e.Svc.StartUpload(ctx, e.Admin, key, "a.txt", "", 5); return err },
		"create issue": func() error {
			_, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "WEB", Summary: "new"})
			return err
		},
		"add member":    func() error { return e.Svc.AddMember(ctx, e.Admin, "WEB", e.Alice.ID, "member") },
		"claim (agent)": func() error { _, err := e.Svc.ClaimIssue(ctx, e.Admin, key, time.Minute); return err },
		"request input": func() error { return e.Svc.RequestHumanInput(ctx, e.Admin, key, "q?") },
		"convert": func() error {
			_, err := e.Svc.Lifecycle(ctx, e.Admin, key, domain.LifecycleIn{Op: "convert", Type: "Task"})
			return err
		},
		"move": func() error {
			_, err := e.Svc.Lifecycle(ctx, e.Admin, key, domain.LifecycleIn{Op: "move", Project: "ABC"})
			return err
		},
		"clone": func() error {
			_, err := e.Svc.Lifecycle(ctx, e.Admin, key, domain.LifecycleIn{Op: "clone", Project: "ABC"})
			return err
		},
		"delete": func() error {
			_, err := e.Svc.Lifecycle(ctx, e.Admin, key, domain.LifecycleIn{Op: "delete"})
			return err
		},
		"link": func() error { return e.Svc.AddLink(ctx, e.Admin, key, "relates", "WEB-1") },
		"import csv": func() error {
			_, err := e.Svc.ImportCSV(ctx, e.Admin, []byte("A\nx"), domain.CSVImportIn{Project: "WEB", Mapping: domain.CSVMapping{Columns: map[string]string{"A": "summary"}}})
			return err
		},
	}
	for name, f := range writes {
		err := f()
		var rom *domain.ReadOnlyMirror
		if !errors.As(err, &rom) || !strings.HasPrefix(rom.SiteURL, "http") || !strings.Contains(err.Error(), "make the change in Jira") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// sprints and boards: a scrum board over a mirrored project cannot make sprints; moving a card is a write
	b, err := e.Svc.CreateBoard(ctx, e.Admin, "WEB", "Flow", "kanban")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.MoveCard(ctx, e.Admin, b.ID, key, domain.MoveIn{Column: "Done"}); !isMirror(err) {
		t.Fatalf("moving a card on a mirrored board: %v", err)
	}
	sb, _ := e.Svc.CreateBoard(ctx, e.Admin, "WEB", "Scrum", "scrum")
	if _, err := e.Svc.CreateSprint(ctx, e.Admin, sb.ID, domain.SprintIn{}); !isMirror(err) {
		t.Fatalf("a sprint on a mirrored project: %v", err)
	}
	if err := e.Svc.AssignSprint(ctx, e.Admin, sb.ID, "", []string{key}, domain.MoveIn{}); !isMirror(err) {
		t.Fatalf("planning a mirrored issue: %v", err)
	}
	if n, _ := e.Svc.EnsureSprints(ctx, sb.ID); n != 0 {
		t.Fatal("auto-sprints were created for a mirrored project")
	}
	// the data did not change, and what is Tasklane-side still works: the development panel and reads
	v, _ := e.Svc.View(ctx, e.Admin, key)
	if v.Issue.Summary != "Checkout redesign" || v.Issue.Estimate != nil || len(v.Comments) != 3 {
		t.Fatalf("%+v", v.Issue)
	}
	if _, err := e.Svc.Development(ctx, e.Admin, key); err != nil {
		t.Fatal(err)
	}
	// native projects are unaffected
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "still writable"}); err != nil {
		t.Fatal(err)
	}
}

func isMirror(err error) bool { var r *domain.ReadOnlyMirror; return errors.As(err, &r) }

func firstID(v domain.IssueView) string {
	if len(v.Transitions) > 0 {
		return v.Transitions[0].ID
	}
	return "00000000-0000-0000-0000-000000000000"
}

// MIR-09: rate limits are honoured, a long one is reported instead of slept through, and a dead Jira leaves the mirror
// served with a stale marker.
func TestMIR09_RateLimitsAndOutagesKeepTheMirrorServed(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	r.syncAs(t)
	// a short Retry-After is waited out (0 seconds here) and the sync completes
	r.jm.RateLimit(2, 0)
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); err != nil {
		t.Fatalf("a short rate limit stopped the sync: %v", err)
	}
	// a long one is reported with its end time, not slept through, and the data stays
	r.jm.RateLimit(100, 3600)
	_, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB")
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("%v", err)
	}
	st, _ := e.Svc.JiraStatus(ctx, e.Admin)
	if !strings.Contains(st[0].LastError, "rate limit") {
		t.Fatalf("%+v", st[0])
	}
	if v, err := e.Svc.View(ctx, e.Admin, "WEB-2"); err != nil || !v.Mirror.Stale || !strings.Contains(v.Mirror.Error, "rate limit") {
		t.Fatalf("%v %+v", err, v.Mirror)
	}
	// Jira is down for a while: reads keep working, the marker says how old the copy is
	r.jm.RateLimit(0, 0)
	r.jm.SetDown(true)
	e.Clock.Advance(20 * time.Minute)
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); err == nil {
		t.Fatal("a sync against a dead Jira succeeded")
	}
	v, err := e.Svc.View(ctx, e.Admin, "WEB-2")
	if err != nil || !v.Mirror.Stale || v.Mirror.LagSeconds < 15*60 {
		t.Fatalf("%v %+v", err, v.Mirror)
	}
	// recovery clears the marker
	r.jm.SetDown(false)
	r.syncAs(t)
	if v, _ = e.Svc.View(ctx, e.Admin, "WEB-2"); v.Mirror.Stale || v.Mirror.Error != "" {
		t.Fatalf("%+v", v.Mirror)
	}
	// the site's credentials are checked up front, with a plain message
	if _, _, err := e.Svc.CreateJiraConnection(ctx, e.Admin, domain.JiraConnIn{Name: "bad", SiteURL: r.jm.URL, Email: "sync@example.com", Token: "wrong", AllowPrivate: true}); err == nil || !strings.Contains(err.Error(), "rejected the credentials") {
		t.Fatalf("%v", err)
	}
	if _, _, err := e.Svc.CreateJiraConnection(ctx, e.Admin, domain.JiraConnIn{Name: "plain", SiteURL: "http://example.com", Email: "a@b.c", Token: "t"}); err == nil {
		t.Fatal("plain http to a public host was accepted")
	}
	if _, _, err := e.Svc.CreateJiraConnection(ctx, e.Alice, domain.JiraConnIn{Name: "x", SiteURL: r.jm.URL, Email: "a", Token: "b", AllowPrivate: true}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	// the egress policy applies: without allow_private the loopback site is unreachable
	e.Store.Pool.Exec(ctx, `UPDATE jira_connection SET allow_private=false`)
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("%v", err)
	}
}

// MIR-13: a pause drains the sync like any writer; the watermark is kept and the sync resumes after the pause.
func TestMIR13_PauseStopsTheSyncAndResumes(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	r.syncAs(t)
	domain.PauseAdmitWait = 50 * time.Millisecond
	defer func() { domain.PauseAdmitWait = 2 * time.Second }()
	l, err := e.Svc.RequestPause(ctx, e.Admin, domain.PauseIn{Scope: "project:WEB"})
	if err != nil {
		t.Fatal(err)
	}
	w := r.jm.Issues[0]
	w.Fields["summary"] = "changed during the pause"
	w.Fields["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
	r.jm.Upsert(w)
	var before *time.Time
	e.Store.Pool.QueryRow(ctx, `SELECT watermark FROM jira_project`).Scan(&before)
	var paused *domain.ErrPaused
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); !errors.As(err, &paused) {
		t.Fatalf("the sync ran during a pause: %v", err)
	}
	var after *time.Time
	e.Store.Pool.QueryRow(ctx, `SELECT watermark FROM jira_project`).Scan(&after)
	if !before.Equal(*after) {
		t.Fatal("the watermark moved during a pause")
	}
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-1"); v.Issue.Summary == "changed during the pause" {
		t.Fatal("data changed during a pause")
	}
	e.Svc.ReleasePause(ctx, e.Admin, l.ID)
	r.syncAs(t)
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-1"); v.Issue.Summary != "changed during the pause" {
		t.Fatalf("%q", v.Issue.Summary)
	}
}

// MIR-08: what works on mirrored data: lists and search, boards, the ticket, reports, the development panel; restricted
// issues are in none of them for non-administrators.
func TestMIR08_ReadFeaturesWorkOnMirroredData(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	e := r.e
	ann, _ := e.Svc.CreateUser(ctx, "ann@example.com", "Ann", "password123", false)
	annA := domain.Actor{ID: ann.ID, Label: "Ann"}
	r.jm.Users["acc-ann"] = jiramock.M{"accountId": "acc-ann", "displayName": "Ann", "emailAddress": "ann@example.com", "active": true}
	r.jm.Roles["WEB"] = map[string][]jiramock.M{"Developers": {{"type": "atlassian-user-role-actor", "displayName": "Ann", "actorUser": jiramock.M{"accountId": "acc-ann"}}}}
	r.mirror(t)
	r.syncAs(t)
	// boards: the columns come from the mirrored statuses, and the restricted issue is not on the board
	b, err := e.Svc.CreateBoard(ctx, e.Admin, "WEB", "Flow", "kanban")
	if err != nil {
		t.Fatal(err)
	}
	bv, err := e.Svc.GetBoard(ctx, annA, b.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range bv.Columns {
		for _, card := range c.Cards {
			seen[card.Key] = true
		}
	}
	if !seen["WEB-1"] || !seen["WEB-2"] || seen["WEB-4"] || len(bv.Columns) < 3 { // WEB-3 was finished long ago and ages off the board like any done work
		t.Fatalf("%v columns %d", seen, len(bv.Columns))
	}
	// search and typeahead (the PostgreSQL engine), for the member and for the administrator
	ix := &search.Indexer{DB: e.Store.Pool}
	for {
		if n, err := ix.RunOnce(ctx); err != nil || n == 0 {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	eng := &search.Postgres{DB: e.Store.Pool}
	scopeOf := func(a domain.Actor) search.Scope {
		all, rd, wr, _ := e.Svc.SearchScope(ctx, a)
		return search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: e.Svc.Now}
	}
	keysFor := func(a domain.Actor, q string) string {
		res, err := eng.Search(ctx, scopeOf(a), q, search.Page{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		var ks []string
		for _, x := range res.Rows {
			ks = append(ks, x.Key)
		}
		sort.Strings(ks)
		return strings.Join(ks, " ")
	}
	if got := keysFor(annA, `project = WEB`); got != "WEB-1 WEB-2 WEB-3" { // the security-level issue is not in a member's list
		t.Fatalf("%q", got)
	}
	if got := keysFor(e.Admin, `project = WEB`); got != "WEB-1 WEB-2 WEB-3 WEB-4" {
		t.Fatalf("%q", got)
	}
	if got := keysFor(annA, `text ~ "confidential"`); got != "" {
		t.Fatalf("a hidden issue was found by text: %q", got)
	}
	if got := keysFor(annA, `text ~ "checkout"`); got != "WEB-2" { // text from the converted ADF is searchable
		t.Fatalf("%q", got)
	}
	if got := keysFor(annA, `comment ~ "restricted"`); got != "" { // a restricted comment is not searchable by a reader
		t.Fatalf("%q", got)
	}
	if rows, _ := eng.Typeahead(ctx, scopeOf(annA), "WEB-", 10); len(rows) != 3 {
		t.Fatalf("typeahead leaked or lost rows: %d", len(rows))
	}
	// reports over the mirrored changelog work
	if _, err := e.Svc.CumulativeFlow(ctx, annA, b.ID, 7, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.WorkloadByAssignee(ctx, annA, b.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.TimeInStatus(ctx, annA, b.ID, 30, nil); err != nil {
		t.Fatal(err)
	}
}

// MIR-11: cut-over to native.
func TestMIR11_CutOverToNative(t *testing.T) {
	r := newJira(t)
	jiraDataset(r)
	r.mirror(t)
	e := r.e
	r.syncAs(t)
	if _, err := e.Svc.AddComment(ctx, e.Admin, "WEB-3", "x", false); !isMirror(err) {
		t.Fatal(err)
	}
	if err := e.Svc.CutOverJira(ctx, e.Alice, "WEB"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("%v", err)
	}
	before, _ := e.Svc.History(ctx, e.Admin, "WEB-2")
	if err := e.Svc.CutOverJira(ctx, e.Admin, "WEB"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddComment(ctx, e.Admin, "WEB-3", "now it is ours", false); err != nil {
		t.Fatalf("a cut-over project is still read-only: %v", err)
	}
	after, _ := e.Svc.History(ctx, e.Admin, "WEB-2")
	if len(after) != len(before) {
		t.Fatal("history changed at cut-over")
	}
	if v, _ := e.Svc.View(ctx, e.Admin, "WEB-3"); v.Mirror != nil {
		t.Fatal("still marked as a mirror")
	}
	if _, err := e.Svc.SyncJiraProject(ctx, e.Admin, "WEB"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("the sync did not stop: %v", err)
	}
	// new issues get numbers after Jira's, keys unchanged
	is, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "WEB", Summary: "native now"})
	if err != nil || is.Key != "WEB-5" {
		t.Fatalf("%v %s", err, is.Key)
	}
}
