package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/rt"
	"github.com/larytet/tasklane/internal/testutil"
	"github.com/larytet/tasklane/internal/testutil/gitmock"
	"github.com/larytet/tasklane/internal/testutil/jiramock"
	"github.com/larytet/tasklane/internal/testutil/s3mock"
)

// SEC-01: every API route against every role, from one table. A route cannot be added without a row here: the meta-test
// compares the registered patterns with the table.

type role string

const (
	anon     role = "anonymous"
	outsider role = "outsider"
	viewer   role = "viewer"
	member   role = "member"
	admin    role = "admin"
)

var roles = []role{anon, outsider, viewer, member, admin}

// expectations
func codes(c ...int) []int { return c }

// withCSRF adds the header the portal's sign-in routes ask for, as its page does.
func withCSRF(r req) req { r.headers = map[string]string{"X-Requested-With": "tasklane"}; return r }

type exp map[role][]int

var (
	public    = exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}
	authed    = exp{anon: codes(401), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}
	reader    = exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}
	writer    = exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200, 201), admin: codes(200, 201)}
	projAdmin = exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200, 201)} // a project's administrators; others do not learn the project exists
	adminer   = exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200, 201)}
	owner     = func(ok ...int) exp { // only the person who created it; everyone else sees nothing, administrators included
		return exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: ok, admin: codes(404)}
	}
)

type req struct {
	method, path string
	body         any
	raw          []byte
	headers      map[string]string
}

type fixture struct {
	t     *testing.T
	e     *testutil.Env
	n     int
	spare domain.User
	mock  *s3mock.Server
	git   *gitmock.API
	jira  *jiramock.Server
}

func (f *fixture) issue() string {
	f.n++
	is, err := f.e.Svc.CreateIssue(testutil.Ctx(), f.e.Alice, domain.CreateIn{Project: "ABC", Summary: fmt.Sprint("matrix ", f.n), Description: "orig text"})
	if err != nil {
		f.t.Fatal(err)
	}
	return is.Key
}

func (f *fixture) board() string {
	f.n++
	b, err := f.e.Svc.CreateBoard(testutil.Ctx(), f.e.Admin, "ABC", fmt.Sprint("board ", f.n), "kanban")
	if err != nil {
		f.t.Fatal(err)
	}
	return b.ID
}

func (f *fixture) version() string {
	f.n++
	n := fmt.Sprint("v", f.n)
	v, err := f.e.Svc.CreateVersion(testutil.Ctx(), f.e.Admin, "ABC", domain.VersionIn{Name: &n})
	if err != nil {
		f.t.Fatal(err)
	}
	return v.ID
}

// approval makes an open approval on a fresh issue that the matrix's member (and administrator) is the approver of.
func (f *fixture) approval() string {
	is, err := f.e.Svc.CreateIssue(testutil.Ctx(), f.e.Admin, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "approval matrix"})
	if err != nil {
		f.t.Fatal(err)
	}
	ap, err := f.e.Svc.RequestApproval(testutil.Ctx(), f.e.Admin, is.Key, domain.ApprovalIn{Users: []string{f.e.Alice.ID, f.e.Admin.ID}, Rule: "any"})
	if err != nil {
		f.t.Fatal(err)
	}
	return ap.ID
}

func (f *fixture) requestType() string {
	f.n++
	n := fmt.Sprint("RT ", f.n)
	rt, err := f.e.Svc.CreateRequestType(testutil.Ctx(), f.e.Admin, "ABC", domain.RequestTypeIn{Name: &n})
	if err != nil {
		f.t.Fatal(err)
	}
	return rt.ID
}

func (f *fixture) queue() string {
	f.n++
	n, q := fmt.Sprint("Q ", f.n), "statusCategory != Done"
	v, err := f.e.Svc.CreateQueue(testutil.Ctx(), f.e.Admin, "ABC", domain.QueueIn{Name: &n, TQL: &q})
	if err != nil {
		f.t.Fatal(err)
	}
	return v.ID
}

func (f *fixture) canned() string {
	f.n++
	n, b := fmt.Sprint("C ", f.n), "Hello"
	v, err := f.e.Svc.CreateCanned(testutil.Ctx(), f.e.Admin, "ABC", domain.CannedIn{Name: &n, Body: &b})
	if err != nil {
		f.t.Fatal(err)
	}
	return v.ID
}

func (f *fixture) customerOrg() string {
	f.n++
	o, err := f.e.Svc.CreateCustomerOrg(testutil.Ctx(), f.e.Admin, "ABC", fmt.Sprint("Org ", f.n))
	if err != nil {
		f.t.Fatal(err)
	}
	return o.ID
}

func (f *fixture) customer() string {
	f.n++
	var id string
	err := f.e.Store.Pool.QueryRow(testutil.Ctx(), `INSERT INTO customer(email,verified_at) VALUES($1,now()) RETURNING id::text`, fmt.Sprint("matrix", f.n, "@customer.example")).Scan(&id)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) versionName() string {
	f.n++
	n := fmt.Sprint("n", f.n)
	if _, err := f.e.Svc.CreateVersion(testutil.Ctx(), f.e.Admin, "ABC", domain.VersionIn{Name: &n}); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) sprintBoard() string {
	f.n++
	b, err := f.e.Svc.CreateBoard(testutil.Ctx(), f.e.Admin, "ABC", fmt.Sprint("scrum ", f.n), "scrum")
	if err != nil {
		f.t.Fatal(err)
	}
	return b.ID
}

func (f *fixture) sprint() string {
	b := f.sprintBoard()
	f.e.Svc.EnsureSprints(testutil.Ctx(), b)
	v, err := f.e.Svc.Backlog(testutil.Ctx(), f.e.Admin, b)
	if err != nil {
		f.t.Fatal(err)
	}
	return v.Sprints[0].ID
}

