package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// QuickFilter is a named TQL clause toggled as a chip on the board (SPEC 6).
type QuickFilter struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

type Board struct {
	ID           string        `json:"id"`
	Project      string        `json:"project"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"` // scrum or kanban
	Filter       string        `json:"filter"`
	Estimation   string        `json:"estimation"` // points, time or count
	Swimlane     string        `json:"swimlane"`
	QuickFilters []QuickFilter `json:"quick_filters"`
	SprintWeeks  int           `json:"sprint_weeks"`
	DoneDays     int           `json:"done_days"`
}

type Column struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Statuses []string `json:"statuses"`
	MinWIP   *int     `json:"min_wip,omitempty"`
	MaxWIP   *int     `json:"max_wip,omitempty"`
}

type Card struct {
	Key          string   `json:"key"`
	Summary      string   `json:"summary"`
	Type         string   `json:"type"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	Assignee     string   `json:"assignee,omitempty"`
	Labels       []string `json:"labels"`
	Rank         string   `json:"rank"`
	NeedsInput   bool     `json:"needs_input,omitempty"`
	Subtasks     int      `json:"subtasks,omitempty"`
	SubtasksDone int      `json:"subtasks_done,omitempty"`
	Parent       string   `json:"parent,omitempty"`
}

type ColumnView struct {
	Column
	Cards  []Card `json:"cards"`
	Count  int    `json:"count"`
	Breach string `json:"breach,omitempty"` // "over" the max or "under" the min
}

type Lane struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

type BoardView struct {
	Board    Board        `json:"board"`
	Columns  []ColumnView `json:"columns"`
	Unmapped []Card       `json:"unmapped"` // cards whose status is on no column: shown so nothing disappears
	Lanes    []Lane       `json:"lanes,omitempty"`
	Role     string       `json:"role"`
}

type ColumnIn struct {
	Name     string   `json:"name"`
	Statuses []string `json:"statuses"`
	MinWIP   *int     `json:"min_wip"`
	MaxWIP   *int     `json:"max_wip"`
}

type BoardPatch struct {
	Name         *string        `json:"name"`
	Filter       *string        `json:"filter"`
	Estimation   *string        `json:"estimation"`
	Swimlane     *string        `json:"swimlane"`
	QuickFilters *[]QuickFilter `json:"quick_filters"`
	SprintWeeks  *int           `json:"sprint_weeks"`
	DoneDays     *int           `json:"done_days"`
}

func (s *Service) boardRow(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (Board, string, error) {
	var b Board
	var pid string
	var qf []byte
	err := q.QueryRow(ctx, `SELECT b.id,b.project_id,p.key,b.name,b.kind,b.filter,b.estimation,b.swimlane,b.quick_filters,b.sprint_weeks,b.done_days FROM board b JOIN project p ON p.id=b.project_id WHERE b.id::text=$1`, id).
		Scan(&b.ID, &pid, &b.Project, &b.Name, &b.Kind, &b.Filter, &b.Estimation, &b.Swimlane, &qf, &b.SprintWeeks, &b.DoneDays)
	if err == pgx.ErrNoRows || (err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")) {
		return b, "", ErrNotFound
	}
	if err != nil {
		return b, "", err
	}
	json.Unmarshal(qf, &b.QuickFilters)
	if b.QuickFilters == nil {
		b.QuickFilters = []QuickFilter{}
	}
	return b, pid, nil
}

// BoardAccess is boardAccess for the API layer.
func (s *Service) BoardAccess(ctx context.Context, a Actor, id string) (Board, string, string, error) {
	return s.boardAccess(ctx, a, id)
}

// boardAccess loads a board and the caller's role; people with no role in the project learn nothing.
func (s *Service) boardAccess(ctx context.Context, a Actor, id string) (Board, string, string, error) {
	b, pid, err := s.boardRow(ctx, s.DB, id)
	if err != nil {
		return b, "", "", err
	}
	role := s.role(ctx, a, pid)
	if role == "" {
		return b, pid, "", ErrNotFound
	}
	return b, pid, role, nil
}

func validBoardFields(b Board) error {
	if strings.TrimSpace(b.Name) == "" || len(b.Name) > 100 {
		return invalid("a board needs a name of up to 100 characters")
	}
	if len(b.QuickFilters) > 20 {
		return invalid("at most 20 quick filters")
	}
	for _, q := range b.QuickFilters {
		if strings.TrimSpace(q.Name) == "" || strings.TrimSpace(q.Query) == "" {
			return invalid("a quick filter needs a name and a query")
		}
	}
	return nil
}

// CreateBoard makes a board with one column per workflow status (in workflow order). Project administrators only.
func (s *Service) CreateBoard(ctx context.Context, a Actor, projectKey, name, kind string) (Board, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return Board{}, err
	}
	role := s.role(ctx, a, pid)
	if role == "" {
		return Board{}, ErrNotFound
	}
	if role != "admin" {
		return Board{}, ErrForbidden
	}
	if kind != "scrum" && kind != "kanban" {
		return Board{}, invalid("a board is scrum or kanban")
	}
	b := Board{Name: strings.TrimSpace(name), Kind: kind, Project: strings.ToUpper(projectKey), Estimation: "count", Swimlane: "none", SprintWeeks: 1, DoneDays: 14, QuickFilters: []QuickFilter{}}
	if err := validBoardFields(b); err != nil {
		return b, err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO board(project_id,name,kind,created_by) VALUES($1,$2,$3,$4) RETURNING id`, pid, b.Name, kind, a.ID).Scan(&b.ID); err != nil {
			if strings.Contains(err.Error(), "board_project_id_name_key") {
				return invalid("this project already has a board called %q", b.Name)
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT st.id,st.name FROM status st JOIN project p ON p.workflow_id=st.workflow_id WHERE p.id=$1 ORDER BY st.position,st.name`, pid)
		if err != nil {
			return err
		}
		type st struct{ id, name string }
		var sts []st
		for rows.Next() {
			var x st
			rows.Scan(&x.id, &x.name)
			sts = append(sts, x)
		}
		rows.Close()
		for i, x := range sts {
			var cid string
			if err := tx.QueryRow(ctx, `INSERT INTO board_column(board_id,name,position) VALUES($1,$2,$3) RETURNING id`, b.ID, x.name, i).Scan(&cid); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO board_column_status(column_id,board_id,status_id) VALUES($1,$2,$3)`, cid, b.ID, x.id); err != nil {
				return err
			}
		}
		return outbox(ctx, tx, "board:"+b.ID, "board.created", map[string]any{"board": b.ID, "project": b.Project, "actor": a.Label, "actor_id": a.ID})
	})
	return b, err
}

