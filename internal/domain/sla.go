package domain

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/calendar"
)

// Service level clocks (SPEC 15). A request type has a policy: a goal for the first response and one for the resolution, in minutes; the
// working calendar the minutes count in (none: every minute counts); and the statuses that stop the clocks (for example "Waiting for
// customer"). Each request raised through the type gets a clock per goal, with a snapshot of the policy, started when the request was made.
//
// A clock is a function of facts, never of events seen: the status history says when it was stopped, the first public reply by staff
// meets the first response goal, the issue being resolved meets the resolution goal. So it can be recomputed at any time and a missed
// event only delays a result until the next pass. State: running, paused, met, breached. A clock that met its goal late is met with
// breached_at set (the time the goal fell due), so it shows as late.

type SLACalendar struct {
	Preset   string   `json:"preset,omitempty"` // mon-fri (default), sun-thu, mon-sat
	TimeZone string   `json:"time_zone"`
	Start    string   `json:"start,omitempty"`         // HH:MM, default 09:00
	Hours    float64  `json:"hours_per_day,omitempty"` // default 8
	Holidays []string `json:"holidays,omitempty"`
}

type SLAPolicy struct {
	FirstResponse int          `json:"first_response_minutes,omitempty"`
	Resolution    int          `json:"resolution_minutes,omitempty"`
	Calendar      *SLACalendar `json:"calendar,omitempty"`
	PauseStatuses []string     `json:"pause_statuses,omitempty"`
}

func (p SLAPolicy) empty() bool { return p.FirstResponse == 0 && p.Resolution == 0 }

const maxSLAMinutes = 60 * 24 * 365

func (c SLACalendar) build() (calendar.Calendar, error) {
	preset := c.Preset
	if preset == "" {
		preset = "mon-fri"
	}
	cal, err := calendar.Preset(preset, c.TimeZone)
	if err != nil {
		return cal, invalid("%v", err)
	}
	if c.Hours != 0 {
		cal.Hours = c.Hours
	}
	cal.Start, cal.Holidays = c.Start, c.Holidays
	if err := cal.Validate(); err != nil {
		return cal, invalid("%v", err)
	}
	return cal.Normalize(), nil
}

// checkSLAPolicy validates a policy against the project's workflow.
func (s *Service) checkSLAPolicy(ctx context.Context, pid string, p SLAPolicy) (SLAPolicy, error) {
	if p.FirstResponse < 0 || p.Resolution < 0 || p.FirstResponse > maxSLAMinutes || p.Resolution > maxSLAMinutes {
		return p, invalid("a goal is between 1 minute and a year")
	}
	if p.FirstResponse > 0 && p.Resolution > 0 && p.FirstResponse > p.Resolution {
		return p, invalid("the first response goal cannot be longer than the resolution goal")
	}
	if p.Calendar != nil {
		if _, err := p.Calendar.build(); err != nil {
			return p, err
		}
	}
	var names []string
	for _, n := range p.PauseStatuses {
		var real string
		if err := s.DB.QueryRow(ctx, `SELECT st.name FROM project p JOIN status st ON st.workflow_id=p.workflow_id WHERE p.id=$1 AND lower(st.name)=lower($2) AND NOT st.retired`, pid, strings.TrimSpace(n)).Scan(&real); err != nil {
			return p, invalid("the workflow has no status %q to stop the clock in", n)
		}
		names = append(names, real)
	}
	p.PauseStatuses = names
	return p, nil
}

type SLAClock struct {
	Kind       string     `json:"kind"` // first_response, resolution
	Goal       int        `json:"goal_minutes"`
	State      string     `json:"state"`
	Elapsed    int        `json:"elapsed_minutes"`
	Remaining  int        `json:"remaining_minutes"` // negative once the goal is passed
	Due        *time.Time `json:"due,omitempty"`
	Met        *time.Time `json:"met,omitempty"`
	BreachedAt *time.Time `json:"breached_at,omitempty"`
	Late       bool       `json:"late,omitempty"` // met, but after the goal
}

// timeline: how the minutes are counted.
type slaCount struct {
	cal *calendar.Calendar
}

func (c slaCount) work(a, b time.Time) float64 {
	if !b.After(a) {
		return 0
	}
	if c.cal == nil {
		return b.Sub(a).Minutes()
	}
	return c.cal.WorkingMinutesBetween(a, b)
}

