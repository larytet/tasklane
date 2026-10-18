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

func getWith(t *testing.T, ts *httptest.Server, path, token string, hdr map[string]string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// UPG-08: the previous client is served, a client older than that is told to reload, and things that are not browser clients
// (no header: tokens, scripts, MCP) are never refused on that ground.
func TestUPG08_AnOutOfDateClientIsToldToReloadAndOthersAreNeverRefused(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Alice.ID, "skew")
	srv := &api.Server{Svc: e.Svc, SessionTTL: time.Hour, Version: "1.4.0", MinClient: 5}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	// no header: not a browser page
	if r, _ := getWith(t, ts, "/api/v1/system/status", tok, nil); r.StatusCode != 200 || r.Header.Get("X-Tasklane-Server") != "1.4.0" || r.Header.Get("X-Tasklane-Client-Min") != "5" {
		t.Fatalf("%d %v", r.StatusCode, r.Header)
	}
	// the current and the oldest accepted client
	for _, v := range []string{"5", "6", "99"} {
		if r, _ := getWith(t, ts, "/api/v1/system/status", tok, map[string]string{"X-Tasklane-Client": v}); r.StatusCode != 200 {
			t.Fatalf("client %s refused: %d", v, r.StatusCode)
		}
	}
	// older: 426 with the reload hint and a code the page recognises
	r, body := getWith(t, ts, "/api/v1/system/status", tok, map[string]string{"X-Tasklane-Client": "4"})
	if r.StatusCode != 426 || body["code"] != "client_too_old" || !strings.Contains(body["error"].(string), "reload") {
		t.Fatalf("%d %v", r.StatusCode, body)
	}
	if d, _ := body["detail"].(map[string]any); d == nil || d["min"].(float64) != 5 || d["reload"] != true {
		t.Fatalf("%v", body["detail"])
	}
	// a header that is not a number is not a client version
	if r, _ := getWith(t, ts, "/api/v1/system/status", tok, map[string]string{"X-Tasklane-Client": "abc"}); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
	// the page itself (not under /api) is always served, so a stale tab can be reloaded
	if resp, err := http.Get(ts.URL + "/healthz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v", err)
	}
	// the default accepts the previous client: version 1 now, so nothing a page sends today is refused
	ts2 := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: time.Hour}).Handler())
	defer ts2.Close()
	if r, _ := getWith(t, ts2, "/api/v1/system/status", tok, map[string]string{"X-Tasklane-Client": "1"}); r.StatusCode != 200 {
		t.Fatalf("%d", r.StatusCode)
	}
}

