package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/calendar"
)

// DefaultCalendar is the work week used until an organization or board sets its own: Monday to Friday, UTC.
func DefaultCalendar() calendar.Calendar {
	c, _ := calendar.Preset("mon-fri", "UTC")
	return c
}

func (b Board) effectiveCalendar() calendar.Calendar {
	if b.Calendar != nil {
		return *b.Calendar
	}
	return DefaultCalendar()
}

type Sprint struct {
	ID        string            `json:"id"`
	Board     string            `json:"board"`
	Seq       int               `json:"seq"`
	Name      string            `json:"name"`
	Goal      string            `json:"goal"`
	State     string            `json:"state"` // future, active, closed
	Start     time.Time         `json:"starts_at"`
	End       time.Time         `json:"ends_at"` // exclusive: the next week start
	StartedAt *time.Time        `json:"started_at,omitempty"`
	ClosedAt  *time.Time        `json:"closed_at,omitempty"`
	Retro     string            `json:"retro"`
	Calendar  calendar.Calendar `json:"calendar"` // the snapshot the boundaries were computed with
}

type Bucket struct {
	Count    int     `json:"count"`
	Estimate float64 `json:"estimate"`
}

// Totals are the issue counts and estimates by status category.
type Totals struct {
	Todo       Bucket `json:"todo"`
	InProgress Bucket `json:"in_progress"`
	Done       Bucket `json:"done"`
}

type PersonCapacity struct {
	User     string  `json:"user"`
	Hours    float64 `json:"hours"`
	OffHours float64 `json:"off_hours"`
}

type SprintView struct {
	Sprint
	Issues       []Card           `json:"issues"`
	Totals       Totals           `json:"totals"`
	WorkingDays  int              `json:"working_days"`
	DaysLeft     int              `json:"days_left"`
	Capacity     float64          `json:"capacity_hours"`
	People       []PersonCapacity `json:"people"`
	Committed    float64          `json:"committed_estimate"`
	Commit       string           `json:"commit,omitempty"` // over, under or fit, when estimates are hours
	ScopeAdded   int              `json:"scope_added"`
	ScopeRemoved int              `json:"scope_removed"`
}

type BacklogView struct {
	Board         Board        `json:"board"`
	Sprints       []SprintView `json:"sprints"` // active first, then future
	Backlog       []Card       `json:"backlog"`
	BacklogTotals Totals       `json:"backlog_totals"`
	Role          string       `json:"role"`
}

// SystemActor runs the scheduled jobs (rollover, auto-sprints).
var SystemActor = Actor{Label: "System", Admin: true, Kind: "system"}

const sprintCols = `id::text,board_id::text,seq,name,goal,state,starts_at,ends_at,started_at,closed_at,retro,calendar`

func scanSprint(row pgx.Row) (Sprint, error) {
	var sp Sprint
	var cal []byte
	err := row.Scan(&sp.ID, &sp.Board, &sp.Seq, &sp.Name, &sp.Goal, &sp.State, &sp.Start, &sp.End, &sp.StartedAt, &sp.ClosedAt, &sp.Retro, &cal)
	if err == pgx.ErrNoRows {
		return sp, ErrNotFound
	}
	json.Unmarshal(cal, &sp.Calendar)
	return sp, err
}

// sprintAccess loads a sprint and the caller's role on its board's project.
func (s *Service) sprintAccess(ctx context.Context, a Actor, id string) (Sprint, Board, string, error) {
	sp, err := scanSprint(s.DB.QueryRow(ctx, `SELECT `+sprintCols+` FROM sprint WHERE id::text=$1`, id))
	if err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid") {
		err = ErrNotFound
	}
	if err != nil {
		return sp, Board{}, "", err
	}
	b, _, role, err := s.boardAccess(ctx, a, sp.Board)
	return sp, b, role, err
}

func (s *Service) lastSprintEnd(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, boardID string) *time.Time {
	var t *time.Time
	q.QueryRow(ctx, `SELECT max(ends_at) FROM sprint WHERE board_id::text=$1`, boardID).Scan(&t)
	return t
}

// alignStart moves t forward to the next week start of the calendar (t itself when it already is one), so a sprint
// never begins before the previous one ended even when the calendar changed in between.
func alignStart(cal calendar.Calendar, t time.Time) time.Time {
	if cal.WeekStartOf(t).Equal(t) {
		return t
	}
	_, end := cal.Bounds(t, 1)
	return end
}

// nextStart is where the next sprint begins: right after the last one, or the current week when there is none (or it is long over).
func (s *Service) nextStart(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, b Board, cal calendar.Calendar) time.Time {
	now := s.Now()
	if e := s.lastSprintEnd(ctx, q, b.ID); e != nil && e.After(cal.WeekStartOf(now)) {
		return alignStart(cal, *e)
	}
	return cal.WeekStartOf(now)
}

