// Package domain holds the tracker's business logic. Every mutation is one transaction that
// writes the change, its history and an outbox event (DESIGN 11.2).
package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	DB       *pgxpool.Pool
	Now      func() time.Time
	Presence *Presence
	Blobs    *Blobs
	Mail     *MailConfig
	gateOnce sync.Once
	gate     *gate
	// TrashRetention is how long a deleted issue stays restorable (default 30 days).
	TrashRetention time.Duration
	// Publish, when set, receives every committed change for the realtime channel. Events are best effort:
	// a lost event is repaired by a refetch, and nothing depends on delivery.
	Publish func(Event)
}

func New(db *pgxpool.Pool) *Service {
	s := &Service{DB: db, Now: time.Now}
	s.Presence = NewPresence(func() time.Time { return s.Now() })
	return s
}

// Actor is whoever performs an action: a person, an API token, an agent or an automation rule.
type Actor struct {
	ID    string `json:"id"`
	Label string `json:"label"` // shown in history and presence, e.g. "Jana K.", "API: bot", "Agent: claude"
	Admin bool   `json:"admin"`
	Kind  string `json:"kind"`
}

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
	// ErrGone: the issue is in the trash (HTTP 410). Only people who can see its project are told.
	ErrGone = errors.New("gone")
)

// InvalidError is a validation failure that is the caller's fault (HTTP 400).
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string  { return e.Msg }
func invalid(f string, a ...any) error { return &InvalidError{fmt.Sprintf(f, a...)} }

// VersionConflict is returned when a scalar write is based on a stale issue version.
type VersionConflict struct {
	CurrentVersion int64    `json:"current_version"`
	Fields         []string `json:"fields"` // fields changed by someone else since the caller's base
}

func (e *VersionConflict) Error() string { return "version conflict on " + fmt.Sprint(e.Fields) }

func (s *Service) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func outbox(ctx context.Context, tx pgx.Tx, aggregate, typ string, payload any) error {
	b, _ := json.Marshal(payload)
	_, err := tx.Exec(ctx, `INSERT INTO outbox(aggregate,type,payload) VALUES($1,$2,$3)`, aggregate, typ, b)
	return err
}

func (s *Service) hist(ctx context.Context, tx pgx.Tx, issueID string, a Actor, field string, before, after *string) error {
	if field != "created" {
		var track bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT track_history FROM field_setting WHERE field=$1), true)`, field).Scan(&track); err == nil && !track {
			return nil // an untracked field leaves no history record (text versions are kept: they are what a conflict and a restore need)
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO issue_history(issue_id,field,before,after,actor_id,actor_label) VALUES($1,$2,$3,$4,$5,$6)`,
		issueID, field, before, after, nilIfEmpty(a.ID), a.Label)
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func sp(s string) *string { return &s }

// Event is a committed change, carrying both counters so open pages know what moved (SPEC 4.5).
type Event struct {
	Type    string   `json:"type"` // issue.updated, issue.transitioned, issue.text_saved, comment.added, attachment.changed
	Issue   string   `json:"issue"`
	Fields  []string `json:"fields,omitempty"`
	Version int64    `json:"version,omitempty"`
	TextSeq int64    `json:"text_seq,omitempty"`
	Field   string   `json:"field,omitempty"`
	Actor   string   `json:"actor"`
	ActorID string   `json:"actor_id"`
}

func (s *Service) emit(e Event) {
	if s.Publish != nil {
		s.Publish(e)
	}
}

// CanRead reports whether the actor may read the issue; the realtime gateway authorizes channels with it.
func (s *Service) CanRead(ctx context.Context, a Actor, key string) bool {
	_, _, err := s.access(ctx, a, key)
	return err == nil
}

// trackable lists the fields whose history can be switched off.
var trackable = map[string]bool{"summary": true, "description": true, "priority": true, "assignee": true, "labels": true, "status": true, "attachment": true}

// SetFieldTracking turns history on or off for a field going forward; records already written stay.
func (s *Service) SetFieldTracking(ctx context.Context, a Actor, field string, track bool) error {
	if !a.Admin {
		return ErrForbidden
	}
	if !trackable[field] {
		return invalid("unknown field %q", field)
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO field_setting(field,track_history) VALUES($1,$2) ON CONFLICT (field) DO UPDATE SET track_history=$2`, field, track)
	return err
}

func (s *Service) FieldTracking(ctx context.Context, a Actor) (map[string]bool, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	out := map[string]bool{}
	for f := range trackable {
		out[f] = true
	}
	rows, err := s.DB.Query(ctx, `SELECT field, track_history FROM field_setting`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		var t bool
		rows.Scan(&f, &t)
		out[f] = t
	}
	return out, rows.Err()
}

func jsonUnmarshal(b []byte, v any) { _ = json.Unmarshal(b, v) }
func jsonMarshal(v any) []byte      { b, _ := json.Marshal(v); return b }
