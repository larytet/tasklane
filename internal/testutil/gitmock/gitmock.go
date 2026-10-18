// Package gitmock is the in-process stand-in for GitHub and GitLab (TESTPLAN 3.2): webhook payload and signature builders,
// and a REST API double with pagination, ETags, rate-limit responses and a request counter. The same contract checks
// run against the real services nightly.
package gitmock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ---------- webhook builders ----------

type Hook struct {
	Header http.Header
	Body   []byte
}

func GitHubHook(secret, event, delivery string, payload map[string]any) Hook {
	body, _ := json.Marshal(payload)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	h.Set("X-GitHub-Event", event)
	if delivery != "" {
		h.Set("X-GitHub-Delivery", delivery)
	}
	return Hook{h, body}
}

func GitLabHook(secret, event, uuid string, payload map[string]any) Hook {
	body, _ := json.Marshal(payload)
	h := http.Header{}
	h.Set("X-Gitlab-Token", secret)
	h.Set("X-Gitlab-Event", event)
	if uuid != "" {
		h.Set("X-Gitlab-Event-UUID", uuid)
	}
	return Hook{h, body}
}

type M = map[string]any

func GHPR(repo string, number int, title, body, branch, sha, state string, merged bool, updated time.Time) M {
	return M{"action": "edited", "repository": M{"full_name": repo}, "pull_request": M{
		"number": number, "title": title, "body": body, "state": state, "draft": state == "draft", "merged": merged,
		"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), "user": M{"login": "octo"},
		"head": M{"ref": branch, "sha": sha}, "base": M{"ref": "main"}, "updated_at": updated.UTC().Format(time.RFC3339)}}
}

func GHPush(repo, branch, after string, commits []M, ts time.Time) M {
	return M{"ref": "refs/heads/" + branch, "after": after, "repository": M{"full_name": repo, "html_url": "https://github.com/" + repo},
		"head_commit": M{"timestamp": ts.UTC().Format(time.RFC3339)}, "commits": commits}
}

func GHCommit(sha, msg, email string, ts time.Time) M {
	return M{"id": sha, "message": msg, "timestamp": ts.UTC().Format(time.RFC3339), "url": "https://github.com/x/y/commit/" + sha, "author": M{"name": "Octo Cat", "email": email, "username": "octo"}}
}

func GHCheck(repo, name, sha, status, conclusion string, at time.Time) M {
	return M{"repository": M{"full_name": repo}, "check_run": M{"id": 1, "name": name, "head_sha": sha, "status": status, "conclusion": conclusion,
		"html_url": "https://github.com/" + repo + "/runs/1", "completed_at": at.UTC().Format(time.RFC3339), "started_at": at.UTC().Format(time.RFC3339)}}
}

func GHDeploy(repo string, id int, env, sha, state string, at time.Time) M {
	return M{"repository": M{"full_name": repo}, "deployment": M{"id": id, "sha": sha, "environment": env, "ref": "main"},
		"deployment_status": M{"state": state, "environment": env, "updated_at": at.UTC().Format(time.RFC3339)}}
}

func GLMR(repo string, iid int, title, desc, branch, sha, state string, updated time.Time) M {
	return M{"project": M{"path_with_namespace": repo, "web_url": "https://gitlab.example/" + repo}, "user": M{"username": "tanuki", "email": "tanuki@example.com"},
		"object_attributes": M{"iid": iid, "title": title, "description": desc, "state": state, "source_branch": branch, "target_branch": "main",
			"url": fmt.Sprintf("https://gitlab.example/%s/-/merge_requests/%d", repo, iid), "last_commit": M{"id": sha}, "updated_at": updated.UTC().Format("2006-01-02 15:04:05 UTC"), "action": "update"}}
}

func GLPipeline(repo string, id int, sha, ref, status string, at time.Time) M {
	return M{"project": M{"path_with_namespace": repo, "web_url": "https://gitlab.example/" + repo}, "object_attributes": M{"id": id, "sha": sha, "ref": ref, "status": status, "finished_at": at.UTC().Format("2006-01-02 15:04:05 UTC")}}
}

// ---------- REST API double ----------

type API struct {
	*httptest.Server
	mu       sync.Mutex
	Provider string
	Token    string
	// Data served; tests fill these.
	PRs, Branches, Commits, Checks, Pipelines []M
	PerPage                                   int
	reqs                                      []string
	limit                                     []limitRule
	notModified                               bool
	etag                                      string
	Hits                                      int
}

type limitRule struct {
	n      int
	status int
	hdr    map[string]string
}

func New(provider, token string) *API {
	a := &API{Provider: provider, Token: token, PerPage: 2, etag: `"v1"`}
	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	return a
}

// RateLimit makes the next n requests answer with the given status and headers.
func (a *API) RateLimit(n, status int, hdr map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.limit = append(a.limit, limitRule{n, status, hdr})
}

func (a *API) SetETag(e string) { a.mu.Lock(); a.etag = e; a.mu.Unlock() }

func (a *API) Requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.reqs...)
}

func (a *API) page(w http.ResponseWriter, r *http.Request, items []M) {
	pp := a.PerPage
	pageNo := 1
	fmt.Sscanf(r.URL.Query().Get("page"), "%d", &pageNo)
	lo, hi := (pageNo-1)*pp, pageNo*pp
	if lo > len(items) {
		lo = len(items)
	}
	if hi > len(items) {
		hi = len(items)
	}
	if hi < len(items) {
		next := *r.URL
		q := next.Query()
		q.Set("page", fmt.Sprint(pageNo+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s>; rel="next"`, r.Host, next.RequestURI()))
	}
	w.Header().Set("ETag", a.etag)
	json.NewEncoder(w).Encode(items[lo:hi])
}

func (a *API) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.Hits++
	a.reqs = append(a.reqs, r.Method+" "+r.URL.RequestURI())
	for i := range a.limit {
		if a.limit[i].n > 0 {
			a.limit[i].n--
			for k, v := range a.limit[i].hdr {
				w.Header().Set(k, v)
			}
			a.mu.Unlock()
			w.WriteHeader(a.limit[i].status)
			return
		}
	}
	etag := a.etag
	a.mu.Unlock()
	auth := r.Header.Get("Authorization") == "Bearer "+a.Token
	if a.Provider == "gitlab" {
		auth = r.Header.Get("PRIVATE-TOKEN") == a.Token
	}
	if !auth {
		w.WriteHeader(401)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(405) // the integration is read-only
		return
	}
	if r.Header.Get("If-None-Match") == etag && strings.Contains(r.URL.Path, "/pulls") || r.Header.Get("If-None-Match") == etag && strings.Contains(r.URL.Path, "/merge_requests") {
		w.WriteHeader(304)
		return
	}
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/pulls") || strings.HasSuffix(p, "/merge_requests"):
		a.page(w, r, a.PRs)
	case strings.HasSuffix(p, "/branches"):
		a.page(w, r, a.Branches)
	case strings.HasSuffix(p, "/commits"):
		a.page(w, r, a.Commits)
	case strings.HasSuffix(p, "/pipelines"):
		a.page(w, r, a.Pipelines)
	case strings.Contains(p, "/check-runs"):
		json.NewEncoder(w).Encode(M{"check_runs": a.Checks})
	default:
		w.WriteHeader(404)
	}
}
