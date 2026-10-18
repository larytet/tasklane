package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Plan is what a lifecycle operation will do (SPEC 9.1). A dry run produces it by running the operation inside a
// transaction and rolling that back, so the plan and the result cannot disagree except when someone else changes the
// issue in between, and then the result is reported per issue rather than half applied.
type Plan struct {
	Op       string            `json:"op"`
	Key      string            `json:"key"`
	Counts   map[string]int    `json:"counts"`
	Changes  []string          `json:"changes"`
	Drops    []string          `json:"drops"`
	Warnings []string          `json:"warnings"`
	Blockers []string          `json:"blockers"`
	NewKeys  map[string]string `json:"new_keys,omitempty"` // old key -> key after the operation (move, clone)
	Executed bool              `json:"executed"`
	Key2     string            `json:"result_key,omitempty"` // the issue's key after the operation
}

func newPlan(op, key string) *Plan {
	return &Plan{Op: op, Key: key, Counts: map[string]int{}, Changes: []string{}, Drops: []string{}, Warnings: []string{}, Blockers: []string{}}
}

func (p *Plan) change(f string, a ...any) { p.Changes = append(p.Changes, fmt.Sprintf(f, a...)) }
func (p *Plan) drop(f string, a ...any)   { p.Drops = append(p.Drops, fmt.Sprintf(f, a...)) }
func (p *Plan) warn(f string, a ...any)   { p.Warnings = append(p.Warnings, fmt.Sprintf(f, a...)) }
func (p *Plan) block(f string, a ...any)  { p.Blockers = append(p.Blockers, fmt.Sprintf(f, a...)) }

// BlockedError is returned when a plan has blockers; the plan says what to fix.
type BlockedError struct{ Plan *Plan }

func (e *BlockedError) Error() string {
	return "the operation cannot run: " + strings.Join(e.Plan.Blockers, "; ")
}

// ConflictError is returned when the issue changed under the operation (moved, deleted, converted by someone else).
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

type CloneOpts struct {
	Project      string  `json:"project"`     // target project key, default the source's
	Attachments  *bool   `json:"attachments"` // default true
	Links        bool    `json:"links"`       // default false
	Subtasks     *bool   `json:"subtasks"`    // default true
	Comments     bool    `json:"comments"`    // default false
	KeepAssignee bool    `json:"keep_assignee"`
	NoCloneLink  bool    `json:"no_clone_link"` // the "clones" link back is on by default
	Prefix       *string `json:"prefix"`        // default "CLONE - "
}

type LifecycleIn struct {
	Op        string            `json:"op"`         // delete, restore, purge, clone, move, convert
	DryRun    bool              `json:"dry_run"`    // return the plan and change nothing
	Project   string            `json:"project"`    // move: target project
	Type      string            `json:"type"`       // convert: new type
	TypeMap   map[string]string `json:"type_map"`   // move: source type -> target type
	StatusMap map[string]string `json:"status_map"` // convert and move: source status -> target status
	Clone     CloneOpts         `json:"clone"`
	// ExpectKey guards a bulk or stale plan: the operation refuses to run if the issue is no longer under this key.
	ExpectKey string `json:"expect_key"`
}

const (
	defaultTrashRetention = 30 * 24 * time.Hour
	defaultSubtaskCap     = 50
)

func (s *Service) trashRetention() time.Duration {
	if s.TrashRetention > 0 {
		return s.TrashRetention
	}
	return defaultTrashRetention
}

// any issue, live or trashed, by key or alias
type rawIssue struct {
	ID, Key, ProjectID, ProjectKey, TypeID, TypeName, StatusID, StatusName, StatusCat string
	Level                                                                             int
	ParentID, ReporterID                                                              *string
	DeletedAt                                                                         *time.Time
	DeletedBy                                                                         *string
	DeleteBatch                                                                       *string
	LegalHold                                                                         bool
	Version                                                                           int64
	Summary                                                                           string
}

const rawSelect = `SELECT i.id,i.key,i.project_id,p.key,i.issue_type_id,t.name,t.level,i.status_id,st.name,st.category,i.parent_id,i.reporter_id,i.deleted_at,i.deleted_by,i.delete_batch,i.legal_hold,i.version,i.summary
	FROM issue i JOIN project p ON p.id=i.project_id JOIN issue_type t ON t.id=i.issue_type_id JOIN status st ON st.id=i.status_id `

