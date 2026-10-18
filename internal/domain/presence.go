package domain

import (
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
func (p *Presence) Heartbeat(issue string, a Actor, field string, appearOffline bool) {
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
	}
	if field != r.field || r.since.IsZero() {
		r.since = now
	}
	r.field, r.seen, r.offline = field, now, appearOffline
}

func (p *Presence) Leave(issue, userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.m[issue], userID)
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
