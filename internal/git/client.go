package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client reads a provider's REST API for backfill and reconcile. It respects the provider's rate limits (Retry-After and
// the primary limit headers), sends conditional requests, and follows pagination. Reading only ever sends GET requests; the
// opt-in write actions are in write.go.
type Client struct {
	Provider string // github or gitlab
	BaseURL  string // https://api.github.com, https://gitlab.example.com
	Token    string
	HTTP     *http.Client
	// MaxWait is the longest a rate limit may make us sleep before we give up and report it.
	MaxWait time.Duration
	Sleep   func(time.Duration)
	Now     func() time.Time
	// ETags remembers validators per URL between runs (the caller persists it).
	ETags map[string]string
	// Requests counts calls, for budgets and tests.
	Requests int
}

// ErrRateLimited is returned when the provider asks us to wait longer than MaxWait.
type ErrRateLimited struct{ Until time.Time }

func (e *ErrRateLimited) Error() string {
	return "the provider's rate limit is used up until " + e.Until.UTC().Format(time.RFC3339)
}

var ErrNotModified = errors.New("not modified")

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// get fetches one URL, waiting out rate limits within MaxWait and retrying server errors a few times. A 304 returns
// ErrNotModified when a validator was sent.
func (c *Client) get(ctx context.Context, u string) ([]byte, http.Header, error) {
	max := c.MaxWait
	if max == 0 {
		max = 30 * time.Second
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, nil, err
		}
		if c.Provider == "github" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		} else {
			req.Header.Set("PRIVATE-TOKEN", c.Token)
		}
		req.Header.Set("User-Agent", "tasklane")
		if et := c.ETags[u]; et != "" {
			req.Header.Set("If-None-Match", et)
		}
		c.Requests++
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, nil, err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if rerr != nil {
			return nil, nil, rerr
		}
		switch {
		case resp.StatusCode == 304:
			return nil, resp.Header, ErrNotModified
		case resp.StatusCode/100 == 2:
			if et := resp.Header.Get("ETag"); et != "" && c.ETags != nil {
				c.ETags[u] = et
			}
			return body, resp.Header, nil
		case resp.StatusCode == 429 || (resp.StatusCode == 403 && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0")):
			until := c.waitUntil(resp.Header)
			if d := until.Sub(c.now()); d > max || attempt >= 4 {
				return nil, nil, &ErrRateLimited{Until: until}
			} else if d > 0 {
				c.sleep(d)
			}
			continue
		case resp.StatusCode >= 500 && attempt < 3:
			c.sleep(time.Duration(200<<attempt) * time.Millisecond)
			continue
		case resp.StatusCode == 401:
			return nil, nil, fmt.Errorf("%w (%s %s: 401)", ErrBadCredentials, c.Provider, safeURL(u))
		default:
			return nil, nil, fmt.Errorf("%s %s: %d %s", c.Provider, safeURL(u), resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 200)])))
		}
	}
}

func safeURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "?"
	}
	return p.Path
}

func (c *Client) waitUntil(h http.Header) time.Time {
	if ra := h.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil {
			return c.now().Add(time.Duration(n) * time.Second)
		}
	}
	if rs := h.Get("X-RateLimit-Reset"); rs != "" {
		if n, err := strconv.ParseInt(rs, 10, 64); err == nil {
			return time.Unix(n, 0)
		}
	}
	return c.now().Add(time.Minute)
}

func nextLink(h http.Header) string {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		if strings.Contains(part, `rel="next"`) {
			if i, j := strings.Index(part, "<"), strings.Index(part, ">"); i >= 0 && j > i {
				return part[i+1 : j]
			}
		}
	}
	return ""
}

// pages walks every page of a list endpoint, calling fn for each decoded object, up to maxItems.
func (c *Client) pages(ctx context.Context, u string, maxItems int, fn func(map[string]any) bool) error {
	n := 0
	for u != "" {
		body, h, err := c.get(ctx, u)
		if errors.Is(err, ErrNotModified) {
			return nil // the first page has not changed since the stored validator: nothing new to read
		}
		if err != nil {
			return err
		}
		var items []map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			return fmt.Errorf("unexpected response from %s: %v", safeURL(u), err)
		}
		for _, it := range items {
			if n++; n > maxItems || !fn(it) {
				return nil
			}
		}
		u = nextLink(h) // both providers send a Link header with rel="next"
	}
	return nil
}

