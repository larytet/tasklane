package domain

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/egress"
)

// Transition rules (SPEC 9). A transition carries conditions (is it offered and allowed to this person on this issue),
// validators (a message when the issue is not ready), in-transaction post-functions (applied atomically with the
// status change) and asynchronous post-functions (queued in the same transaction, delivered afterwards with retries).
// The transition screen lists the fields a person is asked to fill before it runs.

type Spec map[string]any

type TRules struct {
	Conditions    []Spec   `json:"conditions"`
	Validators    []Spec   `json:"validators"`
	PostFunctions []Spec   `json:"post_functions"`
	Screen        []string `json:"screen"`
}

func (r TRules) empty() bool {
	return len(r.Conditions) == 0 && len(r.Validators) == 0 && len(r.PostFunctions) == 0
}

func (sp Spec) str(k string) string { v, _ := sp[k].(string); return v }

func (sp Spec) strs(k string) []string { return asStrings(sp[k]) }

var roleRank = map[string]int{"": 0, "viewer": 1, "member": 2, "admin": 3}

var condKinds = map[string]bool{"role": true, "assignee_only": true, "reporter_only": true, "assignee_or_reporter": true, "field": true, "subtasks_resolved": true}
var valKinds = map[string]bool{"field_required": true, "regex": true, "permission": true, "previous_status": true}
var pfKinds = map[string]bool{"set_field": true, "assign": true, "add_comment": true, "webhook": true, "create_subtask": true}
var asyncKinds = map[string]bool{"webhook": true, "create_subtask": true}

// parseRules reads the stored rules; unknown JSON is treated as no rules rather than failing a transition.
func parseRules(raw []byte) TRules {
	var r TRules
	json.Unmarshal(raw, &r)
	return r
}

// ValidateRules checks a rule set before it is saved: known kinds, required parameters, resolvable fields and safe webhook targets.
func (s *Service) ValidateRules(ctx context.Context, q querier, r TRules) error {
	field := func(sp Spec, k string) error {
		f := canonKey(sp.str(k))
		if f == "" {
			return invalid("%s needs a %q", sp.str("kind"), k)
		}
		return s.validFieldKey(ctx, q, f)
	}
	for _, c := range r.Conditions {
		switch k := c.str("kind"); {
		case !condKinds[k]:
			return invalid("unknown condition %q (conditions: role, assignee_only, reporter_only, assignee_or_reporter, field, subtasks_resolved)", k)
		case k == "role":
			for _, x := range c.strs("roles") {
				if roleRank[x] == 0 {
					return invalid("a role is viewer, member or admin")
				}
			}
			if len(c.strs("roles")) == 0 {
				return invalid("the role condition needs roles")
			}
		case k == "field":
			if err := field(c, "field"); err != nil {
				return err
			}
			if op := c.str("op"); op != "" && !ruleOps[op] {
				return invalid("a field condition operator is eq, ne, in, empty or notempty")
			}
		}
	}
	for _, v := range r.Validators {
		switch k := v.str("kind"); {
		case !valKinds[k]:
			return invalid("unknown validator %q (validators: field_required, regex, permission, previous_status)", k)
		case k == "field_required":
			if err := field(v, "field"); err != nil {
				return err
			}
		case k == "regex":
			if err := field(v, "field"); err != nil {
				return err
			}
			if _, err := regexp.Compile(v.str("pattern")); err != nil || v.str("pattern") == "" {
				return invalid("the regex validator needs a valid pattern")
			}
		case k == "permission":
			if roleRank[v.str("role")] == 0 {
				return invalid("the permission validator needs a role: viewer, member or admin")
			}
		case k == "previous_status":
			if len(v.strs("statuses")) == 0 {
				return invalid("the previous_status validator needs statuses")
			}
		}
	}
	for _, p := range r.PostFunctions {
		switch k := p.str("kind"); {
		case !pfKinds[k]:
			return invalid("unknown post-function %q (post-functions: set_field, assign, add_comment, webhook, create_subtask)", k)
		case k == "set_field":
			if err := field(p, "field"); err != nil {
				return err
			}
		case k == "add_comment":
			if strings.TrimSpace(p.str("text")) == "" {
				return invalid("add_comment needs text")
			}
		case k == "webhook":
			if u := p.str("url"); !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
				return invalid("a webhook needs an http or https url")
			}
		case k == "create_subtask":
			if strings.TrimSpace(p.str("summary")) == "" {
				return invalid("create_subtask needs a summary")
			}
		}
	}
	for _, f := range r.Screen {
		if err := s.validFieldKey(ctx, q, canonKey(f)); err != nil {
			return err
		}
	}
	return nil
}

