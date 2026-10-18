package api_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/testutil"
)

// notAdminActions lists matrix rows that only an administrator may call but that are not administrative changes:
// they sign in, sign out, or only read or plan.
var notAudited = map[string]string{
	"POST /api/v1/login":  "sign-in is not an administrative change",
	"POST /api/v1/logout": "sign-out is not an administrative change",
}

func isMutation(pattern string) bool {
	m := strings.SplitN(pattern, " ", 2)[0]
	return m == "POST" || m == "PUT" || m == "PATCH" || m == "DELETE"
}

// SEC-07: every route that only an administrator may use and that changes something leaves an audit record.
func TestSEC07_EveryAdministrativeChangeIsAudited(t *testing.T) {
	f, tok, ts, _ := matrixEnv(t)
	ctx := testutil.Ctx()
	var missing []string
	checked := 0
	for _, r := range rows() {
		if !isMutation(r.pattern) || notAudited[r.pattern] != "" {
			continue
		}
		adminOnly := has(r.exp[admin], 200) || has(r.exp[admin], 201)
		for _, other := range []role{member, viewer, outsider} {
			for _, c := range r.exp[other] {
				if c == 200 || c == 201 {
					adminOnly = false
				}
			}
		}
		if !adminOnly {
			continue
		}
		q := r.build(f)
		var before, after int
		f.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&before)
		code, body := do(t, ts, tok[admin], q)
		if code != 200 && code != 201 {
			continue // the row expects another status for this fixture; the matrix test covers it
		}
		f.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&after)
		checked++
		if after <= before {
			missing = append(missing, r.pattern+"   "+strings.TrimSpace(body)[:min(40, len(strings.TrimSpace(body)))])
		}
	}
	sort.Strings(missing)
	t.Logf("%d administrative changes checked", checked)
	if len(missing) > 0 {
		t.Fatalf("administrative changes with no audit record:\n  %s", strings.Join(missing, "\n  "))
	}
}

func TestSEC07_OrdinaryWorkListHasNoStaleEntries(t *testing.T) {
	_, _, _, srv := matrixEnv(t)
	registered := map[string]bool{}
	for _, p := range srv.Routes() {
		registered[p] = true
	}
	for _, p := range api.OrdinaryRoutes() {
		if !registered[p] {
			t.Errorf("%q is listed as ordinary work but no such route exists", p)
		}
	}
}

func TestSEC07_AuditRecordsWhoWhatAndHow(t *testing.T) {
	f, tok, ts, _ := matrixEnv(t)
	ctx := testutil.Ctx()
	last := func() (actor, action, target string, detail map[string]any) {
		var raw []byte
		if err := f.e.Store.Pool.QueryRow(ctx, `SELECT actor_label,action,target,detail FROM audit_event WHERE action LIKE 'POST /api/v1/fields%' ORDER BY id DESC LIMIT 1`).Scan(&actor, &action, &target, &raw); err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(raw, &detail)
		return
	}
	code, _ := do(t, ts, tok[admin], req{method: "POST", path: "/api/v1/fields", body: map[string]any{"name": "Audited", "type": "text"}})
	if code != 201 {
		t.Fatal(code)
	}
	actor, action, target, detail := last()
	if action != "POST /api/v1/fields" || target != "/api/v1/fields" || detail["status"] != float64(201) || detail["via"] != "token" || detail["ip"] == "" || actor == "" {
		t.Fatalf("%q %q %q %v", actor, action, target, detail)
	}
	// a refused or failed request leaves nothing behind
	var before, after int
	f.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&before)
	if code, _ := do(t, ts, tok[member], req{method: "POST", path: "/api/v1/fields", body: map[string]any{"name": "Nope", "type": "text"}}); code != 403 {
		t.Fatal(code)
	}
	if code, _ := do(t, ts, tok[admin], req{method: "POST", path: "/api/v1/fields", body: map[string]any{"name": "Bad", "type": "nonsense"}}); code != 400 {
		t.Fatal(code)
	}
	f.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&after)
	if after != before {
		t.Fatalf("%d records for requests that did nothing", after-before)
	}
	// ordinary work is not logged here
	is := f.issue()
	f.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&before)
	if code, _ := do(t, ts, tok[member], req{method: "POST", path: "/api/v1/issues/" + is + "/comments", body: map[string]any{"body": "hello"}}); code != 201 && code != 200 {
		t.Fatal(code)
	}
	f.e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event`).Scan(&after)
	if after != before {
		t.Fatalf("a comment was written to the audit log")
	}
	// the viewer endpoints are administrators only, and the chain checks out through the API
	if code, _ := do(t, ts, tok[member], req{method: "GET", path: "/api/v1/admin/audit"}); code != 403 {
		t.Fatal(code)
	}
	code, body := do(t, ts, tok[admin], req{method: "GET", path: "/api/v1/admin/audit?action=POST%20/api/v1/fields&limit=5"})
	if code != 200 || !strings.Contains(body, "/api/v1/fields") {
		t.Fatalf("%d %s", code, body)
	}
	code, body = do(t, ts, tok[admin], req{method: "GET", path: "/api/v1/admin/audit/verify"})
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("%d %s", code, body)
	}
}
