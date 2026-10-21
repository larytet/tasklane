package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/credvault"
	"github.com/larytet/tasklane/internal/jira"
	"github.com/larytet/tasklane/internal/text"
)

// Jira write-through (SPEC 18.1.1): a person edits a mirrored issue in Tasklane, the edit is made in Jira with that person's own
// token, and the mirror changes only after Jira accepted it. The save sequence is: find the person's token, read the fields about
// to change from Jira and compare them with what the person saw (WT-08), send the edit, read the issue back and apply it exactly as
// the sync would (WT-11). Nothing is queued and nothing is retried on the person's behalf.

// JiraTokenPage is where a person creates an Atlassian API token.
const JiraTokenPage = "https://id.atlassian.com/manage-profile/security/api-tokens"

// JiraUserTokenTTL is how long a personal token stays in memory without being supplied again (WT-06).
const JiraUserTokenTTL = 8 * time.Hour

// JiraWriteTimeout bounds one save, shorter than the page's own patience (WT-13).
var JiraWriteTimeout = 20 * time.Second

// ErrTokenRequired: this node holds no live Jira token for the person; the UI asks for one and repeats the save (WT-04).
type ErrTokenRequired struct{ ConnectionID, Connection, SiteURL, TokenPage string }

func (e *ErrTokenRequired) Error() string {
	return fmt.Sprintf("saving to Jira needs your personal Jira API token (create one at %s and paste it here; it is kept in memory only)", e.TokenPage)
}

// JiraFieldConflict is one field that someone changed in Jira since the person loaded it.
type JiraFieldConflict struct {
	Field string `json:"field"`
	Jira  string `json:"jira"` // the value Jira has now, as shown
	Base  string `json:"base"` // the value the person started from, as shown
	// Current is Jira's value in the form a save's Changes.Base takes, so "overwrite with mine" can say what it has now looked at.
	Current any        `json:"current,omitempty"`
	By      string     `json:"by,omitempty"`
	At      *time.Time `json:"at,omitempty"`
}

// JiraConflict: a save that would overwrite a change made in Jira. Nothing was sent (WT-08).
type JiraConflict struct {
	Issue  string              `json:"issue"`
	Fields []JiraFieldConflict `json:"fields"`
}

func (e *JiraConflict) Error() string {
	names := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		names[i] = f.Field
	}
	return fmt.Sprintf("%s was changed in Jira since you opened it (%s); nothing was saved", e.Issue, strings.Join(names, ", "))
}

// JiraRejected: Jira refused the write (a validator, a permission, a transition that is not available, a deleted issue) or the edit
// cannot be made without loss. Status is Jira's HTTP status, 0 when Tasklane refused before asking.
type JiraRejected struct {
	Issue    string            `json:"issue"`
	Status   int               `json:"jira_status"`
	Messages []string          `json:"messages"`
	Fields   map[string]string `json:"fields,omitempty"`
}

func (e *JiraRejected) Error() string {
	parts := append([]string{}, e.Messages...)
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	if len(parts) == 0 {
		parts = []string{"Jira refused the change"}
	}
	if e.Status > 0 {
		return fmt.Sprintf("Jira refused the change on %s (%d): %s", e.Issue, e.Status, strings.Join(parts, "; "))
	}
	return fmt.Sprintf("%s cannot be changed from Tasklane: %s", e.Issue, strings.Join(parts, "; "))
}

// JiraUnavailable: Jira is slow, down or rate limiting; the draft is kept and the mirror is unchanged (WT-13).
type JiraUnavailable struct {
	RetryAfter time.Duration
	Cause      string
}

func (e *JiraUnavailable) Error() string { return "Jira could not be reached: " + e.Cause }

// wtTarget is a mirrored project that writes through.
type wtTarget struct {
	jiraConnInfo
	pid, pkey string
}

// writeThrough returns the target of a project in write_through mode, or nil.
func (s *Service) writeThrough(ctx context.Context, projectKey string) *wtTarget {
	var t wtTarget
	var connID string
	err := s.DB.QueryRow(ctx, `SELECT p.id::text,p.key,jp.connection_id::text FROM project p JOIN jira_project jp ON jp.project_id=p.id WHERE p.key=$1 AND jp.mode='write_through'`, strings.ToUpper(projectKey)).Scan(&t.pid, &t.pkey, &connID)
	if err != nil {
		return nil
	}
	ci, err := s.jiraConn(ctx, connID)
	if err != nil {
		return nil
	}
	t.jiraConnInfo = ci
	return &t
}

