package domain_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

type privacyRig struct {
	*testutil.Env
	bobEmail, bobName    string
	bobIssue, aliceIssue string
}

// newPrivacy gives Bob a footprint in every place the system keeps a person.
func newPrivacy(t *testing.T) *privacyRig {
	e := testutil.New(t)
	e.Project()
	e.WithBlobs()
	e.Clock.T = time.Now()
	r := &privacyRig{Env: e, bobEmail: "bob@example.com", bobName: "Bob R."}
	bi, err := e.Svc.CreateIssue(ctx, e.Bob, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "Bob R. reports a thing", Description: "reported by bob@example.com", Assignee: e.Alice.ID, Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	ai, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "Alice's issue", Description: "ask @bob or write to bob@example.com about it"})
	if err != nil {
		t.Fatal(err)
	}
	r.bobIssue, r.aliceIssue = bi.Key, ai.Key
	hi := "high"
	e.Svc.UpdateFields(ctx, e.Bob, ai.Key, nil, false, domain.Changes{Priority: &hi})
	e.Svc.AddComment(ctx, e.Bob, ai.Key, "my own comment, from bob@example.com", false)
	e.Svc.AddComment(ctx, e.Alice, ai.Key, "thanks @bob, Bob R. - noted", false)
	e.Svc.AttachStream(ctx, e.Bob, ai.Key, "bobs-notes.txt", "text/plain", strings.NewReader("bob's file"))
	e.Svc.SetWatch(ctx, e.Bob, ai.Key, true)
	e.Svc.SetNotifyPrefs(ctx, e.Bob, domain.NotifyPrefs{AutoWatch: true, Inbox: true, Email: true, Muted: []string{"edit"}})
	e.Svc.NotifyOnce(ctx)
	e.Svc.AddComment(ctx, e.Alice, ai.Key, "another one for the watcher", false)
	e.Svc.NotifyOnce(ctx)
	e.Svc.CreateOwnToken(ctx, e.Bob, "ci", time.Hour)
	sess, _ := e.Svc.LoginWith(ctx, "bob@example.com", "password123", time.Hour, domain.ClientMeta{IP: "203.0.113.77", UserAgent: "Bob's browser"})
	e.Svc.LoginWith(ctx, "bob@example.com", "wrong", time.Hour, domain.ClientMeta{IP: "203.0.113.77"})
	su, _ := e.Svc.SetupMFA(ctx, e.Bob)
	if _, err := e.Svc.EnableMFA(ctx, e.Bob, totp(t, su.Secret, e.Clock.T), sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.Pool.Exec(ctx, `INSERT INTO mail_message(idempotency_key,recipient_id,recipient,subject,message_id,in_reply_to,refs,body_text,body_html,kind) VALUES('x:1',$1,'Bob R. <bob@example.com>','Re: thing','<m1@x>','','','Hello Bob R.','','event')`, e.Bob.ID); err != nil {
		t.Fatal(err)
	}
	e.Store.Pool.Exec(ctx, `UPDATE app_user SET external_id='okta-bob-1' WHERE email='bob@example.com'`)
	e.Svc.AuditAPI(ctx, e.Bob, "something.bob.did", ai.Key, map[string]any{"email": "bob@example.com", "name": "Bob R.", "ip": "203.0.113.77", "kept": "detail"})
	return r
}

