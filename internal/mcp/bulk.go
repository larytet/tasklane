package mcp

import (
	"context"
	"fmt"
	"sort"
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

type bulkItem struct {
	Key       string
	OK        bool
	Unchanged bool
	Err       string
	Changes   []map[string]any
	Version   int64
	apply     func(ctx context.Context) error
}

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

func canEditRole(role string) bool { return role == "member" || role == "admin" }

// bulkRun plans every target, and applies the plan when dry is false.
func (s *Server) bulkRun(ctx context.Context, a domain.Actor, args map[string]any, dry bool, plan func(v domain.IssueView) bulkItem) (toolResult, error) {
	keys, err := s.bulkTargets(ctx, a, args)
	if err != nil {
		return toolResult{}, err
	}
	if len(keys) == 0 {
		return toolResult{Data: map[string]any{"dry_run": dry, "items": []any{}, "summary": map[string]any{"issues": 0}}}, nil
	}
	items := make([]bulkItem, 0, len(keys))
	for _, k := range keys {
		v, err := s.Svc.View(ctx, a, k)
		if err != nil {
			items = append(items, bulkItem{Key: k, Err: errorText(err)})
			continue
		}
		it := plan(v)
		it.Key, it.Version = v.Issue.Key, v.Issue.Version
		if it.Err == "" && !canEditRole(v.Role) {
			it.Err = "forbidden: your identity may not change this issue"
			it.OK = false
		}
		items = append(items, it)
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
		for i := range items {
			it := &items[i]
			if !it.OK || it.Unchanged {
				continue
			}
			if err := it.apply(ctx); err != nil {
				it.OK, it.Err = false, errorText(err)
				failed++
				continue
			}
			applied++
		}
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
			_, hasAssignee := args["assignee"].(string)
			if prio == "" && len(add) == 0 && len(rem) == 0 && !hasAssignee {
				return toolResult{}, &domain.InvalidError{Msg: "say what to change: priority, labels_add, labels_remove or assignee"}
			}
			dry := boolArg(args, "dry_run", true)
			return s.bulkRun(ctx, a, args, dry, func(v domain.IssueView) bulkItem {
				it := bulkItem{OK: true}
				var ch domain.Changes
				if prio != "" && prio != v.Issue.Priority {
					ch.Priority = &prio
					it.Changes = append(it.Changes, map[string]any{"field": "priority", "before": v.Issue.Priority, "after": prio})
				}
				if len(add)+len(rem) > 0 {
					set := map[string]bool{}
					for _, l := range v.Issue.Labels {
						set[l] = true
					}
					for _, l := range add {
						set[l] = true
					}
					for _, l := range rem {
						delete(set, l)
					}
					next := make([]string, 0, len(set))
					for l := range set {
						next = append(next, l)
					}
					sort.Strings(next)
					cur := append([]string{}, v.Issue.Labels...)
					sort.Strings(cur)
					if strings.Join(next, "\x00") != strings.Join(cur, "\x00") {
						ch.Labels = &next
						it.Changes = append(it.Changes, map[string]any{"field": "labels", "before": cur, "after": next})
					}
				}
				if hasAssignee {
					id, err := s.resolveAssignee(ctx, a, v.Issue.Key, strings.TrimSpace(str(args, "assignee")))
					if err != nil {
						it.OK, it.Err = false, errorText(err)
						return it
					}
					before, after := "", ""
					if v.Issue.Assignee != nil {
						before = v.Issue.Assignee.Name
					}
					cur := ""
					if v.Issue.Assignee != nil {
						cur = v.Issue.Assignee.ID
					}
					if id != cur {
						if id != "" {
							s.Svc.DB.QueryRow(ctx, `SELECT name FROM app_user WHERE id::text=$1`, id).Scan(&after)
						}
						ch.Assignee = &id
						it.Changes = append(it.Changes, map[string]any{"field": "assignee", "before": before, "after": after})
					}
				}
				if len(it.Changes) == 0 {
					it.Unchanged = true
					return it
				}
				ver := v.Issue.Version
				key := v.Issue.Key
				it.apply = func(ctx context.Context) error {
					_, err := s.Svc.UpdateFields(ctx, a, key, &ver, false, ch)
					return err
				}
				return it
			})
		}})
	s.add(&Tool{Name: "bulk_transition", Description: "Move many issues along the same workflow transition (by name). A dry run (the default) shows which issues can take it and which cannot, and why; a real run does the ones that can. Transitions that need a person are refused for agents.", Scope: "transition", Write: true, DryRun: true,
		Props: append(append([]Prop{}, target...), Prop{"transition", "string", "Transition name", true, nil}),
		Handler: func(ctx context.Context, s *Server, a domain.Actor, args map[string]any) (toolResult, error) {
			if err := need(args, "transition"); err != nil {
				return toolResult{}, err
			}
			name := str(args, "transition")
			dry := boolArg(args, "dry_run", true)
			return s.bulkRun(ctx, a, args, dry, func(v domain.IssueView) bulkItem {
				it := bulkItem{OK: true}
				var tid, to string
				var human bool
				var names []string
				for _, t := range v.Transitions {
					names = append(names, t.Name)
					if strings.EqualFold(t.Name, name) {
						tid, to, human = t.ID, t.To, t.RequiresHuman
					}
				}
				switch {
				case tid == "":
					it.OK, it.Err = false, fmt.Sprintf("%q is not available from status %q; available: %s", name, v.Issue.Status, strings.Join(names, ", "))
				case human && a.Kind == "agent":
					it.OK, it.Err = false, fmt.Sprintf("%q needs a person: agents cannot take this transition", to)
				default:
					it.Changes = []map[string]any{{"field": "status", "before": v.Issue.Status, "after": to}}
					ver, key := v.Issue.Version, v.Issue.Key
					it.apply = func(ctx context.Context) error {
						_, err := s.Svc.Transition(ctx, a, key, tid, &ver)
						return err
					}
				}
				return it
			})
		}})
}