func (t *wtTarget) tokenRequired() error {
	return &ErrTokenRequired{ConnectionID: t.id, Connection: t.name, SiteURL: t.site, TokenPage: JiraTokenPage}
}

// notWritable is the refusal for what the first phase does not write (WT-10): the typed mirror error with the Jira address.
func (t *wtTarget) notWritable() error { return &ReadOnlyMirror{Project: t.pkey, SiteURL: t.site} }

// client returns a client that carries the person's own token. There is no fallback to the service credential for writes (WT-05).
func (s *Service) wtClient(a Actor, t *wtTarget) (*jira.Client, error) {
	if a.ID == "" || a.Kind == "agent" {
		return nil, t.notWritable()
	}
	cr, ok := s.vault().User(a.ID, t.id)
	if !ok {
		return nil, t.tokenRequired()
	}
	return s.clientFor(t.jiraConnInfo, cr), nil
}

// wtErr turns what the Jira client returned into the typed errors of the API. A 401 discards the person's token (WT-06).
func (s *Service) wtErr(a Actor, t *wtTarget, issue string, err error) error {
	if err == nil {
		return nil
	}
	var rl *jira.ErrRateLimited
	var st *jira.ErrStatus
	switch {
	case errors.As(err, &rl):
		ra := rl.Until.Sub(s.Now())
		if ra < time.Second {
			ra = time.Second
		}
		return &JiraUnavailable{RetryAfter: ra, Cause: "Jira is limiting requests"}
	case errors.As(err, &st):
		switch {
		case st.Status == 401:
			s.vault().DropUserConn(a.ID, t.id)
			return t.tokenRequired()
		case st.Status >= 500:
			return &JiraUnavailable{Cause: fmt.Sprintf("Jira answered %d", st.Status)}
		}
		rj := &JiraRejected{Issue: issue, Status: st.Status}
		var body struct {
			Messages []string          `json:"errorMessages"`
			Errors   map[string]string `json:"errors"`
		}
		if json.Unmarshal([]byte(st.Body), &body) == nil {
			rj.Messages, rj.Fields = body.Messages, body.Errors
		}
		if len(rj.Messages) == 0 && len(rj.Fields) == 0 {
			rj.Messages = []string{strings.TrimSpace(st.Body)}
			if rj.Messages[0] == "" {
				rj.Messages = nil
			}
		}
		return rj
	}
	var rj *JiraRejected
	var jc *JiraConflict
	if errors.As(err, &rj) || errors.As(err, &jc) {
		return err
	}
	return &JiraUnavailable{Cause: err.Error()}
}

// wtDone records the outcome of a write attempt in the audit log (WT-14): who, which issue and fields, and what came of it; never a token.
func (s *Service) wtDone(ctx context.Context, a Actor, issueKey string, fields []string, err error) {
	outcome, status := "accepted", 0
	var jc *JiraConflict
	var rj *JiraRejected
	var ju *JiraUnavailable
	var tr *ErrTokenRequired
	switch {
	case err == nil:
	case errors.As(err, &tr):
		return // no write was attempted
	case errors.As(err, &jc):
		outcome = "conflict"
	case errors.As(err, &rj):
		outcome, status = "rejected", rj.Status
	case errors.As(err, &ju):
		outcome = "failed"
	default:
		return // a refusal before anything was asked of Jira (validation, forbidden)
	}
	s.auditDirect(context.WithoutCancel(ctx), a, "jira.write", issueKey, map[string]any{"fields": fields, "outcome": outcome, "jira_status": status})
}

func (s *Service) originKey(ctx context.Context, issueID, fallback string) string {
	var k *string
	s.DB.QueryRow(ctx, `SELECT origin_key FROM issue WHERE id::text=$1`, issueID).Scan(&k)
	if k != nil && *k != "" {
		return *k
	}
	return fallback
}

