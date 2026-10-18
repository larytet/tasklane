package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The workflow editor (SPEC 9). A workflow is edited as a draft (a whole definition kept beside the live one), checked,
// and published. Publishing keeps the ids of statuses and transitions that survive, so issues and boards stay attached.
// A status that is removed while issues sit in it is retired: nothing can move into it, issues in it keep working as if
// they were in the status they are mapped to, and a background job moves them in batches and then deletes it.

type WStatus struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Color    string `json:"color,omitempty"`
	X        *int   `json:"x,omitempty"`
	Y        *int   `json:"y,omitempty"`
}

type WTransition struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name"`
	From          string `json:"from"` // a status name; empty means from any status
	To            string `json:"to"`
	RequiresHuman bool   `json:"requires_human,omitempty"`
	Rules         TRules `json:"rules"`
}

type WDef struct {
	Statuses    []WStatus     `json:"statuses"` // in order; the first To Do status is where new issues start
	Transitions []WTransition `json:"transitions"`
}

type WorkflowInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Version   int      `json:"version"`
	Projects  []string `json:"projects"`
	HasDraft  bool     `json:"has_draft"`
	Migrating bool     `json:"migrating"`
}

type WorkflowView struct {
	WorkflowInfo
	Live       WDef             `json:"live"`
	Draft      *WDef            `json:"draft,omitempty"`
	DraftBase  int              `json:"draft_base,omitempty"`
	Migrations []WorkflowMigRow `json:"migrations"`
}

