package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Automation (SPEC 15): a rule is a trigger, conditions and actions. Rules run in the background from the outbox, as
// "Automation: <rule name>", through the same domain operations a person uses (so workflow conditions, validators,
// screens and field rules apply). Loops are cut by what the events carry: an event caused by a rule names the rules
// that led to it, and a rule never runs on an event it caused, directly or through another rule.

// IssueMatcher decides whether issues match a TQL query (set by the server; the search package supplies one).
type IssueMatcher interface {
	Matches(ctx context.Context, key, query string) (bool, error)
	Keys(ctx context.Context, query string, limit int) ([]string, error)
}

const (
	consumerAutomation   = "automation"
	maxRulesPerProject   = 100
	maxActionsPerRule    = 10
	maxAutoDepth         = 3
	defaultRunsPerHour   = 300
	maxScheduledPerTick  = 200
	autoActorKind        = "automation"
	maxAutomationEventsN = 100
)

type autoKey struct{}

type autoInfo struct {
	Depth int     `json:"depth"`
	Chain []int64 `json:"chain"` // the rules that led to this event, oldest first
}

func withAuto(ctx context.Context, parent autoInfo, rule int64) context.Context {
	return context.WithValue(ctx, autoKey{}, autoInfo{Depth: parent.Depth + 1, Chain: append(append([]int64(nil), parent.Chain...), rule)})
}

// outbox payloads written under an automation context carry where they came from, which is how loops are recognised.
func autoPayload(ctx context.Context, payload any) any {
	info, ok := ctx.Value(autoKey{}).(autoInfo)
	if !ok {
		return payload
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return payload
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return payload
	}
	m["auto"] = info
	return m
}

type AutoRule struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Project    string    `json:"project"` // empty: every project
	Enabled    bool      `json:"enabled"`
	Trigger    Spec      `json:"trigger"`
	Conditions []Spec    `json:"conditions"`
	Actions    []Spec    `json:"actions"`
	CreatedBy  string    `json:"created_by,omitempty"`
	Updated    time.Time `json:"updated"`
}

var triggerTypes = map[string]bool{"issue.created": true, "issue.updated": true, "issue.transitioned": true, "comment.added": true, "schedule": true}
var autoCondKinds = map[string]bool{"query": true, "field": true}
var autoActionKinds = map[string]bool{"transition": true, "assign": true, "set_field": true, "comment": true, "create_subtask": true, "webhook": true, "notify": true}

func (s *Service) projectAdmin(ctx context.Context, a Actor, projectID string) bool {
	return a.Admin || s.role(ctx, a, projectID) == "admin"
}

func (s *Service) validateAutoRule(ctx context.Context, q querier, r AutoRule) error {
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 120 {
		return invalid("a rule needs a name of up to 120 characters")
	}
	t := r.Trigger.str("type")
	if !triggerTypes[t] {
		return invalid("unknown trigger %q (issue.created, issue.updated, issue.transitioned, comment.added, schedule)", t)
	}
	if t == "schedule" {
		em, da := r.Trigger["every_minutes"], r.Trigger.str("daily_at")
		n, isNum := toFloat(em)
		switch {
		case isNum && n >= 5 && n <= 60*24*7 && n == float64(int(n)) && da == "":
		case !isNum && em == nil && da != "":
			if _, ok := parseClock(da); !ok {
				return invalid("daily_at is a time such as 09:00")
			}
			if tz := r.Trigger.str("timezone"); tz != "" {
				if _, err := time.LoadLocation(tz); err != nil {
					return invalid("unknown time zone %q", tz)
				}
			}
		default:
			return invalid("a schedule runs every_minutes (5 to 10080) or daily_at (HH:MM), not both")
		}
		if strings.TrimSpace(r.Trigger.str("query")) == "" {
			return invalid("a schedule needs a query that picks the issues to act on")
		}
		if _, err := tql.Parse(r.Trigger.str("query")); err != nil {
			return invalid("the schedule's query: %v", err)
		}
	}
	for _, c := range r.Conditions {
		switch k := c.str("kind"); {
		case !autoCondKinds[k]:
			return invalid("unknown condition %q (query, field)", k)
		case k == "query":
			if _, err := tql.Parse(c.str("query")); err != nil || c.str("query") == "" {
				return invalid("the condition's query is not valid TQL")
			}
		case k == "field":
			if err := s.validFieldKey(ctx, q, canonKey(c.str("field"))); err != nil {
				return err
			}
		}
	}
	if len(r.Actions) == 0 || len(r.Actions) > maxActionsPerRule {
		return invalid("a rule has 1 to %d actions", maxActionsPerRule)
	}
	for _, a := range r.Actions {
		k := a.str("kind")
		if !autoActionKinds[k] {
			return invalid("unknown action %q (transition, assign, set_field, comment, create_subtask, webhook, notify)", k)
		}
		switch k {
		case "transition":
			if strings.TrimSpace(a.str("transition")) == "" {
				return invalid("the transition action needs the name of a transition")
			}
		case "set_field":
			if err := s.validFieldKey(ctx, q, canonKey(a.str("field"))); err != nil {
				return err
			}
		case "comment":
			if strings.TrimSpace(a.str("text")) == "" {
				return invalid("the comment action needs text")
			}
		case "create_subtask":
			if strings.TrimSpace(a.str("summary")) == "" {
				return invalid("the create_subtask action needs a summary")
			}
		case "webhook":
			if u := a.str("url"); !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
				return invalid("a webhook needs an http or https url")
			}
		case "notify":
			if len(a.strs("users")) == 0 || strings.TrimSpace(a.str("text")) == "" {
				return invalid("the notify action needs users and text")
			}
		}
	}
	return nil
}