// lastJiraChange finds who changed a field last and when, from the changelog the check asked for.
func lastJiraChange(ji jira.Issue, field string) (string, *time.Time) {
	var best time.Time
	by := ""
	hs, _ := jmap(ji, "changelog")["histories"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		items, _ := hm["items"].([]any)
		for _, it := range items {
			im, _ := it.(map[string]any)
			if strings.EqualFold(jstr(im, "field"), field) {
				if at := jiraTime(hm["created"]); at.After(best) {
					best, by = at, jstr(jmap(hm, "author"), "displayName")
				}
			}
		}
	}
	if best.IsZero() {
		return "", nil
	}
	return by, &best
}

// wtReadback reads the issue back after Jira accepted a write and applies it exactly as the sync does, in one transaction per issue
// (WT-11). It reads with the sync's credential when this node has it, else with the person's own. If it fails the write still stands in
// Jira: the sync is woken and catches up, and the person is not told the save failed.
func (s *Service) wtReadback(ctx context.Context, t *wtTarget, person *jira.Client, jiraKey string) {
	cl := person
	if cr, ok := s.vault().Service(t.id); ok {
		cl = s.clientFor(t.jiraConnInfo, cr)
	}
	apply := func() error {
		ji, err := cl.IssueFields(ctx, jiraKey, jiraFields, "changelog")
		if err != nil {
			return err
		}
		end, perr := s.admitGate(ctx, t.pkey)
		if perr != nil {
			return perr
		}
		defer end()
		if _, err := s.applyJiraIssue(ctx, cl, t.pid, t.pkey, t.actor, ji); err != nil {
			return err
		}
		s.resolveJiraParents(ctx, t.pid)
		return nil
	}
	if err := apply(); err != nil {
		s.DB.Exec(context.WithoutCancel(ctx), `UPDATE jira_project SET wake=true WHERE project_id::text=$1`, t.pid)
	}
}

func (s *Service) jiraAccountOf(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	if acct, ok := strings.CutPrefix(userID, "jira:"); ok { // a base that names Jira's own account, for a person Tasklane cannot match
		return acct
	}
	var acct *string
	s.DB.QueryRow(ctx, `SELECT jira_account_id FROM app_user WHERE id::text=$1`, userID).Scan(&acct)
	if acct == nil {
		return ""
	}
	return *acct
}

