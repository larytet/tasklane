package domain_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/jiramock"
)

// Tests of Jira write-through (SPEC 18.1.1, TESTPLAN WT-01 to WT-18) against the recorded Jira double.

const adminToken = "admin-personal-token-9f3c"

var other = jiramock.Account{Email: "other@example.com", AccountID: "acc-other", Name: "Other Person"}

func wtStr(s string) *string { return &s }

// richDescription is a description as Jira's editor makes one: a heading, bold text, a link, a mention, a list, a code block, and a panel
// that Tasklane's model does not hold (it travels as an opaque block).
func richDescription() jiramock.M {
	txt := func(s string, marks ...jiramock.M) jiramock.M {
		n := jiramock.M{"type": "text", "text": s}
		if len(marks) > 0 {
			ms := []any{}
			for _, m := range marks {
				ms = append(ms, m)
			}
			n["marks"] = ms
		}
		return n
	}
	item := func(s string) jiramock.M {
		return jiramock.M{"type": "listItem", "content": []any{jiramock.M{"type": "paragraph", "content": []any{txt(s)}}}}
	}
	return jiramock.M{"type": "doc", "version": 1, "content": []any{
		jiramock.M{"type": "heading", "attrs": jiramock.M{"level": float64(2)}, "content": []any{txt("Plan")}},
		jiramock.M{"type": "paragraph", "content": []any{txt("Do "), txt("this", jiramock.M{"type": "strong"}), txt(" first, see "),
			txt("the spec", jiramock.M{"type": "link", "attrs": jiramock.M{"href": "https://example.com/spec"}}), txt(", then ask "),
			jiramock.M{"type": "mention", "attrs": jiramock.M{"id": "acc-ann", "text": "@Ann"}}}},
		jiramock.M{"type": "bulletList", "content": []any{item("one"), item("two")}},
		jiramock.M{"type": "codeBlock", "attrs": jiramock.M{"language": "go"}, "content": []any{txt("fmt.Println(1)")}},
		jiramock.M{"type": "panel", "attrs": jiramock.M{"panelType": "info"}, "content": []any{jiramock.M{"type": "paragraph", "content": []any{txt("keep me")}}}},
	}}
}

// adfNodeTypes counts the node types of a document (and marks as "mark:<type>").
func adfNodeTypes(v any) map[string]int {
	out := map[string]int{}
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			if t, ok := x["type"].(string); ok {
				out[t]++
			}
			if ms, ok := x["marks"].([]any); ok {
				for _, m := range ms {
					if mm, ok := m.(map[string]any); ok {
						out["mark:"+fmt.Sprint(mm["type"])]++
					}
				}
			}
			if c, ok := x["content"].([]any); ok {
				for _, e := range c {
					walk(e)
				}
			}
		}
	}
	walk(v)
	return out
}

// wtRig is a mirrored project WEB that writes through, with the administrator holding a personal token.
func wtRig(t *testing.T) *jiraRig {
	t.Helper()
	r := newJira(t)
	r.mirror(t)
	r.issue(1, "Fix login", "To Do", "new", "Task", func(is *jiramock.Issue) {
		is.Fields["description"] = jiramock.ADFParagraph("First paragraph", "Second paragraph")
		is.Fields["labels"] = []any{"auth"}
	})
	r.issue(2, "Formatted", "To Do", "new", "Task", func(is *jiramock.Issue) {
		is.Fields["description"] = richDescription()
	})
	r.jm.Transitions = []jiramock.Trans{
		{ID: "11", Name: "Start", From: "To Do", To: "In Progress"},
		{ID: "21", Name: "Finish", From: "In Progress", To: "Done"},
	}
	r.syncAs(t)
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "WEB", "write_through"); err != nil {
		t.Fatal(err)
	}
	r.jm.AddAccount("admin@example.com", adminToken, "acc-admin", "Admin")
	// a status Jira offers that the mirror has not seen yet still needs its move: the sync adds it when it sees the status
	return r
}

func (r *jiraRig) giveToken(t *testing.T) {
	t.Helper()
	if err := r.e.Svc.SetJiraSessionToken(ctx, r.e.Admin, r.conn.ID, "", adminToken); err != nil {
		t.Fatal(err)
	}
}

func (r *jiraRig) puts() int {
	n := 0
	for _, w := range r.jm.Writes() {
		if w.Method == "PUT" {
			n++
		}
	}
	return n
}

func (r *jiraRig) issueOf(t *testing.T, key string) domain.Issue {
	t.Helper()
	v, err := r.e.Svc.View(ctx, r.e.Admin, key)
	if err != nil {
		t.Fatal(err)
	}
	return v.Issue
}

