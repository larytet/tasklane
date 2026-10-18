package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/search"
	"github.com/larytet/tasklane/internal/search/tql"
)

// Dashboards (SPEC 11): a layout of gadgets, each over a query or a saved filter. The one rule that matters: **a gadget's data is
// computed for whoever is looking**, with their own visibility, never for the owner. Sharing a dashboard shares the layout and the
// queries, not the data: two people looking at the same dashboard see counts of the issues each of them may see.

const maxGadgets = 20

type Gadget struct {
	ID       string         `json:"id"`
	Col      int            `json:"col"`
	Position int            `json:"position"`
	Kind     string         `json:"kind"`
	Title    string         `json:"title"`
	Config   map[string]any `json:"config"`
}

type Dashboard struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Columns        int       `json:"columns"`
	Shared         string    `json:"shared"`
	RefreshSeconds int       `json:"refresh_seconds"`
	Owner          string    `json:"owner"`
	Mine           bool      `json:"mine"`
	CanEdit        bool      `json:"can_edit"`
	Favourite      bool      `json:"favourite"`
	Default        bool      `json:"default"`
	Gadgets        []Gadget  `json:"gadgets,omitempty"`
	Updated        time.Time `json:"updated_at"`
}

type DashboardIn struct {
	Name           string `json:"name"`
	Columns        int    `json:"columns"`
	Shared         string `json:"shared"`
	RefreshSeconds int    `json:"refresh_seconds"`
}

type GadgetIn struct {
	Kind     string         `json:"kind"`
	Title    string         `json:"title"`
	Col      int            `json:"col"`
	Position *int           `json:"position,omitempty"`
	Config   map[string]any `json:"config"`
}

// GadgetKinds lists what a dashboard can show (the kinds the spec lists that are not here: heat map, sprint health, release burndown,
// time since).
var GadgetKinds = []string{"filter_results", "assigned_to_me", "activity_stream", "pie", "stats_2d", "created_vs_resolved", "average_age", "workload", "sprint_burndown", "text", "iframe"}

func (in *DashboardIn) clean() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return invalid("a dashboard needs a name of up to 100 characters")
	}
	if in.Columns == 0 {
		in.Columns = 2
	}
	if in.Columns < 1 || in.Columns > 3 {
		return invalid("a dashboard has 1, 2 or 3 columns")
	}
	if in.Shared == "" {
		in.Shared = "private"
	}
	if in.Shared != "private" && in.Shared != "org" {
		return invalid("shared is private or org")
	}
	if in.RefreshSeconds != 0 && (in.RefreshSeconds < 30 || in.RefreshSeconds > 3600) {
		return invalid("auto-refresh is off (0) or every 30 to 3600 seconds")
	}
	return nil
}

type dashRow struct {
	id, org, owner, name, shared string
	columns, refresh             int
	updated                      time.Time
}

// dashFor loads a dashboard the actor may see and says whether they may change it.
func (s *Service) dashFor(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, a Actor, id string) (dashRow, bool, error) {
	var d dashRow
	err := q.QueryRow(ctx, `SELECT id::text,org_id::text,owner_id::text,name,shared,columns,refresh_seconds,updated_at FROM dashboard WHERE id::text=$1`, id).
		Scan(&d.id, &d.org, &d.owner, &d.name, &d.shared, &d.columns, &d.refresh, &d.updated)
	if err != nil {
		return d, false, ErrNotFound
	}
	visible := d.owner == a.ID || (d.shared == "org" && d.org == a.OrgID()) || a.Admin
	if !visible {
		return d, false, ErrNotFound // a dashboard one cannot see does not exist
	}
	edit := d.owner == a.ID || a.Admin || (a.OrgAdmin && d.org == a.OrgID())
	return d, edit, nil
}

