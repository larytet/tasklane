package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Approvals (SPEC 15): a request on an issue to approvers (people, groups, the reporter or assignee, or the people in a user field), a
// rule (all of them, any one, or N), an optional deadline with one reminder. Each decision carries a comment that is written into the
// issue's thread. A workflow transition can require a granted approval (the condition approval_granted). Agents never decide: an
// approval is a person's word.

type Approver struct {
	User     User   `json:"user"`
	Decision string `json:"decision,omitempty"` // approve, reject, or empty while undecided
	Comment  string `json:"comment,omitempty"`
	At       string `json:"decided_at,omitempty"`
}

type Approval struct {
	ID          string     `json:"id"`
	Issue       string     `json:"issue"`
	Summary     string     `json:"summary,omitempty"`
	RequestedBy *User      `json:"requested_by,omitempty"`
	Rule        string     `json:"rule"` // all, any, n
	Needed      int        `json:"needed"`
	Status      string     `json:"status"` // pending, granted, rejected, cancelled
	Comment     string     `json:"comment,omitempty"`
	Deadline    *time.Time `json:"deadline,omitempty"`
	Overdue     bool       `json:"overdue,omitempty"`
	Created     time.Time  `json:"created"`
	Decided     *time.Time `json:"decided,omitempty"`
	Approvers   []Approver `json:"approvers"`
	// Mine is set for the caller who is an approver and has not decided yet: they can act on it.
	Mine bool `json:"mine,omitempty"`
}

type ApprovalIn struct {
	Users   []string `json:"users"`   // ids or email addresses
	Groups  []string `json:"groups"`  // ids or names
	Derived []string `json:"derived"` // "reporter", "assignee", or the key of a user field
	Rule    string   `json:"rule"`    // all (default), any, n
	N       int      `json:"n"`
	// Deadline is when a decision is due; one reminder goes to those who have not decided when it is near or past
	Deadline *time.Time `json:"deadline"`
	Comment  string     `json:"comment"`
}

const maxApprovers = 25

// approvalOutcome applies the rule to the counts: "granted", "rejected", or "" while it is still open.
func approvalOutcome(rule string, needed, total, approved, rejected int) string {
	switch rule {
	case "any":
		if approved >= 1 {
			return "granted"
		}
		if rejected >= total {
			return "rejected"
		}
	case "n":
		if approved >= needed {
			return "granted"
		}
		if total-rejected < needed {
			return "rejected"
		}
	default: // all
		if rejected >= 1 {
			return "rejected"
		}
		if approved >= total {
			return "granted"
		}
	}
	return ""
}

func (s *Service) loadApprovals(ctx context.Context, q querier, where string, args ...any) ([]Approval, error) {
	rows, err := q.Query(ctx, `SELECT a.id::text,i.key,i.summary,a.requested_by::text,COALESCE(rb.name,''),a.rule,a.needed,a.status,a.comment,a.deadline,a.created_at,a.decided_at
		FROM approval a JOIN issue i ON i.id=a.issue_id LEFT JOIN app_user rb ON rb.id=a.requested_by WHERE `+where+` ORDER BY a.created_at DESC LIMIT 50`, args...)
	if err != nil {
		return nil, err
	}
	var out []Approval
	for rows.Next() {
		var a Approval
		var rbID *string
		var rbName string
		if err := rows.Scan(&a.ID, &a.Issue, &a.Summary, &rbID, &rbName, &a.Rule, &a.Needed, &a.Status, &a.Comment, &a.Deadline, &a.Created, &a.Decided); err != nil {
			rows.Close()
			return nil, err
		}
		if rbID != nil {
			a.RequestedBy = &User{ID: *rbID, Name: rbName}
		}
		out = append(out, a)
	}
	rows.Close()
	now := s.Now()
	for i := range out {
		out[i].Approvers = []Approver{}
		ar, err := q.Query(ctx, `SELECT u.id::text,u.name,COALESCE(x.decision,''),x.comment,COALESCE(to_char(x.decided_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),'') FROM approval_approver x JOIN app_user u ON u.id=x.user_id WHERE x.approval_id::text=$1 ORDER BY u.name`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for ar.Next() {
			var p Approver
			ar.Scan(&p.User.ID, &p.User.Name, &p.Decision, &p.Comment, &p.At)
			out[i].Approvers = append(out[i].Approvers, p)
		}
		ar.Close()
		out[i].Overdue = out[i].Status == "pending" && out[i].Deadline != nil && out[i].Deadline.Before(now)
	}
	return out, nil
}

// IssueApprovals lists an issue's approvals, newest first, for someone who can see the issue; Mine marks what the caller may decide.
func (s *Service) IssueApprovals(ctx context.Context, a Actor, key string) ([]Approval, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return nil, err
	}
	out, err := s.loadApprovals(ctx, s.DB, `a.issue_id::text=$1`, is.ID)
	if err != nil {
		return nil, err
	}
	markMine(out, a.ID)
	if out == nil {
		out = []Approval{}
	}
	return out, nil
}

