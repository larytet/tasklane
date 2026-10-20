package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/search/tql"
)

// The bulk bar (SPEC 4.1): one action on up to one page of issues (100), with the planning and applying the MCP bulk tools use
// (domain.PlanBulk). A dry run is the default and changes nothing; a real run applies the plan issue by issue and answers per
// issue. A transition, archive or delete run leaves one undo token for the whole batch (Tasklane-Undo). "Select all N matching"
// as a background job is a later milestone; until then more than 100 issues is refused.
const maxBulkREST = 100

type bulkReq struct {
	Keys           []string `json:"keys"`
	Query          string   `json:"query"`
	Op             string   `json:"op"`
	Priority       string   `json:"priority"`
	LabelsAdd      []string `json:"labels_add"`
	LabelsRemove   []string `json:"labels_remove"`
	Assignee       *string  `json:"assignee"` // a person's id, "me", or "" for nobody
	Transition     string   `json:"transition"`
	Project        string   `json:"project"`
	DryRun         *bool    `json:"dry_run"`
	IdempotencyKey string   `json:"idempotency_key"`
}

func (s *Server) registerBulkREST(mux *http.ServeMux) {
	s.handle(mux, "POST /api/v1/bulk", s.auth(s.bulk))
	s.handle(mux, "POST /api/v1/bulk/transitions", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Keys []string `json:"keys"`
		}
		if !s.decode(w, r, &in) {
			return
		}
		keys, err := bulkKeys(in.Keys)
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		out, err := s.Svc.BulkTransitionsFor(r.Context(), actorOf(r), keys)
		s.reply(w, out, err)
	}))
}

func bulkKeys(in []string) ([]string, error) {
	seen, keys := map[string]bool{}, []string{}
	for _, k := range in {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k != "" && !seen[k] { // a key named twice is changed once
			seen[k] = true
			keys = append(keys, k)
		}
	}
	if len(keys) > maxBulkREST {
		return nil, &domain.InvalidError{Msg: fmt.Sprintf("a bulk action takes at most %d issues (one page); you selected %d", maxBulkREST, len(keys))}
	}
	return keys, nil
}

