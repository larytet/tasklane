package domain

import (
	"encoding/json"
	"time"
)

// The cluster event bus (SPEC 17.8): with more than one node, what a change tells the people watching it (live events) and who is looking at an
// issue (presence) must reach the pages connected to the other nodes. A bus such as Redis carries both. They are hints and never the record:
// a save is committed in PostgreSQL before anything is published, a publish never waits and never fails a save, and a node that cannot reach the
// bus carries on alone: its own pages still see its own changes and presence, the other nodes' pages catch up by polling within seconds
// (as they do without a bus), and nothing needs repairing when the bus returns.

// EventBus is the part of a bus the service uses.
type EventBus interface {
	Publish(channel string, payload []byte)
	Subscribe(channel string, f func([]byte))
	BusStatus() BusStatus
}

// BusStatus is what a node knows about its link to the bus.
type BusStatus struct {
	Connected bool      `json:"connected"`
	Since     time.Time `json:"since"`
	LastError string    `json:"last_error,omitempty"`
	Dropped   int64     `json:"dropped"`
	Received  int64     `json:"received"`
	Published int64     `json:"published"`
}

const busChannel = "tasklane:cluster"

type busMsg struct {
	Node     string         `json:"node"`
	Kind     string         `json:"kind"` // event, presence, forget
	Event    *Event         `json:"event,omitempty"`
	Presence *presenceBusIn `json:"presence,omitempty"`
	Forget   *forgetBusIn   `json:"forget,omitempty"`
}

// forgetBusIn tells the other nodes that something they remember changed: an organization's security policy, a person's second factor.
// Kinds a node does not know are ignored, so an older release beside this one keeps its own cache time-out as the bound.
type forgetBusIn struct {
	Org  string `json:"org,omitempty"`
	User string `json:"user,omitempty"`
}

type presenceBusIn struct {
	Issue   string `json:"issue"`
	UserID  string `json:"user_id"`
	Name    string `json:"name"`
	Field   string `json:"field,omitempty"`
	Offline bool   `json:"offline,omitempty"`
	Leave   bool   `json:"leave,omitempty"`
}

// AttachBus connects the service to a bus. Call it once, before Run.
func (s *Service) AttachBus(b EventBus) {
	s.Bus = b
	if s.busID == "" {
		s.busID = s.NodeID
		if s.busID == "" {
			s.busID = randURLToken(8)
		}
	}
	b.Subscribe(busChannel, s.fromBus)
}

// BusStatus reports the link, and whether there is one.
func (s *Service) BusLink() (BusStatus, bool) {
	if s.Bus == nil {
		return BusStatus{}, false
	}
	return s.Bus.BusStatus(), true
}

func (s *Service) busSend(m busMsg) {
	if s.Bus == nil {
		return
	}
	m.Node = s.busID
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	s.Bus.Publish(busChannel, b)
}

// fromBus applies what another node published; what this node published itself is ignored.
func (s *Service) fromBus(payload []byte) {
	var m busMsg
	if json.Unmarshal(payload, &m) != nil || m.Node == s.busID {
		return
	}
	switch {
	case m.Kind == "forget" && m.Forget != nil:
		if m.Forget.Org != "" {
			s.forgetPolicyHere(m.Forget.Org)
		}
		if m.Forget.User != "" {
			s.forgetFactorHere(m.Forget.User)
		}
	case m.Kind == "event" && m.Event != nil:
		s.emitLocal(*m.Event)
	case m.Kind == "presence" && m.Presence != nil && s.Presence != nil:
		p := m.Presence
		var changed bool
		if p.Leave {
			changed = s.Presence.Leave(p.Issue, p.UserID)
		} else {
			changed = s.Presence.Heartbeat(p.Issue, Actor{ID: p.UserID, Label: p.Name}, p.Field, p.Offline)
		}
		if changed && s.OnRemotePresence != nil {
			s.OnRemotePresence(p.Issue)
		}
	}
}

// LeavePresence records that a person left an issue, here and on the other nodes.
func (s *Service) LeavePresence(key, userID string) bool {
	changed := s.Presence.Leave(key, userID)
	s.busSend(busMsg{Kind: "presence", Presence: &presenceBusIn{Issue: key, UserID: userID, Leave: true}})
	return changed
}