// createSprintTx inserts one sprint starting at the week containing `from`; the caller holds the board row lock.
func (s *Service) createSprintTx(ctx context.Context, tx pgx.Tx, b Board, cal calendar.Calendar, from time.Time, name, goal string) (string, error) {
	start, end := cal.Bounds(from, b.SprintWeeks)
	var seq int
	tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0)+1 FROM sprint WHERE board_id::text=$1`, b.ID).Scan(&seq)
	auto := strings.TrimSpace(name) == ""
	if auto {
		name = cal.SprintName(b.NamePattern, start, end, seq)
	}
	calj, _ := json.Marshal(cal.Normalize())
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO sprint(board_id,seq,name,auto_named,goal,starts_at,ends_at,calendar) VALUES($1::uuid,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
		b.ID, seq, name, auto, goal, start.UTC(), end.UTC(), calj).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "sprint_board_id_starts_at_key") {
		return "", invalid("a sprint already starts at %s", start.Format("2006-01-02"))
	}
	return id, err
}

func (s *Service) lockBoard(ctx context.Context, tx pgx.Tx, id string) error {
	var x string
	return tx.QueryRow(ctx, `SELECT id::text FROM board WHERE id::text=$1 FOR UPDATE`, id).Scan(&x)
}

type SprintIn struct {
	Name string `json:"name"`
	Goal string `json:"goal"`
}

// CreateSprint adds a future sprint right after the last one (project administrators; scrum boards only).
func (s *Service) CreateSprint(ctx context.Context, a Actor, boardID string, in SprintIn) (Sprint, error) {
	b, _, role, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return Sprint{}, err
	}
	if !s.canIn(ctx, b.Project, role, "manage_sprints") {
		return Sprint{}, ErrForbidden
	}
	if b.Kind != "scrum" {
		return Sprint{}, invalid("only scrum boards have sprints")
	}
	if site := s.mirrorSite(ctx, b.Project); site != "" { // weekly sprints and rollover are not available on mirrored projects
		return Sprint{}, &ReadOnlyMirror{Project: b.Project, SiteURL: site}
	}
	if len(in.Name) > 100 || len(in.Goal) > 500 {
		return Sprint{}, invalid("a sprint name is up to 100 characters and a goal up to 500")
	}
	var id string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := s.lockBoard(ctx, tx, b.ID); err != nil {
			return err
		}
		cal := b.effectiveCalendar()
		var err error
		id, err = s.createSprintTx(ctx, tx, b, cal, s.nextStart(ctx, tx, b, cal), in.Name, in.Goal)
		return err
	})
	if err != nil {
		return Sprint{}, err
	}
	return scanSprint(s.DB.QueryRow(ctx, `SELECT `+sprintCols+` FROM sprint WHERE id::text=$1`, id))
}

// EnsureSprints creates future sprints until the board has its configured number ready (idempotent, safe on every node).
func (s *Service) EnsureSprints(ctx context.Context, boardID string) (int, error) {
	b, _, err := s.boardRow(ctx, s.DB, boardID)
	if err != nil || b.Kind != "scrum" || s.mirrorSite(ctx, b.Project) != "" {
		return 0, err
	}
	created := 0
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := s.lockBoard(ctx, tx, b.ID); err != nil {
			return err
		}
		cal := b.effectiveCalendar()
		var have int
		tx.QueryRow(ctx, `SELECT count(*) FROM sprint WHERE board_id::text=$1 AND state='future'`, b.ID).Scan(&have)
		for ; have < b.AutoSprints; have++ {
			if _, err := s.createSprintTx(ctx, tx, b, cal, s.nextStart(ctx, tx, b, cal), "", ""); err != nil {
				return err
			}
			created++
		}
		return nil
	})
	return created, err
}

// recomputeFuture re-derives the boundaries, names and calendar snapshot of sprints that have not started, after the
// calendar, sprint length or name pattern changed. The active sprint keeps its boundaries (RecomputeSprint overrides).
func (s *Service) recomputeFuture(ctx context.Context, b Board) error {
	if b.Kind != "scrum" {
		return nil
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if err := s.lockBoard(ctx, tx, b.ID); err != nil {
			return err
		}
		cal := b.effectiveCalendar()
		rows, err := tx.Query(ctx, `SELECT id::text,seq,auto_named FROM sprint WHERE board_id::text=$1 AND state='future' ORDER BY starts_at`, b.ID)
		if err != nil {
			return err
		}
		type fs struct {
			id   string
			seq  int
			auto bool
		}
		var list []fs
		for rows.Next() {
			var f fs
			rows.Scan(&f.id, &f.seq, &f.auto)
			list = append(list, f)
		}
		rows.Close()
		if len(list) == 0 {
			return nil
		}
		// move them out of the way first: the unique (board, start) index would otherwise trip over a neighbour
		if _, err := tx.Exec(ctx, `UPDATE sprint SET starts_at=starts_at + interval '100 years', ends_at=ends_at + interval '100 years' WHERE board_id::text=$1 AND state='future'`, b.ID); err != nil {
			return err
		}
		var prevEnd *time.Time
		tx.QueryRow(ctx, `SELECT max(ends_at) FROM sprint WHERE board_id::text=$1 AND state<>'future'`, b.ID).Scan(&prevEnd)
		from := cal.WeekStartOf(s.Now())
		if prevEnd != nil && prevEnd.After(from) {
			from = alignStart(cal, *prevEnd)
		}
		calj, _ := json.Marshal(cal.Normalize())
		for _, f := range list {
			start, end := cal.Bounds(from, b.SprintWeeks)
			if _, err := tx.Exec(ctx, `UPDATE sprint SET starts_at=$2,ends_at=$3,calendar=$4,name=CASE WHEN auto_named THEN $5 ELSE name END WHERE id::text=$1`,
				f.id, start.UTC(), end.UTC(), calj, cal.SprintName(b.NamePattern, start, end, f.seq)); err != nil {
				return err
			}
			from = end
		}
		return nil
	})
}

// RecomputeSprint applies the board's current calendar to one sprint, the active one included (administrators).
func (s *Service) RecomputeSprint(ctx context.Context, a Actor, sprintID string) (Sprint, error) {
	sp, b, role, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return sp, err
	}
	if !s.canIn(ctx, b.Project, role, "manage_sprints") {
		return sp, ErrForbidden
	}
	if sp.State == "closed" {
		return sp, invalid("a closed sprint keeps its boundaries so its reports never change")
	}
	cal := b.effectiveCalendar()
	start, end := cal.Bounds(sp.Start, b.SprintWeeks)
	calj, _ := json.Marshal(cal.Normalize())
	if _, err := s.DB.Exec(ctx, `UPDATE sprint SET starts_at=$2,ends_at=$3,calendar=$4,name=CASE WHEN auto_named THEN $5 ELSE name END WHERE id::text=$1`,
		sp.ID, start.UTC(), end.UTC(), calj, cal.SprintName(b.NamePattern, start, end, sp.Seq)); err != nil {
		return sp, err
	}
	return scanSprint(s.DB.QueryRow(ctx, `SELECT `+sprintCols+` FROM sprint WHERE id::text=$1`, sp.ID))
}

// UpdateSprint edits the name and goal of a sprint that is not closed (administrators).
func (s *Service) UpdateSprint(ctx context.Context, a Actor, sprintID string, name, goal *string) (Sprint, error) {
	sp, b, role, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return sp, err
	}
	if !s.canIn(ctx, b.Project, role, "manage_sprints") {
		return sp, ErrForbidden
	}
	if sp.State == "closed" {
		return sp, invalid("a closed sprint cannot be edited; its retrospective notes can")
	}
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" || len(n) > 100 {
			return sp, invalid("a sprint name is 1 to 100 characters")
		}
		if _, err := s.DB.Exec(ctx, `UPDATE sprint SET name=$2,auto_named=false WHERE id::text=$1`, sp.ID, n); err != nil {
			return sp, err
		}
	}
	if goal != nil {
		if len(*goal) > 500 {
			return sp, invalid("a goal is up to 500 characters")
		}
		if _, err := s.DB.Exec(ctx, `UPDATE sprint SET goal=$2 WHERE id::text=$1`, sp.ID, *goal); err != nil {
			return sp, err
		}
	}
	return scanSprint(s.DB.QueryRow(ctx, `SELECT `+sprintCols+` FROM sprint WHERE id::text=$1`, sp.ID))
}

// StartSprint makes a future sprint active and records what it was committed to (administrators).
func (s *Service) StartSprint(ctx context.Context, a Actor, sprintID string) (Sprint, error) {
	sp, b, role, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return sp, err
	}
	if !s.canIn(ctx, b.Project, role, "manage_sprints") {
		return sp, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, b.Project)
	if perr != nil {
		return sp, perr
	}
	defer endWrite()
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := s.lockBoard(ctx, tx, b.ID); err != nil {
			return err
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM sprint WHERE id::text=$1 FOR UPDATE`, sp.ID).Scan(&state); err != nil {
			return err
		}
		if state != "future" {
			return invalid("only a future sprint can be started (this one is %s)", state)
		}
		if !b.Parallel {
			var other string
			if tx.QueryRow(ctx, `SELECT name FROM sprint WHERE board_id::text=$1 AND state='active' LIMIT 1`, b.ID).Scan(&other) == nil {
				return invalid("%q is still active; complete it first, or allow parallel sprints on this board", other)
			}
		}
		now := s.Now()
		if _, err := tx.Exec(ctx, `UPDATE sprint SET state='active',started_at=$2 WHERE id::text=$1`, sp.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sprint_issue SET committed=true WHERE sprint_id::text=$1 AND removed_at IS NULL`, sp.ID); err != nil {
			return err
		}
		return outbox(ctx, tx, "board:"+b.ID, "sprint.started", map[string]any{"board": b.ID, "sprint": sp.ID, "name": sp.Name, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return sp, err
	}
	return scanSprint(s.DB.QueryRow(ctx, `SELECT `+sprintCols+` FROM sprint WHERE id::text=$1`, sp.ID))
}

type CompleteResult struct {
	Sprint      Sprint   `json:"sprint"`
	Destination string   `json:"destination"` // "Backlog" or the name of the sprint the unfinished issues go to
	Done        []string `json:"done"`
	Moved       []string `json:"moved"` // unfinished issues, by key
	DryRun      bool     `json:"dry_run"`
}

var errRollback = errors.New("rollback")

// CompleteSprint closes an active sprint and decides where its unfinished issues go: dest is "next" (the next future
// sprint, created when there is none), "new" (a new sprint after the last one), "backlog", or the id of a future sprint.
// With dryRun the work is done in a transaction that is rolled back, so the preview is exactly the result (administrators).
func (s *Service) CompleteSprint(ctx context.Context, a Actor, sprintID, dest string, dryRun bool) (CompleteResult, error) {
	var res CompleteResult
	sp, b, role, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return res, err
	}
	if !s.canIn(ctx, b.Project, role, "manage_sprints") {
		return res, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, b.Project)
	if perr != nil {
		return res, perr
	}
	defer endWrite()
	if dest == "" {
		dest = "next"
	}
	res.DryRun = dryRun
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := s.lockBoard(ctx, tx, b.ID); err != nil {
			return err
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM sprint WHERE id::text=$1 FOR UPDATE`, sp.ID).Scan(&state); err != nil {
			return err
		}
		if state != "active" {
			return invalid("only an active sprint can be completed (this one is %s)", state)
		}
		cal := b.effectiveCalendar()
		destID, destName := "", "Backlog"
		switch dest {
		case "backlog":
		case "next", "new":
			if dest == "next" {
				tx.QueryRow(ctx, `SELECT id::text FROM sprint WHERE board_id::text=$1 AND state='future' ORDER BY starts_at LIMIT 1`, b.ID).Scan(&destID)
			}
			if destID == "" {
				var err error
				if destID, err = s.createSprintTx(ctx, tx, b, cal, s.nextStart(ctx, tx, b, cal), "", ""); err != nil {
					return err
				}
			}
		default:
			var st string
			if err := tx.QueryRow(ctx, `SELECT state FROM sprint WHERE id::text=$1 AND board_id::text=$2`, dest, b.ID).Scan(&st); err != nil {
				return invalid("the destination sprint is not on this board")
			}
			if st != "future" {
				return invalid("unfinished issues can only go to a future sprint, the backlog, or a new sprint")
			}
			destID = dest
		}
		if destID != "" {
			tx.QueryRow(ctx, `SELECT name FROM sprint WHERE id::text=$1`, destID).Scan(&destName)
		}
		rows, err := tx.Query(ctx, `SELECT i.id::text,i.key,st.category FROM sprint_issue si JOIN issue i ON i.id=si.issue_id JOIN status st ON st.id=i.status_id
			WHERE si.sprint_id::text=$1 AND si.removed_at IS NULL AND i.deleted_at IS NULL ORDER BY i.number`, sp.ID)
		if err != nil {
			return err
		}
		type m struct{ id, key, cat string }
		var members []m
		for rows.Next() {
			var x m
			rows.Scan(&x.id, &x.key, &x.cat)
			members = append(members, x)
		}
		rows.Close()
		now := s.Now()
		res.Done, res.Moved = []string{}, []string{}
		for _, x := range members {
			if x.cat == "done" {
				res.Done = append(res.Done, x.key)
				tx.Exec(ctx, `UPDATE sprint_issue SET outcome='done' WHERE sprint_id::text=$1 AND issue_id::text=$2`, sp.ID, x.id)
				continue
			}
			res.Moved = append(res.Moved, x.key)
			if _, err := tx.Exec(ctx, `UPDATE sprint_issue SET outcome='not_done' WHERE sprint_id::text=$1 AND issue_id::text=$2`, sp.ID, x.id); err != nil {
				return err
			}
			if destID != "" {
				if _, err := tx.Exec(ctx, `INSERT INTO sprint_issue(sprint_id,issue_id,added_at) VALUES($1::uuid,$2::uuid,$3)
					ON CONFLICT (sprint_id,issue_id) DO UPDATE SET removed_at=NULL,added_at=$3,committed=false,outcome=NULL`, destID, x.id, now); err != nil {
					return err
				}
			}
			from, to := sp.Name, destName
			s.hist(ctx, tx, x.id, a, "sprint", &from, &to)
		}
		if _, err := tx.Exec(ctx, `UPDATE sprint SET state='closed',closed_at=$2 WHERE id::text=$1`, sp.ID, now); err != nil {
			return err
		}
		res.Destination = destName
		if err := outbox(ctx, tx, "board:"+b.ID, "sprint.completed", map[string]any{"board": b.ID, "sprint": sp.ID, "name": sp.Name, "moved": len(res.Moved), "done": len(res.Done), "actor": a.Label, "actor_id": a.ID}); err != nil {
			return err
		}
		if dryRun {
			return errRollback
		}
		return nil
	})
	if errors.Is(err, errRollback) {
		err = nil
	}
	if err != nil {
		return res, err
	}
	res.Sprint, err = scanSprint(s.DB.QueryRow(ctx, `SELECT `+sprintCols+` FROM sprint WHERE id::text=$1`, sp.ID))
	return res, err
}