func (c slaCount) add(t time.Time, mins float64) time.Time {
	if c.cal == nil {
		return t.Add(time.Duration(mins * float64(time.Minute)))
	}
	return c.cal.AddWorkingMinutes(t, mins)
}

type pauseSpan struct{ from, to time.Time } // to zero: still stopped

// slaFacts is what a clock is computed from.
type slaFacts struct {
	start     time.Time
	met       *time.Time // when the goal was met (first public reply, or resolution)
	pauses    []pauseSpan
	goal      int
	count     slaCount
	nowOrZero time.Time
}

type slaResult struct {
	state      string
	elapsed    float64
	due        *time.Time
	met        *time.Time
	breachedAt *time.Time
}

// computeSLA is the whole logic of a clock, with no database in it.
func computeSLA(f slaFacts) slaResult {
	now := f.nowOrZero
	end := now
	if f.met != nil && f.met.Before(end) {
		end = *f.met
	}
	paused := 0.0
	var stopped bool
	for _, p := range f.pauses {
		a, b := p.from, p.to
		if b.IsZero() || b.After(end) {
			b = end
		}
		if a.Before(f.start) {
			a = f.start
		}
		paused += f.count.work(a, b)
		if p.to.IsZero() {
			stopped = true
		}
	}
	elapsed := f.count.work(f.start, end) - paused
	if elapsed < 0 {
		elapsed = 0
	}
	// when the goal falls due: consume the goal's minutes over the stretches in which the clock runs
	var due time.Time
	remaining := float64(f.goal)
	cursor := f.start
	known := true
	pauses := append([]pauseSpan(nil), f.pauses...)
	sort.Slice(pauses, func(i, j int) bool { return pauses[i].from.Before(pauses[j].from) })
	for _, p := range pauses {
		if p.from.After(cursor) {
			if avail := f.count.work(cursor, p.from); avail >= remaining {
				due = f.count.add(cursor, remaining)
				remaining = 0
				break
			} else {
				remaining -= avail
			}
		}
		if p.to.IsZero() {
			known = false
			break
		}
		if p.to.After(cursor) {
			cursor = p.to
		}
	}
	if remaining > 0 && known {
		due = f.count.add(cursor, remaining)
	}
	res := slaResult{elapsed: elapsed, met: f.met}
	if (known || !due.IsZero()) && !due.IsZero() {
		d := due
		res.due = &d
	}
	switch {
	case f.met != nil:
		res.state = "met"
		if res.due != nil && f.met.After(*res.due) {
			res.breachedAt = res.due
		}
	case stopped && (res.due == nil || now.Before(*res.due)):
		res.state = "paused"
		res.due = nil
	case res.due != nil && !now.Before(*res.due):
		res.state = "breached"
		res.breachedAt = res.due
	default:
		res.state = "running"
	}
	return res
}

// ---- gathering the facts ----

type clockRow struct {
	kind     string
	goal     int
	cal      *calendar.Calendar
	pauseSts []string
	started  time.Time
	notified *time.Time
}