// Backfill reads recent pull or merge requests, branches and commits of one repository (since the given time) and
// returns them as normalized events. Builds for open pull requests are fetched per head commit. Only metadata is read.
func (c *Client) Backfill(ctx context.Context, repo string, since time.Time, maxItems int) ([]Event, error) {
	if maxItems <= 0 {
		maxItems = 500
	}
	var evs []Event
	base := strings.TrimSuffix(c.BaseURL, "/")
	switch c.Provider {
	case "github":
		var prs []Event
		err := c.pages(ctx, fmt.Sprintf("%s/repos/%s/pulls?state=all&sort=updated&direction=desc&per_page=100", base, repo), maxItems, func(m map[string]any) bool {
			e := ghPR(repo, m, time.Time{})
			if !since.IsZero() && !e.At.IsZero() && e.At.Before(since) {
				return false // sorted by update time, newest first: everything after this is older
			}
			prs = append(prs, e)
			return true
		})
		if err != nil {
			return evs, err
		}
		evs = append(evs, prs...)
		err = c.pages(ctx, fmt.Sprintf("%s/repos/%s/branches?per_page=100", base, repo), maxItems, func(m map[string]any) bool {
			evs = append(evs, Event{Kind: KindBranch, Repo: repo, ExtID: str(m, "name"), Title: str(m, "name"), Branch: str(m, "name"), SHA: str(obj(m, "commit"), "sha"), State: "open"})
			return true
		})
		if err != nil {
			return evs, err
		}
		cu := fmt.Sprintf("%s/repos/%s/commits?per_page=100", base, repo)
		if !since.IsZero() {
			cu += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
		}
		err = c.pages(ctx, cu, maxItems, func(m map[string]any) bool {
			cm := obj(m, "commit")
			subj, rest := firstLine(str(cm, "message"))
			au := obj(cm, "author")
			evs = append(evs, Event{Kind: KindCommit, Repo: repo, ExtID: str(m, "sha"), Title: trunc(subj, 300), Body: rest, Author: str(au, "name"), Email: str(au, "email"),
				URL: str(m, "html_url"), SHA: str(m, "sha"), At: ts(ghTime, str(au, "date"))})
			return true
		})
		if err != nil {
			return evs, err
		}
		for _, p := range prs {
			if p.State == "merged" || p.State == "closed" || p.SHA == "" {
				continue
			}
			body, _, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/commits/%s/check-runs", base, repo, p.SHA))
			if err != nil {
				return evs, err
			}
			var cr struct {
				CheckRuns []map[string]any `json:"check_runs"`
			}
			if json.Unmarshal(body, &cr) == nil {
				for _, r := range cr.CheckRuns {
					raw, _ := json.Marshal(map[string]any{"check_run": r, "repository": map[string]any{"full_name": repo}})
					if b, err := (GitHub{}).Parse("check_run", raw); err == nil {
						evs = append(evs, b...)
					}
				}
			}
		}
	case "gitlab":
		pid := url.PathEscape(repo)
		mu := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests?state=all&order_by=updated_at&sort=desc&per_page=100", base, pid)
		if !since.IsZero() {
			mu += "&updated_after=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
		}
		err := c.pages(ctx, mu, maxItems, func(m map[string]any) bool {
			state := map[string]string{"opened": "open", "closed": "closed", "merged": "merged", "locked": "open"}[str(m, "state")]
			if state == "open" && (boolean(m, "draft") || boolean(m, "work_in_progress")) {
				state = "draft"
			}
			evs = append(evs, Event{Kind: KindPR, Repo: repo, ExtID: fmt.Sprint(num(m, "iid")), Title: trunc(str(m, "title"), 500), Body: str(m, "description"), State: state,
				Author: str(obj(m, "author"), "username"), URL: str(m, "web_url"), SHA: str(m, "sha"), Branch: str(m, "source_branch"), Base: str(m, "target_branch"), At: ts(glTime, str(m, "updated_at"))})
			return true
		})
		if err != nil {
			return evs, err
		}
		err = c.pages(ctx, fmt.Sprintf("%s/api/v4/projects/%s/repository/branches?per_page=100", base, pid), maxItems, func(m map[string]any) bool {
			evs = append(evs, Event{Kind: KindBranch, Repo: repo, ExtID: str(m, "name"), Title: str(m, "name"), Branch: str(m, "name"), SHA: str(obj(m, "commit"), "id"), State: "open"})
			return true
		})
		if err != nil {
			return evs, err
		}
		cu := fmt.Sprintf("%s/api/v4/projects/%s/repository/commits?per_page=100", base, pid)
		if !since.IsZero() {
			cu += "&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
		}
		err = c.pages(ctx, cu, maxItems, func(m map[string]any) bool {
			subj, rest := firstLine(str(m, "message"))
			evs = append(evs, Event{Kind: KindCommit, Repo: repo, ExtID: str(m, "id"), Title: trunc(subj, 300), Body: rest, Author: str(m, "author_name"), Email: str(m, "author_email"),
				URL: str(m, "web_url"), SHA: str(m, "id"), At: ts(glTime, str(m, "created_at"))})
			return true
		})
		if err != nil {
			return evs, err
		}
		pu := fmt.Sprintf("%s/api/v4/projects/%s/pipelines?per_page=100&order_by=updated_at&sort=desc", base, pid)
		if !since.IsZero() {
			pu += "&updated_after=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
		}
		err = c.pages(ctx, pu, maxItems, func(m map[string]any) bool {
			state := map[string]string{"success": "passing", "failed": "failing", "canceled": "failing", "running": "running", "pending": "running", "created": "running"}[str(m, "status")]
			if state != "" {
				evs = append(evs, Event{Kind: KindBuild, Repo: repo, ExtID: "pipeline:" + fmt.Sprint(num(m, "id")), Title: "pipeline", State: state, SHA: str(m, "sha"), Branch: str(m, "ref"),
					URL: str(m, "web_url"), At: ts(glTime, str(m, "updated_at"))})
			}
			return true
		})
		if err != nil {
			return evs, err
		}
	default:
		return nil, errors.New("unknown provider")
	}
	return evs, nil
}

func decode(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("the payload is not JSON: %v", err)
	}
	return m, nil
}
