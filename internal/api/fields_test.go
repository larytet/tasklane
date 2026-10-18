package api_test

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestHTTP_CORE03_CustomFieldsEndToEnd(t *testing.T) {
	e, alice, _, key := setup(t)
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, Search: &search.Postgres{DB: e.Store.Pool}, SessionTTL: 3600e9}).Handler())
	defer ts.Close()
	adm := &client{t: t, base: ts.URL}
	tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Admin.ID, "t")
	adm.token = tok

	code, out, _ := alice.do("POST", "/api/v1/fields", map[string]any{"name": "Nope", "type": "text"})
	if code != 403 {
		t.Fatalf("a member created a field: %d %v", code, out)
	}
	code, out, _ = adm.do("POST", "/api/v1/fields", map[string]any{"name": "Severity", "type": "select"})
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	id := int64(out["id"].(float64))
	if code, out, _ = adm.do("POST", fmt.Sprintf("/api/v1/fields/%d/contexts", id), map[string]any{"global": true, "options": []string{"Low", "High"}}); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	// the form definition lists it with its options
	code, out, _ = alice.do("GET", "/api/v1/projects/ABC/fields?type=Bug", nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// set through PATCH, rejected values are a 400
	k := fmt.Sprint("customfield_", id)
	if code, out, _ = alice.do("PATCH", "/api/v1/issues/"+key, map[string]any{"custom": map[string]any{k: "Nonsense"}}); code != 400 {
		t.Fatalf("%d %v", code, out)
	}
	if code, out, _ = alice.do("PATCH", "/api/v1/issues/"+key, map[string]any{"custom": map[string]any{k: "High"}}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	code, out, _ = alice.do("GET", "/api/v1/issues/"+key+"/view", nil)
	is, _ := out["issue"].(map[string]any)
	if is == nil {
		is = out
	}
	if c, _ := is["custom"].(map[string]any); code != 200 || c[k] != "High" {
		t.Fatalf("%d %v", code, out)
	}
	// and found by query
	code, out, _ = adm.do("GET", "/api/v1/issues?q=%22Severity%22+%3D+High", nil)
	if rows, _ := out["issues"].([]any); code != 200 || len(rows) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	// create with a custom value
	if code, out, _ = alice.do("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "with field", "custom": map[string]any{k: "Low"}}); code != 201 {
		t.Fatalf("%d %v", code, out)
	}
}