// every text and JSON column of every table, searched for what must be gone
func (r *privacyRig) traces(t *testing.T, needles ...string) []string {
	t.Helper()
	rows, err := r.Store.Pool.Query(ctx, `SELECT table_name, column_name, data_type FROM information_schema.columns WHERE table_schema=current_schema() AND data_type IN ('text','character varying','jsonb','json','ARRAY') ORDER BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ tbl, col, typ string }
	var cols []col
	for rows.Next() {
		var c col
		rows.Scan(&c.tbl, &c.col, &c.typ)
		cols = append(cols, c)
	}
	rows.Close()
	var found []string
	for _, c := range cols {
		for _, n := range needles {
			var cnt int
			q := fmt.Sprintf(`SELECT count(*) FROM %q WHERE %q::text ILIKE '%%' || $1 || '%%'`, c.tbl, c.col)
			if err := r.Store.Pool.QueryRow(ctx, q, n).Scan(&cnt); err != nil {
				t.Fatalf("%s.%s: %v", c.tbl, c.col, err)
			}
			if cnt > 0 {
				found = append(found, fmt.Sprintf("%s.%s has %d row(s) with %q", c.tbl, c.col, cnt, n))
			}
		}
	}
	return found
}

func TestSEC10_ExportCoversEverythingHeldAboutAPerson(t *testing.T) {
	r := newPrivacy(t)
	e := r.Env
	d, err := e.Svc.ExportPersonalData(ctx, e.Admin, e.Bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	prof := d["profile"].(map[string]any)
	if prof["email"] != "bob@example.com" || prof["external_id"] == nil {
		t.Fatalf("%v", prof)
	}
	for _, k := range []string{"projects", "issues_reported", "comments", "attachments", "history", "notifications", "watching", "sessions", "api_tokens", "mail_sent_to_them", "audit"} {
		if l, ok := d[k].([]map[string]any); !ok || len(l) == 0 {
			t.Errorf("%s is empty: %v", k, d[k])
		}
	}
	if d["two_factor_enabled"] != true || d["notification_preferences"] == nil {
		t.Fatalf("%v", d)
	}
	// it holds Bob's data and nobody else's
	for _, c := range d["comments"].([]map[string]any) {
		if strings.Contains(fmt.Sprint(c["body"]), "thanks @bob") {
			t.Fatal("another person's comment is in Bob's export")
		}
	}
	// only administrators, and the export is itself recorded
	if _, err := e.Svc.ExportPersonalData(ctx, e.Alice, e.Bob.ID); err == nil {
		t.Fatal("a member exported someone's data")
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='privacy.export'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d export records", n)
	}
	if _, err := e.Svc.ExportPersonalData(ctx, e.Admin, "00000000-0000-0000-0000-000000000000"); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
}

func TestSEC10_ErasingAPersonLeavesNoTraceAndKeepsTheWork(t *testing.T) {
	r := newPrivacy(t)
	e := r.Env
	before := r.traces(t, "bob@example.com")
	if len(before) < 8 {
		t.Fatalf("the fixture left too few traces to prove anything: %v", before)
	}
	res, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{ScrubMentions: true})
	if err != nil || !res.Erased {
		t.Fatalf("%+v %v", res, err)
	}
	if left := r.traces(t, "bob@example.com", "Bob R.", "okta-bob-1", "203.0.113.77", "Bob's browser"); len(left) > 0 {
		t.Fatalf("traces remain:\n%s\n(counts %v)", strings.Join(left, "\n"), res.Counts)
	}
	// the person is gone, the work is not
	var name, email string
	var active bool
	e.Store.Pool.QueryRow(ctx, `SELECT name,email,active FROM app_user WHERE id::text=$1`, e.Bob.ID).Scan(&name, &email, &active)
	if name != "Deleted user" || !strings.HasSuffix(email, "@erased.invalid") || active {
		t.Fatalf("%q %q %v", name, email, active)
	}
	if _, err := e.Svc.LoginWith(ctx, email, "password123", time.Hour, domain.ClientMeta{}); err == nil {
		t.Fatal("an erased person signed in")
	}
	v, err := e.Svc.View(ctx, e.Admin, r.bobIssue)
	if err != nil || v.Issue.Reporter.Name != "Deleted user" || !strings.Contains(v.Issue.Summary, "[deleted user]") || strings.Contains(v.Issue.Description, "bob@") {
		t.Fatalf("%+v %v", v.Issue, err)
	}
	av, _ := e.Svc.View(ctx, e.Admin, r.aliceIssue)
	var bodies []string
	for _, c := range av.Comments {
		bodies = append(bodies, c.Author.Name+": "+c.Body)
	}
	all := strings.Join(bodies, "\n")
	if !strings.Contains(all, "Deleted user: my own comment, from [deleted user]") || !strings.Contains(all, "thanks [deleted user], [deleted user] - noted") {
		t.Fatalf("comments:\n%s", all)
	}
	if strings.Contains(av.Issue.Description, "bob") || !strings.Contains(av.Issue.Description, "ask [deleted user] or write to [deleted user] about it") {
		t.Fatalf("%q", av.Issue.Description)
	}
	h, _ := e.Svc.History(ctx, e.Admin, r.aliceIssue)
	foundDeleted := false
	for _, x := range h {
		if x.Actor == "Deleted user" {
			foundDeleted = true
		}
		if strings.Contains(x.Actor, "Bob") {
			t.Fatalf("history still names Bob: %+v", x)
		}
	}
	if !foundDeleted {
		t.Fatal("history lost the fact that someone made the change")
	}
	// credentials, sessions and tokens are gone
	var cnt int
	for _, q := range []string{`SELECT count(*) FROM session WHERE user_id::text=$1`, `SELECT count(*) FROM api_token WHERE user_id::text=$1`, `SELECT count(*) FROM user_mfa WHERE user_id::text=$1`,
		`SELECT count(*) FROM notification WHERE user_id::text=$1`, `SELECT count(*) FROM issue_watch WHERE user_id::text=$1`, `SELECT count(*) FROM project_member WHERE user_id::text=$1`} {
		e.Store.Pool.QueryRow(ctx, q, e.Bob.ID).Scan(&cnt)
		if cnt != 0 {
			t.Errorf("%s left %d", q, cnt)
		}
	}
	// the audit log was redacted and still verifies; the erase itself is recorded without the person's details
	c, err := e.Svc.VerifyAudit(ctx, e.Admin)
	if err != nil || !c.OK {
		t.Fatalf("the audit chain broke: %+v %v", c, err)
	}
	var detail string
	e.Store.Pool.QueryRow(ctx, `SELECT detail::text FROM audit_event WHERE action='something.bob.did'`).Scan(&detail)
	if strings.Contains(detail, "bob@example.com") || strings.Contains(detail, "Bob R.") || !strings.Contains(detail, "kept") {
		t.Fatalf("%s", detail)
	}
	var erasedRows int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='privacy.erased' AND actor_label='Admin'`).Scan(&erasedRows)
	if erasedRows != 1 {
		t.Fatalf("%d", erasedRows)
	}
	if res.Counts["audit_entries_redacted"] == 0 || res.Counts["issues_reindexed"] == 0 {
		t.Fatalf("%v", res.Counts)
	}
	// the search index follows: events were queued for the issues whose text changed
	var ev int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE type='issue.updated' AND payload->>'reason'='erase'`).Scan(&ev)
	if ev == 0 {
		t.Fatal("no reindex events")
	}
	// the other issues' watchers, assignees and plain structure are untouched
	if av.Issue.Key != r.aliceIssue || av.Issue.Assignee != nil && av.Issue.Assignee.ID == e.Bob.ID {
		t.Fatalf("%+v", av.Issue)
	}
	// doing it again does nothing
	again, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{ScrubMentions: true})
	if err != nil || again.Erased {
		t.Fatalf("%+v %v", again, err)
	}
	// and the hash chain is still good after another write
	e.Svc.AuditAPI(ctx, e.Admin, "after.erase", "", map[string]any{})
	if c, _ := e.Svc.VerifyAudit(ctx, e.Admin); !c.OK {
		t.Fatalf("%+v", c)
	}
}

func TestSEC10_DeleteContentRemovesWhatThePersonWrote(t *testing.T) {
	r := newPrivacy(t)
	e := r.Env
	res, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{DeleteContent: true, ScrubMentions: true})
	if err != nil || res.Counts["comments_deleted"] != 1 || res.Counts["attachments_deleted"] != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	av, _ := e.Svc.View(ctx, e.Admin, r.aliceIssue)
	for _, c := range av.Comments {
		if strings.Contains(c.Body, "my own comment") {
			t.Fatal("a comment the person wrote survived")
		}
	}
	if len(av.Attachments) != 0 {
		t.Fatal("an attachment the person uploaded survived")
	}
	var refs int
	e.Store.Pool.QueryRow(ctx, `SELECT COALESCE(sum(refcount),0) FROM blob`).Scan(&refs)
	if refs != 0 {
		t.Fatalf("the file kept %d reference(s)", refs)
	}
	if left := r.traces(t, "bobs-notes", "bob@example.com", "Bob R."); len(left) > 0 {
		t.Fatalf("traces remain after deleting content:\n%s", strings.Join(left, "\n"))
	}
	// the issue they reported is still there, under the pseudonym
	if v, err := e.Svc.View(ctx, e.Admin, r.bobIssue); err != nil || v.Issue.Reporter.Name != "Deleted user" {
		t.Fatalf("%v", err)
	}
}

func TestSEC10_LegalHoldAndGuardsStopAnErase(t *testing.T) {
	r := newPrivacy(t)
	e := r.Env
	if err := e.Svc.SetLegalHold(ctx, e.Admin, r.aliceIssue, true); err != nil {
		t.Fatal(err)
	}
	_, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{DeleteContent: true})
	if err == nil || !strings.Contains(err.Error(), r.aliceIssue) || !strings.Contains(err.Error(), "legal hold") {
		t.Fatalf("%v", err)
	}
	var email string
	e.Store.Pool.QueryRow(ctx, `SELECT email FROM app_user WHERE id::text=$1`, e.Bob.ID).Scan(&email)
	if email != "bob@example.com" {
		t.Fatal("a refused erase changed the account")
	}
	if err := e.Svc.SetLegalHold(ctx, e.Admin, r.aliceIssue, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ErasePerson(ctx, e.Admin, e.Admin.ID, domain.EraseOpts{}); err == nil {
		t.Fatal("an administrator erased themselves")
	}
	if _, err := e.Svc.ErasePerson(ctx, e.Alice, e.Bob.ID, domain.EraseOpts{}); err == nil {
		t.Fatal("a member erased someone")
	}
	if _, err := e.Svc.ErasePerson(ctx, e.Admin, "00000000-0000-0000-0000-000000000000", domain.EraseOpts{}); err != domain.ErrNotFound {
		t.Fatalf("%v", err)
	}
	// the last administrator cannot be erased; with another one around, an administrator can
	two := e.NewUser("second-admin@example.com", "Second Admin", true)
	e.Store.Pool.Exec(ctx, `UPDATE app_user SET is_admin=false WHERE email='second-admin@example.com'`)
	if _, err := e.Svc.ErasePerson(ctx, two, e.Admin.ID, domain.EraseOpts{}); err == nil || !strings.Contains(err.Error(), "last administrator") {
		t.Fatalf("%v", err)
	}
	e.Store.Pool.Exec(ctx, `UPDATE app_user SET is_admin=true WHERE email='second-admin@example.com'`)
	if res, err := e.Svc.ErasePerson(ctx, two, e.Admin.ID, domain.EraseOpts{}); err != nil || !res.Erased {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestSEC10_CleanExpiredRemovesWhatOutlivedItsPurpose(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	old := e.Clock.T.Add(-200 * 24 * time.Hour)
	e.Svc.LoginWith(ctx, "alice@example.com", "password123", time.Hour, domain.ClientMeta{})
	e.Store.Pool.Exec(ctx, `UPDATE session SET expires_at=$1`, e.Clock.T.Add(-time.Hour))
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "n", Assignee: e.Bob.ID})
	e.Svc.NotifyOnce(ctx)
	e.Store.Pool.Exec(ctx, `UPDATE notification SET state='read',created_at=$1`, old)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "fresh", false)
	e.Svc.NotifyOnce(ctx)
	e.Store.Pool.Exec(ctx, `INSERT INTO login_attempt(email,ok,at) VALUES('x@example.com',false,$1)`, e.Clock.T.Add(-48*time.Hour))
	out, err := e.Svc.CleanExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out["sessions"] != 1 || out["notifications"] != 1 || out["sign_in_records"] != 1 {
		t.Fatalf("%v", out)
	}
	// what is current stays
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM notification WHERE state='unread'`).Scan(&n)
	if n == 0 {
		t.Fatal("a recent notification was removed")
	}
}
