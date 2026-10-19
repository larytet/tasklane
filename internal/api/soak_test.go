//go:build soak

package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

// TestLIVE09_Soak is the SPEC 21 campaign made to run for as long as asked (`make soak DURATION=30m`): rounds of simulated people saving into shared texts
// through the real HTTP server while a quarter of the acknowledgements are lost, each round judged by the reconciler (E1 lost acknowledged save, E2 a stale
// save replacing a newer change, E4 stored content that differs from what was saved, gaps in the sequence), alongside editing sessions that save server
// drafts and crash at random and are judged on recovery (E3). It reports what it counted and what the counts prove: with no error in N trials the rate of
// each class is below 3/N with 95% confidence (the rule of three), to be set against the targets of SPEC 21 (1 in 10^8 saves for E1 and E4, 1 in 10^9 for E2,
// 1 in 10^6 editing sessions for E3). A short run proves little: the report says how far it is from those numbers.
func TestLIVE09_Soak(t *testing.T) {
	dur := 2 * time.Minute
	if v := os.Getenv("SOAK_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
		dur = d
	}
	users := 24
	if v := os.Getenv("SOAK_USERS"); v != "" {
		fmt.Sscan(v, &users)
	}
	const perIssue = 3 // people sharing a text, so that saves race
	issuesPerRound := users / perIssue
	savesPerUser := 10

	e := testutil.New(t)
	e.Project()
	var dropped int64
	srv := httptest.NewServer(dropAcks((&api.Server{Svc: e.Svc, SessionTTL: 3600e9}).Handler(), 0.25, &dropped))
	defer srv.Close()
	var clients []*client
	var actors []domain.Actor
	for i := 0; i < users; i++ {
		u, err := e.Svc.CreateUser(testutil.Ctx(), fmt.Sprintf("soak%d@example.com", i), fmt.Sprintf("Soak %d", i), "password123", false)
		if err != nil {
			t.Fatal(err)
		}
		e.Svc.AddMember(testutil.Ctx(), e.Admin, "ABC", u.ID, "member")
		tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), u.ID, "soak")
		clients = append(clients, &client{t: t, base: srv.URL, token: tok})
		actors = append(actors, domain.Actor{ID: u.ID, Label: u.Name})
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
			return 0, nil, err
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out, nil
	}

	var saves, conflicts, e1, e2, e4, gaps, rounds int64
	var sessions, e3, draftWrites int64
	stop := make(chan struct{})
	var bg sync.WaitGroup

	// the editing sessions: type, save the server draft as the page does, lose some acknowledgements, crash at a random moment, recover
	for s := 0; s < 4; s++ {
		bg.Add(1)
		go func(s int) {
			defer bg.Done()
			rng := rand.New(rand.NewSource(int64(1000 + s)))
			who := actors[s%len(actors)]
			is, err := e.Svc.CreateIssue(testutil.Ctx(), who, domain.CreateIn{Project: "ABC", Summary: fmt.Sprintf("drafts %d", s), Description: "start"})
			if err != nil {
				t.Error(err)
				return
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				var typed string
				var acked []string // drafts whose save was acknowledged, oldest first
				var all []string
				steps := 3 + rng.Intn(12)
				crashAt := 1 + rng.Intn(steps)
				for i := 1; i <= crashAt; i++ {
					typed += fmt.Sprintf("word%d ", rng.Intn(1000))
					code, _, err := call(clients[s%len(clients)], "PUT", "/api/v1/issues/"+is.Key+"/drafts/description", map[string]any{"body": typed, "base_seq": 1})
					atomic.AddInt64(&draftWrites, 1)
					all = append(all, typed) // the server took it whether or not the acknowledgement came back (acknowledgements are lost after the commit)
					if err == nil && code/100 == 2 {
						acked = append(acked, typed)
					}
				}
				atomic.AddInt64(&sessions, 1)
				if len(acked) == 0 {
					continue
				}
				// the crash: the page is gone; a new one asks the server for the draft
				ds, err := e.Svc.GetDrafts(testutil.Ctx(), who, is.Key)
				var got string
				for _, d := range ds {
					if d.Field == "description" {
						got = d.Body
					}
				}
				// it must be the last draft the server took, which is at least as new as the last one acknowledged, and unaltered
				ok := err == nil && got == all[len(all)-1]
				if !ok {
					atomic.AddInt64(&e3, 1)
					t.Errorf("E3: a draft acknowledged for %s was not recovered: got %q, last saved %q", is.Key, got, all[len(all)-1])
				}
				e.Svc.DeleteDraft(testutil.Ctx(), who, is.Key, "description")
			}
		}(s)
	}

	end := time.Now().Add(dur)
	for time.Now().Before(end) {
		round := atomic.AddInt64(&rounds, 1)
		var keys []string
		for i := 0; i < issuesPerRound; i++ {
			is, err := e.Svc.CreateIssue(testutil.Ctx(), e.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprintf("soak round %d text %d", round, i), Description: "start"})
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, is.Key)
		}
		tokens := make([][]string, issuesPerRound)
		var tmu sync.Mutex
		var wg sync.WaitGroup
		for u := 0; u < users; u++ {
			wg.Add(1)
			go func(u int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(round*1000 + int64(u)))
				c, ix := clients[u], u/perIssue%issuesPerRound
				key := keys[ix]
				for n := 0; n < savesPerUser; n++ {
					token := fmt.Sprintf("[r%dn%02du%02d]", round, n, u)
					code, v, err := call(c, "GET", "/api/v1/issues/"+key+"/view", nil)
					if err != nil || code != 200 {
						t.Errorf("view: %v %d", err, code)
						return
					}
					iss := v["issue"].(map[string]any)
					body, seq := iss["description"].(string), int64(iss["description_seq"].(float64))
					saveID := fmt.Sprintf("soak-%d-%d-%d-%d", round, u, n, rng.Int63())
					acked := false
					for attempt := 0; attempt < 400 && !acked; attempt++ {
						if attempt > 0 {
							time.Sleep(time.Duration(rng.Intn(5+attempt*3)) * time.Millisecond)
						}
						code, out, err := call(c, "PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": body + "\n\n" + token, "base_seq": seq, "save_id": saveID})
						switch {
						case err != nil:
						case code == 200:
							acked = true
						case code == 409:
							atomic.AddInt64(&conflicts, 1)
							d := out["detail"].(map[string]any)
							body, seq = d["current"].(string), int64(d["current_seq"].(float64))
							saveID = fmt.Sprintf("soak-%d-%d-%d-%d", round, u, n, rng.Int63())
						default:
							t.Errorf("unexpected %d %v", code, out)
							return
						}
					}
					if !acked {
						t.Errorf("save %s was never acknowledged", token)
						return
					}
					tmu.Lock()
					tokens[ix] = append(tokens[ix], token)
					tmu.Unlock()
				}
			}(u)
		}
		wg.Wait()
		for i, key := range keys {
			vs, err := e.Svc.TextVersions(testutil.Ctx(), e.Alice, key, "description")
			if err != nil {
				t.Fatal(err)
			}
			a, b, c, gap := testutil.Reconcile(tokens[i], vs)
			atomic.AddInt64(&saves, int64(len(tokens[i])))
			atomic.AddInt64(&e1, int64(a))
			atomic.AddInt64(&e2, int64(b))
			atomic.AddInt64(&e4, int64(c))
			if gap {
				atomic.AddInt64(&gaps, 1)
			}
			if a+b+c != 0 || gap {
				t.Errorf("round %d %s: E1=%d E2=%d E4=%d gap=%v", round, key, a, b, c, gap)
			}
		}
	}
	close(stop)
	bg.Wait()

	bound := func(n int64) string {
		if n == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%.2e", 3/float64(n))
	}
	t.Logf("%d saves in %d rounds by %d people on shared texts: %d conflicts resolved, %d acknowledgements dropped", saves, rounds, users, conflicts, dropped)
	t.Logf("E1=%d E2=%d E4=%d gaps=%d: with no error in %d saves each rate is below %s with 95%% confidence (targets: E1 and E4 1e-08, E2 1e-09)", e1, e2, e4, gaps, saves, bound(saves))
	t.Logf("%d editing sessions (%d draft saves, crashes at random, a quarter of acknowledgements lost): E3=%d; rate below %s (target 1e-06)", sessions, draftWrites, e3, bound(sessions))
	t.Logf("the SPEC 21 sample sizes are 3e8 saves and 3e6 sessions: this run is %.4f%% and %.2f%% of them", 100*float64(saves)/3e8, 100*float64(sessions)/3e6)
	if e1+e2+e4+gaps+e3 != 0 {
		t.Fatalf("errors were counted")
	}
}