func (s *Service) autoScope(ctx context.Context, a Actor, project string) (*string, error) {
	if project == "" {
		if !a.Admin {
			return nil, ErrForbidden // a rule for every project changes things everywhere
		}
		return nil, nil
	}
	pid, err := s.projectID(ctx, project)
	if err != nil {
		return nil, err
	}
	if !s.projectAdmin(ctx, a, pid) {
		if s.role(ctx, a, pid) == "" {
			return nil, ErrNotFound
		}
		return nil, ErrForbidden
	}
	return &pid, nil
}

// sealedAction keeps a webhook secret out of the stored rule: it is encrypted, and the rule shows only that one is set.
func (s *Service) sealActions(ctx context.Context, actions []Spec) error {
	for _, a := range actions {
		if sec, _ := a["secret"].(string); sec != "" && a.str("kind") == "webhook" {
			enc, err := s.seal(ctx, sec)
			if err != nil {
				return err
			}
			delete(a, "secret")
			a["secret_enc"] = fmt.Sprintf("%x", enc)
		}
	}
	return nil
}

func hideSecrets(actions []Spec) []Spec {
	out := make([]Spec, len(actions))
	for i, a := range actions {
		c := Spec{}
		for k, v := range a {
			if k == "secret_enc" {
				c["secret_set"] = true
				continue
			}
			c[k] = v
		}
		out[i] = c
	}
	return out
}

func (s *Service) SaveAutoRule(ctx context.Context, a Actor, id int64, in AutoRule) (AutoRule, error) {
	pid, err := s.autoScope(ctx, a, in.Project)
	if err != nil {
		return in, err
	}
	if in.Conditions == nil {
		in.Conditions = []Spec{}
	}
	if err := s.validateAutoRule(ctx, s.DB, in); err != nil {
		return in, err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if id == 0 && pid != nil {
			var n int
			tx.QueryRow(ctx, `SELECT count(*) FROM automation_rule WHERE project_id::text=$1`, *pid).Scan(&n)
			if n >= maxRulesPerProject {
				return invalid("a project has at most %d automation rules", maxRulesPerProject)
			}
		}
		old := []Spec{}
		if id != 0 { // a rule is edited in the scope it has: its project cannot be changed, so nobody moves a rule into a project they do not run
			var cur *string
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT project_id::text,actions FROM automation_rule WHERE id=$1 FOR UPDATE`, id).Scan(&cur, &raw); err != nil {
				return ErrNotFound
			}
			if (cur == nil) != (pid == nil) || (cur != nil && *cur != *pid) {
				return invalid("a rule stays in its project; create a new one for another project")
			}
			json.Unmarshal(raw, &old)
		}
		// an edit that leaves a webhook secret out keeps the stored one
		for i, act := range in.Actions {
			if act.str("kind") == "webhook" && act["secret"] == nil && act["secret_enc"] == nil && i < len(old) && old[i].str("kind") == "webhook" && old[i].str("url") == act.str("url") {
				act["secret_enc"] = old[i]["secret_enc"]
			}
			delete(act, "secret_set")
		}
		if err := s.sealActions(ctx, in.Actions); err != nil {
			return err
		}
		trig, _ := json.Marshal(in.Trigger)
		cond, _ := json.Marshal(in.Conditions)
		act, _ := json.Marshal(in.Actions)
		if id == 0 {
			if err := tx.QueryRow(ctx, `INSERT INTO automation_rule(name,project_id,enabled,trigger,conditions,actions,created_by) VALUES($1,$2::uuid,$3,$4,$5,$6,$7::uuid) RETURNING id`,
				in.Name, pid, in.Enabled, trig, cond, act, nilIfEmpty(a.ID)).Scan(&id); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE automation_rule SET name=$2,enabled=$3,trigger=$4,conditions=$5,actions=$6,updated_at=$7 WHERE id=$1`, id, in.Name, in.Enabled, trig, cond, act, s.Now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "automation.save", fmt.Sprint(id), map[string]any{"name": in.Name, "project": in.Project, "enabled": in.Enabled})
	})
	if err != nil {
		return in, err
	}
	return s.GetAutoRule(ctx, a, id)
}