func scanRaw(row pgx.Row) (rawIssue, error) {
	var r rawIssue
	err := row.Scan(&r.ID, &r.Key, &r.ProjectID, &r.ProjectKey, &r.TypeID, &r.TypeName, &r.Level, &r.StatusID, &r.StatusName, &r.StatusCat, &r.ParentID, &r.ReporterID, &r.DeletedAt, &r.DeletedBy, &r.DeleteBatch, &r.LegalHold, &r.Version, &r.Summary)
	if err == pgx.ErrNoRows {
		return r, ErrNotFound
	}
	return r, err
}

// lockIssue finds an issue by key or alias in any state and locks the row for the rest of the transaction.
func lockIssue(ctx context.Context, tx pgx.Tx, key string) (rawIssue, error) {
	key = strings.ToUpper(key)
	r, err := scanRaw(tx.QueryRow(ctx, rawSelect+`WHERE i.key=$1 FOR UPDATE OF i`, key))
	if err == ErrNotFound {
		r, err = scanRaw(tx.QueryRow(ctx, rawSelect+`WHERE i.id=(SELECT issue_id FROM issue_key_alias WHERE old_key=$1) FOR UPDATE OF i`, key))
	}
	return r, err
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, a Actor, action, target string, detail any) error {
	b, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,$3,$4,$5)`, nilIfEmpty(a.ID), a.Label, action, target, b)
	return err
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Lifecycle runs one operation on one issue. With DryRun it returns the plan and changes nothing.
func (s *Service) Lifecycle(ctx context.Context, a Actor, key string, in LifecycleIn) (*Plan, error) {
	op := strings.ToLower(in.Op)
	switch op {
	case "delete", "restore", "purge", "clone", "move", "convert":
	default:
		return nil, invalid("unknown operation %q (delete, restore, purge, clone, move or convert)", in.Op)
	}
	plan := newPlan(op, strings.ToUpper(key))
	var srcProject string
	s.DB.QueryRow(ctx, `SELECT p.key FROM issue i JOIN project p ON p.id=i.project_id WHERE i.key=$1 OR i.id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1) LIMIT 1`, plan.Key).Scan(&srcProject)
	if srcProject != "" && !in.DryRun { // a dry run only reads; a real operation counts as a write in every project it touches
		endWrite, perr := s.admit(ctx, srcProject)
		if perr != nil {
			return nil, perr
		}
		defer endWrite()
		tgt := in.Project
		if op == "clone" {
			tgt = in.Clone.Project
		}
		if tgt != "" && !strings.EqualFold(tgt, srcProject) && (op == "move" || op == "clone") {
			endWrite2, perr := s.admit(ctx, tgt)
			if perr != nil {
				return nil, perr
			}
			defer endWrite2()
		}
	}
	var events []Event
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if op == "move" || op == "convert" { // archived work is read-only; moving or converting it would change it
		var archived bool
		tx.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM issue WHERE key=$1 OR id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1) LIMIT 1`, plan.Key).Scan(&archived)
		if archived {
			return plan, invalid("%s is archived and read-only: unarchive it first", plan.Key)
		}
	}
	switch op {
	case "delete":
		err = s.opDelete(ctx, tx, a, key, in, plan, &events)
	case "restore":
		err = s.opRestore(ctx, tx, a, key, in, plan, &events)
	case "purge":
		err = s.opPurge(ctx, tx, a, key, in, plan, &events)
	case "clone":
		err = s.opClone(ctx, tx, a, key, in, plan, &events)
	case "move":
		err = s.opMove(ctx, tx, a, key, in, plan, &events)
	case "convert":
		err = s.opConvert(ctx, tx, a, key, in, plan, &events)
	}
	if err != nil {
		return plan, err
	}
	if len(plan.Blockers) > 0 {
		return plan, &BlockedError{plan}
	}
	if in.DryRun {
		return plan, nil // the deferred rollback discards everything the operation did
	}
	if err := tx.Commit(ctx); err != nil {
		return plan, err
	}
	plan.Executed = true
	for _, e := range events {
		s.emit(e)
	}
	return plan, nil
}

func (s *Service) projectRole(ctx context.Context, tx pgx.Tx, a Actor, projectID string) string {
	if a.Admin {
		return "admin"
	}
	var r string
	tx.QueryRow(ctx, `SELECT role FROM effective_member WHERE project_id=$1 AND user_id=$2`, projectID, a.ID).Scan(&r)
	return r
}

