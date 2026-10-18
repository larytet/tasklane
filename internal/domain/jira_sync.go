package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/jira"
)

const jiraOverlap = 5 * time.Minute // polling goes back this far from the watermark, so late or minute-rounded changes are never missed

var jiraFields = []string{"summary", "description", "status", "priority", "issuetype", "labels", "assignee", "reporter", "created", "updated", "resolutiondate", "parent", "issuelinks", "security", "comment", "project"}

func jiraTime(v any) time.Time {
	s, _ := v.(string)
	for _, l := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05.000Z0700", time.RFC3339, "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func jstr(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

func jmap(m map[string]any, k string) map[string]any { v, _ := m[k].(map[string]any); return v }

var jiraPriority = map[string]string{"highest": "highest", "blocker": "highest", "critical": "highest", "high": "high", "major": "high", "medium": "medium", "normal": "medium", "low": "low", "minor": "low", "lowest": "lowest", "trivial": "lowest"}

// SyncJiraProject runs one catch-up of a mirrored project (administrators; the worker calls the same code).
func (s *Service) SyncJiraProject(ctx context.Context, a Actor, projectKey string) (int, error) {
	if err := adminOnly(a); err != nil {
		return 0, err
	}
	var pid string
	if err := s.DB.QueryRow(ctx, `SELECT project_id::text FROM jira_project jp JOIN project p ON p.id=jp.project_id WHERE p.key=$1`, strings.ToUpper(projectKey)).Scan(&pid); err != nil {
		return 0, ErrNotFound
	}
	return s.syncJira(ctx, pid, false)
}

var ErrSyncBusy = errors.New("another node is already syncing this project")

// syncJira reads what changed in Jira since the watermark and applies it, one transaction per issue. With full, the
// watermark is ignored (a backfill from scratch). One node syncs a project at a time (an advisory lock).
func (s *Service) syncJira(ctx context.Context, pid string, full bool) (int, error) {
	conn, err := s.DB.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('jira:'||$1))`, pid).Scan(&got); err != nil || !got {
		return 0, ErrSyncBusy
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('jira:'||$1))`, pid)

	var connID, jiraKey, jql, pkey string
	var watermark, permAt *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT jp.connection_id::text,jp.jira_key,jp.jql,jp.watermark,jp.perm_synced_at,p.key FROM jira_project jp JOIN project p ON p.id=jp.project_id WHERE jp.project_id::text=$1`, pid).
		Scan(&connID, &jiraKey, &jql, &watermark, &permAt, &pkey); err != nil {
		return 0, ErrNotFound
	}
	fail := func(err error) (int, error) {
		s.DB.Exec(ctx, `UPDATE jira_project SET last_error=$2 WHERE project_id::text=$1`, pid, trimErr(err))
		return 0, err
	}
	// a pause drains the sync like any other writer; the watermark is already stored, so restore resumes from it
	end, perr := s.admitGate(ctx, pkey)
	if perr != nil {
		return 0, perr
	}
	defer end()
	cl, actor, err := s.jiraClient(ctx, connID)
	if err != nil {
		return fail(err)
	}
	if _, err := cl.Myself(ctx); err != nil {
		return fail(err)
	}
	q := fmt.Sprintf(`project = "%s"`, jiraKey)
	if jql != "" {
		q += " AND (" + jql + ")"
	}
	if watermark != nil && !full {
		q += fmt.Sprintf(` AND updated >= "%s"`, cl.JQLTime(watermark.Add(-jiraOverlap)))
	}
	q += " ORDER BY updated ASC"
	started := s.Now()
	n := 0
	token := ""
	maxUpdated := time.Time{}
	if watermark != nil && !full {
		maxUpdated = *watermark
	}
	for {
		pg, err := cl.Search(ctx, q, jiraFields, "changelog", token, 50)
		if err != nil {
			s.advanceWatermark(ctx, pid, maxUpdated, n)
			return fail(err)
		}
		for _, ji := range pg.Issues {
			updated, err := s.applyJiraIssue(ctx, cl, pid, pkey, actor, ji)
			if err != nil {
				s.advanceWatermark(ctx, pid, maxUpdated, n)
				return fail(fmt.Errorf("%s: %w", ji.Key(), err))
			}
			if updated.After(maxUpdated) {
				maxUpdated = updated
			}
			n++
		}
		s.resolveJiraParents(ctx, pid)
		s.advanceWatermark(ctx, pid, maxUpdated, 0)
		if pg.Next == "" {
			break
		}
		token = pg.Next
	}
	// roles and groups are read after the issues, so people the issues just introduced (by account id and email) can be mapped
	var permErr error
	if permAt == nil || s.Now().Sub(*permAt) > permRefresh {
		if permErr = s.syncJiraPermissions(ctx, cl, pid, jiraKey); permErr != nil {
			permErr = fmt.Errorf("permissions: %w", permErr) // the project fails closed once the last good read is stale
		}
	}
	msg := ""
	if permErr != nil {
		msg = trimErr(permErr)
	}
	s.DB.Exec(ctx, `UPDATE jira_project SET synced_at=$2,last_error=$4,backfilled=true,wake=false,issues_synced=issues_synced+$3 WHERE project_id::text=$1`, pid, started, n, msg)
	s.refreshJiraWarnings(ctx, pid)
	return n, nil
}

func (s *Service) advanceWatermark(ctx context.Context, pid string, t time.Time, n int) {
	if t.IsZero() {
		return
	}
	s.DB.Exec(ctx, `UPDATE jira_project SET watermark=GREATEST(COALESCE(watermark,'epoch'),$2) WHERE project_id::text=$1`, pid, t)
}

// resolveJiraLinks turns stored Jira links into issue links between issues that are both mirrored.
func (s *Service) resolveJiraLinks(ctx context.Context, pid string) {
	s.DB.Exec(ctx, `INSERT INTO issue_link(from_id,to_id,type)
		SELECT CASE WHEN l->>'Dir'='out' THEN i.id ELSE o.id END, CASE WHEN l->>'Dir'='out' THEN o.id ELSE i.id END, l->>'Type'
		FROM issue i CROSS JOIN LATERAL jsonb_array_elements(i.origin_links) l JOIN issue o ON o.project_id=i.project_id AND o.external_id=l->>'Ext' AND o.deleted_at IS NULL
		WHERE i.project_id::text=$1 AND i.origin_links IS NOT NULL AND i.deleted_at IS NULL ON CONFLICT DO NOTHING`, pid)
}

func (s *Service) resolveJiraParents(ctx context.Context, pid string) {
	s.resolveJiraLinks(ctx, pid)
	s.DB.Exec(ctx, `UPDATE issue c SET parent_id=p.id FROM issue p WHERE c.project_id::text=$1 AND c.origin_parent IS NOT NULL AND p.project_id=c.project_id AND p.external_id=c.origin_parent AND c.parent_id IS DISTINCT FROM p.id`, pid)
	s.DB.Exec(ctx, `UPDATE issue SET parent_id=NULL WHERE project_id::text=$1 AND origin_parent IS NULL AND parent_id IS NOT NULL AND external_id IS NOT NULL`, pid)
}

// jiraUser maps a Jira account to a Tasklane user: by account id, then by email, otherwise an inactive placeholder with no
// credentials. An account that later signs in with the same email takes over the placeholder's history.
func (s *Service) jiraUser(ctx context.Context, tx pgx.Tx, acct map[string]any) (string, error) {
	if acct == nil {
		return "", nil
	}
	aid, name, email := jstr(acct, "accountId"), jstr(acct, "displayName"), strings.ToLower(jstr(acct, "emailAddress"))
	if aid == "" {
		return "", nil
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE jira_account_id=$1`, aid).Scan(&id); err == nil {
		return id, nil
	}
	if email != "" {
		if err := tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE lower(email)=$1`, email).Scan(&id); err == nil {
			tx.Exec(ctx, `UPDATE app_user SET jira_account_id=$2 WHERE id::text=$1 AND jira_account_id IS NULL`, id, aid)
			return id, nil
		}
	} else {
		email = aid + "@jira.invalid"
	}
	if name == "" {
		name = aid
	}
	err := tx.QueryRow(ctx, `INSERT INTO app_user(email,name,kind,active,jira_account_id) VALUES($1,$2,'human',false,$3) ON CONFLICT (email) DO UPDATE SET jira_account_id=COALESCE(app_user.jira_account_id,EXCLUDED.jira_account_id) RETURNING id::text`, email, name, aid).Scan(&id)
	return id, err
}

func (s *Service) ensureJiraStatus(ctx context.Context, tx pgx.Tx, pid, name, category string) (string, error) {
	cat := map[string]string{"new": "todo", "indeterminate": "in_progress", "done": "done"}[category]
	if cat == "" {
		cat = "todo"
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO status(workflow_id,name,category,position) SELECT p.workflow_id,$2,$3,COALESCE((SELECT max(position)+1 FROM status WHERE workflow_id=p.workflow_id),0) FROM project p WHERE p.id::text=$1
		ON CONFLICT (workflow_id,name) DO UPDATE SET category=EXCLUDED.category RETURNING id::text`, pid, name, cat).Scan(&id)
	return id, err
}

