package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

type fieldRig struct {
	e   *testutil.Env
	eng *search.Postgres
}

func newFields(t *testing.T) *fieldRig {
	e := testutil.New(t)
	e.Clock.T = time.Now()
	e.Project()
	return &fieldRig{e: e, eng: &search.Postgres{DB: e.Store.Pool}}
}

func (r *fieldRig) field(t *testing.T, name, typ string, opts ...string) domain.FieldDef {
	f, err := r.e.Svc.CreateField(ctx, r.e.Admin, domain.FieldIn{Name: name, Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.e.Svc.AddContext(ctx, r.e.Admin, f.ID, domain.ContextIn{Global: true, Options: opts}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (r *fieldRig) set(t *testing.T, a domain.Actor, key string, vals map[string]any) error {
	_, err := r.e.Svc.UpdateFields(ctx, a, key, nil, false, domain.Changes{Custom: vals})
	return err
}

func (r *fieldRig) get(t *testing.T, key, field string) any {
	v, err := r.e.Svc.View(ctx, r.e.Admin, key)
	if err != nil {
		t.Fatal(err)
	}
	return v.Issue.Custom[field]
}

func (r *fieldRig) keys(t *testing.T, a domain.Actor, q string) string {
	all, rd, wr, _ := r.e.Svc.SearchScope(ctx, a)
	res, err := r.eng.Search(ctx, search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: r.e.Svc.Now}, q, search.Page{Limit: 100})
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	var ks []string
	for _, x := range res.Rows {
		ks = append(ks, x.Key)
	}
	sort.Strings(ks)
	return strings.Join(ks, " ")
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// CORE-03: every type stores its canonical form, refuses what does not fit, and reads back.
func TestCORE03_FieldTypesStoreAndValidate(t *testing.T) {
	r := newFields(t)
	e := r.e
	is := e.Issue("fields", "")
	other, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "another"})
	bob := e.Bob
	cases := []struct {
		name, typ string
		opts      []string
		good      any
		want      string
		bad       []any
	}{
		{"Short", "text", nil, "  hello  ", `"hello"`, []any{5, "two\nlines", strings.Repeat("x", 256)}},
		{"Long", "textarea", nil, "line one\nline two", `"line one\nline two"`, []any{7, strings.Repeat("x", 40000)}},
		{"Count", "integer", nil, 7, `7`, []any{1.5, "many", "7.5"}},
		{"Ratio", "float", nil, "2.5", `2.5`, []any{"abc"}},
		{"Cost", "number", nil, 10.25, `10.25`, []any{true, "1,5"}},
		{"Due", "date", nil, "2026-12-31", `"2026-12-31"`, []any{"31/12/2026", "2026-13-01", 5}},
		{"Seen", "datetime", nil, "2026-10-19T09:30:00+02:00", `"2026-10-19T07:30:00Z"`, []any{"2026-10-19", "yesterday"}},
		{"Tier", "select", []string{"Gold", "Silver"}, "gold", `"Gold"`, []any{"Bronze", 3}},
		{"Mode", "radio", []string{"A", "B"}, "B", `"B"`, []any{"C"}},
		{"Areas", "multi_select", []string{"UI", "API", "DB"}, []any{"api", "UI", "api"}, `["API","UI"]`, []any{[]any{"Mobile"}, "UI"}},
		{"Checks", "checkbox_group", []string{"one", "two"}, []any{"two"}, `["two"]`, []any{[]any{"three"}}},
		{"Owner", "user", nil, bob.ID, `"` + bob.ID + `"`, []any{"nobody@example.com", 4}},
		{"Reviewers", "multi_user", nil, []any{"alice@example.com", bob.ID}, `["` + e.Alice.ID + `","` + bob.ID + `"]`, []any{[]any{"ghost@example.com"}}},
		{"Spec", "url", nil, "https://example.com/spec", `"https://example.com/spec"`, []any{"javascript:alert(1)", "example.com", "ftp://x/y"}},
		{"Tags", "labels", nil, []any{"alpha", "beta"}, `["alpha","beta"]`, []any{"alpha", []any{"has space"}}},
		{"Effort", "duration", nil, "1d 2h 30m", `37800`, []any{"soon", "-5h", "1x"}},
		{"Stars", "rating", nil, 4, `4`, []any{6, 2.5, "five"}},
		{"Parent issue", "issue_picker", nil, strings.ToLower(other.Key), `"` + other.Key + `"`, []any{"ABC-9999", 8}},
	}
	for _, c := range cases {
		f := r.field(t, c.name, c.typ, c.opts...)
		if err := r.set(t, e.Alice, is.Key, map[string]any{f.Key: c.good}); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := jsonOf(r.get(t, is.Key, f.Key)); got != c.want {
			t.Errorf("%s: stored %s, want %s", c.name, got, c.want)
		}
		for _, bad := range c.bad {
			if err := r.set(t, e.Alice, is.Key, map[string]any{f.Key: bad}); err == nil {
				t.Errorf("%s accepted %v", c.name, bad)
			} else if !strings.Contains(err.Error(), c.name) {
				t.Errorf("%s: the error does not name the field: %v", c.name, err)
			}
		}
		if got := jsonOf(r.get(t, is.Key, f.Key)); got != c.want {
			t.Errorf("%s: a refused value changed the stored one: %s", c.name, got)
		}
		// clear with null; it is gone from the issue
		if err := r.set(t, e.Alice, is.Key, map[string]any{f.Name: nil}); err != nil { // by name
			t.Errorf("%s: %v", c.name, err)
		}
		if r.get(t, is.Key, f.Key) != nil {
			t.Errorf("%s: not cleared", c.name)
		}
	}
	// a field that applies to nothing is refused, by key or name
	if err := r.set(t, e.Alice, is.Key, map[string]any{"customfield_999999": "x"}); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestCORE03_ContextsDefaultsRequiredAndRoles(t *testing.T) {
	r := newFields(t)
	e := r.e
	e.Svc.CreateProject(ctx, e.Admin, "XYZ", "Other")
	e.Svc.AddMember(ctx, e.Admin, "XYZ", e.Alice.ID, "member")
	// "Severity": a global context with its own options, and a specific one for ABC bugs that is required with a default
	f, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Severity", Type: "select"})
	if _, err := e.Svc.AddContext(ctx, e.Admin, f.ID, domain.ContextIn{Global: true, Options: []string{"Low", "High"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.AddContext(ctx, e.Admin, f.ID, domain.ContextIn{Projects: []string{"ABC"}, Types: []string{"Bug"}, Required: true, Default: "Nonsense", Options: []string{"Minor", "Major", "Blocker"}}); err == nil {
		t.Fatal("a default that is not among the options was accepted")
	}
	// (the failed attempts above left no context behind)
	ctxs, _ := e.Svc.ListContexts(ctx, f.ID)
	if len(ctxs) != 1 {
		t.Fatalf("%d contexts", len(ctxs))
	}
	specific, err := e.Svc.AddContext(ctx, e.Admin, f.ID, domain.ContextIn{Projects: []string{"ABC"}, Types: []string{"Bug"}, Required: true, Options: []string{"Minor", "Major", "Blocker"}})
	if err != nil {
		t.Fatal(err)
	}
	// the most specific context wins: ABC bugs get the required three-level list, everything else the global one
	forBug, _ := e.Svc.ProjectFields(ctx, e.Alice, "ABC", "Bug")
	forTask, _ := e.Svc.ProjectFields(ctx, e.Alice, "ABC", "Task")
	forXYZ, _ := e.Svc.ProjectFields(ctx, e.Alice, "XYZ", "Bug")
	opt := func(cs []domain.FieldConfig) string {
		for _, c := range cs {
			if c.Name == "Severity" {
				var o []string
				for _, x := range c.Options {
					o = append(o, x.Value)
				}
				return fmt.Sprintf("%v required=%v", o, c.Required)
			}
		}
		return "absent"
	}
	if opt(forBug) != "[Minor Major Blocker] required=true" || opt(forTask) != "[Low High] required=false" || opt(forXYZ) != "[Low High] required=false" {
		t.Fatalf("%s | %s | %s", opt(forBug), opt(forTask), opt(forXYZ))
	}
	// creating a bug without it fails and says so; with a valid value it works; a task needs nothing
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "no severity"}); err == nil || !strings.Contains(err.Error(), "Severity is required") {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "bad", Custom: map[string]any{"Severity": "Low"}}); err == nil { // "Low" is a global option, not this context's
		t.Fatal("an option of another context was accepted")
	}
	bug, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "with severity", Custom: map[string]any{"Severity": "blocker"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := jsonOf(r.get(t, bug.Key, f.Key)); got != `"Blocker"` {
		t.Fatal(got)
	}
	if _, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "task"}); err != nil {
		t.Fatal(err)
	}
	// a required field cannot be cleared; a field that does not apply to the type is refused
	if err := r.set(t, e.Alice, bug.Key, map[string]any{f.Key: nil}); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("%v", err)
	}
	// a default fills a new issue when the context has one
	d, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Region", Type: "select"})
	e.Svc.AddContext(ctx, e.Admin, d.ID, domain.ContextIn{Global: true, Options: []string{"EU", "US"}, Default: "EU"})
	nd, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: "defaults"})
	if err != nil || jsonOf(r.get(t, nd.Key, d.Key)) != `"EU"` {
		t.Fatalf("%v %v", err, r.get(t, nd.Key, d.Key))
	}
	// contexts can be removed; the field then applies nowhere there
	e.Svc.RemoveContext(ctx, e.Admin, f.ID, specific.ID)
	if opt(mustFields(t, e, "ABC", "Bug")) != "[Low High] required=false" {
		t.Fatal("the removed context still applies")
	}
	// field-level permissions: only project admins may edit; viewers and members may not see a hidden one
	secret, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Budget", Type: "number", EditRoles: []string{"admin"}, ViewRoles: []string{"member", "admin"}})
	e.Svc.AddContext(ctx, e.Admin, secret.ID, domain.ContextIn{Global: true})
	if err := r.set(t, e.Alice, nd.Key, map[string]any{secret.Key: 100}); err == nil || !strings.Contains(err.Error(), "may not change") {
		t.Fatalf("a member edited an admin-only field: %v", err)
	}
	if err := r.set(t, e.Admin, nd.Key, map[string]any{secret.Key: 100}); err != nil {
		t.Fatal(err)
	}
	viewer, _ := e.Svc.CreateUser(ctx, "view@example.com", "Viewer", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", viewer.ID, "viewer")
	va := domain.Actor{ID: viewer.ID, Kind: "human"}
	if v, _ := e.Svc.View(ctx, va, nd.Key); v.Issue.Custom[secret.Key] != nil {
		t.Fatal("a viewer saw a field restricted to members")
	}
	if v, _ := e.Svc.View(ctx, e.Alice, nd.Key); v.Issue.Custom[secret.Key] == nil {
		t.Fatal("a member lost a field they may view")
	}
	if cs, _ := e.Svc.ProjectFields(ctx, va, "ABC", "Task"); func() bool {
		for _, c := range cs {
			if c.Name == "Budget" {
				return true
			}
		}
		return false
	}() {
		t.Fatal("the form definition lists a field the viewer may not see")
	}
	// and the hidden field cannot be searched by someone who may not view it (it is refused as unknown)
	if _, err := r.eng.Search(ctx, search.Scope{UserID: va.ID, Readable: mustReadable(t, e, va), WeekStart: time.Monday, Now: e.Svc.Now}, `Budget > 50`, search.Page{}); err == nil {
		t.Fatal("a restricted field could be searched by a viewer")
	}
	if got := r.keys(t, e.Admin, `Budget > 50`); got != nd.Key {
		t.Fatalf("%q", got)
	}
}

