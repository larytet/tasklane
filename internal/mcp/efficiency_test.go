package mcp_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/larytet/tasklane/internal/domain"
)

// teamDataset makes issues shaped like a team's work: most threads are short, some are long, a few are pathological (a huge pasted
// log). Text is words, not one repeated character, so sizes mean something. It returns the keys with their comment counts.
func teamDataset(t *testing.T, e *env) (map[string]int, string) {
	t.Helper()
	words := strings.Fields("the checkout page fails when the card requires a challenge and the retry button does nothing after the second attempt so we need to look at the session handling and the timeout in the payment provider callback before the release on friday")
	text := func(n, seed int) string {
		var b strings.Builder
		for i := 0; b.Len() <= n; i++ {
			b.WriteString(words[(i*7+seed)%len(words)])
			b.WriteByte(' ')
		}
		return strings.TrimSpace(b.String()[:n])
	}
	shapes := []struct {
		count, desc, comments, commentLen int
	}{
		{20, 600, 2, 250},  // ordinary issues
		{7, 900, 12, 350},  // a busy thread
		{2, 6000, 60, 400}, // a long-running one
	}
	out := map[string]int{}
	seed := 0
	var longest string
	for _, sh := range shapes {
		for i := 0; i < sh.count; i++ {
			seed++
			is, err := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: fmt.Sprintf("work item %d", seed), Description: text(sh.desc, seed)})
			if err != nil {
				t.Fatal(err)
			}
			for c := 0; c < sh.comments; c++ {
				if _, err := e.Svc.AddComment(ctx, e.Alice, is.Key, text(sh.commentLen, seed+c), false); err != nil {
					t.Fatal(err)
				}
			}
			out[is.Key] = sh.comments
			longest = is.Key
		}
	}
	// the pathological one: a pasted log of 40 000 characters in one comment, and a 30 000-character description
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Bug", Summary: "crash with a huge log", Description: text(30000, 3)})
	e.Svc.AddComment(ctx, e.Alice, is.Key, text(40000, 5), false)
	e.Svc.AddComment(ctx, e.Alice, is.Key, "short follow-up", false)
	out[is.Key] = 2
	_ = longest
	return out, is.Key
}

// MCP-18: what an agent reads to start work must fit a budget. On the team dataset the median context is a few hundred tokens and even
// the long threads stay within a few thousand; nothing reaches the hard cap. (Tokens are estimated as characters / 4.)
const (
	budgetMedianChars = 3000  // ~750 tokens
	budgetP95Chars    = 10000 // ~2,500 tokens
	budgetMaxChars    = 10000
)

func TestMCP18_ContextStaysWithinTheSizeBudgetOnATeamDataset(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("measure", "member", nil)
	a.init()
	keys, huge := teamDataset(t, e)
	var sizes []int
	for k := range keys {
		r := a.ok("get_issue_context", map[string]any{"key": k})
		sizes = append(sizes, len(r.Text))
		if strings.Contains(r.Text, "[truncated: the result was cut at") {
			t.Errorf("%s hit the hard result cap", k)
		}
	}
	sort.Ints(sizes)
	med, p95, max := sizes[len(sizes)/2], sizes[len(sizes)*95/100], sizes[len(sizes)-1]
	t.Logf("%d issues: median %d, p95 %d, max %d characters (~%d, ~%d, ~%d tokens)", len(sizes), med, p95, max, med/4, p95/4, max/4)
	if med > budgetMedianChars || p95 > budgetP95Chars || max > budgetMaxChars {
		t.Fatalf("the context is over budget: median %d (limit %d), p95 %d (limit %d), max %d (limit %d)", med, budgetMedianChars, p95, budgetP95Chars, max, budgetMaxChars)
	}
	_ = huge
}

