package domain_test

import (
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
)

func (e *inboundEnv) rawSha(id int64) string {
	var sha *string
	e.Store.Pool.QueryRow(ctx, `SELECT raw_sha256 FROM mail_inbound WHERE id=$1`, id).Scan(&sha)
	if sha == nil {
		return ""
	}
	return *sha
}

func (e *inboundEnv) hasBlob(sha string) bool {
	if sha == "" {
		return false
	}
	o, err := e.Svc.OpenBlob(sha)
	if err != nil {
		return false
	}
	o.Close()
	return true
}

func TestMAIL18_RawMessagesArePurgedAfterTheRetentionAndTheRowKeepsRecognisingRedeliveries(t *testing.T) {
	e := newInbound(t)
	e.Svc.MailRetentionSet, e.Svc.MailRawDays = true, 30
	addr := e.replyAddr("bob@example.com")
	acc := e.eml("Bob R. <bob@example.com>", addr, "Re: [ABC-1] Printer is on fire", []string{authOK}, "Thanks, I will look today."+quoted)
	ra := e.ingest(true, acc)
	if ra.Outcome != "accepted" {
		t.Fatalf("%+v", ra)
	}
	// unproven mail waits for a decision
	q := e.ingest(false, e.eml("Bob R. <bob@example.com>", addr, "Re: [ABC-1] Printer is on fire", nil, "Please look at this one."+quoted))
	if q.Outcome != "quarantined" {
		t.Fatalf("%+v", q)
	}
	shaA, shaQ := e.rawSha(ra.ID), e.rawSha(q.ID)
	if !e.hasBlob(shaA) || !e.hasBlob(shaQ) {
		t.Fatal("originals not stored")
	}
	// nothing is purged before the retention ends
	e.Clock.Advance(29 * 24 * time.Hour)
	r, err := e.Svc.RunMailRetention(ctx)
	if err != nil || r.Messages != 0 || !e.hasBlob(shaA) {
		t.Fatalf("%+v %v", r, err)
	}
	e.Clock.Advance(2 * 24 * time.Hour)
	if _, err := e.Svc.RunMailRetentionNow(ctx, e.Alice); err == nil {
		t.Fatal("a member ran retention")
	}
	r, err = e.Svc.RunMailRetentionNow(ctx, e.Admin)
	if err != nil || r.Messages != 1 || r.Blobs != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if e.hasBlob(shaA) {
		t.Fatal("the original of the accepted message is still in the blob store")
	}
	var sender, subject, body string
	var purged *time.Time
	e.Store.Pool.QueryRow(ctx, `SELECT sender,subject,body,purged_at FROM mail_inbound WHERE id=$1`, ra.ID).Scan(&sender, &subject, &body, &purged)
	if sender != "" || subject != "" || body != "" || purged == nil {
		t.Fatalf("%q %q %q %v", sender, subject, body, purged)
	}
	if _, err := e.Svc.InboundRaw(ctx, e.Admin, ra.ID); err == nil {
		t.Fatal("a purged original was served")
	}
	// the comment it made stays (that is the issue's own history), and a redelivery is still a duplicate, not new work
	if len(e.comments()) != 1 {
		t.Fatalf("%d comments", len(e.comments()))
	}
	if d := e.ingest(true, acc); !d.Duplicate {
		t.Fatalf("a redelivery after the purge was processed again: %+v", d)
	}
	// the quarantined message is still there, with its original, because someone has yet to decide
	if !e.hasBlob(shaQ) {
		t.Fatal("a message waiting for a decision was purged")
	}
	if _, err := e.Svc.ReleaseInbound(ctx, e.Admin, q.ID); err != nil {
		t.Fatalf("it can no longer be released: %v", err)
	}
	// the run is audited with counts and without content
	var n int
	var detail string
	e.Store.Pool.QueryRow(ctx, `SELECT count(*),COALESCE(max(detail::text),'') FROM audit_event WHERE action='mail.retention'`).Scan(&n, &detail)
	if n != 1 || detail == "" {
		t.Fatalf("%d %s", n, detail)
	}
	// once decided, the released message is ordinary old mail and the next run purges it; after that, repeating changes nothing
	if r, _ := e.Svc.RunMailRetention(ctx); r.Messages != 1 {
		t.Fatalf("%+v", r)
	}
	if r, _ := e.Svc.RunMailRetention(ctx); r.Messages != 0 {
		t.Fatalf("%+v", r)
	}
	// zero keeps everything
	e.Svc.MailRawDays = 0
	e.Clock.Advance(3650 * 24 * time.Hour)
	if r, _ := e.Svc.RunMailRetention(ctx); r.Messages != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestMAIL18_LegalHoldKeepsTheMailOfAHeldIssue(t *testing.T) {
	e := newInbound(t)
	e.Svc.MailRetentionSet, e.Svc.MailRawDays = true, 30
	addr := e.replyAddr("bob@example.com")
	r := e.ingest(true, e.eml("Bob R. <bob@example.com>", addr, "Re: [ABC-1] Printer is on fire", []string{authOK}, "Evidence."+quoted))
	e.Store.Pool.Exec(ctx, `UPDATE issue SET legal_hold=true WHERE key=$1`, e.issue.Key)
	e.Clock.Advance(60 * 24 * time.Hour)
	if res, _ := e.Svc.RunMailRetention(ctx); res.Messages != 0 || !e.hasBlob(e.rawSha(r.ID)) {
		t.Fatalf("mail on a held issue was purged: %+v", res)
	}
	e.Store.Pool.Exec(ctx, `UPDATE issue SET legal_hold=false WHERE key=$1`, e.issue.Key)
	if res, _ := e.Svc.RunMailRetention(ctx); res.Messages != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestMAIL18_BounceRecordsAndClearedSuppressionsExpireButActiveOnesNeverDo(t *testing.T) {
	e := newInbound(t)
	e.Svc.MailRetentionSet, e.Svc.MailBounceDays, e.Svc.MailRawDays = true, 100, 0
	now := e.Clock.Now()
	e.Store.Pool.Exec(ctx, `INSERT INTO mail_bounce(email,kind,at) VALUES('old@example.org','hard',$1),('live@example.org','hard',$1),('new@example.org','soft',$2)`, now.AddDate(0, 0, -200), now.AddDate(0, 0, -10))
	e.Store.Pool.Exec(ctx, `INSERT INTO mail_suppression(email,kind,since,cleared_at) VALUES('old@example.org','hard',$1,$2)`, now.AddDate(0, 0, -300), now.AddDate(0, 0, -150))
	e.Store.Pool.Exec(ctx, `INSERT INTO mail_suppression(email,kind,since) VALUES('live@example.org','hard',$1)`, now.AddDate(0, 0, -300))
	r, err := e.Svc.RunMailRetention(ctx)
	if err != nil || r.Bounces != 1 || r.Suppression != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	var live, bounces int
	e.Store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM mail_suppression WHERE email='live@example.org' AND cleared_at IS NULL),(SELECT count(*) FROM mail_bounce)`).Scan(&live, &bounces)
	if live != 1 || bounces != 2 {
		t.Fatalf("a suppression still in force was removed by age, or the wrong bounce records went: %d %d", live, bounces)
	}
}

func TestMAIL18_ErasingAPersonRemovesTheirMailAndOriginalsAtOnceAndTheAuditStays(t *testing.T) {
	e := newInbound(t)
	e.Svc.MailRetentionSet, e.Svc.MailRawDays = true, 0 // retention would keep it for ever: erase does not wait
	addr := e.replyAddr("bob@example.com")
	r := e.ingest(true, e.eml("Bob R. <bob@example.com>", addr, "Re: [ABC-1] Printer is on fire", []string{authOK}, "My phone number is 555-0100."+quoted))
	sha := e.rawSha(r.ID)
	e.Svc.CreateIssue(ctx, e.Bob, domain.CreateIn{Project: "ABC", Summary: "For Alice", Assignee: e.Alice.ID})
	e.flush()
	other := e.ingest(true, e.eml("Alice K. <alice@example.com>", e.replyAddr("alice@example.com"), "Re: [ABC-1] Printer is on fire", []string{authOK}, "Alice's own reply."+quoted))
	e.Store.Pool.Exec(ctx, `INSERT INTO mail_bounce(email,kind) VALUES('bob@example.com','soft')`)
	e.Store.Pool.Exec(ctx, `INSERT INTO mail_suppression(email,kind) VALUES('bob@example.com','soft')`)
	exp, err := e.Svc.ExportPersonalData(ctx, e.Admin, e.Bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := exp["inbound_mail"].([]map[string]any); len(got) != 1 {
		t.Fatalf("the export lacks the inbound mail: %+v", exp["inbound_mail"])
	}
	if got, _ := exp["bounce_records"].([]map[string]any); len(got) != 1 {
		t.Fatalf("the export lacks the bounce records: %+v", exp["bounce_records"])
	}
	res, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts["inbound_mail_purged"] != 1 || res.Counts["originals_removed"] != 1 || res.Counts["bounce_records_removed"] != 1 || res.Counts["suppressions_removed"] != 1 {
		t.Fatalf("%+v", res.Counts)
	}
	if e.hasBlob(sha) {
		t.Fatal("the person's original is still in the blob store")
	}
	var left int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE sender ILIKE '%bob@example.com%' OR subject LIKE '%Printer%' AND id=$1`, r.ID).Scan(&left)
	if left != 0 {
		t.Fatalf("%d rows still name the person", left)
	}
	// someone else's mail is untouched
	if !e.hasBlob(e.rawSha(other.ID)) {
		t.Fatal("another person's original was removed")
	}
	// the erase itself stays in the audit log, with counts and no address
	var detail string
	e.Store.Pool.QueryRow(ctx, `SELECT detail::text FROM audit_event WHERE action='privacy.erased' ORDER BY id DESC LIMIT 1`).Scan(&detail)
	if detail == "" {
		t.Fatal("the erase is not audited")
	}
}

func TestMAIL18_ErasingIsRefusedWhileTheirMailIsOnAnIssueUnderLegalHold(t *testing.T) {
	e := newInbound(t)
	addr := e.replyAddr("bob@example.com")
	e.ingest(true, e.eml("Bob R. <bob@example.com>", addr, "Re: [ABC-1] Printer is on fire", []string{authOK}, "Evidence."+quoted))
	e.Store.Pool.Exec(ctx, `UPDATE issue SET legal_hold=true WHERE key=$1`, e.issue.Key)
	if _, err := e.Svc.ErasePerson(ctx, e.Admin, e.Bob.ID, domain.EraseOpts{}); err == nil {
		t.Fatal("erased a person whose mail is evidence on a held issue")
	}
}