// containers

func (s *Service) containerCond(sprintID string) (string, []any) {
	if sprintID == "" {
		return `i.id NOT IN (SELECT si.issue_id FROM sprint_issue si JOIN sprint sp ON sp.id=si.sprint_id WHERE sp.board_id::text=$%d AND sp.state<>'closed' AND si.removed_at IS NULL)
			AND i.status_id IN (SELECT id FROM status WHERE category<>'done')`, nil
	}
	return `i.id IN (SELECT si.issue_id FROM sprint_issue si WHERE si.sprint_id::text=$%d AND si.removed_at IS NULL)`, []any{sprintID}
}

// Backlog returns the sprints (active first, then future) with their issues and totals, and the ranked backlog below.
func (s *Service) Backlog(ctx context.Context, a Actor, boardID string) (BacklogView, error) {
	b, pid, role, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return BacklogView{}, err
	}
	if b.Kind != "scrum" {
		return BacklogView{}, invalid("a kanban board has no sprints; its backlog is the board's first column")
	}
	v := BacklogView{Board: b, Role: role, Sprints: []SprintView{}}
	rows, err := s.DB.Query(ctx, `SELECT `+sprintCols+` FROM sprint WHERE board_id::text=$1 AND state<>'closed' ORDER BY (state='active') DESC, starts_at`, b.ID)
	if err != nil {
		return v, err
	}
	var sps []Sprint
	for rows.Next() {
		sp, err := scanSprint(rows)
		if err != nil {
			rows.Close()
			return v, err
		}
		sps = append(sps, sp)
	}
	rows.Close()
	for _, sp := range sps {
		sv, err := s.sprintView(ctx, b, pid, sp)
		if err != nil {
			return v, err
		}
		v.Sprints = append(v.Sprints, sv)
	}
	v.Backlog, err = s.containerCards(ctx, b, pid, "")
	if err != nil {
		return v, err
	}
	v.BacklogTotals = totalsOf(v.Backlog)
	return v, nil
}