func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.fail(w, 400, "bad_request", "the request is too large", nil)
		return
	}
	var in bulkReq
	if err := json.Unmarshal(raw, &in); err != nil {
		s.fail(w, 400, "bad_request", "invalid JSON body: "+err.Error(), nil)
		return
	}
	a := actorOf(r)
	ctx := r.Context()
	op := domain.BulkOp{Kind: in.Op, Priority: in.Priority, LabelsAdd: in.LabelsAdd, LabelsRemove: in.LabelsRemove, Transition: in.Transition, Project: in.Project}
	if in.Assignee != nil {
		who := strings.TrimSpace(*in.Assignee)
		op.SetAssignee = true
		op.ResolveAssignee = s.bulkAssignee(a, who)
	}
	if err := domain.CheckBulkOp(op); err != nil {
		s.reply(w, nil, err)
		return
	}
	var keys []string
	switch {
	case len(in.Keys) > 0:
		if keys, err = bulkKeys(in.Keys); err != nil {
			s.reply(w, nil, err)
			return
		}
	case strings.TrimSpace(in.Query) != "":
		sc, err := s.scope(r)
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		ks, err := s.Search.Keys(ctx, sc, in.Query, maxBulkREST+1)
		if te, ok := search.IsQueryError(err); ok {
			s.fail(w, 400, "tql_error", te.Msg, map[string]any{"pos": te.Pos})
			return
		}
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		if len(ks) > maxBulkREST {
			s.fail(w, 400, "invalid", fmt.Sprintf("the query matches more than %d issues; a bulk action takes one page at a time", maxBulkREST), nil)
			return
		}
		keys = ks
	default:
		s.fail(w, 400, "invalid", "select some issues: keys, or a query", nil)
		return
	}
	dry := in.DryRun == nil || *in.DryRun
	idem := strings.TrimSpace(in.IdempotencyKey)
	if !dry && idem != "" { // a retry after a lost answer returns the first answer and does nothing more
		if len(idem) < 8 || len(idem) > 128 {
			s.fail(w, 400, "invalid", "the idempotency_key must be 8 to 128 characters", nil)
			return
		}
		sum := sha256.Sum256(append([]byte("bulk\x00"), raw...))
		state, stored, err := s.Svc.IdempotencyBegin(ctx, a.ID, idem, hex.EncodeToString(sum[:]))
		if err != nil {
			s.reply(w, nil, err)
			return
		}
		switch state {
		case "mismatch":
			s.fail(w, 409, "conflict", "this idempotency_key was already used for a different request", nil)
			return
		case "inflight":
			w.Header().Set("Retry-After", "2")
			s.fail(w, 409, "conflict", "a request with this idempotency_key is still running", nil)
			return
		case "replay":
			var m map[string]any
			json.Unmarshal(stored, &m)
			m["idempotent_replay"] = true
			s.json(w, 200, m)
			return
		}
	}
	items, err := s.Svc.PlanBulk(ctx, a, keys, op)
	if err != nil {
		if idem != "" && !dry {
			s.Svc.IdempotencyAbort(ctx, a.ID, idem)
		}
		s.reply(w, nil, err)
		return
	}
	writes := 0
	for _, it := range items {
		if it.OK && !it.Unchanged {
			writes++
		}
	}
	applied, failed := 0, 0
	if !dry {
		actx, slot := domain.WithUndoBatch(ctx)
		applied, failed = domain.ApplyBulk(actx, items, func(err error) string { return bulkReason(err) })
		if err := s.Svc.SaveUndoBatch(ctx, a, slot); err == nil {
			s.giveUndo(w, slot, nil)
		}
	}
	out := items
	errs, unchanged, drops := 0, 0, 0
	for _, it := range items {
		switch {
		case it.Err != "":
			errs++
		case it.Unchanged:
			unchanged++
		}
		if len(it.Drops) > 0 {
			drops++
		}
	}
	summary := map[string]any{"issues": len(items), "unchanged": unchanged, "errors": errs, "with_drops": drops}
	if dry {
		summary["would_change"] = writes
	} else {
		summary["changed"], summary["failed"] = applied, failed
	}
	// the bar applies a clean plan at once and shows the plan only when some issues would fail or lose data
	res := map[string]any{"dry_run": dry, "op": in.Op, "items": out, "summary": summary, "needs_review": errs > 0 || drops > 0}
	if !dry && idem != "" {
		b, _ := json.Marshal(res)
		s.Svc.IdempotencyFinish(ctx, a.ID, idem, b)
	}
	s.json(w, 200, res)
}

// matchingKeys runs the list's query as the person (their scope, so never an issue they cannot read) and returns every key it matches, up
// to the job limit. It answers the request itself and returns false when the query is wrong or too wide.
func (s *Server) matchingKeys(w http.ResponseWriter, r *http.Request, q, syntax string) ([]string, bool) {
	sc, err := s.scope(r)
	if err != nil {
		s.reply(w, nil, err)
		return nil, false
	}
	if syntax == "text" {
		if q, err = tql.FromText(q); err != nil {
			te, _ := search.IsQueryError(err)
			s.fail(w, 400, "tql_error", te.Msg, map[string]any{"pos": te.Pos})
			return nil, false
		}
	}
	keys, err := s.Search.Keys(r.Context(), sc, q, domain.MaxBulkJobItems+1)
	if te, ok := search.IsQueryError(err); ok {
		s.fail(w, 400, "tql_error", te.Msg, map[string]any{"pos": te.Pos})
		return nil, false
	}
	if err != nil {
		s.reply(w, nil, err)
		return nil, false
	}
	return keys, true
}

// bulkAssignee resolves "me", "" (nobody) or a person's id; the person must be able to work in the issue's project.
func (s *Server) bulkAssignee(a domain.Actor, who string) func(ctx context.Context, key string) (string, error) {
	return s.Svc.BulkAssignee(a, who)
}

func bulkReason(err error) string { return domain.BulkReason(err) }
