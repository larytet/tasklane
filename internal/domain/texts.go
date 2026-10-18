package domain

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/text"
)

var textFields = map[string]bool{"summary": true, "description": true}

// TextConflict is returned when a save was based on a stale text_seq (HTTP 409).
// It carries what the conflict dialog needs: current, base, who changed it, and the merge.
type TextConflict struct {
	Field      string      `json:"field"`
	Current    string      `json:"current"`
	CurrentSeq int64       `json:"current_seq"`
	Base       string      `json:"base"`
	BaseSeq    int64       `json:"base_seq"`
	By         string      `json:"by"`
	At         time.Time   `json:"at"`
	Mine       string      `json:"mine"`
	Merge      text.Result `json:"merge"`
}

func (e *TextConflict) Error() string { return "text conflict on " + e.Field }

// TextPrecondition is the 412 for a conditional API replace.
type TextPrecondition struct {
	Field      string `json:"field"`
	Current    string `json:"current"`
	CurrentSeq int64  `json:"current_seq"`
}

func (e *TextPrecondition) Error() string { return "text changed since If-Match" }

// PatchMismatch is returned when an anchored patch's old_text does not match exactly once.
type PatchMismatch struct {
	Matches    int    `json:"matches"`
	Current    string `json:"current"`
	CurrentSeq int64  `json:"current_seq"`
}

func (e *PatchMismatch) Error() string { return "old_text must match exactly once" }

type TextResult struct {
	Seq       int64    `json:"seq"`
	Removed   []string `json:"removed,omitempty"` // content sanitization removed, reported to the saver
	Unchanged bool     `json:"unchanged,omitempty"`
	Retried   bool     `json:"retried,omitempty"`
}

type SaveIn struct {
	Field   string
	Body    string
	BaseSeq int64
	Mode    string // save | keep_mine | merge
	SaveID  string // client-generated; makes retries idempotent
}

// SaveText is the UI save: compare-and-set on text_seq.
func (s *Service) SaveText(ctx context.Context, a Actor, key string, in SaveIn) (TextResult, error) {
	mode := in.Mode
	if mode == "" {
		mode = "save"
	}
	if mode != "save" && mode != "keep_mine" && mode != "merge" {
		return TextResult{}, invalid("unknown save mode %q", mode)
	}
	return s.mutateText(ctx, a, key, in.Field, mode, in.SaveID, func(cur string, seq int64, snapshot func(int64) string, last func() (string, time.Time)) (string, error) {
		if seq != in.BaseSeq {
			by, at := last()
			mine, _ := Sanitize(in.Body)
			base := snapshot(in.BaseSeq)
			return "", &TextConflict{Field: in.Field, Current: cur, CurrentSeq: seq, Base: base, BaseSeq: in.BaseSeq, By: by, At: at, Mine: mine,
				Merge: text.Merge(base, cur, mine)}
		}
		return in.Body, nil
	})
}

// PatchText is the anchored patch (preferred API/agent write): applies only if old matches once.
func (s *Service) PatchText(ctx context.Context, a Actor, key, field, oldText, newText string) (TextResult, error) {
	if oldText == "" {
		return TextResult{}, invalid("old_text is required")
	}
	return s.mutateText(ctx, a, key, field, "patch", "", func(cur string, seq int64, _ func(int64) string, _ func() (string, time.Time)) (string, error) {
		n := strings.Count(cur, oldText)
		if n != 1 {
			return "", &PatchMismatch{Matches: n, Current: cur, CurrentSeq: seq}
		}
		return strings.Replace(cur, oldText, newText, 1), nil
	})
}

// ReplaceText is a full replace. With ifMatch it is conditional (412 on mismatch); without it
// the text is last-write-wins and the overwritten version stays restorable.
func (s *Service) ReplaceText(ctx context.Context, a Actor, key, field, body string, ifMatch *int64) (TextResult, error) {
	return s.mutateText(ctx, a, key, field, "replace", "", func(cur string, seq int64, _ func(int64) string, _ func() (string, time.Time)) (string, error) {
		if ifMatch != nil && *ifMatch != seq {
			return "", &TextPrecondition{Field: field, Current: cur, CurrentSeq: seq}
		}
		return body, nil
	})
}