// visible returns the issue only if the actor can see its project; everyone else gets ErrNotFound, never a hint.
func (s *Service) visible(ctx context.Context, tx pgx.Tx, a Actor, key string) (rawIssue, string, error) {
	r, err := lockIssue(ctx, tx, key)
	if err != nil {
		return r, "", err
	}
	role := s.projectRole(ctx, tx, a, r.ProjectID)
	if role == "" {
		return r, "", ErrNotFound
	}
	return r, role, nil
}

func (s *Service) opDelete(ctx context.Context, tx pgx.Tx, a Actor, key string, in LifecycleIn, p *Plan, ev *[]Event) error {
	r, role, err := s.visible(ctx, tx, a, key)
	if err != nil {
		return err
	}
	if r.DeletedAt != nil {
		return ErrGone
	}
	if in.ExpectKey != "" && !strings.EqualFold(in.ExpectKey, r.Key) {
		return &ConflictError{fmt.Sprintf("the issue is now %s, not %s", r.Key, in.ExpectKey)}
	}
	// who may delete: a project administrator, or the reporter (who can edit)
	mine := r.ReporterID != nil && *r.ReporterID == a.ID
	if !(role == "admin" || (mine && canWrite(role))) {
		return ErrForbidden
	}
	p.Key = r.Key
	batch := newUUID()
	now := s.Now()
	purge := now.Add(s.trashRetention())
	kids, err := tx.Query(ctx, `SELECT i.id,i.key,t.level FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.parent_id=$1 AND i.deleted_at IS NULL ORDER BY i.number`, r.ID)
	if err != nil {
		return err
	}
	type kid struct {
		id, key string
		level   int
	}
	var children []kid
	for kids.Next() {
		var k kid
		kids.Scan(&k.id, &k.key, &k.level)
		children = append(children, k)
	}
	kids.Close()
	trash := []kid{{r.ID, r.Key, r.Level}}
	for _, c := range children {
		if c.level < r.Level && c.level == -1 { // subtasks go with their parent
			trash = append(trash, c)
			p.change("delete subtask %s with its parent", c.key)
			p.Counts["subtasks_deleted"]++
		} else { // an epic's children are detached, not deleted
			p.change("detach %s from the deleted parent", c.key)
			p.Counts["children_detached"]++
			if _, err := tx.Exec(ctx, `UPDATE issue SET parent_id=NULL, version=version+1, updated_at=now() WHERE id=$1`, c.id); err != nil {
				return err
			}
			b, _ := json.Marshal(map[string]string{"parent": r.Key})
			if _, err := tx.Exec(ctx, `INSERT INTO removed_values(issue_id,kind,data,expires_at) VALUES($1,'detach',$2,$3)`, c.id, b, purge); err != nil {
				return err
			}
			s.hist(ctx, tx, c.id, a, "parent", sp(r.Key), sp(""))
			outbox(ctx, tx, "issue:"+c.key, "issue.updated", map[string]any{"key": c.key, "fields": []string{"parent"}, "actor": a.Label, "actor_id": a.ID})
		}
	}
	for _, t := range trash {
		if _, err := tx.Exec(ctx, `UPDATE issue SET deleted_at=$2, deleted_by=$3, purge_after=$4, delete_batch=$5, version=version+1 WHERE id=$1`, t.id, now, nilIfEmpty(a.ID), purge, batch); err != nil {
			return err
		}
		s.hist(ctx, tx, t.id, a, "deleted", nil, sp(t.key))
		if err := outbox(ctx, tx, "issue:"+t.key, "issue.deleted", map[string]any{"key": t.key, "actor": a.Label, "actor_id": a.ID}); err != nil {
			return err
		}
		*ev = append(*ev, Event{Type: "issue.deleted", Issue: t.key, Actor: a.Label, ActorID: a.ID})
	}
	p.Counts["issues"] = len(trash)
	p.change("move %s to the trash until %s; its key stays reserved", r.Key, purge.Format("2006-01-02"))
	p.Warnings = append(p.Warnings, "links to this issue will show a deleted-issue placeholder")
	return s.audit(ctx, tx, a, "issue.delete", r.Key, map[string]any{"issues": len(trash), "purge_after": purge})
}

