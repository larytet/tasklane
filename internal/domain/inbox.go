package domain

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Watching, the in-app inbox, preferences and notification schemes (SPEC 13). The planner in notify.go decides who hears
// about an event; this file holds what people can read and set.

// NotifyPrefs are one person's choices. Defaults: watch what you touch, inbox and email on, nothing muted.
type NotifyPrefs struct {
	AutoWatch  bool     `json:"auto_watch"`  // hear about issues you reported, are assigned or commented on
	Inbox      bool     `json:"inbox"`       // in-app notifications
	Email      bool     `json:"email"`       // notification email (when the server sends any)
	Muted      []string `json:"muted"`       // kinds you do not want: assigned, mention, comment, status, edit, created
	QuietStart string   `json:"quiet_start"` // email waits until quiet hours end, such as 22:00 to 07:00
	QuietEnd   string   `json:"quiet_end"`
	Timezone   string   `json:"timezone"` // an IANA name; quiet hours use it (default UTC)
	// EmailBouncing is read only: mail to your address bounced or drew a complaint and is being held back (the inbox is not affected).
	EmailBouncing bool `json:"email_bouncing,omitempty"`
}

var notifyKinds = map[string]bool{"assigned": true, "mention": true, "comment": true, "status": true, "edit": true, "created": true}

func defaultPrefs() NotifyPrefs {
	return NotifyPrefs{AutoWatch: true, Inbox: true, Email: true, Muted: []string{}}
}

func (p NotifyPrefs) mutes(kind string) bool {
	for _, m := range p.Muted {
		if m == kind {
			return true
		}
	}
	return false
}

func (s *Service) prefsOf(ctx context.Context, q querier, userID string) NotifyPrefs {
	p := defaultPrefs()
	var raw []byte
	if q.QueryRow(ctx, `SELECT prefs FROM notification_pref WHERE user_id=$1`, userID).Scan(&raw) == nil {
		json.Unmarshal(raw, &p) // missing keys keep their defaults
	}
	if p.Muted == nil {
		p.Muted = []string{}
	}
	return p
}

func parseClock(v string) (int, bool) {
	t, err := time.Parse("15:04", v)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// quietUntil reports when quiet hours end if now falls inside them.
func (p NotifyPrefs) quietUntil(now time.Time) (time.Time, bool) {
	st, ok1 := parseClock(p.QuietStart)
	en, ok2 := parseClock(p.QuietEnd)
	if !ok1 || !ok2 || st == en {
		return time.Time{}, false
	}
	loc := time.UTC
	if p.Timezone != "" {
		if l, err := time.LoadLocation(p.Timezone); err == nil {
			loc = l
		}
	}
	l := now.In(loc)
	m := l.Hour()*60 + l.Minute()
	in := (st < en && m >= st && m < en) || (st > en && (m >= st || m < en))
	if !in {
		return time.Time{}, false
	}
	end := time.Date(l.Year(), l.Month(), l.Day(), en/60, en%60, 0, 0, loc)
	if !end.After(l) {
		end = end.AddDate(0, 0, 1)
	}
	return end, true
}

func (s *Service) MyNotifyPrefs(ctx context.Context, a Actor) NotifyPrefs {
	p := s.prefsOf(ctx, s.DB, a.ID)
	p.EmailBouncing = s.EmailBouncing(ctx, a.ID)
	return p
}

func (s *Service) SetNotifyPrefs(ctx context.Context, a Actor, p NotifyPrefs) (NotifyPrefs, error) {
	for _, m := range p.Muted {
		if !notifyKinds[m] {
			return p, invalid("unknown notification kind %q (assigned, mention, comment, status, edit, created)", m)
		}
	}
	if (p.QuietStart != "") != (p.QuietEnd != "") {
		return p, invalid("quiet hours need a start and an end")
	}
	if p.QuietStart != "" {
		if _, ok := parseClock(p.QuietStart); !ok {
			return p, invalid("quiet hours start as HH:MM, such as 22:00")
		}
		if _, ok := parseClock(p.QuietEnd); !ok {
			return p, invalid("quiet hours end as HH:MM, such as 07:00")
		}
	}
	if p.Timezone != "" {
		if _, err := time.LoadLocation(p.Timezone); err != nil {
			return p, invalid("unknown time zone %q", p.Timezone)
		}
	}
	if p.Muted == nil {
		p.Muted = []string{}
	}
	raw, _ := json.Marshal(p)
	_, err := s.DB.Exec(ctx, `INSERT INTO notification_pref(user_id,prefs) VALUES($1,$2) ON CONFLICT (user_id) DO UPDATE SET prefs=EXCLUDED.prefs`, a.ID, raw)
	return p, err
}

// ----- watching

type WatchState struct {
	Watching bool `json:"watching"` // you will hear about this issue (explicitly, or because you reported, were assigned or commented)
	Explicit bool `json:"explicit"` // you chose to watch it
	Muted    bool `json:"muted"`    // you chose not to hear about it
	Watchers int  `json:"watchers"`
}

// WatchState reports whether the person hears about an issue and how many people explicitly watch it.
func (s *Service) WatchState(ctx context.Context, a Actor, key string) (WatchState, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return WatchState{}, err
	}
	var w WatchState
	var muted *bool
	s.DB.QueryRow(ctx, `SELECT muted FROM issue_watch WHERE issue_id=$1 AND user_id=$2`, is.ID, a.ID).Scan(&muted)
	if muted != nil {
		w.Explicit, w.Muted = !*muted, *muted
	}
	s.DB.QueryRow(ctx, `SELECT count(*) FROM issue_watch WHERE issue_id=$1 AND NOT muted`, is.ID).Scan(&w.Watchers)
	implicit := false
	if is.Reporter != nil && is.Reporter.ID == a.ID || is.Assignee != nil && is.Assignee.ID == a.ID {
		implicit = true
	}
	if !implicit {
		s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM comment WHERE issue_id=$1 AND author_id=$2)`, is.ID, a.ID).Scan(&implicit)
	}
	w.Watching = w.Explicit || (implicit && !w.Muted && s.prefsOf(ctx, s.DB, a.ID).AutoWatch)
	return w, nil
}

// SetWatch records the person's choice: watch (true), or do not hear about this issue at all (false).
func (s *Service) SetWatch(ctx context.Context, a Actor, key string, watch bool) (WatchState, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return WatchState{}, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO issue_watch(issue_id,user_id,muted) VALUES($1,$2,$3) ON CONFLICT (issue_id,user_id) DO UPDATE SET muted=EXCLUDED.muted`, is.ID, a.ID, !watch); err != nil {
		return WatchState{}, err
	}
	return s.WatchState(ctx, a, key)
}

