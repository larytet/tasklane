package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/testutil"
)

func pct95(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)*95/100]
}

// Server-side latency on the 100,000-issue dataset (PERF-01, 03, 07, 10, 11). The handler is called in process, so the
// number is server time, which SPEC 21 budgets at 50 ms p95 (100 ms for the board, and for the 409 response).
func TestPERF_ServerSideLatencyAt100k(t *testing.T) {
	if os.Getenv("TASKLANE_PERF") == "" {
		t.Skip("set TASKLANE_PERF=1 (make perf)")
	}
	e := testutil.New(t)
	testutil.LoadScale(e)
	h := (&api.Server{Svc: e.Svc, SessionTTL: 3600e9, Search: &search.Postgres{DB: e.Store.Pool}}).Handler()
	tok, _ := e.Svc.CreateAPIToken(testutil.Ctx(), e.Alice.ID, "perf")
	do := func(method, path string, body any, hdr ...string) (int, []byte, time.Duration) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		r := httptest.NewRequest(method, path, rd)
		r.Header.Set("Authorization", "Bearer "+tok)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		s := time.Now()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes(), time.Since(s)
	}
	measure := func(name string, n int, budget time.Duration, fn func(i int) (int, time.Duration)) {
		var ds []time.Duration
		for i := 0; i < n; i++ {
			code, d := fn(i)
			if code >= 400 {
				t.Fatalf("%s: status %d", name, code)
			}
			ds = append(ds, d)
		}
		p := pct95(ds)
		t.Logf("%-52s p95 %8v  (budget %v)", name, p.Round(10*time.Microsecond), budget)
		if p > budget {
			t.Errorf("%s: p95 %v exceeds %v", name, p, budget)
		}
	}
	// warm the pool and plans
	for i := 0; i < 20; i++ {
		do("GET", "/api/v1/issues/PA-"+fmt.Sprint(100+i)+"/view", nil)
	}
	measure("PERF-01 open ticket (view bootstrap)", 300, 50*time.Millisecond, func(i int) (int, time.Duration) {
		c, _, d := do("GET", fmt.Sprintf("/api/v1/issues/P%c-%d/view", 'A'+i%5, 1+(i*37)%4000), nil)
		return c, d
	})
	measure("PERF-01 revalidation (ETag, 304)", 200, 50*time.Millisecond, func(i int) (int, time.Duration) {
		path := fmt.Sprintf("/api/v1/issues/PA-%d/view", 1+i%50)
		_, _, _ = do("GET", path, nil)
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		etag := w.Header().Get("ETag")
		r = httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("If-None-Match", etag)
		w = httptest.NewRecorder()
		s := time.Now()
		h.ServeHTTP(w, r)
		if w.Code != 304 {
			t.Fatalf("revalidation gave %d", w.Code)
		}
		return 200, time.Since(s)
	})
	measure("PERF-03 issue list page (project filter, 50 rows)", 200, 50*time.Millisecond, func(i int) (int, time.Duration) {
		c, _, d := do("GET", "/api/v1/issues?limit=50&q="+strings.ReplaceAll(fmt.Sprintf("project = P%c ORDER BY updated DESC", 'A'+i%5), " ", "%20"), nil)
		return c, d
	})
	measure("PERF-03 issue list page (empty query, member)", 100, 50*time.Millisecond, func(i int) (int, time.Duration) {
		c, _, d := do("GET", "/api/v1/issues?limit=50", nil)
		return c, d
	})
	measure("PERF-07 transition", 100, 50*time.Millisecond, func(i int) (int, time.Duration) {
		key := fmt.Sprintf("PB-%d", 1+i)
		_, body, _ := do("GET", "/api/v1/issues/"+key+"/view", nil)
		var v struct {
			Transitions []struct{ ID string }
		}
		json.Unmarshal(body, &v)
		c, _, d := do("POST", "/api/v1/issues/"+key+"/transitions", map[string]any{"transition": v.Transitions[0].ID})
		return c, d
	})
	measure("PERF-07 assign", 100, 50*time.Millisecond, func(i int) (int, time.Duration) {
		c, _, d := do("PATCH", fmt.Sprintf("/api/v1/issues/PC-%d", 1+i), map[string]any{"assignee": e.Alice.ID})
		return c, d
	})
	measure("PERF-07 comment", 100, 50*time.Millisecond, func(i int) (int, time.Duration) {
		c, _, d := do("POST", fmt.Sprintf("/api/v1/issues/PD-%d/comments", 1+i), map[string]any{"body": "looks good"})
		return c, d
	})
	// text saves on a normal description (PERF-10) and 409 responses for typical and large documents (PERF-11)
	var seq int64 = 1
	measure("PERF-10 save a description", 150, 50*time.Millisecond, func(i int) (int, time.Duration) {
		c, out, d := do("PUT", "/api/v1/issues/PE-1/text/description", map[string]any{"body": fmt.Sprintf("Steps %d\n\nExpected\n\nNotes", i), "base_seq": seq, "save_id": fmt.Sprint("perf-", i)})
		var r struct{ Seq int64 }
		json.Unmarshal(out, &r)
		seq = r.Seq
		return c, d
	})
	doc := func(paragraphs int) string {
		var b strings.Builder
		for i := 0; i < paragraphs; i++ {
			fmt.Fprintf(&b, "Paragraph %d: the quick brown fox jumps over the lazy dog and keeps running through the field.", i)
			if i < paragraphs-1 {
				b.WriteString("\n\n")
			}
		}
		return b.String()
	}
	for _, c := range []struct {
		name string
		n    int
		lim  time.Duration
	}{{"PERF-11 409 with merge, 100 KB document", 1100, 100 * time.Millisecond}, {"PERF-11 409 with merge, ~1 MB document", 10000, 100 * time.Millisecond}} {
		key := "PA-" + fmt.Sprint(200+c.n/1100)
		base := doc(c.n)
		_, out, _ := do("PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": base, "base_seq": 1})
		var r struct{ Seq int64 }
		json.Unmarshal(out, &r)
		// theirs edits the first and a middle paragraph, mine edits the last and a middle one: the whole document is the merge span
		theirs := strings.Replace(strings.Replace(base, "Paragraph 0:", "Paragraph zero:", 1), fmt.Sprintf("Paragraph %d:", c.n/2), "Paragraph middle (theirs):", 1)
		mine := strings.Replace(strings.Replace(base, fmt.Sprintf("Paragraph %d:", c.n-1), "Paragraph last (mine):", 1), fmt.Sprintf("Paragraph %d:", c.n/2), "Paragraph middle (mine):", 1)
		if code, _, _ := do("PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": theirs, "base_seq": r.Seq}); code != 200 {
			t.Fatalf("setup save: %d", code)
		}
		measure(c.name, 15, c.lim, func(i int) (int, time.Duration) {
			code, out, d := do("PUT", "/api/v1/issues/"+key+"/text/description", map[string]any{"body": mine, "base_seq": r.Seq})
			if code != 409 {
				t.Fatalf("expected 409, got %d", code)
			}
			var cf struct {
				Detail struct {
					Merge struct{ Conflicts []any }
				}
			}
			json.Unmarshal(out, &cf)
			if len(cf.Detail.Merge.Conflicts) != 1 { // only the middle paragraph conflicts; the two ends merge cleanly
				t.Fatalf("conflicts: %d", len(cf.Detail.Merge.Conflicts))
			}
			return 200, d
		})
	}
	_ = http.StatusOK
}
