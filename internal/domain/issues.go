package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Issue struct {
	ID          string     `json:"id"`
	Key         string     `json:"key"`
	Project     string     `json:"project"`
	Type        string     `json:"type"`
	Summary     string     `json:"summary"`
	SummarySeq  int64      `json:"summary_seq"`
	Description string     `json:"description"`
	DescSeq     int64      `json:"description_seq"`
	Status      string     `json:"status"`
	StatusCat   string     `json:"status_category"`
	Priority    string     `json:"priority"`
	Reporter    *User      `json:"reporter"`
	Assignee    *User      `json:"assignee"`
	Labels      []string   `json:"labels"`
	Parent      string     `json:"parent,omitempty"`
	Version     int64      `json:"version"`
	Created     time.Time  `json:"created"`
	Updated     time.Time  `json:"updated"`
	Resolved    *time.Time `json:"resolved,omitempty"`
	Estimate    *float64   `json:"estimate,omitempty"` // points, hours or nothing, by the board's estimation setting
	// Custom holds the custom field values the caller may see, by field key (customfield_<id>)
	Custom map[string]any `json:"custom,omitempty"`
}

type TransitionOpt struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   string `json:"to"`
	// RequiresHuman: the workflow lets only a person take this transition (agents are refused)
	RequiresHuman bool `json:"requires_human,omitempty"`
	// Screen lists the fields to ask for before the transition runs
	Screen []string `json:"screen,omitempty"`
}

// InputRequest is an agent's open question for a person.
type InputRequest struct {
	By       string    `json:"by"`
	Question string    `json:"question"`
	At       time.Time `json:"at"`
}

type IssueView struct {
	Issue       Issue           `json:"issue"`
	Transitions []TransitionOpt `json:"transitions"`
	Comments    []Comment       `json:"comments"`
	Attachments []Attachment    `json:"attachments"`
	Links       []Link          `json:"links"`
	Alias       string          `json:"alias,omitempty"` // the old key the caller used, when it differs from the current one
	Role        string          `json:"role"`
	Presence    []PresenceEntry `json:"presence"`
	Input       *InputRequest   `json:"input_requested,omitempty"`
	Mirror      *MirrorInfo     `json:"mirror,omitempty"` // set on issues mirrored from Jira: read-only, and how fresh the copy is
}

var priorities = map[string]bool{"lowest": true, "low": true, "medium": true, "high": true, "highest": true}

