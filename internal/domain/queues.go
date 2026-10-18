package domain

import (
	"context"
	"strings"

	"github.com/larytet/tasklane/internal/search"
)

// Queues and canned responses (SPEC 15). A queue is a named TQL query held to one project, shown to agents with the number of issues
// in it for them: what they see is the query run with their own permissions, never wider than the project. A canned response is a
// saved reply an agent can insert into a comment (where {{issue.key}}, {{customer}} and {{agent}} are filled in by the page).
// Project administrators manage both; anyone who can see the project sees the queues, and people who can comment see the responses.

type Queue struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Name     string `json:"name"`
	TQL      string `json:"tql"`
	Position int    `json:"position"`
	Count    int    `json:"count"`            // issues in it, for the viewer
	Capped   bool   `json:"capped,omitempty"` // the real count is at least this
	Error    string `json:"error,omitempty"`  // the query no longer runs (a field was removed, for example)
}

type QueueIn struct {
	Name     *string `json:"name"`
	TQL      *string `json:"tql"`
	Position *int    `json:"position"`
}

func (s *Service) queueAdmin(ctx context.Context, a Actor, projectKey string) (string, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return "", err
	}
	switch s.role(ctx, a, pid) {
	case "":
		return "", ErrNotFound
	case "admin":
		return pid, nil
	}
	return "", ErrForbidden
}

func queueErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "queue_name_uq") {
		return invalid("this project already has a queue with that name")
	}
	return err
}

// checkQueueQuery refuses a query that does not parse or compile, saying where.
func (s *Service) checkQueueQuery(ctx context.Context, a Actor, q string) error {
	if strings.TrimSpace(q) == "" {
		return invalid("a queue needs a query")
	}
	if len(q) > 2000 {
		return invalid("a query is at most 2000 characters")
	}
	sc, err := s.scopeFor(ctx, a)
	if err != nil {
		return err
	}
	if _, err := (&search.Postgres{DB: s.DB}).Search(ctx, sc, q, search.Page{Limit: 1}); err != nil {
		if _, isQuery := search.IsQueryError(err); isQuery {
			return invalid("the query is not valid: %v", err)
		}
		return err
	}
	return nil
}

func (s *Service) CreateQueue(ctx context.Context, a Actor, projectKey string, in QueueIn) (Queue, error) {
	pid, err := s.queueAdmin(ctx, a, projectKey)
	if err != nil {
		return Queue{}, err
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100 {
		return Queue{}, invalid("a queue needs a name of at most 100 characters")
	}
	q := ""
	if in.TQL != nil {
		q = *in.TQL
	}
	if strings.TrimSpace(q) == "" {
		return Queue{}, invalid("a queue needs a query")
	}
	if err := s.checkQueueQuery(ctx, a, andQuery(q, "project = "+strings.ToUpper(projectKey))); err != nil {
		return Queue{}, err
	}
	pos := 0
	if in.Position != nil {
		pos = *in.Position
	}
	var id string
	if err := s.DB.QueryRow(ctx, `INSERT INTO queue(project_id,name,tql,position) VALUES($1,$2,$3,$4) RETURNING id::text`, pid, strings.TrimSpace(*in.Name), q, pos).Scan(&id); err != nil {
		return Queue{}, queueErr(err)
	}
	return s.queueByID(ctx, a, id, true)
}

func (s *Service) queueByID(ctx context.Context, a Actor, id string, withCount bool) (Queue, error) {
	var q Queue
	var pid string
	err := s.DB.QueryRow(ctx, `SELECT qu.id::text,p.key,qu.name,qu.tql,qu.position,p.id::text FROM queue qu JOIN project p ON p.id=qu.project_id WHERE qu.id::text=$1`, id).Scan(&q.ID, &q.Project, &q.Name, &q.TQL, &q.Position, &pid)
	if err != nil {
		return q, ErrNotFound // also a malformed id
	}
	if s.role(ctx, a, pid) == "" {
		return q, ErrNotFound
	}
	if withCount {
		s.countQueue(ctx, a, &q)
	}
	return q, nil
}

func (s *Service) countQueue(ctx context.Context, a Actor, q *Queue) {
	sc, err := s.scopeFor(ctx, a)
	if err != nil {
		q.Error = err.Error()
		return
	}
	res, err := (&search.Postgres{DB: s.DB}).Search(ctx, sc, andQuery(q.TQL, "project = "+q.Project), search.Page{Limit: 1})
	if err != nil {
		q.Error = "this query no longer runs: " + err.Error()
		return
	}
	q.Count, q.Capped = res.Total, res.TotalCapped
}

// ListQueues lists a project's queues in order, each with its count for the caller.
func (s *Service) ListQueues(ctx context.Context, a Actor, projectKey string) ([]Queue, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	if s.role(ctx, a, pid) == "" {
		return nil, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT qu.id::text,p.key,qu.name,qu.tql,qu.position FROM queue qu JOIN project p ON p.id=qu.project_id WHERE qu.project_id=$1 ORDER BY qu.position,lower(qu.name)`, pid)
	if err != nil {
		return nil, err
	}
	out := []Queue{}
	for rows.Next() {
		var q Queue
		rows.Scan(&q.ID, &q.Project, &q.Name, &q.TQL, &q.Position)
		out = append(out, q)
	}
	rows.Close()
	for i := range out {
		s.countQueue(ctx, a, &out[i])
	}
	return out, nil
}

func (s *Service) UpdateQueue(ctx context.Context, a Actor, id string, in QueueIn) (Queue, error) {
	q, err := s.queueByID(ctx, a, id, false)
	if err != nil {
		return q, err
	}
	if _, err := s.queueAdmin(ctx, a, q.Project); err != nil {
		return q, err
	}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100 {
			return q, invalid("a queue needs a name of at most 100 characters")
		}
		q.Name = strings.TrimSpace(*in.Name)
	}
	if in.TQL != nil {
		q.TQL = *in.TQL
	}
	if in.Position != nil {
		q.Position = *in.Position
	}
	if strings.TrimSpace(q.TQL) == "" {
		return q, invalid("a queue needs a query")
	}
	if err := s.checkQueueQuery(ctx, a, andQuery(q.TQL, "project = "+q.Project)); err != nil {
		return q, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE queue SET name=$2,tql=$3,position=$4 WHERE id::text=$1`, id, q.Name, q.TQL, q.Position); err != nil {
		return q, queueErr(err)
	}
	return s.queueByID(ctx, a, id, true)
}

func (s *Service) DeleteQueue(ctx context.Context, a Actor, id string) error {
	q, err := s.queueByID(ctx, a, id, false)
	if err != nil {
		return err
	}
	if _, err := s.queueAdmin(ctx, a, q.Project); err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM queue WHERE id::text=$1`, id)
	return err
}

// QueueIssues is what is in a queue for the caller: the queue's query run with the caller's permissions and held to the queue's project.
func (s *Service) QueueIssues(ctx context.Context, a Actor, id string, limit int) (*search.Result, error) {
	q, err := s.queueByID(ctx, a, id, false)
	if err != nil {
		return nil, err
	}
	sc, err := s.scopeFor(ctx, a)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	return (&search.Postgres{DB: s.DB}).Search(ctx, sc, andQuery(q.TQL, "project = "+q.Project), search.Page{Limit: limit})
}

// ---- canned responses ----

type CannedResponse struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	Name    string `json:"name"`
	Body    string `json:"body"`
}

type CannedIn struct {
	Name *string `json:"name"`
	Body *string `json:"body"`
}

// ListCanned lists a project's saved replies for people who can comment in it.
func (s *Service) ListCanned(ctx context.Context, a Actor, projectKey string) ([]CannedResponse, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	switch role := s.role(ctx, a, pid); {
	case role == "":
		return nil, ErrNotFound
	case !canWrite(role):
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT c.id::text,p.key,c.name,c.body FROM canned_response c JOIN project p ON p.id=c.project_id WHERE c.project_id=$1 ORDER BY lower(c.name)`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CannedResponse{}
	for rows.Next() {
		var c CannedResponse
		rows.Scan(&c.ID, &c.Project, &c.Name, &c.Body)
		out = append(out, c)
	}
	return out, rows.Err()
}

func cannedErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "canned_name_uq") {
		return invalid("this project already has a canned response with that name")
	}
	return err
}

