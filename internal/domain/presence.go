package domain

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Presence is ephemeral and advisory (SPEC 4.5): never stored, refreshed every 10 s by clients,
// expired after 30 s, and never blocks anything.
type Presence struct {
	mu  sync.Mutex
	now func() time.Time
	TTL time.Duration
	m   map[string]map[string]*presenceRec // issue key -> user id -> record
}

type presenceRec struct {
	name    string
	field   string    // text field being edited, "" = viewing
	since   time.Time // when the current editing started
	seen    time.Time
	offline bool // "appear offline in editors"
}

type PresenceEntry struct {
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Editing  string `json:"editing,omitempty"`
	EditingS int    `json:"editing_seconds,omitempty"`
}

func NewPresence(now func() time.Time) *Presence {
	return &Presence{now: now, TTL: 30 * time.Second, m: map[string]map[string]*presenceRec{}}
}

// Heartbeat records that the user is viewing the issue, optionally editing a field.
func (p *Presence) Heartbeat(issue string, a Actor, field string, appearOffline bool) (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	us := p.m[issue]
	if us == nil {
		us = map[string]*presenceRec{}
		p.m[issue] = us
	}
	r := us[a.ID]
	now := p.now()
	if r == nil {
		r = &presenceRec{name: a.Label}
		us[a.ID] = r
		changed = true
	}
	if field != r.field || r.since.IsZero() {
		r.since = now
		changed = true
	}
	if r.offline != appearOffline {
		changed = true
	}
	r.field, r.seen, r.offline = field, now, appearOffline
	return changed
}

func (p *Presence) Leave(issue, userID string) (changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.m[issue][userID]; ok {
		delete(p.m[issue], userID)
		return true
	}
	return false
}

// Sweep drops entries that missed their heartbeat and returns the issues whose visible set changed.
func (p *Presence) Sweep() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var changed []string
	for issue, us := range p.m {
		drop := false
		for id, r := range us {
			if now.Sub(r.seen) > p.TTL {
				delete(us, id)
				drop = true
			}
		}
		if drop {
			changed = append(changed, issue)
		}
		if len(us) == 0 {
			delete(p.m, issue)
		}
	}
	return changed
}

// List returns who else is on the issue; the caller's own entry is excluded and
// users who chose to appear offline in editors are not shown as editing.
func (p *Presence) List(issue, exclude string) []PresenceEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := []PresenceEntry{}
	for id, r := range p.m[issue] {
		if now.Sub(r.seen) > p.TTL {
			delete(p.m[issue], id)
			continue
		}
		if id == exclude {
			continue
		}
		e := PresenceEntry{UserID: id, Name: r.name}
		if !r.offline {
			e.Editing = r.field
			if r.field != "" {
				e.EditingS = int(now.Sub(r.since).Seconds())
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Heartbeat is the service-level call: it checks read access, honours the user's
// "appear offline in editors" setting, records presence and returns who else is here.
func (s *Service) Heartbeat(ctx context.Context, a Actor, key, field string) ([]PresenceEntry, error) {
	l, _, err := s.HeartbeatChanged(ctx, a, key, field)
	return l, err
}

// HeartbeatChanged also reports whether the visible presence state changed, so the realtime gateway broadcasts only on change.
func (s *Service) HeartbeatChanged(ctx context.Context, a Actor, key, field string) ([]PresenceEntry, bool, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return nil, false, err
	}
	if field != "" && (!canWrite(role) || !textFields[field]) {
		field = "" // viewers and unknown fields are shown as viewing, never as editing
	}
	var offline bool
	s.DB.QueryRow(ctx, `SELECT appear_offline FROM app_user WHERE id=$1`, a.ID).Scan(&offline)
	changed := s.Presence.Heartbeat(is.Key, a, field, offline)
	s.busSend(busMsg{Kind: "presence", Presence: &presenceBusIn{Issue: is.Key, UserID: a.ID, Name: a.Label, Field: field, Offline: offline}})
	return s.Presence.List(is.Key, a.ID), changed, nil
}