type WorkflowMigRow struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	To     string `json:"to"`
	Total  int    `json:"total"`
	Done   int    `json:"done"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}

func (s *Service) ListWorkflows(ctx context.Context, a Actor) ([]WorkflowInfo, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT w.id::text,w.name,w.version,COALESCE((SELECT array_agg(p.key ORDER BY p.key) FROM project p WHERE p.workflow_id=w.id),'{}'),
		EXISTS(SELECT 1 FROM workflow_draft d WHERE d.workflow_id=w.id),EXISTS(SELECT 1 FROM workflow_migration m WHERE m.workflow_id=w.id AND m.state='pending')
		FROM workflow w ORDER BY lower(w.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkflowInfo{}
	for rows.Next() {
		var w WorkflowInfo
		if err := rows.Scan(&w.ID, &w.Name, &w.Version, &w.Projects, &w.HasDraft, &w.Migrating); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// liveDef reads the live definition (system transitions and retired statuses are not part of what an editor sees).
func (s *Service) liveDef(ctx context.Context, q querier, wf string) (WDef, error) {
	def := WDef{Statuses: []WStatus{}, Transitions: []WTransition{}}
	rows, err := q.Query(ctx, `SELECT id::text,name,category,color,pos_x,pos_y FROM status WHERE workflow_id::text=$1 AND NOT retired ORDER BY position,name`, wf)
	if err != nil {
		return def, err
	}
	for rows.Next() {
		var st WStatus
		if err := rows.Scan(&st.ID, &st.Name, &st.Category, &st.Color, &st.X, &st.Y); err != nil {
			rows.Close()
			return def, err
		}
		def.Statuses = append(def.Statuses, st)
	}
	rows.Close()
	tr, err := q.Query(ctx, `SELECT t.id::text,t.name,COALESCE(f.name,''),tt.name,t.requires_human,t.rules FROM transition t LEFT JOIN status f ON f.id=t.from_status
		JOIN status tt ON tt.id=t.to_status WHERE t.workflow_id::text=$1 AND NOT t.system ORDER BY t.name,tt.name,f.name`, wf)
	if err != nil {
		return def, err
	}
	defer tr.Close()
	for tr.Next() {
		var t WTransition
		var raw []byte
		if err := tr.Scan(&t.ID, &t.Name, &t.From, &t.To, &t.RequiresHuman, &raw); err != nil {
			return def, err
		}
		t.Rules = parseRules(raw)
		def.Transitions = append(def.Transitions, t)
	}
	return def, tr.Err()
}

func (s *Service) GetWorkflow(ctx context.Context, a Actor, id string) (WorkflowView, error) {
	var v WorkflowView
	if err := adminOnly(a); err != nil {
		return v, err
	}
	all, err := s.ListWorkflows(ctx, a)
	if err != nil {
		return v, err
	}
	found := false
	for _, w := range all {
		if w.ID == id {
			v.WorkflowInfo, found = w, true
		}
	}
	if !found {
		return v, ErrNotFound
	}
	if v.Live, err = s.liveDef(ctx, s.DB, id); err != nil {
		return v, err
	}
	var raw []byte
	var base int
	if err := s.DB.QueryRow(ctx, `SELECT definition,base_version FROM workflow_draft WHERE workflow_id::text=$1`, id).Scan(&raw, &base); err == nil {
		var d WDef
		json.Unmarshal(raw, &d)
		v.Draft, v.DraftBase = &d, base
	}
	rows, err := s.DB.Query(ctx, `SELECT id,status_name,(SELECT name FROM status WHERE id=status_to),total,done,state,error FROM workflow_migration WHERE workflow_id::text=$1 ORDER BY id DESC LIMIT 20`, id)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	v.Migrations = []WorkflowMigRow{}
	for rows.Next() {
		var m WorkflowMigRow
		if err := rows.Scan(&m.ID, &m.Status, &m.To, &m.Total, &m.Done, &m.State, &m.Error); err != nil {
			return v, err
		}
		v.Migrations = append(v.Migrations, m)
	}
	return v, rows.Err()
}

// Problems are what checking a definition found. Errors block publishing; warnings do not.
type Problems struct {
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

func (p *Problems) errf(f string, a ...any)  { p.Errors = append(p.Errors, fmt.Sprintf(f, a...)) }
func (p *Problems) warnf(f string, a ...any) { p.Warnings = append(p.Warnings, fmt.Sprintf(f, a...)) }

func (s *Service) checkDef(ctx context.Context, q querier, d WDef) Problems {
	p := Problems{Errors: []string{}, Warnings: []string{}}
	if len(d.Statuses) == 0 {
		p.errf("a workflow needs at least one status")
		return p
	}
	byName := map[string]WStatus{}
	hasTodo := false
	for _, st := range d.Statuses {
		n := strings.TrimSpace(st.Name)
		switch {
		case n == "":
			p.errf("a status needs a name")
		case len(n) > 60:
			p.errf("status %q: names are at most 60 characters", n)
		}
		if _, dup := byName[strings.ToLower(n)]; dup {
			p.errf("two statuses are called %q", n)
		}
		byName[strings.ToLower(n)] = st
		if st.Category != "todo" && st.Category != "in_progress" && st.Category != "done" {
			p.errf("status %q: the category is todo, in_progress or done", n)
		}
		hasTodo = hasTodo || st.Category == "todo"
	}
	if !hasTodo {
		p.errf("a workflow needs a To Do status for new issues to start in")
	} else if d.Statuses[0].Category != "todo" {
		p.warnf("new issues start in the first status, %q, which is not a To Do status", d.Statuses[0].Name)
	}
	seen := map[string]bool{}
	out := map[string]int{}
	anyFrom := false
	for _, t := range d.Transitions {
		if strings.TrimSpace(t.Name) == "" {
			p.errf("a transition needs a name")
		}
		if t.From != "" {
			if _, ok := byName[strings.ToLower(t.From)]; !ok {
				p.errf("transition %q starts from %q, which is not a status", t.Name, t.From)
			}
		}
		if _, ok := byName[strings.ToLower(t.To)]; !ok {
			p.errf("transition %q goes to %q, which is not a status", t.Name, t.To)
		}
		k := strings.ToLower(t.Name + "|" + t.From + "|" + t.To)
		if seen[k] {
			p.errf("transition %q from %q to %q is listed twice", t.Name, t.From, t.To)
		}
		seen[k] = true
		if err := s.ValidateRules(ctx, q, t.Rules); err != nil {
			p.errf("transition %q: %v", t.Name, err)
		}
		if t.From == "" {
			anyFrom = true
		} else {
			out[strings.ToLower(t.From)]++
		}
	}
	// reachability from the initial status (an "any" transition is reachable from everywhere)
	if len(d.Statuses) > 0 {
		reach := map[string]bool{strings.ToLower(d.Statuses[0].Name): true}
		for changed := true; changed; {
			changed = false
			for _, t := range d.Transitions {
				to := strings.ToLower(t.To)
				if reach[to] {
					continue
				}
				if t.From == "" || reach[strings.ToLower(t.From)] {
					reach[to], changed = true, true
				}
			}
		}
		for _, st := range d.Statuses {
			n := strings.ToLower(st.Name)
			if !reach[n] {
				p.warnf("%q cannot be reached from the first status; issues can only get there by import or by being moved", st.Name)
			}
			if out[n] == 0 && !anyFrom && st.Category != "done" {
				p.warnf("%q has no way out: an issue there would be stuck", st.Name)
			}
		}
	}
	sort.Strings(p.Errors)
	return p
}

// SaveWorkflowDraft stores a draft definition (creating the draft from the live one when there is none yet).
func (s *Service) SaveWorkflowDraft(ctx context.Context, a Actor, id string, d WDef) (WorkflowView, error) {
	if err := adminOnly(a); err != nil {
		return WorkflowView{}, err
	}
	for i := range d.Statuses {
		d.Statuses[i].Name = strings.TrimSpace(d.Statuses[i].Name)
	}
	for i := range d.Transitions {
		d.Transitions[i].Name = strings.TrimSpace(d.Transitions[i].Name)
		for j := range d.Transitions[i].Rules.Screen {
			d.Transitions[i].Rules.Screen[j] = canonKey(d.Transitions[i].Rules.Screen[j])
		}
	}
	raw, _ := json.Marshal(d)
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var ver int
		if err := tx.QueryRow(ctx, `SELECT version FROM workflow WHERE id::text=$1 FOR UPDATE`, id).Scan(&ver); err != nil {
			return ErrNotFound
		}
		_, err := tx.Exec(ctx, `INSERT INTO workflow_draft(workflow_id,base_version,definition,updated_by,updated_at) VALUES($1::uuid,$2,$3,$4::uuid,$5)
			ON CONFLICT (workflow_id) DO UPDATE SET definition=EXCLUDED.definition,updated_by=EXCLUDED.updated_by,updated_at=EXCLUDED.updated_at`, id, ver, raw, nilIfEmpty(a.ID), s.Now())
		return err
	})
	if err != nil {
		return WorkflowView{}, err
	}
	return s.GetWorkflow(ctx, a, id)
}

// StartWorkflowDraft copies the live definition into a draft (unchanged when one exists).
func (s *Service) StartWorkflowDraft(ctx context.Context, a Actor, id string) (WorkflowView, error) {
	v, err := s.GetWorkflow(ctx, a, id)
	if err != nil {
		return v, err
	}
	if v.Draft != nil {
		return v, nil
	}
	return s.SaveWorkflowDraft(ctx, a, id, v.Live)
}

func (s *Service) AbandonWorkflowDraft(ctx context.Context, a Actor, id string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM workflow_draft WHERE workflow_id::text=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PublishPlan is what publishing the draft would do.
type PublishPlan struct {
	Problems
	Added      []string        `json:"added"`
	Renamed    []string        `json:"renamed"`
	Removed    []RemovedStatus `json:"removed"`
	NeedsInput bool            `json:"needs_mapping"`
}

type RemovedStatus struct {
	Name   string `json:"name"`
	Issues int    `json:"issues"`
	MapTo  string `json:"map_to,omitempty"`
}

func (s *Service) planPublish(ctx context.Context, q querier, id string, mapping map[string]string) (PublishPlan, WDef, WDef, int, error) {
	plan := PublishPlan{Added: []string{}, Renamed: []string{}, Removed: []RemovedStatus{}}
	var raw []byte
	var base, ver int
	if err := q.QueryRow(ctx, `SELECT definition,base_version FROM workflow_draft WHERE workflow_id::text=$1`, id).Scan(&raw, &base); err != nil {
		return plan, WDef{}, WDef{}, 0, invalid("there is no draft to publish")
	}
	if err := q.QueryRow(ctx, `SELECT version FROM workflow WHERE id::text=$1`, id).Scan(&ver); err != nil {
		return plan, WDef{}, WDef{}, 0, ErrNotFound
	}
	var draft WDef
	json.Unmarshal(raw, &draft)
	live, err := s.liveDef(ctx, q, id)
	if err != nil {
		return plan, draft, live, 0, err
	}
	plan.Problems = s.checkDef(ctx, q, draft)
	if base != ver {
		plan.errf("the workflow was published (version %d) since this draft was started from version %d; abandon the draft and start again", ver, base)
	}
	keep := map[string]bool{}
	liveByID := map[string]WStatus{}
	for _, st := range live.Statuses {
		liveByID[st.ID] = st
	}
	names := map[string]bool{}
	for _, st := range draft.Statuses {
		names[strings.ToLower(st.Name)] = true
		if st.ID == "" {
			plan.Added = append(plan.Added, st.Name)
			continue
		}
		keep[st.ID] = true
		if old, ok := liveByID[st.ID]; ok && old.Name != st.Name {
			plan.Renamed = append(plan.Renamed, old.Name+" → "+st.Name)
		} else if !ok {
			plan.errf("status %q refers to an id that is not in the workflow", st.Name)
		}
	}
	for _, st := range live.Statuses {
		if keep[st.ID] {
			continue
		}
		var pending int
		q.QueryRow(ctx, `SELECT count(*) FROM workflow_migration WHERE status_to::text=$1 AND state='pending'`, st.ID).Scan(&pending)
		if pending > 0 {
			plan.errf("%q is where issues from an earlier removal are still being moved; wait for that migration to finish before removing it", st.Name)
		}
		var n int
		q.QueryRow(ctx, `SELECT count(*) FROM issue WHERE status_id::text=$1`, st.ID).Scan(&n)
		r := RemovedStatus{Name: st.Name, Issues: n, MapTo: mapping[st.Name]}
		if n > 0 {
			plan.NeedsInput = true
			switch {
			case r.MapTo == "":
				plan.errf("%d issue(s) are in %q, which the draft removes: choose a status to move them to", n, st.Name)
			case !names[strings.ToLower(r.MapTo)]:
				plan.errf("%q cannot be moved to %q: that is not a status in the draft", st.Name, r.MapTo)
			}
		}
		plan.Removed = append(plan.Removed, r)
	}
	return plan, draft, live, ver, nil
}

// PlanWorkflowPublish checks the draft and reports what publishing would change, without changing anything.
func (s *Service) PlanWorkflowPublish(ctx context.Context, a Actor, id string, mapping map[string]string) (PublishPlan, error) {
	if err := adminOnly(a); err != nil {
		return PublishPlan{}, err
	}
	p, _, _, _, err := s.planPublish(ctx, s.DB, id, mapping)
	return p, err
}

type PublishResult struct {
	Version    int     `json:"version"`
	Migrations []int64 `json:"migrations"`
}

// PublishWorkflow makes the draft the live workflow. Statuses and transitions that exist in both keep their ids. Issues in a
// removed status are not touched here: the status is retired and the migration job moves them.
func (s *Service) PublishWorkflow(ctx context.Context, a Actor, id string, mapping map[string]string, note string) (PublishResult, error) {
	var res PublishResult
	if err := adminOnly(a); err != nil {
		return res, err
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var ver int
		if err := tx.QueryRow(ctx, `SELECT version FROM workflow WHERE id::text=$1 FOR UPDATE`, id).Scan(&ver); err != nil {
			return ErrNotFound
		}
		plan, draft, live, _, err := s.planPublish(ctx, tx, id, mapping)
		if err != nil {
			return err
		}
		if len(plan.Errors) > 0 {
			return &PublishRefused{Plan: plan}
		}
		idOf := map[string]string{} // status name -> id after publishing
		keep := map[string]bool{}
		// every live name is parked first so renames can swap and a new status may reuse the name of one that is going away
		if _, err := tx.Exec(ctx, `UPDATE status SET name=name||'~'||id::text WHERE workflow_id::text=$1 AND NOT retired`, id); err != nil {
			return err
		}
		for i, st := range draft.Statuses {
			sid := st.ID
			if sid == "" {
				if err := tx.QueryRow(ctx, `INSERT INTO status(workflow_id,name,category,position,color,pos_x,pos_y) VALUES($1::uuid,$2,$3,$4,$5,$6,$7) RETURNING id::text`,
					id, st.Name, st.Category, i, st.Color, st.X, st.Y).Scan(&sid); err != nil {
					return err
				}
			}
			idOf[strings.ToLower(st.Name)] = sid
			keep[sid] = true
		}
		for i, st := range draft.Statuses {
			sid := idOf[strings.ToLower(st.Name)]
			if _, err := tx.Exec(ctx, `UPDATE status SET name=$2,category=$3,position=$4,color=$5,pos_x=$6,pos_y=$7,retired=false WHERE id::text=$1`, sid, st.Name, st.Category, i, st.Color, st.X, st.Y); err != nil {
				return err
			}
		}
		// transitions: update the ones that survive by id, insert new ones, delete the rest (system transitions are rebuilt below)
		if _, err := tx.Exec(ctx, `DELETE FROM transition WHERE workflow_id::text=$1 AND system`, id); err != nil {
			return err
		}
		keepT := map[string]bool{}
		for _, t := range draft.Transitions {
			var from any
			if t.From != "" {
				from = idOf[strings.ToLower(t.From)]
			}
			to := idOf[strings.ToLower(t.To)]
			rj, _ := json.Marshal(t.Rules)
			tid := t.ID
			if tid != "" {
				tag, err := tx.Exec(ctx, `UPDATE transition SET name=$2,from_status=$3::uuid,to_status=$4::uuid,requires_human=$5,rules=$6 WHERE id::text=$1 AND workflow_id::text=$7 AND NOT system`, tid, t.Name, from, to, t.RequiresHuman, rj, id)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 0 {
					tid = ""
				}
			}
			if tid == "" {
				if err := tx.QueryRow(ctx, `INSERT INTO transition(workflow_id,name,from_status,to_status,requires_human,rules) VALUES($1::uuid,$2,$3::uuid,$4::uuid,$5,$6) RETURNING id::text`, id, t.Name, from, to, t.RequiresHuman, rj).Scan(&tid); err != nil {
					return err
				}
			}
			keepT[tid] = true
		}
		rows, err := tx.Query(ctx, `SELECT id::text FROM transition WHERE workflow_id::text=$1`, id)
		if err != nil {
			return err
		}
		var drop []string
		for rows.Next() {
			var tid string
			rows.Scan(&tid)
			if !keepT[tid] {
				drop = append(drop, tid)
			}
		}
		rows.Close()
		for _, tid := range drop {
			if _, err := tx.Exec(ctx, `DELETE FROM transition WHERE id::text=$1`, tid); err != nil {
				return err
			}
		}
		// removed statuses: delete when empty, retire when issues are in them
		for _, st := range live.Statuses {
			if keep[st.ID] {
				continue
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM issue WHERE status_id::text=$1`, st.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				if _, err := tx.Exec(ctx, `DELETE FROM status WHERE id::text=$1`, st.ID); err != nil {
					return err
				}
				continue
			}
			target := idOf[strings.ToLower(mapping[st.Name])]
			if _, err := tx.Exec(ctx, `UPDATE status SET retired=true,name=$2 WHERE id::text=$1`, st.ID, st.Name+" (retired)"); err != nil {
				return err
			}
			var mid int64
			if err := tx.QueryRow(ctx, `INSERT INTO workflow_migration(workflow_id,version,status_from,status_name,status_to,total) VALUES($1::uuid,$2,$3::uuid,$4,$5::uuid,$6) RETURNING id`,
				id, ver+1, st.ID, st.Name, target, n).Scan(&mid); err != nil {
				return err
			}
			res.Migrations = append(res.Migrations, mid)
		}
		// until the job has moved them, an issue in a retired status can take every transition of the status it will become
		if _, err := tx.Exec(ctx, `INSERT INTO transition(workflow_id,name,from_status,to_status,requires_human,rules,system)
			SELECT t.workflow_id,t.name,m.status_from,t.to_status,t.requires_human,t.rules,true FROM workflow_migration m JOIN transition t ON t.from_status=m.status_to AND NOT t.system
			WHERE m.workflow_id::text=$1 AND m.state='pending'`, id); err != nil {
			return err
		}
		snap, _ := json.Marshal(draft)
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_version(workflow_id,version,definition,note,published_by,published_at) VALUES($1::uuid,$2,$3,$4,$5::uuid,$6)`, id, ver+1, snap, note, nilIfEmpty(a.ID), s.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow SET version=$2,published_at=$3 WHERE id::text=$1`, id, ver+1, s.Now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workflow_draft WHERE workflow_id::text=$1`, id); err != nil {
			return err
		}
		res.Version = ver + 1
		return s.audit(ctx, tx, a, "workflow.publish", id, map[string]any{"version": ver + 1, "note": note, "migrations": len(res.Migrations)})
	})
	return res, err
}

// PublishRefused carries the plan that explains why publishing was refused.
type PublishRefused struct{ Plan PublishPlan }

func (e *PublishRefused) Error() string {
	return "the draft cannot be published: " + strings.Join(e.Plan.Errors, "; ")
}

// ----- the migration job

// RunWorkflowMigrations moves issues out of retired statuses until the context ends.
func (s *Service) RunWorkflowMigrations(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if _, err := s.ProcessWorkflowMigrations(ctx, 500, 20); err != nil && ctx.Err() == nil {
			logf("workflow migration: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ProcessWorkflowMigrations runs up to maxBatches batches of batch issues each and returns how many issues it moved.
// Each batch is its own transaction, so editors are never blocked for long and a crash loses nothing: the job resumes
// from what is still in the retired status.
func (s *Service) ProcessWorkflowMigrations(ctx context.Context, batch, maxBatches int) (int, error) {
	moved := 0
	for b := 0; b < maxBatches; b++ {
		var did int
		var finished bool
		err := s.tx(ctx, func(tx pgx.Tx) error {
			var id int64
			var from, to, wf, name string
			err := tx.QueryRow(ctx, `SELECT id,status_from::text,status_to::text,workflow_id::text,status_name FROM workflow_migration WHERE state='pending' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id, &from, &to, &wf, &name)
			if err != nil {
				return err
			}
			var toName, cat string
			if err := tx.QueryRow(ctx, `SELECT name,category FROM status WHERE id::text=$1`, to).Scan(&toName, &cat); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT i.id::text,i.key,i.version,i.field_ver FROM issue i WHERE i.status_id::text=$1 ORDER BY i.id LIMIT $2 FOR UPDATE OF i SKIP LOCKED`, from, batch)
			if err != nil {
				return err
			}
			type row struct {
				id, key string
				ver     int64
				fv      []byte
			}
			var rs []row
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.id, &r.key, &r.ver, &r.fv); err != nil {
					rows.Close()
					return err
				}
				rs = append(rs, r)
			}
			rows.Close()
			sys := Actor{Label: "workflow migration"}
			label := strings.TrimSuffix(name, " (retired)")
			for _, r := range rs {
				var fv map[string]int64
				json.Unmarshal(r.fv, &fv)
				if fv == nil {
					fv = map[string]int64{}
				}
				next := r.ver + 1
				fv["status"] = next
				fj, _ := json.Marshal(fv)
				resolved := "NULL"
				if cat == "done" {
					resolved = "COALESCE(resolved_at,now())"
				}
				if _, err := tx.Exec(ctx, `UPDATE issue SET status_id=$2::uuid,version=$3,field_ver=$4,updated_at=now(),resolved_at=`+resolved+` WHERE id::text=$1`, r.id, to, next, fj); err != nil {
					return err
				}
				if err := s.hist(ctx, tx, r.id, sys, "status", &label, &toName); err != nil {
					return err
				}
				if err := outbox(ctx, tx, "issue:"+r.key, "issue.transitioned", map[string]any{"key": r.key, "from": label, "to": toName, "version": next, "actor": sys.Label}); err != nil {
					return err
				}
			}
			did = len(rs)
			if _, err := tx.Exec(ctx, `UPDATE workflow_migration SET done=done+$2 WHERE id=$1`, id, did); err != nil {
				return err
			}
			var left int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM issue WHERE status_id::text=$1`, from).Scan(&left); err != nil {
				return err
			}
			if left == 0 {
				// the retired status is no longer needed; its system transitions go with it
				if _, err := tx.Exec(ctx, `DELETE FROM status WHERE id::text=$1`, from); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE workflow_migration SET state='done',finished_at=$2,status_from=status_from WHERE id=$1`, id, s.Now()); err != nil {
					return err
				}
				finished = true
			}
			return nil
		})
		if err == pgx.ErrNoRows {
			return moved, nil
		}
		if err != nil {
			return moved, err
		}
		moved += did
		_ = finished
	}
	return moved, nil
}

// ----- templates, import and export

var workflowTemplates = map[string]WDef{
	"simple": {Statuses: []WStatus{{Name: "To Do", Category: "todo"}, {Name: "Done", Category: "done"}},
		Transitions: []WTransition{{Name: "Done", From: "To Do", To: "Done"}, {Name: "Reopen", From: "Done", To: "To Do"}}},
	"software": {Statuses: []WStatus{{Name: "To Do", Category: "todo"}, {Name: "In Progress", Category: "in_progress"}, {Name: "In Review", Category: "in_progress"}, {Name: "Done", Category: "done"}},
		Transitions: []WTransition{{Name: "Start progress", From: "To Do", To: "In Progress"}, {Name: "Request review", From: "In Progress", To: "In Review"}, {Name: "Approve", From: "In Review", To: "Done"},
			{Name: "Changes needed", From: "In Review", To: "In Progress"}, {Name: "Back to To Do", From: "In Progress", To: "To Do"}, {Name: "Reopen", From: "Done", To: "To Do"}}},
	"service desk": {Statuses: []WStatus{{Name: "Open", Category: "todo"}, {Name: "Waiting for customer", Category: "in_progress"}, {Name: "In Progress", Category: "in_progress"}, {Name: "Resolved", Category: "done"}},
		Transitions: []WTransition{{Name: "Start progress", From: "Open", To: "In Progress"}, {Name: "Ask the customer", From: "In Progress", To: "Waiting for customer"}, {Name: "Customer replied", From: "Waiting for customer", To: "In Progress"},
			{Name: "Resolve", To: "Resolved"}, {Name: "Reopen", From: "Resolved", To: "Open"}}},
	"approval": {Statuses: []WStatus{{Name: "Draft", Category: "todo"}, {Name: "Pending approval", Category: "in_progress"}, {Name: "Approved", Category: "done"}, {Name: "Rejected", Category: "done"}},
		Transitions: []WTransition{{Name: "Submit", From: "Draft", To: "Pending approval"}, {Name: "Approve", From: "Pending approval", To: "Approved", RequiresHuman: true, Rules: TRules{Conditions: []Spec{{"kind": "role", "roles": []string{"admin"}}}}},
			{Name: "Reject", From: "Pending approval", To: "Rejected", RequiresHuman: true, Rules: TRules{Conditions: []Spec{{"kind": "role", "roles": []string{"admin"}}}}}, {Name: "Revise", From: "Rejected", To: "Draft"}}},
}

func WorkflowTemplateNames() []string {
	out := make([]string, 0, len(workflowTemplates))
	for k := range workflowTemplates {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CreateWorkflow makes a new workflow from a template or an imported definition. Nothing uses it until a project is switched to it.
func (s *Service) CreateWorkflow(ctx context.Context, a Actor, name, template string, def *WDef) (WorkflowInfo, error) {
	if err := adminOnly(a); err != nil {
		return WorkflowInfo{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return WorkflowInfo{}, invalid("a workflow needs a name")
	}
	var d WDef
	switch {
	case def != nil:
		d = *def
	case template != "":
		t, ok := workflowTemplates[strings.ToLower(template)]
		if !ok {
			return WorkflowInfo{}, invalid("unknown template %q (templates: %s)", template, strings.Join(WorkflowTemplateNames(), ", "))
		}
		d = t
	default:
		d = workflowTemplates["simple"]
	}
	for i := range d.Statuses {
		d.Statuses[i].ID = ""
	}
	for i := range d.Transitions {
		d.Transitions[i].ID = ""
	}
	if p := s.checkDef(ctx, s.DB, d); len(p.Errors) > 0 {
		return WorkflowInfo{}, invalid("%s", strings.Join(p.Errors, "; "))
	}
	var id string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO workflow(name) VALUES($1) RETURNING id::text`, name).Scan(&id); err != nil {
			if strings.Contains(err.Error(), "workflow_name_key") {
				return invalid("a workflow named %q exists", name)
			}
			return err
		}
		ids := map[string]string{}
		for i, st := range d.Statuses {
			var sid string
			if err := tx.QueryRow(ctx, `INSERT INTO status(workflow_id,name,category,position,color,pos_x,pos_y) VALUES($1::uuid,$2,$3,$4,$5,$6,$7) RETURNING id::text`, id, st.Name, st.Category, i, st.Color, st.X, st.Y).Scan(&sid); err != nil {
				return err
			}
			ids[strings.ToLower(st.Name)] = sid
		}
		for _, t := range d.Transitions {
			var from any
			if t.From != "" {
				from = ids[strings.ToLower(t.From)]
			}
			rj, _ := json.Marshal(t.Rules)
			if _, err := tx.Exec(ctx, `INSERT INTO transition(workflow_id,name,from_status,to_status,requires_human,rules) VALUES($1::uuid,$2,$3::uuid,$4::uuid,$5,$6)`, id, t.Name, from, ids[strings.ToLower(t.To)], t.RequiresHuman, rj); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, a, "workflow.create", id, map[string]any{"name": name})
	})
	if err != nil {
		return WorkflowInfo{}, err
	}
	return WorkflowInfo{ID: id, Name: name, Version: 1, Projects: []string{}}, nil
}