func (s *Service) ListBoards(ctx context.Context, a Actor, projectKey string) ([]Board, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	if s.role(ctx, a, pid) == "" {
		return nil, ErrNotFound
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM board WHERE project_id=$1 ORDER BY name`, pid)
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
	out := []Board{}
	for _, id := range ids {
		b, _, err := s.boardRow(ctx, s.DB, id)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// UpdateBoard changes the board's settings (project administrators).
func (s *Service) UpdateBoard(ctx context.Context, a Actor, id string, p BoardPatch) (Board, error) {
	b, _, role, err := s.boardAccess(ctx, a, id)
	if err != nil {
		return b, err
	}
	if role != "admin" {
		return b, ErrForbidden
	}
	if p.Name != nil {
		b.Name = strings.TrimSpace(*p.Name)
	}
	if p.Filter != nil {
		b.Filter = strings.TrimSpace(*p.Filter)
	}
	if p.Estimation != nil {
		switch *p.Estimation {
		case "points", "time", "count":
			b.Estimation = *p.Estimation
		default:
			return b, invalid("estimation is points, time or count")
		}
	}
	if p.Swimlane != nil {
		switch *p.Swimlane {
		case "none", "assignee", "epic", "priority", "stories":
			b.Swimlane = *p.Swimlane
		default:
			return b, invalid("swimlanes are none, assignee, epic, priority or stories")
		}
	}
	if p.QuickFilters != nil {
		b.QuickFilters = *p.QuickFilters
	}
	if p.SprintWeeks != nil {
		if *p.SprintWeeks < 1 || *p.SprintWeeks > 8 {
			return b, invalid("a sprint is 1 to 8 weeks")
		}
		b.SprintWeeks = *p.SprintWeeks
	}
	if p.DoneDays != nil {
		if *p.DoneDays < 0 || *p.DoneDays > 365 {
			return b, invalid("done_days is 0 to 365")
		}
		b.DoneDays = *p.DoneDays
	}
	if err := validBoardFields(b); err != nil {
		return b, err
	}
	qf, _ := json.Marshal(b.QuickFilters)
	_, err = s.DB.Exec(ctx, `UPDATE board SET name=$2,filter=$3,estimation=$4,swimlane=$5,quick_filters=$6,sprint_weeks=$7,done_days=$8 WHERE id::text=$1`, id, b.Name, b.Filter, b.Estimation, b.Swimlane, qf, b.SprintWeeks, b.DoneDays)
	if err != nil && strings.Contains(err.Error(), "board_project_id_name_key") {
		return b, invalid("this project already has a board called %q", b.Name)
	}
	return b, err
}

// SetColumns replaces the board's columns: add, rename, reorder and delete in one call. A status may be on at most one
// column; statuses on no column show in the "unmapped" strip so no card disappears.
func (s *Service) SetColumns(ctx context.Context, a Actor, id string, cols []ColumnIn) error {
	b, pid, role, err := s.boardAccess(ctx, a, id)
	if err != nil {
		return err
	}
	if role != "admin" {
		return ErrForbidden
	}
	if len(cols) == 0 || len(cols) > 20 {
		return invalid("a board has 1 to 20 columns")
	}
	names := map[string]bool{}
	used := map[string]string{}
	for _, c := range cols {
		n := strings.TrimSpace(c.Name)
		if n == "" || len(n) > 50 || names[strings.ToLower(n)] {
			return invalid("column names must be unique, non-empty and up to 50 characters")
		}
		names[strings.ToLower(n)] = true
		if c.MinWIP != nil && *c.MinWIP < 0 || c.MaxWIP != nil && *c.MaxWIP < 1 || c.MinWIP != nil && c.MaxWIP != nil && *c.MinWIP > *c.MaxWIP {
			return invalid("column %q: the minimum cannot exceed the maximum, and the maximum is at least 1", n)
		}
		for _, st := range c.Statuses {
			if other, dup := used[st]; dup {
				return invalid("status %q is on both %q and %q", st, other, n)
			}
			used[st] = n
		}
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		ids := map[string]string{}
		rows, err := tx.Query(ctx, `SELECT st.name,st.id::text FROM status st JOIN project p ON p.workflow_id=st.workflow_id WHERE p.id=$1`, pid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n, i string
			rows.Scan(&n, &i)
			ids[n] = i
		}
		rows.Close()
		for st := range used {
			if ids[st] == "" {
				return invalid("the workflow has no status %q", st)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM board_column WHERE board_id=$1`, b.ID); err != nil { // cascades to the mapping
			return err
		}
		for i, c := range cols {
			var cid string
			if err := tx.QueryRow(ctx, `INSERT INTO board_column(board_id,name,position,min_wip,max_wip) VALUES($1,$2,$3,$4,$5) RETURNING id`, b.ID, strings.TrimSpace(c.Name), i, c.MinWIP, c.MaxWIP).Scan(&cid); err != nil {
				return err
			}
			for _, st := range c.Statuses {
				if _, err := tx.Exec(ctx, `INSERT INTO board_column_status(column_id,board_id,status_id) VALUES($1,$2,$3)`, cid, b.ID, ids[st]); err != nil {
					return err
				}
			}
		}
		return outbox(ctx, tx, "board:"+b.ID, "board.updated", map[string]any{"board": b.ID, "actor": a.Label, "actor_id": a.ID})
	})
}