func (s *Service) loadGadgets(ctx context.Context, id string) ([]Gadget, error) {
	rows, err := s.DB.Query(ctx, `SELECT id::text,col,position,kind,title,config FROM dashboard_gadget WHERE dashboard_id::text=$1 ORDER BY col,position,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Gadget{}
	for rows.Next() {
		var g Gadget
		var raw []byte
		if err := rows.Scan(&g.ID, &g.Col, &g.Position, &g.Kind, &g.Title, &raw); err != nil {
			return nil, err
		}
		json.Unmarshal(raw, &g.Config)
		if g.Config == nil {
			g.Config = map[string]any{}
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) ListDashboards(ctx context.Context, a Actor) ([]Dashboard, error) {
	rows, err := s.DB.Query(ctx, `SELECT d.id::text,d.name,d.columns,d.shared,d.refresh_seconds,u.name,d.owner_id::text,d.updated_at,
		EXISTS(SELECT 1 FROM dashboard_favourite f WHERE f.dashboard_id=d.id AND f.user_id::text=$1),
		EXISTS(SELECT 1 FROM user_default_dashboard x WHERE x.dashboard_id=d.id AND x.user_id::text=$1)
		FROM dashboard d JOIN app_user u ON u.id=d.owner_id
		WHERE d.owner_id::text=$1 OR (d.shared='org' AND d.org_id::text=$2) ORDER BY 9 DESC, lower(d.name)`, a.ID, a.OrgID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dashboard{}
	for rows.Next() {
		var d Dashboard
		var owner string
		if err := rows.Scan(&d.ID, &d.Name, &d.Columns, &d.Shared, &d.RefreshSeconds, &d.Owner, &owner, &d.Updated, &d.Favourite, &d.Default); err != nil {
			return nil, err
		}
		d.Mine = owner == a.ID
		d.CanEdit = d.Mine || a.Admin || (a.OrgAdmin && d.Shared == "org")
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Service) GetDashboard(ctx context.Context, a Actor, id string) (Dashboard, error) {
	r, edit, err := s.dashFor(ctx, s.DB, a, id)
	if err != nil {
		return Dashboard{}, err
	}
	d := Dashboard{ID: r.id, Name: r.name, Columns: r.columns, Shared: r.shared, RefreshSeconds: r.refresh, Mine: r.owner == a.ID, CanEdit: edit, Updated: r.updated}
	s.DB.QueryRow(ctx, `SELECT u.name FROM app_user u WHERE u.id::text=$1`, r.owner).Scan(&d.Owner)
	s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dashboard_favourite WHERE dashboard_id::text=$1 AND user_id::text=$2), EXISTS(SELECT 1 FROM user_default_dashboard WHERE dashboard_id::text=$1 AND user_id::text=$2)`, id, a.ID).Scan(&d.Favourite, &d.Default)
	d.Gadgets, err = s.loadGadgets(ctx, id)
	return d, err
}

func (s *Service) CreateDashboard(ctx context.Context, a Actor, in DashboardIn) (Dashboard, error) {
	if err := in.clean(); err != nil {
		return Dashboard{}, err
	}
	var id string
	if err := s.DB.QueryRow(ctx, `INSERT INTO dashboard(org_id,owner_id,name,columns,shared,refresh_seconds) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text`, a.OrgID(), a.ID, in.Name, in.Columns, in.Shared, in.RefreshSeconds).Scan(&id); err != nil {
		return Dashboard{}, err
	}
	return s.GetDashboard(ctx, a, id)
}