// ExportWorkflow returns the live definition for download. Webhook secrets are never part of a definition (they are not
// stored in it), so an export can be shared.
func (s *Service) ExportWorkflow(ctx context.Context, a Actor, id string) (WDef, error) {
	v, err := s.GetWorkflow(ctx, a, id)
	if err != nil {
		return WDef{}, err
	}
	d := v.Live
	for i := range d.Statuses {
		d.Statuses[i].ID = ""
	}
	for i := range d.Transitions {
		d.Transitions[i].ID = ""
	}
	return d, nil
}

// SetProjectWorkflow points an empty project at another workflow. A project that has issues keeps its workflow: its
// issues sit in statuses of it, so the way to change it is to edit that workflow (with a mapping for removed statuses).
func (s *Service) SetProjectWorkflow(ctx context.Context, a Actor, projectKey, workflowID string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id::text=$1`, pid).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return invalid("%s has %d issue(s), so its workflow cannot be swapped: edit its workflow instead", projectKey, n)
		}
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM workflow WHERE id::text=$1`, workflowID).Scan(&one); err != nil {
			return invalid("unknown workflow")
		}
		// boards map statuses of the old workflow; they are rebuilt from the new one
		if _, err := tx.Exec(ctx, `DELETE FROM board_column_status WHERE status_id IN (SELECT id FROM status WHERE workflow_id=(SELECT workflow_id FROM project WHERE id::text=$1))`, pid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE project SET workflow_id=$2::uuid WHERE id::text=$1`, pid, workflowID); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "project.workflow", projectKey, map[string]any{"workflow": workflowID})
	})
}
