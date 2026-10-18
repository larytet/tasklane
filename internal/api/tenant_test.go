package api_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// SEC-09: a second organization on the same instance, probed with every request of the authorization matrix.

type testutilServer struct{ ts *httptest.Server }

type tenants struct {
	f                     *fixture
	tok                   map[role]string
	foreign, foreignAdmin string // tokens of a member and of the administrator of organization B
	bAdmin, bMember       domain.Actor
}

func twoOrgs(t *testing.T) (*tenants, *testutilServer) {
	f, tok, ts, _ := matrixEnv(t)
	ctx := testutil.Ctx()
	e := f.e
	if _, err := e.Svc.CreateOrganization(ctx, e.Admin, "Globex", "boss@globex.example", "Boss", "password123"); err != nil {
		t.Fatal(err)
	}
	var bid string
	e.Store.Pool.QueryRow(ctx, `SELECT id FROM app_user WHERE email='boss@globex.example'`).Scan(&bid)
	boss, err := e.Svc.ActorFor(ctx, bid)
	if err != nil || !boss.OrgAdmin || boss.Admin {
		t.Fatalf("%+v %v", boss, err)
	}
	if _, err := e.Svc.CreateProject(ctx, boss, "GLX", "Globex work"); err != nil {
		t.Fatal(err)
	}
	mu, err := e.Svc.CreateUserAs(ctx, boss, "gina@globex.example", "Gina", "password123", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.AddMember(ctx, boss, "GLX", mu.ID, "member"); err != nil {
		t.Fatal(err)
	}
	gina, _ := e.Svc.ActorFor(ctx, mu.ID)
	if _, err := e.Svc.CreateIssue(ctx, gina, domain.CreateIn{Project: "GLX", Summary: "globex secret plan"}); err != nil {
		t.Fatal(err)
	}
	tn := &tenants{f: f, tok: tok, bAdmin: boss, bMember: gina}
	tn.foreign, _ = e.Svc.CreateAPIToken(ctx, gina.ID, "gina")
	tn.foreignAdmin, _ = e.Svc.CreateAPIToken(ctx, boss.ID, "boss")
	return tn, &testutilServer{ts: ts}
}

// tables a person of the other organization may legitimately touch while probing: their own session and token use, and the
// records of what happened (the audit log, the outbox) which carry no organization-A content in a response
// oauth_client: a client registers itself before anyone is signed in and belongs to no organization
var neutralTables = map[string]bool{"oauth_client": true, "session": true, "api_token": true, "audit_event": true, "outbox": true, "outbox_cursor": true, "login_attempt": true, "rate_limit": true}

func snapshot(t *testing.T, e *testutil.Env) map[string]string {
	ctx := context.Background()
	rows, err := e.Store.Pool.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_type='BASE TABLE' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := map[string]string{}
	for _, n := range names {
		if neutralTables[n] {
			continue
		}
		q := fmt.Sprintf(`SELECT coalesce(string_agg(to_jsonb(x)::text,'|' ORDER BY to_jsonb(x)::text),'') FROM %q x`, n)
		var hasUser bool // a table keyed by person holds the other organization's own rows too: compare organization A's people only
		e.Store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name='user_id')`, n).Scan(&hasUser)
		if hasUser {
			q = fmt.Sprintf(`SELECT coalesce(string_agg(to_jsonb(x)::text,'|' ORDER BY to_jsonb(x)::text),'') FROM %q x WHERE user_id IN (SELECT id FROM app_user WHERE org_id::text='`+domain.DefaultOrg+`')`, n)
		}
		switch n { // the tables that hold organization B's own rows are compared on organization A's part only
		case "project":
			q = `SELECT coalesce(string_agg(to_jsonb(x)::text,'|' ORDER BY to_jsonb(x)::text),'') FROM project x WHERE org_id::text='` + domain.DefaultOrg + `'`
		case "app_user":
			q = `SELECT coalesce(string_agg(to_jsonb(x)::text,'|' ORDER BY to_jsonb(x)::text),'') FROM app_user x WHERE org_id::text='` + domain.DefaultOrg + `'`
		case "project_member":
			q = `SELECT coalesce(string_agg(to_jsonb(x)::text,'|' ORDER BY to_jsonb(x)::text),'') FROM project_member x WHERE project_id IN (SELECT id FROM project WHERE org_id::text='` + domain.DefaultOrg + `')`
		case "organization":
			continue
		}
		var s string
		if err := e.Store.Pool.QueryRow(ctx, q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		h := md5.Sum([]byte(s))
		out[n] = hex.EncodeToString(h[:])
	}
	return out
}

// Strings that only organization A's data contains.
var orgAMarkers = []string{"alice@example.com", "bob@example.com", "eve@example.com", "vera@example.com", "spare@example.com", "admin@example.com", "Alice K.", "Bob R.", `"ABC`, "ABC-", "orig text", "matrix "}

func TestSEC09_CrossTenantSweepEveryRouteAsAPersonOfAnotherOrganization(t *testing.T) {
	tn, srv := twoOrgs(t)
	f := tn.f
	type probe struct {
		pattern string
		q       req
	}
	var probes []probe
	for _, r := range rows() { // build everything first: fixtures create organization A's resources
		probes = append(probes, probe{r.pattern, r.build(f)})
	}
	before := snapshot(t, f.e)
	skipped := map[string]bool{
		"POST /api/v1/login": true, "POST /api/v1/login/mfa": true, // a login is the person's own, with their own credentials
	}
	for _, who := range []struct{ name, token string }{{"member of org B", tn.foreign}, {"administrator of org B", tn.foreignAdmin}} {
		n2xx := 0
		for _, p := range probes {
			if skipped[p.pattern] {
				continue
			}
			code, body := do(t, srv.ts, who.token, p.q)
			if code >= 500 {
				t.Errorf("%s %s: server error %d %s", who.name, p.pattern, code, strings.TrimSpace(body))
			}
			if code/100 == 2 {
				n2xx++
				sent := fmt.Sprint(p.q.body, string(p.q.raw), p.q.path) // what the probe itself sent may come back
				for _, m := range orgAMarkers {
					if strings.Contains(body, m) && !strings.Contains(sent, m) {
						t.Errorf("%s: %s answered %d with organization A's data (%q):\n    %.300s", who.name, p.pattern, code, m, strings.TrimSpace(body))
						break
					}
				}
			}
		}
		t.Logf("%s: %d routes answered 2xx (all free of organization A's data)", who.name, n2xx)
	}
	after := snapshot(t, f.e)
	var changed []string
	for n, h := range before {
		if after[n] != h {
			changed = append(changed, n)
		}
	}
	sort.Strings(changed)
	if len(changed) > 0 {
		t.Errorf("a person of organization B changed organization A's data in: %s", strings.Join(changed, ", "))
	}
}

// A control for the sweep: the same probes as a member of organization A do reach A's data and do change it, so a pass above
// means something. (If this ever passes with no changes, the snapshot has stopped seeing anything.)
func TestSEC09_TheSweepCanFail(t *testing.T) {
	tn, srv := twoOrgs(t)
	f := tn.f
	var probes []req
	for _, r := range rows() {
		probes = append(probes, r.build(f))
	}
	before := snapshot(t, f.e)
	leaks := 0
	for _, q := range probes {
		code, body := do(t, srv.ts, tn.tok[member], q)
		if code/100 == 2 {
			for _, m := range orgAMarkers {
				if strings.Contains(body, m) {
					leaks++
					break
				}
			}
		}
	}
	after := snapshot(t, f.e)
	changed := 0
	for n, h := range before {
		if after[n] != h {
			changed++
		}
	}
	if leaks < 10 || changed < 10 {
		t.Fatalf("a member of organization A reached A's data in %d responses and changed %d tables: the sweep cannot tell a leak from silence", leaks, changed)
	}
	t.Logf("control: %d responses with A's data, %d tables changed", leaks, changed)
}
