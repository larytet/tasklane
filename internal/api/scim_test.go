package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func TestSEC04_SCIMProvisioningLifecycle(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: time.Hour}).Handler())
	defer ts.Close()
	ctx := testutil.Ctx()
	tok, _, err := e.Svc.CreateSCIMToken(ctx, e.Admin, "okta")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any, token string) (int, map[string]any, http.Header) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Content-Type", "application/scim+json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out, resp.Header
	}
	// only a SCIM token opens the door: an administrator's own token does not
	adminTok, _ := e.Svc.CreateAPIToken(ctx, e.Admin.ID, "admin")
	for _, bad := range []string{"", "nope", adminTok} {
		if code, out, _ := call("GET", "/scim/v2/Users", nil, bad); code != 401 || out["status"] != "401" {
			t.Fatalf("%q: %d %v", bad, code, out)
		}
	}
	code, cfg, hdr := call("GET", "/scim/v2/ServiceProviderConfig", nil, tok)
	if code != 200 || !strings.Contains(hdr.Get("Content-Type"), "application/scim+json") || cfg["patch"].(map[string]any)["supported"] != true {
		t.Fatalf("%d %v", code, cfg)
	}
	// create: 201 with the id, a second create of the same userName takes over the account (200, same id)
	code, u, _ := call("POST", "/scim/v2/Users", map[string]any{"userName": "New.Person@Example.com", "externalId": "ext-1", "name": map[string]any{"givenName": "New", "familyName": "Person"}, "active": true}, tok)
	if code != 201 || u["userName"] != "new.person@example.com" || u["displayName"] != "New Person" || u["externalId"] != "ext-1" || u["active"] != true {
		t.Fatalf("%d %v", code, u)
	}
	id := u["id"].(string)
	if code, again, _ := call("POST", "/scim/v2/Users", map[string]any{"userName": "new.person@example.com", "externalId": "ext-1"}, tok); code != 200 || again["id"] != id {
		t.Fatalf("%d %v", code, again)
	}
	if code, _, _ := call("POST", "/scim/v2/Users", map[string]any{"userName": "new.person@example.com", "externalId": "someone-else"}, tok); code != 409 {
		t.Fatalf("a different identity took the account: %d", code)
	}
	if code, out, _ := call("POST", "/scim/v2/Users", map[string]any{"userName": "not-an-email"}, tok); code != 400 || out["scimType"] != "invalidValue" {
		t.Fatalf("%d %v", code, out)
	}
	// the account exists in the application, without a password and without admin rights
	var admin bool
	var hash *string
	e.Store.Pool.QueryRow(ctx, `SELECT is_admin,password_hash FROM app_user WHERE email='new.person@example.com'`).Scan(&admin, &hash)
	if admin || hash != nil {
		t.Fatalf("admin=%v hash=%v", admin, hash)
	}
	// filters and listing
	code, list, _ := call("GET", `/scim/v2/Users?filter=userName%20eq%20%22NEW.person%40example.com%22`, nil, tok)
	if code != 200 || list["totalResults"] != float64(1) || len(list["Resources"].([]any)) != 1 {
		t.Fatalf("%d %v", code, list)
	}
	if code, list, _ = call("GET", `/scim/v2/Users?filter=externalId%20eq%20%22ext-1%22`, nil, tok); list["totalResults"] != float64(1) {
		t.Fatalf("%d %v", code, list)
	}
	if code, out, _ := call("GET", `/scim/v2/Users?filter=emails.value%20co%20%22x%22`, nil, tok); code != 400 || out["scimType"] != "invalidFilter" {
		t.Fatalf("%d %v", code, out)
	}
	if code, list, _ = call("GET", `/scim/v2/Users?startIndex=1&count=2`, nil, tok); code != 200 || len(list["Resources"].([]any)) != 2 || list["totalResults"].(float64) < 3 {
		t.Fatalf("%d %v", code, list)
	}
	// an update through PUT and through PATCH, in both shapes identity providers use
	if code, upd, _ := call("PUT", "/scim/v2/Users/"+id, map[string]any{"displayName": "Renamed Person"}, tok); code != 200 || upd["displayName"] != "Renamed Person" {
		t.Fatalf("%d %v", code, upd)
	}
	if code, upd, _ := call("PATCH", "/scim/v2/Users/"+id, map[string]any{"Operations": []map[string]any{{"op": "replace", "value": map[string]any{"displayName": "Patched"}}}}, tok); code != 200 || upd["displayName"] != "Patched" {
		t.Fatalf("%d %v", code, upd)
	}
	// deprovisioning: a signed-in person is out at once, their token dies, and nothing is deleted
	sess, _ := e.Svc.CreateUser(ctx, "gone@example.com", "Gone", "password123", false)
	login, _ := e.Svc.LoginWith(ctx, "gone@example.com", "password123", time.Hour, clientMetaZero())
	apiTok, _ := e.Svc.CreateAPIToken(ctx, sess.ID, "t")
	if _, err := e.Svc.Authenticate(ctx, login.Token); err != nil {
		t.Fatal(err)
	}
	code, _, _ = call("PATCH", "/scim/v2/Users/"+sess.ID, map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}}}, tok)
	if code != 200 {
		t.Fatal(code)
	}
	if _, err := e.Svc.Authenticate(ctx, login.Token); err == nil {
		t.Fatal("a deprovisioned person's session still works")
	}
	if _, err := e.Svc.Authenticate(ctx, apiTok); err == nil {
		t.Fatal("a deprovisioned person's API token still works")
	}
	if _, err := e.Svc.LoginWith(ctx, "gone@example.com", "password123", time.Hour, clientMetaZero()); err == nil {
		t.Fatal("a deprovisioned person signed in")
	}
	// DELETE deactivates, never removes; reactivation through POST or PATCH works
	if code, _, _ := call("DELETE", "/scim/v2/Users/"+id, nil, tok); code != 204 {
		t.Fatal(code)
	}
	if code, got, _ := call("GET", "/scim/v2/Users/"+id, nil, tok); code != 200 || got["active"] != false {
		t.Fatalf("%d %v", code, got)
	}
	if code, back, _ := call("POST", "/scim/v2/Users", map[string]any{"userName": "new.person@example.com", "externalId": "ext-1"}, tok); code != 200 || back["active"] != true {
		t.Fatalf("%d %v", code, back)
	}
	if code, _, _ := call("GET", "/scim/v2/Users/00000000-0000-0000-0000-000000000000", nil, tok); code != 404 {
		t.Fatal(code)
	}
	// the last administrator cannot be switched off from outside
	var adminID string
	e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM app_user WHERE email='admin@example.com'`).Scan(&adminID)
	if code, out, _ := call("PATCH", "/scim/v2/Users/"+adminID, map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}}}, tok); code != 409 {
		t.Fatalf("%d %v", code, out)
	}
	// provisioning is in the audit log, by "SCIM"
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE actor_label='SCIM' AND action LIKE 'scim.user_%'`).Scan(&n)
	if n < 5 {
		t.Fatalf("%d audit rows", n)
	}
	// a revoked token stops working at once
	toks, _ := e.Svc.ListSCIMTokens(ctx, e.Admin)
	if err := e.Svc.RevokeSCIMToken(ctx, e.Admin, toks[0].ID); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := call("GET", "/scim/v2/Users", nil, tok); code != 401 {
		t.Fatalf("a revoked SCIM token: %d", code)
	}
}

func clientMetaZero() domain.ClientMeta { return domain.ClientMeta{} }
