package domain

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/calendar"
)

// Reports (SPEC 7.3) are computed by replaying the issue history, so they reflect what was true at each moment, not
// only today's values. Sprint reports use the sprint's own calendar snapshot, so a closed sprint never changes.

type estPoint struct {
	at  time.Time
	val *float64
}
type statPoint struct {
	at   time.Time
	name string
}

// timeline is one issue's estimate and status over time.
type timeline struct {
	id, key, summary string
	created          time.Time
	resolved         *time.Time
	assignee         string
	epic             string
	est0             *float64 // before the first recorded change
	estimates        []estPoint
	status0          string
	statuses         []statPoint
	curStatus        string
	curEst           *float64
}

func (t *timeline) estAt(at time.Time) *float64 {
	v := t.est0
	for _, p := range t.estimates {
		if p.at.After(at) {
			break
		}
		v = p.val
	}
	return v
}

func (t *timeline) statusAt(at time.Time) string {
	if at.Before(t.created) {
		return ""
	}
	v := t.status0
	for _, p := range t.statuses {
		if p.at.After(at) {
			break
		}
		v = p.name
	}
	return v
}

func parseEst(s *string) *float64 {
	if s == nil || *s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		return nil
	}
	return &f
}

// loadTimelines replays the history of the given issues (by id).
func (s *Service) loadTimelines(ctx context.Context, ids []string) (map[string]*timeline, error) {
	out := map[string]*timeline{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT i.id::text,i.key,i.summary,i.created_at,i.resolved_at,COALESCE(a.name,''),COALESCE(par.key,''),st.name,i.estimate
		FROM issue i JOIN status st ON st.id=i.status_id LEFT JOIN app_user a ON a.id=i.assignee_id LEFT JOIN issue par ON par.id=i.parent_id WHERE i.id::text = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &timeline{}
		if err := rows.Scan(&t.id, &t.key, &t.summary, &t.created, &t.resolved, &t.assignee, &t.epic, &t.curStatus, &t.curEst); err != nil {
			rows.Close()
			return nil, err
		}
		t.status0, t.est0 = t.curStatus, t.curEst
		out[t.id] = t
	}
	rows.Close()
	hr, err := s.DB.Query(ctx, `SELECT issue_id::text,field,before,after,at FROM issue_history WHERE issue_id::text = ANY($1) AND field IN ('status','estimate') ORDER BY issue_id,id`, ids)
	if err != nil {
		return nil, err
	}
	defer hr.Close()
	seenS, seenE := map[string]bool{}, map[string]bool{}
	for hr.Next() {
		var id, field string
		var before, after *string
		var at time.Time
		hr.Scan(&id, &field, &before, &after, &at)
		t := out[id]
		if t == nil {
			continue
		}
		switch field {
		case "status":
			if !seenS[id] {
				seenS[id] = true
				if before != nil {
					t.status0 = *before
				}
			}
			if after != nil {
				t.statuses = append(t.statuses, statPoint{at, *after})
			}
		case "estimate":
			if !seenE[id] {
				seenE[id] = true
				t.est0 = parseEst(before)
			}
			t.estimates = append(t.estimates, estPoint{at, parseEst(after)})
		}
	}
	return out, hr.Err()
}

func (s *Service) categories(ctx context.Context, projectID string) map[string]string {
	m := map[string]string{}
	rows, err := s.DB.Query(ctx, `SELECT st.name,st.category FROM status st JOIN project p ON p.workflow_id=st.workflow_id WHERE p.id::text=$1`, projectID)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var n, c string
		rows.Scan(&n, &c)
		m[n] = c
	}
	return m
}