func (s *Service) loadRule(ctx context.Context, q querier, id int64) (AutoRule, *string, error) {
	var r AutoRule
	var pid, by *string
	var trig, cond, act []byte
	err := q.QueryRow(ctx, `SELECT r.id,r.name,r.project_id::text,COALESCE(p.key,''),r.enabled,r.trigger,r.conditions,r.actions,r.created_by::text,r.updated_at FROM automation_rule r LEFT JOIN project p ON p.id=r.project_id WHERE r.id=$1`, id).
		Scan(&r.ID, &r.Name, &pid, &r.Project, &r.Enabled, &trig, &cond, &act, &by, &r.Updated)
	if err == pgx.ErrNoRows {
		return r, nil, ErrNotFound
	}
	if err != nil {
		return r, nil, err
	}
	json.Unmarshal(trig, &r.Trigger)
	json.Unmarshal(cond, &r.Conditions)
	json.Unmarshal(act, &r.Actions)
	if by != nil {
		r.CreatedBy = *by
	}
	return r, pid, nil
}

func (s *Service) GetAutoRule(ctx context.Context, a Actor, id int64) (AutoRule, error) {
	r, pid, err := s.loadRule(ctx, s.DB, id)
	if err != nil {
		return r, err
	}
	if pid == nil && !a.Admin || pid != nil && !s.projectAdmin(ctx, a, *pid) {
		if pid != nil && s.role(ctx, a, *pid) == "" {
			return AutoRule{}, ErrNotFound
		}
		return AutoRule{}, ErrForbidden
	}
	r.Actions = hideSecrets(r.Actions)
	return r, nil
}

// ListAutoRules lists a project's rules (project administrators) or, with an empty project, the rules for every project (administrators).
func (s *Service) ListAutoRules(ctx context.Context, a Actor, project string) ([]AutoRule, error) {
	pid, err := s.autoScope(ctx, a, project)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id FROM automation_rule WHERE project_id IS NOT DISTINCT FROM $1::uuid ORDER BY id`, pid)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []AutoRule{}
	for _, id := range ids {
		r, err := s.GetAutoRule(ctx, a, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Service) DeleteAutoRule(ctx context.Context, a Actor, id int64) error {
	if _, err := s.GetAutoRule(ctx, a, id); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM automation_rule WHERE id=$1`, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "automation.delete", fmt.Sprint(id), map[string]any{})
	})
}

type AutoRun struct {
	ID      int64          `json:"id"`
	Issue   string         `json:"issue"`
	State   string         `json:"state"`
	Detail  map[string]any `json:"detail"`
	Started time.Time      `json:"started"`
}

