package api_test

import (
	"net/url"
	"testing"

	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestHTTP_SearchEndpoint(t *testing.T) {
	e, alice, bob, key := setup(t)
	alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "Second issue about invoices", "labels": []string{"billing"}})
	if _, err := (&search.Indexer{DB: e.Store.Pool}).RunOnce(testutil.Ctx()); err != nil {
		t.Fatal(err)
	}
	get := func(c *client, q string) (int, map[string]any) {
		code, out, _ := c.do("GET", "/api/v1/issues?q="+url.QueryEscape(q), nil)
		return code, out
	}
	code, out := get(alice, `project = ABC AND text ~ "invoices"`)
	rows, _ := out["issues"].([]any)
	if code != 200 || len(rows) != 1 || rows[0].(map[string]any)["key"] != "ABC-2" || out["total"].(float64) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out := get(bob, ``); code != 200 || out["total"].(float64) != 2 {
		t.Fatalf("%d %v", code, out)
	}
	// a malformed query is a 400 with a position, not a 500
	code, out = get(alice, `project = ABC AND (`)
	if code != 400 || out["code"] != "tql_error" || out["detail"].(map[string]any)["pos"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	code, out = get(alice, `nosuchfield = 1`)
	if code != 400 || out["code"] != "tql_error" {
		t.Fatalf("%d %v", code, out)
	}
	// typeahead
	c, _, _ := alice.do("GET", "/api/v1/search/typeahead?q=ABC-", nil)
	if c != 200 {
		t.Fatal(c)
	}
	_ = key
	// anonymous is refused
	anon := &client{t: t, base: alice.base}
	if c, _, _ := anon.do("GET", "/api/v1/issues?q=", nil); c != 401 {
		t.Fatalf("anonymous: %d", c)
	}
}