// ClearWatch forgets the explicit choice: the person is back to the default (hear about what they touch).
func (s *Service) ClearWatch(ctx context.Context, a Actor, key string) (WatchState, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return WatchState{}, err
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM issue_watch WHERE issue_id=$1 AND user_id=$2`, is.ID, a.ID); err != nil {
		return WatchState{}, err
	}
	return s.WatchState(ctx, a, key)
}

// ----- the inbox

type Notification struct {
	ID      int64      `json:"id"`
	Issue   string     `json:"issue"`
	Summary string     `json:"summary"`
	Kind    string     `json:"kind"`
	What    string     `json:"what"`
	Body    string     `json:"body,omitempty"`
	Actor   string     `json:"actor"`
	State   string     `json:"state"`
	At      time.Time  `json:"at"`
	ReadAt  *time.Time `json:"read_at,omitempty"`
}

type InboxQuery struct {
	State  string // unread, read, archived; empty: unread and read
	Limit  int
	Before int64
}

// visibleSQL is the permission check applied every time notifications are read: a notification names an issue, so it is
// shown only while the person can still read that issue's project, and an internal comment only while they may see those.
func (s *Service) inboxScope(ctx context.Context, a Actor) (all bool, readable, writable []string, err error) {
	all, readable, writable, err = s.SearchScope(ctx, a)
	return
}

func (s *Service) Inbox(ctx context.Context, a Actor, q InboxQuery) ([]Notification, error) {
	all, readable, writable, err := s.inboxScope(ctx, a)
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	states := []string{"unread", "read"}
	switch q.State {
	case "unread", "read", "archived":
		states = []string{q.State}
	}
	rows, err := s.DB.Query(ctx, `SELECT n.id,i.key,i.summary,n.kind,n.what,n.body,n.actor_label,n.state,n.created_at,n.read_at
		FROM notification n JOIN issue i ON i.id=n.issue_id
		WHERE n.user_id=$1 AND n.state = ANY($2) AND i.deleted_at IS NULL AND ($3 OR i.project_id = ANY($4::uuid[])) AND (NOT n.internal OR $3 OR i.project_id = ANY($5::uuid[]))
		  AND ($6 = 0 OR n.id < $6) ORDER BY n.id DESC LIMIT $7`, a.ID, states, all, readable, writable, q.Before, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Issue, &n.Summary, &n.Kind, &n.What, &n.Body, &n.Actor, &n.State, &n.At, &n.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// UnreadCount counts what Inbox would show as unread.
func (s *Service) UnreadCount(ctx context.Context, a Actor) (int, error) {
	all, readable, writable, err := s.inboxScope(ctx, a)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.DB.QueryRow(ctx, `SELECT count(*) FROM notification n JOIN issue i ON i.id=n.issue_id
		WHERE n.user_id=$1 AND n.state='unread' AND i.deleted_at IS NULL AND ($2 OR i.project_id = ANY($3::uuid[])) AND (NOT n.internal OR $2 OR i.project_id = ANY($4::uuid[]))`, a.ID, all, readable, writable).Scan(&n)
	return n, err
}

// MarkNotification sets one notification's state (read, unread, archived). Only its owner can: anything else is not found.
func (s *Service) MarkNotification(ctx context.Context, a Actor, id int64, state string) error {
	if state != "read" && state != "unread" && state != "archived" {
		return invalid("a notification is read, unread or archived")
	}
	var readAt any
	if state == "read" || state == "archived" {
		readAt = s.Now()
	}
	tag, err := s.DB.Exec(ctx, `UPDATE notification SET state=$3,read_at=CASE WHEN $3='unread' THEN NULL ELSE COALESCE(read_at,$4::timestamptz) END WHERE id=$1 AND user_id=$2`, id, a.ID, state, readAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead marks the person's unread notifications read and returns how many changed.
func (s *Service) MarkAllRead(ctx context.Context, a Actor) (int, error) {
	tag, err := s.DB.Exec(ctx, `UPDATE notification SET state='read',read_at=$2 WHERE user_id=$1 AND state='unread'`, a.ID, s.Now())
	return int(tag.RowsAffected()), err
}

// ----- notification schemes

var schemeEvents = map[string]bool{"issue.created": true, "issue.assigned": true, "issue.transitioned": true, "comment.added": true, "issue.text_saved": true}
var schemeClasses = map[string]bool{"assignee": true, "reporter": true, "commenters": true, "watchers": true}

var defaultScheme = map[string][]string{
	"issue.created":      {"assignee"},
	"issue.assigned":     {"assignee"},
	"issue.transitioned": {"reporter", "assignee", "watchers"},
	"comment.added":      {"reporter", "assignee", "commenters", "watchers"},
	"issue.text_saved":   {"reporter", "assignee", "watchers"},
}

// NotificationScheme returns who each event notifies in a project (the built-in default where the project has no override).
func (s *Service) NotificationScheme(ctx context.Context, a Actor, projectKey string) (map[string][]string, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	if s.role(ctx, a, pid) == "" && !a.Admin {
		return nil, ErrNotFound
	}
	out := map[string][]string{}
	for k, v := range defaultScheme {
		out[k] = v
	}
	rows, err := s.DB.Query(ctx, `SELECT event,recipients FROM notification_scheme WHERE project_id::text=$1`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ev string
		var rs []string
		rows.Scan(&ev, &rs)
		out[ev] = rs
	}
	return out, rows.Err()
}

// SetNotificationScheme overrides who the given events notify in a project (administrators). An event left out keeps
// its current setting; an event set to the empty list notifies nobody by this scheme (mentions still reach the people mentioned).
func (s *Service) SetNotificationScheme(ctx context.Context, a Actor, projectKey string, scheme map[string][]string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	for ev, cl := range scheme {
		if !schemeEvents[ev] {
			return invalid("unknown event %q (issue.created, issue.assigned, issue.transitioned, comment.added, issue.text_saved)", ev)
		}
		for _, c := range cl {
			if !schemeClasses[c] {
				return invalid("unknown recipient kind %q (assignee, reporter, commenters, watchers)", c)
			}
		}
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		for ev, cl := range scheme {
			if cl == nil {
				cl = []string{}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO notification_scheme(project_id,event,recipients) VALUES($1::uuid,$2,$3) ON CONFLICT (project_id,event) DO UPDATE SET recipients=EXCLUDED.recipients`, pid, ev, cl); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, a, "notification_scheme.save", strings.ToUpper(projectKey), scheme)
	})
}