func val(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// Point is one sample of a sprint chart; the same samples feed the burndown and the burnup.
type Point struct {
	At        time.Time `json:"at"`
	Remaining float64   `json:"remaining"` // estimate not yet done, for issues in the sprint at that moment
	Ideal     float64   `json:"ideal"`     // straight line over the working days
	Scope     float64   `json:"scope"`     // total estimate in the sprint at that moment (the burnup's ceiling)
	Done      float64   `json:"done"`      // estimate completed
}

type ReportItem struct {
	Key      string   `json:"key"`
	Summary  string   `json:"summary"`
	Estimate *float64 `json:"estimate,omitempty"`
}

type SprintReport struct {
	Sprint       Sprint       `json:"sprint"`
	Committed    float64      `json:"committed"`
	Completed    float64      `json:"completed"`
	CompletedIs  []ReportItem `json:"completed_issues"`
	NotCompleted []ReportItem `json:"not_completed_issues"`
	Removed      []ReportItem `json:"removed_issues"`
	Added        []ReportItem `json:"added_issues"`
	Burn         []Point      `json:"burn"`
}

type member struct {
	id        string
	addedAt   time.Time
	removedAt *time.Time
	committed bool
	outcome   *string
}

func (s *Service) SprintReport(ctx context.Context, a Actor, sprintID string) (SprintReport, error) {
	sp, b, role, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return SprintReport{}, err
	}
	if role == "" {
		return SprintReport{}, ErrNotFound
	}
	rep := SprintReport{Sprint: sp, CompletedIs: []ReportItem{}, NotCompleted: []ReportItem{}, Removed: []ReportItem{}, Added: []ReportItem{}, Burn: []Point{}}
	var pid string
	s.DB.QueryRow(ctx, `SELECT project_id::text FROM board WHERE id::text=$1`, b.ID).Scan(&pid)
	rows, err := s.DB.Query(ctx, `SELECT si.issue_id::text,si.added_at,si.removed_at,si.committed,si.outcome FROM sprint_issue si JOIN issue i ON i.id=si.issue_id WHERE si.sprint_id::text=$1 AND i.deleted_at IS NULL`, sp.ID)
	if err != nil {
		return rep, err
	}
	var ms []member
	var ids []string
	for rows.Next() {
		var m member
		rows.Scan(&m.id, &m.addedAt, &m.removedAt, &m.committed, &m.outcome)
		ms = append(ms, m)
		ids = append(ids, m.id)
	}
	rows.Close()
	tl, err := s.loadTimelines(ctx, ids)
	if err != nil {
		return rep, err
	}
	cats := s.categories(ctx, pid)
	cal := sp.Calendar
	now := s.Now()
	// the chart runs from the start to the end, the close, or now, whichever comes first
	start := sp.Start
	last := sp.End
	if sp.ClosedAt != nil && sp.ClosedAt.Before(last) {
		last = *sp.ClosedAt
	}
	if sp.State == "future" {
		return rep, nil
	}
	if now.Before(last) {
		last = now
	}
	if sp.StartedAt != nil && sp.StartedAt.After(start) {
		start = *sp.StartedAt // a sprint started late is measured from its real start
	}
	at := func(t time.Time) (remaining, scope, done float64) {
		for _, m := range ms {
			if m.addedAt.After(t) || (m.removedAt != nil && !m.removedAt.After(t)) {
				continue
			}
			x := tl[m.id]
			e := val(x.estAt(t))
			scope += e
			if cats[x.statusAt(t)] == "done" {
				done += e
			} else {
				remaining += e
			}
		}
		return
	}
	r0, _, _ := at(start)
	totalDays := cal.WorkingDaysBetween(start, sp.End)
	var samples []time.Time
	samples = append(samples, start)
	for d := cal.StartOfDay(start).AddDate(0, 0, 1); d.Before(last); d = cal.StartOfDay(d.AddDate(0, 0, 1).Add(12 * time.Hour)) {
		samples = append(samples, d)
		if len(samples) > 400 {
			break
		}
	}
	if last.After(start) {
		samples = append(samples, last)
	}
	for _, t := range samples {
		r, sc, dn := at(t)
		ideal := 0.0
		if totalDays > 0 {
			ideal = r0 * float64(cal.WorkingDaysBetween(t, sp.End)) / float64(totalDays)
		}
		rep.Burn = append(rep.Burn, Point{At: t, Remaining: round2(r), Ideal: round2(ideal), Scope: round2(sc), Done: round2(dn)})
	}
	item := func(id string, atTime time.Time) ReportItem {
		x := tl[id]
		return ReportItem{Key: x.key, Summary: x.summary, Estimate: x.estAt(atTime)}
	}
	sort.Slice(ms, func(i, j int) bool { return tl[ms[i].id].key < tl[ms[j].id].key })
	for _, m := range ms {
		x := tl[m.id]
		if m.committed {
			rep.Committed += val(x.estAt(start))
		}
		if sp.StartedAt != nil && m.removedAt != nil && !m.removedAt.Before(*sp.StartedAt) {
			rep.Removed = append(rep.Removed, item(m.id, *m.removedAt))
			continue
		}
		if m.removedAt != nil {
			continue // taken out while planning: never part of the sprint
		}
		if !m.committed && sp.StartedAt != nil && !m.addedAt.Before(*sp.StartedAt) {
			rep.Added = append(rep.Added, item(m.id, m.addedAt))
		}
		done := false
		if m.outcome != nil {
			done = *m.outcome == "done"
		} else {
			done = cats[x.statusAt(last)] == "done"
		}
		if done {
			it := item(m.id, last)
			rep.CompletedIs = append(rep.CompletedIs, it)
			rep.Completed += val(it.Estimate)
		} else {
			rep.NotCompleted = append(rep.NotCompleted, item(m.id, last))
		}
	}
	rep.Committed, rep.Completed = round2(rep.Committed), round2(rep.Completed)
	return rep, nil
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

type VelocityBar struct {
	Sprint    string  `json:"sprint"`
	ID        string  `json:"id"`
	Committed float64 `json:"committed"`
	Completed float64 `json:"completed"`
}

// Velocity compares committed with completed estimate over the last closed sprints (oldest first).
func (s *Service) Velocity(ctx context.Context, a Actor, boardID string, n int) ([]VelocityBar, error) {
	b, _, role, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return nil, err
	}
	_ = role
	if b.Kind != "scrum" {
		return nil, invalid("velocity needs sprints: use a scrum board")
	}
	if n < 1 || n > 30 {
		n = 6
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM sprint WHERE board_id::text=$1 AND state='closed' ORDER BY closed_at DESC LIMIT $2`, b.ID, n)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []VelocityBar{}
	for i := len(ids) - 1; i >= 0; i-- {
		r, err := s.SprintReport(ctx, a, ids[i])
		if err != nil {
			return nil, err
		}
		out = append(out, VelocityBar{Sprint: r.Sprint.Name, ID: r.Sprint.ID, Committed: r.Committed, Completed: r.Completed})
	}
	return out, nil
}

// flowScope loads the issues a kanban-style report covers: the board's project, optionally restricted by filter keys.
func (s *Service) flowScope(ctx context.Context, a Actor, boardID string, restrict []string) (Board, string, map[string]*timeline, map[string]string, error) {
	b, pid, _, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return b, "", nil, nil, err
	}
	q := `SELECT id::text FROM issue WHERE project_id::text=$1 AND deleted_at IS NULL AND NOT restricted`
	args := []any{pid}
	if restrict != nil {
		q += ` AND key = ANY($2)`
		args = append(args, restrict)
	}
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return b, pid, nil, nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	tl, err := s.loadTimelines(ctx, ids)
	return b, pid, tl, s.categories(ctx, pid), err
}

func clampDays(d int) int {
	if d < 1 || d > 365 {
		return 30
	}
	return d
}

type FlowDay struct {
	Day    string         `json:"day"`
	Counts map[string]int `json:"counts"`
}

type CumulativeFlow struct {
	Statuses []string  `json:"statuses"` // in workflow order
	Days     []FlowDay `json:"days"`
}

// CumulativeFlow counts issues by status at the end of each of the last `days` local days.
func (s *Service) CumulativeFlow(ctx context.Context, a Actor, boardID string, days int, restrict []string) (CumulativeFlow, error) {
	b, pid, tl, cats, err := s.flowScope(ctx, a, boardID, restrict)
	if err != nil {
		return CumulativeFlow{}, err
	}
	days = clampDays(days)
	cal := b.effectiveCalendar()
	out := CumulativeFlow{Statuses: []string{}, Days: []FlowDay{}}
	sr, _ := s.DB.Query(ctx, `SELECT st.name FROM status st JOIN project p ON p.workflow_id=st.workflow_id WHERE p.id::text=$1 ORDER BY st.position`, pid)
	for sr.Next() {
		var n string
		sr.Scan(&n)
		out.Statuses = append(out.Statuses, n)
	}
	sr.Close()
	_ = cats
	today := cal.StartOfDay(s.Now())
	for i := days - 1; i >= 0; i-- {
		d := cal.StartOfDay(today.AddDate(0, 0, -i).Add(12 * time.Hour))
		endOfDay := cal.StartOfDay(d.AddDate(0, 0, 1).Add(12 * time.Hour)).Add(-time.Nanosecond)
		if endOfDay.After(s.Now()) {
			endOfDay = s.Now()
		}
		fd := FlowDay{Day: d.In(cal.LocationOrUTC()).Format("2006-01-02"), Counts: map[string]int{}}
		for _, st := range out.Statuses {
			fd.Counts[st] = 0
		}
		for _, t := range tl {
			if st := t.statusAt(endOfDay); st != "" {
				fd.Counts[st]++
			}
		}
		out.Days = append(out.Days, fd)
	}
	return out, nil
}

type CyclePoint struct {
	Key      string    `json:"key"`
	Resolved time.Time `json:"resolved"`
	Days     float64   `json:"cycle_days"`
}

type ControlChart struct {
	Points  []CyclePoint `json:"points"`
	Average float64      `json:"average_days"`
	StdDev  float64      `json:"std_dev_days"`
}

// ControlChart plots the cycle time (first work started to resolved) of issues resolved in the last `days` days.
func (s *Service) ControlChart(ctx context.Context, a Actor, boardID string, days int, restrict []string) (ControlChart, error) {
	_, _, tl, cats, err := s.flowScope(ctx, a, boardID, restrict)
	if err != nil {
		return ControlChart{}, err
	}
	days = clampDays(days)
	from := s.Now().AddDate(0, 0, -days)
	out := ControlChart{Points: []CyclePoint{}}
	for _, t := range tl {
		if t.resolved == nil || t.resolved.Before(from) || t.resolved.After(s.Now()) {
			continue
		}
		startWork := t.created
		for _, p := range t.statuses {
			if cats[p.name] == "in_progress" {
				startWork = p.at
				break
			}
		}
		out.Points = append(out.Points, CyclePoint{Key: t.key, Resolved: *t.resolved, Days: round2(t.resolved.Sub(startWork).Hours() / 24)})
	}
	sort.Slice(out.Points, func(i, j int) bool { return out.Points[i].Resolved.Before(out.Points[j].Resolved) })
	if n := float64(len(out.Points)); n > 0 {
		sum := 0.0
		for _, p := range out.Points {
			sum += p.Days
		}
		out.Average = round2(sum / n)
		v := 0.0
		for _, p := range out.Points {
			v += (p.Days - out.Average) * (p.Days - out.Average)
		}
		out.StdDev = round2(math.Sqrt(v / n))
	}
	return out, nil
}

type CreatedResolvedDay struct {
	Day                  string `json:"day"`
	Created, Resolved    int
	CumCreated, CumResol int
}

// CreatedVsResolved counts issues created and resolved per local day over the last `days` days.
func (s *Service) CreatedVsResolved(ctx context.Context, a Actor, boardID string, days int, restrict []string) ([]CreatedResolvedDay, error) {
	b, _, tl, _, err := s.flowScope(ctx, a, boardID, restrict)
	if err != nil {
		return nil, err
	}
	days = clampDays(days)
	cal := b.effectiveCalendar()
	today := cal.StartOfDay(s.Now())
	out := []CreatedResolvedDay{}
	cc, cr := 0, 0
	for i := days - 1; i >= 0; i-- {
		d := cal.StartOfDay(today.AddDate(0, 0, -i).Add(12 * time.Hour))
		next := cal.StartOfDay(d.AddDate(0, 0, 1).Add(12 * time.Hour))
		row := CreatedResolvedDay{Day: d.In(cal.LocationOrUTC()).Format("2006-01-02")}
		for _, t := range tl {
			if !t.created.Before(d) && t.created.Before(next) {
				row.Created++
			}
			if t.resolved != nil && !t.resolved.Before(d) && t.resolved.Before(next) {
				row.Resolved++
			}
		}
		cc += row.Created
		cr += row.Resolved
		row.CumCreated, row.CumResol = cc, cr
		out = append(out, row)
	}
	return out, nil
}

type Workload struct {
	Assignee string  `json:"assignee"`
	Open     int     `json:"open"`
	Estimate float64 `json:"estimate"`
}

// WorkloadByAssignee is the open work each person holds right now.
func (s *Service) WorkloadByAssignee(ctx context.Context, a Actor, boardID string, restrict []string) ([]Workload, error) {
	_, _, tl, cats, err := s.flowScope(ctx, a, boardID, restrict)
	if err != nil {
		return nil, err
	}
	m := map[string]*Workload{}
	for _, t := range tl {
		if cats[t.curStatus] == "done" {
			continue
		}
		n := t.assignee
		if n == "" {
			n = "Unassigned"
		}
		w := m[n]
		if w == nil {
			w = &Workload{Assignee: n}
			m[n] = w
		}
		w.Open++
		w.Estimate += val(t.curEst)
	}
	out := []Workload{}
	for _, w := range m {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Open != out[j].Open {
			return out[i].Open > out[j].Open
		}
		return out[i].Assignee < out[j].Assignee
	})
	return out, nil
}

type StatusTime struct {
	Status     string  `json:"status"`
	TotalDays  float64 `json:"total_days"`
	AvgDays    float64 `json:"avg_days_per_issue"`
	IssueCount int     `json:"issues"`
}

// TimeInStatus sums how long issues sat in each status within the last `days` days.
func (s *Service) TimeInStatus(ctx context.Context, a Actor, boardID string, days int, restrict []string) ([]StatusTime, error) {
	_, _, tl, _, err := s.flowScope(ctx, a, boardID, restrict)
	if err != nil {
		return nil, err
	}
	days = clampDays(days)
	now := s.Now()
	from := now.AddDate(0, 0, -days)
	tot := map[string]float64{}
	cnt := map[string]int{}
	for _, t := range tl {
		seen := map[string]bool{}
		cur, since := t.status0, t.created
		add := func(name string, a, b time.Time) {
			if a.Before(from) {
				a = from
			}
			if b.After(now) {
				b = now
			}
			if !b.After(a) || name == "" {
				return
			}
			tot[name] += b.Sub(a).Hours() / 24
			if !seen[name] {
				seen[name] = true
				cnt[name]++
			}
		}
		for _, p := range t.statuses {
			add(cur, since, p.at)
			cur, since = p.name, p.at
		}
		add(cur, since, now)
	}
	out := []StatusTime{}
	for n, v := range tot {
		out = append(out, StatusTime{Status: n, TotalDays: round2(v), AvgDays: round2(v / float64(cnt[n])), IssueCount: cnt[n]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status < out[j].Status })
	return out, nil
}

type EpicBurn struct {
	Epic   string  `json:"epic"`
	Points []Point `json:"points"`
}

// EpicBurndown charts the stories under an epic: remaining and total estimate at the end of each day since the first
// child appeared (at most 120 days).
func (s *Service) EpicBurndown(ctx context.Context, a Actor, boardID, epicKey string) (EpicBurn, error) {
	b, pid, _, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return EpicBurn{}, err
	}
	is, role, err := s.access(ctx, a, epicKey)
	if err != nil || role == "" || is.Project != b.Project {
		return EpicBurn{}, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT c.id::text FROM issue c WHERE c.parent_id::text=$1 AND c.deleted_at IS NULL`, is.ID)
	if err != nil {
		return EpicBurn{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	tl, err := s.loadTimelines(ctx, ids)
	if err != nil {
		return EpicBurn{}, err
	}
	cats := s.categories(ctx, pid)
	cal := b.effectiveCalendar()
	out := EpicBurn{Epic: is.Key, Points: []Point{}}
	if len(tl) == 0 {
		return out, nil
	}
	first := s.Now()
	for _, t := range tl {
		if t.created.Before(first) {
			first = t.created
		}
	}
	from := cal.StartOfDay(first)
	if min := cal.StartOfDay(s.Now()).AddDate(0, 0, -120); from.Before(min) {
		from = min
	}
	for d := from; ; d = cal.StartOfDay(d.AddDate(0, 0, 1).Add(12 * time.Hour)) {
		end := cal.StartOfDay(d.AddDate(0, 0, 1).Add(12 * time.Hour)).Add(-time.Nanosecond)
		if end.After(s.Now()) {
			end = s.Now()
		}
		var rem, scope, done float64
		for _, t := range tl {
			if t.statusAt(end) == "" {
				continue
			}
			e := val(t.estAt(end))
			scope += e
			if cats[t.statusAt(end)] == "done" {
				done += e
			} else {
				rem += e
			}
		}
		out.Points = append(out.Points, Point{At: end, Remaining: round2(rem), Scope: round2(scope), Done: round2(done)})
		if !end.Before(s.Now()) || len(out.Points) > 130 {
			break
		}
	}
	return out, nil
}

var _ = pgx.ErrNoRows
var _ calendar.Calendar
