package domain

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Bulk actions on a handful of issues at once (SPEC 4.1, 18.3), shared by the MCP bulk tools and the REST bulk endpoint of the
// bulk bar. Every target is planned first from the person's own view of it; a run applies that plan issue by issue, each change
// checked against the version that was planned, so a concurrent edit is reported on that issue and never overwritten. Each
// issue succeeds or fails on its own; none is half-applied.

// BulkOp is one action for every target. Kind is fields, transition, move, archive or delete.
type BulkOp struct {
	Kind         string
	Priority     string
	LabelsAdd    []string
	LabelsRemove []string
	// SetAssignee: change the assignee; ResolveAssignee gives the person's id for an issue ("" is nobody)
	SetAssignee     bool
	ResolveAssignee func(ctx context.Context, key string) (string, error)
	Transition      string // by name
	Project         string // move: the target project
}

// BulkPlanned is the plan for one issue; Apply carries it out (nil when there is nothing to do or it cannot be done).
type BulkPlanned struct {
	Key       string                          `json:"key"`
	OK        bool                            `json:"ok"`
	Unchanged bool                            `json:"unchanged,omitempty"`
	Err       string                          `json:"error,omitempty"`
	Changes   []map[string]any                `json:"changes,omitempty"`
	Drops     []string                        `json:"drops,omitempty"` // what a move would lose
	Version   int64                           `json:"-"`
	Apply     func(ctx context.Context) error `json:"-"`
}

var bulkPriorities = map[string]bool{"lowest": true, "low": true, "medium": true, "high": true, "highest": true}

func canEditRole(role string) bool { return role == "member" || role == "admin" }

// CheckBulkOp refuses an operation that says nothing to do, before any issue is read.
func CheckBulkOp(op BulkOp) error {
	switch op.Kind {
	case "fields":
		if op.Priority != "" && !bulkPriorities[op.Priority] {
			return invalid("unknown priority %q", op.Priority)
		}
		if op.Priority == "" && len(op.LabelsAdd) == 0 && len(op.LabelsRemove) == 0 && !op.SetAssignee {
			return invalid("say what to change: priority, labels_add, labels_remove or assignee")
		}
	case "transition":
		if strings.TrimSpace(op.Transition) == "" {
			return invalid("name the transition")
		}
	case "move":
		if strings.TrimSpace(op.Project) == "" {
			return invalid("name the project to move to")
		}
	case "archive", "delete":
	default:
		return invalid("unknown bulk operation %q (fields, transition, move, archive, delete)", op.Kind)
	}
	return nil
}

// PlanBulk plans op for every key, in order. Unknown and invisible keys are reported per issue, never fatal.
func (s *Service) PlanBulk(ctx context.Context, a Actor, keys []string, op BulkOp) ([]BulkPlanned, error) {
	if err := CheckBulkOp(op); err != nil {
		return nil, err
	}
	items := make([]BulkPlanned, 0, len(keys))
	for _, k := range keys {
		v, err := s.View(ctx, a, k)
		if err != nil {
			items = append(items, BulkPlanned{Key: k, Err: bulkErrText(err)})
			continue
		}
		it := s.planOne(ctx, a, v, op)
		it.Key, it.Version = v.Issue.Key, v.Issue.Version
		if it.Err == "" && !canEditRole(v.Role) && !(op.Kind == "delete" && v.Role != "") {
			it.Err, it.OK, it.Apply = "forbidden: you may not change this issue", false, nil
		}
		items = append(items, it)
	}
	return items, nil
}

func bulkErrText(err error) string {
	switch {
	case err == ErrNotFound:
		return "not found, or you do not have access"
	case err == ErrForbidden:
		return "forbidden: you may not change this issue"
	case err == ErrGone:
		return "this issue was deleted"
	}
	return err.Error()
}