// audience resolves the recipient kinds of an event to people. Direct events (an assignment, a mention) always reach
// the person; the others reach reporters, assignees and commenters only while they watch by default, and nobody who
// muted the issue.
func (s *Service) audience(ctx context.Context, tx pgx.Tx, issueID, reporter string, assignee *string, classes []string, direct bool) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	explicit := map[string]bool{}
	muted := map[string]bool{}
	wr, err := tx.Query(ctx, `SELECT user_id::text,muted FROM issue_watch WHERE issue_id=$1`, issueID)
	if err != nil {
		return nil, err
	}
	for wr.Next() {
		var u string
		var m bool
		wr.Scan(&u, &m)
		if m {
			muted[u] = true
		} else {
			explicit[u] = true
		}
	}
	wr.Close()
	for _, c := range classes {
		switch c {
		case "assignee":
			if assignee != nil {
				add(*assignee)
			}
		case "reporter":
			add(reporter)
		case "commenters":
			rows, err := tx.Query(ctx, `SELECT DISTINCT author_id::text FROM comment WHERE issue_id=$1`, issueID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var u string
				rows.Scan(&u)
				add(u)
			}
			rows.Close()
		case "watchers":
			for u := range explicit {
				add(u)
			}
		}
	}
	if direct {
		return ids, nil
	}
	out := ids[:0]
	for _, u := range ids {
		if muted[u] {
			continue
		}
		if !explicit[u] && !s.prefsOf(ctx, tx, u).AutoWatch {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}