func (r *jiraRig) bodyOf(t *testing.T, key, field string) string {
	t.Helper()
	var b string
	if err := r.e.Store.Pool.QueryRow(ctx, `SELECT body FROM issue_text t JOIN issue i ON i.id=t.issue_id WHERE i.key=$1 AND t.field=$2`, key, field).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *jiraRig) auditCount(t *testing.T, outcome string) int {
	t.Helper()
	var n int
	r.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='jira.write' AND detail->>'outcome'=$1`, outcome).Scan(&n)
	return n
}

// WT-01: no value that authenticates to Jira is in any table, whatever was done with the connection.
func TestWT01_NoJiraTokenInAnyTable(t *testing.T) {
	const svcTok = "service-token-SECRET-77aa"
	e := testutil.New(t)
	e.Clock.T = time.Now()
	jm := jiramock.New("sync@example.com", svcTok)
	t.Cleanup(jm.Close)
	jm.AddAccount("admin@example.com", adminToken, "acc-admin", "Admin")
	conn, _, err := e.Svc.CreateJiraConnection(ctx, e.Admin, domain.JiraConnIn{Name: "acme", SiteURL: jm.URL, Email: "sync@example.com", Token: svcTok, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.UnlockJiraConnection(ctx, e.Admin, conn.ID, svcTok); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.SetJiraSessionToken(ctx, e.Admin, conn.ID, "", adminToken); err != nil {
		t.Fatal(err)
	}
	// the fingerprint is the only trace, and it is short
	if len(conn.TokenFP) != 8 {
		t.Fatalf("fingerprint %q", conn.TokenFP)
	}
	rows, err := e.Store.Pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	if len(tables) < 20 {
		t.Fatalf("only %d tables listed", len(tables))
	}
	for _, tok := range []string{svcTok, adminToken} {
		for _, tb := range tables {
			var hit bool
			q := fmt.Sprintf(`SELECT bool_or(position($1 in row_to_json(x)::text) > 0 OR position($2 in row_to_json(x)::text) > 0) FROM %s x`, pgx.Identifier{tb}.Sanitize())
			var h *bool
			if err := e.Store.Pool.QueryRow(ctx, q, tok, hex.EncodeToString([]byte(tok))).Scan(&h); err != nil {
				t.Fatalf("%s: %v", tb, err)
			}
			if h != nil {
				hit = *h
			}
			if hit {
				t.Fatalf("the token %q... is in table %s", tok[:8], tb)
			}
		}
	}
	// the stored column, if the release still has it, is empty
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM jira_connection WHERE token_enc IS NOT NULL AND length(token_enc) > 0`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d connections still hold an encrypted token", n)
	}
}