func mustFields(t *testing.T, e *testutil.Env, project, typ string) []domain.FieldConfig {
	cs, err := e.Svc.ProjectFields(ctx, e.Alice, project, typ)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func mustReadable(t *testing.T, e *testutil.Env, a domain.Actor) []string {
	_, rd, _, _ := e.Svc.SearchScope(ctx, a)
	return rd
}

func TestCORE03_ValidationUniquenessHistoryAndConflicts(t *testing.T) {
	r := newFields(t)
	e := r.e
	min, max := 1.0, 100.0
	score, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Score", Type: "integer", Min: &min, Max: &max})
	e.Svc.AddContext(ctx, e.Admin, score.ID, domain.ContextIn{Global: true})
	code, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Code", Type: "text", Regex: ptr("^[A-Z]{3}-\\d{3}$"), Unique: ptr2(true)})
	e.Svc.AddContext(ctx, e.Admin, code.ID, domain.ContextIn{Global: true})
	a, b := e.Issue("a", ""), e.Issue("b", "")
	for _, bad := range []any{0, 101, 50.5} {
		if err := r.set(t, e.Alice, a.Key, map[string]any{score.Key: bad}); err == nil {
			t.Errorf("score %v accepted", bad)
		}
	}
	if err := r.set(t, e.Alice, a.Key, map[string]any{score.Key: 42, code.Key: "ABC-123"}); err != nil {
		t.Fatal(err)
	}
	if err := r.set(t, e.Alice, a.Key, map[string]any{code.Key: "abc-123"}); err == nil || !strings.Contains(err.Error(), "pattern") {
		t.Fatalf("%v", err)
	}
	if err := r.set(t, e.Alice, b.Key, map[string]any{code.Key: "ABC-123"}); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("a duplicate of a unique field was accepted: %v", err)
	}
	if err := r.set(t, e.Alice, a.Key, map[string]any{code.Key: "ABC-123"}); err != nil { // the same issue may keep its own value
		t.Fatal(err)
	}
	if _, err := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Bad", Type: "text", Regex: ptr("([")}); err == nil {
		t.Fatal("an invalid pattern was accepted")
	}
	if _, err := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "score", Type: "text"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("a duplicate name (case-insensitive) was accepted: %v", err)
	}
	// history: every change is recorded with before and after, unless the field opted out
	r.set(t, e.Alice, a.Key, map[string]any{score.Key: 43})
	h, _ := e.Svc.History(ctx, e.Alice, a.Key)
	var found *domain.HistoryEntry
	for i := range h {
		if h[i].Field == score.Key && h[i].After != nil && *h[i].After == "43" {
			found = &h[i]
		}
	}
	if found == nil || found.Before == nil || *found.Before != "42" || found.Actor != "Alice K." {
		t.Fatalf("%+v", h)
	}
	quiet, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Quiet", Type: "text", TrackHistory: ptr2(false)})
	e.Svc.AddContext(ctx, e.Admin, quiet.ID, domain.ContextIn{Global: true})
	r.set(t, e.Alice, a.Key, map[string]any{quiet.Key: "shh"})
	h, _ = e.Svc.History(ctx, e.Alice, a.Key)
	for _, x := range h {
		if x.Field == quiet.Key {
			t.Fatal("an untracked field left history")
		}
	}
	// conflicts are per field: a stale base conflicts on the field that changed, and not on others
	v, _ := e.Svc.View(ctx, e.Alice, a.Key)
	stale := v.Issue.Version
	r.set(t, e.Bob, a.Key, map[string]any{score.Key: 44})
	_, err := e.Svc.UpdateFields(ctx, e.Alice, a.Key, &stale, false, domain.Changes{Custom: map[string]any{score.Key: 45}})
	var vc *domain.VersionConflict
	if !errors.As(err, &vc) || len(vc.Fields) != 1 || vc.Fields[0] != score.Key {
		t.Fatalf("%v", err)
	}
	if _, err := e.Svc.UpdateFields(ctx, e.Alice, a.Key, &stale, false, domain.Changes{Custom: map[string]any{quiet.Key: "other field, no conflict"}}); err != nil {
		t.Fatalf("a different field conflicted: %v", err)
	}
}

