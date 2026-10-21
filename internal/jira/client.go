package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Jira Cloud site with one API token (basic authentication): the sync's service account, or one person's own
// token for a write (write.go). Reads honour 429 and Retry-After and pace themselves so a backfill cannot starve other users of the
// site's quota; a write is sent once and never repeated (SPEC 18.1.1).
type Client struct {
	Site     string // https://example.atlassian.net
	Email    string
	Token    string
	HTTP     *http.Client
	MaxWait  time.Duration // the longest a rate limit may make us sleep before it is reported
	MinGap   time.Duration // pause between requests (the per-site token bucket, simplified)
	Sleep    func(time.Duration)
	Now      func() time.Time
	Requests int
	lastReq  time.Time
	tz       *time.Location
}

type ErrRateLimited struct{ Until time.Time }

func (e *ErrRateLimited) Error() string {
	return "Jira's rate limit is used up until " + e.Until.UTC().Format(time.RFC3339)
}

type ErrStatus struct {
	Status int
	Path   string
	Body   string
}

func (e *ErrStatus) Error() string { return fmt.Sprintf("jira %s: %d %s", e.Path, e.Status, e.Body) }

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
	} else {
		time.Sleep(d)
	}
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, "GET", path, q, nil, out, true)
}

// send makes one write. A write is never repeated by the client: a rate limit or an error is reported at once, because a repeated
// POST could add a comment twice and a repeated transition could apply a decision made on stale data (SPEC 18.1.1, WT-07, WT-09).
func (c *Client) send(ctx context.Context, method, path string, body, out any) error {
	return c.do(ctx, method, path, nil, body, out, false)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any, retry bool) error {
	u := strings.TrimSuffix(c.Site, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	max := c.MaxWait
	if max == 0 {
		max = 30 * time.Second
	}
	for attempt := 0; ; attempt++ {
		if gap := c.MinGap - c.now().Sub(c.lastReq); gap > 0 {
			c.sleep(gap)
		}
		c.lastReq = c.now()
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		req.SetBasicAuth(c.Email, c.Token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		c.Requests++
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode/100 == 2:
			if out == nil || len(bytes.TrimSpace(rb)) == 0 {
				return nil
			}
			if err := json.Unmarshal(rb, out); err != nil {
				return fmt.Errorf("jira %s: unexpected response: %v", path, err)
			}
			return nil
		case resp.StatusCode == 429 || (resp.StatusCode == 503 && resp.Header.Get("Retry-After") != ""):
			wait := time.Minute
			if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = time.Duration(n) * time.Second
			}
			if !retry || wait > max || attempt >= 4 {
				return &ErrRateLimited{Until: c.now().Add(wait)}
			}
			c.sleep(wait)
		case retry && resp.StatusCode >= 500 && attempt < 3:
			c.sleep(time.Duration(200<<attempt) * time.Millisecond)
		default:
			b := strings.TrimSpace(string(rb))
			if len(b) > 2000 && !retry {
				b = b[:2000] // a refusal of a write carries Jira's messages by field: keep them
			} else if len(b) > 200 && retry {
				b = b[:200]
			}
			return &ErrStatus{Status: resp.StatusCode, Path: path, Body: b}
		}
	}
}

type Myself struct {
	AccountID string `json:"accountId"`
	Email     string `json:"emailAddress"`
	Name      string `json:"displayName"`
	TimeZone  string `json:"timeZone"`
}

// Myself proves the credentials and reports the service account's time zone, which JQL uses for date literals.
func (c *Client) Myself(ctx context.Context) (Myself, error) {
	var m Myself
	err := c.get(ctx, "/rest/api/3/myself", nil, &m)
	if err == nil && m.TimeZone != "" {
		c.tz, _ = time.LoadLocation(m.TimeZone)
	}
	return m, err
}

// JQLTime formats an instant the way JQL wants it: minute precision, in the account's time zone.
func (c *Client) JQLTime(t time.Time) string {
	loc := c.tz
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006/01/02 15:04")
}

type Issue map[string]any

func (i Issue) ID() string  { s, _ := i["id"].(string); return s }
func (i Issue) Key() string { s, _ := i["key"].(string); return s }
func (i Issue) Fields() map[string]any {
	f, _ := i["fields"].(map[string]any)
	return f
}

type Page struct {
	Issues []Issue
	Next   string
}

