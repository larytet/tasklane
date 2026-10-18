package domain_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/testutil"
)

func trans(t *testing.T, e *testutil.Env, a domain.Actor, key, name string) (string, bool) {
	t.Helper()
	v, err := e.Svc.View(ctx, a, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range v.Transitions {
		if x.Name == name {
			return x.ID, true
		}
	}
	return "", false
}

// transitionID looks the transition up directly: View hides one whose conditions fail, and tests need its id anyway.
func transitionID(t *testing.T, e *testutil.Env, name string) string {
	t.Helper()
	var id string
	if err := e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM transition WHERE name=$1`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func statusOf(t *testing.T, e *testutil.Env, key string) string {
	v, err := e.Svc.View(ctx, e.Admin, key)
	if err != nil {
		t.Fatal(err)
	}
	return v.Issue.Status
}

func TestLIFE02_ConditionsHideAndRefuse(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	key := mk(t, e, e.Alice, "Task", "t", "")
	tid := transitionID(t, e, "Resolve")
	if err := e.Svc.SetTransitionRules(ctx, e.Admin, tid, domain.TRules{Conditions: []domain.Spec{{"kind": "role", "roles": []string{"admin"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := trans(t, e, e.Alice, key, "Resolve"); ok {
		t.Fatal("a transition whose condition fails is offered")
	}
	if _, ok := trans(t, e, e.Admin, key, "Resolve"); !ok {
		t.Fatal("the administrator does not see it")
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, key, tid, nil); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("%v", err)
	}
	// assignee only
	e.Svc.SetTransitionRules(ctx, e.Admin, tid, domain.TRules{Conditions: []domain.Spec{{"kind": "assignee_only"}}})
	if _, ok := trans(t, e, e.Alice, key, "Resolve"); ok {
		t.Fatal("offered to someone who is not the assignee")
	}
	bob := e.Bob.ID
	e.Svc.UpdateFields(ctx, e.Alice, key, nil, false, domain.Changes{Assignee: &bob})
	if _, err := e.Svc.Transition(ctx, e.Alice, key, tid, nil); err == nil {
		t.Fatal("a non-assignee transitioned")
	}
	if _, err := e.Svc.Transition(ctx, e.Bob, key, tid, nil); err != nil {
		t.Fatal(err)
	}
	// every subtask must be done
	story := mk(t, e, e.Alice, "Story", "parent", "")
	sub := mk(t, e, e.Alice, "Subtask", "child", story)
	e.Svc.SetTransitionRules(ctx, e.Admin, tid, domain.TRules{Conditions: []domain.Spec{{"kind": "subtasks_resolved"}}})
	if _, ok := trans(t, e, e.Alice, story, "Resolve"); ok {
		t.Fatal("offered with an open subtask")
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, sub, tid, nil); err == nil {
		// the subtask itself has no children, so its own condition holds
	} else {
		t.Fatal(err)
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, story, tid, nil); err != nil {
		t.Fatal(err)
	}
	// bad rules are refused when saved
	for _, bad := range []domain.TRules{
		{Conditions: []domain.Spec{{"kind": "nonsense"}}},
		{Validators: []domain.Spec{{"kind": "regex", "field": "labels", "pattern": "("}}},
		{PostFunctions: []domain.Spec{{"kind": "webhook", "url": "ftp://x"}}},
		{Validators: []domain.Spec{{"kind": "field_required", "field": "customfield_99999"}}},
	} {
		if err := e.Svc.SetTransitionRules(ctx, e.Admin, tid, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if err := e.Svc.SetTransitionRules(ctx, e.Alice, tid, domain.TRules{}); err == nil {
		t.Fatal("a member saved rules")
	}
}

func TestLIFE02_ValidatorsAndTransitionScreen(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	f, _ := e.Svc.CreateField(ctx, e.Admin, domain.FieldIn{Name: "Root cause", Type: "text"})
	e.Svc.AddContext(ctx, e.Admin, f.ID, domain.ContextIn{Global: true})
	key := mk(t, e, e.Alice, "Bug", "b", "")
	tid := transitionID(t, e, "Resolve")
	rules := domain.TRules{
		Validators: []domain.Spec{
			{"kind": "field_required", "field": f.Key, "message": "say what caused it"},
			{"kind": "regex", "field": f.Key, "pattern": "^[a-z ]+$"},
			{"kind": "previous_status", "statuses": []string{"To Do"}},
		},
		Screen: []string{"CF[" + f.Key[len("customfield_"):] + "]", "priority"},
	}
	if err := e.Svc.SetTransitionRules(ctx, e.Admin, tid, rules); err != nil {
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, key)
	var screen []string
	for _, x := range v.Transitions {
		if x.ID == tid {
			screen = x.Screen
		}
	}
	if len(screen) != 2 || screen[0] != f.Key {
		t.Fatalf("the transition carries its screen: %v", screen)
	}
	_, err := e.Svc.Transition(ctx, e.Alice, key, tid, nil)
	wantErr(t, err, "say what caused it")
	e.Svc.UpdateFields(ctx, e.Alice, key, nil, false, domain.Changes{Custom: map[string]any{f.Key: "Bad Value 7"}})
	_, err = e.Svc.Transition(ctx, e.Alice, key, tid, nil)
	wantErr(t, err, "does not match")
	e.Svc.UpdateFields(ctx, e.Alice, key, nil, false, domain.Changes{Custom: map[string]any{f.Key: "a race in the cache"}})
	if _, err := e.Svc.Transition(ctx, e.Alice, key, tid, nil); err != nil {
		t.Fatal(err)
	}
	if statusOf(t, e, key) != "Done" {
		t.Fatal("not done")
	}
	// permission and previous_status
	k2 := mk(t, e, e.Alice, "Task", "t2", "")
	e.Svc.SetTransitionRules(ctx, e.Admin, tid, domain.TRules{Validators: []domain.Spec{{"kind": "permission", "role": "admin"}}})
	_, err = e.Svc.Transition(ctx, e.Alice, k2, tid, nil)
	wantErr(t, err, "admin role")
	if _, err := e.Svc.Transition(ctx, e.Admin, k2, tid, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLIFE02_PostFunctionsAreAtomic(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	key := mk(t, e, e.Alice, "Task", "t", "")
	tid := transitionID(t, e, "Resolve")
	good := domain.TRules{PostFunctions: []domain.Spec{
		{"kind": "set_field", "field": "priority", "value": "low"},
		{"kind": "assign", "to": "reporter"},
		{"kind": "add_comment", "text": "moved {{from}} to {{to}} on {{key}}"},
	}}
	if err := e.Svc.SetTransitionRules(ctx, e.Admin, tid, good); err != nil {
		t.Fatal(err)
	}
	bob := e.Bob.ID
	hi := "high"
	e.Svc.UpdateFields(ctx, e.Alice, key, nil, false, domain.Changes{Priority: &hi, Assignee: &bob})
	if _, err := e.Svc.Transition(ctx, e.Alice, key, tid, nil); err != nil {
		t.Fatal(err)
	}
	v, _ := e.Svc.View(ctx, e.Alice, key)
	if v.Issue.Status != "Done" || v.Issue.Priority != "low" || v.Issue.Assignee == nil || v.Issue.Assignee.ID != e.Alice.ID {
		t.Fatalf("%+v", v.Issue)
	}
	if len(v.Comments) == 0 || v.Comments[len(v.Comments)-1].Body != "moved To Do to Done on "+key {
		t.Fatalf("%+v", v.Comments)
	}
	// history shows who changed what
	h, _ := e.Svc.History(ctx, e.Alice, key)
	var sawPrio bool
	for _, x := range h {
		if x.Field == "priority" && x.After != nil && *x.After == "low" {
			sawPrio = true
		}
	}
	if !sawPrio {
		t.Fatalf("%+v", h)
	}
	// a failing post-function rolls the whole transition back: status, earlier post-functions and comment
	k2 := mk(t, e, e.Alice, "Task", "t2", "")
	bad := domain.TRules{PostFunctions: []domain.Spec{
		{"kind": "add_comment", "text": "should not survive"},
		{"kind": "set_field", "field": "priority", "value": "banana"},
	}}
	e.Svc.SetTransitionRules(ctx, e.Admin, tid, bad)
	if _, err := e.Svc.Transition(ctx, e.Alice, k2, tid, nil); err == nil {
		t.Fatal("a failing post-function did not fail the transition")
	}
	v2, _ := e.Svc.View(ctx, e.Alice, k2)
	if v2.Issue.Status != "To Do" || len(v2.Comments) != 0 {
		t.Fatalf("not atomic: %s %d comments", v2.Issue.Status, len(v2.Comments))
	}
}

func TestLIFE02_ConcurrentTransitionsOneWins(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	key := mk(t, e, e.Alice, "Task", "race", "")
	tid := transitionID(t, e, "Start progress")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.Svc.Transition(ctx, e.Alice, key, tid, nil)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if strings.Contains(err.Error(), "not available from status") {
				refused++
			} else {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || refused != 11 {
		t.Fatalf("%d won, %d refused", ok, refused)
	}
}

func TestLIFE02_AsyncPostFunctions(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	var mu sync.Mutex
	var got []string
	var sigs []string
	fail := 1 // the first delivery is refused: it must be retried
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if fail > 0 {
			fail--
			w.WriteHeader(500)
			return
		}
		got = append(got, string(b))
		sigs = append(sigs, r.Header.Get("X-Tasklane-Signature"))
	}))
	defer srv.Close()
	e.Svc.HookEgress = func(p egress.Policy) egress.Policy { p.AllowPrivate = true; return p }
	key := mk(t, e, e.Alice, "Story", "parent", "")
	tid := transitionID(t, e, "Start progress")
	rules := domain.TRules{PostFunctions: []domain.Spec{
		{"kind": "webhook", "url": srv.URL, "secret": "s3cret"},
		{"kind": "create_subtask", "summary": "Write the release notes"},
	}}
	if err := e.Svc.SetTransitionRules(ctx, e.Admin, tid, rules); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Transition(ctx, e.Alice, key, tid, nil); err != nil {
		t.Fatal(err)
	}
	// nothing has been delivered yet and the secret is not stored in the clear
	var plain int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM workflow_job WHERE params::text LIKE '%s3cret%'`).Scan(&plain)
	if plain != 0 || len(got) != 0 {
		t.Fatalf("plain secrets %d, early deliveries %d", plain, len(got))
	}
	if n, err := e.Svc.ProcessWorkflowJobs(ctx, 10); err != nil || n != 1 { // the subtask; the webhook failed once
		t.Fatalf("%d %v", n, err)
	}
	var state, lastErr string
	e.Store.Pool.QueryRow(ctx, `SELECT state,last_error FROM workflow_job WHERE kind='webhook'`).Scan(&state, &lastErr)
	if state != "pending" || !strings.Contains(lastErr, "500") {
		t.Fatalf("%s %q", state, lastErr)
	}
	e.Clock.T = e.Clock.T.Add(time.Hour)
	if n, err := e.Svc.ProcessWorkflowJobs(ctx, 10); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("deliveries: %d", len(got))
	}
	var p map[string]any
	json.Unmarshal([]byte(got[0]), &p)
	if p["key"] != key || p["from"] != "To Do" || p["to"] != "In Progress" {
		t.Fatalf("%v", p)
	}
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte(got[0]))
	if sigs[0] != "sha256="+hex.EncodeToString(m.Sum(nil)) {
		t.Fatalf("signature %q", sigs[0])
	}
	// the subtask exists once, even if the job runs again
	var kids int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE summary='Write the release notes'`).Scan(&kids)
	if kids != 1 {
		t.Fatalf("subtasks: %d", kids)
	}
	// a private address is refused unless allowed
	e.Svc.HookEgress = nil
	k2 := mk(t, e, e.Alice, "Task", "other", "")
	e.Svc.SetTransitionRules(ctx, e.Admin, tid, domain.TRules{PostFunctions: []domain.Spec{{"kind": "webhook", "url": srv.URL}}})
	if _, err := e.Svc.Transition(ctx, e.Alice, k2, tid, nil); err != nil {
		t.Fatal(err)
	}
	e.Svc.ProcessWorkflowJobs(ctx, 10)
	e.Store.Pool.QueryRow(ctx, `SELECT last_error FROM workflow_job WHERE kind='webhook' AND state='pending' ORDER BY id DESC LIMIT 1`).Scan(&lastErr)
	if lastErr == "" {
		t.Fatal("a loopback webhook was delivered without allowing private addresses")
	}
}