func (s *Service) AutoRuns(ctx context.Context, a Actor, id int64, limit int) ([]AutoRun, error) {
	if _, err := s.GetAutoRule(ctx, a, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id,i.key,r.state,r.detail,r.started_at FROM automation_run r JOIN issue i ON i.id=r.issue_id WHERE r.rule_id=$1 ORDER BY r.id DESC LIMIT $2`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AutoRun{}
	for rows.Next() {
		var x AutoRun
		var raw []byte
		if err := rows.Scan(&x.ID, &x.Issue, &x.State, &raw, &x.Started); err != nil {
			return nil, err
		}
		json.Unmarshal(raw, &x.Detail)
		out = append(out, x)
	}
	return out, rows.Err()
}

// ----- smart values

var smartRe = regexp.MustCompile(`\{\{\s*([a-zA-Z_.]+)\s*\}\}`)

type smartCtx struct {
	issue                                                   *tctx
	summary, status, priority, assignee, reporter, ruleName string
	from, to, actor                                         string
}

func (sc smartCtx) render(text string) string {
	return smartRe.ReplaceAllStringFunc(text, func(m string) string {
		name := smartRe.FindStringSubmatch(m)[1]
		switch name {
		case "issue.key":
			return sc.issue.Key
		case "issue.project":
			return sc.issue.Project
		case "issue.summary":
			return sc.summary
		case "issue.status":
			return sc.status
		case "issue.priority":
			return sc.priority
		case "issue.assignee":
			return sc.assignee
		case "issue.reporter":
			return sc.reporter
		case "rule.name":
			return sc.ruleName
		case "trigger.from":
			return sc.from
		case "trigger.to":
			return sc.to
		case "actor":
			return sc.actor
		}
		return m // unknown names stay as written; nothing is evaluated
	})
}

func (s *Service) smartFor(ctx context.Context, q querier, t *tctx, r AutoRule, e notifyEvent) smartCtx {
	sc := smartCtx{issue: t, status: t.Status, ruleName: r.Name, from: e.Payload.From, to: e.Payload.To, actor: e.Payload.Actor}
	q.QueryRow(ctx, `SELECT i.summary,i.priority,COALESCE(a.name,''),rp.name FROM issue i LEFT JOIN app_user a ON a.id=i.assignee_id JOIN app_user rp ON rp.id=i.reporter_id WHERE i.id=$1`, t.ID).Scan(&sc.summary, &sc.priority, &sc.assignee, &sc.reporter)
	return sc
}

// ----- running rules

type autoEvent struct {
	ID   int64
	Type string
	P    struct {
		Key     string   `json:"key"`
		Actor   string   `json:"actor"`
		ActorID string   `json:"actor_id"`
		Comment string   `json:"comment"`
		From    string   `json:"from"`
		To      string   `json:"to"`
		Fields  []string `json:"fields"`
		Auto    autoInfo `json:"auto"`
	}
}

var autoEventTypes = []string{"issue.created", "issue.updated", "issue.transitioned", "comment.added"}

// ProcessAutomationEvents reads new outbox events and runs the rules they trigger. It returns the number of rule runs started.
func (s *Service) ProcessAutomationEvents(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = maxAutomationEventsN
	}
	var evs []autoEvent
	var last int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO outbox_cursor(consumer,last_id) VALUES($1,COALESCE((SELECT max(id) FROM outbox),0)) ON CONFLICT DO NOTHING`, consumerAutomation); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT last_id FROM outbox_cursor WHERE consumer=$1 FOR UPDATE`, consumerAutomation).Scan(&last); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,type,payload FROM outbox WHERE id>$1 ORDER BY id LIMIT $2`, last, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e autoEvent
			var raw []byte
			if err := rows.Scan(&e.ID, &e.Type, &raw); err != nil {
				return err
			}
			json.Unmarshal(raw, &e.P)
			evs = append(evs, e)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	started := 0
	for _, e := range evs {
		relevant := false
		for _, t := range autoEventTypes {
			relevant = relevant || e.Type == t
		}
		if relevant && e.P.Key != "" {
			n, err := s.automateEvent(ctx, e)
			started += n
			if err != nil {
				return started, err // the cursor stays: the event is tried again (runs already claimed are not repeated)
			}
		}
		last = e.ID
		if _, err := s.DB.Exec(ctx, `UPDATE outbox_cursor SET last_id=$2 WHERE consumer=$1 AND last_id<$2`, consumerAutomation, last); err != nil {
			return started, err
		}
	}
	return started, nil
}

func (s *Service) triggerMatches(r AutoRule, e autoEvent) bool {
	if r.Trigger.str("type") != e.Type {
		return false
	}
	switch e.Type {
	case "issue.updated":
		want := r.Trigger.strs("fields")
		if len(want) == 0 {
			return true
		}
		for _, w := range want {
			for _, f := range e.P.Fields {
				if canonKey(w) == canonKey(f) {
					return true
				}
			}
		}
		return false
	case "issue.transitioned":
		if f := r.Trigger.str("from"); f != "" && !strings.EqualFold(f, e.P.From) {
			return false
		}
		if t := r.Trigger.str("to"); t != "" && !strings.EqualFold(t, e.P.To) {
			return false
		}
	case "comment.added":
		if c := r.Trigger.str("contains"); c != "" {
			var body string
			s.DB.QueryRow(context.Background(), `SELECT body FROM comment WHERE id::text=$1`, e.P.Comment).Scan(&body)
			if !strings.Contains(strings.ToLower(body), strings.ToLower(c)) {
				return false
			}
		}
	}
	return true
}