type CreateIn struct {
	Project     string   `json:"project"`
	Type        string   `json:"type"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	Assignee    string   `json:"assignee"`
	Labels      []string `json:"labels"`
	Parent      string   `json:"parent"`
	// Custom sets custom field values by key (customfield_<id>) or by name; defaults fill what is missing and required fields must be set
	Custom map[string]any `json:"custom"`
}

func normLabels(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.ContainsAny(l, " \t,") {
			return nil, invalid("labels cannot contain spaces or commas: %q", l)
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out, nil
}

const maxSummary = 255

func (s *Service) CreateIssue(ctx context.Context, a Actor, in CreateIn) (Issue, error) {
	endWrite, perr := s.admit(ctx, in.Project)
	if perr != nil {
		return Issue{}, perr
	}
	defer endWrite()
	pid, err := s.projectID(ctx, in.Project)
	if err != nil {
		return Issue{}, err
	}
	switch r := s.role(ctx, a, pid); {
	case r == "":
		return Issue{}, ErrNotFound // a project one cannot see answers like one that does not exist
	case !canWrite(r):
		return Issue{}, ErrForbidden
	}
	summary, _ := Sanitize(strings.TrimSpace(in.Summary))
	if summary == "" {
		return Issue{}, invalid("summary is required")
	}
	if len([]rune(summary)) > maxSummary || strings.Contains(summary, "\n") {
		return Issue{}, invalid("summary must be a single line of at most %d characters", maxSummary)
	}
	if in.Type == "" {
		in.Type = "Task"
	}
	if in.Priority == "" {
		in.Priority = "medium"
	}
	if !priorities[in.Priority] {
		return Issue{}, invalid("unknown priority %q", in.Priority)
	}
	labels, err := normLabels(in.Labels)
	if err != nil {
		return Issue{}, err
	}
	desc, removed := Sanitize(in.Description)
	if err := checkSize(desc); err != nil {
		return Issue{}, err
	}
	_ = removed
	var key string
	creatorRole := s.role(ctx, a, pid) // read before the transaction: a pool query inside it would need a second connection
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var typeID string
		if err := tx.QueryRow(ctx, `SELECT id FROM issue_type WHERE lower(name)=lower($1)`, in.Type).Scan(&typeID); err != nil {
			return invalid("unknown issue type %q", in.Type)
		}
		var parentID *string
		var childLevel int
		tx.QueryRow(ctx, `SELECT level FROM issue_type WHERE id=$1`, typeID).Scan(&childLevel)
		customVals, cfgs, cerr := s.prepareCreate(ctx, tx, pid, typeID, creatorRole, a.Admin, in.Custom)
		if cerr != nil {
			return cerr
		}
		if cerr := s.checkCreateForm(ctx, tx, pid, typeID, creatorRole, in, customVals, cfgs); cerr != nil {
			return cerr
		}
		if in.Parent != "" {
			var p string
			var pl int
			if err := tx.QueryRow(ctx, `SELECT i.id,t.level FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.key=$1 AND i.project_id=$2 AND i.deleted_at IS NULL`, strings.ToUpper(in.Parent), pid).Scan(&p, &pl); err != nil {
				return invalid("parent %s not found in this project", in.Parent)
			}
			if pl != childLevel+1 {
				return invalid("a %s cannot be placed under %s: a parent sits exactly one level above its child (Epic, then Story, Task or Bug, then Subtask)", in.Type, in.Parent)
			}
			parentID = &p
		} else if childLevel == -1 {
			return invalid("a Subtask needs a parent")
		}
		var assignee any
		if in.Assignee != "" {
			if err := s.checkAssignable(ctx, tx, pid, in.Assignee); err != nil {
				return err
			}
			assignee = in.Assignee
		}
		var n int64
		var pkey string
		if err := tx.QueryRow(ctx, `UPDATE project SET next_number=next_number+1 WHERE id=$1 RETURNING next_number-1, key`, pid).Scan(&n, &pkey); err != nil {
			return err
		}
		key = pkey + "-" + itoa(n)
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO issue(project_id,number,key,issue_type_id,status_id,priority,reporter_id,assignee_id,labels,parent_id,summary,rank,created_at,updated_at)
			VALUES($1,$2,$3,$4,(SELECT st.id FROM project p JOIN status st ON st.workflow_id=p.workflow_id AND NOT st.retired WHERE p.id=$1 ORDER BY st.position LIMIT 1),$5,$6,$7,$8,$9,$10,$11,$12,$12) RETURNING id`,
			pid, n, key, typeID, in.Priority, a.ID, assignee, labels, parentID, summary, rankFor(n), s.Now()).Scan(&id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO issue_text(issue_id,field,body,updated_by,last_save_id) VALUES($1,'description',$2,$3,'')`, id, desc, nilIfEmpty(a.ID)); err != nil {
			return err
		}
		for f, v := range map[string]string{"summary": summary, "description": desc} {
			if _, err := tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode) VALUES($1,$2,1,$3,$4,$5,'create')`, id, f, v, nilIfEmpty(a.ID), a.Label); err != nil {
				return err
			}
		}
		for _, c := range cfgs {
			if v, ok := customVals[c.ID]; ok {
				if err := s.storeValue(ctx, tx, id, c.FieldDef, v); err != nil {
					return err
				}
			}
		}
		if err := s.hist(ctx, tx, id, a, "created", nil, sp(key)); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+key, "issue.created", map[string]any{"key": key, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return Issue{}, err
	}
	return s.getIssue(ctx, key)
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// rankFor gives new issues a sortable lexorank-style string that appends to the end.
func rankFor(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := []byte{}
	for x := n; x > 0; x /= 36 {
		b = append([]byte{digits[x%36]}, b...)
	}
	pad := 8 - len(b)
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat("0", pad) + string(b)
}

