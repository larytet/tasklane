package testutil

import (
	"fmt"
	"os"
	"time"

	"github.com/larytet/tasklane/internal/search"
)

// LoadScale loads 100,000 issues (the PostgreSQL engine's design size), 20 projects (the first five with Alice as
// a member) and about 100,000 comments, and builds the search index. It is slow; callers gate it on TASKLANE_PERF.
func LoadScale(e *Env) {
	ctx := Ctx()
	t := e.T
	pool := e.Store.Pool
	n := 100000
	if v := os.Getenv("TASKLANE_PERF_N"); v != "" {
		fmt.Sscan(v, &n)
	}
	t0 := time.Now()
	mustExec := func(q string, a ...any) {
		if _, err := pool.Exec(ctx, q, a...); err != nil {
			t.Fatal(err)
		}
	}
	// 20 projects, Alice a member of the first 5 (so the permission pre-filter is exercised)
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("P%c", 'A'+i)
		if _, err := e.Svc.CreateProject(ctx, e.Admin, key, "Project "+key); err != nil {
			t.Fatal(err)
		}
		if i < 5 {
			e.Svc.AddMember(ctx, e.Admin, key, e.Alice.ID, "member")
		}
	}
	mustExec(`UPDATE project SET next_number = $1 + 1`, n/20)
	mustExec(`
WITH words AS (SELECT ARRAY['checkout','payment','login','export','import','report','sprint','board','backlog','search','upload','email','webhook','timeout','crash','latency','cache','index','migration','audit','permission','dashboard','filter','workflow','release','mobile','offline','theme','editor','merge'] w)
INSERT INTO issue(project_id, number, key, issue_type_id, status_id, priority, reporter_id, assignee_id, labels, rank, summary, created_at, updated_at)
SELECT p.id, g, p.key || '-' || g,
  (SELECT id FROM issue_type ORDER BY name OFFSET (g % 5) LIMIT 1),
  (SELECT id FROM status WHERE workflow_id = p.workflow_id ORDER BY position OFFSET (g % 3) LIMIT 1),
  (ARRAY['lowest','low','medium','high','highest'])[1 + g % 5], $1::uuid, CASE WHEN g % 4 = 0 THEN $1::uuid END,
  ARRAY[(SELECT w[1 + (g % 30)] FROM words)],
  lpad(g::text, 8, '0'),
  initcap((SELECT w[1 + (g * 7 % 30)] FROM words)) || ' ' || (SELECT w[1 + (g * 11 % 30)] FROM words) || ' ' || (SELECT w[1 + (g * 13 % 30)] FROM words) || ' issue number ' || g,
  now() - (g || ' minutes')::interval, now() - (g || ' minutes')::interval
FROM project p, generate_series(1, $2) g`, e.Alice.ID, n/20)
	mustExec(`INSERT INTO issue_text(issue_id, field, body)
SELECT id, 'description', 'Steps to reproduce ' || summary || '. The ' || labels[1] || ' flow fails when the user retries. Expected behavior is documented in the runbook section ' || (number % 50) FROM issue`)
	mustExec(`INSERT INTO comment(issue_id, author_id, body, internal, created_at)
SELECT i.id, $1::uuid, 'Comment ' || c || ' on ' || i.key || ': investigating ' || i.labels[1], c = 2 AND i.number % 10 = 0, now()
FROM issue i, generate_series(1, 2) c WHERE i.number % 2 = 0`, e.Alice.ID)
	t.Logf("loaded %d issues in %v", n, time.Since(t0).Round(time.Millisecond))
	t0 = time.Now()
	if err := search.ReindexAll(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Logf("indexed in %v", time.Since(t0).Round(time.Millisecond))
}