// WT-01: a backup made before the token left the database may still carry one; restoring it brings none back.
func TestWT01_AnOldBackupDoesNotBringATokenBack(t *testing.T) {
	src := testutil.New(t)
	src.Clock.T = time.Now()
	jm := jiramock.New("sync@example.com", "api-token")
	t.Cleanup(jm.Close)
	conn, _, err := src.Svc.CreateJiraConnection(ctx, src.Admin, domain.JiraConnIn{Name: "acme", SiteURL: jm.URL, Email: "sync@example.com", Token: "api-token", AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	// what an old release had stored
	if _, err := src.Store.Pool.Exec(ctx, `UPDATE jira_connection SET token_enc = '\xdeadbeefcafe'::bytea`); err != nil {
		t.Fatal(err)
	}
	dir, _ := backupTo(t, src)
	dst := freshTarget(t)
	if _, err := dst.Svc.Restore(ctx, dir); err != nil {
		t.Fatal(err)
	}
	var n, withTok int
	dst.Store.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE length(token_enc) > 0) FROM jira_connection`).Scan(&n, &withTok)
	if n != 1 || withTok != 0 {
		t.Fatalf("%d connections restored, %d with a stored token", n, withTok)
	}
	cs, err := dst.Svc.ListJiraConnections(ctx, restoredActor(t, dst, "admin@example.com"))
	if err != nil || len(cs) != 1 || cs[0].ID != conn.ID || !cs[0].Locked {
		t.Fatalf("a restored connection must be locked: %+v %v", cs, err)
	}
}

// WT-03: a restart locks every connection; the sync waits and the mirror keeps being served; an administrator unlocks it.
func TestWT03_LockedAfterRestartUntilUnlocked(t *testing.T) {
	r := wtRig(t)
	r.jm.Upsert(jiramock.IssueJSON("10003", "WEB-3", "WEB", "New in Jira", "To Do", "new", "Task", r.base, time.Now()))
	svc2 := domain.New(r.e.Store.Pool) // a restarted process: same database, empty memory
	svc2.Now = r.e.Clock.Now
	_, err := svc2.SyncJiraProject(ctx, r.e.Admin, "WEB")
	var locked *domain.ErrJiraLocked
	if !errors.As(err, &locked) {
		t.Fatalf("a restarted process must be locked, got %v", err)
	}
	cs, _ := svc2.ListJiraConnections(ctx, r.e.Admin)
	if len(cs) != 1 || !cs[0].Locked {
		t.Fatalf("status must say locked: %+v", cs)
	}
	st, _ := svc2.JiraStatus(ctx, r.e.Admin)
	if len(st) != 1 || !st[0].Locked || st[0].Mode != "write_through" || !strings.Contains(st[0].LastError, "locked") {
		t.Fatalf("project status: %+v", st)
	}
	// the mirror is still served
	if v, err := svc2.View(ctx, r.e.Admin, "WEB-1"); err != nil || v.Issue.Summary != "Fix login" {
		t.Fatalf("mirror not served while locked: %v", err)
	}
	// only an administrator unlocks; a wrong token and another account's token are refused and keep it locked
	if err := svc2.UnlockJiraConnection(ctx, r.e.Alice, r.conn.ID, "api-token"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a non-administrator unlocked: %v", err)
	}
	var inv *domain.InvalidError
	if err := svc2.UnlockJiraConnection(ctx, r.e.Admin, r.conn.ID, "wrong"); !errors.As(err, &inv) {
		t.Fatalf("a wrong token was accepted: %v", err)
	}
	r.jm.AddAccount("sync@example.com", "someone-elses", "acc-x", "X") // the same email cannot have two tokens in the double: use another email for "belongs to another"
	if _, err := svc2.SyncJiraProject(ctx, r.e.Admin, "WEB"); !errors.As(err, &locked) {
		t.Fatalf("still locked expected: %v", err)
	}
	if err := svc2.UnlockJiraConnection(ctx, r.e.Admin, r.conn.ID, "api-token"); err != nil {
		t.Fatal(err)
	}
	n, err := svc2.SyncJiraProject(ctx, r.e.Admin, "WEB")
	if err != nil || n < 1 {
		t.Fatalf("after unlock: %d %v", n, err)
	}
	cs, _ = svc2.ListJiraConnections(ctx, r.e.Admin)
	if cs[0].Locked {
		t.Fatal("unlocked connection reported as locked")
	}
}

// WT-04, WT-05: no token, no write; a person's own token is checked against their Jira account; the write is made as them.
func TestWT04_TokenRequiredThenWriteAsThePerson(t *testing.T) {
	r := wtRig(t)
	ch := domain.Changes{Priority: wtStr("high")}
	_, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, ch)
	var tr *domain.ErrTokenRequired
	if !errors.As(err, &tr) || tr.TokenPage != domain.JiraTokenPageFor(r.jm.URL) || tr.ConnectionID != r.conn.ID {
		t.Fatalf("no token must ask for one: %v", err)
	}
	if len(r.jm.Writes()) != 0 {
		t.Fatal("something was written without a token")
	}
	var inv *domain.InvalidError
	if err := r.e.Svc.SetJiraSessionToken(ctx, r.e.Admin, r.conn.ID, "", "not-the-token"); !errors.As(err, &inv) {
		t.Fatalf("a wrong token was accepted: %v", err)
	}
	// a valid token of someone else is refused: nobody saves under another name
	r.jm.AddAccount("bob@example.com", "bob-token", "acc-bob", "Bob")
	if err := r.e.Svc.SetJiraSessionToken(ctx, r.e.Admin, r.conn.ID, "bob@example.com", "bob-token"); !errors.As(err, &inv) {
		t.Fatalf("another person's token was accepted: %v", err)
	}
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, ch); !errors.As(err, &tr) {
		t.Fatalf("a refused token must not be kept: %v", err)
	}
	r.giveToken(t)
	out, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, ch)
	if err != nil {
		t.Fatal(err)
	}
	if out.Priority != "high" || r.issueOf(t, "WEB-1").Priority != "high" {
		t.Fatalf("the mirror must show Jira's accepted value: %q", out.Priority)
	}
	ws := r.jm.Writes()
	if len(ws) != 1 || ws[0].Method != "PUT" || ws[0].Email != "admin@example.com" {
		t.Fatalf("the write must be one PUT made as the person: %+v", ws)
	}
	for _, w := range ws {
		if w.Email == "sync@example.com" {
			t.Fatal("a write was made with the service credential (WT-05)")
		}
	}
	if got := r.jm.Field("WEB-1", "priority"); fmt.Sprint(got) != "map[name:High]" {
		t.Fatalf("Jira has %v", got)
	}
	if r.auditCount(t, "accepted") != 1 {
		t.Fatal("the accepted write was not audited")
	}
}

// WT-06: a token Jira stops accepting is dropped and asked for again; logout and a mode change drop tokens.
func TestWT06_TokenLifetime(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	cs, _ := r.e.Svc.ListJiraConnections(ctx, r.e.Admin)
	if cs[0].LiveTokens != 1 {
		t.Fatalf("live tokens %d", cs[0].LiveTokens)
	}
	r.jm.Accounts["admin@example.com"].Token = "rotated" // revoked in Jira
	_, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("low")})
	var tr *domain.ErrTokenRequired
	if !errors.As(err, &tr) {
		t.Fatalf("a revoked token must ask again: %v", err)
	}
	if cs, _ = r.e.Svc.ListJiraConnections(ctx, r.e.Admin); cs[0].LiveTokens != 0 {
		t.Fatal("the refused token was kept")
	}
	r.jm.Accounts["admin@example.com"].Token = adminToken
	r.giveToken(t)
	r.e.Svc.DropJiraTokensOf(r.e.Admin.ID) // logout
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("low")}); !errors.As(err, &tr) {
		t.Fatalf("logout must drop the token: %v", err)
	}
	r.giveToken(t)
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "WEB", "read_only"); err != nil {
		t.Fatal(err)
	}
	if cs, _ = r.e.Svc.ListJiraConnections(ctx, r.e.Admin); cs[0].LiveTokens != 0 {
		t.Fatal("leaving write_through must drop the tokens (WT-16)")
	}
	var rom *domain.ReadOnlyMirror
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("low")}); !errors.As(err, &rom) {
		t.Fatalf("read_only must refuse as before: %v", err)
	}
}

// WT-08: a change made in Jira since the person loaded the issue is reported, nothing is sent, and other fields do not collide.
func TestWT08_ConflictAgainstJiraNothingSent(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	r.jm.Change("WEB-1", "priority", jiramock.M{"name": "High"}, other) // the mirror has not seen it yet
	_, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("low")})
	var jc *domain.JiraConflict
	if !errors.As(err, &jc) || len(jc.Fields) != 1 || jc.Fields[0].Field != "priority" || jc.Fields[0].Jira != "high" || jc.Fields[0].Current != "high" || jc.Fields[0].Base != "medium" || jc.Fields[0].By != "Other Person" || jc.Fields[0].At == nil {
		t.Fatalf("conflict: %+v %v", jc, err)
	}
	if r.puts() != 0 {
		t.Fatal("a conflicting save sent a write")
	}
	if got := r.jm.Field("WEB-1", "priority"); fmt.Sprint(got) != "map[name:High]" {
		t.Fatalf("Jira's value was overwritten: %v", got)
	}
	if r.issueOf(t, "WEB-1").Priority != "medium" {
		t.Fatal("the mirror changed on a conflict")
	}
	if r.auditCount(t, "conflict") != 1 {
		t.Fatal("the conflict was not audited")
	}
	// "overwrite with mine": the person decides, the base now is Jira's value
	hi := "high"
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("low"), Base: &domain.ChangeBase{Priority: &hi}}); err != nil {
		t.Fatalf("overwrite after looking: %v", err)
	}
	if r.issueOf(t, "WEB-1").Priority != "low" {
		t.Fatal("the overwrite did not land")
	}
	// a different field changed in Jira does not collide with this edit
	r.jm.Change("WEB-1", "labels", []any{"auth", "urgent"}, other)
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("highest")}); err != nil {
		t.Fatalf("a change to another field must not collide: %v", err)
	}
	// the base the editor started from counts, even when the mirror and Jira agree with each other now
	low := "low"
	_, err = r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("medium"), Base: &domain.ChangeBase{Priority: &low}})
	if !errors.As(err, &jc) || jc.Fields[0].Base != "low" || jc.Fields[0].Jira != "highest" {
		t.Fatalf("the editor's base was ignored: %v", err)
	}
}

// WT-08: the short race between the check and the send is accepted: the last writer wins, as in Jira itself.
func TestWT08_RaceAfterTheCheckIsLastWriterWins(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	r.jm.OnWrite = func(method, path string) {
		if method == "PUT" {
			r.jm.Change("WEB-1", "priority", jiramock.M{"name": "Low"}, other)
		}
	}
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")}); err != nil {
		t.Fatal(err)
	}
	if got := r.jm.Field("WEB-1", "priority"); fmt.Sprint(got) != "map[name:High]" || r.issueOf(t, "WEB-1").Priority != "high" {
		t.Fatalf("last writer must win: %v", got)
	}
}

// WT-07, WT-09: Jira's refusals reach the person as they are; nothing changes in the mirror; nothing is retried.
func TestWT07_JiraRefusalsPassedThrough(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	var rj *domain.JiraRejected
	// permission
	r.jm.Accounts["admin@example.com"].CannotEdit = true
	_, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")})
	if !errors.As(err, &rj) || rj.Status != 403 || len(rj.Messages) == 0 || !strings.Contains(rj.Messages[0], "permission") {
		t.Fatalf("permission refusal: %v", err)
	}
	r.jm.Accounts["admin@example.com"].CannotEdit = false
	if r.issueOf(t, "WEB-1").Priority != "medium" {
		t.Fatal("the mirror changed on a refusal")
	}
	// a validator, with Jira's text by field
	r.jm.FailNext("PUT /rest/api/3/issue/WEB-1", 400, `{"errorMessages":[],"errors":{"priority":"Priority is required by the workflow."}}`, 1)
	_, err = r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")})
	if !errors.As(err, &rj) || rj.Fields["priority"] != "Priority is required by the workflow." || rj.Status != 400 {
		t.Fatalf("validator refusal: %v", err)
	}
	// a 409 is passed on and not repeated
	before := r.puts()
	r.jm.FailNext("PUT /rest/api/3/issue/WEB-1", 409, `{"errorMessages":["Issue was updated by someone else."]}`, 1)
	_, err = r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")})
	if !errors.As(err, &rj) || rj.Status != 409 || r.puts() != before+1 {
		t.Fatalf("409: %v, puts %d", err, r.puts()-before)
	}
	if r.auditCount(t, "rejected") != 3 {
		t.Fatalf("rejected writes audited: %d", r.auditCount(t, "rejected"))
	}
}

// WT-13: rate limit, outage and a slow Jira leave the mirror alone and say when to retry.
func TestWT13_JiraUnavailable(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	var ju *domain.JiraUnavailable
	r.jm.FailRate("PUT /rest/api/3/issue/WEB-1", 7)
	before := r.puts()
	_, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")})
	if !errors.As(err, &ju) || ju.RetryAfter < 6*time.Second || r.puts() != before+1 {
		t.Fatalf("rate limit: %v (writes %d)", err, r.puts()-before)
	}
	r.jm.FailNext("PUT /rest/api/3/issue/WEB-1", 503, ``, 1)
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")}); !errors.As(err, &ju) {
		t.Fatalf("outage: %v", err)
	}
	old := domain.JiraWriteTimeout
	domain.JiraWriteTimeout = 150 * time.Millisecond
	defer func() { domain.JiraWriteTimeout = old }()
	r.jm.SlowWrites(600 * time.Millisecond)
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")}); !errors.As(err, &ju) {
		t.Fatalf("a slow Jira: %v", err)
	}
	if r.issueOf(t, "WEB-1").Priority != "medium" {
		t.Fatal("the mirror changed although every save failed")
	}
	if r.auditCount(t, "failed") != 3 {
		t.Fatalf("failed writes audited: %d", r.auditCount(t, "failed"))
	}
}

// WT-09: a move goes through the transition Jira offers; Jira's no is passed on; a status changed in Jira is a conflict.
func TestWT09_Transitions(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	v, _ := r.e.Svc.View(ctx, r.e.Admin, "WEB-1")
	move := func(v domain.IssueView, to string) string {
		for _, o := range v.Transitions {
			if o.To == to {
				return o.ID
			}
		}
		return ""
	}
	// the mirror has seen "To Do" only: its own status is the only one offered until the sync learns the others, and Jira offers
	// "In Progress" next. Let the sync see it by moving another issue there in Jira.
	r.jm.Change("WEB-2", "status", jiramock.M{"name": "In Progress", "statusCategory": jiramock.M{"key": "indeterminate"}}, other)
	r.issue(3, "Already done", "Done", "done", "Task", nil)
	r.syncAs(t)
	v, _ = r.e.Svc.View(ctx, r.e.Admin, "WEB-1")
	id := move(v, "In Progress")
	if id == "" {
		t.Fatalf("a write-through project must offer a move to every status it has: %+v", v.Transitions)
	}
	out, err := r.e.Svc.Transition(ctx, r.e.Admin, "WEB-1", id, nil)
	if err != nil || out.Status != "In Progress" {
		t.Fatalf("transition: %q %v", out.Status, err)
	}
	if got := fmt.Sprint(r.jm.Field("WEB-1", "status")); !strings.Contains(got, "In Progress") {
		t.Fatalf("Jira has %v", got)
	}
	// Jira offers no move from In Progress to To Do: its answer is passed on
	v, _ = r.e.Svc.View(ctx, r.e.Admin, "WEB-1")
	back := move(v, "To Do")
	var rj *domain.JiraRejected
	if _, err := r.e.Svc.Transition(ctx, r.e.Admin, "WEB-1", back, nil); !errors.As(err, &rj) || !strings.Contains(rj.Error(), "no move") {
		t.Fatalf("a move Jira does not offer: %v", err)
	}
	// someone moved the issue in Jira meanwhile: the person's view of the status is stale
	r.jm.Change("WEB-1", "status", jiramock.M{"name": "Done", "statusCategory": jiramock.M{"key": "done"}}, other)
	var jc *domain.JiraConflict
	if _, err := r.e.Svc.Transition(ctx, r.e.Admin, "WEB-1", move(v, "Done"), nil); !errors.As(err, &jc) || jc.Fields[0].Field != "status" || jc.Fields[0].Jira != "Done" {
		t.Fatalf("a stale status: %v", err)
	}
	posts := 0
	for _, w := range r.jm.Writes() {
		if strings.HasSuffix(w.Path, "/transitions") {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("%d transition posts: refused and conflicting moves must not be sent", posts)
	}
	// a 409 for two moves at once is passed on and not repeated
	r.jm.Change("WEB-1", "status", jiramock.M{"name": "In Progress", "statusCategory": jiramock.M{"key": "indeterminate"}}, other)
	r.syncAs(t)
	v, _ = r.e.Svc.View(ctx, r.e.Admin, "WEB-1")
	r.jm.FailNext("POST /rest/api/3/issue/WEB-1/transitions", 409, `{"errorMessages":["Another transition is in progress."]}`, 1)
	if _, err := r.e.Svc.Transition(ctx, r.e.Admin, "WEB-1", move(v, "Done"), nil); !errors.As(err, &rj) || rj.Status != 409 {
		t.Fatalf("409 on a transition: %v", err)
	}
}

// WT-10: what the first phase does not write keeps the typed mirror refusal with the Jira address.
func TestWT10_UnsupportedEditsAreRefused(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	var rom *domain.ReadOnlyMirror
	est := 3.0
	for name, fn := range map[string]func() error{
		"estimate": func() error {
			_, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Estimate: &est})
			return err
		},
		"custom field": func() error {
			_, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Custom: map[string]any{"x": 1}})
			return err
		},
		"internal comment": func() error {
			_, err := r.e.Svc.AddComment(ctx, r.e.Admin, "WEB-1", "secret", true)
			return err
		},
		"edit comment": func() error {
			return r.e.Svc.EditComment(ctx, r.e.Admin, "WEB-1", "00000000-0000-0000-0000-000000000000", "x")
		},
		"create": func() error {
			_, err := r.e.Svc.CreateIssue(ctx, r.e.Admin, domain.CreateIn{Project: "WEB", Type: "Task", Summary: "new"})
			return err
		},
	} {
		if err := fn(); !errors.As(err, &rom) || rom.SiteURL == "" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(r.jm.Writes()) != 0 {
		t.Fatal("a refused edit reached Jira")
	}
}

// WT-12: summary and description are written back only when nothing is lost; comments are added as the person.
func TestWT12_TextAndComments(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	// the summary
	res, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-1", "summary", "Fix the login page", nil)
	if err != nil || res.Unchanged {
		t.Fatalf("summary: %+v %v", res, err)
	}
	if r.issueOf(t, "WEB-1").Summary != "Fix the login page" || fmt.Sprint(r.jm.Field("WEB-1", "summary")) != "Fix the login page" {
		t.Fatal("summary not in both places")
	}
	// a plain description: two paragraphs in Markdown are two paragraphs in Jira
	nb := "Alpha\n\nGamma"
	if _, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-1", "description", nb, nil); err != nil {
		t.Fatal(err)
	}
	if got := r.bodyOf(t, "WEB-1", "description"); got != nb {
		t.Fatalf("description in the mirror: %q", got)
	}
	if paras := adfNodeTypes(r.jm.Field("WEB-1", "description"))["paragraph"]; paras != 2 {
		t.Fatalf("Jira has %d paragraphs", paras)
	}
	before := len(r.jm.Writes())
	// the text is compared with Jira's current text: someone edited it in Jira
	r.jm.Change("WEB-1", "summary", "Changed in Jira", other)
	var tc *domain.TextConflict
	if _, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-1", "summary", "Mine", nil); !errors.As(err, &tc) || tc.Current != "Changed in Jira" || tc.By != "Other Person" || tc.Mine != "Mine" {
		t.Fatalf("text conflict: %+v %v", tc, err)
	}
	before = len(r.jm.Writes())
	if got := fmt.Sprint(r.jm.Field("WEB-1", "summary")); got != "Changed in Jira" {
		t.Fatalf("Jira's text was overwritten by a conflicting save: %q", got)
	}
	// "keep mine", chosen after the dialog showed Jira's text, goes ahead
	if _, err := r.e.Svc.SaveText(ctx, r.e.Admin, "WEB-1", domain.SaveIn{Field: "summary", BaseSeq: tc.CurrentSeq, Body: "Mine", Mode: "keep_mine"}); err != nil {
		t.Fatalf("keep mine: %v", err)
	}
	if got := fmt.Sprint(r.jm.Field("WEB-1", "summary")); got != "Mine" || len(r.jm.Writes()) != before+1 {
		t.Fatalf("keep mine did not land: %q", got)
	}
	// a comment is added in Jira as the person and shows in the mirror
	c, err := r.e.Svc.AddComment(ctx, r.e.Admin, "WEB-1", "looks fine\nto me", false)
	if err != nil || c.ID == "" {
		t.Fatalf("comment: %+v %v", c, err)
	}
	ws := r.jm.Writes()
	last := ws[len(ws)-1]
	if last.Method != "POST" || !strings.HasSuffix(last.Path, "/comment") || last.Email != "admin@example.com" {
		t.Fatalf("comment write: %+v", last)
	}
	v, _ := r.e.Svc.View(ctx, r.e.Admin, "WEB-1")
	found := false
	for _, cm := range v.Comments {
		found = found || cm.Body == "looks fine\nto me"
	}
	if !found {
		t.Fatalf("the comment is not in the mirror: %+v", v.Comments)
	}
}

// WT-11: what the sync reads back later is already there: no duplicate history, no second copy of a comment.
func TestWT11_EchoIsIdempotent(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Priority: wtStr("high")}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Svc.AddComment(ctx, r.e.Admin, "WEB-1", "once", false); err != nil {
		t.Fatal(err)
	}
	count := func() (int, int) {
		var h, c int
		r.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE i.key='WEB-1' AND h.source='Jira'`).Scan(&h)
		r.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM comment c JOIN issue i ON i.id=c.issue_id WHERE i.key='WEB-1'`).Scan(&c)
		return h, c
	}
	h0, c0 := count()
	if h0 == 0 {
		t.Fatal("the write left no history row from Jira's changelog")
	}
	for i := 0; i < 2; i++ {
		r.syncAs(t)
		if _, err := r.e.Svc.SyncJiraProject(ctx, r.e.Admin, "WEB"); err != nil {
			t.Fatal(err)
		}
	}
	if h1, c1 := count(); h1 != h0 || c1 != c0 {
		t.Fatalf("the echo duplicated rows: history %d -> %d, comments %d -> %d", h0, h1, c0, c1)
	}
	// the history row names the person who wrote, as Jira recorded it
	var who string
	r.e.Store.Pool.QueryRow(ctx, `SELECT actor_label FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE i.key='WEB-1' AND h.field='priority' AND h.source='Jira' ORDER BY at DESC LIMIT 1`).Scan(&who)
	if who != "Admin" {
		t.Fatalf("history author %q", who)
	}
}

// Assignee: Tasklane's person is sent as their Jira account; a person without a linked account is refused.
func TestWT10_Assignee(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	var inv *domain.InvalidError
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Assignee: wtStr(r.e.Alice.ID)}); !errors.As(err, &inv) {
		t.Fatalf("an assignee without a Jira account: %v", err)
	}
	r.e.Store.Pool.Exec(ctx, `UPDATE app_user SET jira_account_id='acc-alice' WHERE id::text=$1`, r.e.Alice.ID)
	r.jm.Users["acc-alice"] = jiramock.M{"accountId": "acc-alice", "displayName": "Alice K.", "emailAddress": "alice@example.com"}
	out, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, "WEB-1", nil, false, domain.Changes{Assignee: wtStr(r.e.Alice.ID)})
	if err != nil || out.Assignee == nil || out.Assignee.ID != r.e.Alice.ID {
		t.Fatalf("assignee: %+v %v", out.Assignee, err)
	}
	if got := fmt.Sprint(r.jm.Field("WEB-1", "assignee")); !strings.Contains(got, "acc-alice") {
		t.Fatalf("Jira has %v", got)
	}
}

// WT-19: the link to where a token is created depends on the Jira site.
func TestWT19_TokenPageDependsOnTheSite(t *testing.T) {
	for site, want := range map[string]string{
		"https://acme.atlassian.net":         "https://id.atlassian.com/manage-profile/security/api-tokens",
		"https://ACME.Atlassian.NET/":        "https://id.atlassian.com/manage-profile/security/api-tokens",
		"https://jira.atlassian.com":         "https://id.atlassian.com/manage-profile/security/api-tokens",
		"https://acme.jira.com":              "https://id.atlassian.com/manage-profile/security/api-tokens",
		"https://jira.example.org":           "https://jira.example.org/secure/ViewProfile.jspa",
		"https://jira.example.org/":          "https://jira.example.org/secure/ViewProfile.jspa",
		"https://evilatlassian.net":          "https://evilatlassian.net/secure/ViewProfile.jspa", // a look-alike host is not Atlassian's
		"https://atlassian.net.evil.example": "https://atlassian.net.evil.example/secure/ViewProfile.jspa",
		"http://127.0.0.1:9999":              "http://127.0.0.1:9999/secure/ViewProfile.jspa",
	} {
		if got := domain.JiraTokenPageFor(site); got != want {
			t.Errorf("%s: got %s, want %s", site, got, want)
		}
	}
}

// WT-20: a person is told which connections need their token, whether this node has one, and only the projects they can see.
func TestWT20_TokenStatusForThePerson(t *testing.T) {
	r := wtRig(t)
	st, err := r.e.Svc.JiraTokenStatus(ctx, r.e.Admin)
	if err != nil || len(st) != 1 || st[0].ConnectionID != r.conn.ID || st[0].HasToken || len(st[0].Projects) != 1 || st[0].Projects[0] != "WEB" || st[0].TokenPage != domain.JiraTokenPageFor(r.jm.URL) {
		t.Fatalf("before a token: %+v %v", st, err)
	}
	r.giveToken(t)
	if st, _ = r.e.Svc.JiraTokenStatus(ctx, r.e.Admin); !st[0].HasToken {
		t.Fatal("a token this node holds must be reported")
	}
	r.e.Svc.ClearJiraSessionToken(r.e.Admin, r.conn.ID)
	if st, _ = r.e.Svc.JiraTokenStatus(ctx, r.e.Admin); st[0].HasToken {
		t.Fatal("a forgotten token must not be reported")
	}
	// a person with no access to the project is not told it exists
	if st, err = r.e.Svc.JiraTokenStatus(ctx, r.e.Alice); err != nil || len(st) != 0 {
		t.Fatalf("a person who cannot see the project: %+v %v", st, err)
	}
	// and a mirror that only reads needs nobody's token
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "WEB", "read_only"); err != nil {
		t.Fatal(err)
	}
	if st, _ = r.e.Svc.JiraTokenStatus(ctx, r.e.Admin); len(st) != 0 {
		t.Fatalf("a read-only project needs no token: %+v", st)
	}
	if _, err := r.e.Svc.JiraTokenStatus(ctx, domain.Actor{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("anonymous: %v", err)
	}
}

func findNode(v any, typ string) any {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == typ {
			return x
		}
		if c, ok := x["content"].([]any); ok {
			for _, e := range c {
				if n := findNode(e, typ); n != nil {
					return n
				}
			}
		}
	}
	return nil
}

func asJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// WT-23: a description with formatting is edited as Markdown made from Jira's own document, and what is saved keeps the structure.
func TestWT23_DescriptionIsEditedAsMarkdown(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	body := r.bodyOf(t, "WEB-2", "description")
	for _, want := range []string{"## Plan", "**this**", "[the spec](https://example.com/spec)", "@[Ann](user:acc-ann)", "- one", "- two", "```go", "fmt.Println(1)", "```adf-opaque"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the Markdown lacks %q:\n%s", want, body)
		}
	}
	// edit one line: everything else must reach Jira as it was
	before := r.jm.Field("WEB-2", "description")
	if _, err := r.e.Svc.PatchText(ctx, r.e.Admin, "WEB-2", "description", "- two", "- two and three"); err != nil {
		t.Fatal(err)
	}
	after := r.jm.Field("WEB-2", "description")
	b, a := adfNodeTypes(before), adfNodeTypes(after)
	for _, k := range []string{"doc", "heading", "paragraph", "bulletList", "listItem", "codeBlock", "panel", "mention", "mark:strong", "mark:link"} {
		if b[k] == 0 || b[k] != a[k] {
			t.Errorf("%s: %d before, %d after", k, b[k], a[k])
		}
	}
	if asJSON(findNode(before, "panel")) != asJSON(findNode(after, "panel")) {
		t.Errorf("the panel, which the model does not hold, changed:\n%s\n%s", asJSON(findNode(before, "panel")), asJSON(findNode(after, "panel")))
	}
	if m := findNode(after, "mention").(map[string]any)["attrs"].(map[string]any); m["id"] != "acc-ann" {
		t.Errorf("the mention lost its person: %v", m)
	}
	if !strings.Contains(asJSON(after), "two and three") {
		t.Error("the edit did not reach Jira")
	}
	if got := r.bodyOf(t, "WEB-2", "description"); !strings.Contains(got, "- two and three") || !strings.Contains(got, "## Plan") {
		t.Fatalf("the mirror after the save:\n%s", got)
	}

	// someone changes the description in Jira: the person is told, with Jira's text as Markdown
	r.jm.Change("WEB-2", "description", jiramock.ADFParagraph("Rewritten in Jira"), other)
	var tc *domain.TextConflict
	if _, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-2", "description", "Mine", nil); !errors.As(err, &tc) || tc.Current != "Rewritten in Jira" || tc.By != "Other Person" {
		t.Fatalf("conflict: %+v %v", tc, err)
	}
	// the sync brings a change made in Jira in as Markdown
	r.jm.Change("WEB-2", "description", jiramock.M{"type": "doc", "version": 1, "content": []any{
		jiramock.M{"type": "heading", "attrs": jiramock.M{"level": float64(2)}, "content": []any{jiramock.M{"type": "text", "text": "Hello"}}},
		jiramock.M{"type": "paragraph", "content": []any{jiramock.M{"type": "text", "text": "it is "}, jiramock.M{"type": "text", "text": "bold", "marks": []any{jiramock.M{"type": "strong"}}}}}}}, other)
	r.syncAs(t)
	const md = "## Hello\n\nit is **bold**"
	if got := r.bodyOf(t, "WEB-2", "description"); got != md {
		t.Fatalf("the sync must store Markdown for a project that writes through: %q", got)
	}

	// emptying a description clears it in Jira
	if _, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-1", "description", "", nil); err != nil {
		t.Fatal(err)
	}
	if v := r.jm.Field("WEB-1", "description"); v != nil {
		t.Fatalf("Jira has %v", v)
	}
	if got := r.bodyOf(t, "WEB-1", "description"); got != "" {
		t.Fatalf("the mirror has %q", got)
	}

	// back to read only: the stored descriptions are plain text again
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "WEB", "read_only"); err != nil {
		t.Fatal(err)
	}
	if got := r.bodyOf(t, "WEB-2", "description"); strings.Contains(got, "##") || strings.Contains(got, "**") || !strings.Contains(got, "Hello") || !strings.Contains(got, "bold") {
		t.Fatalf("read only must keep plain text: %q", got)
	}
	// and on again: Markdown
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "WEB", "write_through"); err != nil {
		t.Fatal(err)
	}
	if got := r.bodyOf(t, "WEB-2", "description"); got != md {
		t.Fatalf("write through must keep Markdown: %q", got)
	}
}

// WT-23: a link with a scheme that is not http, https or mailto is refused, and the dialect's own schemes are kept only as nodes.
func TestWT23_UnsafeLinksAreRefusedAndMentionsKept(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	before := len(r.jm.Writes())
	var rj *domain.JiraRejected
	// `user:` outside a mention (no @) stays a link with that scheme: not something Jira should be sent
	if _, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-1", "description", "see [x](user:abc)", nil); !errors.As(err, &rj) {
		t.Fatalf("a link with another scheme: %v", err)
	}
	if len(r.jm.Writes()) != before {
		t.Fatal("a refused edit was sent")
	}
	// a script link is cleaned to its label by the cleaning every text goes through, so what reaches Jira is text
	if _, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-1", "description", "click [here](javascript:alert(1))", nil); err != nil {
		t.Fatal(err)
	}
	if got := asJSON(r.jm.Field("WEB-1", "description")); strings.Contains(got, "javascript") || !strings.Contains(got, "click here") {
		t.Fatalf("Jira got %s", got)
	}
	// a mention written as a mention is a mention
	if _, err := r.e.Svc.ReplaceText(ctx, r.e.Admin, "WEB-1", "description", "ask @[Ann](user:acc-ann) and see ABC", nil); err != nil {
		t.Fatal(err)
	}
	if n := adfNodeTypes(r.jm.Field("WEB-1", "description"))["mention"]; n != 1 {
		t.Fatalf("%d mentions in Jira", n)
	}
}

// WT-15: the mode is an administrator's choice.
func TestWT15_ModeIsAdministratorsOnly(t *testing.T) {
	r := wtRig(t)
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Alice, "WEB", "read_only"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a non-administrator changed the mode: %v", err)
	}
	var inv *domain.InvalidError
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "WEB", "sideways"); !errors.As(err, &inv) {
		t.Fatalf("an unknown mode: %v", err)
	}
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "NOPE", "read_only"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unknown project: %v", err)
	}
	if v, _ := r.e.Svc.View(ctx, r.e.Admin, "WEB-1"); v.Mirror == nil || v.Mirror.Mode != "write_through" {
		t.Fatalf("the page must be told the mode: %+v", v.Mirror)
	}
}

// A native project and a read-only mirror are unaffected: the writes they refused or allowed before are as they were.
func TestWT_NativeAndReadOnlyUnchanged(t *testing.T) {
	r := wtRig(t)
	r.giveToken(t)
	if err := r.e.Svc.SetJiraProjectMode(ctx, r.e.Admin, "WEB", "read_only"); err != nil {
		t.Fatal(err)
	}
	var rom *domain.ReadOnlyMirror
	if _, err := r.e.Svc.AddComment(ctx, r.e.Admin, "WEB-1", "x", false); !errors.As(err, &rom) {
		t.Fatalf("read_only must refuse: %v", err)
	}
	is := r.e.Issue("native", "")
	if _, err := r.e.Svc.UpdateFields(ctx, r.e.Admin, is.Key, nil, false, domain.Changes{Priority: wtStr("high")}); err != nil {
		t.Fatalf("a native project must stay writable: %v", err)
	}
	if len(r.jm.Writes()) != 0 {
		t.Fatal("something reached Jira")
	}
}
