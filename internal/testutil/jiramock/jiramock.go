// Package jiramock is the in-process Jira Cloud double (TESTPLAN 3.2): the REST endpoints the mirror reads (myself,
// search/jql with pagination and changelog expansion, comments, changelog, project roles, group members), basic
// authentication, a JQL subset (project, updated >=, labels), rate limiting and outages, plus a signed webhook builder.
package jiramock

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type M = map[string]any

type Issue struct {
	ID       string
	Key      string
	Project  string
	Fields   M
	Comments []M
	History  []M
}

type Server struct {
	*httptest.Server
	mu          sync.Mutex
	Email       string
	Token       string
	TZ          *time.Location
	Issues      []*Issue
	Roles       map[string]map[string][]M // project -> role name -> actors
	Groups      map[string][]M            // group -> users
	Assignable  map[string][]M            // project -> people issues can be assigned to
	RolesDenied map[string]bool           // project -> Jira refuses its roles (the account does not administer it)
	Users       map[string]M
	PageSize    int
	InlineHist  int // changelog entries included inline (Jira caps this); the rest need the changelog endpoint
	reqs        []string
	limit       int
	retryAfter  int
	down        bool
	downAfter   int // when > 0, the Jira goes down after this many search requests have been answered
	searches    int
	Now         func() time.Time
	// write support (see jirawrite.go)
	Accounts    map[string]*Account // other people who may sign in, by email
	Transitions []Trans
	writes      []Write
	fail        []injected
	slowWrite   time.Duration
	// OnWrite runs before a write is applied, with the server unlocked: a test uses it to change the issue in "another person's" name
	// between the client's check and its send.
	OnWrite func(method, path string)
}

func New(email, token string) *Server {
	s := &Server{Email: email, Token: token, TZ: time.UTC, Roles: map[string]map[string][]M{}, Groups: map[string][]M{}, Users: map[string]M{}, PageSize: 2, InlineHist: 100, Now: time.Now}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func (s *Server) RateLimit(n, retryAfter int) {
	s.mu.Lock()
	s.limit, s.retryAfter = n, retryAfter
	s.mu.Unlock()
}
func (s *Server) SetDown(d bool) {
	s.mu.Lock()
	s.down = d
	if !d {
		s.downAfter = 0
	}
	s.mu.Unlock()
}

// DownAfterSearches takes the site down once n search requests have been answered (a connection that dies mid-sync).
func (s *Server) DownAfterSearches(n int) { s.mu.Lock(); s.downAfter, s.searches = n, 0; s.mu.Unlock() }
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}
func (s *Server) Count(contains string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.Contains(r, contains) {
			n++
		}
	}
	return n
}

// Upsert adds or replaces an issue; its `updated` field is stamped now when the test did not set one.
func (s *Server) Upsert(is *Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is.Fields["updated"] == nil {
		is.Fields["updated"] = s.Now().UTC().Format("2006-01-02T15:04:05.000-0700")
	}
	for i, x := range s.Issues {
		if x.ID == is.ID {
			s.Issues[i] = is
			return
		}
	}
	s.Issues = append(s.Issues, is)
}

func (s *Server) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, x := range s.Issues {
		if x.ID == id {
			s.Issues = append(s.Issues[:i], s.Issues[i+1:]...)
			return
		}
	}
}

// WebhookSecret signs a webhook body the way Jira does for an admin webhook with a secret.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func jiraTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000-0700") }

func parseJiraTime(v any) time.Time {
	str, _ := v.(string)
	for _, l := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339} {
		if t, err := time.Parse(l, str); err == nil {
			return t
		}
	}
	return time.Time{}
}

// IssueJSON builds a minimal but realistic issue.
func IssueJSON(id, key, project, summary, status, category, typ string, created, updated time.Time) *Issue {
	return &Issue{ID: id, Key: key, Project: project, Fields: M{
		"summary": summary, "created": jiraTime(created), "updated": jiraTime(updated),
		"project":     M{"key": project},
		"issuetype":   M{"name": typ, "subtask": typ == "Sub-task"},
		"status":      M{"name": status, "statusCategory": M{"key": category}},
		"priority":    M{"name": "Medium"},
		"labels":      []any{},
		"reporter":    M{"accountId": "acc-reporter", "displayName": "Rita Reporter", "emailAddress": "rita@example.com"},
		"description": nil, "assignee": nil, "resolutiondate": nil, "parent": nil, "issuelinks": []any{}, "security": nil,
	}}
}

