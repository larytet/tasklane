package search_test

import (
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

func load100k(t *testing.T) *fx {
	f := newFx(t)
	testutil.LoadScale(f.Env)
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
