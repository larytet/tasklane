package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/search"
)

// Bulk tools (MCP-12): they act on up to fifty issues named by key or picked by a TQL query. dry_run is on unless the
// caller says otherwise, a dry run changes nothing and shows exactly what a real run would do to each issue, and a real run
// applies that plan issue by issue (each change is checked against the version that was planned, so a concurrent edit is
// reported on that issue and not overwritten). A real run spends one write of the hour's budget for each issue it changes.

const maxBulk = 50

var bulkPriorities = map[string]bool{"lowest": true, "low": true, "medium": true, "high": true, "highest": true}

func (s *Server) bulkTargets(ctx context.Context, a domain.Actor, args map[string]any) ([]string, error) {
	var keys []string
	seen := map[string]bool{}
	if ks, ok := args["keys"].([]any); ok {
		for _, k := range ks {
			key := strings.ToUpper(strings.TrimSpace(fmt.Sprint(k)))
			if key != "" && !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
		if len(keys) > maxBulk {
			return nil, &domain.InvalidError{Msg: fmt.Sprintf("a bulk call takes at most %d issues; you named %d", maxBulk, len(keys))}
		}
		return keys, nil
	}
	q := str(args, "query")
	if q == "" {
		return nil, &domain.InvalidError{Msg: "name the issues with keys, or pick them with a query"}
	}
	all, rd, wr, err := s.Svc.SearchScope(ctx, a)
	if err != nil {
		return nil, err
	}
	ks, err := s.Search.Keys(ctx, search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: s.Now}, q, maxBulk+1)
	if err != nil {
		if te, ok := search.IsQueryError(err); ok {
			return nil, &domain.InvalidError{Msg: "query error: " + te.Error()}
		}
		return nil, err
	}
	if len(ks) > maxBulk {
		return nil, &domain.InvalidError{Msg: fmt.Sprintf("the query matches more than %d issues; narrow it, or work in batches", maxBulk)}
	}
	return ks, nil
}

// bulkRun plans every target with the shared bulk code (domain.PlanBulk), and applies the plan when dry is false within the
// agent's write budget.
func (s *Server) bulkRun(ctx context.Context, a domain.Actor, args map[string]any, dry bool, op domain.BulkOp) (toolResult, error) {
	if err := domain.CheckBulkOp(op); err != nil {
		return toolResult{}, err
	}
	keys, err := s.bulkTargets(ctx, a, args)
	if err != nil {
		return toolResult{}, err
	}
	if len(keys) == 0 {
		return toolResult{Data: map[string]any{"dry_run": dry, "items": []any{}, "summary": map[string]any{"issues": 0}}}, nil
	}
	items, err := s.Svc.PlanBulk(ctx, a, keys, op)
	if err != nil {
		return toolResult{}, err
	}
	for i := range items {
		if items[i].Err == "forbidden: you may not change this issue" {
			items[i].Err = "forbidden: your identity may not change this issue"
		}
	}
	writes := 0
	for _, it := range items {
		if it.OK && !it.Unchanged {
			writes++
		}
	}
	applied, failed := 0, 0
	if !dry && writes > 0 {
		if ok, wait := s.allowWriteN(a.ID, writes); !ok {
			return toolResult{}, &domain.InvalidError{Msg: fmt.Sprintf("the write budget cannot cover %d changes; retry in %d min or change fewer issues", writes, int(wait.Minutes())+1)}
		}
		applied, failed = domain.ApplyBulk(ctx, items, errorText)
	}
	out := make([]any, 0, len(items))
	errs, unchanged := 0, 0
	for _, it := range items {
		m := map[string]any{"key": it.Key, "ok": it.OK}
		switch {
		case it.Err != "":
			m["error"] = it.Err
			errs++
		case it.Unchanged:
			m["unchanged"] = true
			unchanged++
		default:
			m["changes"] = it.Changes
		}
		out = append(out, m)
	}
	sum := map[string]any{"issues": len(items), "unchanged": unchanged, "errors": errs}
	if dry {
		sum["would_change"] = writes
	} else {
		sum["changed"], sum["failed"] = applied, failed
	}
	res := map[string]any{"dry_run": dry, "items": out, "summary": sum}
	if dry {
		res["next"] = "Nothing was changed. Call again with dry_run set to false (and an idempotency_key) to apply exactly this."
	}
	return toolResult{Data: res}, nil
}

func (s *Server) registerBulk() {
	target := []Prop{{"keys", "array", "Issue keys (up to 50). Use this or query", false, nil}, {"query", "string", "A TQL query that picks the issues (up to 50). Use this or keys", false, nil},
		{"dry_run", "boolean", "Default true: show what would change and change nothing. Set false to apply", false, nil}}
	s.add(&Tool{Name: "bulk_update_fields", Description: "Change priority, labels or assignee on many issues at once. A dry run (the default) shows each issue's before and after; a real run applies that plan issue by issue and reports each one. Labels are changed with labels_add and labels_remove.", Scope: "edit-fields", Write: true, DryRun: true,
		Props: append(append([]Prop{}, target...), Prop{"priority", "string", "lowest, low, medium, high, highest", false, nil}, Prop{"labels_add", "array", "Labels to add", false, nil}, Prop{"labels_remove", "array", "Labels to remove", false, nil}, Prop{"assignee", "string", "me, none, or a project member's name", false, nil}),
		Handler: func(ctx context.Context, s *Server, a domain.Actor, args map[string]any) (toolResult, error) {
			prio := str(args, "priority")
			if prio != "" && !bulkPriorities[prio] {
				return toolResult{}, &domain.InvalidError{Msg: fmt.Sprintf("unknown priority %q", prio)}
			}
			list := func(k string) []string {
				var out []string
				if ls, ok := args[k].([]any); ok {
					for _, x := range ls {
						if v := strings.TrimSpace(fmt.Sprint(x)); v != "" {
							out = append(out, v)
						}
					}
				}
				return out
			}
			add, rem := list("labels_add"), list("labels_remove")
			who, hasAssignee := args["assignee"].(string)
			if prio == "" && len(add) == 0 && len(rem) == 0 && !hasAssignee {
				return toolResult{}, &domain.InvalidError{Msg: "say what to change: priority, labels_add, labels_remove or assignee"}
			}
			op := domain.BulkOp{Kind: "fields", Priority: prio, LabelsAdd: add, LabelsRemove: rem, SetAssignee: hasAssignee,
				ResolveAssignee: func(ctx context.Context, key string) (string, error) {
					return s.resolveAssignee(ctx, a, key, strings.TrimSpace(who))
				}}
			return s.bulkRun(ctx, a, args, boolArg(args, "dry_run", true), op)
		}})
	s.add(&Tool{Name: "bulk_transition", Description: "Move many issues along the same workflow transition (by name). A dry run (the default) shows which issues can take it and which cannot, and why; a real run does the ones that can. Transitions that need a person are refused for agents.", Scope: "transition", Write: true, DryRun: true,
		Props: append(append([]Prop{}, target...), Prop{"transition", "string", "Transition name", true, nil}),
		Handler: func(ctx context.Context, s *Server, a domain.Actor, args map[string]any) (toolResult, error) {
			if err := need(args, "transition"); err != nil {
				return toolResult{}, err
			}
			op := domain.BulkOp{Kind: "transition", Transition: str(args, "transition")}
			return s.bulkRun(ctx, a, args, boolArg(args, "dry_run", true), op)
		}})
}