func (f *fixture) activeSprint() string {
	id := f.sprint()
	if _, err := f.e.Svc.StartSprint(testutil.Ctx(), f.e.Admin, id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// direct starts a direct upload as Alice and puts the bytes at its URL, so completing it is the next step.
func (f *fixture) direct() string {
	du, err := f.e.Svc.StartDirectUpload(testutil.Ctx(), f.e.Alice, f.issue(), "d.txt", "text/plain", 6, "")
	if err != nil {
		f.t.Fatal(err)
	}
	req, _ := http.NewRequest("PUT", du.URL, strings.NewReader("direct"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		f.t.Fatalf("%v %v", err, resp)
	}
	resp.Body.Close()
	return du.ID
}

// gitConn creates a Git connection pointing at the in-process provider double, with one mapped repository.
func (f *fixture) gitConn() (conn, repo string) {
	f.n++
	if f.git == nil {
		f.git = gitmock.New("github", "t")
		f.t.Cleanup(f.git.Close)
	}
	c, _, err := f.e.Svc.CreateGitConnection(testutil.Ctx(), f.e.Admin, domain.GitConnIn{Name: fmt.Sprint("conn", f.n), Provider: "github", BaseURL: f.git.URL, Token: "t", AllowPrivate: true})
	if err != nil {
		f.t.Fatal(err)
	}
	r, err := f.e.Svc.MapGitRepo(testutil.Ctx(), f.e.Admin, c.ID, "acme/shop", []string{"ABC"})
	if err != nil {
		f.t.Fatal(err)
	}
	return c.ID, r.ID
}

// jiraConn creates a Jira connection to the in-process site; mirrored makes a mirrored project on it.
func (f *fixture) jiraConn() string {
	f.n++
	if f.jira == nil {
		f.jira = jiramock.New("sync@example.com", "tok")
		f.t.Cleanup(f.jira.Close)
	}
	c, _, err := f.e.Svc.CreateJiraConnection(testutil.Ctx(), f.e.Admin, domain.JiraConnIn{Name: fmt.Sprint("jira", f.n), SiteURL: f.jira.URL, Email: "sync@example.com", Token: "tok", AllowPrivate: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return c.ID
}

func (f *fixture) mirrored() string {
	c := f.jiraConn()
	key := fmt.Sprintf("M%d", f.n)
	if _, err := f.e.Svc.MirrorJiraProject(testutil.Ctx(), f.e.Admin, c, key, key, "Mirror "+key, ""); err != nil {
		f.t.Fatal(err)
	}
	return key
}

func (f *fixture) field() int64 {
	f.n++
	fd, err := f.e.Svc.CreateField(testutil.Ctx(), f.e.Admin, domain.FieldIn{Name: fmt.Sprint("Field ", f.n), Type: "select"})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.e.Svc.AddContext(testutil.Ctx(), f.e.Admin, fd.ID, domain.ContextIn{Global: true, Options: []string{"A", "B"}}); err != nil {
		f.t.Fatal(err)
	}
	return fd.ID
}

func (f *fixture) screen() int64 {
	f.n++
	sc, err := f.e.Svc.SaveScreen(testutil.Ctx(), f.e.Admin, 0, domain.Screen{Name: fmt.Sprint("Screen ", f.n), Tabs: []domain.ScreenTab{{Name: "Main", Fields: []string{"priority"}}}})
	if err != nil {
		f.t.Fatal(err)
	}
	return sc.ID
}

func (f *fixture) scheme() int64 {
	f.n++
	sc, err := f.e.Svc.SaveScheme(testutil.Ctx(), f.e.Admin, 0, domain.Scheme{Name: fmt.Sprint("Scheme ", f.n)})
	if err != nil {
		f.t.Fatal(err)
	}
	return sc.ID
}

func (f *fixture) rule() int64 {
	r, err := f.e.Svc.SaveRule(testutil.Ctx(), f.e.Admin, 0, domain.Rule{Enabled: false, Action: "require", Target: "labels"})
	if err != nil {
		f.t.Fatal(err)
	}
	return r.ID
}

func (f *fixture) workflow() string {
	f.n++
	w, err := f.e.Svc.CreateWorkflow(testutil.Ctx(), f.e.Admin, fmt.Sprint("Flow ", f.n), "simple", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return w.ID
}

func (f *fixture) autoRule() int64 {
	r, err := f.e.Svc.SaveAutoRule(testutil.Ctx(), f.e.Admin, 0, domain.AutoRule{Name: "matrix rule", Project: "ABC", Enabled: false,
		Trigger: domain.Spec{"type": "issue.created"}, Actions: []domain.Spec{{"kind": "comment", "text": "hi"}}})
	if err != nil {
		f.t.Fatal(err)
	}
	return r.ID
}

// eraseTarget makes a fresh account, so each role's request erases a different one
func (f *fixture) eraseTarget() string {
	f.n++
	u, err := f.e.Svc.CreateUser(testutil.Ctx(), fmt.Sprintf("erase%d@example.com", f.n), "Erase Me", "password123", false)
	if err != nil {
		f.t.Fatal(err)
	}
	return u.ID
}

func (f *fixture) group() string {
	f.n++
	g, err := f.e.Svc.CreateGroup(testutil.Ctx(), f.e.Admin, fmt.Sprint("Group ", f.n))
	if err != nil {
		f.t.Fatal(err)
	}
	return g.ID
}

func (f *fixture) attachment(key string) string {
	a, err := f.e.Svc.AttachStream(testutil.Ctx(), f.e.Alice, key, "n.txt", "text/plain", strings.NewReader("hello matrix"))
	if err != nil {
		f.t.Fatal(err)
	}
	return a.ID
}

func (f *fixture) comment(key string) string {
	c, err := f.e.Svc.AddComment(testutil.Ctx(), f.e.Alice, key, "a comment", false)
	if err != nil {
		f.t.Fatal(err)
	}
	return c.ID
}

func (f *fixture) upload(key string) string {
	u, err := f.e.Svc.StartUpload(testutil.Ctx(), f.e.Alice, key, "big.bin", "", 100)
	if err != nil {
		f.t.Fatal(err)
	}
	return u.ID
}

const uuid0 = "00000000-0000-0000-0000-000000000000"

type row struct {
	pattern string
	exp     exp
	build   func(f *fixture) req
}

func rows() []row {
	J := func(m, p string, b any) req { return req{method: m, path: p, body: b} }
	return []row{
		{"GET /healthz", public, func(f *fixture) req { return J("GET", "/healthz", nil) }},
		{"GET /readyz", public, func(f *fixture) req { return J("GET", "/readyz", nil) }},
		{"POST /api/v1/login", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("POST", "/api/v1/login", map[string]string{"email": "nobody@example.com", "password": "wrong-password"})
		}},
		{"POST /api/v1/logout", authed, func(f *fixture) req { return J("POST", "/api/v1/logout", nil) }},
		{"GET /api/v1/me", authed, func(f *fixture) req { return J("GET", "/api/v1/me", nil) }},
		{"POST /api/v1/agents/kill", adminer, func(f *fixture) req { return J("POST", "/api/v1/agents/kill", map[string]any{"on": false}) }},
		{"POST /api/v1/agents/{id}/pause", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("POST", "/api/v1/agents/"+uuid0+"/pause", map[string]any{"paused": false})
		}},
		{"GET /api/v1/projects/{key}/boards", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/boards", nil) }},
		{"POST /api/v1/projects/{key}/boards", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/projects/ABC/boards", map[string]any{"name": fmt.Sprint("b", f.n), "kind": "scrum"})
		}},
		{"GET /api/v1/boards/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.board(), nil) }},
		{"PATCH /api/v1/boards/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PATCH", "/api/v1/boards/"+f.board(), map[string]any{"swimlane": "assignee"})
		}},
		{"PUT /api/v1/boards/{id}/columns", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/boards/"+f.board()+"/columns", map[string]any{"columns": []map[string]any{{"name": "All", "statuses": []string{"To Do", "In Progress", "Done"}}}})
		}},
		{"POST /api/v1/boards/{id}/move", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/boards/"+f.board()+"/move", map[string]any{"key": f.issue(), "column": "In Progress"})
		}},
		{"GET /api/v1/boards/{id}/backlog", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.sprintBoard()+"/backlog", nil) }},
		{"GET /api/v1/boards/{id}/sprints", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.sprintBoard()+"/sprints", nil) }},
		{"POST /api/v1/boards/{id}/sprints", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/boards/"+f.sprintBoard()+"/sprints", map[string]any{"goal": "g"})
		}},
		{"POST /api/v1/boards/{id}/plan", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/boards/"+f.sprintBoard()+"/plan", map[string]any{"sprint": "", "keys": []string{f.issue()}})
		}},
		{"POST /api/v1/boards/{id}/rank", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/boards/"+f.sprintBoard()+"/rank", map[string]any{"key": f.issue(), "top": true})
		}},
		{"GET /api/v1/sprints/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/sprints/"+f.sprint(), nil) }},
		{"PATCH /api/v1/sprints/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PATCH", "/api/v1/sprints/"+f.sprint(), map[string]any{"goal": "ship it"})
		}},
		{"POST /api/v1/sprints/{id}/start", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("POST", "/api/v1/sprints/"+f.sprint()+"/start", nil) }},
		{"POST /api/v1/sprints/{id}/complete", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/sprints/"+f.activeSprint()+"/complete", map[string]any{"destination": "backlog", "dry_run": true})
		}},
		{"POST /api/v1/sprints/{id}/recompute", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("POST", "/api/v1/sprints/"+f.sprint()+"/recompute", nil) }},
		{"PUT /api/v1/sprints/{id}/retro", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/sprints/"+f.sprint()+"/retro", map[string]any{"text": "went well"})
		}},
		{"PUT /api/v1/sprints/{id}/capacity/{user}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/sprints/"+f.sprint()+"/capacity/"+f.spare.ID, map[string]any{"off_hours": 4})
		}},
		{"PUT /api/v1/users/{id}/calendar", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/users/"+f.spare.ID+"/calendar", map[string]any{"calendar": nil})
		}},
		{"GET /api/v1/boards/{id}/reports/cumulative-flow", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.board()+"/reports/cumulative-flow", nil) }},
		{"GET /api/v1/boards/{id}/reports/control-chart", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.board()+"/reports/control-chart", nil) }},
		{"GET /api/v1/boards/{id}/reports/created-vs-resolved", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.board()+"/reports/created-vs-resolved", nil) }},
		{"GET /api/v1/boards/{id}/reports/workload", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.board()+"/reports/workload", nil) }},
		{"GET /api/v1/boards/{id}/reports/time-in-status", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.board()+"/reports/time-in-status", nil) }},
		{"GET /api/v1/boards/{id}/reports/velocity", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/boards/"+f.sprintBoard()+"/reports/velocity", nil) }},
		{"GET /api/v1/boards/{id}/reports/epic/{key}", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			ep, _ := f.e.Svc.CreateIssue(testutil.Ctx(), f.e.Alice, domain.CreateIn{Project: "ABC", Type: "Epic", Summary: "e"})
			return J("GET", "/api/v1/boards/"+f.board()+"/reports/epic/"+ep.Key, nil)
		}},
		{"GET /api/v1/sprints/{id}/report", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/sprints/"+f.activeSprint()+"/report", nil) }},
		{"POST /api/v1/issues/{key}/uploads/direct", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(201), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/uploads/direct", map[string]any{"name": "d.txt", "size": 6, "type": "text/plain"})
		}},
		{"POST /api/v1/uploads/direct/{id}/complete", owner(201), func(f *fixture) req {
			return J("POST", "/api/v1/uploads/direct/"+f.direct()+"/complete", map[string]any{})
		}},
		{"DELETE /api/v1/uploads/direct/{id}", owner(204), func(f *fixture) req { return J("DELETE", "/api/v1/uploads/direct/"+f.direct(), nil) }},
		{"POST /api/v1/git/webhooks/{id}", exp{anon: codes(403), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req {
			c, _ := f.gitConn()
			return J("POST", "/api/v1/git/webhooks/"+c, map[string]any{"zen": "x"})
		}}, // proves itself with a signature, never with a session
		{"GET /api/v1/git/connections", adminer, func(f *fixture) req { return J("GET", "/api/v1/git/connections", nil) }},
		{"POST /api/v1/git/connections", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/git/connections", map[string]any{"name": fmt.Sprint("c", f.n), "provider": "github"})
		}},
		{"DELETE /api/v1/git/connections/{id}", adminer, func(f *fixture) req { c, _ := f.gitConn(); return J("DELETE", "/api/v1/git/connections/"+c, nil) }},
		{"POST /api/v1/git/connections/{id}/repos", adminer, func(f *fixture) req {
			c, _ := f.gitConn()
			return J("POST", "/api/v1/git/connections/"+c+"/repos", map[string]any{"full_name": "acme/other", "projects": []string{"ABC"}})
		}},
		{"GET /api/v1/git/repos", adminer, func(f *fixture) req { return J("GET", "/api/v1/git/repos", nil) }},
		{"POST /api/v1/git/repos/{id}/sync", adminer, func(f *fixture) req { _, r := f.gitConn(); return J("POST", "/api/v1/git/repos/"+r+"/sync", nil) }},
		{"GET /api/v1/issues/{key}/development", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/issues/"+f.issue()+"/development", nil) }},
		{"POST /api/v1/issues/{key}/development/links", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(400), admin: codes(400)}, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/development/links", map[string]any{"url": "https://github.com/acme/shop/pull/1"})
		}},
		{"DELETE /api/v1/issues/{key}/development/links/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/issues/"+f.issue()+"/development/links/"+uuid0, nil)
		}},
		{"GET /api/v1/admin/search", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/search", nil) }},
		{"POST /api/v1/admin/search/rebuild", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/admin/search/rebuild", nil) }},
		{"POST /api/v1/admin/search/reconcile", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/admin/search/reconcile", map[string]any{}) }},
		{"POST /api/v1/admin/search/reindex", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req {
			return J("POST", "/api/v1/admin/search/reindex", map[string]any{"project": "ABC"})
		}},
		{"POST /api/v1/admin/search/rollback", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/admin/search/rollback", nil) }},
		{"POST /api/v1/jira/webhooks/{id}", exp{anon: codes(403), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req {
			return J("POST", "/api/v1/jira/webhooks/"+f.jiraConn(), map[string]any{"webhookEvent": "x"})
		}},
		{"GET /api/v1/jira/connections", adminer, func(f *fixture) req { return J("GET", "/api/v1/jira/connections", nil) }},
		{"POST /api/v1/jira/connections", adminer, func(f *fixture) req {
			f.n++
			if f.jira == nil {
				f.jiraConn()
			}
			return J("POST", "/api/v1/jira/connections", map[string]any{"name": fmt.Sprint("new", f.n), "site_url": f.jira.URL, "email": "sync@example.com", "token": "tok", "allow_private": true})
		}},
		{"POST /api/v1/jira/connections/{id}/projects", adminer, func(f *fixture) req {
			c := f.jiraConn()
			return J("POST", "/api/v1/jira/connections/"+c+"/projects", map[string]any{"jira_key": fmt.Sprintf("JP%d", f.n), "key": fmt.Sprintf("JP%d", f.n)})
		}},
		{"POST /api/v1/jira/projects/{key}/sync", adminer, func(f *fixture) req { return J("POST", "/api/v1/jira/projects/"+f.mirrored()+"/sync", nil) }},
		{"POST /api/v1/jira/projects/{key}/reconcile", adminer, func(f *fixture) req { return J("POST", "/api/v1/jira/projects/"+f.mirrored()+"/reconcile", nil) }},
		{"POST /api/v1/jira/projects/{key}/cutover", adminer, func(f *fixture) req { return J("POST", "/api/v1/jira/projects/"+f.mirrored()+"/cutover", nil) }},
		{"GET /api/v1/jira/status", adminer, func(f *fixture) req { return J("GET", "/api/v1/jira/status", nil) }},
		{"GET /api/v1/fields", authed, func(f *fixture) req { return J("GET", "/api/v1/fields", nil) }},
		{"POST /api/v1/fields", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/fields", map[string]any{"name": fmt.Sprint("New ", f.n), "type": "text"})
		}},
		{"PATCH /api/v1/fields/{id}", adminer, func(f *fixture) req {
			return J("PATCH", fmt.Sprintf("/api/v1/fields/%d", f.field()), map[string]any{"help_text": "h"})
		}},
		{"DELETE /api/v1/fields/{id}", adminer, func(f *fixture) req { return J("DELETE", fmt.Sprintf("/api/v1/fields/%d", f.field()), nil) }},
		{"POST /api/v1/fields/{id}/restore", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("POST", fmt.Sprintf("/api/v1/fields/%d/restore", f.field()), nil) }},
		{"GET /api/v1/fields/{id}/contexts", adminer, func(f *fixture) req { return J("GET", fmt.Sprintf("/api/v1/fields/%d/contexts", f.field()), nil) }},
		{"POST /api/v1/fields/{id}/contexts", adminer, func(f *fixture) req {
			return J("POST", fmt.Sprintf("/api/v1/fields/%d/contexts", f.field()), map[string]any{"global": true, "options": []string{"X"}})
		}},
		{"DELETE /api/v1/fields/{id}/contexts/{ctx}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", fmt.Sprintf("/api/v1/fields/%d/contexts/999999", f.field()), nil)
		}},
		{"POST /api/v1/fields/{id}/contexts/{ctx}/options", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req {
			return J("POST", fmt.Sprintf("/api/v1/fields/%d/contexts/999999/options", f.field()), map[string]any{"value": "Z"})
		}},
		{"PATCH /api/v1/fields/options/{option}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("PATCH", "/api/v1/fields/options/999999", map[string]any{"archived": true})
		}},
		{"POST /api/v1/fields/{id}/merge-options", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", fmt.Sprintf("/api/v1/fields/%d/merge-options", f.field()), map[string]any{"from": "A", "into": "B"})
		}},
		{"POST /api/v1/fields/{id}/replace", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", fmt.Sprintf("/api/v1/fields/%d/replace", f.field()), map[string]any{"with": f.field()})
		}},
		{"GET /api/v1/fields/warnings", adminer, func(f *fixture) req { return J("GET", "/api/v1/fields/warnings", nil) }},
		{"GET /api/v1/projects/{key}/fields", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/fields?type=Task", nil) }},
		{"GET /api/v1/screens", adminer, func(f *fixture) req { return J("GET", "/api/v1/screens", nil) }},
		{"POST /api/v1/screens", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/screens", map[string]any{"name": fmt.Sprint("S ", f.n), "tabs": []map[string]any{{"name": "T", "fields": []string{"labels"}}}})
		}},
		{"PUT /api/v1/screens/{id}", adminer, func(f *fixture) req {
			return J("PUT", fmt.Sprintf("/api/v1/screens/%d", f.screen()), map[string]any{"name": fmt.Sprint("Renamed ", f.n), "tabs": []any{}})
		}},
		{"DELETE /api/v1/screens/{id}", adminer, func(f *fixture) req { return J("DELETE", fmt.Sprintf("/api/v1/screens/%d", f.screen()), nil) }},
		{"GET /api/v1/screen-schemes", adminer, func(f *fixture) req { return J("GET", "/api/v1/screen-schemes", nil) }},
		{"POST /api/v1/screen-schemes", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/screen-schemes", map[string]any{"name": fmt.Sprint("Scheme ", f.n)})
		}},
		{"PUT /api/v1/screen-schemes/{id}", adminer, func(f *fixture) req {
			return J("PUT", fmt.Sprintf("/api/v1/screen-schemes/%d", f.scheme()), map[string]any{"name": fmt.Sprint("Renamed ", f.n)})
		}},
		{"DELETE /api/v1/screen-schemes/{id}", adminer, func(f *fixture) req { return J("DELETE", fmt.Sprintf("/api/v1/screen-schemes/%d", f.scheme()), nil) }},
		{"PUT /api/v1/projects/{key}/screen-scheme", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/screen-scheme", map[string]any{"scheme": nil})
		}},
		{"GET /api/v1/field-configs", adminer, func(f *fixture) req { return J("GET", "/api/v1/field-configs", nil) }},
		{"PUT /api/v1/field-configs", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/field-configs", map[string]any{"field": "labels", "required": false})
		}},
		{"DELETE /api/v1/field-configs/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/field-configs/999999", nil) }},
		{"GET /api/v1/behavior-rules", adminer, func(f *fixture) req { return J("GET", "/api/v1/behavior-rules", nil) }},
		{"POST /api/v1/behavior-rules", adminer, func(f *fixture) req {
			return J("POST", "/api/v1/behavior-rules", map[string]any{"action": "hide", "target": "labels", "enabled": false})
		}},
		{"PUT /api/v1/behavior-rules/{id}", adminer, func(f *fixture) req {
			return J("PUT", fmt.Sprintf("/api/v1/behavior-rules/%d", f.rule()), map[string]any{"action": "show", "target": "labels", "enabled": false})
		}},
		{"DELETE /api/v1/behavior-rules/{id}", adminer, func(f *fixture) req { return J("DELETE", fmt.Sprintf("/api/v1/behavior-rules/%d", f.rule()), nil) }},
		{"GET /api/v1/projects/{key}/form", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/form?type=Task", nil) }},
		{"GET /api/v1/workflows", adminer, func(f *fixture) req { return J("GET", "/api/v1/workflows", nil) }},
		{"POST /api/v1/workflows", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/workflows", map[string]any{"name": fmt.Sprint("New flow ", f.n), "template": "software"})
		}},
		{"PUT /api/v1/projects/{key}/workflow", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/workflow", map[string]any{"workflow": f.workflow()})
		}},
		{"GET /api/v1/workflow-templates", authed, func(f *fixture) req { return J("GET", "/api/v1/workflow-templates", nil) }},
		{"GET /api/v1/workflows/{id}", adminer, func(f *fixture) req { return J("GET", "/api/v1/workflows/"+f.workflow(), nil) }},
		{"GET /api/v1/workflows/{id}/export", adminer, func(f *fixture) req { return J("GET", "/api/v1/workflows/"+f.workflow()+"/export", nil) }},
		{"POST /api/v1/workflows/{id}/draft", adminer, func(f *fixture) req { return J("POST", "/api/v1/workflows/"+f.workflow()+"/draft", nil) }},
		{"PUT /api/v1/workflows/{id}/draft", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/workflows/"+f.workflow()+"/draft", map[string]any{"statuses": []map[string]any{{"name": "Open", "category": "todo"}}, "transitions": []any{}})
		}},
		{"DELETE /api/v1/workflows/{id}/draft", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/workflows/"+f.workflow()+"/draft", nil) }},
		{"POST /api/v1/workflows/{id}/draft/check", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req {
			return J("POST", "/api/v1/workflows/"+f.workflow()+"/draft/check", map[string]any{})
		}},
		{"POST /api/v1/workflows/{id}/publish", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/workflows/"+f.workflow()+"/publish", map[string]any{}) }},
		{"POST /api/v1/issues/{key}/archive", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(400), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/issues/"+f.issue()+"/archive", nil) }}, // an open issue is not archived
		{"DELETE /api/v1/issues/{key}/archive", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(400), admin: codes(400)}, func(f *fixture) req { return J("DELETE", "/api/v1/issues/"+f.issue()+"/archive", nil) }},
		{"GET /api/v1/projects/{key}/archive-policy", adminer, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/archive-policy", nil) }},
		{"PUT /api/v1/projects/{key}/archive-policy", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/archive-policy", map[string]any{"days": nil})
		}},
		{"GET /api/v1/admin/audit", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/audit", nil) }},
		{"GET /api/v1/admin/audit/verify", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/audit/verify", nil) }},
		{"GET /api/v1/admin/audit/export", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/audit/export", nil) }},
		{"GET /api/v1/notifications", authed, func(f *fixture) req { return J("GET", "/api/v1/notifications", nil) }},
		{"GET /api/v1/notifications/unread-count", authed, func(f *fixture) req { return J("GET", "/api/v1/notifications/unread-count", nil) }},
		{"POST /api/v1/notifications/read-all", authed, func(f *fixture) req { return J("POST", "/api/v1/notifications/read-all", nil) }},
		{"POST /api/v1/notifications/{id}/state", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("POST", "/api/v1/notifications/999999/state", map[string]any{"state": "read"})
		}},
		{"GET /api/v1/me/notification-prefs", authed, func(f *fixture) req { return J("GET", "/api/v1/me/notification-prefs", nil) }},
		{"PUT /api/v1/me/notification-prefs", authed, func(f *fixture) req {
			return J("PUT", "/api/v1/me/notification-prefs", map[string]any{"auto_watch": true, "inbox": true, "email": true})
		}},
		{"GET /api/v1/issues/{key}/watch", reader, func(f *fixture) req { return J("GET", "/api/v1/issues/"+f.issue()+"/watch", nil) }},
		{"PUT /api/v1/issues/{key}/watch", reader, func(f *fixture) req {
			return J("PUT", "/api/v1/issues/"+f.issue()+"/watch", map[string]any{"watch": true})
		}},
		{"DELETE /api/v1/issues/{key}/watch", reader, func(f *fixture) req { return J("DELETE", "/api/v1/issues/"+f.issue()+"/watch", nil) }},
		{"GET /api/v1/projects/{key}/notification-scheme", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("GET", "/api/v1/projects/ABC/notification-scheme", nil)
		}},
		{"PUT /api/v1/projects/{key}/notification-scheme", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/notification-scheme", map[string]any{"issue.transitioned": []string{"assignee"}})
		}},
		{"GET /api/v1/automation/rules", projAdmin, func(f *fixture) req { return J("GET", "/api/v1/automation/rules?project=ABC", nil) }},
		{"POST /api/v1/automation/rules", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/automation/rules", map[string]any{"name": "new", "project": "ABC", "enabled": false, "trigger": map[string]any{"type": "issue.created"}, "actions": []map[string]any{{"kind": "comment", "text": "hi"}}})
		}},
		{"GET /api/v1/automation/rules/{id}", projAdmin, func(f *fixture) req { return J("GET", fmt.Sprintf("/api/v1/automation/rules/%d", f.autoRule()), nil) }},
		{"PUT /api/v1/automation/rules/{id}", projAdmin, func(f *fixture) req {
			return J("PUT", fmt.Sprintf("/api/v1/automation/rules/%d", f.autoRule()), map[string]any{"name": "renamed", "project": "ABC", "enabled": false, "trigger": map[string]any{"type": "issue.created"}, "actions": []map[string]any{{"kind": "comment", "text": "hi"}}})
		}},
		{"DELETE /api/v1/automation/rules/{id}", projAdmin, func(f *fixture) req {
			return J("DELETE", fmt.Sprintf("/api/v1/automation/rules/%d", f.autoRule()), nil)
		}},
		{"POST /api/v1/automation/rules/{id}/dry-run", projAdmin, func(f *fixture) req {
			return J("POST", fmt.Sprintf("/api/v1/automation/rules/%d/dry-run", f.autoRule()), map[string]any{"issue": f.issue()})
		}},
		{"GET /api/v1/automation/rules/{id}/runs", projAdmin, func(f *fixture) req {
			return J("GET", fmt.Sprintf("/api/v1/automation/rules/%d/runs", f.autoRule()), nil)
		}},
		{"GET /api/v1/admin/security-policy", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/security-policy", nil) }},
		{"PUT /api/v1/admin/security-policy", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/admin/security-policy", map[string]any{"require_second_factor": false, "ip_allowlist": []string{}, "session_max_hours": 0})
		}},
		{"GET /api/v1/roles", exp{anon: codes(401), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/roles", nil) }},
		{"POST /api/v1/roles", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/roles", map[string]any{"name": "Matrix role", "base": "viewer", "grants": []string{"comment"}})
		}},
		{"PUT /api/v1/roles/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/roles/00000000-0000-0000-0000-000000000001", map[string]any{"name": "x", "base": "viewer"})
		}},
		{"DELETE /api/v1/roles/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/roles/00000000-0000-0000-0000-000000000001", nil) }},
		{"GET /api/v1/filters", authed, func(f *fixture) req { return J("GET", "/api/v1/filters", nil) }},
		{"POST /api/v1/filters", exp{anon: codes(401), outsider: codes(201), viewer: codes(201), member: codes(201), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/filters", map[string]any{"name": "Matrix filter", "tql": "project = ABC"})
		}},
		{"PUT /api/v1/filters/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/filters/00000000-0000-0000-0000-000000000009", map[string]any{"name": "x", "tql": "project = ABC"})
		}},
		{"DELETE /api/v1/filters/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/filters/00000000-0000-0000-0000-000000000009", nil) }},
		{"GET /api/v1/dashboard-kinds", authed, func(f *fixture) req { return J("GET", "/api/v1/dashboard-kinds", nil) }},
		{"GET /api/v1/projects/{key}/versions", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/versions", nil) }},
		{"POST /api/v1/projects/{key}/versions", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/projects/ABC/versions", map[string]any{"name": fmt.Sprint("m", f.n)})
		}},
		{"GET /api/v1/versions/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/versions/"+f.version(), nil) }},
		{"PUT /api/v1/versions/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/versions/"+f.version(), map[string]any{"description": "changed"})
		}},
		{"DELETE /api/v1/versions/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("DELETE", "/api/v1/versions/"+f.version(), nil) }},
		{"POST /api/v1/versions/{id}/release", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/versions/"+f.version()+"/release", map[string]any{})
		}},
		{"POST /api/v1/versions/{id}/unrelease", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req {
			return J("POST", "/api/v1/versions/"+f.version()+"/unrelease", nil)
		}},
		{"GET /api/v1/versions/{id}/notes", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/versions/"+f.version()+"/notes", nil) }},
		{"PUT /api/v1/issues/{key}/versions", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/issues/"+f.issue()+"/versions", map[string]any{"fix": []string{f.versionName()}})
		}},
		{"GET /api/v1/issues/{key}/approvals", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/issues/"+f.issue()+"/approvals", nil) }},
		{"POST /api/v1/issues/{key}/approvals", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(201), admin: codes(201)}, func(f *fixture) req {
			is, _ := f.e.Svc.CreateIssue(testutil.Ctx(), f.e.Admin, domain.CreateIn{Project: "ABC", Type: "Task", Summary: "ask matrix"})
			return J("POST", "/api/v1/issues/"+is.Key+"/approvals", map[string]any{"users": []string{f.e.Alice.ID}})
		}},
		{"GET /api/v1/approvals", authed, func(f *fixture) req { return J("GET", "/api/v1/approvals", nil) }},
		{"POST /api/v1/approvals/{id}/decision", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/approvals/"+f.approval()+"/decision", map[string]any{"decision": "approve"})
		}},
		{"POST /api/v1/approvals/{id}/cancel", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/approvals/"+f.approval()+"/cancel", nil)
		}},
		{"GET /api/v1/portal/{key}/challenge", exp{anon: codes(404), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("GET", "/api/v1/portal/ABC/challenge", nil) }},
		{"GET /api/v1/portal/{key}", exp{anon: codes(404), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("GET", "/api/v1/portal/ABC", nil) }}, // the portal is off until its administrator turns it on
		{"POST /api/v1/portal/{key}/code", exp{anon: codes(404), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return withCSRF(J("POST", "/api/v1/portal/ABC/code", map[string]any{"email": "c@example.com"}))
		}},
		{"POST /api/v1/portal/{key}/verify", exp{anon: codes(404), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return withCSRF(J("POST", "/api/v1/portal/ABC/verify", map[string]any{"email": "c@example.com", "code": "123456"}))
		}},
		{"POST /api/v1/portal/{key}/login", exp{anon: codes(404), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return withCSRF(J("POST", "/api/v1/portal/ABC/login", map[string]any{"email": "c@example.com", "password": "x"}))
		}},
		{"POST /api/v1/portal/link", exp{anon: codes(400), outsider: codes(400), viewer: codes(400), member: codes(400), admin: codes(400)}, func(f *fixture) req {
			return withCSRF(J("POST", "/api/v1/portal/link", map[string]any{"token": "nope"}))
		}},
		// a staff session or token opens nothing of the portal, whoever holds it
		{"GET /api/v1/portal/me", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/api/v1/portal/me", nil) }},
		{"PUT /api/v1/portal/me", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("PUT", "/api/v1/portal/me", map[string]any{"name": "x"}) }},
		{"PUT /api/v1/portal/password", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("PUT", "/api/v1/portal/password", map[string]any{"password": "x"}) }},
		{"POST /api/v1/portal/logout", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("POST", "/api/v1/portal/logout", nil) }},
		{"GET /api/v1/projects/{key}/portal", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/portal", nil) }},
		{"PUT /api/v1/projects/{key}/portal", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/portal", map[string]any{"enabled": false})
		}},
		{"POST /api/v1/projects/{key}/portal/block", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("POST", "/api/v1/projects/ABC/portal/block", map[string]any{"email": "nobody@example.com", "block": true})
		}},
		{"GET /api/v1/projects/{key}/request-types", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/request-types", nil) }},
		{"POST /api/v1/projects/{key}/request-types", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/projects/ABC/request-types", map[string]any{"name": fmt.Sprint("Form ", f.n)})
		}},
		{"PUT /api/v1/request-types/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/request-types/"+f.requestType(), map[string]any{"help_text": "x"})
		}},
		{"DELETE /api/v1/request-types/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/request-types/"+f.requestType(), nil)
		}},
		// a staff session or token opens nothing of a customer's requests
		{"GET /api/v1/portal/{key}/request-types", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/api/v1/portal/ABC/request-types", nil) }},
		{"GET /api/v1/portal/{key}/requests", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/api/v1/portal/ABC/requests", nil) }},
		{"POST /api/v1/portal/{key}/requests", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("POST", "/api/v1/portal/ABC/requests", map[string]any{"summary": "x"}) }},
		{"GET /api/v1/portal/{key}/requests/{issue}", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/api/v1/portal/ABC/requests/"+f.issue(), nil) }},
		{"POST /api/v1/portal/{key}/requests/{issue}/comments", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("POST", "/api/v1/portal/ABC/requests/"+f.issue()+"/comments", map[string]any{"body": "x"})
		}},
		{"POST /api/v1/portal/{key}/requests/{issue}/attachments", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("POST", "/api/v1/portal/ABC/requests/"+f.issue()+"/attachments?name=x.txt", nil)
		}},
		{"GET /api/v1/portal/{key}/attachments/{id}", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/api/v1/portal/ABC/attachments/"+f.attachment(f.issue()), nil) }},
		{"DELETE /api/v1/portal/{key}/attachments/{id}", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/portal/ABC/attachments/"+f.attachment(f.issue()), nil)
		}},
		{"PUT /api/v1/attachments/{id}/customer-visible", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(400), admin: codes(400)}, func(f *fixture) req {
			return J("PUT", "/api/v1/attachments/"+f.attachment(f.issue())+"/customer-visible", map[string]any{"visible": true})
		}},
		{"GET /api/v1/projects/{key}/queues", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/queues", nil) }},
		{"POST /api/v1/projects/{key}/queues", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/projects/ABC/queues", map[string]any{"name": fmt.Sprint("Queue ", f.n), "tql": "statusCategory != Done"})
		}},
		{"PUT /api/v1/queues/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/queues/"+f.queue(), map[string]any{"position": 3})
		}},
		{"DELETE /api/v1/queues/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("DELETE", "/api/v1/queues/"+f.queue(), nil) }},
		{"GET /api/v1/queues/{id}/issues", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/queues/"+f.queue()+"/issues", nil) }},
		{"GET /api/v1/projects/{key}/canned-responses", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/canned-responses", nil) }},
		{"POST /api/v1/projects/{key}/canned-responses", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/projects/ABC/canned-responses", map[string]any{"name": fmt.Sprint("Reply ", f.n), "body": "Hello"})
		}},
		{"PUT /api/v1/canned-responses/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/canned-responses/"+f.canned(), map[string]any{"body": "Changed"})
		}},
		{"DELETE /api/v1/canned-responses/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/canned-responses/"+f.canned(), nil)
		}},
		{"POST /api/v1/portal/{key}/requests/{issue}/share", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("POST", "/api/v1/portal/ABC/requests/"+f.issue()+"/share", map[string]any{"organization": true})
		}},
		{"GET /api/v1/projects/{key}/portal/customer-orgs", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/portal/customer-orgs", nil) }},
		{"POST /api/v1/projects/{key}/portal/customer-orgs", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/projects/ABC/portal/customer-orgs", map[string]any{"name": fmt.Sprint("Company ", f.n)})
		}},
		{"DELETE /api/v1/projects/{key}/portal/customer-orgs/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/projects/ABC/portal/customer-orgs/"+f.customerOrg(), nil)
		}},
		{"PUT /api/v1/projects/{key}/portal/customer-orgs/{id}/members", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/portal/customer-orgs/"+f.customerOrg()+"/members", map[string]any{"add": []string{"member@company.example"}})
		}},
		{"GET /api/v1/portal/me/export", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/api/v1/portal/me/export", nil) }},
		{"POST /api/v1/portal/me/erase", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("POST", "/api/v1/portal/me/erase", map[string]any{"confirm": "x"}) }},
		{"GET /api/v1/admin/customers", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/customers", nil) }},
		{"GET /api/v1/admin/customers/{id}/personal-data", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("GET", "/api/v1/admin/customers/"+f.customer()+"/personal-data", nil)
		}},
		{"POST /api/v1/admin/customers/{id}/erase", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/admin/customers/"+f.customer()+"/erase", map[string]any{})
		}},
		{"POST /api/v1/admin/customers/{id}/promote", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/admin/customers/"+f.customer()+"/promote", map[string]any{"password": "a long enough one"})
		}},
		{"GET /api/v1/import/tools", authed, func(f *fixture) req { return J("GET", "/api/v1/import/tools", nil) }},
		{"POST /api/v1/import/{tool}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/import/asana", map[string]any{"project": "ABC", "dry_run": true, "data": "Task ID,Name\n" + fmt.Sprint(f.n) + ",A task\n"})
		}},
		{"GET /api/v1/auth/saml/{name}/metadata", exp{anon: codes(404), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("GET", "/api/v1/auth/saml/nope/metadata", nil) }},
		{"POST /api/v1/auth/saml/acs", exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return req{method: "POST", path: "/api/v1/auth/saml/acs", raw: []byte("SAMLResponse=eA%3D%3D&RelayState=nope"), headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}} // refused, and the browser is sent to the sign-in page
		}},
		{"GET /api/v1/dashboards", authed, func(f *fixture) req { return J("GET", "/api/v1/dashboards", nil) }},
		{"POST /api/v1/dashboards", exp{anon: codes(401), outsider: codes(201), viewer: codes(201), member: codes(201), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/dashboards", map[string]any{"name": "Matrix dashboard"})
		}},
		{"GET /api/v1/dashboards/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("GET", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009", nil) }},
		{"PUT /api/v1/dashboards/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009", map[string]any{"name": "x"})
		}},
		{"DELETE /api/v1/dashboards/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009", nil)
		}},
		{"POST /api/v1/dashboards/{id}/clone", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("POST", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009/clone", nil)
		}},
		{"PUT /api/v1/dashboards/{id}/favourite", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009/favourite", nil)
		}},
		{"DELETE /api/v1/dashboards/{id}/favourite", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009/favourite", nil)
		}},
		{"PUT /api/v1/me/default-dashboard", authed, func(f *fixture) req { return J("PUT", "/api/v1/me/default-dashboard", map[string]any{"id": ""}) }},
		{"POST /api/v1/dashboards/{id}/gadgets", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("POST", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009/gadgets", map[string]any{"kind": "text", "config": map[string]any{"markdown": "x"}})
		}},
		{"PUT /api/v1/dashboards/{id}/gadgets/{gid}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009/gadgets/00000000-0000-0000-0000-000000000009", map[string]any{"config": map[string]any{"markdown": "x"}})
		}},
		{"DELETE /api/v1/dashboards/{id}/gadgets/{gid}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009/gadgets/00000000-0000-0000-0000-000000000009", nil)
		}},
		{"GET /api/v1/dashboards/{id}/gadgets/{gid}/data", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("GET", "/api/v1/dashboards/00000000-0000-0000-0000-000000000009/gadgets/00000000-0000-0000-0000-000000000009/data", nil)
		}},
		{"POST /api/v1/login/passkey", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("POST", "/api/v1/login/passkey", map[string]any{"mfa_token": "nope", "id": "AA", "client_data_json": "AA", "authenticator_data": "AA", "signature": "AA"})
		}},
		{"GET /api/v1/me/passkeys", exp{anon: codes(401), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/me/passkeys", nil) }},
		{"POST /api/v1/me/passkeys/options", exp{anon: codes(401), outsider: codes(400), viewer: codes(400), member: codes(400), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/me/passkeys/options", map[string]any{"password": "x"}) }},
		{"POST /api/v1/me/passkeys", exp{anon: codes(401), outsider: codes(400), viewer: codes(400), member: codes(400), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/me/passkeys", map[string]any{"name": "k"}) }},
		{"DELETE /api/v1/me/passkeys/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req { return J("DELETE", "/api/v1/me/passkeys/1", map[string]any{"password": "wrong"}) }},
		{"POST /api/v1/login/mfa", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("POST", "/api/v1/login/mfa", map[string]any{"mfa_token": "nope", "code": "000000"})
		}},
		{"GET /api/v1/me/sessions", authed, func(f *fixture) req { return J("GET", "/api/v1/me/sessions", nil) }},
		{"DELETE /api/v1/me/sessions/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/me/sessions/999999", nil) }},
		{"POST /api/v1/me/sessions/revoke-others", authed, func(f *fixture) req { return J("POST", "/api/v1/me/sessions/revoke-others", nil) }},
		{"GET /api/v1/me/tokens", authed, func(f *fixture) req { return J("GET", "/api/v1/me/tokens", nil) }},
		{"POST /api/v1/me/tokens", exp{anon: codes(401), outsider: codes(201), viewer: codes(201), member: codes(201), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/me/tokens", map[string]any{"name": "matrix", "expires_in_days": 1})
		}},
		{"DELETE /api/v1/me/tokens/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/me/tokens/00000000-0000-0000-0000-000000000000", nil)
		}},
		{"POST /api/v1/me/password", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req {
			return J("POST", "/api/v1/me/password", map[string]any{"current": "wrong", "new": "another-password"})
		}},
		{"GET /api/v1/me/2fa", authed, func(f *fixture) req { return J("GET", "/api/v1/me/2fa", nil) }},
		{"POST /api/v1/me/2fa/setup", authed, func(f *fixture) req { return J("POST", "/api/v1/me/2fa/setup", nil) }},
		{"POST /api/v1/me/2fa/enable", exp{anon: codes(401), outsider: codes(400), viewer: codes(400), member: codes(400), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/me/2fa/enable", map[string]any{"code": "000000"}) }},
		{"DELETE /api/v1/me/2fa", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req { return J("DELETE", "/api/v1/me/2fa", map[string]any{"password": "wrong"}) }},
		{"POST /api/v1/users/{id}/revoke-access", adminer, func(f *fixture) req {
			return J("POST", "/api/v1/users/"+f.spare.ID+"/revoke-access", map[string]any{"tokens": true})
		}},
		{"POST /api/v1/users/{id}/2fa/reset", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("POST", "/api/v1/users/"+f.spare.ID+"/2fa/reset", nil) }},
		{"GET /api/v1/admin/sso/providers", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/sso/providers", nil) }},
		{"POST /api/v1/admin/sso/providers", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/admin/sso/providers", map[string]any{"name": fmt.Sprint("idp", f.n), "issuer": "https://idp.example.com", "client_id": "abc"})
		}},
		{"PUT /api/v1/admin/sso/providers/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/admin/sso/providers/999999", map[string]any{"name": "x", "issuer": "https://idp.example.com", "client_id": "abc"})
		}},
		{"DELETE /api/v1/admin/sso/providers/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/admin/sso/providers/999999", nil) }},
		{"GET /api/v1/auth/sso/providers", exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/auth/sso/providers", nil) }},
		{"GET /api/v1/auth/sso/{name}/start", exp{anon: codes(404), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("GET", "/api/v1/auth/sso/nosuch/start", nil) }},
		{"GET /api/v1/auth/sso/callback", exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/auth/sso/callback?code=x&state=y", nil) }}, // a failed sign-in is a redirect to the sign-in page, which the client follows
		{"GET /api/v1/admin/scim/tokens", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/scim/tokens", nil) }},
		{"POST /api/v1/admin/scim/tokens", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req { return J("POST", "/api/v1/admin/scim/tokens", map[string]any{"name": "okta"}) }},
		{"DELETE /api/v1/admin/scim/tokens/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/admin/scim/tokens/999999", nil) }},
		{"GET /scim/v2/ServiceProviderConfig", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/scim/v2/ServiceProviderConfig", nil) }},
		{"GET /scim/v2/Users", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/scim/v2/Users", nil) }},
		{"POST /scim/v2/Users", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("POST", "/scim/v2/Users", map[string]any{"userName": "x@example.com"}) }},
		{"GET /scim/v2/Users/{id}", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/scim/v2/Users/"+f.spare.ID, nil) }},
		{"PUT /scim/v2/Users/{id}", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("PUT", "/scim/v2/Users/"+f.spare.ID, map[string]any{"displayName": "x"})
		}},
		{"PATCH /scim/v2/Users/{id}", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("PATCH", "/scim/v2/Users/"+f.spare.ID, map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}}})
		}},
		{"DELETE /scim/v2/Users/{id}", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("DELETE", "/scim/v2/Users/"+f.spare.ID, nil) }},
		{"GET /.well-known/oauth-protected-resource", exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/.well-known/oauth-protected-resource", nil) }},
		{"GET /.well-known/oauth-protected-resource/mcp", exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/.well-known/oauth-protected-resource/mcp", nil) }},
		{"GET /.well-known/oauth-authorization-server", exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/.well-known/oauth-authorization-server", nil) }},
		{"POST /oauth/register", exp{anon: codes(201), outsider: codes(201), viewer: codes(201), member: codes(201), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/oauth/register", map[string]any{"client_name": "matrix client", "redirect_uris": []string{"http://127.0.0.1:9999/cb"}})
		}},
		{"GET /oauth/authorize", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req {
			return J("GET", "/oauth/authorize?client_id=nope&redirect_uri=http://127.0.0.1/cb", nil)
		}},
		{"GET /api/v1/oauth/consent", exp{anon: codes(401), outsider: codes(400), viewer: codes(400), member: codes(400), admin: codes(400)}, func(f *fixture) req {
			return J("GET", "/api/v1/oauth/consent?client_id=nope&redirect_uri=http://127.0.0.1/cb", nil)
		}},
		{"POST /api/v1/oauth/authorize", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req {
			return J("POST", "/api/v1/oauth/authorize", map[string]any{"client_id": "nope", "redirect_uri": "http://127.0.0.1/cb", "approve": true})
		}},
		{"POST /oauth/token", exp{anon: codes(400), outsider: codes(400), viewer: codes(400), member: codes(400), admin: codes(400)}, func(f *fixture) req { return J("POST", "/oauth/token", map[string]any{}) }},
		{"POST /oauth/revoke", exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("POST", "/oauth/revoke", map[string]any{}) }},
		{"GET /api/v1/me/oauth/grants", authed, func(f *fixture) req { return J("GET", "/api/v1/me/oauth/grants", nil) }},
		{"DELETE /api/v1/me/oauth/grants/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(404), member: codes(404), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/me/oauth/grants/999999", nil) }},
		{"GET /api/v1/admin/users/{id}/personal-data", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/users/"+f.spare.ID+"/personal-data", nil) }},
		{"POST /api/v1/admin/users/{id}/erase", adminer, func(f *fixture) req {
			return J("POST", "/api/v1/admin/users/"+f.eraseTarget()+"/erase", map[string]any{"delete_content": true, "scrub_mentions": true})
		}},
		{"GET /api/v1/groups", adminer, func(f *fixture) req { return J("GET", "/api/v1/groups", nil) }},
		{"POST /api/v1/groups", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/groups", map[string]any{"name": fmt.Sprint("New group ", f.n)})
		}},
		{"GET /api/v1/groups/{id}", adminer, func(f *fixture) req { return J("GET", "/api/v1/groups/"+f.group(), nil) }},
		{"DELETE /api/v1/groups/{id}", adminer, func(f *fixture) req { return J("DELETE", "/api/v1/groups/"+f.group(), nil) }},
		{"POST /api/v1/groups/{id}/members", adminer, func(f *fixture) req {
			return J("POST", "/api/v1/groups/"+f.group()+"/members", map[string]any{"user": f.spare.ID})
		}},
		{"DELETE /api/v1/groups/{id}/members/{user}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/groups/"+f.group()+"/members/"+f.spare.ID, nil)
		}},
		{"GET /api/v1/projects/{key}/groups", projAdmin, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/groups", nil) }},
		{"PUT /api/v1/projects/{key}/groups/{id}", projAdmin, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/groups/"+f.group(), map[string]any{"role": "viewer"})
		}},
		{"DELETE /api/v1/projects/{key}/groups/{id}", projAdmin, func(f *fixture) req { return J("DELETE", "/api/v1/projects/ABC/groups/"+f.group(), nil) }},
		{"GET /api/v1/projects/{key}/permissions", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/permissions", nil) }},
		{"PUT /api/v1/projects/{key}/permissions", projAdmin, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/permissions", map[string]any{"comment": ""})
		}},
		{"GET /api/v1/projects/{key}/storage", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/storage", nil) }},
		{"PUT /api/v1/projects/{key}/storage-quota", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/storage-quota", map[string]any{"mib": nil})
		}},
		{"GET /api/v1/admin/storage", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/storage", nil) }},
		{"GET /api/v1/users", authed, func(f *fixture) req { return J("GET", "/api/v1/users", nil) }},
		{"POST /api/v1/users", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/users", map[string]any{"email": fmt.Sprintf("m%d@example.com", f.n), "name": "M", "password": "password123"})
		}},
		{"GET /api/v1/projects", authed, func(f *fixture) req { return J("GET", "/api/v1/projects", nil) }},
		{"POST /api/v1/projects", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/projects", map[string]any{"key": fmt.Sprintf("M%c%c", 'A'+f.n%26, 'A'+(f.n/26)%26), "name": "m"})
		}},
		{"POST /api/v1/projects/{key}/members", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/projects/ABC/members", map[string]any{"user": f.spare.ID, "role": "viewer"})
		}},
		{"POST /api/v1/issues", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues", map[string]any{"project": "ABC", "summary": "from the matrix"})
		}},
		{"GET /api/v1/issues/{key}/view", reader, func(f *fixture) req { return J("GET", "/api/v1/issues/"+f.issue()+"/view", nil) }},
		{"PATCH /api/v1/issues/{key}", writer, func(f *fixture) req {
			return J("PATCH", "/api/v1/issues/"+f.issue(), map[string]any{"priority": "high"})
		}},
		{"POST /api/v1/issues/{key}/transitions", writer, func(f *fixture) req {
			k := f.issue()
			v, _ := f.e.Svc.View(testutil.Ctx(), f.e.Alice, k)
			return J("POST", "/api/v1/issues/"+k+"/transitions", map[string]any{"transition": v.Transitions[0].ID})
		}},
		{"GET /api/v1/issues/{key}/history", reader, func(f *fixture) req { return J("GET", "/api/v1/issues/"+f.issue()+"/history", nil) }},
		{"PUT /api/v1/issues/{key}/text/{field}", writer, func(f *fixture) req {
			return J("PUT", "/api/v1/issues/"+f.issue()+"/text/description", map[string]any{"body": "new", "base_seq": 1})
		}},
		{"POST /api/v1/issues/{key}/text/{field}/patch", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/text/description/patch", map[string]any{"old_text": "orig", "new_text": "o"})
		}},
		{"POST /api/v1/issues/{key}/text/{field}/replace", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/text/description/replace", map[string]any{"body": "replaced"})
		}},
		{"GET /api/v1/issues/{key}/text/{field}/versions", reader, func(f *fixture) req { return J("GET", "/api/v1/issues/"+f.issue()+"/text/description/versions", nil) }},
		{"POST /api/v1/issues/{key}/text/{field}/restore", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/text/description/restore", map[string]any{"seq": 1})
		}},
		{"GET /api/v1/issues/{key}/drafts", reader, func(f *fixture) req { return J("GET", "/api/v1/issues/"+f.issue()+"/drafts", nil) }},
		{"PUT /api/v1/issues/{key}/drafts/{field}", writer, func(f *fixture) req {
			return J("PUT", "/api/v1/issues/"+f.issue()+"/drafts/description", map[string]any{"body": "d", "base_seq": 1})
		}},
		{"DELETE /api/v1/issues/{key}/drafts/{field}", reader, func(f *fixture) req { return J("DELETE", "/api/v1/issues/"+f.issue()+"/drafts/description", nil) }},
		{"POST /api/v1/issues/{key}/comments", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/comments", map[string]any{"body": "hello"})
		}},
		{"PATCH /api/v1/issues/{key}/comments/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200), admin: codes(403)}, func(f *fixture) req {
			k := f.issue()
			return J("PATCH", "/api/v1/issues/"+k+"/comments/"+f.comment(k), map[string]any{"body": "edited"}) // only the author edits a comment
		}},
		{"POST /api/v1/issues/{key}/presence", reader, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/presence", map[string]any{"field": ""})
		}},
		{"DELETE /api/v1/issues/{key}/presence", authed, func(f *fixture) req { return J("DELETE", "/api/v1/issues/"+f.issue()+"/presence", nil) }},
		{"GET /api/v1/bootstrap", authed, func(f *fixture) req { return J("GET", "/api/v1/bootstrap", nil) }},
		{"GET /api/v1/admin/mail", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/mail", nil) }},
		{"POST /api/v1/mail/inbound/{id}", exp{anon: codes(403), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req {
			return J("POST", "/api/v1/mail/inbound/00000000-0000-0000-0000-000000000000", map[string]any{"x": 1})
		}}, // a source proves itself with a signature, never with a session
		{"GET /api/v1/projects/{key}/mail", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/mail", nil) }},
		{"PUT /api/v1/projects/{key}/mail-policy", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/mail-policy", map[string]any{"policy": "reject"})
		}},
		{"POST /api/v1/mail/bounces/{id}", exp{anon: codes(403), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req {
			return J("POST", "/api/v1/mail/bounces/00000000-0000-0000-0000-000000000000", map[string]any{"events": []any{}})
		}},
		{"GET /api/v1/system/status", exp{anon: codes(401), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/system/status", nil) }},
		{"GET /api/v1/admin/cluster", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/cluster", nil) }},
		{"POST /api/v1/admin/cluster/gates/{name}/confirm", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("POST", "/api/v1/admin/cluster/gates/nothing/confirm", nil) }},
		{"POST /api/v1/admin/mail/retention/run", adminer, func(f *fixture) req { return J("POST", "/api/v1/admin/mail/retention/run", nil) }},
		{"GET /api/v1/admin/mail/suppressions", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/mail/suppressions", nil) }},
		{"DELETE /api/v1/admin/mail/suppressions/{email}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/admin/mail/suppressions/nobody@example.org", nil) }},
		{"GET /api/v1/admin/mail/imap", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/mail/imap", nil) }},
		{"POST /api/v1/admin/mail/imap", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/admin/mail/imap", map[string]any{"name": fmt.Sprint("box", f.n), "host": "mail.example.org", "port": 993, "username": "u", "password": "p"})
		}},
		{"DELETE /api/v1/admin/mail/imap/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/admin/mail/imap/00000000-0000-0000-0000-000000000000", nil)
		}},
		{"POST /api/v1/admin/mail/imap/{id}/check", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("POST", "/api/v1/admin/mail/imap/00000000-0000-0000-0000-000000000000/check", nil)
		}},
		{"GET /api/v1/orgs", adminer, func(f *fixture) req { return J("GET", "/api/v1/orgs", nil) }},
		{"POST /api/v1/orgs", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/orgs", map[string]any{"name": fmt.Sprint("Org ", f.n), "admin_email": fmt.Sprint("boss", f.n, "@org.example"), "admin_name": "Boss", "admin_password": "password123"})
		}},
		{"GET /api/v1/me/org", authed, func(f *fixture) req { return J("GET", "/api/v1/me/org", nil) }},
		{"GET /api/v1/admin/storage/tiering", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/storage/tiering", nil) }},
		{"PUT /api/v1/admin/storage/tiering/rules", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/admin/storage/tiering/rules", map[string]any{"rules": []any{map[string]any{"name": "old logs", "class": "log", "min_age_days": 30, "enabled": true}}})
		}},
		{"POST /api/v1/admin/storage/tiering/run", adminer, func(f *fixture) req { return J("POST", "/api/v1/admin/storage/tiering/run", nil) }},
		{"GET /api/v1/projects/{key}/git/rules", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/git/rules", nil) }},
		{"POST /api/v1/projects/{key}/git/rules", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(201)}, func(f *fixture) req {
			return J("POST", "/api/v1/projects/ABC/git/rules", map[string]any{"name": "r", "trigger": "pr_merged", "comment": "merged", "mode": "off"})
		}},
		{"PUT /api/v1/projects/{key}/git/rules/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/git/rules/999999", map[string]any{"name": "r", "trigger": "pr_merged", "comment": "merged", "mode": "off"})
		}},
		{"DELETE /api/v1/projects/{key}/git/rules/{id}", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/projects/ABC/git/rules/999999", nil) }},
		{"GET /api/v1/projects/{key}/git/rule-runs", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/git/rule-runs", nil) }},
		{"GET /api/v1/projects/{key}/git/smart-commits", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/git/smart-commits", nil) }},
		{"PUT /api/v1/projects/{key}/git/smart-commits", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("PUT", "/api/v1/projects/ABC/git/smart-commits", map[string]any{"mode": "off"})
		}},
		{"GET /api/v1/git/identities", adminer, func(f *fixture) req { return J("GET", "/api/v1/git/identities", nil) }},
		{"POST /api/v1/git/identities", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/git/identities", map[string]any{"provider": "github", "login": fmt.Sprint("octo", f.n), "user": f.spare.ID})
		}},
		{"DELETE /api/v1/git/identities/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/git/identities/999999", nil) }},
		{"PUT /api/v1/git/connections/{id}/options", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			c, _ := f.gitConn()
			return J("PUT", "/api/v1/git/connections/"+c+"/options", map[string]any{"write_enabled": false})
		}},
		{"POST /api/v1/git/connections/{id}/check", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200, 400, 404, 500)}, func(f *fixture) req {
			c, _ := f.gitConn()
			return J("POST", "/api/v1/git/connections/"+c+"/check", nil)
		}},
		{"POST /api/v1/issues/{key}/git/branch", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(400), admin: codes(400)}, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/git/branch", map[string]any{})
		}},
		{"GET /api/v1/git/visibility", adminer, func(f *fixture) req { return J("GET", "/api/v1/git/visibility", nil) }},
		{"PUT /api/v1/git/visibility", adminer, func(f *fixture) req { return J("PUT", "/api/v1/git/visibility", map[string]any{"mode": "default"}) }},
		{"PUT /api/v1/git/repos/{id}/privacy", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("PUT", "/api/v1/git/repos/00000000-0000-0000-0000-000000000000/privacy", map[string]any{"private": true})
		}},
		{"GET /api/v1/admin/mail/inbound", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/mail/inbound", nil) }},
		{"GET /api/v1/admin/mail/inbound/{id}/raw", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("GET", "/api/v1/admin/mail/inbound/999999/raw", nil) }},
		{"POST /api/v1/admin/mail/inbound/{id}/release", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req { return J("POST", "/api/v1/admin/mail/inbound/999999/release", nil) }},
		{"DELETE /api/v1/admin/mail/inbound/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req { return J("DELETE", "/api/v1/admin/mail/inbound/999999", nil) }},
		{"GET /api/v1/admin/mail/sources", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/mail/sources", nil) }},
		{"POST /api/v1/admin/mail/sources", adminer, func(f *fixture) req {
			f.n++
			return J("POST", "/api/v1/admin/mail/sources", map[string]any{"name": fmt.Sprint("src", f.n)})
		}},
		{"DELETE /api/v1/admin/mail/sources/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/admin/mail/sources/00000000-0000-0000-0000-000000000000", nil)
		}},
		{"POST /api/v1/admin/mail/reply-keys", adminer, func(f *fixture) req { return J("POST", "/api/v1/admin/mail/reply-keys", nil) }},
		{"DELETE /api/v1/admin/mail/reply-keys/{kid}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req { return J("DELETE", "/api/v1/admin/mail/reply-keys/99", nil) }},
		{"GET /api/v1/admin/fields", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/fields", nil) }},
		{"PUT /api/v1/admin/fields/{field}/history", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/admin/fields/priority/history", map[string]any{"track": true})
		}},
		{"POST /api/v1/ws/ticket", authed, func(f *fixture) req { return J("POST", "/api/v1/ws/ticket", nil) }},
		// /mcp takes bearer tokens only (a personal token acts as its owner); anonymous callers get 401. Tool-level scope x role checks live in MCP-03.
		{"POST /mcp", exp{anon: codes(401), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
		}},
		{"GET /mcp", exp{anon: codes(401), outsider: codes(400), viewer: codes(400), member: codes(400), admin: codes(400)}, func(f *fixture) req { return J("GET", "/mcp", nil) }},
		{"DELETE /mcp", exp{anon: codes(401), outsider: codes(204), viewer: codes(204), member: codes(204), admin: codes(204)}, func(f *fixture) req { return J("DELETE", "/mcp", nil) }},
		{"GET /api/v1/ws", exp{anon: codes(401), outsider: codes(401), viewer: codes(401), member: codes(401), admin: codes(401)}, func(f *fixture) req { return J("GET", "/api/v1/ws", nil) }},
		{"GET /api/v1/issues", authed, func(f *fixture) req { return J("GET", "/api/v1/issues?q=", nil) }},
		{"GET /api/v1/search/typeahead", authed, func(f *fixture) req { return J("GET", "/api/v1/search/typeahead?q=mat", nil) }},
		{"POST /api/v1/issues/{key}/attachments", writer, func(f *fixture) req {
			return req{method: "POST", path: "/api/v1/issues/" + f.issue() + "/attachments?name=a.txt", raw: []byte("raw body"), headers: map[string]string{"Content-Type": "text/plain"}}
		}},
		{"POST /api/v1/issues/{key}/uploads", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/uploads", map[string]any{"name": "x.bin", "size": 10})
		}},
		{"HEAD /api/v1/uploads/{id}", owner(200), func(f *fixture) req { return J("HEAD", "/api/v1/uploads/"+f.upload(f.issue()), nil) }},
		{"GET /api/v1/uploads/{id}", owner(200), func(f *fixture) req { return J("GET", "/api/v1/uploads/"+f.upload(f.issue()), nil) }},
		{"PATCH /api/v1/uploads/{id}", owner(200), func(f *fixture) req {
			return req{method: "PATCH", path: "/api/v1/uploads/" + f.upload(f.issue()), raw: []byte("12345"), headers: map[string]string{"Upload-Offset": "0"}}
		}},
		{"POST /api/v1/uploads/{id}/commit", owner(409), func(f *fixture) req { return J("POST", "/api/v1/uploads/"+f.upload(f.issue())+"/commit", nil) }},
		{"DELETE /api/v1/issues/{key}/attachments/{id}", writer, func(f *fixture) req {
			k := f.issue()
			return J("DELETE", "/api/v1/issues/"+k+"/attachments/"+f.attachment(k), nil)
		}},
		{"POST /api/v1/attachments/{id}/thumbnail", writer, func(f *fixture) req {
			return J("POST", "/api/v1/attachments/"+f.attachment(f.issue())+"/thumbnail", nil)
		}},
		{"POST /api/v1/attachments/{id}/link", reader, func(f *fixture) req { return J("POST", "/api/v1/attachments/"+f.attachment(f.issue())+"/link", nil) }},
		{"GET /api/v1/attachments/{id}/download", reader, func(f *fixture) req { return J("GET", "/api/v1/attachments/"+f.attachment(f.issue())+"/download", nil) }},
		{"POST /api/v1/issues/{key}/lifecycle", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/lifecycle", map[string]any{"op": "clone"})
		}},
		{"GET /api/v1/projects/{key}/trash", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req { return J("GET", "/api/v1/projects/ABC/trash", nil) }},
		{"POST /api/v1/issues/{key}/links", writer, func(f *fixture) req {
			return J("POST", "/api/v1/issues/"+f.issue()+"/links", map[string]any{"type": "relates", "target": "ABC-1"})
		}},
		{"PUT /api/v1/issues/{key}/legal-hold", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/issues/"+f.issue()+"/legal-hold", map[string]any{"hold": false})
		}},
		{"GET /api/v1/admin/pause", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/pause", nil) }},
		{"POST /api/v1/admin/pause", adminer, func(f *fixture) req {
			return J("POST", "/api/v1/admin/pause", map[string]any{"scope": "instance", "mode": "none"}) // none holds nothing, so the matrix itself is not paused
		}},
		{"DELETE /api/v1/admin/pause/{id}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("DELETE", "/api/v1/admin/pause/00000000-0000-4000-8000-000000000000", nil)
		}},
		{"POST /api/v1/export", exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/export", map[string]any{"attachments": false})
		}},
		{"POST /api/v1/import", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(400)}, func(f *fixture) req {
			return req{method: "POST", path: "/api/v1/import?dry_run=1", raw: []byte("not a bundle")} // an administrator is let in and told the upload is not a bundle
		}},
		{"POST /api/v1/import/{id}/rollback", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("POST", "/api/v1/import/00000000-0000-4000-8000-000000000000/rollback", nil)
		}},
		{"POST /api/v1/import/csv", exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(403), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/api/v1/import/csv", map[string]any{"project": "ABC", "csv": "title\nfrom csv", "mapping": map[string]any{"columns": map[string]string{"title": "summary"}}})
		}},
		{"PUT /api/v1/import/csv/templates/{name}", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/import/csv/templates/t", map[string]any{"columns": map[string]string{"title": "summary"}})
		}},
		{"GET /api/v1/import/csv/templates/{name}", exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(404)}, func(f *fixture) req {
			return J("GET", "/api/v1/import/csv/templates/does-not-exist", nil)
		}},
		{"GET /files/{id}", exp{anon: codes(403), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(403)}, func(f *fixture) req { return J("GET", "/files/"+f.attachment(f.issue()), nil) }},
	}
}