func markMine(out []Approval, uid string) {
	for i := range out {
		if out[i].Status != "pending" {
			continue
		}
		for _, p := range out[i].Approvers {
			if p.User.ID == uid && p.Decision == "" {
				out[i].Mine = true
			}
		}
	}
}

// MyApprovals lists the pending approvals waiting for the caller, on issues they can still see.
func (s *Service) MyApprovals(ctx context.Context, a Actor) ([]Approval, error) {
	out, err := s.loadApprovals(ctx, s.DB, `a.status='pending' AND i.deleted_at IS NULL AND EXISTS (SELECT 1 FROM approval_approver x WHERE x.approval_id=a.id AND x.user_id::text=$1 AND x.decision IS NULL)`, a.ID)
	if err != nil {
		return nil, err
	}
	keep := out[:0]
	for _, ap := range out {
		if s.CanRead(ctx, a, ap.Issue) {
			keep = append(keep, ap)
		}
	}
	markMine(keep, a.ID)
	if keep == nil {
		keep = []Approval{}
	}
	return keep, nil
}

func (s *Service) resolveApprovers(ctx context.Context, a Actor, is Issue, pid string, in ApprovalIn) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, u := range in.Users {
		u = strings.TrimSpace(u)
		var id string
		err := s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE active AND org_id::text=$2 AND (id::text=$1 OR lower(email)=lower($1))`, u, a.OrgID()).Scan(&id)
		if err != nil {
			return nil, invalid("there is no active person %q", u)
		}
		add(id)
	}
	for _, g := range in.Groups {
		rows, err := s.DB.Query(ctx, `SELECT m.user_id::text FROM user_group_member m JOIN user_group g ON g.id=m.group_id JOIN app_user u ON u.id=m.user_id AND u.active WHERE g.id::text=$1 OR lower(g.name)=lower($1)`, strings.TrimSpace(g))
		if err != nil {
			return nil, invalid("there is no group %q", g)
		}
		n := 0
		for rows.Next() {
			var id string
			rows.Scan(&id)
			add(id)
			n++
		}
		rows.Close()
		if n == 0 {
			return nil, invalid("the group %q does not exist or has no active members", g)
		}
	}
	if len(in.Derived) > 0 {
		t, err := s.loadTctx(ctx, s.DB, a, is.ID, "admin")
		if err != nil {
			return nil, err
		}
		for _, d := range in.Derived {
			switch strings.ToLower(strings.TrimSpace(d)) {
			case "reporter":
				add(t.Reporter)
			case "assignee":
				if t.Assignee == "" {
					return nil, invalid("the issue has no assignee to approve it")
				}
				add(t.Assignee)
			default:
				v, ok := t.Vals[canonKey(d)]
				if !ok || isEmptyValue(v) {
					return nil, invalid("the field %q is empty on this issue", d)
				}
				switch x := v.(type) {
				case string:
					add(x)
				case []any:
					for _, e := range x {
						if str, ok := e.(string); ok {
							add(str)
						}
					}
				default:
					return nil, invalid("the field %q does not hold people", d)
				}
			}
		}
	}
	if len(ids) == 0 {
		return nil, invalid("an approval needs at least one approver")
	}
	if len(ids) > maxApprovers {
		return nil, invalid("an approval has at most %d approvers", maxApprovers)
	}
	for _, id := range ids { // an approver must be able to see the issue: otherwise the request could never be answered
		var admin bool
		var role string
		if err := s.DB.QueryRow(ctx, `SELECT is_admin FROM app_user WHERE id::text=$1 AND active`, id).Scan(&admin); err != nil {
			return nil, invalid("an approver is not an active person")
		}
		s.DB.QueryRow(ctx, `SELECT role FROM effective_member WHERE project_id::text=$1 AND user_id::text=$2`, pid, id).Scan(&role)
		if !admin && role == "" {
			var name string
			s.DB.QueryRow(ctx, `SELECT name FROM app_user WHERE id::text=$1`, id).Scan(&name)
			return nil, invalid("%s cannot see this project, so cannot approve in it", name)
		}
	}
	return ids, nil
}

// RequestApproval asks a set of people to approve an issue (people who can edit it). Only one approval is open on an issue at a time.
func (s *Service) RequestApproval(ctx context.Context, a Actor, key string, in ApprovalIn) (Approval, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return Approval{}, err
	}
	if !canWrite(role) || is.Archived != nil {
		return Approval{}, ErrForbidden
	}
	end, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return Approval{}, perr
	}
	defer end()
	rule := strings.ToLower(strings.TrimSpace(in.Rule))
	if rule == "" {
		rule = "all"
	}
	if rule != "all" && rule != "any" && rule != "n" {
		return Approval{}, invalid("the rule is all, any or n")
	}
	if len([]rune(in.Comment)) > 2000 {
		return Approval{}, invalid("the comment is too long")
	}
	pid, err := s.projectID(ctx, is.Project)
	if err != nil {
		return Approval{}, err
	}
	ids, err := s.resolveApprovers(ctx, a, is, pid, in)
	if err != nil {
		return Approval{}, err
	}
	needed := 1
	switch rule {
	case "all":
		needed = len(ids)
	case "n":
		if in.N < 1 || in.N > len(ids) {
			return Approval{}, invalid("n is between 1 and the number of approvers (%d)", len(ids))
		}
		needed = in.N
	}
	if in.Deadline != nil && !in.Deadline.After(s.Now()) {
		return Approval{}, invalid("the deadline is in the past")
	}
	var id string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO approval(issue_id,requested_by,rule,needed,comment,deadline) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text`, is.ID, a.ID, rule, needed, in.Comment, in.Deadline).Scan(&id); err != nil {
			if strings.Contains(err.Error(), "approval_one_pending_uq") {
				return invalid("this issue already has an approval waiting for a decision")
			}
			return err
		}
		for _, u := range ids {
			if _, err := tx.Exec(ctx, `INSERT INTO approval_approver(approval_id,user_id) VALUES($1,$2)`, id, u); err != nil {
				return err
			}
		}
		b, n := "", "requested"
		if err := s.hist(ctx, tx, is.ID, a, "approval", &b, &n); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+is.Key, "approval.requested", map[string]any{"key": is.Key, "actor": a.Label, "actor_id": a.ID, "users": ids, "text": in.Comment})
	})
	if err != nil {
		return Approval{}, err
	}
	s.emit(Event{Type: "issue.updated", Issue: is.Key, Fields: []string{"approval"}, Actor: a.Label, ActorID: a.ID})
	return s.oneApproval(ctx, a, id)
}

