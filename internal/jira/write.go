package jira

import (
	"context"
	"net/url"
	"strings"
)

// The write half of the client (SPEC 18.1.1). A Client is made for one credential, so a person's token is never mixed with the
// sync's: the caller builds a new Client with the credential of the person who is writing.

// IssueFields reads only the named fields of one issue (and, with expand, its recent history): the conflict check before a write
// asks for what is about to change and nothing more.
func (c *Client) IssueFields(ctx context.Context, key string, fields []string, expand string) (Issue, error) {
	q := url.Values{}
	if len(fields) > 0 {
		q.Set("fields", strings.Join(fields, ","))
	}
	if expand != "" {
		q.Set("expand", expand)
	}
	var is Issue
	err := c.get(ctx, "/rest/api/3/issue/"+url.PathEscape(key), q, &is)
	return is, err
}

// EditIssue sets fields (Jira's field ids to values: strings, objects, lists) on one issue.
func (c *Client) EditIssue(ctx context.Context, key string, fields map[string]any) error {
	return c.send(ctx, "PUT", "/rest/api/3/issue/"+url.PathEscape(key), map[string]any{"fields": fields}, nil)
}

// AddComment adds a comment whose body is an Atlassian Document Format document.
func (c *Client) AddComment(ctx context.Context, key string, adf any) (Comment, error) {
	var out Comment
	err := c.send(ctx, "POST", "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", map[string]any{"body": adf}, &out)
	return out, err
}

// EditComment replaces the body of a comment.
func (c *Client) EditComment(ctx context.Context, key, id string, adf any) error {
	return c.send(ctx, "PUT", "/rest/api/3/issue/"+url.PathEscape(key)+"/comment/"+url.PathEscape(id), map[string]any{"body": adf}, nil)
}

// Transition is one move Jira offers from the issue's current status.
type Transition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   struct {
		Name string `json:"name"`
	} `json:"to"`
}

// Transitions lists the moves available now.
func (c *Client) Transitions(ctx context.Context, key string) ([]Transition, error) {
	var out struct {
		Transitions []Transition `json:"transitions"`
	}
	err := c.get(ctx, "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions", nil, &out)
	return out.Transitions, err
}

// DoTransition performs a move by its id; Jira refuses it (400, or 409 when two arrive at once) when it is not available.
func (c *Client) DoTransition(ctx context.Context, key, id string, fields map[string]any) error {
	body := map[string]any{"transition": map[string]any{"id": id}}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	return c.send(ctx, "POST", "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions", body, nil)
}