func (s *Service) opRestore(ctx context.Context, tx pgx.Tx, a Actor, key string, in LifecycleIn, p *Plan, ev *[]Event) error {
	r, role, err := s.visible(ctx, tx, a, key)
	if err != nil {
		return err
	}
	if r.DeletedAt == nil {
		p.block("%s is not in the trash", r.Key)
		return nil
	}
	deleter := r.DeletedBy != nil && *r.DeletedBy == a.ID
	if !(role == "admin" || deleter) { // the deleter, a project administrator or an administrator
		return ErrForbidden
	}
	p.Key = r.Key
	rows, err := tx.Query(ctx, `SELECT id,key FROM issue WHERE delete_batch=$1 AND deleted_at IS NOT NULL ORDER BY number`, r.DeleteBatch)
	if err != nil {
		return err
	}
	type it struct{ id, key string }
	var batch []it
	for rows.Next() {
		var i it
		rows.Scan(&i.id, &i.key)
		batch = append(batch, i)
	}
	rows.Close()
	if len(batch) == 0 {
		batch = []it{{r.ID, r.Key}}
	}
	for _, i := range batch {
		if _, err := tx.Exec(ctx, `UPDATE issue SET deleted_at=NULL, deleted_by=NULL, purge_after=NULL, delete_batch=NULL, version=version+1, updated_at=now() WHERE id=$1`, i.id); err != nil {
			return err
		}
		s.hist(ctx, tx, i.id, a, "restored", nil, sp(i.key))
		outbox(ctx, tx, "issue:"+i.key, "issue.restored", map[string]any{"key": i.key, "actor": a.Label, "actor_id": a.ID})
		*ev = append(*ev, Event{Type: "issue.restored", Issue: i.key, Actor: a.Label, ActorID: a.ID})
		if i.id != r.ID {
			p.change("restore subtask %s", i.key)
			p.Counts["subtasks_restored"]++
		}
	}
	p.Counts["issues"] = len(batch)
	p.change("restore %s with its status %q, comments, attachments and key", r.Key, r.StatusName)
	p.Key2 = r.Key
	return s.audit(ctx, tx, a, "issue.restore", r.Key, map[string]any{"issues": len(batch)})
}

func (s *Service) opPurge(ctx context.Context, tx pgx.Tx, a Actor, key string, in LifecycleIn, p *Plan, ev *[]Event) error {
	if a.Kind == "agent" || a.Kind == "bot" {
		return ErrForbidden // purge is never available to agents
	}
	r, role, err := s.visible(ctx, tx, a, key)
	if err != nil {
		return err
	}
	if role != "admin" { // the Purge permission
		return ErrForbidden
	}
	p.Key = r.Key
	if r.DeletedAt == nil {
		p.block("%s is not in the trash; delete it first", r.Key)
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id,key,legal_hold FROM issue WHERE delete_batch=$1 AND deleted_at IS NOT NULL ORDER BY number`, r.DeleteBatch)
	if err != nil {
		return err
	}
	type it struct {
		id, key string
		hold    bool
	}
	var batch []it
	for rows.Next() {
		var i it
		rows.Scan(&i.id, &i.key, &i.hold)
		batch = append(batch, i)
	}
	rows.Close()
	if len(batch) == 0 {
		batch = []it{{r.ID, r.Key, r.LegalHold}}
	}
	for _, i := range batch {
		if i.hold {
			p.block("%s is under legal hold and cannot be purged", i.key)
		}
	}
	if len(p.Blockers) > 0 {
		return nil
	}
	for _, i := range batch {
		n, err := s.purgeOne(ctx, tx, i.id)
		if err != nil {
			return err
		}
		p.Counts["blobs_released"] += n
		p.Counts["issues"]++
		p.change("hard-delete %s with its comments, drafts, history, text versions and links", i.key)
		*ev = append(*ev, Event{Type: "issue.purged", Issue: i.key, Actor: a.Label, ActorID: a.ID})
	}
	return s.audit(ctx, tx, a, "issue.purge", r.Key, map[string]any{"issues": len(batch), "blobs_released": p.Counts["blobs_released"]}) // the audit event outlives the data
}

// purgeOne hard-deletes one issue and releases its blob references for the garbage collector.
func (s *Service) purgeOne(ctx context.Context, tx pgx.Tx, id string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT blob_id, thumb_blob_id FROM attachment WHERE issue_id=$1`, id)
	if err != nil {
		return 0, err
	}
	var blobs []string
	for rows.Next() {
		var b string
		var t *string
		rows.Scan(&b, &t)
		blobs = append(blobs, b)
		if t != nil {
			blobs = append(blobs, *t)
		}
	}
	rows.Close()
	for _, b := range blobs {
		if _, err := tx.Exec(ctx, `UPDATE blob SET refcount=refcount-1, zero_since=CASE WHEN refcount-1<=0 THEN $2::timestamptz ELSE zero_since END WHERE id=$1`, b, s.Now()); err != nil {
			return 0, err
		}
	}
	// children that still point at it lose the link (they were detached at delete time; this covers restores in between)
	if _, err := tx.Exec(ctx, `UPDATE issue SET parent_id=NULL WHERE parent_id=$1`, id); err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `DELETE FROM issue WHERE id=$1`, id) // cascades: text, versions, drafts, comments, history, attachments, links, aliases, receipts, search rows
	return len(blobs), err
}