func matrixEnv(t *testing.T) (*fixture, map[role]string, *httptest.Server, *api.Server) {
	e := testutil.New(t)
	e.Project()
	_, mock := e.WithS3Blobs() // the matrix runs on the S3 backend, which has every route the filesystem one has and more
	srv := &api.Server{Svc: e.Svc, Hub: rt.New(e.Svc), SessionTTL: time.Hour}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx := testutil.Ctx()
	tok := map[role]string{}
	mk := func(r role, a domain.Actor) { tok[r], _ = e.Svc.CreateAPIToken(ctx, a.ID, string(r)) }
	ou, _ := e.Svc.CreateUser(ctx, "eve@example.com", "Eve", "password123", false)
	vu, _ := e.Svc.CreateUser(ctx, "vera@example.com", "Vera", "password123", false)
	e.Svc.AddMember(ctx, e.Admin, "ABC", vu.ID, "viewer")
	spare, _ := e.Svc.CreateUser(ctx, "spare@example.com", "Spare", "password123", false)
	mk(outsider, domain.Actor{ID: ou.ID, Label: "Eve"})
	mk(viewer, domain.Actor{ID: vu.ID, Label: "Vera"})
	mk(member, e.Alice)
	mk(admin, e.Admin)
	return &fixture{t: t, e: e, spare: spare, mock: mock}, tok, ts, srv
}

