package domain_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestSEC07_AuditLogIsAppendOnlyAndChained(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	pool := e.Store.Pool
	for i := 0; i < 5; i++ {
		if err := e.Svc.AuditAPI(ctx, e.Admin, "test.action", "t", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := e.Svc.VerifyAudit(ctx, e.Admin)
	if err != nil || !c.OK || c.Entries < 5 {
		t.Fatalf("%+v %v", c, err)
	}
	// the database itself refuses to change or remove a row, or to empty the table
	for _, q := range []string{
		`UPDATE audit_event SET action='x'`,
		`DELETE FROM audit_event`,
		`TRUNCATE audit_event`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", q, err)
		}
	}
	// someone with the rights to switch the triggers off still cannot hide a change: the chain notices
	tamper := func(change string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `ALTER TABLE audit_event DISABLE TRIGGER USER`); err != nil {
			t.Skip("cannot disable triggers here:", err)
		}
		if _, err := pool.Exec(ctx, change); err != nil {
			t.Fatal(err)
		}
		pool.Exec(ctx, `ALTER TABLE audit_event ENABLE TRIGGER USER`)
	}
	tamper(`UPDATE audit_event SET detail='{"i": 99}' WHERE id=(SELECT id FROM audit_event WHERE action='test.action' ORDER BY id OFFSET 2 LIMIT 1)`)
	c, _ = e.Svc.VerifyAudit(ctx, e.Admin)
	if c.OK || c.BrokenAt == nil || c.Problem != "this entry was changed" {
		t.Fatalf("an altered entry was not detected: %+v", c)
	}
	tamper(`UPDATE audit_event SET detail='{"i": 2}' WHERE detail='{"i": 99}'`)
	if c, _ = e.Svc.VerifyAudit(ctx, e.Admin); !c.OK {
		t.Fatalf("restoring the entry should restore the chain: %+v", c)
	}
	tamper(`DELETE FROM audit_event WHERE id=(SELECT id FROM audit_event WHERE action='test.action' ORDER BY id OFFSET 1 LIMIT 1)`)
	c, _ = e.Svc.VerifyAudit(ctx, e.Admin)
	if c.OK || c.BrokenAt == nil || !strings.Contains(c.Problem, "missing") {
		t.Fatalf("a removed entry was not detected: %+v", c)
	}
	if _, err := e.Svc.VerifyAudit(ctx, e.Alice); err == nil {
		t.Fatal("a member verified the log")
	}
}

func TestSEC07_ConcurrentWritersKeepOneChain(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Svc.AuditAPI(ctx, e.Admin, "burst", "t", map[string]any{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c, err := e.Svc.VerifyAudit(ctx, e.Admin)
	if err != nil || !c.OK {
		t.Fatalf("%+v %v", c, err)
	}
	var distinct, n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(DISTINCT seq), count(*) FROM audit_event WHERE action='burst'`).Scan(&distinct, &n)
	if distinct != 40 || n != 40 {
		t.Fatalf("%d distinct of %d", distinct, n)
	}
}

func TestSEC07_QueryFilterPageAndExport(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Svc.AuditAPI(ctx, e.Admin, "workflow.publish", "wf1", map[string]any{"v": 1})
	e.Svc.AuditAPI(ctx, e.Admin, "workflow.create", "wf2", map[string]any{})
	e.Svc.AuditAPI(ctx, e.Alice, "issue.move", "ABC-1", map[string]any{})
	e.Svc.AuditAPI(ctx, e.Admin, `=HYPERLINK("http://evil")`, "x", map[string]any{})
	q := func(f func(*domain.AuditQuery)) []domain.AuditEntry {
		var aq domain.AuditQuery
		f(&aq)
		out, err := e.Svc.QueryAudit(ctx, e.Admin, aq)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := q(func(a *domain.AuditQuery) { a.Action = "workflow." }); len(got) != 2 {
		t.Fatalf("prefix: %d", len(got))
	}
	if got := q(func(a *domain.AuditQuery) { a.Action = "workflow.publish" }); len(got) != 1 {
		t.Fatalf("exact: %d", len(got))
	}
	if got := q(func(a *domain.AuditQuery) { a.Actor = "Alice" }); len(got) != 1 || got[0].Action != "issue.move" {
		t.Fatalf("actor: %+v", got)
	}
	if got := q(func(a *domain.AuditQuery) { a.Target = "ABC-1" }); len(got) != 1 {
		t.Fatalf("target: %d", len(got))
	}
	if got := q(func(a *domain.AuditQuery) { a.Action = "%" }); len(got) != 0 {
		t.Fatalf("a wildcard matched %d entries", len(got))
	}
	if got := q(func(a *domain.AuditQuery) { a.From = time.Now().Add(time.Hour) }); len(got) != 0 {
		t.Fatalf("future: %d", len(got))
	}
	all := q(func(a *domain.AuditQuery) {})
	page1 := q(func(a *domain.AuditQuery) { a.Limit = 2 })
	page2 := q(func(a *domain.AuditQuery) { a.Limit = 2; a.Before = page1[1].ID })
	if len(page1) != 2 || len(page2) == 0 || page2[0].ID >= page1[1].ID || page1[0].ID != all[0].ID {
		t.Fatalf("paging: %v %v", page1, page2)
	}
	if _, err := e.Svc.QueryAudit(ctx, e.Alice, domain.AuditQuery{}); err == nil {
		t.Fatal("a member read the log")
	}
	// the export is safe to open in a spreadsheet and is itself recorded
	var buf bytes.Buffer
	n, err := e.Svc.ExportAudit(ctx, e.Admin, &buf, domain.AuditQuery{})
	if err != nil || n < 4 {
		t.Fatalf("%d %v", n, err)
	}
	if strings.Contains(buf.String(), ",=HYPERLINK") || !strings.Contains(buf.String(), "'=HYPERLINK") {
		t.Fatalf("formula not neutralized:\n%s", buf.String())
	}
	if got := q(func(a *domain.AuditQuery) { a.Action = "audit.export" }); len(got) != 1 {
		t.Fatalf("the export was not recorded: %d", len(got))
	}
}