type textFn func(cur string, seq int64, snapshot func(int64) string, last func() (by string, at time.Time)) (string, error)

func validateText(field, v string) error {
	if err := checkSize(v); err != nil {
		return err
	}
	if field == "summary" {
		if strings.TrimSpace(v) == "" {
			return invalid("summary is required")
		}
		if strings.Contains(v, "\n") || len([]rune(v)) > maxSummary {
			return invalid("summary must be a single line of at most %d characters", maxSummary)
		}
	}
	return nil
}

func (s *Service) mutateText(ctx context.Context, a Actor, key, field, mode, saveID string, fn textFn) (TextResult, error) {
	if !textFields[field] {
		return TextResult{}, invalid("unknown text field %q", field)
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return TextResult{}, err
	}
	if !canWrite(role) {
		return TextResult{}, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return TextResult{}, perr
	}
	defer endWrite()
	var res TextResult
	err = s.tx(ctx, func(tx pgx.Tx) error {
		// lock the text row; the lock is what makes compare-and-set atomic
		var cur string
		var seq int64
		if field == "summary" {
			if err := tx.QueryRow(ctx, `SELECT summary,summary_seq FROM issue WHERE id=$1 FOR UPDATE`, is.ID).Scan(&cur, &seq); err != nil {
				return err
			}
		} else {
			if err := tx.QueryRow(ctx, `SELECT body,text_seq FROM issue_text WHERE issue_id=$1 AND field=$2 FOR UPDATE`, is.ID, field).Scan(&cur, &seq); err != nil {
				return err
			}
		}
		if saveID != "" {
			var rs int64
			err := tx.QueryRow(ctx, `SELECT seq FROM save_receipt WHERE save_id=$1`, saveID).Scan(&rs)
			if err == nil {
				res = TextResult{Seq: rs, Retried: true}
				return nil
			}
			if err != pgx.ErrNoRows {
				return err
			}
		}
		snapshot := func(at int64) string {
			var b string
			tx.QueryRow(ctx, `SELECT body FROM text_version WHERE issue_id=$1 AND field=$2 AND seq=$3`, is.ID, field, at).Scan(&b)
			return b
		}
		last := func() (string, time.Time) {
			var by string
			var at time.Time
			tx.QueryRow(ctx, `SELECT actor_label,at FROM text_version WHERE issue_id=$1 AND field=$2 ORDER BY seq DESC LIMIT 1`, is.ID, field).Scan(&by, &at)
			return by, at
		}
		nb, err := fn(cur, seq, snapshot, last)
		if err != nil {
			return err
		}
		clean, removed := Sanitize(nb)
		if field == "summary" {
			clean = strings.TrimSpace(clean)
		}
		if err := validateText(field, clean); err != nil {
			return err
		}
		res.Removed = removed
		if clean == cur {
			res.Seq, res.Unchanged = seq, true
			return nil
		}
		next := seq + 1
		if field == "summary" {
			_, err = tx.Exec(ctx, `UPDATE issue SET summary=$2,summary_seq=$3,updated_at=now() WHERE id=$1`, is.ID, clean, next)
		} else {
			_, err = tx.Exec(ctx, `UPDATE issue_text SET body=$3,text_seq=$4,updated_by=$5,updated_at=now() WHERE issue_id=$1 AND field=$2`, is.ID, field, clean, next, nilIfEmpty(a.ID))
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE issue SET updated_at=now() WHERE id=$1`, is.ID)
			}
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			is.ID, field, next, clean, nilIfEmpty(a.ID), a.Label, mode); err != nil {
			return err
		}
		if err := s.hist(ctx, tx, is.ID, a, field, &cur, &clean); err != nil {
			return err
		}
		if saveID != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO save_receipt(save_id,issue_id,field,seq) VALUES($1,$2,$3,$4)`, saveID, is.ID, field, next); err != nil {
				return err
			}
		}
		if a.ID != "" {
			tx.Exec(ctx, `DELETE FROM draft WHERE user_id=$1 AND issue_id=$2 AND field=$3`, a.ID, is.ID, field)
		}
		res.Seq = next
		return outbox(ctx, tx, "issue:"+is.Key, "issue.text_saved", map[string]any{"key": is.Key, "field": field, "text_seq": next, "version": is.Version, "actor": a.Label, "actor_id": a.ID, "mode": mode})
	})
	if err == nil {
		s.touchLease(ctx, a, is.ID)
	}
	if err == nil && !res.Unchanged && !res.Retried {
		s.emit(Event{Type: "issue.text_saved", Issue: is.Key, Field: field, TextSeq: res.Seq, Version: is.Version, Actor: a.Label, ActorID: a.ID})
	}
	return res, err
}