// tctx is what rules look at: the issue, the person acting, and the field values as they are now.
type tctx struct {
	ID, Key, Project, ProjectID, TypeID string
	Status, Reporter, Assignee          string
	Role                                string
	Actor                               Actor
	Vals                                map[string]any
}

func (s *Service) loadTctx(ctx context.Context, q querier, a Actor, issueID, role string) (*tctx, error) {
	t := &tctx{ID: issueID, Actor: a, Role: role, Vals: map[string]any{}}
	if a.Admin {
		t.Role = "admin"
	}
	var assignee *string
	var priority string
	var labels []string
	err := q.QueryRow(ctx, `SELECT i.key,p.key,p.id::text,i.issue_type_id::text,st.name,i.reporter_id::text,i.assignee_id::text,i.priority,i.labels
		FROM issue i JOIN project p ON p.id=i.project_id JOIN status st ON st.id=i.status_id WHERE i.id=$1`, issueID).Scan(&t.Key, &t.Project, &t.ProjectID, &t.TypeID, &t.Status, &t.Reporter, &assignee, &priority, &labels)
	if err != nil {
		return nil, err
	}
	if assignee != nil {
		t.Assignee = *assignee
	}
	t.Vals["priority"], t.Vals["assignee"] = nonEmpty(priority), nonEmpty(t.Assignee)
	if len(labels) > 0 {
		t.Vals["labels"] = labels
	}
	rows, err := q.Query(ctx, `SELECT field_id,value FROM issue_field_value WHERE issue_id=$1`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var v any
		json.Unmarshal(raw, &v)
		t.Vals[fieldKey(id)] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d := q.QueryRow(ctx, `SELECT body FROM issue_text WHERE issue_id=$1 AND field='description'`, issueID)
	var desc string
	if err := d.Scan(&desc); err == nil {
		t.Vals["description"] = nonEmpty(desc)
	} else if err != pgx.ErrNoRows {
		return nil, err
	}
	return t, nil
}

func (s *Service) checkConditions(ctx context.Context, q querier, specs []Spec, t *tctx) (bool, string) {
	for _, c := range specs {
		switch c.str("kind") {
		case "role":
			ok := false
			for _, r := range c.strs("roles") {
				if roleRank[t.Role] >= roleRank[r] && (r != "admin" || t.Role == "admin") {
					ok = true
				}
			}
			if !ok {
				return false, "it needs the role " + strings.Join(c.strs("roles"), " or ")
			}
		case "assignee_only":
			if t.Assignee != t.Actor.ID {
				return false, "only the assignee can do this"
			}
		case "reporter_only":
			if t.Reporter != t.Actor.ID {
				return false, "only the reporter can do this"
			}
		case "assignee_or_reporter":
			if t.Assignee != t.Actor.ID && t.Reporter != t.Actor.ID {
				return false, "only the assignee or the reporter can do this"
			}
		case "field":
			op := c.str("op")
			if op == "" {
				op = "eq"
			}
			if !ruleHolds(Rule{WhenField: canonKey(c.str("field")), WhenOp: op, WhenValue: c["value"]}, t.Vals, "") {
				return false, fmt.Sprintf("%s must be %s", c.str("field"), op)
			}
		case "subtasks_resolved":
			var open int
			q.QueryRow(ctx, `SELECT count(*) FROM issue c JOIN status st ON st.id=c.status_id WHERE c.parent_id=$1 AND c.deleted_at IS NULL AND st.category<>'done'`, t.ID).Scan(&open)
			if open > 0 {
				return false, fmt.Sprintf("%d subtask(s) are not done", open)
			}
		}
	}
	return true, ""
}

func (s *Service) checkValidators(specs []Spec, t *tctx) error {
	for _, v := range specs {
		f := canonKey(v.str("field"))
		msg := v.str("message")
		switch v.str("kind") {
		case "field_required":
			if isEmptyValue(t.Vals[f]) {
				if msg == "" {
					msg = fmt.Sprintf("%s is required for this transition", fieldLabel(v.str("field")))
				}
				return invalid("%s", msg)
			}
		case "regex":
			val := fmt.Sprint(t.Vals[f])
			if re, err := regexp.Compile(v.str("pattern")); err == nil && !re.MatchString(val) {
				if msg == "" {
					msg = fmt.Sprintf("%s does not match %s", fieldLabel(v.str("field")), v.str("pattern"))
				}
				return invalid("%s", msg)
			}
		case "permission":
			if roleRank[t.Role] < roleRank[v.str("role")] {
				if msg == "" {
					msg = "you need the " + v.str("role") + " role for this transition"
				}
				return invalid("%s", msg)
			}
		case "previous_status":
			ok := false
			for _, st := range v.strs("statuses") {
				if strings.EqualFold(st, t.Status) {
					ok = true
				}
			}
			if !ok {
				if msg == "" {
					msg = fmt.Sprintf("this transition is only allowed from %s", strings.Join(v.strs("statuses"), ", "))
				}
				return invalid("%s", msg)
			}
		}
	}
	return nil
}

func fieldLabel(k string) string {
	if n, ok := builtinFields[canonKey(k)]; ok {
		return n
	}
	return k
}

// runPostFunctions applies the in-transaction functions and queues the asynchronous ones. next is the issue version
// the status change produced; the functions run after it and leave the version where it is (they are part of it).
func (s *Service) runPostFunctions(ctx context.Context, tx pgx.Tx, a Actor, t *tctx, specs []Spec, from, to string, next int64) error {
	for _, p := range specs {
		kind := p.str("kind")
		if asyncKinds[kind] {
			payload := map[string]any{"event": "issue.transitioned", "key": t.Key, "project": t.Project, "from": from, "to": to, "actor": a.Label, "actor_id": a.ID, "at": s.Now().UTC().Format(time.RFC3339)}
			params := map[string]any{}
			for k, v := range p {
				params[k] = v
			}
			if sec, _ := params["secret"].(string); sec != "" { // a webhook secret is stored encrypted, like every other credential
				enc, err := s.seal(ctx, sec)
				if err != nil {
					return err
				}
				delete(params, "secret")
				params["secret_enc"] = hex.EncodeToString(enc)
			}
			pj, _ := json.Marshal(params)
			dj, _ := json.Marshal(payload)
			if _, err := tx.Exec(ctx, `INSERT INTO workflow_job(kind,issue_id,params,payload,next_at) VALUES($1,$2,$3,$4,$5)`, kind, t.ID, pj, dj, s.Now()); err != nil {
				return err
			}
			continue
		}
		switch kind {
		case "set_field":
			if err := s.pfSet(ctx, tx, a, t, canonKey(p.str("field")), p["value"], next); err != nil {
				return invalid("post-function set_field %s: %v", p.str("field"), err)
			}
		case "assign":
			to := p.str("to")
			switch to {
			case "", "unassigned":
				to = ""
			case "reporter":
				to = t.Reporter
			case "actor":
				to = a.ID
			}
			if err := s.pfSet(ctx, tx, a, t, "assignee", nonEmpty(to), next); err != nil {
				return invalid("post-function assign: %v", err)
			}
		case "add_comment":
			if _, err := tx.Exec(ctx, `INSERT INTO comment(issue_id,author_id,body,internal) VALUES($1,$2,$3,$4)`, t.ID, a.ID, strings.NewReplacer("{{from}}", from, "{{to}}", to, "{{key}}", t.Key).Replace(p.str("text")), p["internal"] == true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) pfSet(ctx context.Context, tx pgx.Tx, a Actor, t *tctx, key string, val any, next int64) error {
	bump := func(f string) string {
		return fmt.Sprintf(`field_ver = field_ver || jsonb_build_object('%s', %d::bigint)`, f, next)
	}
	switch key {
	case "priority":
		str, _ := val.(string)
		if !priorities[str] {
			return fmt.Errorf("unknown priority %v", val)
		}
		before := fmt.Sprint(t.Vals["priority"])
		if _, err := tx.Exec(ctx, `UPDATE issue SET priority=$2,`+bump("priority")+` WHERE id=$1`, t.ID, str); err != nil {
			return err
		}
		t.Vals["priority"] = str
		return s.hist(ctx, tx, t.ID, a, "priority", &before, &str)
	case "assignee":
		var id *string
		if !isEmptyValue(val) {
			ref := fmt.Sprint(val)
			var x string
			if err := tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE active AND (id::text=$1 OR lower(email)=lower($1))`, ref).Scan(&x); err != nil {
				return fmt.Errorf("%q is not an active user", ref)
			}
			if err := s.checkAssignable(ctx, tx, t.ProjectID, x); err != nil {
				return err
			}
			id = &x
		}
		if _, err := tx.Exec(ctx, `UPDATE issue SET assignee_id=$2::uuid,`+bump("assignee")+` WHERE id=$1`, t.ID, id); err != nil {
			return err
		}
		before, after := t.Assignee, ""
		if id != nil {
			after = *id
		}
		t.Assignee, t.Vals["assignee"] = after, nonEmpty(after)
		return s.hist(ctx, tx, t.ID, a, "assignee", &before, &after)
	case "labels":
		l := asStrings(val)
		l, err := normLabels(l)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE issue SET labels=$2,`+bump("labels")+` WHERE id=$1`, t.ID, l); err != nil {
			return err
		}
		before, after := fmt.Sprint(t.Vals["labels"]), strings.Join(l, ",")
		t.Vals["labels"] = l
		return s.hist(ctx, tx, t.ID, a, "labels", &before, &after)
	}
	id, ok := parseFieldKey(key)
	if !ok {
		return fmt.Errorf("%q cannot be set by a post-function", key)
	}
	cfgs, err := s.fieldsFor(ctx, tx, t.ProjectID, t.TypeID)
	if err != nil {
		return err
	}
	for _, c := range cfgs {
		if c.ID != id {
			continue
		}
		var v any
		if !isEmptyValue(val) {
			if v, err = s.normalize(ctx, tx, c.FieldDef, c.Options, val, t.ID); err != nil {
				return err
			}
		} else if c.Required {
			return fmt.Errorf("%s is required and cannot be cleared", c.Name)
		}
		if err := s.storeValue(ctx, tx, t.ID, c.FieldDef, v); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE issue SET `+bump(c.Key)+` WHERE id=$1`, t.ID); err != nil {
			return err
		}
		t.Vals[c.Key] = v
		return nil
	}
	return fmt.Errorf("the field does not apply to this issue")
}

// ----- the asynchronous worker

func (s *Service) hookClient() *http.Client {
	pol := egress.Policy{AllowPrivate: s.WebhookAllowPrivate, MaxBody: 1 << 20}
	if s.HookEgress != nil {
		pol = s.HookEgress(pol)
	}
	return pol.Client()
}

// RunWorkflowJobs delivers queued asynchronous post-functions until the context ends.
func (s *Service) RunWorkflowJobs(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if _, err := s.ProcessWorkflowJobs(ctx, 50); err != nil && ctx.Err() == nil {
			logf("workflow jobs: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ProcessWorkflowJobs runs due jobs once and returns how many it finished. A failed job is retried with a growing
// delay and given up after eight attempts; the error stays on the row.
func (s *Service) ProcessWorkflowJobs(ctx context.Context, limit int) (int, error) {
	done := 0
	for n := 0; n < limit; n++ {
		var id, attempts int64
		var kind, issueID string
		var params, payload []byte
		err := s.tx(ctx, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `SELECT id,kind,issue_id::text,params,payload,attempts FROM workflow_job WHERE state='pending' AND next_at<=$1 ORDER BY next_at,id LIMIT 1 FOR UPDATE SKIP LOCKED`, s.Now()).Scan(&id, &kind, &issueID, &params, &payload, &attempts)
			if err != nil {
				return err
			}
			// lease the row for a minute so a slow delivery is not picked up twice
			_, err = tx.Exec(ctx, `UPDATE workflow_job SET next_at=$2 WHERE id=$1`, id, s.Now().Add(time.Minute))
			return err
		})
		if err == pgx.ErrNoRows {
			return done, nil
		}
		if err != nil {
			return done, err
		}
		runErr := s.runJob(ctx, kind, issueID, params, payload)
		if runErr == nil {
			s.DB.Exec(ctx, `UPDATE workflow_job SET state='done',done_at=$2,attempts=attempts+1,last_error='' WHERE id=$1`, id, s.Now())
			done++
			continue
		}
		state, delay := "pending", time.Duration(1<<min(int(attempts), 10))*5*time.Second
		if attempts+1 >= 8 {
			state = "failed"
		}
		s.DB.Exec(ctx, `UPDATE workflow_job SET state=$2,attempts=attempts+1,next_at=$3,last_error=$4 WHERE id=$1`, id, state, s.Now().Add(delay), runErr.Error())
	}
	return done, nil
}

func (s *Service) runJob(ctx context.Context, kind, issueID string, params, payload []byte) error {
	var p Spec
	json.Unmarshal(params, &p)
	switch kind {
	case "webhook":
		secret := ""
		if enc := p.str("secret_enc"); enc != "" {
			b, _ := hex.DecodeString(enc)
			var err error
			if secret, err = s.unseal(ctx, b); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", p.str("url"), bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "tasklane-webhook")
		if secret != "" {
			m := hmac.New(sha256.New, []byte(secret))
			m.Write(payload)
			req.Header.Set("X-Tasklane-Signature", "sha256="+hex.EncodeToString(m.Sum(nil)))
		}
		resp, err := s.hookClient().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("the endpoint answered %d", resp.StatusCode)
		}
		return nil
	case "create_subtask":
		var key, project string
		if err := s.DB.QueryRow(ctx, `SELECT i.key,p.key FROM issue i JOIN project p ON p.id=i.project_id WHERE i.id=$1 AND i.deleted_at IS NULL`, issueID).Scan(&key, &project); err != nil {
			return nil // the issue is gone: nothing to do
		}
		typ := p.str("type")
		if typ == "" {
			typ = "Subtask"
		}
		var pl map[string]any
		json.Unmarshal(payload, &pl)
		actorID, _ := pl["actor_id"].(string)
		sys := Actor{ID: actorID, Label: "workflow", Admin: true} // the subtask is reported by the person whose transition made it
		// idempotent on retry: the job id is part of the summary marker kept in a label-free way, so look for an existing child first
		var n int
		s.DB.QueryRow(ctx, `SELECT count(*) FROM issue c WHERE c.parent_id=$1 AND c.summary=$2 AND c.deleted_at IS NULL`, issueID, p.str("summary")).Scan(&n)
		if n > 0 {
			return nil
		}
		_, err := s.CreateIssue(ctx, sys, CreateIn{Project: project, Type: typ, Summary: p.str("summary"), Parent: key})
		return err
	}
	return fmt.Errorf("unknown job kind %q", kind)
}

// SetTransitionRules replaces the rules of one transition (administrators). The editor saves rules with the whole draft;
// this is the direct path for scripts and tests.
func (s *Service) SetTransitionRules(ctx context.Context, a Actor, transitionID string, r TRules) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	if err := s.ValidateRules(ctx, s.DB, r); err != nil {
		return err
	}
	for i := range r.Screen {
		r.Screen[i] = canonKey(r.Screen[i])
	}
	raw, _ := json.Marshal(r)
	tag, err := s.DB.Exec(ctx, `UPDATE transition SET rules=$2 WHERE id=$1`, transitionID, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