func (s *Service) containerCards(ctx context.Context, b Board, pid, sprintID string) ([]Card, error) {
	cond, extra := s.containerCond(sprintID)
	arg := b.ID
	if sprintID != "" {
		arg = sprintID
	}
	rows, err := s.DB.Query(ctx, cardSQL+` WHERE i.project_id=$2 AND i.deleted_at IS NULL AND i.archived_at IS NULL AND NOT i.restricted AND `+fmt.Sprintf(cond, 3)+` ORDER BY COALESCE(br.rank,i.rank) COLLATE "C", i.number`, b.ID, pid, arg)
	_ = extra
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Card{}
	for rows.Next() {
		c, _, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func totalsOf(cards []Card) Totals {
	var t Totals
	for _, c := range cards {
		e := 0.0
		if c.Estimate != nil {
			e = *c.Estimate
		}
		var bk *Bucket
		switch c.Category {
		case "done":
			bk = &t.Done
		case "in_progress":
			bk = &t.InProgress
		default:
			bk = &t.Todo
		}
		bk.Count++
		bk.Estimate += e
	}
	return t
}

func (s *Service) sprintView(ctx context.Context, b Board, pid string, sp Sprint) (SprintView, error) {
	v := SprintView{Sprint: sp, People: []PersonCapacity{}}
	var err error
	if v.Issues, err = s.containerCards(ctx, b, pid, sp.ID); err != nil {
		return v, err
	}
	v.Totals = totalsOf(v.Issues)
	for _, c := range v.Issues {
		if c.Estimate != nil {
			v.Committed += *c.Estimate
		}
	}
	cal := sp.Calendar
	v.WorkingDays = cal.WorkingDaysBetween(sp.Start, sp.End)
	if sp.State == "active" {
		v.DaysLeft = cal.DaysLeft(s.Now(), sp.End)
	} else if sp.State == "future" {
		v.DaysLeft = v.WorkingDays
	}
	s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE committed=false AND removed_at IS NULL AND added_at>=$2), count(*) FILTER (WHERE committed AND removed_at IS NOT NULL)
		FROM sprint_issue WHERE sprint_id::text=$1`, sp.ID, sp.StartedAt).Scan(&v.ScopeAdded, &v.ScopeRemoved)
	if sp.StartedAt == nil {
		v.ScopeAdded, v.ScopeRemoved = 0, 0 // nothing is scope change before the sprint starts
	}
	// capacity: each person under their own calendar (working days, hours and holidays), minus time booked off
	pr, err := s.DB.Query(ctx, `SELECT u.name,u.calendar,COALESCE(c.off_hours,0) FROM project_member m JOIN app_user u ON u.id=m.user_id AND u.active
		LEFT JOIN sprint_capacity c ON c.sprint_id::text=$2 AND c.user_id=u.id WHERE m.project_id=$1 AND m.role IN ('member','admin') AND u.kind='human' ORDER BY u.name`, pid, sp.ID)
	if err != nil {
		return v, err
	}
	for pr.Next() {
		var name string
		var raw []byte
		var off float64
		pr.Scan(&name, &raw, &off)
		pc := cal
		if len(raw) > 0 {
			var c calendar.Calendar
			if json.Unmarshal(raw, &c) == nil && c.Validate() == nil {
				pc = c
			}
		}
		h := calendar.Capacity(pc, sp.Start, sp.End, off)
		v.People = append(v.People, PersonCapacity{User: name, Hours: h, OffHours: off})
		v.Capacity += h
	}
	pr.Close()
	if b.Estimation == "time" && v.Capacity > 0 {
		switch {
		case v.Committed > v.Capacity:
			v.Commit = "over"
		case v.Committed < v.Capacity*0.6:
			v.Commit = "under"
		default:
			v.Commit = "fit"
		}
	}
	return v, nil
}

// ListSprints lists every sprint of a board, newest first, closed ones included (for the reports picker).
func (s *Service) ListSprints(ctx context.Context, a Actor, boardID string) ([]Sprint, error) {
	b, _, _, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+sprintCols+` FROM sprint WHERE board_id::text=$1 ORDER BY starts_at DESC`, b.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sprint{}
	for rows.Next() {
		sp, err := scanSprint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// GetSprint returns one sprint with its issues, totals, capacity and scope changes.
func (s *Service) GetSprint(ctx context.Context, a Actor, sprintID string) (SprintView, error) {
	sp, b, _, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return SprintView{}, err
	}
	var pid string
	s.DB.QueryRow(ctx, `SELECT project_id::text FROM board WHERE id::text=$1`, b.ID).Scan(&pid)
	return s.sprintView(ctx, b, pid, sp)
}

// AssignSprint puts issues into a sprint, or back into the backlog when sprintID is empty. An issue is in at most one
// open sprint of the board at a time. Epics and subtasks are not planned on their own (a subtask goes with its parent).
// pos places the first issue in the container; the others follow it. Any member may plan.
func (s *Service) AssignSprint(ctx context.Context, a Actor, boardID, sprintID string, keys []string, pos MoveIn) error {
	b, pid, role, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	if b.Kind != "scrum" {
		return invalid("only scrum boards have sprints")
	}
	if len(keys) == 0 || len(keys) > 500 {
		return invalid("move 1 to 500 issues at a time")
	}
	endWrite, perr := s.admit(ctx, b.Project)
	if perr != nil {
		return perr
	}
	defer endWrite()
	return s.tx(ctx, func(tx pgx.Tx) error {
		var target Sprint
		if sprintID != "" {
			var err error
			if target, err = scanSprint(tx.QueryRow(ctx, `SELECT `+sprintCols+` FROM sprint WHERE id::text=$1 AND board_id::text=$2 FOR UPDATE`, sprintID, b.ID)); err != nil {
				return invalid("that sprint is not on this board")
			}
			if target.State == "closed" {
				return invalid("a closed sprint cannot take issues")
			}
		}
		now := s.Now()
		var moved []string
		for _, k := range keys {
			var id, ikey string
			var level int
			var parent *string
			err := tx.QueryRow(ctx, `SELECT i.id::text,i.key,t.level,i.parent_id::text FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.key=$1 AND i.project_id::text=$2 AND i.deleted_at IS NULL`, strings.ToUpper(k), pid).Scan(&id, &ikey, &level, &parent)
			if err == pgx.ErrNoRows {
				return invalid("%s is not an issue of this board's project", strings.ToUpper(k))
			}
			if err != nil {
				return err
			}
			if level == 1 {
				return invalid("%s is an epic: plan its stories instead", ikey)
			}
			if level < 0 {
				return invalid("%s is a subtask: it moves with its parent", ikey)
			}
			// leave the open sprint it is in (recording scope change if that sprint is running)
			orows, err := tx.Query(ctx, `SELECT sp.id::text,sp.name FROM sprint_issue si JOIN sprint sp ON sp.id=si.sprint_id WHERE si.issue_id::text=$1 AND si.removed_at IS NULL AND sp.board_id::text=$2 AND sp.state<>'closed'`, id, b.ID)
			if err != nil {
				return err
			}
			type old struct{ id, name string }
			var olds []old
			for orows.Next() {
				var o old
				orows.Scan(&o.id, &o.name)
				olds = append(olds, o)
			}
			orows.Close()
			if len(olds) == 1 && olds[0].id == sprintID {
				moved = append(moved, id)
				continue // already there
			}
			from := "Backlog"
			for _, o := range olds {
				from = o.name
				if _, err := tx.Exec(ctx, `UPDATE sprint_issue SET removed_at=$3 WHERE sprint_id::text=$1 AND issue_id::text=$2`, o.id, id, now); err != nil {
					return err
				}
			}
			to := "Backlog"
			if sprintID != "" {
				to = target.Name
				if _, err := tx.Exec(ctx, `INSERT INTO sprint_issue(sprint_id,issue_id,added_at) VALUES($1::uuid,$2::uuid,$3)
					ON CONFLICT (sprint_id,issue_id) DO UPDATE SET removed_at=NULL,added_at=$3,committed=false,outcome=NULL`, sprintID, id, now); err != nil {
					return err
				}
			}
			if from != to {
				s.hist(ctx, tx, id, a, "sprint", &from, &to)
			}
			moved = append(moved, id)
		}
		if pos.After != "" || pos.Before != "" || pos.Top {
			if err := s.placeInContainer(ctx, tx, b, pid, sprintID, moved, pos); err != nil {
				return err
			}
		}
		return outbox(ctx, tx, "board:"+b.ID, "sprint.issues", map[string]any{"board": b.ID, "sprint": sprintID, "keys": keys, "actor": a.Label, "actor_id": a.ID})
	})
}

// RankInContainer reorders one issue inside its sprint or inside the backlog (before, after, top or bottom).
func (s *Service) RankInContainer(ctx context.Context, a Actor, boardID, key string, pos MoveIn) error {
	b, pid, role, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	if b.Kind != "scrum" {
		return invalid("only scrum boards have a backlog")
	}
	endWrite, perr := s.admit(ctx, b.Project)
	if perr != nil {
		return perr
	}
	defer endWrite()
	return s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT i.id::text FROM issue i WHERE i.key=$1 AND i.project_id::text=$2 AND i.deleted_at IS NULL`, strings.ToUpper(key), pid).Scan(&id); err != nil {
			return invalid("%s is not an issue of this board's project", strings.ToUpper(key))
		}
		var sprintID string
		tx.QueryRow(ctx, `SELECT sp.id::text FROM sprint_issue si JOIN sprint sp ON sp.id=si.sprint_id WHERE si.issue_id::text=$1 AND si.removed_at IS NULL AND sp.board_id::text=$2 AND sp.state<>'closed'`, id, b.ID).Scan(&sprintID)
		return s.placeInContainer(ctx, tx, b, pid, sprintID, []string{id}, pos)
	})
}

