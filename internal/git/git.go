// Package git is the provider layer of the Git integration (SPEC 18.4): a normalized model of branches, commits, pull or
// merge requests, builds and deployments, webhook verification and parsing, and the REST backfill client. Providers are
// read-only; GitHub and GitLab sit behind one interface so others can be added without touching the rules or the UI.
package git

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kinds of normalized objects.
const (
	KindPR         = "pr"
	KindBranch     = "branch"
	KindCommit     = "commit"
	KindBuild      = "build"
	KindDeployment = "deployment"
)

// Event is one provider fact in the normalized shape. No source code or diffs: titles, subjects, authors, states, URLs.
type Event struct {
	Kind    string
	Repo    string // full name: owner/name or group/project
	ExtID   string // stable id inside the repo and kind (PR number, branch name, commit sha, check name + sha, deployment id)
	Title   string // PR title, commit subject, branch name, build name
	Body    string // searched for issue keys, then dropped (the PR description, the rest of a commit message): never stored
	State   string // pr: draft|open|merged|closed; build: running|passing|failing; deployment: pending|running|success|failure
	Review  string // pr: ""|approved|changes_requested
	Author  string // provider login or name
	Email   string
	URL     string
	SHA     string    // head or commit sha
	Branch  string    // pr head branch, branch name, or the branch a build ran on
	Base    string    // pr target branch
	Env     string    // deployments
	Deleted bool      // a branch that was deleted
	At      time.Time // the provider's own timestamp for this version of the object: later wins
	Extra   map[string]any
}

var ErrBadSignature = errors.New("webhook signature does not verify")

// Delivery is a verified webhook, ready to store and process.
type Delivery struct {
	ID    string // the provider's delivery id (or a content hash when it sends none)
	Event string // the provider's event name
}

// Provider is the interface every Git host implements.
type Provider interface {
	Name() string
	// Verify checks the webhook's authenticity against the connection secret and identifies the delivery.
	Verify(h http.Header, body []byte, secret string) (Delivery, error)
	// Parse turns a verified payload into normalized events. Unknown events yield none, not an error.
	Parse(event string, body []byte) ([]Event, error)
}

func ByName(name string) (Provider, bool) {
	switch name {
	case "github":
		return GitHub{}, true
	case "gitlab":
		return GitLab{}, true
	}
	return nil, false
}

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:16]) }