func (s *Service) automateEvent(ctx context.Context, e autoEvent) (int, error) {
	var issueID, pid string
	var archived *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT id::text,project_id::text,archived_at FROM issue WHERE key=$1 AND deleted_at IS NULL`, e.P.Key).Scan(&issueID, &pid, &archived); err != nil || archived != nil {
		return 0, nil
	}
	rows, err := s.DB.Query(ctx, `SELECT id FROM automation_rule WHERE enabled AND (project_id IS NULL OR project_id::text=$1) ORDER BY id`, pid)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		r, _, err := s.loadRule(ctx, s.DB, id)
		if err != nil {
			continue
		}
		if !s.triggerMatches(r, e) {
			continue
		}
		started, err := s.runRule(ctx, r, issueID, e.ID, e)
		if err != nil {
			return n, err
		}
		if started {
			n++
		}
	}
	return n, nil
}

func (s *Service) finishRun(ctx context.Context, runID int64, state string, detail map[string]any) {
	b, _ := json.Marshal(detail)
	s.DB.Exec(ctx, `UPDATE automation_run SET state=$2,detail=$3 WHERE id=$1`, runID, state, b)
}

func (s *Service) autoActor(ctx context.Context, r AutoRule) Actor {
	var id string
	if r.CreatedBy != "" {
		s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE id::text=$1 AND active`, r.CreatedBy).Scan(&id)
	}
	if id == "" {
		s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE is_admin AND active ORDER BY created_at LIMIT 1`).Scan(&id)
	}
	return Actor{ID: id, Label: "Automation: " + r.Name, Admin: true, Kind: autoActorKind}
}

// runRule claims a run for (rule, issue, event) and executes it. It reports whether this call started a run.
func (s *Service) runRule(ctx context.Context, r AutoRule, issueID string, eventID int64, e autoEvent) (bool, error) {
	var runID int64
	err := s.DB.QueryRow(ctx, `INSERT INTO automation_run(rule_id,issue_id,event_id,state) VALUES($1,$2::uuid,$3,'running') ON CONFLICT (rule_id,issue_id,event_id) DO NOTHING RETURNING id`, r.ID, issueID, eventID).Scan(&runID)
	if err == pgx.ErrNoRows {
		return false, nil // already handled
	}
	if err != nil {
		return false, err
	}
	parent := e.P.Auto
	for _, c := range parent.Chain {
		if c == r.ID {
			s.finishRun(ctx, runID, "loop", map[string]any{"reason": "this rule caused the event", "chain": parent.Chain})
			return true, nil
		}
	}
	if parent.Depth >= maxAutoDepth {
		s.finishRun(ctx, runID, "loop", map[string]any{"reason": fmt.Sprintf("rules triggered rules %d levels deep", parent.Depth), "chain": parent.Chain})
		return true, nil
	}
	var recent int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM automation_run WHERE rule_id=$1 AND started_at > $2 AND state IN ('done','failed')`, r.ID, s.Now().Add(-time.Hour)).Scan(&recent)
	if recent >= defaultRunsPerHour {
		s.finishRun(ctx, runID, "throttled", map[string]any{"reason": fmt.Sprintf("the rule ran %d times in the last hour", recent)})
		return true, nil
	}
	t, err := s.loadTctx(ctx, s.DB, Actor{Admin: true}, issueID, "admin")
	if err != nil {
		s.finishRun(ctx, runID, "failed", map[string]any{"error": err.Error()})
		return true, nil
	}
	if ok, why := s.autoConditions(ctx, r, t); !ok {
		s.finishRun(ctx, runID, "skipped", map[string]any{"reason": why})
		return true, nil
	}
	actx := withAuto(ctx, parent, r.ID)
	actor := s.autoActor(ctx, r)
	done := []string{}
	for i, act := range r.Actions {
		if err := s.runAction(actx, actor, r, act, t, e, runID); err != nil {
			s.finishRun(ctx, runID, "failed", map[string]any{"action": i, "kind": act.str("kind"), "error": err.Error(), "done": done})
			return true, nil
		}
		done = append(done, act.str("kind"))
		if nt, err := s.loadTctx(ctx, s.DB, Actor{Admin: true}, issueID, "admin"); err == nil {
			t = nt // later actions see the effect of earlier ones
		}
	}
	s.finishRun(ctx, runID, "done", map[string]any{"actions": done})
	return true, nil
}

