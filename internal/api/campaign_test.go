package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// dropAcks is the fault injector: after the real handler has run and committed, with some probability it
// closes the connection without sending the response, which is exactly "the node died after the commit but
// before the acknowledgement" (DUR-01) and a lost packet (CHAOS-03) as the client sees them.
func dropAcks(next http.Handler, p float64, dropped *int64) http.Handler {
	var mu sync.Mutex
	rng := rand.New(rand.NewSource(7))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || !(strings.Contains(r.URL.Path, "/text/") || strings.Contains(r.URL.Path, "/drafts/")) {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		mu.Lock()
		drop := rng.Float64() < p
		mu.Unlock()
		if drop {
			atomic.AddInt64(dropped, 1)
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					c.Close()
					return
				}
			}
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	})
}

type opResult struct {
	token string
	acked bool
}

// TestLIVE09_MiniCampaign: simulated users save concurrently through the real HTTP server while a share of
// acknowledgements is lost. At the end a reconciler checks the E1, E2 and E4 definitions of SPEC 21:
//
//	E1 an acknowledged save is missing from the final text
//	E2 a save replaced someone else's newer change without a conflict (a token disappeared between versions)
//	E4 stored content differs from what was saved (a token appears twice, or text was altered)
//
// plus the invariants: gapless text_seq, every save applied exactly once.
func TestLIVE09_MiniCampaign(t *testing.T) {
	users, perUser := 12, 15
	if testing.Short() {
		users, perUser = 6, 6
	}
	e := testutil.New(t)
	e.Project()
	var dropped int64
	srv := httptest.NewServer(dropAcks((&api.Server{Svc: e.Svc, SessionTTL: 3600e9}).Handler(), 0.25, &dropped))
	defer srv.Close()
	is := e.Issue("campaign", "start")
	// one token per simulated user
	var clients []*client
	for i := 0; i < users; i++ {
		u, err := e.Svc.CreateUser(testutil.Ctx(), fmt.Sprintf("sim%d@example.com", i), fmt.Sprintf("Sim %d", i), "password123", false)
		if err != nil {
			t.Fatal(err)
		}
		e.Svc.AddMember(testutil.Ctx(), e.Admin, "ABC", u.ID, "member")
		tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), u.ID, "sim")
		clients = append(clients, &client{t: t, base: srv.URL, token: tok})
	}
	httpc := &http.Client{Timeout: 10 * time.Second}
	call := func(c *client, method, path string, body any) (int, map[string]any, error) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, c.base+path, rd)
		req.Header.Set("Authorization", "Bearer "+c.token)
		resp, err := httpc.Do(req)
		if err != nil {
			return 0, nil, err // a lost acknowledgement looks like this to the client
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out, nil
	}
	results := make(chan opResult, users*perUser)
	var wg sync.WaitGroup
	var conflicts, retries, worst int64
	for u := 0; u < users; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(u)))
			c := clients[u]
			for n := 0; n < perUser; n++ {
				token := fmt.Sprintf("[u%02dn%02d]", u, n)
				time.Sleep(time.Duration(rng.Intn(15)) * time.Millisecond)
				// read the current text, then save "current + my block" with compare-and-set
				code, v, err := call(c, "GET", "/api/v1/issues/"+is.Key+"/view", nil)
				if err != nil || code != 200 {
					t.Errorf("view: %v %d", err, code)
					return
				}
				iss := v["issue"].(map[string]any)
				body, seq := iss["description"].(string), int64(iss["description_seq"].(float64))
				saveID := fmt.Sprintf("sim-%d-%d-%d", u, n, rng.Int63())
				acked := false
				for attempt := 0; attempt < 400 && !acked; attempt++ {
					if attempt > 0 { // jittered backoff, as a real client has (a person takes time to resolve a dialog)
						time.Sleep(time.Duration(rng.Intn(5+attempt*3)) * time.Millisecond)
					}
					if attempt > int(atomic.LoadInt64(&worst)) {
						atomic.StoreInt64(&worst, int64(attempt))
					}
					code, out, err := call(c, "PUT", "/api/v1/issues/"+is.Key+"/text/description",
						map[string]any{"body": body + "\n\n" + token, "base_seq": seq, "save_id": saveID})
					switch {
					case err != nil: // no acknowledgement: retry the very same save id and base
						atomic.AddInt64(&retries, 1)
					case code == 200:
						acked = true
					case code == 409: // someone saved first: re-read what the dialog would show and save on top of it
						atomic.AddInt64(&conflicts, 1)
						d := out["detail"].(map[string]any)
						body, seq = d["current"].(string), int64(d["current_seq"].(float64))
						saveID = fmt.Sprintf("sim-%d-%d-%d", u, n, rng.Int63()) // a new decision is a new save
					default:
						t.Errorf("unexpected %d %v", code, out)
						return
					}
				}
				results <- opResult{token, acked}
			}
		}(u)
	}
	wg.Wait()
	close(results)
	// ---- reconciler
	var tokens []string
	for r := range results {
		if !r.acked {
			t.Errorf("save %s never acknowledged", r.token)
		}
		tokens = append(tokens, r.token)
	}
	vs, err := e.Svc.TextVersions(testutil.Ctx(), e.Alice, is.Key, "description")
	if err != nil {
		t.Fatal(err)
	}
	e1, e2, e4, gap := testutil.Reconcile(tokens, vs)
	if gap {
		t.Fatal("text_seq has a gap or the final seq is wrong: a save was applied zero or two times")
	}
	t.Logf("%d saves by %d users: %d conflicts, %d acknowledgements dropped, %d same-id retries, worst save needed %d attempts; E1=%d E2=%d E4=%d", len(tokens), users, conflicts, dropped, retries, worst+1, e1, e2, e4)
	if dropped == 0 || conflicts == 0 {
		t.Fatal("the campaign did not exercise both faults and conflicts")
	}
}