// placeInContainer ranks the issues (ids, in order) at the requested spot among the container's members. Neighbours
// must be in the same container; a missing position means the bottom, Top means the top.
func (s *Service) placeInContainer(ctx context.Context, tx pgx.Tx, b Board, pid, sprintID string, ids []string, pos MoveIn) error {
	cond, _ := s.containerCond(sprintID)
	arg := b.ID
	if sprintID != "" {
		arg = sprintID
	}
	cond = fmt.Sprintf(cond, 3)
	// every member gets a board rank first, so neighbours always have one to compare with
	if _, err := tx.Exec(ctx, `INSERT INTO board_rank(board_id,issue_id,rank) SELECT $1::uuid,i.id,i.rank FROM issue i WHERE i.project_id::text=$2 AND i.deleted_at IS NULL AND `+cond+` ON CONFLICT DO NOTHING`, b.ID, pid, arg); err != nil {
		return err
	}
	rankOf := func(key string) (string, error) {
		var r string
		err := tx.QueryRow(ctx, `SELECT br.rank FROM board_rank br JOIN issue i ON i.id=br.issue_id WHERE br.board_id=$1::uuid AND i.key=$4 AND i.project_id::text=$2 AND `+cond+` AND NOT (i.id::text = ANY($5))`, b.ID, pid, arg, strings.ToUpper(key), ids).Scan(&r)
		if err == pgx.ErrNoRows {
			return "", invalid("%s is not in that list", strings.ToUpper(key))
		}
		return r, err
	}
	edge := func(agg, extra string, ref string) string {
		var r *string
		tx.QueryRow(ctx, `SELECT `+agg+`(br.rank COLLATE "C") FROM board_rank br JOIN issue i ON i.id=br.issue_id WHERE br.board_id=$1::uuid AND i.project_id::text=$2 AND i.deleted_at IS NULL AND `+cond+` AND NOT (i.id::text = ANY($5)) `+extra, b.ID, pid, arg, ref, ids).Scan(&r)
		if r == nil {
			return ""
		}
		return *r
	}
	var lo, hi string
	switch {
	case pos.After != "":
		r, err := rankOf(pos.After)
		if err != nil {
			return err
		}
		lo, hi = r, edge("min", `AND br.rank COLLATE "C" > $4`, r)
	case pos.Before != "":
		r, err := rankOf(pos.Before)
		if err != nil {
			return err
		}
		hi, lo = r, edge("max", `AND br.rank COLLATE "C" < $4`, r)
	case pos.Top:
		hi = edge("min", `AND $4=$4`, "")
	default:
		lo = edge("max", `AND $4=$4`, "")
	}
	for _, id := range ids {
		r, ok := rankBetween(lo, hi)
		if !ok || len(r) > 40 { // rank strings grew long: spread this container out again, then retry once
			if err := s.rebalanceContainer(ctx, tx, b, pid, cond, arg); err != nil {
				return err
			}
			return s.placeInContainer(ctx, tx, b, pid, sprintID, ids, pos)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO board_rank(board_id,issue_id,rank) VALUES($1::uuid,$2::uuid,$3) ON CONFLICT (board_id,issue_id) DO UPDATE SET rank=$3`, b.ID, id, r); err != nil {
			return err
		}
		lo = r // the next one goes right after it
	}
	return nil
}

func (s *Service) rebalanceContainer(ctx context.Context, tx pgx.Tx, b Board, pid, cond, arg string) error {
	rows, err := tx.Query(ctx, `SELECT i.id::text FROM issue i LEFT JOIN board_rank br ON br.board_id=$1::uuid AND br.issue_id=i.id WHERE i.project_id::text=$2 AND i.deleted_at IS NULL AND `+cond+` ORDER BY COALESCE(br.rank,i.rank) COLLATE "C", i.number`, b.ID, pid, arg)
	if err != nil {
		return err
	}
	var order []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		order = append(order, id)
	}
	rows.Close()
	for i, id := range order {
		if _, err := tx.Exec(ctx, `INSERT INTO board_rank(board_id,issue_id,rank) VALUES($1::uuid,$2::uuid,$3) ON CONFLICT (board_id,issue_id) DO UPDATE SET rank=$3`, b.ID, id, spacedRank(i)); err != nil {
			return err
		}
	}
	return nil
}

// SetCapacity records time a person has booked off during a sprint (themselves, or an administrator for anyone).
func (s *Service) SetCapacity(ctx context.Context, a Actor, sprintID, userID string, offHours float64) error {
	sp, _, role, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return err
	}
	if !(role == "admin" || (canWrite(role) && userID == a.ID)) {
		return ErrForbidden
	}
	if offHours < 0 || offHours > 500 {
		return invalid("hours off are between 0 and 500")
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO sprint_capacity(sprint_id,user_id,off_hours) VALUES($1::uuid,$2::uuid,$3) ON CONFLICT (sprint_id,user_id) DO UPDATE SET off_hours=$3`, sp.ID, userID, offHours)
	if err != nil && strings.Contains(err.Error(), "violates foreign key") {
		return ErrNotFound
	}
	return err
}

// SetUserCalendar sets one person's own work calendar (themselves or an administrator); nil clears it.
func (s *Service) SetUserCalendar(ctx context.Context, a Actor, userID string, c *calendar.Calendar) error {
	if a.ID != userID && !a.Admin {
		return ErrForbidden
	}
	var val any
	if c != nil {
		n := c.Normalize()
		if err := n.Validate(); err != nil {
			return invalid("calendar: %v", err)
		}
		val, _ = json.Marshal(n)
	}
	tag, err := s.DB.Exec(ctx, `UPDATE app_user SET calendar=$2 WHERE id::text=$1`, userID, val)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRetro stores the retrospective notes of a sprint (any member; usually after it closes).
func (s *Service) SetRetro(ctx context.Context, a Actor, sprintID, text string) error {
	sp, _, role, err := s.sprintAccess(ctx, a, sprintID)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	text, _ = Sanitize(text)
	if err := checkSize(text); err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE sprint SET retro=$2 WHERE id::text=$1`, sp.ID, text)
	return err
}

// RunSprintJobs is the scheduler tick (SPEC 7.2 auto-sprints and auto-rollover). It is safe to run on every node: each
// step re-checks state under a lock. It returns the number of sprints rolled over and created.
func (s *Service) RunSprintJobs(ctx context.Context) (rolled, created int, err error) {
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM board WHERE kind='scrum' AND (auto_rollover OR auto_sprints>0)`)
	if err != nil {
		return 0, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var firstErr error
	for _, id := range ids {
		b, _, err := s.boardRow(ctx, s.DB, id)
		if err != nil {
			continue
		}
		if b.AutoRollover {
			ar, err := s.DB.Query(ctx, `SELECT id::text FROM sprint WHERE board_id::text=$1 AND state='active' AND ends_at<=$2 ORDER BY starts_at`, id, s.Now())
			if err != nil {
				continue
			}
			var due []string
			for ar.Next() {
				var sid string
				ar.Scan(&sid)
				due = append(due, sid)
			}
			ar.Close()
			for _, sid := range due {
				if _, err := s.CompleteSprint(ctx, SystemActor, sid, b.Rollover, false); err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				rolled++
				var next string // the next sprint starts as the old one ends
				s.DB.QueryRow(ctx, `SELECT id::text FROM sprint WHERE board_id::text=$1 AND state='future' ORDER BY starts_at LIMIT 1`, id).Scan(&next)
				if next == "" && b.Rollover == "backlog" {
					s.EnsureSprints(ctx, id)
					s.DB.QueryRow(ctx, `SELECT id::text FROM sprint WHERE board_id::text=$1 AND state='future' ORDER BY starts_at LIMIT 1`, id).Scan(&next)
				}
				if next != "" {
					if _, err := s.StartSprint(ctx, SystemActor, next); err != nil && firstErr == nil {
						firstErr = err
					}
				}
			}
		}
		if b.AutoSprints > 0 {
			n, err := s.EnsureSprints(ctx, id)
			created += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return rolled, created, firstErr
}