// sharable checks that everything a dashboard shows can be seen by the people it is shared with: an organization-wide dashboard may
// only use organization-wide filters.
func (s *Service) sharable(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, dashID string) error {
	rows, err := q.Query(ctx, `SELECT DISTINCT f.name FROM dashboard_gadget g JOIN saved_filter f ON f.id::text=g.config->>'filter_id' WHERE g.dashboard_id::text=$1 AND f.shared<>'org'`, dashID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	if len(names) > 0 {
		return &ConflictError{Msg: "these filters are private, so the dashboard cannot be shared with the organization: " + strings.Join(names, ", ")}
	}
	return nil
}

func (s *Service) UpdateDashboard(ctx context.Context, a Actor, id string, in DashboardIn) error {
	if err := in.clean(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		d, edit, err := s.dashFor(ctx, tx, a, id)
		if err != nil {
			return err
		}
		if !edit {
			return ErrForbidden
		}
		if in.Shared == "org" {
			if err := s.sharable(ctx, tx, id); err != nil {
				return err
			}
		}
		_ = d
		_, err = tx.Exec(ctx, `UPDATE dashboard SET name=$2,columns=$3,shared=$4,refresh_seconds=$5,updated_at=now() WHERE id::text=$1`, id, in.Name, in.Columns, in.Shared, in.RefreshSeconds)
		if err == nil && in.Shared == "private" { // people it was shared with lose it: their favourites and defaults go with it
			tx.Exec(ctx, `DELETE FROM dashboard_favourite WHERE dashboard_id::text=$1 AND user_id::text<>$2`, id, d.owner)
			tx.Exec(ctx, `DELETE FROM user_default_dashboard WHERE dashboard_id::text=$1 AND user_id::text<>$2`, id, d.owner)
		}
		// gadgets that were placed in a column that no longer exists move to the last one
		tx.Exec(ctx, `UPDATE dashboard_gadget SET col=$2-1 WHERE dashboard_id::text=$1 AND col>=$2`, id, in.Columns)
		return err
	})
}

func (s *Service) DeleteDashboard(ctx context.Context, a Actor, id string) error {
	_, edit, err := s.dashFor(ctx, s.DB, a, id)
	if err != nil {
		return err
	}
	if !edit {
		return ErrForbidden
	}
	_, err = s.DB.Exec(ctx, `DELETE FROM dashboard WHERE id::text=$1`, id)
	return err
}

// CloneDashboard makes the actor a private copy (the way to change a dashboard that someone else shares).
func (s *Service) CloneDashboard(ctx context.Context, a Actor, id string) (Dashboard, error) {
	r, _, err := s.dashFor(ctx, s.DB, a, id)
	if err != nil {
		return Dashboard{}, err
	}
	nd, err := s.CreateDashboard(ctx, a, DashboardIn{Name: r.name + " (copy)", Columns: r.columns, RefreshSeconds: r.refresh})
	if err != nil {
		return Dashboard{}, err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO dashboard_gadget(dashboard_id,col,position,kind,title,config) SELECT $2::uuid,col,position,kind,title,config FROM dashboard_gadget WHERE dashboard_id::text=$1`, id, nd.ID); err != nil {
		return Dashboard{}, err
	}
	return s.GetDashboard(ctx, a, nd.ID)
}

func (s *Service) SetFavourite(ctx context.Context, a Actor, id string, on bool) error {
	if _, _, err := s.dashFor(ctx, s.DB, a, id); err != nil {
		return err
	}
	if on {
		_, err := s.DB.Exec(ctx, `INSERT INTO dashboard_favourite(user_id,dashboard_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, a.ID, id)
		return err
	}
	_, err := s.DB.Exec(ctx, `DELETE FROM dashboard_favourite WHERE user_id::text=$1 AND dashboard_id::text=$2`, a.ID, id)
	return err
}