func ADFParagraph(texts ...string) M {
	var content []any
	for _, t := range texts {
		content = append(content, M{"type": "paragraph", "content": []any{M{"type": "text", "text": t}}})
	}
	return M{"type": "doc", "version": 1, "content": content}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r.Method+" "+r.URL.RequestURI())
	if s.down {
		s.mu.Unlock()
		w.WriteHeader(503)
		return
	}
	if s.limit > 0 {
		s.limit--
		ra := s.retryAfter
		s.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(ra))
		w.WriteHeader(429)
		return
	}
	s.mu.Unlock()
	u, p, ok := r.BasicAuth()
	who, known := s.identity(u, p)
	if !ok || !known {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(M{"message": "unauthorized"})
		return
	}
	if r.Method != "GET" {
		s.write(w, r, who)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := r.URL.Path
	q := r.URL.Query()
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	switch {
	case path == "/rest/api/3/myself":
		write(M{"accountId": who.AccountID, "displayName": who.Name, "emailAddress": who.Email, "timeZone": s.TZ.String()})
	case regexp.MustCompile(`^/rest/api/3/issue/[^/]+$`).MatchString(path):
		is := s.byKeyOrID(strings.Split(path, "/")[5])
		if is == nil {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(M{"errorMessages": []string{"Issue does not exist or you do not have permission to see it."}})
			return
		}
		write(s.issueItem(is, q.Get("fields"), q.Get("expand")))
	case regexp.MustCompile(`^/rest/api/3/issue/[^/]+/transitions$`).MatchString(path):
		is := s.byKeyOrID(strings.Split(path, "/")[5])
		if is == nil {
			w.WriteHeader(404)
			return
		}
		write(M{"transitions": s.availableTransitions(is)})
	case path == "/rest/api/3/search/jql":
		if s.downAfter > 0 && s.searches >= s.downAfter {
			s.down = true
			w.WriteHeader(503)
			return
		}
		s.searches++
		s.search(w, q)
	case strings.HasPrefix(path, "/rest/api/3/issue/") && strings.HasSuffix(path, "/comment"):
		is := s.byID(strings.Split(path, "/")[5])
		if is == nil {
			w.WriteHeader(404)
			return
		}
		start, _ := strconv.Atoi(q.Get("startAt"))
		size := s.PageSize
		end := start + size
		if end > len(is.Comments) {
			end = len(is.Comments)
		}
		if start > len(is.Comments) {
			start = len(is.Comments)
		}
		write(M{"comments": is.Comments[start:end], "total": len(is.Comments), "startAt": start})
	case strings.HasPrefix(path, "/rest/api/3/issue/") && strings.HasSuffix(path, "/changelog"):
		is := s.byID(strings.Split(path, "/")[5])
		if is == nil {
			w.WriteHeader(404)
			return
		}
		start, _ := strconv.Atoi(q.Get("startAt"))
		end := start + s.PageSize
		if end > len(is.History) {
			end = len(is.History)
		}
		if start > len(is.History) {
			start = len(is.History)
		}
		write(M{"values": is.History[start:end], "total": len(is.History)})
	case path == "/rest/api/3/user/assignable/search":
		list := s.Assignable[q.Get("project")]
		start, _ := strconv.Atoi(q.Get("startAt"))
		if start > len(list) {
			start = len(list)
		}
		write(list[start:])
	case regexp.MustCompile(`^/rest/api/3/project/[^/]+/role$`).MatchString(path) && s.RolesDenied[strings.Split(path, "/")[5]]:
		w.WriteHeader(401)
		write(M{"errorMessages": []string{"You cannot edit the configuration of this project."}, "errors": M{}})
	case regexp.MustCompile(`^/rest/api/3/project/[^/]+/role$`).MatchString(path):
		key := strings.Split(path, "/")[5]
		out := M{}
		for role := range s.Roles[key] {
			out[role] = fmt.Sprintf("http://%s/rest/api/3/project/%s/role/%s", r.Host, key, role)
		}
		write(out)
	case regexp.MustCompile(`^/rest/api/3/project/[^/]+/role/[^/]+$`).MatchString(path):
		parts := strings.Split(path, "/")
		write(M{"actors": s.Roles[parts[5]][parts[7]]})
	case path == "/rest/api/3/group/member":
		start, _ := strconv.Atoi(q.Get("startAt"))
		g := s.Groups[q.Get("groupname")]
		end := start + 50
		if end > len(g) {
			end = len(g)
		}
		if start > len(g) {
			start = len(g)
		}
		write(M{"values": g[start:end], "total": len(g)})
	case path == "/rest/api/3/user":
		if u, ok := s.Users[q.Get("accountId")]; ok {
			write(u)
			return
		}
		w.WriteHeader(404)
	default:
		w.WriteHeader(404)
	}
}