func (s *Service) checkAssignable(ctx context.Context, tx pgx.Tx, pid, userID string) error {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM app_user u WHERE u.id=$2 AND u.active AND (u.is_admin OR EXISTS(SELECT 1 FROM project_member m WHERE m.project_id=$1 AND m.user_id=u.id AND m.role<>'viewer')))`, pid, userID).Scan(&ok)
	if err != nil {
		return invalid("unknown assignee")
	}
	if !ok {
		return invalid("assignee has no access to this project")
	}
	return nil
}

const issueSelect = `SELECT i.id,i.key,p.key,t.name,i.summary,i.summary_seq,COALESCE(d.body,''),COALESCE(d.text_seq,1),st.name,st.category,i.priority,
	i.reporter_id,rp.name,rp.email,i.assignee_id,asg.name,asg.email,i.labels,COALESCE(par.key,''),i.version,i.created_at,i.updated_at,i.resolved_at,i.estimate
	FROM issue i JOIN project p ON p.id=i.project_id JOIN issue_type t ON t.id=i.issue_type_id JOIN status st ON st.id=i.status_id
	LEFT JOIN issue_text d ON d.issue_id=i.id AND d.field='description' JOIN app_user rp ON rp.id=i.reporter_id
	LEFT JOIN app_user asg ON asg.id=i.assignee_id LEFT JOIN issue par ON par.id=i.parent_id `

func scanIssue(row pgx.Row) (Issue, error) {
	var i Issue
	var rid, rname, remail string
	var aid, aname, aemail *string
	err := row.Scan(&i.ID, &i.Key, &i.Project, &i.Type, &i.Summary, &i.SummarySeq, &i.Description, &i.DescSeq, &i.Status, &i.StatusCat, &i.Priority,
		&rid, &rname, &remail, &aid, &aname, &aemail, &i.Labels, &i.Parent, &i.Version, &i.Created, &i.Updated, &i.Resolved, &i.Estimate)
	if err == pgx.ErrNoRows {
		return i, ErrNotFound
	}
	i.Reporter = &User{ID: rid, Name: rname, Email: remail}
	if aid != nil {
		i.Assignee = &User{ID: *aid, Name: *aname, Email: *aemail}
	}
	return i, err
}

// getIssue finds a live issue by its key or by an old key (alias). A trashed issue answers ErrGone.
func (s *Service) getIssue(ctx context.Context, key string) (Issue, error) {
	key = strings.ToUpper(key)
	is, err := scanIssue(s.DB.QueryRow(ctx, issueSelect+`WHERE i.key=$1 AND i.deleted_at IS NULL`, key))
	if err != ErrNotFound {
		return is, err
	}
	var cur string
	if s.DB.QueryRow(ctx, `SELECT i.key FROM issue_key_alias a JOIN issue i ON i.id=a.issue_id WHERE a.old_key=$1 AND i.deleted_at IS NULL`, key).Scan(&cur) == nil {
		return scanIssue(s.DB.QueryRow(ctx, issueSelect+`WHERE i.key=$1 AND i.deleted_at IS NULL`, cur))
	}
	var trashed bool
	if s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM issue i WHERE i.deleted_at IS NOT NULL AND (i.key=$1 OR i.id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1)))`, key).Scan(&trashed) == nil && trashed {
		return is, ErrGone
	}
	return is, ErrNotFound
}

// ResolveKey returns the current key for a key or an old key, or ErrNotFound.
func (s *Service) ResolveKey(ctx context.Context, key string) (string, error) {
	is, err := s.getIssue(ctx, key)
	return is.Key, err
}