func checkCanned(name, body string) (string, string, error) {
	name, body = strings.TrimSpace(name), strings.TrimSpace(body)
	if name == "" || len([]rune(name)) > 100 {
		return "", "", invalid("a canned response needs a name of at most 100 characters")
	}
	if body == "" {
		return "", "", invalid("a canned response needs text")
	}
	if len([]rune(body)) > 10000 {
		return "", "", invalid("a canned response is at most 10000 characters")
	}
	return name, body, nil
}

func (s *Service) CreateCanned(ctx context.Context, a Actor, projectKey string, in CannedIn) (CannedResponse, error) {
	pid, err := s.queueAdmin(ctx, a, projectKey)
	if err != nil {
		return CannedResponse{}, err
	}
	if in.Name == nil || in.Body == nil {
		return CannedResponse{}, invalid("a canned response needs a name and text")
	}
	name, body, err := checkCanned(*in.Name, *in.Body)
	if err != nil {
		return CannedResponse{}, err
	}
	var c CannedResponse
	err = s.DB.QueryRow(ctx, `INSERT INTO canned_response(project_id,name,body) VALUES($1,$2,$3) RETURNING id::text,$4,name,body`, pid, name, body, strings.ToUpper(projectKey)).Scan(&c.ID, &c.Project, &c.Name, &c.Body)
	return c, cannedErr(err)
}

func (s *Service) cannedByID(ctx context.Context, a Actor, id string) (CannedResponse, error) {
	var c CannedResponse
	var pid string
	if err := s.DB.QueryRow(ctx, `SELECT c.id::text,p.key,c.name,c.body,p.id::text FROM canned_response c JOIN project p ON p.id=c.project_id WHERE c.id::text=$1`, id).Scan(&c.ID, &c.Project, &c.Name, &c.Body, &pid); err != nil {
		return c, ErrNotFound
	}
	if s.role(ctx, a, pid) == "" {
		return c, ErrNotFound
	}
	return c, nil
}

func (s *Service) UpdateCanned(ctx context.Context, a Actor, id string, in CannedIn) (CannedResponse, error) {
	c, err := s.cannedByID(ctx, a, id)
	if err != nil {
		return c, err
	}
	if _, err := s.queueAdmin(ctx, a, c.Project); err != nil {
		return c, err
	}
	if in.Name != nil {
		c.Name = *in.Name
	}
	if in.Body != nil {
		c.Body = *in.Body
	}
	if c.Name, c.Body, err = checkCanned(c.Name, c.Body); err != nil {
		return c, err
	}
	_, err = s.DB.Exec(ctx, `UPDATE canned_response SET name=$2,body=$3 WHERE id::text=$1`, id, c.Name, c.Body)
	return c, cannedErr(err)
}

func (s *Service) DeleteCanned(ctx context.Context, a Actor, id string) error {
	c, err := s.cannedByID(ctx, a, id)
	if err != nil {
		return err
	}
	if _, err := s.queueAdmin(ctx, a, c.Project); err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM canned_response WHERE id::text=$1`, id)
	return err
}