func (s *Service) autoConditions(ctx context.Context, r AutoRule, t *tctx) (bool, string) {
	for _, c := range r.Conditions {
		switch c.str("kind") {
		case "field":
			op := c.str("op")
			if op == "" {
				op = "eq"
			}
			if !ruleHolds(Rule{WhenField: canonKey(c.str("field")), WhenOp: op, WhenValue: c["value"]}, t.Vals, "") {
				return false, fmt.Sprintf("%s is not %s", c.str("field"), op)
			}
		case "query":
			if s.Matcher == nil {
				return false, "search is not available to evaluate the query"
			}
			ok, err := s.Matcher.Matches(ctx, t.Key, c.str("query"))
			if err != nil {
				return false, "the condition's query failed: " + err.Error()
			}
			if !ok {
				return false, "the issue does not match: " + c.str("query")
			}
		}
	}
	return true, ""
}

func (s *Service) userRef(ctx context.Context, t *tctx, ref string) (string, error) {
	switch ref {
	case "", "unassigned":
		return "", nil
	case "reporter":
		return t.Reporter, nil
	case "assignee":
		return t.Assignee, nil
	}
	var id string
	if err := s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE active AND (id::text=$1 OR lower(email)=lower($1))`, strings.TrimSpace(ref)).Scan(&id); err != nil {
		return "", fmt.Errorf("%q is not an active user", ref)
	}
	return id, nil
}

func (s *Service) runAction(ctx context.Context, actor Actor, r AutoRule, a Spec, t *tctx, e autoEvent, runID int64) error {
	sc := s.smartFor(ctx, s.DB, t, r, notifyEvent{})
	sc.from, sc.to, sc.actor = e.P.From, e.P.To, e.P.Actor
	switch a.str("kind") {
	case "transition":
		var tid string
		err := s.DB.QueryRow(ctx, `SELECT t.id::text FROM transition t JOIN issue i ON i.id=$1 JOIN project p ON p.id=i.project_id
			WHERE t.workflow_id=p.workflow_id AND lower(t.name)=lower($2) AND (t.from_status IS NULL OR t.from_status=i.status_id) ORDER BY t.from_status IS NULL LIMIT 1`, t.ID, a.str("transition")).Scan(&tid)
		if err == pgx.ErrNoRows {
			return fmt.Errorf("the transition %q is not available from %q", a.str("transition"), t.Status)
		}
		if err != nil {
			return err
		}
		_, err = s.Transition(ctx, actor, t.Key, tid, nil)
		return err
	case "assign":
		id, err := s.userRef(ctx, t, a.str("to"))
		if err != nil {
			return err
		}
		_, err = s.UpdateFields(ctx, actor, t.Key, nil, false, Changes{Assignee: &id})
		return err
	case "set_field":
		f := canonKey(a.str("field"))
		switch f {
		case "priority":
			v := a.str("value")
			_, err := s.UpdateFields(ctx, actor, t.Key, nil, false, Changes{Priority: &v})
			return err
		case "labels":
			cur, _ := t.Vals["labels"].([]string)
			set := map[string]bool{}
			var out []string
			for _, l := range cur {
				set[l] = true
			}
			if v, ok := a["value"]; ok {
				set = map[string]bool{}
				for _, l := range asStrings(v) {
					set[l] = true
				}
			}
			for _, l := range a.strs("add") {
				set[l] = true
			}
			for _, l := range a.strs("remove") {
				delete(set, l)
			}
			for l := range set {
				out = append(out, l)
			}
			if out == nil {
				out = []string{}
			}
			sortStrings(out)
			_, err := s.UpdateFields(ctx, actor, t.Key, nil, false, Changes{Labels: &out})
			return err
		case "assignee", "description":
			return fmt.Errorf("use the assign action for the assignee; the description is not set by automation")
		}
		val := a["value"]
		if str, ok := val.(string); ok {
			val = sc.render(str)
		}
		_, err := s.UpdateFields(ctx, actor, t.Key, nil, false, Changes{Custom: map[string]any{f: val}})
		return err
	case "comment":
		_, err := s.AddComment(ctx, actor, t.Key, sc.render(a.str("text")), a["internal"] == true)
		return err
	case "create_subtask":
		typ := a.str("type")
		if typ == "" {
			typ = "Subtask"
		}
		_, err := s.CreateIssue(ctx, actor, CreateIn{Project: t.Project, Type: typ, Summary: sc.render(a.str("summary")), Parent: t.Key})
		return err
	case "webhook":
		payload, _ := json.Marshal(map[string]any{"event": "automation", "rule": r.Name, "trigger": e.Type, "key": t.Key, "project": t.Project, "from": e.P.From, "to": e.P.To, "actor": e.P.Actor, "at": s.Now().UTC().Format(time.RFC3339)})
		params := map[string]any{"url": a.str("url")}
		if enc := a.str("secret_enc"); enc != "" {
			params["secret_enc"] = enc
		}
		pj, _ := json.Marshal(params)
		_, err := s.DB.Exec(ctx, `INSERT INTO workflow_job(kind,issue_id,params,payload,next_at) VALUES('webhook',$1,$2,$3,$4)`, t.ID, pj, payload, s.Now())
		return err
	case "notify":
		for _, ref := range a.strs("users") {
			id, err := s.userRef(ctx, t, ref)
			if err != nil {
				return err
			}
			if id == "" || !s.mayReceive(ctx, s.DB, id, t.ProjectID, false) {
				continue
			}
			if _, err := s.DB.Exec(ctx, `INSERT INTO notification(user_id,issue_id,event_id,kind,what,actor_label,created_at) VALUES($1,$2,$3,'automation',$4,$5,$6) ON CONFLICT (user_id,event_id,kind) DO NOTHING`,
				id, t.ID, -runID, sc.render(a.str("text")), actor.Label, s.Now()); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("unknown action %q", a.str("kind"))
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ----- scheduled rules

func scheduleTick(r AutoRule, now time.Time) (int64, bool) {
	if n, ok := toFloat(r.Trigger["every_minutes"]); ok && n >= 1 {
		return now.Unix() / 60 / int64(n), true
	}
	if da := r.Trigger.str("daily_at"); da != "" {
		m, ok := parseClock(da)
		if !ok {
			return 0, false
		}
		loc := time.UTC
		if tz := r.Trigger.str("timezone"); tz != "" {
			if l, err := time.LoadLocation(tz); err == nil {
				loc = l
			}
		}
		l := now.In(loc)
		if l.Hour()*60+l.Minute() < m {
			return 0, false
		}
		return int64(l.Year())*10000 + int64(l.Month())*100 + int64(l.Day()), true // the day, once its time has come
	}
	return 0, false
}

// ProcessAutomationSchedules runs the scheduled rules that are due: once per tick, whichever node claims it, over the
// issues the rule's query picks (at most 200 per tick).
func (s *Service) ProcessAutomationSchedules(ctx context.Context) (int, error) {
	rows, err := s.DB.Query(ctx, `SELECT id FROM automation_rule WHERE enabled AND trigger->>'type'='schedule' ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	runs := 0
	for _, id := range ids {
		r, pid, err := s.loadRule(ctx, s.DB, id)
		if err != nil {
			continue
		}
		tick, due := scheduleTick(r, s.Now())
		if !due || s.Matcher == nil {
			continue
		}
		tag, err := s.DB.Exec(ctx, `UPDATE automation_rule SET last_tick=$2 WHERE id=$1 AND last_tick<$2`, id, tick)
		if err != nil {
			return runs, err
		}
		if tag.RowsAffected() == 0 {
			continue // this tick already ran
		}
		keys, err := s.Matcher.Keys(ctx, r.Trigger.str("query"), maxScheduledPerTick)
		if err != nil {
			continue
		}
		for _, key := range keys {
			var issueID, ipid string
			if s.DB.QueryRow(ctx, `SELECT id::text,project_id::text FROM issue WHERE key=$1 AND deleted_at IS NULL`, key).Scan(&issueID, &ipid) != nil {
				continue
			}
			if pid != nil && *pid != ipid {
				continue // a project's rule acts on that project only
			}
			if started, err := s.runRule(ctx, r, issueID, -tick, autoEvent{Type: "schedule"}); err == nil && started {
				runs++
			}
		}
	}
	return runs, nil
}

