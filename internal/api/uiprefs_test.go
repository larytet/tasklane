package api_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// PREF-01: every allowed key round-trips; unknown keys, wrong types and values out of range are refused naming the key and
// store nothing; a person who never saved gets the defaults.
func TestPREF01_UISettingsRoundTripAndValidation(t *testing.T) {
	_, alice, _, _ := setup(t)
	code, out, hdr := alice.do("GET", "/api/v1/me/ui-prefs", nil)
	if code != 200 || out["version"].(float64) != 0 || hdr.Get("ETag") != `"0"` {
		t.Fatalf("defaults: %d %v %v", code, out, hdr)
	}
	def := out["prefs"].(map[string]any)
	if def["theme"] != "" || def["density"] != "default" || def["font_scale"].(float64) != 100 || def["shortcuts"] != true || def["reduced_motion"] != "system" {
		t.Fatalf("default values: %v", def)
	}
	all := map[string]any{"theme": "light", "density": "comfortable", "list_view": "long", "columns": []string{"key", "summary", "status", "reporter", "labels"},
		"sort": map[string]any{"field": "priority", "dir": "desc"}, "font_scale": 130, "reduced_motion": "on", "high_contrast": true, "shortcuts": false,
		"hints_seen": []string{"create", "settings"}}
	code, out, hdr = alice.do("PUT", "/api/v1/me/ui-prefs", all)
	if code != 200 || out["version"].(float64) != 1 || hdr.Get("ETag") != `"1"` {
		t.Fatalf("save all: %d %v", code, out)
	}
	_, out, _ = alice.do("GET", "/api/v1/me/ui-prefs", nil)
	p := out["prefs"].(map[string]any)
	for k, want := range map[string]any{"theme": "light", "density": "comfortable", "list_view": "long", "font_scale": 130.0, "reduced_motion": "on", "high_contrast": true, "shortcuts": false} {
		if p[k] != want {
			t.Errorf("%s: got %v want %v", k, p[k], want)
		}
	}
	if s := p["sort"].(map[string]any); s["field"] != "priority" || s["dir"] != "desc" {
		t.Errorf("sort: %v", s)
	}
	if c := p["columns"].([]any); len(c) != 5 || c[0] != "key" || c[4] != "labels" {
		t.Errorf("columns: %v", c)
	}
	if h := p["hints_seen"].([]any); len(h) != 2 {
		t.Errorf("hints: %v", h)
	}
	// a partial save keeps the rest
	alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"density": "compact"})
	_, out, _ = alice.do("GET", "/api/v1/me/ui-prefs", nil)
	if p := out["prefs"].(map[string]any); p["density"] != "compact" || p["theme"] != "light" || out["version"].(float64) != 2 {
		t.Fatalf("partial save: %v", out)
	}
	// key and summary cannot be hidden
	alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"columns": []string{"status"}})
	_, out, _ = alice.do("GET", "/api/v1/me/ui-prefs", nil)
	if c := out["prefs"].(map[string]any)["columns"].([]any); len(c) != 3 || c[1] != "key" || c[2] != "summary" {
		t.Fatalf("key and summary kept: %v", c)
	}
	before := out["version"].(float64)
	for _, bad := range []struct {
		body any
		key  string
	}{
		{map[string]any{"colour": "red"}, "colour"},
		{map[string]any{"density": "tiny"}, "density"},
		{map[string]any{"density": 3}, "density"},
		{map[string]any{"font_scale": 120}, "font_scale"},
		{map[string]any{"font_scale": "100"}, "font_scale"},
		{map[string]any{"theme": nil}, "theme"},
		{map[string]any{"columns": []string{"key", "nonsense"}}, "columns"},
		{map[string]any{"columns": []string{"customfield_999999"}}, "columns"},
		{map[string]any{"columns": "key"}, "columns"},
		{map[string]any{"sort": map[string]any{"field": "labels"}}, "sort"},
		{map[string]any{"sort": map[string]any{"field": "key", "dir": "up"}}, "sort"},
		{map[string]any{"sort": map[string]any{"field": "key", "extra": 1}}, "sort"},
		{map[string]any{"shortcuts": "no"}, "shortcuts"},
		{map[string]any{"hints_seen": []string{"everything"}}, "hints_seen"},
		{map[string]any{"reduced_motion": true}, "reduced_motion"},
		{map[string]any{"density": "compact", "high_contrast": 1}, "high_contrast"}, // one bad key refuses the whole save
		{[]string{"density"}, ""},
	} {
		code, out, _ := alice.do("PUT", "/api/v1/me/ui-prefs", bad.body)
		if code != 400 {
			t.Errorf("%v: want 400, got %d %v", bad.body, code, out)
			continue
		}
		if d, _ := out["detail"].(map[string]any); d == nil || d["key"] != bad.key {
			t.Errorf("%v: detail should name %q: %v", bad.body, bad.key, out)
		}
	}
	_, out, _ = alice.do("GET", "/api/v1/me/ui-prefs", nil)
	if out["version"].(float64) != before || out["prefs"].(map[string]any)["density"] != "compact" {
		t.Fatalf("a refused save stored something: %v", out)
	}
}