// Search runs a JQL query and returns one page (follow Next until it is empty).
func (c *Client) Search(ctx context.Context, jql string, fields []string, expand, token string, max int) (Page, error) {
	q := url.Values{"jql": {jql}, "fields": {strings.Join(fields, ",")}, "maxResults": {strconv.Itoa(max)}}
	if expand != "" {
		q.Set("expand", expand)
	}
	if token != "" {
		q.Set("nextPageToken", token)
	}
	var out struct {
		Issues []Issue `json:"issues"`
		Next   string  `json:"nextPageToken"`
	}
	if err := c.get(ctx, "/rest/api/3/search/jql", q, &out); err != nil {
		return Page{}, err
	}
	return Page{Issues: out.Issues, Next: out.Next}, nil
}

type Comment map[string]any

func (c *Client) Comments(ctx context.Context, issueID string) ([]Comment, error) {
	var all []Comment
	for start := 0; ; {
		var out struct {
			Comments   []Comment `json:"comments"`
			Total      int       `json:"total"`
			StartAt    int       `json:"startAt"`
			MaxResults int       `json:"maxResults"`
		}
		if err := c.get(ctx, "/rest/api/3/issue/"+issueID+"/comment", url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {"100"}}, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Comments...)
		start += len(out.Comments)
		if len(out.Comments) == 0 || start >= out.Total {
			return all, nil
		}
	}
}

type History map[string]any

func (c *Client) Changelog(ctx context.Context, issueID string) ([]History, error) {
	var all []History
	for start := 0; ; {
		var out struct {
			Values []History `json:"values"`
			Total  int       `json:"total"`
		}
		if err := c.get(ctx, "/rest/api/3/issue/"+issueID+"/changelog", url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {"100"}}, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Values...)
		start += len(out.Values)
		if len(out.Values) == 0 || start >= out.Total {
			return all, nil
		}
	}
}

// ProjectRoles returns role name -> role URL for a project.
func (c *Client) ProjectRoles(ctx context.Context, key string) (map[string]string, error) {
	var out map[string]string
	err := c.get(ctx, "/rest/api/3/project/"+url.PathEscape(key)+"/role", nil, &out)
	return out, err
}

type Actor struct {
	Type      string
	AccountID string
	Group     string
	Name      string
}

// RoleActors returns the users and groups in a project role (the role URL as given by ProjectRoles).
func (c *Client) RoleActors(ctx context.Context, roleURL string) ([]Actor, error) {
	u, err := url.Parse(roleURL)
	if err != nil {
		return nil, err
	}
	var out struct {
		Actors []struct {
			Type      string `json:"type"`
			Name      string `json:"displayName"`
			ActorUser *struct {
				AccountID string `json:"accountId"`
			} `json:"actorUser"`
			ActorGroup *struct {
				Name string `json:"name"`
			} `json:"actorGroup"`
		} `json:"actors"`
	}
	if err := c.get(ctx, u.Path, nil, &out); err != nil {
		return nil, err
	}
	var as []Actor
	for _, a := range out.Actors {
		x := Actor{Type: a.Type, Name: a.Name}
		if a.ActorUser != nil {
			x.AccountID = a.ActorUser.AccountID
		}
		if a.ActorGroup != nil {
			x.Group = a.ActorGroup.Name
		}
		as = append(as, x)
	}
	return as, nil
}

type User struct {
	AccountID string `json:"accountId"`
	Email     string `json:"emailAddress"`
	Name      string `json:"displayName"`
	Active    bool   `json:"active"`
}

func (c *Client) GroupMembers(ctx context.Context, group string) ([]User, error) {
	var all []User
	for start := 0; ; {
		var out struct {
			Values []User `json:"values"`
			Total  int    `json:"total"`
		}
		if err := c.get(ctx, "/rest/api/3/group/member", url.Values{"groupname": {group}, "startAt": {strconv.Itoa(start)}, "maxResults": {"50"}}, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Values...)
		start += len(out.Values)
		if len(out.Values) == 0 || start >= out.Total {
			return all, nil
		}
	}
}

// AssignableUsers returns the active people Jira lets issues of the project be assigned to. It needs only ordinary access to the
// project (not the right to administer it), so it stands in for the project roles when Jira refuses those.
func (c *Client) AssignableUsers(ctx context.Context, key string) ([]User, error) {
	var all []User
	for start := 0; ; {
		var out []User
		if err := c.get(ctx, "/rest/api/3/user/assignable/search", url.Values{"project": {key}, "startAt": {strconv.Itoa(start)}, "maxResults": {"1000"}}, &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		start += len(out)
		if len(out) == 0 {
			return all, nil
		}
	}
}

// User looks one account up (an email address is only present when the account's privacy settings allow it).
func (c *Client) User(ctx context.Context, accountID string) (User, error) {
	var u User
	err := c.get(ctx, "/rest/api/3/user", url.Values{"accountId": {accountID}}, &u)
	return u, err
}
