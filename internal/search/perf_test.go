package search_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/search"
)

// Loads 100,000 issues (the PostgreSQL engine's design size), 20 projects and 200,000 comments.
// Enabled with TASKLANE_PERF=1 because loading takes a while; run by `make perf`.
func load100k(t *testing.T) *fx {
	f := newFx(t)
	pool := f.Store.Pool
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
		if _, err := f.Svc.CreateProject(ctx, f.Admin, key, "Project "+key); err != nil {
			t.Fatal(err)
		}
		if i < 5 {
			f.Svc.AddMember(ctx, f.Admin, key, f.Alice.ID, "member")
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
FROM project p, generate_series(1, $2) g`, f.Alice.ID, n/20)
	mustExec(`INSERT INTO issue_text(issue_id, field, body)
SELECT id, 'description', 'Steps to reproduce ' || summary || '. The ' || labels[1] || ' flow fails when the user retries. Expected behavior is documented in the runbook section ' || (number % 50) FROM issue`)
	mustExec(`INSERT INTO comment(issue_id, author_id, body, internal, created_at)
SELECT i.id, $1::uuid, 'Comment ' || c || ' on ' || i.key || ': investigating ' || i.labels[1], c = 2 AND i.number % 10 = 0, now()
FROM issue i, generate_series(1, 2) c WHERE i.number % 2 = 0`, f.Alice.ID)
	t.Logf("loaded %d issues in %v", n, time.Since(t0).Round(time.Millisecond))
	t0 = time.Now()
	if err := search.ReindexAll(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Logf("indexed in %v", time.Since(t0).Round(time.Millisecond))
	return f
}

func p95(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)*95/100]
}

func TestPERF16_17_SearchLatencyAt100kIssues(t *testing.T) {
	if os.Getenv("TASKLANE_PERF") == "" {
		t.Skip("set TASKLANE_PERF=1 (make perf) to run the 100k-issue search benchmark")
	}
	f := load100k(t)
	alice, admin := f.scope(f.Alice), f.scope(f.Admin)
	type qc struct {
		name, q string
		sc      search.Scope
		budget  time.Duration
	}
	cases := []qc{
		{"text selective term, admin", `text ~ "777"`, admin, 400 * time.Millisecond},
		{"text common phrase, admin", `text ~ "runbook section 7"`, admin, 400 * time.Millisecond},
		{"text common term, member of 5 of 20 projects", `text ~ "checkout"`, alice, 400 * time.Millisecond},
		{"text and project", `text ~ "payment" AND project = PA`, alice, 400 * time.Millisecond},
		{"text and several filters", `text ~ "latency" AND status = "In Progress" AND priority = high AND assignee is EMPTY`, admin, 400 * time.Millisecond},
		{"key lookup", `key = PB-1234`, admin, 400 * time.Millisecond},
		{"key as text", `text ~ "PC-777"`, alice, 400 * time.Millisecond},
		{"comment match", `comment ~ "investigating webhook"`, alice, 400 * time.Millisecond},
		{"filter list, newest first", `project = PA AND status != Done ORDER BY updated DESC`, alice, 400 * time.Millisecond},
		{"assignee and label", `assignee = currentUser() AND labels = crash`, alice, 400 * time.Millisecond},
		{"empty query, member", ``, alice, 400 * time.Millisecond},
	}
	for _, c := range cases {
		var ds []time.Duration
		var total int
		for i := 0; i < 40; i++ {
			s := time.Now()
			res, err := f.eng.Search(ctx, c.sc, c.q, search.Page{Limit: 50})
			ds = append(ds, time.Since(s))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			total = res.Total
		}
		got := p95(ds)
		t.Logf("%-48s p95 %8v  (total %d)", c.name, got.Round(100*time.Microsecond), total)
		if got > c.budget {
			t.Errorf("%s: p95 %v exceeds %v", c.name, got, c.budget)
		}
	}
	var ds []time.Duration
	for i := 0; i < 60; i++ {
		term := []string{"chec", "PB-12", "pay", "login f", "PA-9"}[i%5]
		s := time.Now()
		if _, err := f.eng.Typeahead(ctx, alice, term, 8); err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(s))
	}
	got := p95(ds)
	t.Logf("%-48s p95 %8v", "typeahead", got.Round(100*time.Microsecond))
	if got > 150*time.Millisecond {
		t.Errorf("typeahead p95 %v exceeds 150ms", got)
	}
}

// TQL-21: plan-shape assertions. The search tables must be reached through their indexes.
func TestTQL21_PlanShapes(t *testing.T) {
	if os.Getenv("TASKLANE_PERF") == "" {
		t.Skip("set TASKLANE_PERF=1 (make perf) to run plan-shape assertions on the large dataset")
	}
	f := load100k(t)
	explain := func(sql string, args ...any) string {
		rows, err := f.Store.Pool.Query(ctx, "EXPLAIN "+sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var sb strings.Builder
		for rows.Next() {
			var l string
			rows.Scan(&l)
			sb.WriteString(l + "\n")
		}
		return sb.String()
	}
	mustNotSeqScan := func(name, plan string, tables ...string) {
		for _, tb := range tables {
			if strings.Contains(plan, "Seq Scan on "+tb) {
				t.Errorf("%s: sequential scan of %s\n%s", name, tb, plan)
			}
		}
	}
	mustUse := func(name, plan string, idx ...string) {
		for _, i := range idx {
			if !strings.Contains(plan, i) {
				t.Errorf("%s: expected index %s\n%s", name, i, plan)
			}
		}
	}
	// selective terms: a term in every row would (correctly) be answered by a sequential scan
	p := explain(`SELECT issue_id FROM issue_search WHERE tsv @@ plainto_tsquery('simple','777')`)
	mustUse("text only", p, "issue_search_tsv")
	mustNotSeqScan("text only", p, "issue_search")
	p = explain(`SELECT issue_id FROM comment_search WHERE tsv @@ plainto_tsquery('simple','pa-1234')`)
	mustUse("comment", p, "comment_search_tsv")
	mustNotSeqScan("comment", p, "comment_search")
	p = explain(`SELECT issue_id FROM issue_search WHERE summary_norm LIKE '%checkout%'`)
	mustUse("typeahead trigram", p, "issue_search_trgm")
	p = explain(`SELECT id FROM issue WHERE key = 'PA-100'`)
	mustUse("key lookup", p, "issue_key_key")
	p = explain(`SELECT id FROM issue WHERE project_id = (SELECT id FROM project WHERE key='PA') AND deleted_at IS NULL ORDER BY updated_at DESC LIMIT 50`)
	mustUse("project list", p, "issue_list_idx")
	mustNotSeqScan("project list", p, "issue")
}