// UPG-10, PAUSE-04: while writes are held, a browser or API client gets 503 with Retry-After and a code the page recognises,
// reads continue, the status says so, and after the pause the same write goes through.
func TestUPG10_MaintenanceIsVisibleWritesAreRefusedWithRetryAfterAndResume(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	e.Clock.T = time.Now()
	old := domain.PauseAdmitWait
	domain.PauseAdmitWait = 100 * time.Millisecond
	defer func() { domain.PauseAdmitWait = old }()
	tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Alice.ID, "pause")
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: time.Hour}).Handler())
	defer ts.Close()
	if _, b := getWith(t, ts, "/api/v1/system/status", tok, nil); len(b["maintenance"].([]any)) != 0 {
		t.Fatalf("%v", b)
	}
	is, _ := e.Svc.CreateIssue(testutil.Ctx(), e.Alice, domain.CreateIn{Project: "ABC", Summary: "x"})
	lease, err := e.Svc.RequestPause(testutil.Ctx(), e.Admin, domain.PauseIn{Scope: "project:ABC", Owner: "test", Mode: "pause", HoldMS: 60000, DrainWaitMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	// a snapshot-mode job elsewhere holds nothing and is not announced
	if _, err := e.Svc.RequestPause(testutil.Ctx(), e.Admin, domain.PauseIn{Scope: "instance", Owner: "backup", Mode: "snapshot"}); err != nil {
		t.Fatal(err)
	}
	_, b := getWith(t, ts, "/api/v1/system/status", tok, nil)
	if m := b["maintenance"].([]any); len(m) != 1 || m[0].(map[string]any)["scope"] != "project:ABC" {
		t.Fatalf("%v", b["maintenance"])
	}
	w, wb := post(t, ts, "/api/v1/issues/"+is.Key+"/comments", map[string]any{"body": "held"}, map[string]string{"Authorization": "Bearer " + tok})
	if w.StatusCode != 503 || wb["code"] != "paused" || w.Header.Get("Retry-After") == "" {
		t.Fatalf("%d %v %v", w.StatusCode, wb, w.Header)
	}
	if r, _ := getWith(t, ts, "/api/v1/issues/"+is.Key+"/view", tok, nil); r.StatusCode != 200 {
		t.Fatalf("reads stopped: %d", r.StatusCode)
	}
	if err := e.Svc.ReleasePause(testutil.Ctx(), e.Admin, lease.ID); err != nil {
		t.Fatal(err)
	}
	if w, _ := post(t, ts, "/api/v1/issues/"+is.Key+"/comments", map[string]any{"body": "after"}, map[string]string{"Authorization": "Bearer " + tok}); w.StatusCode != 201 && w.StatusCode != 200 {
		t.Fatalf("after the pause: %d", w.StatusCode)
	}
}

// CHAOS-06: with the realtime layer switched off the server says so plainly, presence still works by plain requests, saves still go
// through REST, and a stale save is still caught by its version.
func TestCHAOS06_WithoutTheRealtimeLayerSavesPresenceAndConflictsStillWork(t *testing.T) {
	e := testutil.New(t)
	e.Project()
	is := e.Issue("no sockets", "original")
	ta, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Alice.ID, "a")
	tb, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Bob.ID, "b")
	ts := httptest.NewServer((&api.Server{Svc: e.Svc, SessionTTL: time.Hour}).Handler()) // no Hub
	defer ts.Close()
	auth := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	// the ticket and the socket answer 404 with a code a page recognises (and stops asking on)
	r, body := post(t, ts, "/api/v1/ws/ticket", nil, auth(ta))
	if r.StatusCode != 404 || body["code"] != "realtime_disabled" {
		t.Fatalf("%d %v", r.StatusCode, body)
	}
	if r, _ := getWith(t, ts, "/api/v1/ws", ta, nil); r.StatusCode != 404 {
		t.Fatalf("%d", r.StatusCode)
	}
	// presence by plain requests: Bob sees Alice is here
	if r, _ := post(t, ts, "/api/v1/issues/"+is.Key+"/presence", map[string]any{"field": "description"}, auth(ta)); r.StatusCode != 200 {
		t.Fatalf("presence: %d", r.StatusCode)
	}
	rp, _ := http.NewRequest("POST", ts.URL+"/api/v1/issues/"+is.Key+"/presence", strings.NewReader(`{"field":""}`))
	rp.Header.Set("Authorization", "Bearer "+tb)
	rp.Header.Set("Content-Type", "application/json")
	presResp, err := http.DefaultClient.Do(rp)
	if err != nil {
		t.Fatal(err)
	}
	var pres bytes.Buffer
	pres.ReadFrom(presResp.Body)
	presResp.Body.Close()
	if !strings.Contains(pres.String(), e.Alice.ID) || !strings.Contains(pres.String(), `"editing":"description"`) {
		t.Fatalf("Bob does not see Alice: %s", pres.String())
	}
	// saves by REST, and the conflict check by version, without any socket: Bob saves from the version he saw, then Alice does
	put := func(tok, text string, base int64, id string) (int, map[string]any) {
		b, _ := json.Marshal(map[string]any{"body": text, "base_seq": base, "save_id": id, "mode": "save"})
		req, _ := http.NewRequest("PUT", ts.URL+"/api/v1/issues/"+is.Key+"/text/description", strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	v, _ := e.Svc.View(testutil.Ctx(), e.Alice, is.Key)
	if code, _ := put(tb, "Bob's text", v.Issue.DescSeq, "b1"); code != 200 {
		t.Fatalf("%d", code)
	}
	code, out := put(ta, "Alice's text", v.Issue.DescSeq, "a1")
	if code != 409 || out["code"] != "text_conflict" {
		t.Fatalf("a stale save was not caught: %d %v", code, out)
	}
}
