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

type exp map[role][]int

var (
	public  = exp{anon: codes(200), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}
	authed  = exp{anon: codes(401), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}
	reader  = exp{anon: codes(401), outsider: codes(404), viewer: codes(200), member: codes(200), admin: codes(200)}
	writer  = exp{anon: codes(401), outsider: codes(404), viewer: codes(403), member: codes(200, 201), admin: codes(200, 201)}
	adminer = exp{anon: codes(401), outsider: codes(403), viewer: codes(403), member: codes(403), admin: codes(200, 201)}
	owner   = func(ok ...int) exp { // only the person who created it; everyone else sees nothing, administrators included
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
		{"GET /api/v1/admin/fields", adminer, func(f *fixture) req { return J("GET", "/api/v1/admin/fields", nil) }},
		{"PUT /api/v1/admin/fields/{field}/history", adminer, func(f *fixture) req {
			return J("PUT", "/api/v1/admin/fields/priority/history", map[string]any{"track": true})
		}},
		{"POST /api/v1/ws/ticket", authed, func(f *fixture) req { return J("POST", "/api/v1/ws/ticket", nil) }},
		// /mcp takes bearer tokens only (a personal token acts as its owner); anonymous callers get 401. Tool-level scope x role checks live in MCP-03.
		{"POST /mcp", exp{anon: codes(401), outsider: codes(200), viewer: codes(200), member: codes(200), admin: codes(200)}, func(f *fixture) req {
			return J("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
		}},
		{"GET /mcp", exp{anon: codes(401), outsider: codes(405), viewer: codes(405), member: codes(405), admin: codes(405)}, func(f *fixture) req { return J("GET", "/mcp", nil) }},
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