// firstLine splits a commit message into the subject and the rest.
func firstLine(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// MaxCommitsPerPush caps how many commits one push expands into; the rest are counted, not stored (SPEC 18.4).
const MaxCommitsPerPush = 100

func ts(layouts []string, s string) time.Time {
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func obj(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func arr(m map[string]any, k string) []any {
	v, _ := m[k].([]any)
	return v
}

func num(m map[string]any, k string) int64 {
	if f, ok := m[k].(float64); ok {
		return int64(f)
	}
	return 0
}

func boolean(m map[string]any, k string) bool { b, _ := m[k].(bool); return b }

func sigEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// ---------- GitHub ----------

type GitHub struct{}

func (GitHub) Name() string { return "github" }

func (GitHub) Verify(h http.Header, body []byte, secret string) (Delivery, error) {
	got := strings.TrimPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
	if secret == "" || got == "" || got == h.Get("X-Hub-Signature-256") { // a missing prefix means a different scheme
		return Delivery{}, ErrBadSignature
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	if !sigEqual(hex.EncodeToString(m.Sum(nil)), got) {
		return Delivery{}, ErrBadSignature
	}
	d := Delivery{ID: h.Get("X-GitHub-Delivery"), Event: h.Get("X-GitHub-Event")}
	if d.ID == "" {
		d.ID = "sha:" + hash(body)
	}
	if d.Event == "" {
		return Delivery{}, errors.New("missing X-GitHub-Event")
	}
	return d, nil
}

var ghTime = []string{time.RFC3339, "2006-01-02T15:04:05Z"}

func ghPR(repo string, pr map[string]any, at time.Time) Event {
	state := "open"
	switch {
	case boolean(pr, "merged") || str(pr, "merged_at") != "":
		state = "merged"
	case str(pr, "state") == "closed":
		state = "closed"
	case boolean(pr, "draft"):
		state = "draft"
	}
	head, base, user := obj(pr, "head"), obj(pr, "base"), obj(pr, "user")
	if t := ts(ghTime, str(pr, "updated_at")); !t.IsZero() {
		at = t
	}
	return Event{Kind: KindPR, Repo: repo, ExtID: fmt.Sprint(num(pr, "number")), Title: trunc(str(pr, "title"), 500), Body: str(pr, "body"), State: state,
		Author: str(user, "login"), URL: str(pr, "html_url"), SHA: str(head, "sha"), Branch: str(head, "ref"), Base: str(base, "ref"), At: at}
}

func (GitHub) Parse(event string, body []byte) ([]Event, error) {
	m, err := decode(body)
	if err != nil {
		return nil, err
	}
	repo := str(obj(m, "repository"), "full_name")
	now := time.Time{}
	switch event {
	case "pull_request":
		pr := obj(m, "pull_request")
		if pr == nil {
			return nil, errors.New("pull_request payload without a pull request")
		}
		return []Event{ghPR(repo, pr, now)}, nil
	case "pull_request_review":
		pr, rv := obj(m, "pull_request"), obj(m, "review")
		if pr == nil || rv == nil {
			return nil, errors.New("review payload without a review")
		}
		e := ghPR(repo, pr, now)
		switch strings.ToLower(str(rv, "state")) {
		case "approved":
			e.Review = "approved"
		case "changes_requested":
			e.Review = "changes_requested"
		default:
			return nil, nil // a plain comment changes nothing we show
		}
		if t := ts(ghTime, str(rv, "submitted_at")); !t.IsZero() && t.After(e.At) {
			e.At = t
		}
		e.Extra = map[string]any{"review_only": true}
		return []Event{e}, nil
	case "push":
		ref := str(m, "ref")
		if !strings.HasPrefix(ref, "refs/heads/") {
			return nil, nil // tags are not branches
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		at := ts(ghTime, str(obj(m, "head_commit"), "timestamp"))
		if boolean(m, "deleted") {
			return []Event{{Kind: KindBranch, Repo: repo, ExtID: branch, Title: branch, Branch: branch, Deleted: true, At: time.Now().UTC()}}, nil
		}
		evs := []Event{{Kind: KindBranch, Repo: repo, ExtID: branch, Title: branch, Branch: branch, SHA: str(m, "after"), State: "open", At: at,
			URL: strings.TrimSuffix(str(obj(m, "repository"), "html_url"), "/") + "/tree/" + branch}}
		cs := arr(m, "commits")
		for i, c := range cs {
			if i >= MaxCommitsPerPush {
				evs[0].Extra = map[string]any{"commits_not_expanded": len(cs) - MaxCommitsPerPush}
				break
			}
			cm, _ := c.(map[string]any)
			subj, rest := firstLine(str(cm, "message"))
			au := obj(cm, "author")
			name := str(au, "username")
			if name == "" {
				name = str(au, "name")
			}
			evs = append(evs, Event{Kind: KindCommit, Repo: repo, ExtID: str(cm, "id"), Title: trunc(subj, 300), Body: rest, Author: name, Email: str(au, "email"),
				URL: str(cm, "url"), SHA: str(cm, "id"), Branch: branch, At: ts(ghTime, str(cm, "timestamp"))})
		}
		return evs, nil
	case "check_run":
		cr := obj(m, "check_run")
		if cr == nil {
			return nil, errors.New("check_run payload without a check")
		}
		state := "running"
		if str(cr, "status") == "completed" {
			switch str(cr, "conclusion") {
			case "success", "neutral", "skipped":
				state = "passing"
			default:
				state = "failing"
			}
		}
		at := ts(ghTime, str(cr, "completed_at"))
		if at.IsZero() {
			at = ts(ghTime, str(cr, "started_at"))
		}
		sha := str(cr, "head_sha")
		return []Event{{Kind: KindBuild, Repo: repo, ExtID: str(cr, "name") + "@" + sha, Title: str(cr, "name"), State: state, URL: str(cr, "html_url"), SHA: sha, At: at}}, nil
	case "status":
		state := map[string]string{"success": "passing", "pending": "running", "failure": "failing", "error": "failing"}[str(m, "state")]
		if state == "" {
			return nil, nil
		}
		return []Event{{Kind: KindBuild, Repo: repo, ExtID: str(m, "context") + "@" + str(m, "sha"), Title: str(m, "context"), State: state, URL: str(m, "target_url"), SHA: str(m, "sha"),
			At: ts(ghTime, str(m, "updated_at"))}}, nil
	case "deployment_status":
		d, ds := obj(m, "deployment"), obj(m, "deployment_status")
		if d == nil || ds == nil {
			return nil, errors.New("deployment_status payload without a deployment")
		}
		state := map[string]string{"success": "success", "failure": "failure", "error": "failure", "in_progress": "running", "queued": "pending", "pending": "pending", "inactive": "inactive"}[str(ds, "state")]
		if state == "" {
			return nil, nil
		}
		env := str(ds, "environment")
		if env == "" {
			env = str(d, "environment")
		}
		return []Event{{Kind: KindDeployment, Repo: repo, ExtID: fmt.Sprint(num(d, "id")), Title: env, Env: env, State: state, SHA: str(d, "sha"), Branch: str(d, "ref"),
			URL: str(ds, "environment_url"), At: ts(ghTime, str(ds, "updated_at"))}}, nil
	}
	return nil, nil
}

// ---------- GitLab ----------

type GitLab struct{}

func (GitLab) Name() string { return "gitlab" }

func (GitLab) Verify(h http.Header, body []byte, secret string) (Delivery, error) {
	got := h.Get("X-Gitlab-Token")
	if secret == "" || got == "" || !sigEqual(got, secret) {
		return Delivery{}, ErrBadSignature
	}
	d := Delivery{ID: h.Get("X-Gitlab-Event-UUID"), Event: h.Get("X-Gitlab-Event")}
	if d.ID == "" { // older versions send no delivery id: a hash of the content makes a replay a duplicate
		d.ID = "sha:" + hash(body)
	}
	if d.Event == "" {
		return Delivery{}, errors.New("missing X-Gitlab-Event")
	}
	return d, nil
}

var glTime = []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339, "2006-01-02T15:04:05.000Z"}

func (GitLab) Parse(event string, body []byte) ([]Event, error) {
	m, err := decode(body)
	if err != nil {
		return nil, err
	}
	repo := str(obj(m, "project"), "path_with_namespace")
	switch event {
	case "Merge Request Hook":
		a := obj(m, "object_attributes")
		if a == nil {
			return nil, errors.New("merge request payload without attributes")
		}
		state := map[string]string{"opened": "open", "reopened": "open", "closed": "closed", "merged": "merged", "locked": "open"}[str(a, "state")]
		if state == "" {
			state = "open"
		}
		if state == "open" && (boolean(a, "draft") || boolean(a, "work_in_progress")) {
			state = "draft"
		}
		e := Event{Kind: KindPR, Repo: repo, ExtID: fmt.Sprint(num(a, "iid")), Title: trunc(str(a, "title"), 500), Body: str(a, "description"), State: state,
			Author: str(obj(m, "user"), "username"), Email: str(obj(m, "user"), "email"), URL: str(a, "url"), SHA: str(obj(a, "last_commit"), "id"), Branch: str(a, "source_branch"),
			Base: str(a, "target_branch"), At: ts(glTime, str(a, "updated_at"))}
		switch str(a, "action") {
		case "approved":
			e.Review = "approved"
		case "unapproved":
			e.Review = "none"
		}
		return []Event{e}, nil
	case "Push Hook":
		ref := str(m, "ref")
		if !strings.HasPrefix(ref, "refs/heads/") {
			return nil, nil
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		after := str(m, "after")
		if strings.Trim(after, "0") == "" { // all zeros: the branch was deleted
			return []Event{{Kind: KindBranch, Repo: repo, ExtID: branch, Title: branch, Branch: branch, Deleted: true, At: time.Now().UTC()}}, nil
		}
		var last time.Time
		evs := []Event{{Kind: KindBranch, Repo: repo, ExtID: branch, Title: branch, Branch: branch, SHA: after, State: "open",
			URL: strings.TrimSuffix(str(obj(m, "project"), "web_url"), "/") + "/-/tree/" + branch}}
		cs := arr(m, "commits")
		for i, c := range cs {
			if i >= MaxCommitsPerPush {
				evs[0].Extra = map[string]any{"commits_not_expanded": len(cs) - MaxCommitsPerPush}
				break
			}
			cm, _ := c.(map[string]any)
			subj, rest := firstLine(str(cm, "message"))
			if t := str(cm, "title"); t != "" {
				subj = t
			}
			au := obj(cm, "author")
			at := ts(glTime, str(cm, "timestamp"))
			if at.After(last) {
				last = at
			}
			evs = append(evs, Event{Kind: KindCommit, Repo: repo, ExtID: str(cm, "id"), Title: trunc(subj, 300), Body: rest, Author: str(au, "name"), Email: str(au, "email"),
				URL: str(cm, "url"), SHA: str(cm, "id"), Branch: branch, At: at})
		}
		evs[0].At = last
		return evs, nil
	case "Pipeline Hook":
		a := obj(m, "object_attributes")
		if a == nil {
			return nil, errors.New("pipeline payload without attributes")
		}
		state := map[string]string{"success": "passing", "failed": "failing", "canceled": "failing", "running": "running", "pending": "running", "created": "running", "skipped": "passing"}[str(a, "status")]
		if state == "" {
			return nil, nil
		}
		at := ts(glTime, str(a, "finished_at"))
		if at.IsZero() {
			at = ts(glTime, str(a, "created_at"))
		}
		return []Event{{Kind: KindBuild, Repo: repo, ExtID: "pipeline:" + fmt.Sprint(num(a, "id")), Title: "pipeline", State: state, SHA: str(a, "sha"), Branch: str(a, "ref"),
			URL: strings.TrimSuffix(str(obj(m, "project"), "web_url"), "/") + "/-/pipelines/" + fmt.Sprint(num(a, "id")), At: at}}, nil
	case "Deployment Hook":
		state := map[string]string{"success": "success", "failed": "failure", "canceled": "failure", "running": "running", "created": "pending"}[str(m, "status")]
		if state == "" {
			return nil, nil
		}
		return []Event{{Kind: KindDeployment, Repo: repo, ExtID: fmt.Sprint(num(m, "deployment_id")), Title: str(m, "environment"), Env: str(m, "environment"), State: state,
			SHA: str(m, "commit_sha"), URL: str(m, "deployable_url"), At: ts(glTime, str(m, "status_changed_at"))}}, nil
	}
	return nil, nil
}