func ptr2(b bool) *bool { return &b }

func TestCORE03_LifecycleTrashMergeReplaceAndWarnings(t *testing.T) {
	r := newFields(t)
	e := r.e
	tier := r.field(t, "Tier", "select", "Gold", "Silver", "Platinum")
	a, b, c := e.Issue("a", ""), e.Issue("b", ""), e.Issue("c", "")
	r.set(t, e.Alice, a.Key, map[string]any{tier.Key: "Silver"})
	r.set(t, e.Alice, b.Key, map[string]any{tier.Key: "Platinum"})
	// archive an option: issues keep it, new values are refused
	ctxs, _ := e.Svc.ListContexts(ctx, tier.ID)
	var plat int64
	for _, o := range ctxs[0].Options {
		if o.Value == "Platinum" {
			plat = o.ID
		}
	}
	e.Svc.ArchiveOption(ctx, e.Admin, plat, true)
	if got := jsonOf(r.get(t, b.Key, tier.Key)); got != `"Platinum"` {
		t.Fatalf("an archived option was removed from an issue: %s", got)
	}
	if err := r.set(t, e.Alice, c.Key, map[string]any{tier.Key: "Platinum"}); err == nil {
		t.Fatal("an archived option was offered for a new value")
	}
	// merge Silver and Platinum into Gold: every issue is rewritten, the options are gone
	if n, err := e.Svc.MergeOptions(ctx, e.Admin, tier.ID, "Silver", "Gold"); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, err := e.Svc.MergeOptions(ctx, e.Admin, tier.ID, "Platinum", "Gold"); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if jsonOf(r.get(t, a.Key, tier.Key)) != `"Gold"` || jsonOf(r.get(t, b.Key, tier.Key)) != `"Gold"` {
		t.Fatal("not merged")
	}
	if _, err := e.Svc.MergeOptions(ctx, e.Admin, tier.ID, "Gold", "Gold"); err == nil {
		t.Fatal("merging an option into itself")
	}
	// replace by another field of the same type: values move, the old field is trashed; a different type is refused
	plan := r.field(t, "Plan", "select", "Gold", "Basic")
	if n, err := e.Svc.ReplaceField(ctx, e.Admin, tier.ID, plan.ID); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if jsonOf(r.get(t, a.Key, plan.Key)) != `"Gold"` || r.get(t, a.Key, tier.Key) != nil {
		t.Fatal("not replaced")
	}
	txt := r.field(t, "Notes", "text")
	if _, err := e.Svc.ReplaceField(ctx, e.Admin, plan.ID, txt.ID); err == nil {
		t.Fatal("replaced across types")
	}
	// trash and restore within the retention
	if err := e.Svc.TrashField(ctx, e.Admin, plan.ID); err != nil {
		t.Fatal(err)
	}
	if r.get(t, a.Key, plan.Key) != nil {
		t.Fatal("a trashed field still shows on issues")
	}
	if _, err := r.eng.Search(ctx, search.Scope{All: true, WeekStart: time.Monday, Now: e.Svc.Now}, `Plan = Gold`, search.Page{}); err == nil {
		t.Fatal("a trashed field is still searchable")
	}
	if err := e.Svc.RestoreField(ctx, e.Admin, plan.ID); err != nil {
		t.Fatal(err)
	}
	if jsonOf(r.get(t, a.Key, plan.Key)) != `"Gold"` {
		t.Fatal("restore lost the values")
	}
	e.Svc.TrashField(ctx, e.Admin, plan.ID)
	e.Clock.Advance(31 * 24 * time.Hour)
	if err := e.Svc.RestoreField(ctx, e.Admin, plan.ID); err == nil {
		t.Fatal("restored after the retention")
	}
	// warnings for fields nobody uses or that apply nowhere; permissions; limits
	orphan, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Orphan", Type: "text"})
	_ = orphan
	ws, _ := e.Svc.FieldWarnings(ctx, e.Admin)
	joined := strings.Join(ws, "|")
	if !strings.Contains(joined, "Orphan has no context") || !strings.Contains(joined, "Notes is not used") {
		t.Fatalf("%v", ws)
	}
	for _, f := range []func() error{
		func() error {
			_, err := e.Svc.CreateField(ctx, e.Alice, domain.FieldIn{Name: "x", Type: "text"})
			return err
		},
		func() error { return e.Svc.TrashField(ctx, e.Alice, txt.ID) },
		func() error {
			_, err := e.Svc.AddContext(ctx, e.Alice, txt.ID, domain.ContextIn{Global: true})
			return err
		},
		func() error { _, err := e.Svc.FieldWarnings(ctx, e.Alice); return err },
	} {
		if err := f(); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("a member managed fields: %v", err)
		}
	}
	if _, err := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Weird", Type: "hologram"}); err == nil {
		t.Fatal("an unknown type was accepted")
	}
	if _, err := e.Svc.UpdateField(ctx, e.Admin, txt.ID, domain.FieldIn{Type: "number"}); err == nil {
		t.Fatal("a type change was accepted")
	}
}

