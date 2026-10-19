package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/larytet/tasklane/internal/egress"
	"io"
	"net/http"
	"strings"
	"time"
)

// OSClient is a small OpenSearch REST client: index and alias management, bulk writes with external versions, search and
// count. It talks to a cluster the operator configured, through the egress policy's HTTP client.
type OSClient struct {
	BaseURL  string
	User     string
	Password string
	APIKey   string // sent as "Authorization: ApiKey ..." when set
	HTTP     *http.Client
	Timeout  time.Duration
}

// OSError is an error response from the cluster.
type OSError struct {
	Status int
	Type   string
	Reason string
	Op     string
}

func (e *OSError) Error() string {
	return fmt.Sprintf("opensearch %s: %d %s: %s", e.Op, e.Status, e.Type, e.Reason)
}

func (c *OSClient) do(ctx context.Context, op, method, path string, body any, ndjson bool) (map[string]any, int, error) {
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			rd = bytes.NewReader(b)
		default:
			raw, err := json.Marshal(body)
			if err != nil {
				return nil, 0, err
			}
			rd = bytes.NewReader(raw)
		}
	}
	to := c.Timeout
	if to == 0 {
		to = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, rd)
	if err != nil {
		return nil, 0, err
	}
	if ndjson {
		req.Header.Set("Content-Type", "application/x-ndjson")
	} else if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+c.APIKey)
	} else if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	hc := c.HTTP
	if hc == nil {
		hc = egress.Policy{}.Client() // the safe default: private addresses are refused unless the caller supplies a client that allows them
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("opensearch %s: %w", op, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	var out map[string]any
	if len(raw) > 0 {
		json.Unmarshal(raw, &out)
	}
	if resp.StatusCode/100 != 2 {
		e := &OSError{Status: resp.StatusCode, Op: op}
		if er, ok := out["error"].(map[string]any); ok {
			e.Type, _ = er["type"].(string)
			e.Reason, _ = er["reason"].(string)
		} else if s, ok := out["error"].(string); ok {
			e.Reason = s
		}
		return out, resp.StatusCode, e
	}
	return out, resp.StatusCode, nil
}

// Health is the cluster status (green, yellow or red); an unreachable cluster is an error.
// Version is the cluster's version number ("2.17.1").
func (c *OSClient) Version(ctx context.Context) (string, error) {
	out, _, err := c.do(ctx, "version", "GET", "/", nil, false)
	if err != nil {
		return "", err
	}
	if v, ok := out["version"].(map[string]any); ok {
		n, _ := v["number"].(string)
		return n, nil
	}
	return "", nil
}

func (c *OSClient) Health(ctx context.Context) (string, error) {
	out, _, err := c.do(ctx, "health", "GET", "/_cluster/health", nil, false)
	if err != nil {
		return "", err
	}
	s, _ := out["status"].(string)
	return s, nil
}

func (c *OSClient) IndexExists(ctx context.Context, name string) (bool, error) {
	_, st, err := c.do(ctx, "exists", "HEAD", "/"+name, nil, false)
	var oe *OSError
	if errors.As(err, &oe) && oe.Status == 404 {
		return false, nil
	}
	return st == 200, err
}

func (c *OSClient) CreateIndex(ctx context.Context, name string, mapping map[string]any) error {
	_, _, err := c.do(ctx, "create index", "PUT", "/"+name, mapping, false)
	return err
}

func (c *OSClient) DeleteIndex(ctx context.Context, name string) error {
	_, _, err := c.do(ctx, "delete index", "DELETE", "/"+name, nil, false)
	var oe *OSError
	if errors.As(err, &oe) && oe.Status == 404 {
		return nil
	}
	return err
}

// AliasTarget returns the index an alias points at ("" when it does not exist).
func (c *OSClient) AliasTarget(ctx context.Context, alias string) (string, error) {
	out, _, err := c.do(ctx, "alias", "GET", "/_alias/"+alias, nil, false)
	var oe *OSError
	if errors.As(err, &oe) && oe.Status == 404 {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for idx := range out {
		return idx, nil
	}
	return "", nil
}

// SwapAlias moves an alias from one index to another atomically (from may be empty for the first time).
func (c *OSClient) SwapAlias(ctx context.Context, alias, from, to string) error {
	actions := []map[string]any{}
	if from != "" {
		actions = append(actions, map[string]any{"remove": map[string]any{"index": from, "alias": alias}})
	}
	actions = append(actions, map[string]any{"add": map[string]any{"index": to, "alias": alias}})
	_, _, err := c.do(ctx, "swap alias", "POST", "/_aliases", map[string]any{"actions": actions}, false)
	return err
}

// BulkOp is one write: an index (with the document) or a delete, carrying the external version.
type BulkOp struct {
	Index   string
	ID      string
	Version int64
	// GTE accepts an equal version (repairs and rebuilds rewrite a document at the snapshot version).
	GTE    bool
	Delete bool
	Doc    map[string]any
}

// BulkResult counts the outcomes; version conflicts are not failures (an equal or newer version is already there).
type BulkResult struct {
	Written, Conflicts, Failed int
	FirstError                 string
}

func (c *OSClient) Bulk(ctx context.Context, ops []BulkOp) (BulkResult, error) {
	var res BulkResult
	if len(ops) == 0 {
		return res, nil
	}
	var b bytes.Buffer
	for _, o := range ops {
		vt := "external"
		if o.GTE {
			vt = "external_gte"
		}
		meta := map[string]any{"_index": o.Index, "_id": o.ID, "version": o.Version, "version_type": vt}
		verb := "index"
		if o.Delete {
			verb = "delete"
		}
		m, _ := json.Marshal(map[string]any{verb: meta})
		b.Write(m)
		b.WriteByte('\n')
		if !o.Delete {
			d, _ := json.Marshal(o.Doc)
			b.Write(d)
			b.WriteByte('\n')
		}
	}
	out, _, err := c.do(ctx, "bulk", "POST", "/_bulk", b.Bytes(), true)
	if err != nil {
		return res, err
	}
	items, _ := out["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		for _, v := range m {
			r, _ := v.(map[string]any)
			st, _ := r["status"].(float64)
			switch {
			case st == 409:
				res.Conflicts++
			case st >= 200 && st < 300, st == 404 && r["result"] == "not_found":
				res.Written++
			default:
				res.Failed++
				if res.FirstError == "" {
					if er, ok := r["error"].(map[string]any); ok {
						res.FirstError = fmt.Sprint(er["type"], ": ", er["reason"])
					} else {
						res.FirstError = fmt.Sprint("status ", st)
					}
				}
			}
		}
	}
	return res, nil
}

type Hit struct {
	ID    string
	Score float64
	Src   map[string]any
}

type SearchResult struct {
	Hits  []Hit
	Total int
	Rel   string // "eq" or "gte"
}

func (c *OSClient) Search(ctx context.Context, index string, body map[string]any) (SearchResult, error) {
	out, _, err := c.do(ctx, "search", "POST", "/"+index+"/_search", body, false)
	var sr SearchResult
	if err != nil {
		return sr, err
	}
	h, _ := out["hits"].(map[string]any)
	if t, ok := h["total"].(map[string]any); ok {
		if v, ok := t["value"].(float64); ok {
			sr.Total = int(v)
		}
		sr.Rel, _ = t["relation"].(string)
	}
	hits, _ := h["hits"].([]any)
	for _, x := range hits {
		m, _ := x.(map[string]any)
		hit := Hit{}
		hit.ID, _ = m["_id"].(string)
		hit.Score, _ = m["_score"].(float64)
		hit.Src, _ = m["_source"].(map[string]any)
		sr.Hits = append(sr.Hits, hit)
	}
	return sr, nil
}

func (c *OSClient) Count(ctx context.Context, index string, query map[string]any) (int, error) {
	out, _, err := c.do(ctx, "count", "POST", "/"+index+"/_count", map[string]any{"query": query}, false)
	if err != nil {
		return 0, err
	}
	n, _ := out["count"].(float64)
	return int(n), nil
}

func (c *OSClient) DeleteByQuery(ctx context.Context, index string, query map[string]any) (int, error) {
	out, _, err := c.do(ctx, "delete by query", "POST", "/"+index+"/_delete_by_query", map[string]any{"query": query}, false)
	if err != nil {
		return 0, err
	}
	n, _ := out["deleted"].(float64)
	return int(n), nil
}

func (c *OSClient) Refresh(ctx context.Context, index string) error {
	_, _, err := c.do(ctx, "refresh", "POST", "/"+index+"/_refresh", nil, false)
	return err
}
