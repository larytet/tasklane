package domain_test

import (
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/mail"
	"github.com/larytet/tasklane/internal/testutil/smtpmock"
)

func TestMAIL19_MirroredProjectsGetNoTasklaneEmailButKeepTheInboxAndRegainEmailAfterCutOver(t *testing.T) {
	r := newJira(t)
	e := r.e
	m := smtpmock.New(t, smtpmock.Options{TLS: "starttls", User: "u", Pass: "p"})
	e.Svc.Mail = &domain.MailConfig{
		Mailer: &mail.SMTP{Host: "localhost", Port: m.Port(), TLS: "starttls", User: "u", Pass: "p", From: "Tasklane <tasklane@example.com>", TLSConfig: &tls.Config{RootCAs: m.CA()}, Timeout: 3 * time.Second},
		From:   "Tasklane <tasklane@example.com>", Domain: "tasklane.example.com", BaseURL: "https://tasklane.example.com",
	}
	r.mirror(t)
	r.issue(1, "Mirrored thing", "To Do", "new", "Task", nil)
	r.syncAs(t)
	// Bob can see the project and watches the issue; the permission sync is fresh
	e.Store.Pool.Exec(ctx, `UPDATE jira_project SET perm_synced_at=now()`)
	e.Store.Pool.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) SELECT p.id,$1,'member' FROM project p WHERE p.key='WEB'`, e.Bob.ID)
	e.Store.Pool.Exec(ctx, `INSERT INTO issue_watch(issue_id,user_id) SELECT id,$1 FROM issue WHERE key='WEB-1'`, e.Bob.ID)
	event := func() {
		e.Store.Pool.Exec(ctx, `INSERT INTO outbox(aggregate,type,payload) VALUES('issue:WEB-1','issue.transitioned','{"key":"WEB-1","actor":"Jira","from":"To Do","to":"Done"}')`)
		if _, err := e.Svc.NotifyOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Svc.SendMailOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var inbox, queued int
	count := func() {
		e.Store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM notification WHERE user_id=$1),(SELECT count(*) FROM mail_message WHERE recipient_id=$1)`, e.Bob.ID).Scan(&inbox, &queued)
	}
	event()
	count()
	if inbox != 1 || queued != 0 {
		t.Fatalf("while mirrored: %d inbox notice(s), %d mail(s); want 1 and 0", inbox, queued)
	}
	// the Admin tab says so, in words
	st, err := e.Svc.JiraStatus(ctx, e.Admin)
	if err != nil || len(st) != 1 || !strings.Contains(st[0].Mail, "sends no email") {
		t.Fatalf("%+v %v", st, err)
	}
	// after cut-over the project is native and email flows like anywhere else
	if err := e.Svc.CutOverJira(ctx, e.Admin, "WEB"); err != nil {
		t.Fatal(err)
	}
	e.Store.Pool.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) SELECT p.id,$1,'member' FROM project p WHERE p.key='WEB' ON CONFLICT DO NOTHING`, e.Bob.ID)
	event()
	count()
	if inbox != 2 || queued != 1 {
		t.Fatalf("after cut-over: %d inbox notice(s), %d mail(s); want 2 and 1", inbox, queued)
	}
}

func TestMAIL19_MailToAMirroredProjectIsHeldAndNothingIsSentBack(t *testing.T) {
	e := newInbound(t)
	r := e.Store.Pool
	// a mirrored project ABC-like: mark the project mirrored (the sync is covered by the Jira tests)
	var conn string
	if err := r.QueryRow(ctx, `INSERT INTO jira_connection(name,site_url,email,token_enc,webhook_secret_enc) VALUES('x','https://x.example','a@b.c','\x00','\x00') RETURNING id::text`).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	r.Exec(ctx, `UPDATE project SET mirror_of=$1 WHERE key='ABC'`, conn)
	r.Exec(ctx, `INSERT INTO jira_project(project_id,connection_id,jira_key,perm_synced_at) SELECT id,$1,'ABC',now() FROM project WHERE key='ABC'`, conn)
	before := len(e.smtp.Messages())
	// a reply to the issue, and a mail to the project's address: both held with the reason, neither answered
	got := e.ingest(true, e.eml("Bob R. <bob@example.com>", "reply@tasklane.example.com", "Re: [ABC-1] Printer is on fire", []string{authOK}, "Please fix."))
	if got.Outcome != "quarantined" || got.Reason != "mirrored project" {
		t.Fatalf("%+v", got)
	}
	got = e.ingest(true, e.eml("Bob R. <bob@example.com>", "abc@tasklane.example.com", "A new problem", []string{authOK}, "It broke."))
	if got.Outcome != "quarantined" && got.Outcome != "ignored" {
		t.Fatalf("%+v", got)
	}
	if len(e.comments()) != 0 {
		t.Fatal("a comment was written on a mirrored issue")
	}
	// releasing it is refused: a mirrored issue takes no writes from anywhere
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, got.ID); err == nil {
		t.Fatal("a held message to a mirrored project was released")
	}
	if n := len(e.smtp.Messages()); n != before {
		t.Fatalf("%d message(s) were sent back", n-before)
	}
}