// PREF-01: a custom field can be a column and a sort; when it is trashed it drops out instead of failing the person's list.
func TestPREF01_CustomFieldColumnsDropOutWhenTrashed(t *testing.T) {
	e, alice, _, _ := setup(t)
	f, err := e.Svc.CreateField(testutil.Ctx(), e.Admin, domain.FieldIn{Name: "Story points", Type: "number"})
	if err != nil {
		t.Fatal(err)
	}
	col := "customfield_" + strconv.FormatInt(f.ID, 10)
	if code, out, _ := alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"columns": []string{"key", "summary", col}, "sort": map[string]any{"field": col, "dir": "desc"}}); code != 200 {
		t.Fatalf("custom column: %d %v", code, out)
	}
	if err := e.Svc.TrashField(testutil.Ctx(), e.Admin, f.ID); err != nil {
		t.Fatal(err)
	}
	_, out, _ := alice.do("GET", "/api/v1/me/ui-prefs", nil)
	p := out["prefs"].(map[string]any)
	if c := p["columns"].([]any); len(c) != 2 {
		t.Fatalf("trashed column still listed: %v", c)
	}
	if s := p["sort"].(map[string]any); s["field"] != "" {
		t.Fatalf("trashed sort kept: %v", s)
	}
}

// PREF-02: two tabs save with If-Match; the second gets 412 with the current settings and nothing is lost.
func TestPREF02_SecondTabGets412WithTheCurrentSettings(t *testing.T) {
	_, alice, _, _ := setup(t)
	_, out, _ := alice.do("GET", "/api/v1/me/ui-prefs", nil)
	v := strconv.Itoa(int(out["version"].(float64)))
	if code, out, _ := alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"theme": "light"}, "If-Match", `"`+v+`"`); code != 200 {
		t.Fatalf("first tab: %d %v", code, out)
	}
	code, out, hdr := alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"density": "compact"}, "If-Match", `"`+v+`"`)
	if code != 412 || hdr.Get("ETag") != `"1"` {
		t.Fatalf("second tab: want 412, got %d %v", code, out)
	}
	cur := out["detail"].(map[string]any)
	if cur["version"].(float64) != 1 || cur["prefs"].(map[string]any)["theme"] != "light" {
		t.Fatalf("412 should carry the current settings: %v", cur)
	}
	// the client merges its own change onto the current copy and saves again
	if code, _, _ := alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"density": "compact"}, "If-Match", `"1"`); code != 200 {
		t.Fatalf("merged save: %d", code)
	}
	_, out, _ = alice.do("GET", "/api/v1/me/ui-prefs", nil)
	if p := out["prefs"].(map[string]any); p["theme"] != "light" || p["density"] != "compact" {
		t.Fatalf("a setting was lost: %v", p)
	}
	if code, _, _ := alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"density": "compact"}, "If-Match", "soon"); code != 400 {
		t.Fatalf("garbage If-Match: %d", code)
	}
}

// PREF-03, PREF-04: settings belong to the person, not the browser or the session: another sign-in of the same person sees them,
// another person never does and cannot reach them (the route has no person in it); they are in the person's export and gone after
// erasure.
func TestPREF03_04_SettingsFollowThePersonAndNobodyElse(t *testing.T) {
	e, alice, bob, _ := setup(t)
	alice.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"theme": "light", "font_scale": 115})
	second := &client{t: t, base: alice.base}
	if code, _, _ := second.do("POST", "/api/v1/login", map[string]string{"email": "alice@example.com", "password": "password123"}); code != 200 {
		t.Fatal("second sign-in")
	}
	tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Alice.ID, "second device")
	second.token = tok
	_, out, _ := second.do("GET", "/api/v1/me/ui-prefs", nil)
	if p := out["prefs"].(map[string]any); p["theme"] != "light" || p["font_scale"].(float64) != 115 {
		t.Fatalf("second device: %v", p)
	}
	_, out, _ = second.do("GET", "/api/v1/bootstrap", nil)
	if p := out["ui_prefs"].(map[string]any)["prefs"].(map[string]any); p["theme"] != "light" {
		t.Fatalf("bootstrap carries the settings: %v", out["ui_prefs"])
	}
	_, out, _ = bob.do("GET", "/api/v1/me/ui-prefs", nil)
	if p := out["prefs"].(map[string]any); p["theme"] != "" || out["version"].(float64) != 0 {
		t.Fatalf("bob sees alice's settings: %v", out)
	}
	bob.do("PUT", "/api/v1/me/ui-prefs", map[string]any{"density": "compact"})
	_, out, _ = alice.do("GET", "/api/v1/me/ui-prefs", nil)
	if out["prefs"].(map[string]any)["density"] != "default" {
		t.Fatalf("bob's save reached alice: %v", out)
	}
	anon := &client{t: t, base: alice.base}
	if code, _, _ := anon.do("GET", "/api/v1/me/ui-prefs", nil); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	d, err := e.Svc.ExportPersonalData(testutil.Ctx(), e.Admin, e.Alice.ID)
	if err != nil || !strings.Contains(d["ui_settings"].(string), `"light"`) {
		t.Fatalf("export: %v %v", err, d["ui_settings"])
	}
	if _, err := e.Svc.ErasePerson(testutil.Ctx(), e.Admin, e.Alice.ID, domain.EraseOpts{}); err != nil {
		t.Fatal(err)
	}
	var n int
	e.Svc.DB.QueryRow(testutil.Ctx(), `SELECT count(*) FROM user_setting WHERE user_id=$1`, e.Alice.ID).Scan(&n)
	if n != 0 {
		t.Fatal("settings survived erasure")
	}
}