func (s *Service) ensureJiraType(ctx context.Context, tx pgx.Tx, name string, subtask bool) (string, error) {
	level := 0
	if strings.EqualFold(name, "epic") {
		level = 1
	}
	if subtask {
		level = -1
	}
	if name == "" {
		name = "Task"
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO issue_type(name,level) VALUES($1,$2) ON CONFLICT (name) DO UPDATE SET name=EXCLUDED.name RETURNING id::text`, name, level).Scan(&id)
	return id, err
}

// applyJiraIssue stores one issue with its comments, history and links in a single transaction. Network reads happen
// first; the write is idempotent per Jira issue id and `updated`: an older or equal version changes nothing.
func (s *Service) applyJiraIssue(ctx context.Context, cl *jira.Client, pid, pkey, actor string, ji jira.Issue) (time.Time, error) {
	f := ji.Fields()
	updated := jiraTime(f["updated"])
	var curUpdated *time.Time
	var curID, curKey string
	err := s.DB.QueryRow(ctx, `SELECT id::text,key,origin_updated FROM issue WHERE project_id::text=$1 AND external_id=$2`, pid, ji.ID()).Scan(&curID, &curKey, &curUpdated)
	if err == nil && curUpdated != nil && !updated.After(*curUpdated) {
		return updated, nil // already have this version or a newer one
	}
	// comments and the changelog are capped inline: read the rest from their own endpoints
	comments := []jira.Comment{}
	cm := jmap(f, "comment")
	if arr, ok := cm["comments"].([]any); ok {
		for _, c := range arr {
			if m, ok := c.(map[string]any); ok {
				comments = append(comments, m)
			}
		}
	}
	if total, _ := cm["total"].(float64); int(total) > len(comments) {
		all, err := cl.Comments(ctx, ji.ID())
		if err != nil {
			return time.Time{}, err
		}
		comments = all
	}
	var hist []jira.History
	cl2 := jmap(ji, "changelog")
	if arr, ok := cl2["histories"].([]any); ok {
		for _, h := range arr {
			if m, ok := h.(map[string]any); ok {
				hist = append(hist, m)
			}
		}
	}
	if total, _ := cl2["total"].(float64); int(total) > len(hist) {
		all, err := cl.Changelog(ctx, ji.ID())
		if err != nil {
			return time.Time{}, err
		}
		hist = all
	}
	number := int64(0)
	if i := strings.LastIndex(ji.Key(), "-"); i >= 0 {
		number, _ = strconv.ParseInt(ji.Key()[i+1:], 10, 64)
	}
	if number <= 0 {
		return time.Time{}, fmt.Errorf("the Jira key %q has no number", ji.Key())
	}
	newKey := pkey + "-" + strconv.FormatInt(number, 10)
	var key string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		st := jmap(f, "status")
		statusID, err := s.ensureJiraStatus(ctx, tx, pid, jstr(st, "name"), jstr(st, "statusCategory", "key"))
		if err != nil {
			return err
		}
		it := jmap(f, "issuetype")
		typeID, err := s.ensureJiraType(ctx, tx, jstr(it, "name"), it["subtask"] == true)
		if err != nil {
			return err
		}
		prio := jiraPriority[strings.ToLower(jstr(jmap(f, "priority"), "name"))]
		if prio == "" {
			prio = "medium"
		}
		reporter, err := s.jiraUser(ctx, tx, jmap(f, "reporter"))
		if err != nil {
			return err
		}
		if reporter == "" {
			reporter = actor
		}
		var assignee any
		if a, err := s.jiraUser(ctx, tx, jmap(f, "assignee")); err != nil {
			return err
		} else if a != "" {
			assignee = a
		}
		var labels []string
		if arr, ok := f["labels"].([]any); ok {
			for _, l := range arr {
				if ls, ok := l.(string); ok && ls != "" {
					labels = append(labels, ls)
				}
			}
		}
		if labels == nil {
			labels = []string{}
		}
		var desc string
		var adf any = f["description"]
		if adf != nil {
			desc, _ = jira.ADFText(adf)
		}
		var parent any
		if p := jmap(f, "parent"); p != nil {
			parent = jstr(p, "id")
		}
		summary := jstr(f, "summary")
		if strings.TrimSpace(summary) == "" {
			summary = "(no summary)"
		}
		created, resolved := jiraTime(f["created"]), jiraTime(f["resolutiondate"])
		var resolvedAt any
		if !resolved.IsZero() {
			resolvedAt = resolved
		}
		restricted := f["security"] != nil
		var id string
		if curID == "" {
			if err := tx.QueryRow(ctx, `INSERT INTO issue(project_id,number,key,issue_type_id,status_id,priority,reporter_id,assignee_id,labels,rank,summary,created_at,updated_at,resolved_at,external_id,origin_updated,restricted,origin_parent,origin_key)
				VALUES($1::uuid,$2,$3,$4::uuid,$5::uuid,$6,$7::uuid,$8::uuid,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) RETURNING id::text`,
				pid, number, newKey, typeID, statusID, prio, reporter, assignee, labels, rankFor(number), summary, created, updated, resolvedAt, ji.ID(), updated, restricted, parent, ji.Key()).Scan(&id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE project SET next_number=GREATEST(next_number,$2+1) WHERE id::text=$1`, pid, number); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO issue_text(issue_id,field,body,updated_by) VALUES($1::uuid,'description',$2,$3::uuid)`, id, desc, reporter); err != nil {
				return err
			}
			for fld, body := range map[string]string{"summary": summary, "description": desc} {
				tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode,at) VALUES($1::uuid,$2,1,$3,$4::uuid,'Jira','import',$5)`, id, fld, body, reporter, updated)
			}
		} else {
			id = curID
			if curKey != newKey { // a key change in Jira (a move): the old key keeps resolving
				if _, err := tx.Exec(ctx, `INSERT INTO issue_key_alias(old_key,issue_id) VALUES($1,$2::uuid) ON CONFLICT DO NOTHING`, curKey, id); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE issue SET key=$2,number=$3,issue_type_id=$4::uuid,status_id=$5::uuid,priority=$6,reporter_id=$7::uuid,assignee_id=$8::uuid,labels=$9,summary=$10,updated_at=$11,resolved_at=$12,origin_updated=$11,restricted=$13,origin_parent=$14,origin_key=$15,version=version+1 WHERE id::text=$1`,
				id, newKey, number, typeID, statusID, prio, reporter, assignee, labels, summary, updated, resolvedAt, restricted, parent, ji.Key()); err != nil {
				return err
			}
			var old string
			tx.QueryRow(ctx, `SELECT body FROM issue_text WHERE issue_id::text=$1 AND field='description'`, id).Scan(&old)
			if old != desc {
				var seq int64
				tx.QueryRow(ctx, `UPDATE issue_text SET body=$2,text_seq=text_seq+1,updated_at=$3 WHERE issue_id::text=$1 AND field='description' RETURNING text_seq`, id, desc, updated).Scan(&seq)
				tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode,at) VALUES($1::uuid,'description',$2,$3,NULL,'Jira','import',$4)`, id, seq, desc, updated)
			}
		}
		key = newKey
		if adf != nil {
			raw, _ := json.Marshal(adf)
			tx.Exec(ctx, `INSERT INTO jira_adf(issue_id,field,adf) VALUES($1::uuid,'description',$2) ON CONFLICT (issue_id,field) DO UPDATE SET adf=EXCLUDED.adf`, id, raw)
		} else {
			tx.Exec(ctx, `DELETE FROM jira_adf WHERE issue_id::text=$1 AND field='description'`, id)
		}
		// comments: upsert by Jira id, and drop the ones deleted in Jira. A restricted comment becomes an internal one.
		keep := []string{}
		for _, c := range comments {
			cid := jstr(c, "id")
			author, err := s.jiraUser(ctx, tx, jmap(c, "author"))
			if err != nil {
				return err
			}
			if author == "" {
				author = reporter
			}
			body, _ := jira.ADFText(c["body"])
			if body == "" {
				body = "(empty comment)"
			}
			internal := c["visibility"] != nil
			cc, cu := jiraTime(c["created"]), jiraTime(c["updated"])
			var edited any
			if cu.After(cc) {
				edited = cu
			}
			if _, err := tx.Exec(ctx, `INSERT INTO comment(issue_id,author_id,body,internal,created_at,edited_at,external_id) VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7)
				ON CONFLICT (issue_id,external_id) WHERE external_id IS NOT NULL DO UPDATE SET body=EXCLUDED.body,internal=EXCLUDED.internal,edited_at=EXCLUDED.edited_at`, id, author, body, internal, cc, edited, cid); err != nil {
				return err
			}
			if raw, err := json.Marshal(c["body"]); err == nil {
				tx.Exec(ctx, `INSERT INTO jira_adf(issue_id,field,adf) VALUES($1::uuid,$2,$3) ON CONFLICT (issue_id,field) DO UPDATE SET adf=EXCLUDED.adf`, id, "comment:"+cid, raw)
			}
			keep = append(keep, cid)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM comment WHERE issue_id::text=$1 AND external_id IS NOT NULL AND NOT (external_id = ANY($2))`, id, keep); err != nil {
			return err
		}
		// history: idempotent by changelog id and item position; oldest first so the ids stay in time order
		for i := len(hist) - 1; i >= 0; i-- {
			h := hist[i]
			hid := jstr(h, "id")
			au, err := s.jiraUser(ctx, tx, jmap(h, "author"))
			if err != nil {
				return err
			}
			label := jstr(jmap(h, "author"), "displayName")
			at := jiraTime(h["created"])
			items, _ := h["items"].([]any)
			for n, itm := range items {
				im, _ := itm.(map[string]any)
				field := strings.ToLower(jstr(im, "field"))
				var before, after *string
				if v := jstr(im, "fromString"); v != "" {
					before = &v
				}
				if v := jstr(im, "toString"); v != "" {
					after = &v
				}
				if _, err := tx.Exec(ctx, `INSERT INTO issue_history(issue_id,field,before,after,actor_id,actor_label,at,external_id,source) VALUES($1::uuid,$2,$3,$4,$5::uuid,$6,$7,$8,'Jira') ON CONFLICT (issue_id,external_id) WHERE external_id IS NOT NULL DO NOTHING`,
					id, field, before, after, nilIfEmpty(au), label, at, hid+":"+strconv.Itoa(n)); err != nil {
					return err
				}
			}
		}
		// links: kept as received and turned into rows after the batch, once the other issue is certainly there too
		var linkRaw any
		if links, ok := f["issuelinks"].([]any); ok && len(links) > 0 {
			type L struct {
				Type, Dir, Ext string
			}
			var ls []L
			for _, l := range links {
				lm, _ := l.(map[string]any)
				typ := map[string]string{"blocks": "blocks", "duplicate": "duplicates", "cloners": "clones"}[strings.ToLower(jstr(lm, "type", "name"))]
				if typ == "" {
					typ = "relates"
				}
				if o := jmap(lm, "outwardIssue"); o != nil {
					ls = append(ls, L{typ, "out", jstr(o, "id")})
				} else if in := jmap(lm, "inwardIssue"); in != nil {
					ls = append(ls, L{typ, "in", jstr(in, "id")})
				}
			}
			linkRaw, _ = json.Marshal(ls)
		}
		if _, err := tx.Exec(ctx, `UPDATE issue SET origin_links=$2 WHERE id::text=$1`, id, linkRaw); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+key, "issue.synced", map[string]any{"key": key, "actor": "Jira"})
	})
	if err != nil {
		return time.Time{}, err
	}
	s.emit(Event{Type: "issue.updated", Issue: key, Actor: "Jira"})
	return updated, nil
}

// syncJiraPermissions reads the project roles and the groups in them and sets who may read the mirror. Users are matched
// by Jira account id or verified email. Actors it cannot evaluate are listed as a warning; they grant nothing (fail closed).
func (s *Service) syncJiraPermissions(ctx context.Context, cl *jira.Client, pid, jiraKey string) error {
	roles, err := cl.ProjectRoles(ctx, jiraKey)
	if err != nil {
		return err
	}
	type member struct{ acct, email string }
	members := map[string]member{}
	var unmapped []string
	for _, url := range roles {
		actors, err := cl.RoleActors(ctx, url)
		if err != nil {
			return err
		}
		for _, a := range actors {
			switch {
			case a.AccountID != "":
				members[a.AccountID] = member{acct: a.AccountID}
			case a.Group != "":
				us, err := cl.GroupMembers(ctx, a.Group)
				if err != nil {
					return err
				}
				for _, u := range us {
					if u.Active {
						members[u.AccountID] = member{acct: u.AccountID, email: strings.ToLower(u.Email)}
					}
				}
			default:
				unmapped = append(unmapped, fmt.Sprintf("%s (%s)", a.Name, a.Type))
			}
		}
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		ids := map[string]bool{}
		for acct, m := range members {
			var uid string
			err := tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE jira_account_id=$1 AND active`, acct).Scan(&uid)
			if err == pgx.ErrNoRows && m.email != "" {
				err = tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE lower(email)=$1 AND active`, m.email).Scan(&uid)
				if err == nil {
					tx.Exec(ctx, `UPDATE app_user SET jira_account_id=$2 WHERE id::text=$1 AND jira_account_id IS NULL`, uid, acct)
				}
			}
			if err == pgx.ErrNoRows {
				// the account's email may be hidden by Jira's privacy settings: look it up once
				if u, uerr := cl.User(ctx, acct); uerr == nil && u.Email != "" {
					if tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE lower(email)=$1 AND active`, strings.ToLower(u.Email)).Scan(&uid) == nil {
						tx.Exec(ctx, `UPDATE app_user SET jira_account_id=$2 WHERE id::text=$1 AND jira_account_id IS NULL`, uid, acct)
						err = nil
					}
				}
			}
			if err != nil {
				continue // nobody here with that account yet: they get access when they exist and match
			}
			ids[uid] = true
			if _, err := tx.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1::uuid,$2::uuid,'viewer') ON CONFLICT (project_id,user_id) DO NOTHING`, pid, uid); err != nil {
				return err
			}
		}
		// anyone who is no longer in Jira's roles loses access, except people an administrator made project admin here
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM project_member WHERE project_id::text=$1 AND role='viewer' AND custom_role_id IS NULL AND NOT (user_id::text = ANY($2))`, pid, list); err != nil {
			return err
		}
		if len(unmapped) > 0 {
			tx.Exec(ctx, `INSERT INTO jira_warning(project_id,kind,detail) VALUES($1::uuid,'actors',$2) ON CONFLICT (project_id,kind) DO UPDATE SET detail=EXCLUDED.detail,at=now()`, pid,
				"These Jira role members are not users or groups Tasklane can evaluate, so they grant nothing: "+strings.Join(unmapped, ", "))
		} else {
			tx.Exec(ctx, `DELETE FROM jira_warning WHERE project_id::text=$1 AND kind='actors'`, pid)
		}
		_, err := tx.Exec(ctx, `UPDATE jira_project SET perm_synced_at=$2 WHERE project_id::text=$1`, pid, s.Now())
		return err
	})
}