// TQL-04: custom fields are queryable by id and by name with typed operators, across contexts.
func TestTQL04_CustomFieldQueries(t *testing.T) {
	r := newFields(t)
	e := r.e
	tier := r.field(t, "Customer Tier", "select", "Gold", "Silver")
	effort := r.field(t, "Effort", "duration")
	due := r.field(t, "Due", "date")
	areas := r.field(t, "Areas", "multi_select", "UI", "API", "DB")
	score := r.field(t, "Score", "integer")
	owner := r.field(t, "Owner", "user")
	note := r.field(t, "Note", "text")
	k := map[string]string{}
	mk2 := func(name string, vals map[string]any) {
		is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Summary: name, Custom: vals})
		if err != nil {
			t.Fatal(err)
		}
		k[name] = is.Key
	}
	mk2("one", map[string]any{tier.Key: "Gold", effort.Key: "2h", due.Key: "2026-11-01", areas.Key: []any{"UI", "API"}, score.Key: 10, owner.Key: e.Bob.ID, note.Key: "Urgent customer"})
	mk2("two", map[string]any{tier.Key: "Silver", effort.Key: "1d", due.Key: "2026-12-15", areas.Key: []any{"DB"}, score.Key: 20, owner.Key: e.Alice.ID, note.Key: "later"})
	mk2("three", map[string]any{score.Key: 30})
	mk2("four", nil)
	id := func(f domain.FieldDef) string { return fmt.Sprintf("cf[%d]", f.ID) }
	ks := func(names ...string) string {
		var out []string
		for _, n := range names {
			out = append(out, k[n])
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	for q, want := range map[string]string{
		`"Customer Tier" = Gold`:                         ks("one"),
		id(tier) + ` = silver`:                           ks("two"),
		`"customer tier" in (Gold, Silver)`:              ks("one", "two"),
		`"Customer Tier" != Gold`:                        ks("two", "three", "four"), // issues without a value are not Gold
		`"Customer Tier" is EMPTY`:                       ks("three", "four"),
		`"Customer Tier" is not EMPTY`:                   ks("one", "two"),
		`customfield_` + fmt.Sprint(score.ID) + ` >= 20`: ks("two", "three"),
		`Score < 20`:                                     ks("one"),
		`Score in (10, 30)`:                              ks("one", "three"),
		`Effort > 1h`:                                    ks("one", "two"),
		`Effort >= 1d`:                                   ks("two"),
		`Effort = 2h`:                                    ks("one"),
		`Due < "2026-12-01"`:                             ks("one"),
		`Due = "2026-12-15"`:                             ks("two"),
		`Due is EMPTY`:                                   ks("three", "four"),
		`Areas = API`:                                    ks("one"),
		`Areas in (DB, UI)`:                              ks("one", "two"),
		`Areas != UI`:                                    ks("two", "three", "four"),
		`Note ~ customer`:                                ks("one"),
		`Note !~ customer`:                               ks("two", "three", "four"),
		`Owner = "alice@example.com"`:                    ks("two"),
		`Owner = "Bob R."`:                               ks("one"),
		`Owner = currentUser()`:                          ks("two"), // the rig's search user is Alice
		`"Customer Tier" = Gold OR Score = 30`:           ks("one", "three"),
		`NOT Score = 10`:                                 ks("two", "three", "four"),
		`project = ABC AND Score > 5 AND Areas = DB`:     ks("two"),
	} {
		got := r.keys(t, e.Alice, q)
		if got != want {
			t.Errorf("%s\n  got  %s\n  want %s", q, got, want)
		}
	}
	// sorting by typed values: numbers numerically, dates by date, with empty values last
	order := func(q string) string {
		all, rd, wr, _ := e.Svc.SearchScope(ctx, e.Alice)
		res, err := r.eng.Search(ctx, search.Scope{UserID: e.Alice.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: e.Svc.Now}, q, search.Page{Limit: 100})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		var ks []string
		for _, x := range res.Rows {
			ks = append(ks, x.Key)
		}
		return strings.Join(ks, " ")
	}
	if got := order(`project = ABC ORDER BY Score DESC`); !strings.HasPrefix(got, k["three"]+" "+k["two"]+" "+k["one"]) {
		t.Fatalf("%s", got)
	}
	if got := order(`project = ABC AND Due is not EMPTY ORDER BY Due ASC`); got != k["one"]+" "+k["two"] {
		t.Fatalf("%s", got)
	}
	if got := order(`project = ABC ORDER BY "Customer Tier"`); !strings.HasPrefix(got, k["one"]+" "+k["two"]) {
		t.Fatalf("%s", got)
	}
	// history operators: was, changed (by, from, to)
	r.set(t, e.Alice, k["one"], map[string]any{tier.Key: "Silver"})
	r.set(t, e.Bob, k["three"], map[string]any{tier.Key: "Gold"})
	for q, want := range map[string]string{
		`"Customer Tier" was Gold`:                                  ks("one", "three"), // had the value at some point: one was Gold then Silver, three became Gold
		`"Customer Tier" changed`:                                   ks("one", "three"),
		`"Customer Tier" changed from Gold to Silver`:               ks("one"),
		`"Customer Tier" changed by currentUser()`:                  ks("one"),
		`"Customer Tier" was not Silver AND "Customer Tier" = Gold`: ks("three"),
	} {
		if got := r.keys(t, e.Alice, q); got != want {
			t.Errorf("%s\n  got  %s\n  want %s", q, got, want)
		}
	}
	// errors are positioned messages, not 500s
	for _, q := range []string{`Score ~ 5`, `Effort = soon`, `Due > yesterday-ish`, `"No Such Field" = 1`, `Score = EMPTY`} {
		all, rd, wr, _ := e.Svc.SearchScope(ctx, e.Alice)
		if _, err := r.eng.Search(ctx, search.Scope{UserID: e.Alice.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: e.Svc.Now}, q, search.Page{}); err == nil {
			t.Errorf("%q was accepted", q)
		} else if _, ok := search.IsQueryError(err); !ok {
			t.Errorf("%q: not a query error: %v", q, err)
		}
	}
	// a trashed field disappears from search, and from permissions: people see only what they may see
	e.Svc.TrashField(ctx, e.Admin, note.ID)
	all, rd, wr, _ := e.Svc.SearchScope(ctx, e.Alice)
	if _, err := r.eng.Search(ctx, search.Scope{UserID: e.Alice.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: e.Svc.Now}, `Note ~ customer`, search.Page{}); err == nil {
		t.Fatal("a trashed field is still searchable")
	}
}

func ptr(s string) *string { return &s }

func TestCORE03_FieldsThroughConvertAndClone(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	cp := true
	keep, err := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Copied", Type: "text", CopyOnClone: &cp})
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Plain", Type: "text"})
	storyOnly, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "StoryOnly", Type: "text"})
	for _, f := range []domain.FieldDef{keep, plain} {
		if _, err := e.Svc.AddContext(ctx, e.Admin, f.ID, domain.ContextIn{Global: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Svc.AddContext(ctx, e.Admin, storyOnly.ID, domain.ContextIn{Projects: []string{"ABC"}, Types: []string{"Story"}}); err != nil {
		t.Fatal(err)
	}
	is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Story", Summary: "s", Description: "d",
		Custom: map[string]any{fmt.Sprint("customfield_", keep.ID): "k", fmt.Sprint("customfield_", plain.ID): "p", fmt.Sprint("customfield_", storyOnly.ID): "so"}})
	if err != nil {
		t.Fatal(err)
	}
	// clone: only the copy-on-clone field travels
	p := life(t, e, e.Alice, is.Key, domain.LifecycleIn{Op: "clone"})
	if p.Key2 == "" {
		t.Fatalf("%+v", p)
	}
	cv, err := e.Svc.View(ctx, e.Alice, p.Key2)
	if err != nil {
		t.Fatal(err)
	}
	if cv.Issue.Custom[fmt.Sprint("customfield_", keep.ID)] != "k" || cv.Issue.Custom[fmt.Sprint("customfield_", plain.ID)] != nil {
		t.Fatalf("clone fields %v", cv.Issue.Custom)
	}
	// convert to Task: the Story-only value is dropped but kept in the record, and the plan says so
	plan := life(t, e, e.Alice, is.Key, domain.LifecycleIn{Op: "convert", Type: "Task", DryRun: true})
	if plan.Counts["custom_fields_dropped"] != 1 {
		t.Fatalf("%+v", plan)
	}
	life(t, e, e.Alice, is.Key, domain.LifecycleIn{Op: "convert", Type: "Task"})
	v, _ := e.Svc.View(ctx, e.Alice, is.Key)
	if v.Issue.Custom[fmt.Sprint("customfield_", storyOnly.ID)] != nil || v.Issue.Custom[fmt.Sprint("customfield_", plain.ID)] != "p" {
		t.Fatalf("after convert %v", v.Issue.Custom)
	}
	var n int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM removed_values WHERE kind='fields'`).Scan(&n)
	if n != 1 {
		t.Fatalf("removed_values records: %d", n)
	}
}