func (s *Service) oneApproval(ctx context.Context, a Actor, id string) (Approval, error) {
	out, err := s.loadApprovals(ctx, s.DB, `a.id::text=$1`, id)
	if err != nil || len(out) == 0 {
		return Approval{}, ErrNotFound
	}
	markMine(out, a.ID)
	return out[0], nil
}

// approvalFor loads an approval and checks the caller can see its issue.
func (s *Service) approvalFor(ctx context.Context, a Actor, id string) (Approval, Issue, string, error) {
	var key string
	err := s.DB.QueryRow(ctx, `SELECT i.key FROM approval a JOIN issue i ON i.id=a.issue_id WHERE a.id::text=$1`, id).Scan(&key)
	if err != nil {
		return Approval{}, Issue{}, "", ErrNotFound // also a malformed id
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return Approval{}, is, "", err
	}
	ap, err := s.oneApproval(ctx, a, id)
	return ap, is, role, err
}

// DecideApproval records an approver's decision (approve or reject) with a comment, which is also written into the issue's thread. The
// rule decides when the approval as a whole is granted or rejected. A decision is final.
func (s *Service) DecideApproval(ctx context.Context, a Actor, id, decision, comment string) (Approval, error) {
	ap, is, _, err := s.approvalFor(ctx, a, id)
	if err != nil {
		return ap, err
	}
	if a.Kind == "agent" {
		return ap, invalid("an approval is a person's decision: agents cannot approve or reject")
	}
	if decision != "approve" && decision != "reject" {
		return ap, invalid("the decision is approve or reject")
	}
	comment, _ = Sanitize(comment)
	if len([]rune(comment)) > 4000 {
		return ap, invalid("the comment is too long")
	}
	end, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return ap, perr
	}
	defer end()
	if is.Archived != nil {
		return ap, ErrForbidden
	}
	var final string
	var requester *string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM approval WHERE id::text=$1 FOR UPDATE`, id).Scan(&status); err != nil {
			return err
		}
		if status != "pending" {
			return invalid("this approval is %s: it no longer takes decisions", status)
		}
		tag, err := tx.Exec(ctx, `UPDATE approval_approver SET decision=$3,comment=$4,decided_at=$5 WHERE approval_id::text=$1 AND user_id::text=$2 AND decision IS NULL`, id, a.ID, decision, comment, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var is bool
			tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approval_approver WHERE approval_id::text=$1 AND user_id::text=$2)`, id, a.ID).Scan(&is)
			if !is {
				return ErrForbidden // not one of the approvers
			}
			return invalid("you have already decided")
		}
		var rule string
		var needed, total, approved, rejected int
		if err := tx.QueryRow(ctx, `SELECT a.rule,a.needed,count(*),count(*) FILTER (WHERE x.decision='approve'),count(*) FILTER (WHERE x.decision='reject') FROM approval a JOIN approval_approver x ON x.approval_id=a.id WHERE a.id::text=$1 GROUP BY a.rule,a.needed`, id).Scan(&rule, &needed, &total, &approved, &rejected); err != nil {
			return err
		}
		final = approvalOutcome(rule, needed, total, approved, rejected)
		if final != "" {
			if _, err := tx.Exec(ctx, `UPDATE approval SET status=$2,decided_at=$3 WHERE id::text=$1`, id, final, s.Now()); err != nil {
				return err
			}
			b, n := "pending", final
			if err := s.hist(ctx, tx, is.ID, a, "approval", &b, &n); err != nil {
				return err
			}
		}
		word := map[string]string{"approve": "Approved", "reject": "Rejected"}[decision]
		body := word + "."
		if strings.TrimSpace(comment) != "" {
			body = word + ": " + comment
		}
		if _, err := tx.Exec(ctx, `INSERT INTO comment(issue_id,author_id,body,internal) VALUES($1,$2,$3,false)`, is.ID, a.ID, body); err != nil {
			return err
		}
		tx.QueryRow(ctx, `SELECT requested_by::text FROM approval WHERE id::text=$1`, id).Scan(&requester)
		users := []string{}
		if requester != nil {
			users = append(users, *requester)
		}
		text := a.Label + " " + strings.ToLower(word) + " the approval"
		if final != "" {
			text += "; the approval is " + final
		}
		return outbox(ctx, tx, "issue:"+is.Key, "approval.decided", map[string]any{"key": is.Key, "actor": a.Label, "actor_id": a.ID, "users": users, "text": text})
	})
	if err != nil {
		return ap, err
	}
	s.emit(Event{Type: "comment.added", Issue: is.Key, Actor: a.Label, ActorID: a.ID})
	s.emit(Event{Type: "issue.updated", Issue: is.Key, Fields: []string{"approval"}, Actor: a.Label, ActorID: a.ID})
	return s.oneApproval(ctx, a, id)
}