func (s *Service) refreshJiraWarnings(ctx context.Context, pid string) {
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM issue WHERE project_id::text=$1 AND restricted AND deleted_at IS NULL`, pid).Scan(&n)
	if n > 0 {
		s.DB.Exec(ctx, `INSERT INTO jira_warning(project_id,kind,detail) VALUES($1::uuid,'security',$2) ON CONFLICT (project_id,kind) DO UPDATE SET detail=EXCLUDED.detail,at=now()`, pid,
			fmt.Sprintf("%d issues have a Jira security level; Tasklane cannot evaluate those exactly, so they are hidden from everyone except administrators", n))
	} else {
		s.DB.Exec(ctx, `DELETE FROM jira_warning WHERE project_id::text=$1 AND kind='security'`, pid)
	}
}

// ReconcileJira compares Jira's issue ids with the mirror and repairs drift: issues deleted in Jira (or moved out of the
// mirrored scope) are removed here; issues missing here are fetched. It refuses to delete when Jira reports nothing at all,
// which is far more likely a broken query or permission than an emptied project.
func (s *Service) ReconcileJira(ctx context.Context, a Actor, projectKey string) (JiraReconcile, error) {
	if err := adminOnly(a); err != nil {
		return JiraReconcile{}, err
	}
	var pid, connID, jiraKey, jql, pkey string
	if err := s.DB.QueryRow(ctx, `SELECT jp.project_id::text,jp.connection_id::text,jp.jira_key,jp.jql,p.key FROM jira_project jp JOIN project p ON p.id=jp.project_id WHERE p.key=$1`, strings.ToUpper(projectKey)).Scan(&pid, &connID, &jiraKey, &jql, &pkey); err != nil {
		return JiraReconcile{}, ErrNotFound
	}
	return s.reconcileJira(ctx, pid, connID, jiraKey, jql, pkey)
}

type JiraReconcile struct {
	Checked int      `json:"checked"`
	Removed []string `json:"removed"`
	Missing []string `json:"missing"`
	Skipped string   `json:"skipped,omitempty"`
}

func (s *Service) reconcileJira(ctx context.Context, pid, connID, jiraKey, jql, pkey string) (JiraReconcile, error) {
	rep := JiraReconcile{Removed: []string{}, Missing: []string{}}
	cl, _, err := s.jiraClient(ctx, connID)
	if err != nil {
		return rep, err
	}
	if _, err := cl.Myself(ctx); err != nil {
		return rep, err
	}
	q := fmt.Sprintf(`project = "%s"`, jiraKey)
	if jql != "" {
		q += " AND (" + jql + ")"
	}
	inJira := map[string]bool{}
	token := ""
	for {
		pg, err := cl.Search(ctx, q, []string{"summary"}, "", token, 100)
		if err != nil {
			return rep, err
		}
		for _, ji := range pg.Issues {
			inJira[ji.ID()] = true
		}
		if pg.Next == "" {
			break
		}
		token = pg.Next
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,key,external_id FROM issue WHERE project_id::text=$1 AND external_id IS NOT NULL AND deleted_at IS NULL`, pid)
	if err != nil {
		return rep, err
	}
	type loc struct{ id, key, ext string }
	var local []loc
	for rows.Next() {
		var l loc
		rows.Scan(&l.id, &l.key, &l.ext)
		local = append(local, l)
	}
	rows.Close()
	rep.Checked = len(local)
	have := map[string]bool{}
	var gone []loc
	for _, l := range local {
		have[l.ext] = true
		if !inJira[l.ext] {
			gone = append(gone, l)
		}
	}
	for id := range inJira {
		if !have[id] {
			rep.Missing = append(rep.Missing, id)
		}
	}
	if len(inJira) == 0 && len(local) > 0 {
		rep.Skipped = "Jira reported no issues at all; nothing was deleted (check the account's access and the JQL scope)"
		s.DB.Exec(ctx, `INSERT INTO jira_warning(project_id,kind,detail) VALUES($1::uuid,'reconcile',$2) ON CONFLICT (project_id,kind) DO UPDATE SET detail=EXCLUDED.detail,at=now()`, pid, rep.Skipped)
		return rep, nil
	}
	s.DB.Exec(ctx, `DELETE FROM jira_warning WHERE project_id::text=$1 AND kind='reconcile'`, pid)
	end, perr := s.admitGate(ctx, pkey)
	if perr != nil {
		return rep, perr
	}
	defer end()
	for _, l := range gone { // Jira has no trash: a deletion there is a removal here (erasure requests must propagate)
		err := s.tx(ctx, func(tx pgx.Tx) error {
			if err := outbox(ctx, tx, "issue:"+l.key, "issue.purged", map[string]any{"key": l.key, "actor": "Jira"}); err != nil {
				return err
			}
			_, err := s.purgeOne(ctx, tx, l.id)
			return err
		})
		if err != nil {
			return rep, err
		}
		rep.Removed = append(rep.Removed, l.key)
		s.emit(Event{Type: "issue.purged", Issue: l.key, Actor: "Jira"})
	}
	if len(rep.Missing) > 0 { // fetch what we lack: a full pass is idempotent for everything already stored
		if _, err := s.syncJira(ctx, pid, true); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// RunJiraWorkers keeps every mirror current: a project is synced when a webhook hinted at a change or when it has not been
// synced for a minute, and reconciled hourly. Several nodes may run it: an advisory lock lets one sync a project at a time.
func (s *Service) RunJiraWorkers(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastRec := time.Now()
	for {
		rows, err := s.DB.Query(ctx, `SELECT project_id::text FROM jira_project WHERE wake OR synced_at IS NULL OR synced_at < $1`, s.Now().Add(-time.Minute))
		if err == nil {
			var ids []string
			for rows.Next() {
				var id string
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				if _, err := s.syncJira(ctx, id, false); err != nil && !errors.Is(err, ErrSyncBusy) && ctx.Err() == nil {
					logf("jira sync: %v", err)
				}
			}
		}
		if time.Since(lastRec) > time.Hour {
			lastRec = time.Now()
			rows, err := s.DB.Query(ctx, `SELECT project_id::text,connection_id::text,jira_key,jql,(SELECT key FROM project WHERE id=project_id) FROM jira_project WHERE backfilled`)
			if err == nil {
				type jp struct{ pid, conn, jk, jql, pk string }
				var list []jp
				for rows.Next() {
					var x jp
					rows.Scan(&x.pid, &x.conn, &x.jk, &x.jql, &x.pk)
					list = append(list, x)
				}
				rows.Close()
				for _, x := range list {
					if _, err := s.reconcileJira(ctx, x.pid, x.conn, x.jk, x.jql, x.pk); err != nil && ctx.Err() == nil {
						logf("jira reconcile: %v", err)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