// PurgeExpired hard-deletes trashed issues whose retention ended; legal hold blocks it. Run by the janitor.
func (s *Service) PurgeExpired(ctx context.Context) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT delete_batch FROM issue WHERE deleted_at IS NOT NULL AND purge_after<=$1 AND delete_batch IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM issue h WHERE h.delete_batch=issue.delete_batch AND h.legal_hold)`, s.Now())
	if err != nil {
		return 0, err
	}
	var batches []string
	for rows.Next() {
		var b string
		rows.Scan(&b)
		batches = append(batches, b)
	}
	rows.Close()
	sort.Strings(batches)
	n := 0
	for _, b := range batches {
		err := s.tx(ctx, func(tx pgx.Tx) error {
			ids, err := tx.Query(ctx, `SELECT id,key FROM issue WHERE delete_batch=$1 AND deleted_at IS NOT NULL FOR UPDATE`, b)
			if err != nil {
				return err
			}
			type it struct{ id, key string }
			var all []it
			for ids.Next() {
				var i it
				ids.Scan(&i.id, &i.key)
				all = append(all, i)
			}
			ids.Close()
			for _, i := range all {
				if _, err := s.purgeOne(ctx, tx, i.id); err != nil {
					return err
				}
				n++
			}
			return s.audit(ctx, tx, Actor{Label: "Retention policy"}, "issue.purge", "batch:"+b, map[string]any{"issues": len(all), "reason": "retention ended"})
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// SetLegalHold blocks purge of an issue (administrators only).
func (s *Service) SetLegalHold(ctx context.Context, a Actor, key string, hold bool) error {
	if !a.Admin {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `UPDATE issue SET legal_hold=$2 WHERE key=$1 OR id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1)`, strings.ToUpper(key), hold)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type TrashEntry struct {
	Key        string    `json:"key"`
	Summary    string    `json:"summary"`
	DeletedAt  time.Time `json:"deleted_at"`
	DeletedBy  string    `json:"deleted_by"`
	PurgeAfter time.Time `json:"purge_after"`
	LegalHold  bool      `json:"legal_hold"`
}

// Trash lists a project's trash for people who may restore from it (the deleter sees their own; administrators see all).
func (s *Service) Trash(ctx context.Context, a Actor, projectKey string) ([]TrashEntry, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	role := s.role(ctx, a, pid)
	if role == "" {
		return nil, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT i.key,i.summary,i.deleted_at,COALESCE(u.name,''),i.purge_after,i.legal_hold FROM issue i LEFT JOIN app_user u ON u.id=i.deleted_by
		WHERE i.project_id=$1 AND i.deleted_at IS NOT NULL AND ($2 OR i.deleted_by=$3) ORDER BY i.deleted_at DESC`, pid, role == "admin", nilIfEmpty(a.ID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TrashEntry{}
	for rows.Next() {
		var e TrashEntry
		rows.Scan(&e.Key, &e.Summary, &e.DeletedAt, &e.DeletedBy, &e.PurgeAfter, &e.LegalHold)
		out = append(out, e)
	}
	return out, rows.Err()
}

var _ = errors.New

// RunLifecycle purges issues whose trash retention ended, hourly.
func (s *Service) RunLifecycle(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := s.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
			logf("purge: %v", err)
		} else if n > 0 {
			logf("purged %d expired issue(s) from the trash", n)
		}
		if n, err := s.AutoArchive(ctx, 500); err != nil && ctx.Err() == nil {
			logf("auto-archive: %v", err)
		} else if n > 0 {
			logf("auto-archived %d issue(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