type TextVersion struct {
	Seq   int64     `json:"seq"`
	Body  string    `json:"body"`
	Actor string    `json:"actor"`
	Mode  string    `json:"mode"`
	At    time.Time `json:"at"`
}

func (s *Service) TextVersions(ctx context.Context, a Actor, key, field string) ([]TextVersion, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT seq,body,actor_label,mode,at FROM text_version WHERE issue_id=$1 AND field=$2 ORDER BY seq DESC`, is.ID, field)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TextVersion{}
	for rows.Next() {
		var v TextVersion
		rows.Scan(&v.Seq, &v.Body, &v.Actor, &v.Mode, &v.At)
		out = append(out, v)
	}
	return out, rows.Err()
}

// RestoreText saves an earlier version as a new change; history is never rewritten.
func (s *Service) RestoreText(ctx context.Context, a Actor, key, field string, seq int64) (TextResult, error) {
	vs, err := s.TextVersions(ctx, a, key, field)
	if err != nil {
		return TextResult{}, err
	}
	for _, v := range vs {
		if v.Seq == seq {
			return s.mutateText(ctx, a, key, field, "restore", "", func(string, int64, func(int64) string, func() (string, time.Time)) (string, error) {
				return v.Body, nil
			})
		}
	}
	return TextResult{}, ErrNotFound
}

// ---- drafts: private, per user and field, autosaved by the client ----

type Draft struct {
	Field     string    `json:"field"`
	Body      string    `json:"body"`
	BaseSeq   int64     `json:"base_seq"`
	UpdatedAt time.Time `json:"updated_at"`
	Stale     bool      `json:"stale"` // the saved text moved on since the draft's base
}

func (s *Service) SaveDraft(ctx context.Context, a Actor, key, field, body string, baseSeq int64) error {
	if !textFields[field] && field != "comment" {
		return invalid("unknown field %q", field)
	}
	if err := checkSize(body); err != nil {
		return err
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO draft(user_id,issue_id,field,body,base_seq) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (user_id,issue_id,field) DO UPDATE SET body=$4,base_seq=$5,updated_at=now()`, a.ID, is.ID, field, body, baseSeq)
	return err
}

func (s *Service) GetDrafts(ctx context.Context, a Actor, key string) ([]Draft, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT d.field,d.body,d.base_seq,d.updated_at,
		CASE d.field WHEN 'summary' THEN i.summary_seq WHEN 'comment' THEN d.base_seq ELSE COALESCE((SELECT text_seq FROM issue_text t WHERE t.issue_id=i.id AND t.field=d.field),1) END
		FROM draft d JOIN issue i ON i.id=d.issue_id WHERE d.user_id=$1 AND d.issue_id=$2`, a.ID, is.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Draft{}
	for rows.Next() {
		var d Draft
		var cur int64
		rows.Scan(&d.Field, &d.Body, &d.BaseSeq, &d.UpdatedAt, &cur)
		d.Stale = cur != d.BaseSeq
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Service) DeleteDraft(ctx context.Context, a Actor, key, field string) error {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM draft WHERE user_id=$1 AND issue_id=$2 AND field=$3`, a.ID, is.ID, field)
	return err
}