// CancelApproval withdraws a pending approval (the person who asked, or a project administrator).
func (s *Service) CancelApproval(ctx context.Context, a Actor, id string) (Approval, error) {
	ap, is, role, err := s.approvalFor(ctx, a, id)
	if err != nil {
		return ap, err
	}
	if (ap.RequestedBy == nil || ap.RequestedBy.ID != a.ID) && role != "admin" {
		return ap, ErrForbidden
	}
	end, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return ap, perr
	}
	defer end()
	if ap.Status != "pending" {
		return ap, invalid("this approval is %s", ap.Status)
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE approval SET status='cancelled',decided_at=$2 WHERE id::text=$1 AND status='pending'`, id, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid("this approval is no longer pending")
		}
		b, n := "pending", "cancelled"
		return s.hist(ctx, tx, is.ID, a, "approval", &b, &n)
	})
	if err != nil {
		return ap, err
	}
	return s.oneApproval(ctx, a, id)
}

// ApprovalSweep sends the one reminder to approvers who have not decided when a deadline is within a day or past, and returns how many
// approvals it reminded. It is safe to run on every node: the reminder is claimed in the database.
func (s *Service) ApprovalSweep(ctx context.Context) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT a.id::text,i.key FROM approval a JOIN issue i ON i.id=a.issue_id WHERE a.status='pending' AND a.reminded_at IS NULL AND a.deadline IS NOT NULL AND a.deadline <= $1 AND i.deleted_at IS NULL`, s.Now().Add(24*time.Hour))
	if err != nil {
		return 0, err
	}
	type due struct{ id, key string }
	var ds []due
	for rows.Next() {
		var d due
		rows.Scan(&d.id, &d.key)
		ds = append(ds, d)
	}
	rows.Close()
	n := 0
	for _, d := range ds {
		err := s.tx(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE approval SET reminded_at=$2 WHERE id::text=$1 AND status='pending' AND reminded_at IS NULL`, d.id, s.Now())
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			var users []string
			ur, err := tx.Query(ctx, `SELECT user_id::text FROM approval_approver WHERE approval_id::text=$1 AND decision IS NULL`, d.id)
			if err != nil {
				return err
			}
			for ur.Next() {
				var u string
				ur.Scan(&u)
				users = append(users, u)
			}
			ur.Close()
			n++
			return outbox(ctx, tx, "issue:"+d.key, "approval.reminder", map[string]any{"key": d.key, "actor": "Tasklane", "actor_id": "", "users": users, "text": fmt.Sprintf("a decision on %s is due", d.key)})
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// RunApprovals runs the reminder sweep every minute until the context ends.
func (s *Service) RunApprovals(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if _, err := s.ApprovalSweep(ctx); err != nil && ctx.Err() == nil {
			logf("approval reminders: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// approvalGranted is the workflow condition approval_granted: the issue's latest approval was granted.
func (s *Service) approvalGranted(ctx context.Context, q querier, issueID string) bool {
	var st string
	err := q.QueryRow(ctx, `SELECT status FROM approval WHERE issue_id::text=$1 AND status<>'cancelled' ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&st)
	return err == nil && st == "granted"
}
