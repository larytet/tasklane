package api_test

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func listKeys(out map[string]any) []string {
	var ks []string
	for _, x := range out["issues"].([]any) {
		ks = append(ks, x.(map[string]any)["key"].(string))
	}
	return ks
}

// LIST-04: fields= answers only the columns asked for (the key always); an unknown column is refused.
func TestLIST04_FieldsGiveOnlyTheColumnsAskedFor(t *testing.T) {
	_, alice, _, key := setup(t)
	code, out, _ := alice.do("GET", "/api/v1/issues?q="+url.QueryEscape("key = "+key)+"&fields=summary,status,reporter,created", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	row := out["issues"].([]any)[0].(map[string]any)
	want := map[string]bool{"key": true, "summary": true, "status": true, "status_category": true, "reporter": true, "created": true}
	for k := range row {
		if !want[k] {
			t.Errorf("column %q was not asked for", k)
		}
	}
	for k := range want {
		if _, ok := row[k]; !ok {
			t.Errorf("column %q is missing", k)
		}
	}
	if row["reporter"] != "Alice K." {
		t.Fatalf("reporter: %v", row["reporter"])
	}
	for _, bad := range []string{"password_hash", "customfield_999999", "summary,../../etc"} {
		if code, out, _ := alice.do("GET", "/api/v1/issues?q=&fields="+url.QueryEscape(bad), nil); code != 400 || out["detail"].(map[string]any)["key"] != "fields" {
			t.Errorf("%s: want 400 naming fields, got %d %v", bad, code, out)
		}
	}
	// the full row without fields=, as before
	_, out, _ = alice.do("GET", "/api/v1/issues?q="+url.QueryEscape("key = "+key), nil)
	if _, ok := out["issues"].([]any)[0].(map[string]any)["comments"]; !ok {
		t.Fatal("the full row lost a column")
	}
}

// LIST-02: a custom field as a column; a person whose role may not see it never gets its values, and a trashed field is refused.
func TestLIST02_CustomColumnsFollowFieldVisibility(t *testing.T) {
	e, alice, _, key := setup(t)
	ctx := testutil.Ctx()
	f, err := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Budget", Type: "number", ViewRoles: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddContext(ctx, e.Admin, f.ID, domain.ContextIn{Global: true}); err != nil {
		t.Fatal(err)
	}
	open, err := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Points", Type: "number"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddContext(ctx, e.Admin, open.ID, domain.ContextIn{Global: true}); err != nil {
		t.Fatal(err)
	}
	k2, err := e.Svc.CreateIssue(ctx, e.Admin, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "with values", Custom: map[string]any{f.Key: 1000, open.Key: 5}})
	if err != nil {
		t.Fatal(err)
	}
	_ = key
	cols := url.QueryEscape(f.Key + "," + open.Key)
	code, out, _ := alice.do("GET", "/api/v1/issues?q="+url.QueryEscape("key = "+k2.Key)+"&fields="+cols, nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	row := out["issues"].([]any)[0].(map[string]any)
	if _, ok := row[f.Key]; ok {
		t.Fatalf("a member got the value of an admin-only field: %v", row)
	}
	if fmt.Sprint(row[open.Key]) != "5" {
		t.Fatalf("an open field's value is missing: %v", row)
	}
	admin := &client{t: t, base: alice.base}
	admin.token, _ = e.Svc.CreateAPIToken(ctx, e.Admin.ID, "admin")
	_, out, _ = admin.do("GET", "/api/v1/issues?q="+url.QueryEscape("key = "+k2.Key)+"&fields="+cols, nil)
	if fmt.Sprint(out["issues"].([]any)[0].(map[string]any)[f.Key]) != "1000" {
		t.Fatalf("the administrator should see it: %v", out)
	}
	if err := e.Svc.TrashField(ctx, e.Admin, open.ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := alice.do("GET", "/api/v1/issues?q=&fields="+url.QueryEscape(open.Key), nil); code != 400 {
		t.Fatalf("a trashed field is still a column: %d", code)
	}
}

// LIST-03: sort by each sortable column in both directions, with a text query and a TQL query; the order is the database's and
// ties are broken by key, so a page boundary never repeats or skips an issue. A query's own ORDER BY wins.
func TestLIST03_SortByEachColumn(t *testing.T) {
	e, alice, _, _ := setup(t)
	ctx := testutil.Ctx()
	prios := []string{"high", "low", "medium", "high", "lowest", "high"}
	for i, p := range prios {
		if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: fmt.Sprintf("sortme %c", 'f'-rune(i)), Priority: p}); err != nil {
			t.Fatal(err)
		}
	}
	rank := map[string]int{"lowest": 0, "low": 1, "medium": 2, "high": 3, "highest": 4}
	for _, field := range []string{"key", "summary", "priority", "status", "updated", "created", "assignee", "reporter", "type"} {
		for _, dir := range []string{"asc", "desc"} {
			code, out, _ := alice.do("GET", "/api/v1/issues?limit=100&q="+url.QueryEscape("project = ABC")+"&sort="+field+":"+dir+"&fields=summary,priority", nil)
			if code != 200 {
				t.Fatalf("%s %s: %d %v", field, dir, code, out)
			}
			rows := out["issues"].([]any)
			if len(rows) != 7 {
				t.Fatalf("%s %s: %d rows", field, dir, len(rows))
			}
			if field == "priority" {
				ok := sort.SliceIsSorted(rows, func(i, j int) bool {
					a, b := rank[rows[i].(map[string]any)["priority"].(string)], rank[rows[j].(map[string]any)["priority"].(string)]
					if dir == "desc" {
						return a > b
					}
					return a < b
				})
				if !ok {
					t.Fatalf("priority %s not in order: %v", dir, rows)
				}
				// ties (three highs) by key, ascending
				var highs []int
				for _, r := range rows {
					if m := r.(map[string]any); m["priority"] == "high" {
						n, _ := strconv.Atoi(strings.TrimPrefix(m["key"].(string), "ABC-"))
						highs = append(highs, n)
					}
				}
				if !sort.IntsAreSorted(highs) {
					t.Fatalf("ties not broken by key: %v", highs)
				}
			}
		}
	}
	// paging over a sort with ties neither repeats nor skips
	var seen []string
	for off := 0; off < 7; off += 2 {
		_, out, _ := alice.do("GET", fmt.Sprintf("/api/v1/issues?limit=2&offset=%d&q=%s&sort=priority:desc", off, url.QueryEscape("project = ABC")), nil)
		seen = append(seen, listKeys(out)...)
	}
	uniq := map[string]bool{}
	for _, k := range seen {
		uniq[k] = true
	}
	if len(seen) != 7 || len(uniq) != 7 {
		t.Fatalf("paging repeated or skipped: %v", seen)
	}
	// with the plain text box
	if code, out, _ := alice.do("GET", "/api/v1/issues?syntax=text&q=sortme&sort=summary:asc", nil); code != 200 {
		t.Fatalf("text query with sort: %d %v", code, out)
	}
	// the query's own order wins over the person's sort
	_, out, _ := alice.do("GET", "/api/v1/issues?q="+url.QueryEscape("project = ABC ORDER BY key DESC")+"&sort=key:asc", nil)
	ks := listKeys(out)
	if ks[0] != "ABC-7" {
		t.Fatalf("the query's ORDER BY should win: %v", ks)
	}
	for _, bad := range []string{"labels:asc", "key:up", "nonsense"} {
		if code, _, _ := alice.do("GET", "/api/v1/issues?q=&sort="+bad, nil); code != 400 {
			t.Errorf("%s: want 400", bad)
		}
	}
}