// userOfJiraAccount is the Tasklane person linked to a Jira account, or "".
func (s *Service) userOfJiraAccount(ctx context.Context, acct string) string {
	var id string
	s.DB.QueryRow(ctx, `SELECT id::text FROM app_user WHERE jira_account_id=$1`, acct).Scan(&id)
	return id
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
	a, b = sortedCopy(a), sortedCopy(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func jiraLabels(f map[string]any) []string {
	out := []string{}
	if arr, ok := f["labels"].([]any); ok {
		for _, l := range arr {
			if ls, ok := l.(string); ok && ls != "" {
				out = append(out, ls)
			}
		}
	}
	return out
}

func jiraPriorityName(ours string) string {
	if ours == "" {
		return ""
	}
	return strings.ToUpper(ours[:1]) + ours[1:]
}

// wtUpdateFields writes priority, assignee and labels through to Jira (WT-08, WT-10). The base the person started from is the
// request's own (Changes.Base) when it carries one, and what the mirror holds now otherwise.
func (s *Service) wtUpdateFields(ctx context.Context, a Actor, is Issue, t *wtTarget, ch Changes) (Issue, error) {
	if ch.Estimate != nil || ch.ClearEstimate || len(ch.Custom) > 0 {
		return is, t.notWritable()
	}
	var fields []string
	if ch.Priority != nil {
		if !priorities[*ch.Priority] {
			return is, invalid("unknown priority %q", *ch.Priority)
		}
		fields = append(fields, "priority")
	}
	var labels []string
	if ch.Labels != nil {
		var err error
		if labels, err = normLabels(*ch.Labels); err != nil {
			return is, err
		}
		fields = append(fields, "labels")
	}
	assigneeAcct := ""
	if ch.Assignee != nil {
		if *ch.Assignee != "" {
			if assigneeAcct = s.jiraAccountOf(ctx, *ch.Assignee); assigneeAcct == "" {
				return is, invalid("that person has no Jira account linked, so Jira cannot be told to assign the issue to them")
			}
		}
		fields = append(fields, "assignee")
	}
	if len(fields) == 0 {
		return is, invalid("no changes")
	}
	cl, err := s.wtClient(a, t)
	if err != nil {
		return is, err
	}
	ctx, cancel := context.WithTimeout(ctx, JiraWriteTimeout)
	defer cancel()
	key := s.originKey(ctx, is.ID, is.Key)
	out, err := func() (Issue, error) {
		cur, err := cl.IssueFields(ctx, key, []string{"priority", "assignee", "labels", "updated"}, "changelog")
		if err != nil {
			return is, s.wtErr(a, t, is.Key, err)
		}
		f := cur.Fields()
		// the value each field had when the person loaded it
		var conflicts []JiraFieldConflict
		add := func(field, jiraShown, baseShown string, current any) {
			by, at := lastJiraChange(cur, field)
			conflicts = append(conflicts, JiraFieldConflict{Field: field, Jira: jiraShown, Base: baseShown, Current: current, By: by, At: at})
		}
		for _, field := range fields {
			switch field {
			case "priority":
				base := is.Priority
				if ch.Base != nil && ch.Base.Priority != nil {
					base = *ch.Base.Priority
				}
				now := jiraPriority[strings.ToLower(jstr(jmap(f, "priority"), "name"))]
				if now == "" {
					now = "medium"
				}
				if now != base {
					add("priority", now, base, now)
				}
			case "labels":
				base := is.Labels
				if ch.Base != nil && ch.Base.Labels != nil {
					base = *ch.Base.Labels
				}
				if now := jiraLabels(f); !sameStrings(now, base) {
					add("labels", strings.Join(sortedCopy(now), ","), strings.Join(sortedCopy(base), ","), now)
				}
			case "assignee":
				baseID := ""
				if is.Assignee != nil {
					baseID = is.Assignee.ID
				}
				if ch.Base != nil && ch.Base.Assignee != nil {
					baseID = *ch.Base.Assignee
				}
				now := jstr(jmap(f, "assignee"), "accountId")
				if base := s.jiraAccountOf(ctx, baseID); now != base {
					cur := ""
					if now != "" {
						if cur = s.userOfJiraAccount(ctx, now); cur == "" {
							cur = "jira:" + now
						}
					}
					add("assignee", jstr(jmap(f, "assignee"), "displayName"), baseID, cur)
				}
			}
		}
		if len(conflicts) > 0 {
			return is, &JiraConflict{Issue: is.Key, Fields: conflicts}
		}
		payload := map[string]any{}
		if ch.Priority != nil {
			payload["priority"] = map[string]any{"name": jiraPriorityName(*ch.Priority)}
		}
		if ch.Labels != nil {
			payload["labels"] = labels
		}
		if ch.Assignee != nil {
			if assigneeAcct == "" {
				payload["assignee"] = nil
			} else {
				payload["assignee"] = map[string]any{"accountId": assigneeAcct}
			}
		}
		if err := cl.EditIssue(ctx, key, payload); err != nil {
			return is, s.wtErr(a, t, is.Key, err)
		}
		s.wtReadback(ctx, t, cl, key)
		return s.getIssue(ctx, is.Key)
	}()
	s.wtDone(ctx, a, is.Key, fields, err)
	if err != nil {
		return is, err
	}
	s.touchLease(ctx, a, is.ID)
	s.emit(Event{Type: "issue.updated", Issue: out.Key, Fields: fields, Version: out.Version, Actor: a.Label, ActorID: a.ID})
	return out, nil
}

// adfIsPlain says whether a Jira document is only paragraphs of unformatted text and line breaks: the one shape Tasklane can write back
// without losing anything while the editor works on plain text (WT-12). Anything else is edited in Jira.
func adfIsPlain(adf any) bool {
	if adf == nil {
		return true
	}
	doc, _ := adf.(map[string]any)
	if doc == nil || doc["type"] != "doc" {
		return false
	}
	for _, b := range kids(doc) {
		bm, _ := b.(map[string]any)
		if bm == nil || bm["type"] != "paragraph" {
			return false
		}
		for _, in := range kids(bm) {
			im, _ := in.(map[string]any)
			switch {
			case im == nil:
				return false
			case im["type"] == "hardBreak":
			case im["type"] == "text":
				if m, _ := im["marks"].([]any); len(m) > 0 {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

func kids(n map[string]any) []any { k, _ := n["content"].([]any); return k }

// plainADF writes text as paragraphs (blank lines between them) with line breaks inside. The caller checks the round trip.
func plainADF(text string) map[string]any {
	var blocks []any
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		if strings.TrimSpace(para) == "" {
			continue
		}
		var inl []any
		for i, line := range strings.Split(para, "\n") {
			if i > 0 {
				inl = append(inl, map[string]any{"type": "hardBreak"})
			}
			if line != "" {
				inl = append(inl, map[string]any{"type": "text", "text": line})
			}
		}
		blocks = append(blocks, map[string]any{"type": "paragraph", "content": inl})
	}
	if blocks == nil {
		blocks = []any{}
	}
	return map[string]any{"type": "doc", "version": 1, "content": blocks}
}

// plainBody makes a document from text and proves that Jira's text view of it is the text again; otherwise the write is refused.
func plainBody(text string) (map[string]any, bool) {
	doc := plainADF(text)
	back, _ := jira.ADFText(doc)
	return doc, back == strings.TrimSpace(text)
}

func (t *wtTarget) lossy(issue, what string) error {
	return &JiraRejected{Issue: issue, Messages: []string{what + " cannot be written back from Tasklane without losing its formatting: edit it in Jira (" + t.site + ")"}}
}

// wtSaveText writes the summary or the description through (WT-08, WT-12). fn is the same function the native save uses: it applies the
// save mode's own conflict rule against the mirror's text and its sequence number, and returns the new text.
func (s *Service) wtSaveText(ctx context.Context, a Actor, is Issue, t *wtTarget, field, mode string, fn textFn) (TextResult, error) {
	if field != "summary" && field != "description" {
		return TextResult{}, t.notWritable()
	}
	var cur string
	var seq int64
	if field == "summary" {
		s.DB.QueryRow(ctx, `SELECT summary,summary_seq FROM issue WHERE id=$1`, is.ID).Scan(&cur, &seq)
	} else {
		s.DB.QueryRow(ctx, `SELECT body,text_seq FROM issue_text WHERE issue_id=$1 AND field=$2`, is.ID, field).Scan(&cur, &seq)
	}
	snapshot := func(at int64) string {
		var b string
		s.DB.QueryRow(ctx, `SELECT body FROM text_version WHERE issue_id=$1 AND field=$2 AND seq=$3`, is.ID, field, at).Scan(&b)
		return b
	}
	last := func() (string, time.Time) {
		var by string
		var at time.Time
		s.DB.QueryRow(ctx, `SELECT actor_label,at FROM text_version WHERE issue_id=$1 AND field=$2 ORDER BY seq DESC LIMIT 1`, is.ID, field).Scan(&by, &at)
		return by, at
	}
	nb, err := fn(cur, seq, snapshot, last)
	if err != nil {
		return TextResult{}, err
	}
	clean, removed := Sanitize(nb)
	if field == "summary" {
		clean = strings.TrimSpace(clean)
	}
	if err := validateText(field, clean); err != nil {
		return TextResult{}, err
	}
	if clean == cur {
		return TextResult{Seq: seq, Unchanged: true, Removed: removed}, nil
	}
	cl, err := s.wtClient(a, t)
	if err != nil {
		return TextResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, JiraWriteTimeout)
	defer cancel()
	key := s.originKey(ctx, is.ID, is.Key)
	err = func() error {
		curJ, err := cl.IssueFields(ctx, key, []string{field, "updated"}, "changelog")
		if err != nil {
			return s.wtErr(a, t, is.Key, err)
		}
		f := curJ.Fields()
		var now string
		if field == "summary" {
			now = jstr(f, "summary")
		} else {
			now, _ = jira.ADFText(f["description"])
			if !adfIsPlain(f["description"]) {
				return t.lossy(is.Key, "this description")
			}
		}
		// The text is compared with Jira's current text and a difference opens the editor's own conflict dialog (a TextConflict that
		// carries Jira's text). "keep mine" and "merge" are what the person chose after that dialog showed them Jira's text, so
		// those saves go ahead; the window between the dialog and the send is the accepted race.
		if mode != "keep_mine" && mode != "merge" && strings.TrimSpace(now) != strings.TrimSpace(cur) {
			by, at := lastJiraChange(curJ, field)
			mine, _ := Sanitize(nb)
			var when time.Time
			if at != nil {
				when = *at
			}
			return &TextConflict{Field: field, Current: now, CurrentSeq: seq, Base: cur, BaseSeq: seq, By: by, At: when, Mine: mine, Merge: text.Merge(cur, now, mine)}
		}
		var value any = clean
		if field == "description" {
			doc, ok := plainBody(clean)
			if !ok {
				return t.lossy(is.Key, "this text")
			}
			value = doc
		}
		if err := cl.EditIssue(ctx, key, map[string]any{field: value}); err != nil {
			return s.wtErr(a, t, is.Key, err)
		}
		s.wtReadback(ctx, t, cl, key)
		return nil
	}()
	s.wtDone(ctx, a, is.Key, []string{field}, err)
	if err != nil {
		return TextResult{}, err
	}
	res := TextResult{Removed: removed}
	if field == "summary" {
		s.DB.QueryRow(ctx, `SELECT summary_seq FROM issue WHERE id=$1`, is.ID).Scan(&res.Seq)
	} else {
		s.DB.QueryRow(ctx, `SELECT text_seq FROM issue_text WHERE issue_id=$1 AND field=$2`, is.ID, field).Scan(&res.Seq)
	}
	if a.ID != "" {
		s.DB.Exec(ctx, `DELETE FROM draft WHERE user_id=$1 AND issue_id=$2 AND field=$3`, a.ID, is.ID, field)
	}
	s.touchLease(ctx, a, is.ID)
	s.emit(Event{Type: "issue.text_saved", Issue: is.Key, Field: field, TextSeq: res.Seq, Version: is.Version, Actor: a.Label, ActorID: a.ID})
	return res, nil
}

// wtAddComment adds a comment in Jira. A comment is added, never merged, so it cannot collide with another (SPEC 4.5); an internal
// comment would need Jira's visibility restrictions and is not written in this phase (WT-10).
func (s *Service) wtAddComment(ctx context.Context, a Actor, is Issue, t *wtTarget, body string, internal bool) (Comment, error) {
	if internal {
		return Comment{}, t.notWritable()
	}
	body, _ = Sanitize(body)
	if strings.TrimSpace(body) == "" {
		return Comment{}, invalid("comment is empty")
	}
	if err := checkSize(body); err != nil {
		return Comment{}, err
	}
	doc, ok := plainBody(body)
	if !ok {
		return Comment{}, t.lossy(is.Key, "this comment")
	}
	cl, err := s.wtClient(a, t)
	if err != nil {
		return Comment{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, JiraWriteTimeout)
	defer cancel()
	key := s.originKey(ctx, is.ID, is.Key)
	jc, err := cl.AddComment(ctx, key, doc)
	err = s.wtErr(a, t, is.Key, err)
	s.wtDone(ctx, a, is.Key, []string{"comment"}, err)
	if err != nil {
		return Comment{}, err
	}
	s.wtReadback(ctx, t, cl, key)
	c := Comment{Author: User{ID: a.ID, Name: a.Label}, Body: body}
	s.DB.QueryRow(ctx, `SELECT id,created_at FROM comment WHERE issue_id=$1 AND external_id=$2`, is.ID, jstr(jc, "id")).Scan(&c.ID, &c.Created)
	if c.Created.IsZero() {
		c.Created = s.Now()
	}
	s.touchLease(ctx, a, is.ID)
	s.emit(Event{Type: "comment.added", Issue: is.Key, Actor: a.Label, ActorID: a.ID})
	return c, nil
}

// wtTransition moves the issue in Jira. The person chose a target status in Tasklane's workflow; Jira is asked which of its own
// transitions leads there from where the issue is now. Not finding one is Jira's collision signal and is passed on (WT-09).
func (s *Service) wtTransition(ctx context.Context, a Actor, is Issue, t *wtTarget, transitionID string) (Issue, error) {
	var toName string
	err := s.DB.QueryRow(ctx, `SELECT st.name FROM transition t JOIN status st ON st.id=t.to_status
		WHERE t.id=$1 AND (t.from_status IS NULL OR t.from_status=(SELECT status_id FROM issue WHERE id=$2)) AND t.workflow_id=(SELECT p.workflow_id FROM issue i JOIN project p ON p.id=i.project_id WHERE i.id=$2)`,
		transitionID, is.ID).Scan(&toName)
	if err != nil {
		return is, invalid("transition is not available from status %q", is.Status)
	}
	cl, err := s.wtClient(a, t)
	if err != nil {
		return is, err
	}
	ctx, cancel := context.WithTimeout(ctx, JiraWriteTimeout)
	defer cancel()
	key := s.originKey(ctx, is.ID, is.Key)
	err = func() error {
		curJ, err := cl.IssueFields(ctx, key, []string{"status", "updated"}, "changelog")
		if err != nil {
			return s.wtErr(a, t, is.Key, err)
		}
		if now := jstr(jmap(curJ.Fields(), "status"), "name"); !strings.EqualFold(now, is.Status) {
			by, at := lastJiraChange(curJ, "status")
			return &JiraConflict{Issue: is.Key, Fields: []JiraFieldConflict{{Field: "status", Jira: now, Base: is.Status, By: by, At: at}}}
		}
		ts, err := cl.Transitions(ctx, key)
		if err != nil {
			return s.wtErr(a, t, is.Key, err)
		}
		jid := ""
		for _, tr := range ts {
			if strings.EqualFold(tr.To.Name, toName) {
				jid = tr.ID
				break
			}
		}
		if jid == "" {
			return &JiraRejected{Issue: is.Key, Status: 400, Messages: []string{fmt.Sprintf("Jira offers no move to %q from %q", toName, is.Status)}}
		}
		if err := cl.DoTransition(ctx, key, jid, nil); err != nil {
			return s.wtErr(a, t, is.Key, err)
		}
		s.wtReadback(ctx, t, cl, key)
		return nil
	}()
	s.wtDone(ctx, a, is.Key, []string{"status"}, err)
	if err != nil {
		return is, err
	}
	s.touchLease(ctx, a, is.ID)
	out, gerr := s.getIssue(ctx, is.Key)
	if gerr == nil {
		s.emit(Event{Type: "issue.transitioned", Issue: out.Key, Fields: []string{"status"}, Version: out.Version, Actor: a.Label, ActorID: a.ID})
	}
	return out, gerr
}

// SetJiraSessionToken keeps a person's Jira API token in memory for their writes (WT-04). Jira checks it; it must belong to the Jira
// account linked to this person, so nobody can save under someone else's name. Email is the Atlassian account's, defaulting to the
// person's Tasklane email.
func (s *Service) SetJiraSessionToken(ctx context.Context, a Actor, connID, email, token string) error {
	if a.ID == "" || a.Kind == "agent" {
		return ErrForbidden
	}
	c, err := s.jiraConn(ctx, connID)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return invalid("the API token is required")
	}
	var myEmail string
	var linked *string
	if err := s.DB.QueryRow(ctx, `SELECT email,jira_account_id FROM app_user WHERE id::text=$1`, a.ID).Scan(&myEmail, &linked); err != nil {
		return ErrNotFound
	}
	if email = strings.TrimSpace(email); email == "" {
		email = myEmail
	}
	me, err := s.clientFor(c, credvault.Cred{Email: email, Token: token}).Myself(ctx)
	if err != nil {
		var st *jira.ErrStatus
		if errors.As(err, &st) && (st.Status == 401 || st.Status == 403) {
			return invalid("Jira rejected the token: check the token and the email of your Atlassian account")
		}
		return invalid("cannot reach Jira at %s: %v", c.site, err)
	}
	switch {
	case linked != nil && *linked != "":
		if me.AccountID != *linked {
			return invalid("this token belongs to a different Jira account than yours")
		}
	case strings.EqualFold(me.Email, myEmail):
		s.DB.Exec(ctx, `UPDATE app_user SET jira_account_id=$2 WHERE id::text=$1 AND jira_account_id IS NULL`, a.ID, me.AccountID)
	default:
		return invalid("this token belongs to a Jira account whose email is not yours")
	}
	s.vault().PutUser(a.ID, connID, email, me.AccountID, token)
	return nil
}

// ClearJiraSessionToken forgets the person's token for a connection (connID empty: all of them): logout and "forget my token".
func (s *Service) ClearJiraSessionToken(a Actor, connID string) {
	if connID == "" {
		s.vault().DropUser(a.ID)
		return
	}
	s.vault().DropUserConn(a.ID, connID)
}

// DropJiraTokensOf forgets every Jira token held for a person: called when their session ends (WT-02).
func (s *Service) DropJiraTokensOf(userID string) { s.vault().DropUser(userID) }