// access loads the issue and checks the actor may at least read it; returns the role.
func (s *Service) access(ctx context.Context, a Actor, key string) (Issue, string, error) {
	is, err := s.getIssue(ctx, key)
	if errors.Is(err, ErrGone) { // only people who can see the project learn that an issue is in the trash
		var pid string
		s.DB.QueryRow(ctx, `SELECT project_id FROM issue WHERE key=$1 OR id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1) LIMIT 1`, strings.ToUpper(key)).Scan(&pid)
		if pid == "" || s.role(ctx, a, pid) == "" {
			return is, "", ErrNotFound
		}
		return is, "", ErrGone
	}
	if err != nil {
		return is, "", err
	}
	pid, err := s.projectID(ctx, is.Project)
	if err != nil {
		return is, "", err
	}
	r := s.role(ctx, a, pid)
	if r == "" {
		return is, "", ErrNotFound // do not reveal existence
	}
	if !a.Admin { // a Jira security level that Tasklane cannot evaluate: hidden from everyone but administrators
		var restricted bool
		s.DB.QueryRow(ctx, `SELECT restricted FROM issue WHERE id=$1`, is.ID).Scan(&restricted)
		if restricted {
			return is, "", ErrNotFound
		}
	}
	return is, r, nil
}

// View is the single bootstrap call for the ticket screen (DESIGN 11.1).
func (s *Service) View(ctx context.Context, a Actor, key string) (IssueView, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return IssueView{}, err
	}
	v := IssueView{Issue: is, Role: role, Transitions: []TransitionOpt{}}
	rows, err := s.DB.Query(ctx, `SELECT t.id,t.name,st.name,t.requires_human,t.rules FROM issue i JOIN project p ON p.id=i.project_id
		JOIN transition t ON t.workflow_id=p.workflow_id AND (t.from_status IS NULL OR t.from_status=i.status_id) JOIN status st ON st.id=t.to_status
		WHERE i.id=$1 ORDER BY st.position, t.name`, is.ID)
	if err != nil {
		return v, err
	}
	var tc *tctx
	for rows.Next() {
		var t TransitionOpt
		var raw []byte
		rows.Scan(&t.ID, &t.Name, &t.To, &t.RequiresHuman, &raw)
		if rules := parseRules(raw); len(rules.Conditions) > 0 || len(rules.Screen) > 0 {
			if len(rules.Conditions) > 0 { // a transition whose conditions are not met is not offered
				if tc == nil {
					if tc, err = s.loadTctx(ctx, s.DB, a, is.ID, role); err != nil {
						rows.Close()
						return v, err
					}
				}
				if ok, _ := s.checkConditions(ctx, s.DB, rules.Conditions, tc); !ok {
					continue
				}
			}
			t.Screen = rules.Screen
		}
		v.Transitions = append(v.Transitions, t)
	}
	rows.Close()
	if v.Comments, err = s.listComments(ctx, is.ID, canWrite(role)); err != nil {
		return v, err
	}
	if v.Attachments, err = s.listAttachments(ctx, is.ID); err != nil {
		return v, err
	}
	if v.Links, err = s.listLinks(ctx, is.ID); err != nil {
		return v, err
	}
	if !strings.EqualFold(key, is.Key) {
		v.Alias = strings.ToUpper(key)
	}
	v.Presence = s.Presence.List(is.Key, a.ID)
	var ptid, ppid string
	s.DB.QueryRow(ctx, `SELECT i.issue_type_id::text,i.project_id::text FROM issue i WHERE i.id=$1`, is.ID).Scan(&ptid, &ppid)
	if cv, err := s.CustomValues(ctx, a, is.ID, ppid, ptid, role); err == nil && len(cv) > 0 {
		v.Issue.Custom = cv
	}
	v.Input = s.inputRequest(ctx, is.ID)
	if v.Mirror = s.mirrorInfo(ctx, is.Project); v.Mirror != nil {
		s.DB.QueryRow(ctx, `SELECT COALESCE(origin_key,'') FROM issue WHERE id=$1`, is.ID).Scan(&v.Mirror.IssueKey)
	}
	return v, nil
}

