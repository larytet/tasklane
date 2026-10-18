package search

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Admin is the operator's view of the search index: health, backlog and lag, dead letters, the last reconcile, and the
// maintenance actions (rebuild, reconcile, rollback).
type Admin struct {
	Index  *OSIndex
	Router *Router

	mu      sync.Mutex
	running string
	lastErr string
}

type AdminStatus struct {
	Engine        string          `json:"engine"`
	Health        string          `json:"health"`
	Error         string          `json:"error,omitempty"`
	ActiveIndex   string          `json:"active_index"`
	BuildingIndex string          `json:"building_index,omitempty"`
	PreviousIndex string          `json:"previous_index,omitempty"`
	BacklogEvents int64           `json:"backlog_events"`
	LagSeconds    float64         `json:"lag_seconds"`
	DeadLetters   int             `json:"dead_letters"`
	Running       string          `json:"running,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	Reconcile     json.RawMessage `json:"last_reconcile,omitempty"`
	ReconciledAt  *time.Time      `json:"reconciled_at,omitempty"`
}

func (a *Admin) Status(ctx context.Context) AdminStatus {
	st := AdminStatus{Engine: "postgres"}
	if a == nil || a.Index == nil {
		return st
	}
	st.Engine = "opensearch"
	if h, err := a.Index.C.Health(ctx); err != nil {
		st.Health, st.Error = "unreachable", err.Error()
	} else {
		st.Health = h
	}
	s, err := a.Index.state(ctx)
	if err == nil {
		st.ActiveIndex, st.BuildingIndex, st.PreviousIndex = s.Active, s.Building, s.Previous
	}
	if b, err := a.Index.Backlog(ctx); err == nil {
		st.BacklogEvents, st.LagSeconds = b.Events, b.Lag.Seconds()
	}
	st.DeadLetters, _ = a.Index.DeadLetters(ctx)
	var raw []byte
	a.Index.DB.QueryRow(ctx, `SELECT last_reconcile, reconciled_at FROM search_os_state WHERE id=1`).Scan(&raw, &st.ReconciledAt)
	if len(raw) > 0 {
		st.Reconcile = raw
	}
	a.mu.Lock()
	st.Running, st.LastError = a.running, a.lastErr
	a.mu.Unlock()
	return st
}

var ErrBusy = errors.New("another search maintenance job is running")

// StartRebuild runs a full online rebuild in the background (one at a time).
func (a *Admin) StartRebuild(logf func(string, ...any)) error {
	a.mu.Lock()
	if a.running != "" {
		a.mu.Unlock()
		return ErrBusy
	}
	a.running = "rebuild"
	a.lastErr = ""
	a.mu.Unlock()
	go func() {
		rep, err := a.Index.Rebuild(context.Background(), 0)
		a.mu.Lock()
		a.running = ""
		if err != nil {
			a.lastErr = err.Error()
		}
		a.mu.Unlock()
		if err != nil {
			logf("search rebuild: %v", err)
		} else {
			logf("search rebuild: %d documents into %s", rep.Documents, rep.Index)
		}
	}()
	return nil
}

func (a *Admin) Reconcile(ctx context.Context, project string) (ReconcileReport, error) {
	a.mu.Lock()
	if a.running != "" {
		a.mu.Unlock()
		return ReconcileReport{}, ErrBusy
	}
	a.running = "reconcile"
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.running = ""; a.mu.Unlock() }()
	return a.Index.Reconcile(ctx, project)
}

func (a *Admin) ReindexProject(ctx context.Context, project string) (int, error) {
	a.mu.Lock()
	if a.running != "" {
		a.mu.Unlock()
		return 0, ErrBusy
	}
	a.running = "reindex " + project
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.running = ""; a.mu.Unlock() }()
	return a.Index.ReindexProject(ctx, project)
}

func (a *Admin) Rollback(ctx context.Context) error { return a.Index.Rollback(ctx) }

// RunMaintenance reconciles the whole index every interval, so drift stays bounded and visible.
func (a *Admin) RunMaintenance(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if rep, err := a.Reconcile(ctx, ""); err != nil && !errors.Is(err, ErrBusy) {
				logf("search reconcile: %v", err)
			} else if err == nil && !rep.Clean() {
				logf("search reconcile repaired %d documents (%d missing, %d stale, %d orphaned)", rep.Repaired, len(rep.Missing), len(rep.Stale), len(rep.Orphans))
			}
		}
	}
}