func (s *Service) columns(ctx context.Context, boardID string) ([]Column, map[string]int, error) {
	rows, err := s.DB.Query(ctx, `SELECT c.id::text,c.name,c.min_wip,c.max_wip FROM board_column c WHERE c.board_id::text=$1 ORDER BY c.position`, boardID)
	if err != nil {
		return nil, nil, err
	}
	var cols []Column
	for rows.Next() {
		var c Column
		rows.Scan(&c.ID, &c.Name, &c.MinWIP, &c.MaxWIP)
		c.Statuses = []string{}
		cols = append(cols, c)
	}
	rows.Close()
	byStatus := map[string]int{} // status id -> column index
	idx := map[string]int{}
	for i, c := range cols {
		idx[c.ID] = i
	}
	mr, err := s.DB.Query(ctx, `SELECT m.column_id::text,m.status_id::text,st.name FROM board_column_status m JOIN status st ON st.id=m.status_id WHERE m.board_id::text=$1 ORDER BY st.position`, boardID)
	if err != nil {
		return nil, nil, err
	}
	for mr.Next() {
		var cid, sid, name string
		mr.Scan(&cid, &sid, &name)
		i := idx[cid]
		cols[i].Statuses = append(cols[i].Statuses, name)
		byStatus[sid] = i
	}
	mr.Close()
	return cols, byStatus, nil
}