// RunAutomation is the worker loop: events, scheduled rules, and runs that were interrupted.
func (s *Service) RunAutomation(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastSched := time.Time{}
	for {
		if _, err := s.ProcessAutomationEvents(ctx, 0); err != nil && ctx.Err() == nil {
			logf("automation: %v", err)
		}
		if time.Since(lastSched) > 20*time.Second {
			lastSched = time.Now()
			if _, err := s.ProcessAutomationSchedules(ctx); err != nil && ctx.Err() == nil {
				logf("automation schedules: %v", err)
			}
			s.DB.Exec(ctx, `UPDATE automation_run SET state='failed',detail='{"error":"interrupted"}' WHERE state='running' AND started_at < $1`, s.Now().Add(-10*time.Minute))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ----- dry run

type DryCondition struct {
	Kind   string `json:"kind"`
	Passed bool   `json:"passed"`
	Why    string `json:"why,omitempty"`
}

type DryAction struct {
	Kind    string `json:"kind"`
	Would   string `json:"would"`
	Problem string `json:"problem,omitempty"`
}

type DryRun struct {
	Issue      string         `json:"issue"`
	Conditions []DryCondition `json:"conditions"`
	WouldRun   bool           `json:"would_run"`
	Actions    []DryAction    `json:"actions"`
}

// DryRunAutoRule shows what a rule would do to one issue right now, and changes nothing. The trigger is assumed to have fired.
func (s *Service) DryRunAutoRule(ctx context.Context, a Actor, id int64, key string) (DryRun, error) {
	out := DryRun{Issue: strings.ToUpper(key), Conditions: []DryCondition{}, Actions: []DryAction{}}
	r, pid, err := s.loadRule(ctx, s.DB, id)
	if err != nil {
		return out, err
	}
	if _, err := s.GetAutoRule(ctx, a, id); err != nil {
		return out, err
	}
	var issueID, ipid string
	if err := s.DB.QueryRow(ctx, `SELECT id::text,project_id::text FROM issue WHERE key=$1 AND deleted_at IS NULL`, out.Issue).Scan(&issueID, &ipid); err != nil {
		return out, ErrNotFound
	}
	if pid != nil && *pid != ipid {
		return out, invalid("%s is not in this rule's project", out.Issue)
	}
	t, err := s.loadTctx(ctx, s.DB, Actor{Admin: true}, issueID, "admin")
	if err != nil {
		return out, err
	}
	out.WouldRun = true
	for _, c := range r.Conditions {
		single := r
		single.Conditions = []Spec{c}
		ok, why := s.autoConditions(ctx, single, t)
		out.Conditions = append(out.Conditions, DryCondition{Kind: c.str("kind"), Passed: ok, Why: why})
		out.WouldRun = out.WouldRun && ok
	}
	sc := s.smartFor(ctx, s.DB, t, r, notifyEvent{})
	for _, act := range r.Actions {
		d := DryAction{Kind: act.str("kind")}
		switch d.Kind {
		case "transition":
			var to string
			err := s.DB.QueryRow(ctx, `SELECT st.name FROM transition t JOIN issue i ON i.id=$1 JOIN project p ON p.id=i.project_id JOIN status st ON st.id=t.to_status
				WHERE t.workflow_id=p.workflow_id AND lower(t.name)=lower($2) AND (t.from_status IS NULL OR t.from_status=i.status_id) LIMIT 1`, t.ID, act.str("transition")).Scan(&to)
			if err != nil {
				d.Problem = fmt.Sprintf("the transition %q is not available from %q", act.str("transition"), t.Status)
			} else {
				d.Would = fmt.Sprintf("move %s from %s to %s", t.Key, t.Status, to)
			}
		case "assign":
			if id, err := s.userRef(ctx, t, act.str("to")); err != nil {
				d.Problem = err.Error()
			} else if id == "" {
				d.Would = "unassign " + t.Key
			} else {
				var n string
				s.DB.QueryRow(ctx, `SELECT name FROM app_user WHERE id::text=$1`, id).Scan(&n)
				d.Would = "assign " + t.Key + " to " + n
			}
		case "set_field":
			d.Would = fmt.Sprintf("set %s on %s", act.str("field"), t.Key)
		case "comment":
			d.Would = "comment: " + sc.render(act.str("text"))
		case "create_subtask":
			d.Would = "create the subtask: " + sc.render(act.str("summary"))
		case "webhook":
			d.Would = "call " + act.str("url")
		case "notify":
			d.Would = fmt.Sprintf("notify %s: %s", strings.Join(act.strs("users"), ", "), sc.render(act.str("text")))
		}
		out.Actions = append(out.Actions, d)
	}
	return out, nil
}