func do(t *testing.T, ts *httptest.Server, tok string, r req) (int, string) {
	var body io.Reader = bytes.NewReader(nil)
	if r.raw != nil {
		body = bytes.NewReader(r.raw)
	} else if r.body != nil {
		b, _ := json.Marshal(r.body)
		body = bytes.NewReader(b)
	}
	hr, _ := http.NewRequest(r.method, ts.URL+r.path, body)
	if tok != "" {
		hr.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatalf("%s %s: %v", r.method, r.path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func has(c []int, v int) bool {
	for _, x := range c {
		if x == v {
			return true
		}
	}
	return false
}

func TestSEC01_EveryRouteCoveredByTheMatrix(t *testing.T) {
	_, _, _, srv := matrixEnv(t)
	registered := map[string]bool{}
	for _, p := range srv.Routes() {
		registered[p] = true
	}
	covered := map[string]bool{}
	for _, r := range rows() {
		if covered[r.pattern] {
			t.Errorf("duplicate matrix row for %s", r.pattern)
		}
		covered[r.pattern] = true
	}
	var missing, stale []string
	for p := range registered {
		if !covered[p] {
			missing = append(missing, p)
		}
	}
	for p := range covered {
		if !registered[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Fatalf("routes without an authorization matrix row (add one before adding a route):\n  %s", strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Fatalf("matrix rows for routes that no longer exist:\n  %s", strings.Join(stale, "\n  "))
	}
	t.Logf("%d routes, %d roles, %d checks", len(registered), len(roles), len(registered)*len(roles))
}

func TestSEC01_AuthorizationMatrix(t *testing.T) {
	f, tok, ts, _ := matrixEnv(t)
	for _, r := range rows() {
		for _, ro := range roles { // one fresh resource per request, so successes cannot affect later checks
			q := r.build(f)
			token := tok[ro]
			code, body := do(t, ts, token, q)
			if !has(r.exp[ro], code) {
				t.Errorf("%-52s as %-9s: got %d, want %v\n    %s", r.pattern, ro, code, r.exp[ro], strings.TrimSpace(body))
			}
			if code >= 500 {
				t.Errorf("%s as %s: server error %d", r.pattern, ro, code)
			}
		}
	}
}

// Errors must not reveal whether something exists: for a person with no access, an issue they cannot see and an issue that
// does not exist give byte-identical responses on every route that takes a key.
func TestSEC01_NoExistenceLeakInErrors(t *testing.T) {
	f, tok, ts, _ := matrixEnv(t)
	for _, r := range rows() {
		if !strings.Contains(r.pattern, "{key}") || strings.Contains(r.pattern, "/projects/") {
			continue
		}
		q := r.build(f)
		real := q
		ghost := q
		parts := strings.Split(q.path, "/")
		for i, p := range parts {
			if strings.HasPrefix(p, "ABC-") {
				parts[i] = "ABC-99999"
			}
		}
		ghost.path = strings.Join(parts, "/")
		cr, br := do(t, ts, tok[outsider], real)
		cg, bg := do(t, ts, tok[outsider], ghost)
		if cr != cg || br != bg {
			t.Errorf("%s: an issue the outsider cannot see answers differently from one that does not exist:\n  exists:  %d %s\n  missing: %d %s", r.pattern, cr, strings.TrimSpace(br), cg, strings.TrimSpace(bg))
		}
	}
	// the same for projects: creating an issue in, or adding members to, a project one cannot see
	for _, c := range []struct{ method, path string }{{"POST", "/api/v1/projects/%s/members"}, {"POST", "/api/v1/issues"}} {
		mk := func(key string) req {
			if strings.Contains(c.path, "%s") {
				return req{method: c.method, path: fmt.Sprintf(c.path, key), body: map[string]any{"user": f.spare.ID, "role": "viewer"}}
			}
			return req{method: c.method, path: c.path, body: map[string]any{"project": key, "summary": "x"}}
		}
		cr, br := do(t, ts, tok[outsider], mk("ABC"))
		cg, bg := do(t, ts, tok[outsider], mk("ZZZ"))
		if cr != cg || br != bg {
			t.Errorf("%s %s: existing project %d %q, missing project %d %q", c.method, c.path, cr, br, cg, bg)
		}
	}
}

// What a list or search returns is scoped to what the caller may read, whatever the query.
func TestSEC01_ListsAreScoped(t *testing.T) {
	f, tok, ts, _ := matrixEnv(t)
	f.issue()
	for _, q := range []string{"", "project = ABC", "text ~ matrix", "key = ABC-1"} {
		code, body := do(t, ts, tok[outsider], req{method: "GET", path: "/api/v1/issues?q=" + strings.ReplaceAll(q, " ", "%20")})
		if code != 200 || !strings.Contains(body, `"total":0`) {
			t.Errorf("outsider query %q: %d %s", q, code, body)
		}
	}
	code, body := do(t, ts, tok[outsider], req{method: "GET", path: "/api/v1/projects"})
	if code != 200 || strings.Contains(body, "ABC") {
		t.Errorf("outsider sees the project: %d %s", code, body)
	}
	code, body = do(t, ts, tok[outsider], req{method: "GET", path: "/api/v1/search/typeahead?q=ABC"})
	if code != 200 || strings.Contains(body, "ABC-") {
		t.Errorf("outsider typeahead leaks: %s", body)
	}
}