// Long threads collapse as they do in the ticket view: the newest comments in full, the earlier ones counted and left to list_comments.
func TestMCP18_LongThreadsCollapseAndTheRestIsReachable(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("reader", "member", nil)
	a.init()
	keys, _ := teamDataset(t, e)
	var long string
	for k, n := range keys {
		if n == 60 {
			long = k
		}
	}
	r := a.ok("get_issue_context", map[string]any{"key": long})
	cs := r.Data["comments"].([]any)
	if len(cs) != 10 || !strings.Contains(r.Data["earlier_comments"].(string), "50 earlier comments are collapsed") {
		t.Fatalf("%d comments, %v", len(cs), r.Data["earlier_comments"])
	}
	if got := len(a.ok("get_issue_context", map[string]any{"key": long, "comments": 30}).Data["comments"].([]any)); got != 30 {
		t.Fatalf("%d", got)
	}
	if got := len(a.ok("get_issue_context", map[string]any{"key": long, "comments": 0}).Data["comments"].([]any)); got != 0 {
		t.Fatalf("%d", got)
	}
	// the earlier ones are one page at a time, with a cursor
	pg := a.ok("list_comments", map[string]any{"key": long, "limit": 15})
	if len(pg.Data["comments"].([]any)) != 15 || pg.Data["next_cursor"] == nil || pg.Data["total"].(float64) != 60 {
		t.Fatalf("%v", pg.Data)
	}
	// a short thread is not collapsed
	for k, n := range keys {
		if n == 2 {
			if r := a.ok("get_issue_context", map[string]any{"key": k}); r.Data["earlier_comments"] != nil {
				t.Fatalf("a two-comment thread was collapsed")
			}
			break
		}
	}
}

// A pasted log or a huge description cannot fill the context: each text block is cut, says so and how long it was, and the full text
// is one argument away. The cut is on a character boundary.
func TestMCP18_OversizedTextIsCutMarkedAndRetrievable(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("reader", "member", nil)
	a.init()
	_, huge := teamDataset(t, e)
	r := a.ok("get_issue_context", map[string]any{"key": huge})
	desc := r.Data["description"].(map[string]any)
	if desc["truncated"] != true || desc["full_chars"].(float64) != 30000 || len([]rune(desc["text"].(string))) != 2*defaultMaxTextForTest || desc["hint"] == nil {
		t.Fatalf("the description: truncated=%v full=%v len=%d", desc["truncated"], desc["full_chars"], len([]rune(desc["text"].(string))))
	}
	cs := r.Data["comments"].([]any)
	big := cs[0].(map[string]any)
	if big["truncated"] != true || big["full_chars"].(float64) != 40000 {
		t.Fatalf("%v", big["full_chars"])
	}
	if short := cs[1].(map[string]any); short["truncated"] != nil || short["text"] != "short follow-up" {
		t.Fatalf("a short comment was marked: %v", short)
	}
	if len(r.Text) > 8000 {
		t.Fatalf("the huge issue's context is %d characters", len(r.Text))
	}
	// more of it, on request, up to a ceiling
	more := a.ok("get_issue_context", map[string]any{"key": huge, "max_text": 6000})
	if got := len([]rune(more.Data["comments"].([]any)[0].(map[string]any)["text"].(string))); got != 6000 {
		t.Fatalf("with max_text 6000 the comment was %d characters", got)
	}
	// the ceiling: one comment at a time, however much is asked for
	one := a.ok("list_comments", map[string]any{"key": huge, "limit": 1, "max_text": 5000000})
	cm := one.Data["comments"].([]any)[0].(map[string]any)
	if got := len([]rune(cm["text"].(string))); got != 20000 || cm["truncated"] != true || cm["full_chars"].(float64) != 40000 {
		t.Fatalf("a huge max_text was honoured or the cut is not marked: %d characters", got)
	}
	// the same limit applies to get_issue and list_comments
	gi := a.ok("get_issue", map[string]any{"key": huge})
	if gi.Data["description"].(map[string]any)["truncated"] != true {
		t.Fatal("get_issue returned the whole description")
	}
	lc := a.ok("list_comments", map[string]any{"key": huge, "limit": 1})
	if lc.Data["comments"].([]any)[0].(map[string]any)["truncated"] != true {
		t.Fatal("list_comments returned the whole comment")
	}
	// a cut never splits a character
	is, _ := e.Svc.CreateIssue(ctx, e.Alice, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "unicode", Description: strings.Repeat("日本語🙂", 2000)})
	u := a.ok("get_issue", map[string]any{"key": is.Key, "max_text": 500})
	txt := u.Data["description"].(map[string]any)["text"].(string)
	if !utf8.ValidString(txt) || len([]rune(txt)) != 1000 {
		t.Fatalf("valid=%v runes=%d", utf8.ValidString(txt), len([]rune(txt)))
	}
}