func (s *Service) clockRows(ctx context.Context, q querier, issueID string) ([]clockRow, error) {
	rows, err := q.Query(ctx, `SELECT kind,goal_minutes,calendar,pause_statuses,started_at,notified_at FROM sla_clock WHERE issue_id::text=$1 ORDER BY kind`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []clockRow
	for rows.Next() {
		var r clockRow
		var raw []byte
		if err := rows.Scan(&r.kind, &r.goal, &raw, &r.pauseSts, &r.started, &r.notified); err != nil {
			return nil, err
		}
		if len(raw) > 0 && string(raw) != "null" {
			var c calendar.Calendar
			if json.Unmarshal(raw, &c) == nil {
				r.cal = &c
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func inList(list []string, name string) bool {
	for _, x := range list {
		if strings.EqualFold(x, name) {
			return true
		}
	}
	return false
}

// slaFactsFor reads the history of the issue.
func (s *Service) slaFactsFor(ctx context.Context, issueID string, r clockRow, now time.Time) (slaFacts, error) {
	f := slaFacts{start: r.started, goal: r.goal, count: slaCount{cal: r.cal}, nowOrZero: now}
	var current, category string
	var resolved *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT st.name,st.category,i.resolved_at FROM issue i JOIN status st ON st.id=i.status_id WHERE i.id::text=$1`, issueID).Scan(&current, &category, &resolved); err != nil {
		return f, err
	}
	type step struct {
		at          time.Time
		before, aft string
	}
	rows, err := s.DB.Query(ctx, `SELECT at,COALESCE(before,''),COALESCE(after,'') FROM issue_history WHERE issue_id::text=$1 AND field='status' AND at >= $2 ORDER BY at,id`, issueID, r.started)
	if err != nil {
		return f, err
	}
	var steps []step
	for rows.Next() {
		var st step
		rows.Scan(&st.at, &st.before, &st.aft)
		steps = append(steps, st)
	}
	rows.Close()
	// a status stops the clock when the policy says so; the resolution clock also stops while the request is resolved (a status in the
	// Done category), so a request reopened later is not charged for the time it sat resolved
	cats := map[string]string{}
	cr, err := s.DB.Query(ctx, `SELECT lower(st.name),st.category FROM issue i JOIN project p ON p.id=i.project_id JOIN status st ON st.workflow_id=p.workflow_id WHERE i.id::text=$1`, issueID)
	if err != nil {
		return f, err
	}
	for cr.Next() {
		var n, c string
		cr.Scan(&n, &c)
		cats[n] = c
	}
	cr.Close()
	stops := func(name string) bool {
		return inList(r.pauseSts, name) || (r.kind == "resolution" && cats[strings.ToLower(name)] == "done")
	}
	// the status the request began in: what the first change moved away from, or the status it still has
	first := current
	if len(steps) > 0 {
		first = steps[0].before
	}
	paused, since := stops(first), r.started
	for _, st := range steps {
		switch now := stops(st.aft); {
		case now && !paused:
			paused, since = true, st.at
		case !now && paused:
			f.pauses = append(f.pauses, pauseSpan{from: since, to: st.at})
			paused = false
		}
	}
	if paused {
		f.pauses = append(f.pauses, pauseSpan{from: since})
	}
	switch r.kind {
	case "first_response":
		var t *time.Time
		s.DB.QueryRow(ctx, `SELECT min(c.created_at) FROM comment c JOIN app_user u ON u.id=c.author_id WHERE c.issue_id::text=$1 AND NOT c.internal AND u.kind<>'portal' AND c.created_at >= $2`, issueID, r.started).Scan(&t)
		if category == "done" && resolved != nil && (t == nil || resolved.Before(*t)) {
			t = resolved // resolving a request answers it
		}
		f.met = t
	case "resolution":
		if category == "done" && resolved != nil {
			f.met = resolved
		}
	}
	return f, nil
}

// RefreshSLA recomputes an issue's clocks, stores the result, and sends the one notice when a goal is breached.
func (s *Service) RefreshSLA(ctx context.Context, issueKey string) error {
	var id string
	if err := s.DB.QueryRow(ctx, `SELECT id::text FROM issue WHERE key=$1`, strings.ToUpper(issueKey)).Scan(&id); err != nil {
		return ErrNotFound
	}
	return s.refreshSLAByID(ctx, id)
}

func (s *Service) refreshSLAByID(ctx context.Context, id string) error {
	rows, err := s.clockRows(ctx, s.DB, id)
	if err != nil || len(rows) == 0 {
		return err
	}
	now := s.Now()
	for _, r := range rows {
		f, err := s.slaFactsFor(ctx, id, r, now)
		if err != nil {
			return err
		}
		res := computeSLA(f)
		err = s.tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE sla_clock SET state=$3,elapsed_min=$4,due_at=$5,met_at=$6,breached_at=$7,updated_at=$8 WHERE issue_id::text=$1 AND kind=$2`,
				id, r.kind, res.state, res.elapsed, res.due, res.met, res.breachedAt, now); err != nil {
				return err
			}
			if res.state != "breached" || r.notified != nil { // one notice, for a goal that is missed and still open
				return nil
			}
			tag, err := tx.Exec(ctx, `UPDATE sla_clock SET notified_at=$3 WHERE issue_id::text=$1 AND kind=$2 AND notified_at IS NULL`, id, r.kind, now)
			if err != nil || tag.RowsAffected() == 0 {
				return err
			}
			return s.slaBreachNotice(ctx, tx, id, r.kind)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// slaBreachNotice tells the assignee (or, with none, the project's administrators) that a goal was missed.
func (s *Service) slaBreachNotice(ctx context.Context, tx pgx.Tx, id, kind string) error {
	var key, pid string
	var assignee *string
	if err := tx.QueryRow(ctx, `SELECT key,project_id::text,assignee_id::text FROM issue WHERE id::text=$1`, id).Scan(&key, &pid, &assignee); err != nil {
		return err
	}
	users := []string{}
	if assignee != nil {
		users = append(users, *assignee)
	} else {
		rows, err := tx.Query(ctx, `SELECT user_id::text FROM effective_member WHERE project_id::text=$1 AND role='admin'`, pid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u string
			rows.Scan(&u)
			users = append(users, u)
		}
		rows.Close()
	}
	what := map[string]string{"first_response": "first response", "resolution": "resolution"}[kind]
	return outbox(ctx, tx, "issue:"+key, "sla.breached", map[string]any{"key": key, "actor": "Tasklane", "actor_id": "", "users": users, "text": "The " + what + " goal was missed"})
}

// slaTouch refreshes the clocks of an issue after a change that can move them; failures are left to the next sweep.
func (s *Service) slaTouch(ctx context.Context, issueID string) {
	var n int
	if s.DB.QueryRow(ctx, `SELECT count(*) FROM sla_clock WHERE issue_id::text=$1`, issueID).Scan(&n); n > 0 {
		s.refreshSLAByID(ctx, issueID)
	}
}

// SLASweep recomputes every clock that is still open (so breaches are found and noticed without anyone touching the issue) and returns how
// many issues it looked at.
func (s *Service) SLASweep(ctx context.Context) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT c.issue_id::text, p.key FROM sla_clock c JOIN issue i ON i.id=c.issue_id JOIN project p ON p.id=i.project_id WHERE c.state IN ('running','paused','breached')`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id, pkey string
		rows.Scan(&id, &pkey)
		if s.blockedBy(ctx, pkey) != nil {
			continue // a pause covers the project: the sweep stands still and the next one catches up
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.refreshSLAByID(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// RunSLA sweeps every minute until the context ends.
func (s *Service) RunSLA(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if _, err := s.SLASweep(ctx); err != nil && ctx.Err() == nil {
			logf("service level clocks: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// startClocks gives a new request its clocks under the request type's policy.
func (s *Service) startClocks(ctx context.Context, issueID string, p SLAPolicy, at time.Time) error {
	var cal []byte
	if p.Calendar != nil {
		c, err := p.Calendar.build()
		if err != nil {
			return err
		}
		cal, _ = json.Marshal(c)
	}
	for _, g := range []struct {
		kind string
		min  int
	}{{"first_response", p.FirstResponse}, {"resolution", p.Resolution}} {
		if g.min <= 0 {
			continue
		}
		pause := p.PauseStatuses
		if pause == nil {
			pause = []string{}
		}
		if _, err := s.DB.Exec(ctx, `INSERT INTO sla_clock(issue_id,kind,goal_minutes,calendar,pause_statuses,started_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, issueID, g.kind, g.min, cal, pause, at); err != nil {
			return err
		}
	}
	return s.refreshSLAByID(ctx, issueID)
}

// issueClocks are what staff see on a request: the stored state, with the minutes left worked out for now.
func (s *Service) issueClocks(ctx context.Context, issueID string) []SLAClock {
	rows, err := s.DB.Query(ctx, `SELECT kind,goal_minutes,state,elapsed_min,due_at,met_at,breached_at FROM sla_clock WHERE issue_id::text=$1 ORDER BY kind DESC`, issueID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []SLAClock
	for rows.Next() {
		var c SLAClock
		var el float64
		rows.Scan(&c.Kind, &c.Goal, &c.State, &el, &c.Due, &c.Met, &c.BreachedAt)
		c.Elapsed = int(el)
		c.Remaining = c.Goal - int(el)
		c.Late = c.State == "met" && c.BreachedAt != nil
		out = append(out, c)
	}
	return out
}