type Changes struct {
	Priority *string   `json:"priority,omitempty"`
	Assignee *string   `json:"assignee,omitempty"` // "" clears
	Labels   *[]string `json:"labels,omitempty"`
	// Estimate sets the estimate (not negative); ClearEstimate removes it
	Estimate      *float64 `json:"estimate,omitempty"`
	ClearEstimate bool     `json:"clear_estimate,omitempty"`
	// Custom sets custom field values by key or name; a null value clears the field
	Custom map[string]any `json:"custom,omitempty"`
}

// UpdateFields applies scalar changes. baseVersion is the issue version the caller last saw
// (nil = unconditional). Different fields merge; a field someone else changed since the base
// is reported as a conflict and nothing is applied (SPEC 4.5).
func (s *Service) UpdateFields(ctx context.Context, a Actor, key string, baseVersion *int64, strict bool, ch Changes) (Issue, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return is, err
	}
	if !canWrite(role) {
		return is, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return is, perr
	}
	defer endWrite()
	pid, err := s.projectID(ctx, is.Project)
	if err != nil {
		return is, err
	}
	changed := map[string]string{}
	if ch.Priority != nil {
		if !priorities[*ch.Priority] {
			return is, invalid("unknown priority %q", *ch.Priority)
		}
		changed["priority"] = *ch.Priority
	}
	var labels []string
	if ch.Labels != nil {
		if labels, err = normLabels(*ch.Labels); err != nil {
			return is, err
		}
		changed["labels"] = strings.Join(labels, ",")
	}
	if ch.Assignee != nil {
		changed["assignee"] = *ch.Assignee
	}
	if ch.Estimate != nil {
		if *ch.Estimate < 0 || *ch.Estimate > 99999 {
			return is, invalid("an estimate is between 0 and 99999")
		}
		changed["estimate"] = fmt.Sprint(*ch.Estimate)
	} else if ch.ClearEstimate {
		changed["estimate"] = ""
	}
	type customChange struct {
		f     FieldConfig
		v     any
		clear bool
	}
	var customs []customChange
	if len(ch.Custom) > 0 {
		var typeID string
		s.DB.QueryRow(ctx, `SELECT id::text FROM issue_type WHERE lower(name)=lower($1)`, is.Type).Scan(&typeID)
		cfgs, err := s.FieldsFor(ctx, pid, typeID)
		if err != nil {
			return is, err
		}
		byKey := map[string]FieldConfig{}
		for _, c := range cfgs {
			byKey[c.Key] = c
			byKey[strings.ToLower(c.Name)] = c
		}
		for k, raw := range ch.Custom {
			c, ok := byKey[strings.ToLower(k)]
			if !ok {
				return is, invalid("the field %q does not apply to this issue", k)
			}
			if !roleIn(c.EditRoles, role, a.Admin) {
				return is, invalid("%s: your role may not change this field", c.Name)
			}
			if isEmptyValue(raw) {
				if c.Required {
					return is, invalid("%s is required and cannot be cleared", c.Name)
				}
				customs = append(customs, customChange{f: c, clear: true})
				changed[c.Key] = "cleared"
				continue
			}
			v, err := s.normalize(ctx, s.DB, c.FieldDef, c.Options, raw, is.ID)
			if err != nil {
				return is, invalid("%s: %v", c.Name, err)
			}
			customs = append(customs, customChange{f: c, v: v})
			j, _ := json.Marshal(v)
			changed[c.Key] = string(j)
		}
	}
	if len(changed) == 0 {
		return is, invalid("no changes")
	}
	if err := s.checkEditForm(ctx, a, is, role, pid, ch); err != nil {
		return is, err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var cur int64
		var fv map[string]int64
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT version,field_ver FROM issue WHERE id=$1 FOR UPDATE`, is.ID).Scan(&cur, &raw); err != nil {
			return err
		}
		json.Unmarshal(raw, &fv)
		if fv == nil {
			fv = map[string]int64{}
		}
		if baseVersion != nil {
			var stale []string
			for f := range changed {
				if strict && cur != *baseVersion {
					stale = append(stale, f)
				} else if !strict && fv[f] > *baseVersion {
					stale = append(stale, f)
				}
			}
			if len(stale) > 0 {
				sort.Strings(stale)
				return &VersionConflict{CurrentVersion: cur, Fields: stale}
			}
		}
		next := cur + 1
		sets := []string{"version=$2", "updated_at=now()"}
		args := []any{is.ID, next}
		add := func(col string, v any) { args = append(args, v); sets = append(sets, col+"=$"+itoa(int64(len(args)))) }
		if p, ok := changed["priority"]; ok {
			if p != is.Priority {
				add("priority", p)
				s.hist(ctx, tx, is.ID, a, "priority", &is.Priority, &p)
				fv["priority"] = next
			}
		}
		if _, ok := changed["labels"]; ok {
			old, nw := strings.Join(is.Labels, ","), strings.Join(labels, ",")
			if old != nw {
				add("labels", labels)
				s.hist(ctx, tx, is.ID, a, "labels", &old, &nw)
				fv["labels"] = next
			}
		}
		if as, ok := changed["assignee"]; ok {
			oldID := ""
			oldName := ""
			if is.Assignee != nil {
				oldID, oldName = is.Assignee.ID, is.Assignee.Name
			}
			if as != oldID {
				newName := ""
				var val any
				if as != "" {
					if err := s.checkAssignable(ctx, tx, pid, as); err != nil {
						return err
					}
					tx.QueryRow(ctx, `SELECT name FROM app_user WHERE id=$1`, as).Scan(&newName)
					val = as
				}
				add("assignee_id", val)
				s.hist(ctx, tx, is.ID, a, "assignee", &oldName, &newName)
				fv["assignee"] = next
			}
		}
		if es, ok := changed["estimate"]; ok {
			old := ""
			if is.Estimate != nil {
				old = fmt.Sprint(*is.Estimate)
			}
			if es != old {
				var val any
				if es != "" {
					val = *ch.Estimate
				}
				add("estimate", val)
				s.hist(ctx, tx, is.ID, a, "estimate", &old, &es)
				fv["estimate"] = next
			}
		}
		for _, cc := range customs {
			var oldRaw []byte
			tx.QueryRow(ctx, `SELECT value FROM issue_field_value WHERE issue_id=$1 AND field_id=$2`, is.ID, cc.f.ID).Scan(&oldRaw)
			var newRaw []byte
			if !cc.clear {
				newRaw, _ = json.Marshal(cc.v)
			}
			if string(oldRaw) == string(newRaw) {
				continue
			}
			var val any
			if !cc.clear {
				val = cc.v
			}
			if err := s.storeValue(ctx, tx, is.ID, cc.f.FieldDef, val); err != nil {
				return err
			}
			fv[cc.f.Key] = next
			if cc.f.TrackHistory {
				var before, after *string
				if len(oldRaw) > 0 {
					b := string(oldRaw)
					before = &b
				}
				if len(newRaw) > 0 {
					af := string(newRaw)
					after = &af
				}
				tx.Exec(ctx, `INSERT INTO issue_history(issue_id,field,before,after,actor_id,actor_label,at) VALUES($1,$2,$3,$4,$5,$6,$7)`, is.ID, cc.f.Key, before, after, nilIfEmpty(a.ID), a.Label, s.Now())
			}
		}
		fvj, _ := json.Marshal(fv)
		add("field_ver", fvj)
		if _, err := tx.Exec(ctx, `UPDATE issue SET `+strings.Join(sets, ",")+` WHERE id=$1`, args...); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+is.Key, "issue.updated", map[string]any{"key": is.Key, "fields": keys(changed), "version": next, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return is, err
	}
	s.touchLease(ctx, a, is.ID)
	out, gerr := s.getIssue(ctx, key)
	if gerr == nil {
		s.emit(Event{Type: "issue.updated", Issue: key, Fields: keys(changed), Version: out.Version, Actor: a.Label, ActorID: a.ID})
	}
	return out, gerr
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Transition moves the issue along a workflow transition valid from its current status.
func (s *Service) Transition(ctx context.Context, a Actor, key, transitionID string, baseVersion *int64) (Issue, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return is, err
	}
	if !canWrite(role) {
		return is, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return is, perr
	}
	defer endWrite()
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var cur int64
		var raw []byte
		var statusID string
		if err := tx.QueryRow(ctx, `SELECT version,field_ver,status_id FROM issue WHERE id=$1 FOR UPDATE`, is.ID).Scan(&cur, &raw, &statusID); err != nil {
			return err
		}
		var fv map[string]int64
		json.Unmarshal(raw, &fv)
		if fv == nil {
			fv = map[string]int64{}
		}
		if baseVersion != nil && fv["status"] > *baseVersion {
			return &VersionConflict{CurrentVersion: cur, Fields: []string{"status"}}
		}
		var to, toName, cat, fromName, tname string
		var human bool
		var rulesRaw []byte
		err := tx.QueryRow(ctx, `SELECT t.to_status,st.name,st.category,(SELECT name FROM status WHERE id=$3),t.requires_human,t.rules,t.name FROM transition t JOIN status st ON st.id=t.to_status
			WHERE t.id=$1 AND (t.from_status IS NULL OR t.from_status=$2) AND t.workflow_id=(SELECT p.workflow_id FROM issue i JOIN project p ON p.id=i.project_id WHERE i.id=$4)`,
			transitionID, statusID, statusID, is.ID).Scan(&to, &toName, &cat, &fromName, &human, &rulesRaw, &tname)
		if err == pgx.ErrNoRows {
			return invalid("transition is not available from status %q", is.Status)
		}
		if err != nil {
			return err
		}
		if human && a.Kind == "agent" {
			return invalid("%q needs a person: agents cannot take this transition (use request_human_input to ask for review)", toName)
		}
		rules := parseRules(rulesRaw)
		var tc *tctx
		if !rules.empty() {
			if tc, err = s.loadTctx(ctx, tx, a, is.ID, role); err != nil {
				return err
			}
			if ok, why := s.checkConditions(ctx, tx, rules.Conditions, tc); !ok {
				return invalid("%q is not available: %s", tname, why)
			}
			if err := s.checkValidators(rules.Validators, tc); err != nil {
				return err
			}
		}
		next := cur + 1
		fv["status"] = next
		fvj, _ := json.Marshal(fv)
		resolved := "NULL"
		if cat == "done" {
			resolved = "$5"
		}
		args := []any{is.ID, to, next, fvj}
		if cat == "done" {
			args = append(args, s.Now())
		}
		if _, err := tx.Exec(ctx, `UPDATE issue SET status_id=$2,version=$3,field_ver=$4,updated_at=now(),resolved_at=`+resolved+` WHERE id=$1`, args...); err != nil {
			return err
		}
		s.hist(ctx, tx, is.ID, a, "status", &fromName, &toName)
		if tc != nil && len(rules.PostFunctions) > 0 {
			if err := s.runPostFunctions(ctx, tx, a, tc, rules.PostFunctions, fromName, toName, next); err != nil {
				return err
			}
		}
		return outbox(ctx, tx, "issue:"+is.Key, "issue.transitioned", map[string]any{"key": is.Key, "from": fromName, "to": toName, "version": next, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return is, err
	}
	s.touchLease(ctx, a, is.ID)
	out, gerr := s.getIssue(ctx, key)
	if gerr == nil {
		s.emit(Event{Type: "issue.transitioned", Issue: key, Fields: []string{"status"}, Version: out.Version, Actor: a.Label, ActorID: a.ID})
	}
	return out, gerr
}

type HistoryEntry struct {
	Field  string    `json:"field"`
	Before *string   `json:"before"`
	After  *string   `json:"after"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

func (s *Service) History(ctx context.Context, a Actor, key string) ([]HistoryEntry, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT field,before,after,actor_label,at FROM issue_history WHERE issue_id=$1 ORDER BY id`, is.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEntry{}
	for rows.Next() {
		var h HistoryEntry
		rows.Scan(&h.Field, &h.Before, &h.After, &h.Actor, &h.At)
		out = append(out, h)
	}
	return out, rows.Err()
}