const defaultMaxTextForTest = 1500

// The hard cap on a result is a cap on everything sent: when the text is cut, the structured copy of the same data is not sent whole.
func TestMCP11_ACappedResultDoesNotSendItsStructuredCopyAround(t *testing.T) {
	e := newEnv(t)
	e.m.MaxResultChars = 3000
	a := e.newAgent("capped", "member", nil)
	a.init()
	_, huge := teamDataset(t, e)
	r := a.rpc("tools/call", map[string]any{"name": "get_issue_context", "arguments": map[string]any{"key": huge, "max_text": 20000}})["result"].(map[string]any)
	text := r["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "[truncated: the result was cut at 3000") || len(text) > 3200 {
		t.Fatalf("the text was %d characters", len(text))
	}
	if r["structuredContent"] != nil {
		t.Fatal("the structured copy of a capped result was sent whole")
	}
	// a result under the cap carries both, as before
	if r := a.rpc("tools/call", map[string]any{"name": "list_projects", "arguments": map[string]any{}})["result"].(map[string]any); r["structuredContent"] == nil {
		t.Fatal("a normal result lost its structured content")
	}
}

// Search results are a compact page, not documents: twenty rows by default, a few hundred characters each at most.
func TestMCP18_SearchResultsAreACompactPage(t *testing.T) {
	e := newEnv(t)
	a := e.newAgent("searcher", "member", nil)
	a.init()
	teamDataset(t, e)
	r := a.ok("search_issues", map[string]any{"q": "project = ABC ORDER BY updated DESC"})
	rows := r.Data["issues"].([]any)
	if len(rows) != 20 || r.Data["next_cursor"] == nil {
		t.Fatalf("%d rows", len(rows))
	}
	if len(r.Text) > 8000 {
		t.Fatalf("a page of 20 search results is %d characters", len(r.Text))
	}
	for _, x := range rows {
		if _, has := x.(map[string]any)["description"]; has {
			t.Fatal("a search row carries a description")
		}
	}
}

// When the newest comments do not all fit the result cap, the answer is fitted, not cut: the oldest of them are collapsed too and
// counted, the structured data is still sent, and the text is valid JSON that ends where it should.
func TestMCP18_AnAnswerThatDoesNotFitIsFittedNotCutInTheMiddle(t *testing.T) {
	e := newEnv(t)
	e.m.MaxResultChars = 6000
	a := e.newAgent("fitted", "member", nil)
	a.init()
	keys, _ := teamDataset(t, e)
	var long string
	for k, n := range keys {
		if n == 60 {
			long = k
		}
	}
	r := a.ok("get_issue_context", map[string]any{"key": long, "comments": 30})
	if len(r.Text) > 6000 || strings.Contains(r.Text, "[truncated: the result was cut") {
		t.Fatalf("%d characters, cut=%v", len(r.Text), strings.Contains(r.Text, "[truncated"))
	}
	cs := r.Data["comments"].([]any)
	if len(cs) == 0 || len(cs) >= 30 {
		t.Fatalf("%d comments shown", len(cs))
	}
	if want := fmt.Sprintf("%d earlier comments are collapsed", 60-len(cs)); !strings.Contains(r.Data["earlier_comments"].(string), want) {
		t.Fatalf("%v (want %q)", r.Data["earlier_comments"], want)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(r.Text), &back); err != nil {
		t.Fatalf("the text is not valid JSON: %v", err)
	}
	// a page of comments ends early so that it fits, and the cursor continues from where it ended without skipping or repeating any
	seen, cursor := 0, ""
	for page := 0; page < 40; page++ {
		args := map[string]any{"key": long, "limit": 30, "max_text": 1500}
		if cursor != "" {
			args["cursor"] = cursor
		}
		p := a.ok("list_comments", args)
		if len(p.Text) > 6000 {
			t.Fatalf("a page of %d characters", len(p.Text))
		}
		seen += len(p.Data["comments"].([]any))
		c, _ := p.Data["next_cursor"].(string)
		if c == "" {
			break
		}
		cursor = c
	}
	if seen != 60 {
		t.Fatalf("paging under the cap saw %d of 60 comments", seen)
	}
}