// LIVE-12: the measurement must detect every injected error class, or its zeros mean nothing.
func TestLIVE12_ReconcilerDetectsInjectedErrors(t *testing.T) {
	good := []domain.TextVersion{{Seq: 3, Body: "s\n\n[a]\n\n[b]"}, {Seq: 2, Body: "s\n\n[a]"}, {Seq: 1, Body: "s"}}
	toks := []string{"[a]", "[b]"}
	if e1, e2, e4, gap := testutil.Reconcile(toks, good); e1+e2+e4 != 0 || gap {
		t.Fatalf("false positive on a clean history: %d %d %d %v", e1, e2, e4, gap)
	}
	// E1: an acknowledged save is absent from the final text
	if e1, _, _, _ := testutil.Reconcile([]string{"[a]", "[b]", "[c]"}, good); e1 != 1 {
		t.Fatalf("E1 not detected: %d", e1)
	}
	// E2: a stale save was accepted, so the newest version dropped content of the one before it
	stale := []domain.TextVersion{{Seq: 3, Body: "s\n\n[b]"}, {Seq: 2, Body: "s\n\n[a]"}, {Seq: 1, Body: "s"}}
	if _, e2, _, _ := testutil.Reconcile([]string{"[b]"}, stale); e2 != 1 {
		t.Fatalf("E2 not detected: %d", e2)
	}
	// E4: stored content mutated (a save applied twice shows as a duplicate)
	dup := []domain.TextVersion{{Seq: 3, Body: "s\n\n[a]\n\n[a]"}, {Seq: 2, Body: "s\n\n[a]"}, {Seq: 1, Body: "s"}}
	if _, _, e4, _ := testutil.Reconcile([]string{"[a]"}, dup); e4 != 1 {
		t.Fatalf("E4 not detected: %d", e4)
	}
	// a missing version in the chain, and a final seq that does not match the number of acknowledged saves
	holes := []domain.TextVersion{{Seq: 3, Body: "s\n\n[a]\n\n[b]"}, {Seq: 1, Body: "s"}}
	if _, _, _, gap := testutil.Reconcile(toks, holes); !gap {
		t.Fatal("sequence gap not detected")
	}
	if _, _, _, gap := testutil.Reconcile([]string{"[a]"}, good); !gap {
		t.Fatal("a save applied more often than acknowledged was not detected")
	}
}
