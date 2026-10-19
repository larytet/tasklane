package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The pause state (DESIGN 11.8). A job that must not race with writes (an import into existing data, a restore) takes a
// lease on a scope: new writes in scope wait briefly, then get 503 with Retry-After; in-flight writes finish first (draining);
// then the scope is paused until the job releases it or the lease expires. Reads, presence and drafts are untouched.

// ErrPaused is returned to a writer in a paused scope (HTTP 503 with Retry-After).
type ErrPaused struct {
	Scope      string
	RetryAfter time.Duration
	Owner      string
}

func (e *ErrPaused) Error() string {
	return fmt.Sprintf("maintenance in progress on %s (%s); retry in %d s", e.Scope, e.Owner, int(e.RetryAfter.Seconds())+1)
}

// ErrDrainTimeout: the in-flight writes did not finish in time, so the pause was abandoned and the scope runs again.
var ErrDrainTimeout = errors.New("in-flight writes did not finish before the drain deadline; the pause was aborted and nothing changed")

type PauseLease struct {
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	Owner     string          `json:"owner"`
	Mode      string          `json:"mode"`
	State     string          `json:"state"`
	StartedAt time.Time       `json:"started_at"`
	Deadline  time.Time       `json:"deadline"`
	Marker    json.RawMessage `json:"marker,omitempty"`
}

// Marker is what a job needs to reason about "the state at the pause": the outbox position, the log position and the time.
type Marker struct {
	OutboxID int64     `json:"outbox_id"`
	LSN      string    `json:"lsn"`
	At       time.Time `json:"at"`
	Mode     string    `json:"mode"`
	// JiraWatermarks is, per mirrored project in scope, how far the Jira sync had read when the marker was taken (PAUSE-11). Taken
	// after the drain, so no sync batch is half applied; the sync resumes from the stored watermark when the pause ends.
	JiraWatermarks map[string]time.Time `json:"jira_watermarks,omitempty"`
}

type gate struct {
	mu       sync.Mutex
	inflight map[string]int // per project key, plus "*" for all
	cache    []PauseLease
	cachedAt time.Time
}

func (s *Service) g() *gate {
	s.gateOnce.Do(func() { s.gate = &gate{inflight: map[string]int{}} })
	return s.gate
}

// PauseAdmitWait is how long a write in a paused scope waits for the pause to end before it is refused with 503.
var PauseAdmitWait = 2 * time.Second

const pauseCacheTTL = time.Second

func scopeOf(projectKey string) []string {
	return []string{"instance", "project:" + strings.ToUpper(projectKey)}
}

// leases returns the active leases (expired ones are removed), cached for a second so the hot path stays one map read.
func (s *Service) leases(ctx context.Context, force bool) []PauseLease {
	gt := s.g()
	gt.mu.Lock()
	if !force && time.Since(gt.cachedAt) < pauseCacheTTL {
		out := gt.cache
		gt.mu.Unlock()
		return out
	}
	gt.mu.Unlock()
	s.DB.Exec(ctx, `DELETE FROM pause_lease WHERE deadline<=$1`, s.Now()) // a crashed owner cannot hold the system
	rows, err := s.DB.Query(ctx, `SELECT id,scope,owner,mode,state,started_at,deadline,marker FROM pause_lease`)
	var out []PauseLease
	if err == nil {
		for rows.Next() {
			var l PauseLease
			rows.Scan(&l.ID, &l.Scope, &l.Owner, &l.Mode, &l.State, &l.StartedAt, &l.Deadline, &l.Marker)
			out = append(out, l)
		}
		rows.Close()
	}
	gt.mu.Lock()
	gt.cache, gt.cachedAt = out, time.Now()
	gt.mu.Unlock()
	return out
}

func (s *Service) blockedBy(ctx context.Context, projectKey string) *PauseLease {
	for _, l := range s.leases(ctx, false) {
		for _, sc := range scopeOf(projectKey) {
			if l.Scope == sc {
				return &l
			}
		}
	}
	return nil
}

// admit is called at the start of every write that a pause covers. It waits briefly for a pause to end, then refuses with
// ErrPaused. The returned function must be called when the write is done: it is what the drain waits for.
func (s *Service) admit(ctx context.Context, projectKey string) (func(), error) {
	if site := s.mirrorSite(ctx, projectKey); site != "" { // a mirrored project is read-only on every path
		return nil, &ReadOnlyMirror{Project: strings.ToUpper(projectKey), SiteURL: site}
	}
	return s.admitGate(ctx, projectKey)
}

