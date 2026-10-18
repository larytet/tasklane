package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Write actions (SPEC 18.4, opt-in per connection). Everything above this file only reads; these are the only requests the
// package makes with a method other than GET, and the caller must have checked that the connection has write actions turned on.

// Errors a caller tells apart.
var (
	// ErrExists: the branch is already there.
	ErrExists = errors.New("the branch already exists")
	// ErrNoPermission: the token may read but not write (or cannot see the repository at all, which a provider reports the same way).
	ErrNoPermission = errors.New("the token is not allowed to write to this repository")
	// ErrBadCredentials: the provider does not accept the token (revoked, expired, wrong).
	ErrBadCredentials = errors.New("the provider does not accept the token")
	// ErrNoBase: the base branch is not in the repository.
	ErrNoBase = errors.New("the base branch does not exist")
)

// send makes one write request and returns the status and body. It follows the same authentication and size bounds as reads.
func (c *Client) send(ctx context.Context, method, u string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, err
	}
	if c.Provider == "github" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	} else {
		req.Header.Set("PRIVATE-TOKEN", c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "tasklane")
	c.Requests++
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

func (c *Client) failed(op, u string, status int, body []byte) error {
	switch status {
	case 401:
		return fmt.Errorf("%w (%s: 401)", ErrBadCredentials, op)
	case 403, 404:
		return fmt.Errorf("%w (%s: %d)", ErrNoPermission, op, status)
	}
	return fmt.Errorf("%s %s: %d %s", c.Provider, safeURL(u), status, strings.TrimSpace(string(body[:min(len(body), 200)])))
}

// Whoami asks the provider who the token belongs to: a cheap check that it is still accepted.
func (c *Client) Whoami(ctx context.Context) (login string, err error) {
	u := strings.TrimSuffix(c.BaseURL, "/") + "/user"
	if c.Provider == "gitlab" {
		u = strings.TrimSuffix(c.BaseURL, "/") + "/api/v4/user"
	}
	st, b, err := c.send(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	if st == 401 {
		return "", fmt.Errorf("%w (whoami: 401)", ErrBadCredentials)
	}
	if st/100 != 2 {
		return "", c.failed("whoami", u, st, b)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if l := str(m, "login"); l != "" {
		return l, nil
	}
	return str(m, "username"), nil
}

// DefaultBranch asks the provider for the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	base := strings.TrimSuffix(c.BaseURL, "/")
	u := fmt.Sprintf("%s/repos/%s", base, repo)
	if c.Provider == "gitlab" {
		u = fmt.Sprintf("%s/api/v4/projects/%s", base, url.PathEscape(repo))
	}
	st, b, err := c.send(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	if st/100 != 2 {
		return "", c.failed("repository", u, st, b)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return str(m, "default_branch"), nil
}

// CreateBranch creates name at the head of base and returns the web address of the new branch.
func (c *Client) CreateBranch(ctx context.Context, repo, name, base string) (string, error) {
	root := strings.TrimSuffix(c.BaseURL, "/")
	switch c.Provider {
	case "github":
		u := fmt.Sprintf("%s/repos/%s/git/ref/heads/%s", root, repo, url.PathEscape(base))
		st, b, err := c.send(ctx, "GET", u, nil)
		if err != nil {
			return "", err
		}
		if st == 404 {
			return "", ErrNoBase
		}
		if st/100 != 2 {
			return "", c.failed("read base", u, st, b)
		}
		var ref map[string]any
		json.Unmarshal(b, &ref)
		sha := str(obj(ref, "object"), "sha")
		if sha == "" {
			return "", errors.New("the provider did not say where the base branch is")
		}
		pu := fmt.Sprintf("%s/repos/%s/git/refs", root, repo)
		st, b, err = c.send(ctx, "POST", pu, map[string]any{"ref": "refs/heads/" + name, "sha": sha})
		if err != nil {
			return "", err
		}
		switch {
		case st == 422 && strings.Contains(strings.ToLower(string(b)), "already exists"):
			return "", ErrExists
		case st/100 != 2:
			return "", c.failed("create branch", pu, st, b)
		}
		return fmt.Sprintf("https://github.com/%s/tree/%s", repo, name), nil
	case "gitlab":
		u := fmt.Sprintf("%s/api/v4/projects/%s/repository/branches?branch=%s&ref=%s", root, url.PathEscape(repo), url.QueryEscape(name), url.QueryEscape(base))
		st, b, err := c.send(ctx, "POST", u, nil)
		if err != nil {
			return "", err
		}
		switch {
		case st == 400 && strings.Contains(strings.ToLower(string(b)), "already exists"):
			return "", ErrExists
		case st == 400 && strings.Contains(strings.ToLower(string(b)), "invalid reference"):
			return "", ErrNoBase
		case st/100 != 2:
			return "", c.failed("create branch", u, st, b)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		return str(m, "web_url"), nil
	}
	return "", errors.New("unknown provider")
}

// Comment posts a comment on a pull request (GitHub) or merge request (GitLab).
func (c *Client) Comment(ctx context.Context, repo, ext, text string) error {
	root := strings.TrimSuffix(c.BaseURL, "/")
	var u string
	var body map[string]any
	switch c.Provider {
	case "github":
		u, body = fmt.Sprintf("%s/repos/%s/issues/%s/comments", root, repo, url.PathEscape(ext)), map[string]any{"body": text}
	case "gitlab":
		u, body = fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%s/notes", root, url.PathEscape(repo), url.PathEscape(ext)), map[string]any{"body": text}
	default:
		return errors.New("unknown provider")
	}
	st, b, err := c.send(ctx, "POST", u, body)
	if err != nil {
		return err
	}
	if st/100 != 2 {
		return c.failed("comment", u, st, b)
	}
	return nil
}