// SetDefaultDashboard chooses the dashboard a person opens by default (empty id clears it).
func (s *Service) SetDefaultDashboard(ctx context.Context, a Actor, id string) error {
	if id == "" {
		_, err := s.DB.Exec(ctx, `DELETE FROM user_default_dashboard WHERE user_id::text=$1`, a.ID)
		return err
	}
	if _, _, err := s.dashFor(ctx, s.DB, a, id); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO user_default_dashboard(user_id,dashboard_id) VALUES($1,$2) ON CONFLICT (user_id) DO UPDATE SET dashboard_id=$2`, a.ID, id)
	return err
}

// ---- gadgets

func cfgStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return strings.TrimSpace(s)
}

func cfgInt(m map[string]any, k string, def, lo, hi int) (int, error) {
	v, ok := m[k]
	if !ok || v == nil {
		return def, nil
	}
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	default:
		return 0, invalid("%s is a whole number from %d to %d", k, lo, hi)
	}
	if f != float64(int(f)) || int(f) < lo || int(f) > hi {
		return 0, invalid("%s is a whole number from %d to %d", k, lo, hi)
	}
	return int(f), nil
}

func (s *Service) validateGadget(ctx context.Context, a Actor, shared string, kind string, cfg map[string]any) (map[string]any, error) {
	known := false
	for _, k := range GadgetKinds {
		known = known || k == kind
	}
	if !known {
		return nil, invalid("unknown gadget %q (%s)", kind, strings.Join(GadgetKinds, ", "))
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	out := map[string]any{}
	switch kind {
	case "filter_results", "activity_stream", "pie", "stats_2d", "created_vs_resolved", "average_age":
		fid, q := cfgStr(cfg, "filter_id"), cfgStr(cfg, "q")
		if fid != "" && q != "" {
			return nil, invalid("a gadget uses a saved filter or a query, not both")
		}
		if fid != "" {
			f, _, err := s.filterFor(ctx, a, fid)
			if err != nil {
				return nil, invalid("that filter is not available to you")
			}
			if shared == "org" && f.Shared != "org" {
				return nil, invalid("the dashboard is shared with the organization, so it can only use organization-wide filters (%q is private)", f.Name)
			}
			out["filter_id"] = fid
		}
		if q != "" {
			if _, err := tql.Parse(q); err != nil {
				return nil, invalid("the query is not valid: %v", err)
			}
			out["q"] = q
		}
	}
	switch kind {
	case "filter_results", "activity_stream", "assigned_to_me":
		hi := 50
		if kind == "activity_stream" {
			hi = 30
		}
		n, err := cfgInt(cfg, "limit", 10, 1, hi)
		if err != nil {
			return nil, err
		}
		out["limit"] = n
	case "pie":
		by := strings.ToLower(cfgStr(cfg, "by"))
		if _, ok := search.Dimensions[by]; !ok {
			return nil, invalid("by is one of %s", dimNames())
		}
		out["by"] = by
	case "stats_2d":
		r, c := strings.ToLower(cfgStr(cfg, "rows")), strings.ToLower(cfgStr(cfg, "cols"))
		if _, ok := search.Dimensions[r]; !ok {
			return nil, invalid("rows is one of %s", dimNames())
		}
		if _, ok := search.Dimensions[c]; !ok || c == r {
			return nil, invalid("cols is one of %s, and not the same as rows", dimNames())
		}
		out["rows"], out["cols"] = r, c
	case "created_vs_resolved":
		n, err := cfgInt(cfg, "days", 30, 7, 90)
		if err != nil {
			return nil, err
		}
		out["days"] = n
	case "workload":
		b := cfgStr(cfg, "board_id")
		if b == "" {
			return nil, invalid("a workload gadget needs a board_id")
		}
		out["board_id"] = b
	case "sprint_burndown":
		sp := cfgStr(cfg, "sprint_id")
		if sp == "" {
			return nil, invalid("a sprint burndown needs a sprint_id")
		}
		out["sprint_id"] = sp
	case "text":
		md := strings.TrimSpace(cfgStr(cfg, "markdown"))
		if md == "" || len(md) > 4000 {
			return nil, invalid("text is 1 to 4000 characters")
		}
		clean, _ := Sanitize(md)
		out["markdown"] = clean
	case "iframe":
		raw := cfgStr(cfg, "url")
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, invalid("an embedded page must be an https address")
		}
		ok := false
		for _, h := range s.DashboardIframeHosts {
			ok = ok || strings.EqualFold(h, u.Hostname())
		}
		if !ok {
			return nil, invalid("%s is not on the list of hosts that may be embedded (the administrator sets dashboard_iframe_hosts)", u.Hostname())
		}
		h, err := cfgInt(cfg, "height", 300, 100, 1200)
		if err != nil {
			return nil, err
		}
		out["url"], out["height"] = raw, h
	}
	return out, nil
}

func dimNames() string {
	var n []string
	for k := range search.Dimensions {
		n = append(n, k)
	}
	sort.Strings(n)
	return strings.Join(n, ", ")
}

func (s *Service) AddGadget(ctx context.Context, a Actor, dashID string, in GadgetIn) (Gadget, error) {
	var g Gadget
	err := s.tx(ctx, func(tx pgx.Tx) error {
		d, edit, err := s.dashFor(ctx, tx, a, dashID)
		if err != nil {
			return err
		}
		if !edit {
			return ErrForbidden
		}
		cfg, err := s.validateGadget(ctx, a, d.shared, in.Kind, in.Config)
		if err != nil {
			return err
		}
		var n int
		tx.QueryRow(ctx, `SELECT count(*) FROM dashboard_gadget WHERE dashboard_id::text=$1`, dashID).Scan(&n)
		if n >= maxGadgets {
			return invalid("a dashboard has at most %d gadgets", maxGadgets)
		}
		if in.Col < 0 || in.Col >= d.columns {
			in.Col = 0
		}
		var pos int
		tx.QueryRow(ctx, `SELECT COALESCE(max(position)+1,0) FROM dashboard_gadget WHERE dashboard_id::text=$1 AND col=$2`, dashID, in.Col).Scan(&pos)
		title := strings.TrimSpace(in.Title)
		if len(title) > 100 {
			return invalid("the title is at most 100 characters")
		}
		b, _ := json.Marshal(cfg)
		if err := tx.QueryRow(ctx, `INSERT INTO dashboard_gadget(dashboard_id,col,position,kind,title,config) VALUES($1,$2,$3,$4,$5,$6) RETURNING id::text`, dashID, in.Col, pos, in.Kind, title, b).Scan(&g.ID); err != nil {
			return err
		}
		tx.Exec(ctx, `UPDATE dashboard SET updated_at=now() WHERE id::text=$1`, dashID)
		g.Col, g.Position, g.Kind, g.Title, g.Config = in.Col, pos, in.Kind, title, cfg
		return nil
	})
	return g, err
}

func (s *Service) UpdateGadget(ctx context.Context, a Actor, dashID, gadgetID string, in GadgetIn) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		d, edit, err := s.dashFor(ctx, tx, a, dashID)
		if err != nil {
			return err
		}
		if !edit {
			return ErrForbidden
		}
		var kind string
		var col, pos int
		if err := tx.QueryRow(ctx, `SELECT kind,col,position FROM dashboard_gadget WHERE id::text=$1 AND dashboard_id::text=$2 FOR UPDATE`, gadgetID, dashID).Scan(&kind, &col, &pos); err != nil {
			return ErrNotFound
		}
		cfg, err := s.validateGadget(ctx, a, d.shared, kind, in.Config)
		if err != nil {
			return err
		}
		title := strings.TrimSpace(in.Title)
		if len(title) > 100 {
			return invalid("the title is at most 100 characters")
		}
		if in.Col < 0 || in.Col >= d.columns {
			in.Col = col
		}
		npos := pos
		if in.Position != nil {
			npos = *in.Position
		} else if in.Col != col {
			tx.QueryRow(ctx, `SELECT COALESCE(max(position)+1,0) FROM dashboard_gadget WHERE dashboard_id::text=$1 AND col=$2`, dashID, in.Col).Scan(&npos)
		}
		b, _ := json.Marshal(cfg)
		if _, err := tx.Exec(ctx, `UPDATE dashboard_gadget SET title=$3,config=$4,col=$5,position=$6 WHERE id::text=$1 AND dashboard_id::text=$2`, gadgetID, dashID, title, b, in.Col, npos); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE dashboard SET updated_at=now() WHERE id::text=$1`, dashID)
		return err
	})
}