// admitGate is the pause gate alone: the Jira sync writes mirrored projects with its own SQL but still honours a pause.
func (s *Service) admitGate(ctx context.Context, projectKey string) (func(), error) {
	projectKey = strings.ToUpper(projectKey)
	deadline := time.Now().Add(PauseAdmitWait)
	for {
		l := s.blockedBy(ctx, projectKey)
		if l == nil {
			break
		}
		if time.Now().After(deadline) {
			ra := l.Deadline.Sub(s.Now())
			if ra < time.Second {
				ra = time.Second
			}
			return nil, &ErrPaused{Scope: l.Scope, RetryAfter: ra, Owner: l.Owner}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
			s.leases(ctx, true)
		}
	}
	gt := s.g()
	gt.mu.Lock()
	gt.inflight[projectKey]++
	gt.inflight["*"]++
	gt.mu.Unlock()
	// a pause may have started between the check and the registration: back out and wait again
	if l := s.blockedBy(ctx, projectKey); l != nil && l.State == "paused" {
		gt.mu.Lock()
		gt.inflight[projectKey]--
		gt.inflight["*"]--
		gt.mu.Unlock()
		return s.admit(ctx, projectKey)
	}
	return func() {
		gt.mu.Lock()
		gt.inflight[projectKey]--
		gt.inflight["*"]--
		gt.mu.Unlock()
	}, nil
}

func (s *Service) inflightIn(scope string) int {
	gt := s.g()
	gt.mu.Lock()
	defer gt.mu.Unlock()
	if scope == "instance" {
		return gt.inflight["*"]
	}
	return gt.inflight[strings.ToUpper(strings.TrimPrefix(scope, "project:"))]
}

type PauseIn struct {
	Scope       string `json:"scope"` // "instance" or "project:KEY"
	Owner       string `json:"owner"`
	Mode        string `json:"mode"`          // pause (hold writes), snapshot (marker only, no hold), none
	DrainWaitMS int    `json:"drain_wait_ms"` // default 60000
	HoldMS      int    `json:"hold_ms"`       // default 600000
}

// RequestPause starts a pause. It returns once the scope is paused (the marker is the "state at the pause") or fails,
// leaving the scope running and writing an audit event.
func (s *Service) RequestPause(ctx context.Context, a Actor, in PauseIn) (*PauseLease, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	mode := in.Mode
	if mode == "" {
		mode = "pause"
	}
	if mode != "pause" && mode != "snapshot" && mode != "none" {
		return nil, invalid("mode must be pause, snapshot or none")
	}
	if in.Scope != "instance" && !strings.HasPrefix(in.Scope, "project:") {
		return nil, invalid("scope must be instance or project:KEY")
	}
	if strings.HasPrefix(in.Scope, "project:") {
		if _, err := s.projectID(ctx, strings.TrimPrefix(in.Scope, "project:")); err != nil {
			return nil, invalid("unknown project in scope")
		}
		in.Scope = "project:" + strings.ToUpper(strings.TrimPrefix(in.Scope, "project:"))
	}
	if in.Owner == "" {
		in.Owner = a.Label
	}
	mk, err := s.marker(ctx, mode, in.Scope)
	if err != nil {
		return nil, err
	}
	mb, _ := json.Marshal(mk)
	if mode != "pause" { // snapshot and none hold nothing: the job reads a consistent database snapshot at the marker
		return &PauseLease{Scope: in.Scope, Owner: in.Owner, Mode: mode, State: "none", StartedAt: s.Now(), Marker: mb}, nil
	}
	drain := time.Duration(in.DrainWaitMS) * time.Millisecond
	if drain <= 0 {
		drain = 60 * time.Second
	}
	hold := time.Duration(in.HoldMS) * time.Millisecond
	if hold <= 0 {
		hold = 10 * time.Minute
	}
	var id string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(727275)`); err != nil { // serialize requests, so two overlapping pauses cannot both be granted
			return err
		}
		tx.Exec(ctx, `DELETE FROM pause_lease WHERE deadline<=$1`, s.Now())
		rows, err := tx.Query(ctx, `SELECT scope,owner FROM pause_lease`)
		if err != nil {
			return err
		}
		var clash string
		for rows.Next() {
			var sc, ow string
			rows.Scan(&sc, &ow)
			if sc == in.Scope || sc == "instance" || in.Scope == "instance" {
				clash = sc + " (" + ow + ")"
			}
		}
		rows.Close()
		if clash != "" {
			return &ConflictError{"a pause already covers this scope: " + clash}
		}
		return tx.QueryRow(ctx, `INSERT INTO pause_lease(scope,owner,mode,state,started_at,deadline,marker) VALUES($1,$2,'pause','draining',$3,$4,$5) RETURNING id`,
			in.Scope, in.Owner, s.Now(), s.Now().Add(drain+hold), mb).Scan(&id)
	})
	if err != nil {
		return nil, err
	}
	s.leases(ctx, true)
	// drain: wait for the writes that were already running
	end := time.Now().Add(drain)
	abort := func(reason string) (*PauseLease, error) {
		s.DB.Exec(ctx, `DELETE FROM pause_lease WHERE id=$1`, id) // abort: the scope returns to running
		s.leases(ctx, true)
		s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,'pause.aborted',$3,$4)`, nilIfEmpty(a.ID), a.Label, in.Scope, jsonMarshal(map[string]any{"reason": reason}))
		return nil, ErrDrainTimeout
	}
	for s.inflightIn(in.Scope) > 0 {
		if time.Now().After(end) {
			return abort("drain deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// and the writes of the other nodes: each says when it has none left (once every live node can, see PauseBarrierG)
	if waiting := s.pauseBarrier(ctx, id, end); len(waiting) > 0 {
		return abort("drain deadline: still writing on " + strings.Join(waiting, ", "))
	}
	// the marker is taken after the drain, so everything acknowledged before it is in the database
	mk, err = s.marker(ctx, mode, in.Scope)
	if err != nil {
		return nil, err
	}
	mb, _ = json.Marshal(mk)
	s.DB.Exec(ctx, `UPDATE pause_lease SET state='paused', marker=$2 WHERE id=$1`, id, mb)
	s.leases(ctx, true)
	s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,'pause.started',$3,$4)`, nilIfEmpty(a.ID), a.Label, in.Scope, mb)
	return &PauseLease{ID: id, Scope: in.Scope, Owner: in.Owner, Mode: mode, State: "paused", StartedAt: s.Now(), Deadline: s.Now().Add(hold), Marker: mb}, nil
}

func (s *Service) marker(ctx context.Context, mode, scope string) (Marker, error) {
	m := Marker{At: s.Now(), Mode: mode}
	if err := s.DB.QueryRow(ctx, `SELECT COALESCE(max(id),0), pg_current_wal_lsn()::text FROM outbox`).Scan(&m.OutboxID, &m.LSN); err != nil {
		return m, err
	}
	rows, err := s.DB.Query(ctx, `SELECT p.key, jp.watermark FROM jira_project jp JOIN project p ON p.id=jp.project_id WHERE jp.watermark IS NOT NULL AND ($1='instance' OR $1='project:'||p.key)`, scope)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var w time.Time
		if err := rows.Scan(&k, &w); err != nil {
			return m, err
		}
		if m.JiraWatermarks == nil {
			m.JiraWatermarks = map[string]time.Time{}
		}
		m.JiraWatermarks[k] = w
	}
	return m, rows.Err()
}

// ReleasePause ends a pause; held writes continue and go through the normal compare-and-set.
func (s *Service) ReleasePause(ctx context.Context, a Actor, id string) error {
	if !a.Admin {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM pause_lease WHERE id=$1`, id)
	if err != nil {
		return invalid("bad pause id")
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.leases(ctx, true)
	s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,'pause.ended',$3,'{}')`, nilIfEmpty(a.ID), a.Label, id)
	return nil
}

// PauseStatus lists the active leases for the Admin status page.
func (s *Service) PauseStatus(ctx context.Context, a Actor) ([]PauseLease, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	l := s.leases(ctx, true)
	if l == nil {
		l = []PauseLease{}
	}
	return l, nil
}

// Maintenance is what any signed-in person may know about a pause: that one is under way, for which scope and until when.
type Maintenance struct {
	Scope string    `json:"scope"`
	Until time.Time `json:"until"`
}

// ActiveMaintenance lists the pauses that hold writes (a snapshot-mode job holds none and is not shown).
func (s *Service) ActiveMaintenance(ctx context.Context) []Maintenance {
	out := []Maintenance{}
	for _, l := range s.leases(ctx, true) {
		if l.Mode == "pause" {
			out = append(out, Maintenance{Scope: l.Scope, Until: l.Deadline})
		}
	}
	return out
}

// AckPauses is this node's half of the pause barrier: for every lease it has seen it says it has no write of its own left in the lease's scope.
// A write that arrives after the lease is cached is held, so an acknowledgement cannot be overtaken by new work on this node.
func (s *Service) AckPauses(ctx context.Context) {
	if s.NodeID == "" {
		return
	}
	for _, l := range s.leases(ctx, true) {
		if s.inflightIn(l.Scope) == 0 {
			s.DB.Exec(ctx, `INSERT INTO pause_ack(lease_id,node_id,acked_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, l.ID, s.NodeID, s.Now())
		}
	}
}