// GetBoard returns the board with its cards. restrict, when not nil, is the set of issue keys that the board filter and
// any chosen quick filters let through (the API layer evaluates the TQL; nil means no restriction).
func (s *Service) GetBoard(ctx context.Context, a Actor, id string, restrict []string) (BoardView, error) {
	b, pid, role, err := s.boardAccess(ctx, a, id)
	if err != nil {
		return BoardView{}, err
	}
	cols, byStatus, err := s.columns(ctx, b.ID)
	if err != nil {
		return BoardView{}, err
	}
	v := BoardView{Board: b, Role: role, Columns: make([]ColumnView, len(cols)), Unmapped: []Card{}}
	for i, c := range cols {
		v.Columns[i] = ColumnView{Column: c, Cards: []Card{}}
	}
	q := `SELECT i.key,i.summary,t.name,st.name,i.priority,COALESCE(asg.name,''),i.labels,COALESCE(br.rank,i.rank),i.status_id::text,i.input_requested_at IS NOT NULL,
			COALESCE(sub.total,0),COALESCE(sub.done,0),COALESCE(par.key,'')
		FROM issue i JOIN issue_type t ON t.id=i.issue_type_id JOIN status st ON st.id=i.status_id
		LEFT JOIN app_user asg ON asg.id=i.assignee_id
		LEFT JOIN issue par ON par.id=i.parent_id
		LEFT JOIN board_rank br ON br.board_id=$1::uuid AND br.issue_id=i.id
		LEFT JOIN LATERAL (SELECT count(*) AS total, count(*) FILTER (WHERE s2.category='done') AS done FROM issue c JOIN status s2 ON s2.id=c.status_id WHERE c.parent_id=i.id AND c.deleted_at IS NULL) sub ON true
		WHERE i.project_id=$2 AND i.deleted_at IS NULL AND (st.category<>'done' OR i.resolved_at IS NULL OR i.resolved_at > $3)`
	args := []any{b.ID, pid, s.Now().AddDate(0, 0, -b.DoneDays)}
	if restrict != nil {
		q += ` AND i.key = ANY($4)`
		args = append(args, restrict)
	}
	q += ` ORDER BY COALESCE(br.rank,i.rank) COLLATE "C", i.number`
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Card
		var sid string
		if err := rows.Scan(&c.Key, &c.Summary, &c.Type, &c.Status, &c.Priority, &c.Assignee, &c.Labels, &c.Rank, &sid, &c.NeedsInput, &c.Subtasks, &c.SubtasksDone, &c.Parent); err != nil {
			return v, err
		}
		if c.Labels == nil {
			c.Labels = []string{}
		}
		if i, ok := byStatus[sid]; ok {
			v.Columns[i].Cards = append(v.Columns[i].Cards, c)
		} else {
			v.Unmapped = append(v.Unmapped, c)
		}
	}
	for i := range v.Columns {
		c := &v.Columns[i]
		c.Count = len(c.Cards)
		if c.MaxWIP != nil && c.Count > *c.MaxWIP {
			c.Breach = "over"
		} else if c.MinWIP != nil && c.Count < *c.MinWIP {
			c.Breach = "under"
		}
	}
	v.Lanes = lanes(b.Swimlane, v)
	return v, rows.Err()
}

func lanes(kind string, v BoardView) []Lane {
	if kind == "none" || kind == "" {
		return nil
	}
	order := []string{}
	m := map[string][]string{}
	add := func(name, key string) {
		if _, ok := m[name]; !ok {
			order = append(order, name)
		}
		m[name] = append(m[name], key)
	}
	for _, col := range v.Columns {
		for _, c := range col.Cards {
			switch kind {
			case "assignee":
				n := c.Assignee
				if n == "" {
					n = "Unassigned"
				}
				add(n, c.Key)
			case "priority":
				add(c.Priority, c.Key)
			case "epic", "stories":
				if c.Parent != "" {
					add(c.Parent, c.Key)
				} else {
					add("Other", c.Key)
				}
			}
		}
	}
	out := []Lane{}
	for _, n := range order {
		out = append(out, Lane{Name: n, Keys: m[n]})
	}
	return out
}

