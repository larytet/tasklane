package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Undo instead of confirm (SPEC 4.6). A reversible action taken with an undo slot in its context (WithUndo) leaves a single-use
// token for its actor, written in the action's own transaction, so it records exactly the version the action produced. Undoing
// reverts only if nobody changed the issue since; a transition is put back to the previous status directly, even when the
// workflow has no way back or a condition would refuse it: it corrects the person's own action and is not a new transition, so
// no conditions, validators or post-functions run. Automation that already ran is not reversed.

const UndoWindow = 5 * time.Minute // the toast offers Undo for 5 s (longer while hovered or focused); the token outlives it

// UndoSlot receives the token an action leaves. A batch slot (WithUndoBatch) gathers the items of many actions, each in its own
// transaction, for one token written by SaveUndoBatch.
type UndoSlot struct {
	Token   string
	Expires time.Time
	batch   bool
	op      string
	items   []undoItem
}

// WithUndoBatch is WithUndo for a bulk run: one token undoes every issue the run changed.
func WithUndoBatch(ctx context.Context) (context.Context, *UndoSlot) {
	sl := &UndoSlot{batch: true}
	return context.WithValue(ctx, undoKey{}, sl), sl
}

// SaveUndoBatch writes the token for what a batch gathered; nothing when it changed nothing undoable.
func (s *Service) SaveUndoBatch(ctx context.Context, a Actor, sl *UndoSlot) error {
	if sl == nil || len(sl.items) == 0 || a.ID == "" {
		return nil
	}
	return s.tx(ctx, func(tx pgx.Tx) error { return s.writeUndo(ctx, tx, a, sl.op, sl.items, sl) })
}

type undoKey struct{}

// WithUndo asks the actions run with the returned context to leave an undo token in the slot.
func WithUndo(ctx context.Context) (context.Context, *UndoSlot) {
	sl := &UndoSlot{}
	return context.WithValue(ctx, undoKey{}, sl), sl
}

type undoItem struct {
	IssueID  string     `json:"issue_id"`
	Key      string     `json:"key"`
	Version  int64      `json:"version"`             // the issue's version right after the action
	StatusID string     `json:"status_id,omitempty"` // transition: the status before it
	Resolved *time.Time `json:"resolved,omitempty"`  // transition: resolved_at before it
	Acted    *time.Time `json:"acted,omitempty"`     // the database time of this issue's action (a batch has one per issue)
}

// recordUndo writes the token when the context asks for one. acted is the database's time of the action's transaction.
func (s *Service) recordUndo(ctx context.Context, tx pgx.Tx, a Actor, op string, items []undoItem) error {
	sl, _ := ctx.Value(undoKey{}).(*UndoSlot)
	if sl == nil || a.ID == "" || len(items) == 0 {
		return nil
	}
	if sl.batch {
		var now time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return err
		}
		for i := range items {
			items[i].Acted = &now
		}
		sl.op = op
		sl.items = append(sl.items, items...)
		return nil
	}
	return s.writeUndo(ctx, tx, a, op, items, sl)
}

func (s *Service) writeUndo(ctx context.Context, tx pgx.Tx, a Actor, op string, items []undoItem, sl *UndoSlot) error {
	raw := make([]byte, 18)
	rand.Read(raw)
	tok := "un_" + hex.EncodeToString(raw)
	b, _ := json.Marshal(items)
	exp := s.Now().Add(UndoWindow)
	if _, err := tx.Exec(ctx, `INSERT INTO undo_token(id,user_id,op,items,acted_at,expires_at) VALUES($1,$2,$3,$4,now(),$5)`, tok, a.ID, op, b, exp); err != nil {
		return err
	}
	sl.Token, sl.Expires = tok, exp
	return nil
}

// UndoResult says what was put back, and per issue what could not be.
type UndoResult struct {
	Op       string            `json:"op"`
	Undone   []string          `json:"undone"`
	Conflict map[string]string `json:"conflicts,omitempty"` // key -> why it was left as it is
}