// RunPauseAcks keeps AckPauses going.
func (s *Service) RunPauseAcks(ctx context.Context) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.AckPauses(ctx)
		}
	}
}

// pauseBarrier waits until every other live node has acknowledged the lease, and returns the nodes that had not by the deadline. It waits only
// when the cluster qualifies for the barrier: while any live node is older than the release that acknowledges (a rolling upgrade is in progress)
// the gate is off and the other nodes are given the fixed moment they always had to see the lease.
func (s *Service) pauseBarrier(ctx context.Context, leaseID string, deadline time.Time) []string {
	if s.NodeID == "" || !s.GateOn(ctx, PauseBarrierG) {
		return nil
	}
	for {
		rows, err := s.DB.Query(ctx, `SELECT n.id FROM cluster_node n WHERE n.id<>$1 AND n.seen_at > $2 AND NOT EXISTS (SELECT 1 FROM pause_ack a WHERE a.lease_id::text=$3 AND a.node_id=n.id) ORDER BY n.id`,
			s.NodeID, s.Now().Add(-nodeLiveFor), leaseID)
		if err != nil {
			return nil
		}
		var waiting []string
		for rows.Next() {
			var id string
			rows.Scan(&id)
			waiting = append(waiting, id)
		}
		rows.Close()
		if len(waiting) == 0 || time.Now().After(deadline) {
			return waiting
		}
		select {
		case <-ctx.Done():
			return waiting
		case <-time.After(25 * time.Millisecond):
		}
	}
}