func (s *Service) planOne(ctx context.Context, a Actor, v IssueView, op BulkOp) BulkPlanned {
	it := BulkPlanned{OK: true}
	key, ver := v.Issue.Key, v.Issue.Version
	fail := func(f string, args ...any) BulkPlanned { it.OK, it.Err = false, fmt.Sprintf(f, args...); return it }
	if op.Kind != "delete" { // a mirrored issue is changed in Jira: say so, rather than an empty list of transitions
		if merr := s.MirrorRefusal(ctx, v.Issue.Project); merr != nil {
			return fail("%s", merr.Error())
		}
	}
	switch op.Kind {
	case "fields":
		var ch Changes
		if op.Priority != "" && op.Priority != v.Issue.Priority {
			p := op.Priority
			ch.Priority = &p
			it.Changes = append(it.Changes, map[string]any{"field": "priority", "before": v.Issue.Priority, "after": op.Priority})
		}
		if len(op.LabelsAdd)+len(op.LabelsRemove) > 0 {
			set := map[string]bool{}
			for _, l := range v.Issue.Labels {
				set[l] = true
			}
			for _, l := range op.LabelsAdd {
				set[l] = true
			}
			for _, l := range op.LabelsRemove {
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
		if op.SetAssignee {
			id, err := op.ResolveAssignee(ctx, key)
			if err != nil {
				return fail("%s", err.Error())
			}
			before, cur, after := "", "", ""
			if v.Issue.Assignee != nil {
				before, cur = v.Issue.Assignee.Name, v.Issue.Assignee.ID
			}
			if id != cur {
				if id != "" {
					s.DB.QueryRow(ctx, `SELECT name FROM app_user WHERE id::text=$1`, id).Scan(&after)
				}
				ch.Assignee = &id
				it.Changes = append(it.Changes, map[string]any{"field": "assignee", "before": before, "after": after})
			}
		}
		if len(it.Changes) == 0 {
			it.Unchanged = true
			return it
		}
		it.Apply = func(ctx context.Context) error { _, err := s.UpdateFields(ctx, a, key, &ver, false, ch); return err }
	case "transition":
		var tid, to string
		var human bool
		var names []string
		for _, t := range v.Transitions {
			names = append(names, t.Name)
			if strings.EqualFold(t.Name, op.Transition) {
				tid, to, human = t.ID, t.To, t.RequiresHuman
			}
		}
		switch {
		case tid == "":
			return fail("%q is not available from status %q; available: %s", op.Transition, v.Issue.Status, strings.Join(names, ", "))
		case human && a.Kind == "agent":
			return fail("%q needs a person: agents cannot take this transition", to)
		}
		it.Changes = []map[string]any{{"field": "status", "before": v.Issue.Status, "after": to}}
		it.Apply = func(ctx context.Context) error { _, err := s.Transition(ctx, a, key, tid, &ver); return err }
	case "archive":
		switch {
		case v.Issue.Archived != nil:
			it.Unchanged = true
			return it
		case v.Issue.StatusCat != "done":
			return fail("only finished work is archived: %s is %q", key, v.Issue.Status)
		}
		it.Changes = []map[string]any{{"field": "archived", "before": "", "after": "archived"}}
		it.Apply = func(ctx context.Context) error {
			if err := s.versionIs(ctx, key, ver); err != nil {
				return err
			}
			_, err := s.ArchiveIssue(ctx, a, key)
			return err
		}
	case "move", "delete":
		in := LifecycleIn{Op: op.Kind, DryRun: true, Project: op.Project, ExpectKey: key}
		plan, err := s.Lifecycle(ctx, a, key, in)
		if err != nil {
			var be *BlockedError
			if errors.As(err, &be) {
				return fail("%s", strings.Join(be.Plan.Blockers, "; "))
			}
			return fail("%s", bulkErrText(err))
		}
		for _, c := range plan.Changes {
			it.Changes = append(it.Changes, map[string]any{"change": c})
		}
		it.Drops = plan.Drops
		in.DryRun = false
		it.Apply = func(ctx context.Context) error {
			if err := s.versionIs(ctx, key, ver); err != nil {
				return err
			}
			_, err := s.Lifecycle(ctx, a, key, in)
			return err
		}
	}
	return it
}

// versionIs refuses an action planned on an older copy of the issue (the transition and field changes check it themselves).
func (s *Service) versionIs(ctx context.Context, key string, ver int64) error {
	var cur int64
	if err := s.DB.QueryRow(ctx, `SELECT version FROM issue WHERE key=$1`, key).Scan(&cur); err != nil {
		return ErrNotFound
	}
	if cur != ver {
		return &VersionConflict{CurrentVersion: cur, Fields: []string{"version"}}
	}
	return nil
}

// ApplyBulk carries out every planned change and reports how many were applied and failed. Run it with WithUndoBatch to collect
// one undo token for the whole batch (SaveUndoBatch).
func ApplyBulk(ctx context.Context, items []BulkPlanned, errText func(error) string) (applied, failed int) {
	for i := range items {
		it := &items[i]
		if !it.OK || it.Unchanged || it.Apply == nil {
			continue
		}
		if err := it.Apply(ctx); err != nil {
			it.OK, it.Err = false, errText(err)
			failed++
			continue
		}
		applied++
	}
	return applied, failed
}

// BulkTransitionsFor lists the transitions some of the issues can take now, by name, with how many can: the bulk bar offers only
// these (BULK-08).
func (s *Service) BulkTransitionsFor(ctx context.Context, a Actor, keys []string) ([]map[string]any, error) {
	type agg struct {
		to string
		n  int
	}
	by, order := map[string]*agg{}, []string{}
	for _, k := range keys {
		v, err := s.View(ctx, a, k)
		if err != nil || !canEditRole(v.Role) || v.Mirror != nil {
			continue
		}
		for _, t := range v.Transitions {
			g := by[t.Name]
			if g == nil {
				g = &agg{to: t.To}
				by[t.Name] = g
				order = append(order, t.Name)
			}
			g.n++
		}
	}
	out := []map[string]any{}
	for _, name := range order {
		out = append(out, map[string]any{"name": name, "to": by[name].to, "issues": by[name].n})
	}
	return out, nil
}