func (s *Server) byID(id string) *Issue {
	for _, x := range s.Issues {
		if x.ID == id {
			return x
		}
	}
	return nil
}

var (
	reProject = regexp.MustCompile(`project\s*=\s*"?([A-Za-z0-9]+)"?`)
	reUpdated = regexp.MustCompile(`updated\s*>=\s*"(\d{4}/\d{2}/\d{2} \d{2}:\d{2})"`)
	reLabels  = regexp.MustCompile(`labels\s*=\s*"?([A-Za-z0-9_-]+)"?`)
)

func (s *Server) search(w http.ResponseWriter, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	jql := get("jql")
	var match []*Issue
	proj := ""
	if m := reProject.FindStringSubmatch(jql); m != nil {
		proj = strings.ToUpper(m[1])
	}
	var since time.Time
	if m := reUpdated.FindStringSubmatch(jql); m != nil {
		since, _ = time.ParseInLocation("2006/01/02 15:04", m[1], s.TZ)
	}
	label := ""
	if m := reLabels.FindStringSubmatch(jql); m != nil {
		label = m[1]
	}
	for _, is := range s.Issues {
		if proj != "" && strings.ToUpper(is.Project) != proj {
			continue
		}
		if !since.IsZero() && parseJiraTime(is.Fields["updated"]).Truncate(time.Minute).Before(since) {
			continue // JQL date literals have minute precision
		}
		if label != "" {
			has := false
			for _, l := range is.Fields["labels"].([]any) {
				has = has || l == label
			}
			if !has {
				continue
			}
		}
		match = append(match, is)
	}
	sort.SliceStable(match, func(i, j int) bool {
		return parseJiraTime(match[i].Fields["updated"]).Before(parseJiraTime(match[j].Fields["updated"]))
	})
	off, _ := strconv.Atoi(get("nextPageToken"))
	size := s.PageSize
	if n, err := strconv.Atoi(get("maxResults")); err == nil && n > 0 && n < size {
		size = n
	}
	end := off + size
	if end > len(match) {
		end = len(match)
	}
	if off > len(match) {
		off = len(match)
	}
	want := map[string]bool{}
	for _, f := range strings.Split(get("fields"), ",") {
		want[f] = true
	}
	var out []any
	for _, is := range match[off:end] {
		fields := M{}
		for k, v := range is.Fields {
			if want["*all"] || want[k] {
				fields[k] = v
			}
		}
		item := M{"id": is.ID, "key": is.Key, "fields": fields}
		if strings.Contains(get("expand"), "changelog") {
			h := is.History
			if len(h) > s.InlineHist {
				h = h[:s.InlineHist]
			}
			item["changelog"] = M{"histories": h, "total": len(is.History)}
		}
		if want["comment"] {
			c := is.Comments
			if len(c) > s.PageSize {
				c = c[:s.PageSize] // inline comments are capped too: the client must page for the rest
			}
			fields["comment"] = M{"comments": c, "total": len(is.Comments)}
		}
		out = append(out, item)
	}
	resp := M{"issues": out}
	if end < len(match) {
		resp["nextPageToken"] = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