func (s *Service) DeleteGadget(ctx context.Context, a Actor, dashID, gadgetID string) error {
	_, edit, err := s.dashFor(ctx, s.DB, a, dashID)
	if err != nil {
		return err
	}
	if !edit {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM dashboard_gadget WHERE id::text=$1 AND dashboard_id::text=$2`, gadgetID, dashID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// ---- data, computed for the viewer

type GadgetResult struct {
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Data      any       `json:"data,omitempty"`
	Error     string    `json:"error,omitempty"`
	Generated time.Time `json:"generated_at"`
}

// andQuery adds a condition to a TQL query, ahead of any ORDER BY.
func andQuery(q, extra string) string {
	base, order := strings.TrimSpace(q), ""
	if i := strings.Index(strings.ToUpper(base), " ORDER BY "); i >= 0 {
		base, order = base[:i], base[i:]
	} else if strings.HasPrefix(strings.ToUpper(base), "ORDER BY ") {
		base, order = "", " "+base
	}
	if base == "" {
		return extra + order
	}
	return "(" + base + ") AND " + extra + order
}

func (s *Service) scopeFor(ctx context.Context, a Actor) (search.Scope, error) {
	all, rd, wr, err := s.SearchScope(ctx, a)
	if err != nil {
		return search.Scope{}, err
	}
	return search.Scope{UserID: a.ID, All: all, Readable: rd, Writable: wr, WeekStart: time.Monday, Now: s.Now}, nil
}

// gadgetQuery is the query a gadget runs: its own, or its saved filter's, which the viewer must be allowed to use.
func (s *Service) gadgetQuery(ctx context.Context, a Actor, cfg map[string]any) (string, error) {
	if fid := cfgStr(cfg, "filter_id"); fid != "" {
		f, _, err := s.filterFor(ctx, a, fid)
		if err != nil {
			return "", fmt.Errorf("the filter this gadget uses is not available to you")
		}
		return f.TQL, nil
	}
	return cfgStr(cfg, "q"), nil
}

// GadgetData computes one gadget for the viewer.
func (s *Service) GadgetData(ctx context.Context, a Actor, dashID, gadgetID string) (GadgetResult, error) {
	if _, _, err := s.dashFor(ctx, s.DB, a, dashID); err != nil {
		return GadgetResult{}, err
	}
	gs, err := s.loadGadgets(ctx, dashID)
	if err != nil {
		return GadgetResult{}, err
	}
	for _, g := range gs {
		if g.ID != gadgetID {
			continue
		}
		res := GadgetResult{Kind: g.Kind, Title: g.Title, Generated: s.Now()}
		data, err := s.computeGadget(ctx, a, g)
		if err != nil {
			res.Error = errText(err)
		} else {
			res.Data = data
		}
		return res, nil
	}
	return GadgetResult{}, ErrNotFound
}

func errText(err error) string {
	if e, ok := err.(*InvalidError); ok {
		return e.Msg
	}
	return err.Error()
}

func (s *Service) computeGadget(ctx context.Context, a Actor, g Gadget) (any, error) {
	sc, err := s.scopeFor(ctx, a)
	if err != nil {
		return nil, err
	}
	pg := &search.Postgres{DB: s.DB}
	cfg := g.Config
	switch g.Kind {
	case "text":
		return map[string]any{"markdown": cfgStr(cfg, "markdown")}, nil
	case "iframe":
		return map[string]any{"url": cfgStr(cfg, "url"), "height": cfg["height"]}, nil
	case "workload":
		w, err := s.WorkloadByAssignee(ctx, a, cfgStr(cfg, "board_id"), nil)
		if err != nil {
			return nil, err
		}
		return map[string]any{"rows": w}, nil
	case "sprint_burndown":
		r, err := s.SprintReport(ctx, a, cfgStr(cfg, "sprint_id"))
		if err != nil {
			return nil, err
		}
		return map[string]any{"sprint": r.Sprint, "committed": r.Committed, "completed": r.Completed, "burn": r.Burn}, nil
	case "assigned_to_me":
		lim, _ := cfgInt(cfg, "limit", 10, 1, 50)
		res, err := pg.Search(ctx, sc, `assignee = currentUser() AND statusCategory != Done ORDER BY updated DESC`, search.Page{Limit: lim})
		if err != nil {
			return nil, err
		}
		return map[string]any{"issues": res.Rows, "total": res.Total}, nil
	}
	q, err := s.gadgetQuery(ctx, a, cfg)
	if err != nil {
		return nil, err
	}
	switch g.Kind {
	case "filter_results":
		lim, _ := cfgInt(cfg, "limit", 10, 1, 50)
		res, err := pg.Search(ctx, sc, q, search.Page{Limit: lim})
		if err != nil {
			return nil, err
		}
		return map[string]any{"issues": res.Rows, "total": res.Total, "total_capped": res.TotalCapped, "query": q}, nil
	case "pie":
		ag, err := pg.Aggregate(ctx, sc, q, cfgStr(cfg, "by"), "", 500)
		if err != nil {
			return nil, err
		}
		slices := []search.AggRow{}
		other := 0
		for i, r := range ag.Rows {
			if i < 12 {
				slices = append(slices, search.AggRow{A: r.A, Count: r.Count})
			} else {
				other += r.Count
			}
		}
		if other > 0 {
			slices = append(slices, search.AggRow{A: "Other", Count: other})
		}
		return map[string]any{"slices": slices, "total": ag.Total, "by": cfgStr(cfg, "by")}, nil
	case "stats_2d":
		ag, err := pg.Aggregate(ctx, sc, q, cfgStr(cfg, "rows"), cfgStr(cfg, "cols"), 500)
		if err != nil {
			return nil, err
		}
		return map[string]any{"cells": ag.Rows, "total": ag.Total, "rows": cfgStr(cfg, "rows"), "cols": cfgStr(cfg, "cols")}, nil
	case "average_age":
		keys, err := pg.Keys(ctx, sc, andQuery(q, `statusCategory != Done`), 10000)
		if err != nil {
			return nil, err
		}
		var n int
		var avg *float64
		var oldest *string
		s.DB.QueryRow(ctx, `SELECT count(*), avg(extract(epoch FROM ($2::timestamptz - created_at))/86400), (SELECT key FROM issue WHERE key = ANY($1) ORDER BY created_at LIMIT 1) FROM issue WHERE key = ANY($1)`, keys, s.Now()).Scan(&n, &avg, &oldest)
		out := map[string]any{"open_issues": n}
		if avg != nil {
			out["average_age_days"] = round2(*avg)
		}
		if oldest != nil {
			out["oldest"] = *oldest
		}
		return out, nil
	case "created_vs_resolved":
		days, _ := cfgInt(cfg, "days", 30, 7, 90)
		keys, err := pg.Keys(ctx, sc, q, 10000)
		if err != nil {
			return nil, err
		}
		rows, err := s.DB.Query(ctx, `WITH d AS (SELECT generate_series(($2::timestamptz AT TIME ZONE 'UTC')::date - ($3-1), ($2::timestamptz AT TIME ZONE 'UTC')::date, '1 day')::date AS day)
			SELECT d.day, (SELECT count(*) FROM issue i WHERE i.key = ANY($1) AND (i.created_at AT TIME ZONE 'UTC')::date = d.day),
			              (SELECT count(*) FROM issue i WHERE i.key = ANY($1) AND (i.resolved_at AT TIME ZONE 'UTC')::date = d.day) FROM d ORDER BY d.day`, keys, s.Now(), days)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		type day struct {
			Day      string `json:"day"`
			Created  int    `json:"created"`
			Resolved int    `json:"resolved"`
		}
		var out []day
		for rows.Next() {
			var d time.Time
			var x day
			rows.Scan(&d, &x.Created, &x.Resolved)
			x.Day = d.Format("2006-01-02")
			out = append(out, x)
		}
		return map[string]any{"days": out}, rows.Err()
	case "activity_stream":
		lim, _ := cfgInt(cfg, "limit", 10, 1, 30)
		keys, err := pg.Keys(ctx, sc, q, 500)
		if err != nil {
			return nil, err
		}
		rows, err := s.DB.Query(ctx, `SELECT i.key,h.field,left(COALESCE(h.after,''),120),h.actor_label,h.at FROM issue_history h JOIN issue i ON i.id=h.issue_id
			WHERE i.key = ANY($1) AND h.field NOT IN ('description') ORDER BY h.at DESC, h.id DESC LIMIT $2`, keys, lim)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		type ev struct {
			Key   string    `json:"key"`
			Field string    `json:"field"`
			After string    `json:"after"`
			Actor string    `json:"actor"`
			At    time.Time `json:"at"`
		}
		out := []ev{}
		for rows.Next() {
			var e ev
			rows.Scan(&e.Key, &e.Field, &e.After, &e.Actor, &e.At)
			out = append(out, e)
		}
		return map[string]any{"events": out}, rows.Err()
	}
	return nil, invalid("unknown gadget %q", g.Kind)
}