// UndoChanged: the issue was changed by someone (or something) after the action; nothing was reverted.
type UndoChanged struct{ Result UndoResult }

func (e *UndoChanged) Error() string {
	for k, why := range e.Result.Conflict {
		return k + " " + why + ", so it was not undone"
	}
	return "the issue changed since, so it was not undone"
}

// Undo reverts the action the token was left by. Only its own actor may use it, once, before it expires; any other use is
// ErrNotFound, so a token says nothing about anyone else's work. The same person in another tab or session may use it.
func (s *Service) Undo(ctx context.Context, a Actor, token string) (UndoResult, error) {
	if a.ID == "" || len(token) > 100 {
		return UndoResult{}, ErrNotFound
	}
	var op string
	var raw []byte
	var acted time.Time
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE undo_token SET used_at=now() WHERE id=$1 AND user_id=$2 AND used_at IS NULL AND expires_at > $3`, token, a.ID, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return tx.QueryRow(ctx, `SELECT op,items,acted_at FROM undo_token WHERE id=$1`, token).Scan(&op, &raw, &acted)
	})
	if err != nil {
		return UndoResult{}, err
	}
	var items []undoItem
	json.Unmarshal(raw, &items)
	res := UndoResult{Op: op, Undone: []string{}, Conflict: map[string]string{}}
	for _, it := range items {
		at := acted
		if it.Acted != nil {
			at = *it.Acted
		}
		key, err := s.undoOne(ctx, a, op, it, at)
		var ch *undoConflict
		switch {
		case errors.As(err, &ch):
			res.Conflict[key] = ch.why
		case err != nil && len(items) == 1:
			s.releaseUndo(ctx, token, len(items)) // nothing was reverted: the person may try again
			return res, err
		case err != nil: // in a batch, one issue that cannot be put back does not stop the others
			res.Conflict[key] = bulkErrText(err)
		default:
			res.Undone = append(res.Undone, key)
		}
	}
	if len(res.Undone) == 0 && len(res.Conflict) > 0 {
		return res, &UndoChanged{res}
	}
	if len(res.Conflict) == 0 {
		res.Conflict = nil
	}
	return res, nil
}

// releaseUndo makes a token usable again after a failure that reverted nothing (a pause, a lost connection to the database).
func (s *Service) releaseUndo(ctx context.Context, token string, n int) {
	if n == 1 {
		s.DB.Exec(ctx, `UPDATE undo_token SET used_at=NULL WHERE id=$1`, token)
	}
}

type undoConflict struct{ why string }

func (e *undoConflict) Error() string { return e.why }

// changedSince reports why an issue may no longer be put back: its version moved, its row or text was written after the action,
// or someone commented. Watching and viewing do not count.
func changedSince(ctx context.Context, tx pgx.Tx, it undoItem, acted time.Time) (string, bool, error) {
	var ver int64
	var later, commented bool
	err := tx.QueryRow(ctx, `SELECT i.version, i.updated_at > $2, EXISTS(SELECT 1 FROM comment c WHERE c.issue_id=i.id AND c.created_at > $2) FROM issue i WHERE i.id=$1 FOR UPDATE OF i`, it.IssueID, acted).Scan(&ver, &later, &commented)
	if err != nil {
		return "", false, err
	}
	switch {
	case ver != it.Version || later:
		return "was changed since", true, nil
	case commented:
		return "has a new comment since", true, nil
	}
	return "", false, nil
}

func (s *Service) undoOne(ctx context.Context, a Actor, op string, it undoItem, acted time.Time) (string, error) {
	key := it.Key
	if op == "delete" { // the restore of the trash, which checks who may restore
		var bad string
		err := s.tx(ctx, func(tx pgx.Tx) error {
			why, changed, err := changedSince(ctx, tx, it, acted)
			if err != nil {
				return err
			}
			if changed {
				bad = why
			}
			return nil
		})
		if err != nil {
			return key, err
		}
		if bad != "" {
			return key, &undoConflict{bad}
		}
		if _, err := s.Lifecycle(ctx, a, key, LifecycleIn{Op: "restore", ExpectKey: key}); err != nil {
			return key, err
		}
		return key, s.auditDirect(ctx, a, "issue.undo", key, map[string]any{"op": "delete"})
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return key, err
	}
	key = is.Key
	if !canWrite(role) && !(op == "archive" && is.Archived != nil) {
		return key, ErrForbidden
	}
	if op == "archive" { // the issue is archived, so access says read-only: the person's own role decides, as for unarchive
		pid, perr := s.projectID(ctx, is.Project)
		if perr != nil {
			return key, perr
		}
		if !canWrite(s.role(ctx, a, pid)) {
			return key, ErrForbidden
		}
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return key, perr
	}
	defer endWrite()
	var ev Event
	err = s.tx(ctx, func(tx pgx.Tx) error {
		why, changed, err := changedSince(ctx, tx, it, acted)
		if err != nil {
			return err
		}
		if changed {
			return &undoConflict{why}
		}
		next := it.Version + 1
		switch op {
		case "transition":
			var raw []byte
			var fromName, toName string
			if err := tx.QueryRow(ctx, `SELECT i.field_ver, st.name, (SELECT name FROM status WHERE id=$2) FROM issue i JOIN status st ON st.id=i.status_id WHERE i.id=$1`, it.IssueID, it.StatusID).Scan(&raw, &fromName, &toName); err != nil {
				return err
			}
			var fv map[string]int64
			json.Unmarshal(raw, &fv)
			if fv == nil {
				fv = map[string]int64{}
			}
			fv["status"] = next
			fvj, _ := json.Marshal(fv)
			if _, err := tx.Exec(ctx, `UPDATE issue SET status_id=$2,version=$3,field_ver=$4,updated_at=now(),resolved_at=$5 WHERE id=$1`, it.IssueID, it.StatusID, next, fvj, it.Resolved); err != nil {
				return err
			}
			if err := s.hist(ctx, tx, it.IssueID, a, "status", &fromName, &toName); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, a, "issue.undo", key, map[string]any{"op": "transition", "from": fromName, "to": toName}); err != nil {
				return err
			}
			ev = Event{Type: "issue.updated", Issue: key, Fields: []string{"status"}, Version: next, Actor: a.Label, ActorID: a.ID}
			return outbox(ctx, tx, "issue:"+key, "issue.updated", map[string]any{"key": key, "fields": []string{"status"}, "from": fromName, "to": toName, "undo": true, "version": next, "actor": a.Label, "actor_id": a.ID})
		case "archive":
			if _, err := tx.Exec(ctx, `UPDATE issue SET archived_at=NULL,version=$2,updated_at=now() WHERE id=$1`, it.IssueID, next); err != nil {
				return err
			}
			before, after := "archived", ""
			if err := s.hist(ctx, tx, it.IssueID, a, "archived", &before, &after); err != nil {
				return err
			}
			if err := s.audit(ctx, tx, a, "issue.undo", key, map[string]any{"op": "archive"}); err != nil {
				return err
			}
			ev = Event{Type: "issue.updated", Issue: key, Fields: []string{"archived"}, Version: next, Actor: a.Label, ActorID: a.ID}
			return outbox(ctx, tx, "issue:"+key, "issue.updated", map[string]any{"key": key, "fields": []string{"archived"}, "undo": true, "version": next, "actor": a.Label, "actor_id": a.ID})
		}
		return invalid("unknown undo %q", op)
	})
	if err != nil {
		return key, err
	}
	if op == "transition" {
		s.slaTouch(ctx, it.IssueID)
	}
	s.emit(ev)
	return key, nil
}

func (s *Service) auditDirect(ctx context.Context, a Actor, action, target string, detail any) error {
	return s.tx(ctx, func(tx pgx.Tx) error { return s.audit(ctx, tx, a, action, target, detail) })
}