type MoveIn struct {
	Column string `json:"column"` // id or name
	After  string `json:"after"`  // place the card right after this card (key)
	Before string `json:"before"` // or right before this one
}

type MoveResult struct {
	Issue         Issue  `json:"issue"`
	Transitioned  bool   `json:"transitioned"`
	TransitionVia string `json:"transition,omitempty"`
	Rank          string `json:"rank"`
}

// MoveCard drags a card: when the target column does not hold the card's status, the move is a workflow transition
// (conditions, gates and validators apply, and a blocked move says why); then the card is ranked in the column.
func (s *Service) MoveCard(ctx context.Context, a Actor, boardID, key string, in MoveIn) (MoveResult, error) {
	var res MoveResult
	b, pid, role, err := s.boardAccess(ctx, a, boardID)
	if err != nil {
		return res, err
	}
	if !canWrite(role) {
		return res, ErrForbidden
	}
	is, irole, err := s.access(ctx, a, key)
	if err != nil {
		return res, err
	}
	if is.Project != b.Project || irole == "" {
		return res, invalid("%s is not on this board", is.Key)
	}
	cols, byStatus, err := s.columns(ctx, b.ID)
	if err != nil {
		return res, err
	}
	target := -1
	for i, c := range cols {
		if c.ID == in.Column || strings.EqualFold(c.Name, in.Column) {
			target = i
		}
	}
	if target < 0 {
		return res, invalid("this board has no column %q", in.Column)
	}
	var curStatus string
	if err := s.DB.QueryRow(ctx, `SELECT status_id::text FROM issue WHERE id=$1`, is.ID).Scan(&curStatus); err != nil {
		return res, err
	}
	if cur, ok := byStatus[curStatus]; !ok || cur != target {
		// find a transition from the current status into one of the target column's statuses
		var tid, via string
		err := s.DB.QueryRow(ctx, `SELECT t.id::text,t.name FROM transition t JOIN status st ON st.id=t.to_status
			JOIN board_column_status m ON m.status_id=st.id AND m.board_id=$1::uuid AND m.column_id::text=$2
			WHERE t.workflow_id=(SELECT workflow_id FROM project WHERE id=$3) AND (t.from_status IS NULL OR t.from_status::text=$4)
			ORDER BY st.position, t.name LIMIT 1`, b.ID, cols[target].ID, pid, curStatus).Scan(&tid, &via)
		if err == pgx.ErrNoRows {
			var avail []string
			ar, _ := s.DB.Query(ctx, `SELECT DISTINCT st.name FROM transition t JOIN status st ON st.id=t.to_status WHERE t.workflow_id=(SELECT workflow_id FROM project WHERE id=$1) AND (t.from_status IS NULL OR t.from_status::text=$2)`, pid, curStatus)
			for ar.Next() {
				var n string
				ar.Scan(&n)
				avail = append(avail, n)
			}
			ar.Close()
			return res, invalid("%s cannot move to %q: the workflow has no transition from %q into it (it can go to: %s)", is.Key, cols[target].Name, is.Status, strings.Join(avail, ", "))
		}
		if err != nil {
			return res, err
		}
		if res.Issue, err = s.Transition(ctx, a, is.Key, tid, nil); err != nil {
			return res, err
		}
		res.Transitioned, res.TransitionVia = true, via
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return res, perr
	}
	defer endWrite()
	err = s.tx(ctx, func(tx pgx.Tx) error {
		// give every card in the column a board rank first, so neighbours always have one to compare with
		if _, err := tx.Exec(ctx, `INSERT INTO board_rank(board_id,issue_id,rank)
			SELECT $1::uuid,i.id,i.rank FROM issue i JOIN board_column_status m ON m.status_id=i.status_id AND m.column_id::text=$2
			WHERE i.project_id=$3 AND i.deleted_at IS NULL ON CONFLICT DO NOTHING`, b.ID, cols[target].ID, pid); err != nil {
			return err
		}
		rankOf := func(k string) (string, error) {
			var r string
			err := tx.QueryRow(ctx, `SELECT br.rank FROM board_rank br JOIN issue i ON i.id=br.issue_id JOIN board_column_status m ON m.status_id=i.status_id AND m.column_id::text=$3
				WHERE br.board_id=$1::uuid AND i.key=$2 AND i.id<>$4`, b.ID, strings.ToUpper(k), cols[target].ID, is.ID).Scan(&r)
			if err == pgx.ErrNoRows {
				return "", invalid("%s is not in column %q", strings.ToUpper(k), cols[target].Name)
			}
			return r, err
		}
		neighbour := func(sql string, arg string) string {
			var r *string
			tx.QueryRow(ctx, sql, b.ID, cols[target].ID, is.ID, arg).Scan(&r)
			if r == nil {
				return ""
			}
			return *r
		}
		const colRank = `SELECT %s(br.rank COLLATE "C") FROM board_rank br JOIN issue i ON i.id=br.issue_id JOIN board_column_status m ON m.status_id=i.status_id AND m.column_id::text=$2
			WHERE br.board_id=$1::uuid AND i.deleted_at IS NULL AND br.issue_id<>$3 %s`
		var lo, hi string
		switch {
		case in.After != "":
			r, err := rankOf(in.After)
			if err != nil {
				return err
			}
			lo, hi = r, neighbour(fmt.Sprintf(colRank, "min", `AND br.rank COLLATE "C" > $4`), r)
		case in.Before != "":
			r, err := rankOf(in.Before)
			if err != nil {
				return err
			}
			hi, lo = r, neighbour(fmt.Sprintf(colRank, "max", `AND br.rank COLLATE "C" < $4`), r)
		default: // the end of the column
			lo = neighbour(fmt.Sprintf(colRank, "max", `AND $4=$4`), "")
		}
		r, ok := rankBetween(lo, hi)
		if !ok || len(r) > 40 { // the string grew long from repeated inserts in one spot: spread the whole column out again
			ids, err := tx.Query(ctx, `SELECT br.issue_id FROM board_rank br JOIN issue i ON i.id=br.issue_id JOIN board_column_status m ON m.status_id=i.status_id AND m.column_id::text=$2
				WHERE br.board_id=$1::uuid AND br.issue_id<>$3 ORDER BY br.rank COLLATE "C"`, b.ID, cols[target].ID, is.ID)
			if err != nil {
				return err
			}
			var order []string
			for ids.Next() {
				var id string
				ids.Scan(&id)
				order = append(order, id)
			}
			ids.Close()
			pos := len(order)
			if in.After != "" || in.Before != "" {
				for i, id := range order { // find where the neighbour sits
					var k string
					tx.QueryRow(ctx, `SELECT key FROM issue WHERE id=$1`, id).Scan(&k)
					if (in.After != "" && strings.EqualFold(k, in.After)) || (in.Before != "" && strings.EqualFold(k, in.Before)) {
						pos = i
						if in.After != "" {
							pos = i + 1
						}
					}
				}
			}
			order = append(order[:pos], append([]string{is.ID}, order[pos:]...)...)
			for i, id := range order {
				if _, err := tx.Exec(ctx, `INSERT INTO board_rank(board_id,issue_id,rank) VALUES($1::uuid,$2,$3) ON CONFLICT (board_id,issue_id) DO UPDATE SET rank=$3`, b.ID, id, spacedRank(i)); err != nil {
					return err
				}
			}
			r = spacedRank(pos)
		} else if _, err := tx.Exec(ctx, `INSERT INTO board_rank(board_id,issue_id,rank) VALUES($1::uuid,$2,$3) ON CONFLICT (board_id,issue_id) DO UPDATE SET rank=$3`, b.ID, is.ID, r); err != nil {
			return err
		}
		res.Rank = r
		return outbox(ctx, tx, "board:"+b.ID, "board.ranked", map[string]any{"board": b.ID, "key": is.Key, "column": cols[target].Name, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return res, err
	}
	if res.Issue.Key == "" {
		res.Issue, _ = s.getIssue(ctx, is.Key)
	}
	s.touchLease(ctx, a, is.ID)
	s.emit(Event{Type: "issue.ranked", Issue: is.Key, Actor: a.Label, ActorID: a.ID})
	return res, nil
}
