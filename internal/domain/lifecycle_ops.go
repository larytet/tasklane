package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func typeNamed(ctx context.Context, tx pgx.Tx, name string) (id string, level int, err error) {
	err = tx.QueryRow(ctx, `SELECT id,level FROM issue_type WHERE lower(name)=lower($1)`, name).Scan(&id, &level)
	if err == pgx.ErrNoRows {
		return "", 0, invalid("unknown issue type %q", name)
	}
	return
}

func levelOfIssue(ctx context.Context, tx pgx.Tx, id string) (int, error) {
	var l int
	err := tx.QueryRow(ctx, `SELECT t.level FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.id=$1`, id).Scan(&l)
	return l, err
}

func initialStatus(ctx context.Context, tx pgx.Tx, projectID string) (id, name string, err error) {
	err = tx.QueryRow(ctx, `SELECT st.id,st.name FROM project p JOIN status st ON st.workflow_id=p.workflow_id WHERE p.id=$1 ORDER BY st.position LIMIT 1`, projectID).Scan(&id, &name)
	return
}

// mapStatus picks the status an issue gets in the target project's workflow: an explicit choice, else the same name,
// else the same category, else the initial status (SPEC 9.1).
func mapStatus(ctx context.Context, tx pgx.Tx, srcName, srcCat, targetProjectID string, override map[string]string) (id, name, how string, err error) {
	if o, ok := override[srcName]; ok {
		err = tx.QueryRow(ctx, `SELECT st.id,st.name FROM project p JOIN status st ON st.workflow_id=p.workflow_id WHERE p.id=$1 AND lower(st.name)=lower($2)`, targetProjectID, o).Scan(&id, &name)
		if err == nil {
			return id, name, "chosen", nil
		}
		if err != pgx.ErrNoRows {
			return
		}
		return "", "", "", invalid("status %q does not exist in the target workflow", o)
	}
	if err = tx.QueryRow(ctx, `SELECT st.id,st.name FROM project p JOIN status st ON st.workflow_id=p.workflow_id WHERE p.id=$1 AND lower(st.name)=lower($2)`, targetProjectID, srcName).Scan(&id, &name); err == nil {
		return id, name, "same name", nil
	}
	if err = tx.QueryRow(ctx, `SELECT st.id,st.name FROM project p JOIN status st ON st.workflow_id=p.workflow_id WHERE p.id=$1 AND st.category=$2 ORDER BY st.position LIMIT 1`, targetProjectID, srcCat).Scan(&id, &name); err == nil {
		return id, name, "same category", nil
	}
	id, name, err = initialStatus(ctx, tx, targetProjectID)
	return id, name, "initial status", err
}

func saveRemoved(ctx context.Context, s *Service, tx pgx.Tx, issueID, kind string, data map[string]any) error {
	b, _ := json.Marshal(data)
	_, err := tx.Exec(ctx, `INSERT INTO removed_values(issue_id,kind,data,expires_at) VALUES($1,$2,$3,$4)`, issueID, kind, b, s.Now().Add(s.trashRetention()))
	return err
}

// ---------------------------------------------------------------- convert

func (s *Service) opConvert(ctx context.Context, tx pgx.Tx, a Actor, key string, in LifecycleIn, p *Plan, ev *[]Event) error {
	r, role, err := s.visible(ctx, tx, a, key)
	if err != nil {
		return err
	}
	if r.DeletedAt != nil {
		return ErrGone
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	if in.ExpectKey != "" && !strings.EqualFold(in.ExpectKey, r.Key) {
		return &ConflictError{fmt.Sprintf("the issue is now %s, not %s", r.Key, in.ExpectKey)}
	}
	p.Key = r.Key
	if in.Type == "" {
		return invalid("the new type is required")
	}
	newID, newLevel, err := typeNamed(ctx, tx, in.Type)
	if err != nil {
		return err
	}
	if newID == r.TypeID {
		p.block("%s is already a %s", r.Key, r.TypeName)
		return nil
	}
	var newName string
	tx.QueryRow(ctx, `SELECT name FROM issue_type WHERE id=$1`, newID).Scan(&newName)
	p.change("change the type of %s from %s to %s", r.Key, r.TypeName, newName)
	p.change("status stays %q: both types use the project's workflow", r.StatusName)
	// parent rules: a parent must sit exactly one level above its child
	var parentKey string
	var parentID = r.ParentID
	if r.ParentID != nil {
		var pl int
		tx.QueryRow(ctx, `SELECT p.key,t.level FROM issue p JOIN issue_type t ON t.id=p.issue_type_id WHERE p.id=$1`, *r.ParentID).Scan(&parentKey, &pl)
		if pl != newLevel+1 {
			p.drop("detach %s from its parent %s (a %s cannot sit under it)", r.Key, parentKey, newName)
			p.Counts["detached"]++
			if err := saveRemoved(ctx, s, tx, r.ID, "convert", map[string]any{"from_type": r.TypeName, "parent": parentKey}); err != nil {
				return err
			}
			parentID = nil
		}
	}
	if newLevel == -1 && parentID == nil {
		p.block("a %s needs a parent: attach %s to a Story, Task or Bug first", newName, r.Key)
		return nil
	}
	// children that the new type cannot parent are detached and remembered
	kids, err := tx.Query(ctx, `SELECT i.id,i.key,t.level FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.parent_id=$1 AND i.deleted_at IS NULL ORDER BY i.number`, r.ID)
	if err != nil {
		return err
	}
	type kid struct {
		id, key string
		level   int
	}
	var children []kid
	for kids.Next() {
		var k kid
		kids.Scan(&k.id, &k.key, &k.level)
		children = append(children, k)
	}
	kids.Close()
	var detachedKids []string
	for _, k := range children {
		if k.level != newLevel-1 {
			p.drop("detach %s from %s (a %s cannot parent a %s)", k.key, r.Key, newName, k.key)
			p.Counts["children_detached"]++
			detachedKids = append(detachedKids, k.id)
			if _, err := tx.Exec(ctx, `UPDATE issue SET parent_id=NULL, version=version+1, updated_at=now() WHERE id=$1`, k.id); err != nil {
				return err
			}
			s.hist(ctx, tx, k.id, a, "parent", sp(r.Key), sp(""))
			outbox(ctx, tx, "issue:"+k.key, "issue.updated", map[string]any{"key": k.key, "fields": []string{"parent"}, "actor": a.Label, "actor_id": a.ID})
		}
	}
	if len(detachedKids) > 0 {
		if err := saveRemoved(ctx, s, tx, r.ID, "convert", map[string]any{"from_type": r.TypeName, "children": detachedKids}); err != nil {
			return err
		}
	}
	// converting back restores what an earlier conversion removed
	rows, err := tx.Query(ctx, `SELECT id,data FROM removed_values WHERE issue_id=$1 AND kind='convert' AND expires_at>$2 ORDER BY id DESC`, r.ID, s.Now())
	if err != nil {
		return err
	}
	type rv struct {
		id   int64
		data map[string]any
	}
	var recs []rv
	for rows.Next() {
		var x rv
		var raw []byte
		rows.Scan(&x.id, &raw)
		json.Unmarshal(raw, &x.data)
		recs = append(recs, x)
	}
	rows.Close()
	for _, x := range recs {
		if x.data["from_type"] != newName {
			continue
		}
		if pk, ok := x.data["parent"].(string); ok && parentID == nil {
			var pid string
			var pl int
			if tx.QueryRow(ctx, `SELECT p.id,t.level FROM issue p JOIN issue_type t ON t.id=p.issue_type_id WHERE p.key=$1 AND p.deleted_at IS NULL AND p.project_id=$2`, pk, r.ProjectID).Scan(&pid, &pl) == nil && pl == newLevel+1 {
				parentID = &pid
				p.change("restore the parent link to %s that the earlier conversion removed", pk)
				p.Counts["restored"]++
			}
		}
		if cs, ok := x.data["children"].([]any); ok {
			for _, c := range cs {
				cid, _ := c.(string)
				var ck string
				var cl int
				if tx.QueryRow(ctx, `SELECT i.key,t.level FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.id=$1 AND i.deleted_at IS NULL AND i.parent_id IS NULL`, cid).Scan(&ck, &cl) == nil && cl == newLevel-1 {
					tx.Exec(ctx, `UPDATE issue SET parent_id=$2, version=version+1, updated_at=now() WHERE id=$1`, cid, r.ID)
					s.hist(ctx, tx, cid, a, "parent", sp(""), sp(r.Key))
					p.change("re-attach %s that the earlier conversion detached", ck)
					p.Counts["restored"]++
				}
			}
		}
		tx.Exec(ctx, `DELETE FROM removed_values WHERE id=$1`, x.id)
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET issue_type_id=$2, parent_id=$3, version=version+1, updated_at=now() WHERE id=$1`, r.ID, newID, parentID); err != nil {
		return err
	}
	if err := s.hist(ctx, tx, r.ID, a, "type", sp(r.TypeName), sp(newName)); err != nil {
		return err
	}
	if err := outbox(ctx, tx, "issue:"+r.Key, "issue.updated", map[string]any{"key": r.Key, "fields": []string{"type"}, "actor": a.Label, "actor_id": a.ID}); err != nil {
		return err
	}
	*ev = append(*ev, Event{Type: "issue.updated", Issue: r.Key, Fields: []string{"type"}, Actor: a.Label, ActorID: a.ID})
	p.Counts["issues"] = 1
	p.Key2 = r.Key
	p.change("key, comments, attachments and links are untouched")
	return s.audit(ctx, tx, a, "issue.convert", r.Key, map[string]any{"from": r.TypeName, "to": newName})
}

// ---------------------------------------------------------------- move

func (s *Service) opMove(ctx context.Context, tx pgx.Tx, a Actor, key string, in LifecycleIn, p *Plan, ev *[]Event) error {
	r, role, err := s.visible(ctx, tx, a, key)
	if err != nil {
		return err
	}
	if r.DeletedAt != nil {
		return ErrGone
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	if in.ExpectKey != "" && !strings.EqualFold(in.ExpectKey, r.Key) {
		return &ConflictError{fmt.Sprintf("the issue is now %s, not %s; someone moved it first", r.Key, in.ExpectKey)}
	}
	p.Key = r.Key
	if in.Project == "" {
		return invalid("the target project is required")
	}
	var tpid, tkey string
	if err := tx.QueryRow(ctx, `SELECT id,key FROM project WHERE key=$1`, strings.ToUpper(in.Project)).Scan(&tpid, &tkey); err != nil {
		return ErrNotFound
	}
	trole := s.projectRole(ctx, tx, a, tpid)
	if trole == "" {
		return ErrNotFound
	}
	if !canWrite(trole) {
		return ErrForbidden
	}
	if tpid == r.ProjectID {
		p.block("%s is already in %s", r.Key, tkey)
		return nil
	}
	if r.Level == -1 {
		p.block("%s is a subtask: subtasks move with their parent", r.Key)
		return nil
	}
	// the moved set: the issue and its subtasks
	kids, err := tx.Query(ctx, `SELECT i.id,i.key,t.level FROM issue i JOIN issue_type t ON t.id=i.issue_type_id WHERE i.parent_id=$1 AND i.deleted_at IS NULL ORDER BY i.number`, r.ID)
	if err != nil {
		return err
	}
	type mv struct {
		id, key, typeID, typeName, statusName, statusCat string
		level                                            int
		assignee                                         *string
		root                                             bool
	}
	set := []mv{{id: r.ID, key: r.Key, root: true}}
	type child struct {
		id, key string
		level   int
	}
	var all []child // read every row before running another statement on this connection
	for kids.Next() {
		var c child
		kids.Scan(&c.id, &c.key, &c.level)
		all = append(all, c)
	}
	kids.Close()
	for _, c := range all {
		if c.level == -1 {
			set = append(set, mv{id: c.id, key: c.key})
			continue
		}
		// an epic's children stay behind, and the link across projects is dropped
		p.drop("%s keeps its project and loses its link to %s (parent and child must share a project)", c.key, r.Key)
		p.Counts["children_detached"]++
		if _, err := tx.Exec(ctx, `UPDATE issue SET parent_id=NULL, version=version+1, updated_at=now() WHERE id=$1`, c.id); err != nil {
			return err
		}
		if err := saveRemoved(ctx, s, tx, c.id, "move", map[string]any{"parent": r.Key}); err != nil {
			return err
		}
		s.hist(ctx, tx, c.id, a, "parent", sp(r.Key), sp(""))
	}
	for i := range set {
		if err := tx.QueryRow(ctx, `SELECT i.issue_type_id,t.name,t.level,st.name,st.category,i.assignee_id FROM issue i JOIN issue_type t ON t.id=i.issue_type_id JOIN status st ON st.id=i.status_id WHERE i.id=$1`, set[i].id).
			Scan(&set[i].typeID, &set[i].typeName, &set[i].level, &set[i].statusName, &set[i].statusCat, &set[i].assignee); err != nil {
			return err
		}
	}
	// the root's parent (an epic that stays behind) is dropped
	if r.ParentID != nil {
		var pk string
		tx.QueryRow(ctx, `SELECT key FROM issue WHERE id=$1`, *r.ParentID).Scan(&pk)
		p.drop("%s loses its link to the parent %s, which stays in %s", r.Key, pk, r.ProjectKey)
		p.Counts["parent_dropped"]++
		saveRemoved(ctx, s, tx, r.ID, "move", map[string]any{"parent": pk})
	}
	p.NewKeys = map[string]string{}
	for i, m := range set {
		// type
		tname := m.typeName
		if v, ok := in.TypeMap[m.typeName]; ok {
			tname = v
		}
		tid, tlevel, err := typeNamed(ctx, tx, tname)
		if err != nil {
			return err
		}
		if tlevel != m.level {
			p.block("%s: %s cannot become %s in a move (use convert first)", m.key, m.typeName, tname)
			continue
		}
		if tname != m.typeName {
			p.change("%s: type %s becomes %s", m.key, m.typeName, tname)
		}
		sid, sname, how, err := mapStatus(ctx, tx, m.statusName, m.statusCat, tpid, in.StatusMap)
		if err != nil {
			return err
		}
		if sname != m.statusName {
			p.change("%s: status %q becomes %q (%s)", m.key, m.statusName, sname, how)
		}
		var assignee any
		if m.assignee != nil {
			if err := s.checkAssignable(ctx, tx, tpid, *m.assignee); err == nil {
				assignee = *m.assignee
			} else {
				var an string
				tx.QueryRow(ctx, `SELECT name FROM app_user WHERE id=$1`, *m.assignee).Scan(&an)
				p.drop("%s: assignee %s has no access to %s and is cleared", m.key, an, tkey)
				p.Counts["assignee_cleared"]++
				saveRemoved(ctx, s, tx, m.id, "move", map[string]any{"assignee": *m.assignee})
			}
		}
		var num int64
		var pk string
		if err := tx.QueryRow(ctx, `UPDATE project SET next_number=next_number+1 WHERE id=$1 RETURNING next_number-1, key`, tpid).Scan(&num, &pk); err != nil {
			return err
		}
		newKey := pk + "-" + itoa(num)
		if len(p.Blockers) > 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO issue_key_alias(old_key,issue_id) VALUES($1,$2)`, m.key, m.id); err != nil {
			return err
		}
		parent := any(nil)
		if i > 0 {
			parent = r.ID
		}
		if _, err := tx.Exec(ctx, `UPDATE issue SET project_id=$2, number=$3, key=$4, issue_type_id=$5, status_id=$6, assignee_id=$7, parent_id=$8, rank=$9, version=version+1, updated_at=now() WHERE id=$1`,
			m.id, tpid, num, newKey, tid, sid, assignee, parent, rankFor(num)); err != nil {
			return err
		}
		s.hist(ctx, tx, m.id, a, "key", sp(m.key), sp(newKey))
		outbox(ctx, tx, "issue:"+newKey, "issue.moved", map[string]any{"key": newKey, "old_key": m.key, "from": r.ProjectKey, "to": tkey, "actor": a.Label, "actor_id": a.ID})
		*ev = append(*ev, Event{Type: "issue.moved", Issue: m.key, Fields: []string{newKey}, Actor: a.Label, ActorID: a.ID})
		p.NewKeys[m.key] = newKey
		if i == 0 {
			p.Key2 = newKey
			p.change("%s gets the key %s; %s keeps working as an alias forever", m.key, newKey, m.key)
		} else {
			p.change("subtask %s moves with its parent and becomes %s", m.key, newKey)
			p.Counts["subtasks_moved"]++
		}
		p.Counts["issues"]++
	}
	if len(p.Blockers) > 0 {
		return nil
	}
	p.change("comments, attachments, links, history and open drafts move with the issue")
	return s.audit(ctx, tx, a, "issue.move", r.Key, map[string]any{"to": tkey, "keys": p.NewKeys})
}

// ---------------------------------------------------------------- clone

func (s *Service) opClone(ctx context.Context, tx pgx.Tx, a Actor, key string, in LifecycleIn, p *Plan, ev *[]Event) error {
	r, role, err := s.visible(ctx, tx, a, key)
	if err != nil {
		return err
	}
	if r.DeletedAt != nil {
		return ErrGone
	}
	_ = role // Browse on the source is enough: being able to see the issue is the permission
	p.Key = r.Key
	o := in.Clone
	if o.Project == "" {
		o.Project = r.ProjectKey
	}
	var tpid, tkey string
	if err := tx.QueryRow(ctx, `SELECT id,key FROM project WHERE key=$1`, strings.ToUpper(o.Project)).Scan(&tpid, &tkey); err != nil {
		return ErrNotFound
	}
	trole := s.projectRole(ctx, tx, a, tpid)
	if trole == "" {
		return ErrNotFound
	}
	if !canWrite(trole) {
		return ErrForbidden // Create on the target
	}
	attach, subtasks, prefix := true, true, "CLONE - "
	if o.Attachments != nil {
		attach = *o.Attachments
	}
	if o.Subtasks != nil {
		subtasks = *o.Subtasks
	}
	if o.Prefix != nil {
		prefix = *o.Prefix
	}
	if r.Level == -1 && tpid != r.ProjectID {
		p.block("%s is a subtask: clone its parent to copy it into another project", r.Key)
		return nil
	}
	p.NewKeys = map[string]string{}
	cap := defaultSubtaskCap
	var cloneOne func(src rawIssue, parentID *string, root bool) (string, error)
	cloneOne = func(src rawIssue, parentID *string, root bool) (string, error) {
		var desc string
		tx.QueryRow(ctx, `SELECT body FROM issue_text WHERE issue_id=$1 AND field='description'`, src.ID).Scan(&desc)
		var prio string
		var assignee *string
		var labels []string
		var summary string
		if err := tx.QueryRow(ctx, `SELECT priority,assignee_id,labels,summary FROM issue WHERE id=$1`, src.ID).Scan(&prio, &assignee, &labels, &summary); err != nil {
			return "", err
		}
		sid, _, err := initialStatus(ctx, tx, tpid)
		if err != nil {
			return "", err
		}
		var asg any
		switch {
		case o.KeepAssignee && assignee != nil:
			if s.checkAssignable(ctx, tx, tpid, *assignee) == nil {
				asg = *assignee
			} else {
				p.warn("%s: the assignee has no access to %s; the clone is unassigned", src.Key, tkey)
			}
		case !o.KeepAssignee && root:
			asg = a.ID // the default: the clone goes to the person cloning
		}
		var num int64
		var pk string
		if err := tx.QueryRow(ctx, `UPDATE project SET next_number=next_number+1 WHERE id=$1 RETURNING next_number-1, key`, tpid).Scan(&num, &pk); err != nil {
			return "", err
		}
		newKey := pk + "-" + itoa(num)
		title := summary
		if root {
			title = prefix + summary
		}
		if len([]rune(title)) > maxSummary {
			title = string([]rune(title)[:maxSummary])
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO issue(project_id,number,key,issue_type_id,status_id,priority,reporter_id,assignee_id,labels,parent_id,summary,rank)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`, tpid, num, newKey, src.TypeID, sid, prio, a.ID, asg, labels, parentID, title, rankFor(num)).Scan(&id); err != nil {
			return "", err
		}
		// an independent copy of the current text
		if _, err := tx.Exec(ctx, `INSERT INTO issue_text(issue_id,field,body,updated_by,last_save_id) VALUES($1,'description',$2,$3,'')`, id, desc, nilIfEmpty(a.ID)); err != nil {
			return "", err
		}
		for f, v := range map[string]string{"summary": title, "description": desc} {
			if _, err := tx.Exec(ctx, `INSERT INTO text_version(issue_id,field,seq,body,actor_id,actor_label,mode) VALUES($1,$2,1,$3,$4,$5,'create')`, id, f, v, nilIfEmpty(a.ID), a.Label); err != nil {
				return "", err
			}
		}
		s.hist(ctx, tx, id, a, "created", nil, sp(newKey))
		if attach { // shared by reference: the blob is not copied, its reference count goes up
			n, err := s.cloneAttachments(ctx, tx, src.ID, id)
			if err != nil {
				return "", err
			}
			p.Counts["attachments_shared"] += n
		}
		if o.Comments {
			t, err := tx.Exec(ctx, `INSERT INTO comment(issue_id,author_id,body,internal,created_at) SELECT $2,author_id,body,internal,created_at FROM comment WHERE issue_id=$1`, src.ID, id)
			if err != nil {
				return "", err
			}
			p.Counts["comments_copied"] += int(t.RowsAffected())
		}
		if o.Links {
			t, err := tx.Exec(ctx, `INSERT INTO issue_link(from_id,to_id,type) SELECT $2,to_id,type FROM issue_link WHERE from_id=$1 AND type<>'clones' AND to_id<>$2 ON CONFLICT DO NOTHING`, src.ID, id)
			if err != nil {
				return "", err
			}
			p.Counts["links_copied"] += int(t.RowsAffected())
		}
		if !o.NoCloneLink {
			if _, err := tx.Exec(ctx, `INSERT INTO issue_link(from_id,to_id,type) VALUES($1,$2,'clones')`, id, src.ID); err != nil {
				return "", err
			}
		}
		outbox(ctx, tx, "issue:"+newKey, "issue.created", map[string]any{"key": newKey, "cloned_from": src.Key, "actor": a.Label, "actor_id": a.ID})
		*ev = append(*ev, Event{Type: "issue.created", Issue: newKey, Actor: a.Label, ActorID: a.ID})
		p.NewKeys[src.Key] = newKey
		p.Counts["issues"]++
		if root {
			p.Key2 = newKey
			p.change("clone %s as %s (%q)", src.Key, newKey, title)
		} else {
			p.change("clone subtask %s as %s", src.Key, newKey)
			p.Counts["subtasks_cloned"]++
		}
		// subtasks, recursively, up to the cap
		if subtasks {
			rows, err := tx.Query(ctx, rawSelect+`WHERE i.parent_id=$1 AND i.deleted_at IS NULL ORDER BY i.number`, src.ID)
			if err != nil {
				return "", err
			}
			var kids []rawIssue
			for rows.Next() {
				k, err := scanRaw(rows)
				if err != nil {
					rows.Close()
					return "", err
				}
				if k.Level == -1 {
					kids = append(kids, k)
				}
			}
			rows.Close()
			for _, k := range kids {
				if p.Counts["subtasks_cloned"] >= cap {
					p.warn("subtask cap of %d reached: %s and later subtasks were not cloned", cap, k.Key)
					break
				}
				if _, err := cloneOne(k, &id, false); err != nil {
					return "", err
				}
			}
		}
		return newKey, nil
	}
	if _, err := cloneOne(r, nil, true); err != nil {
		return err
	}
	p.change("never copied: history, votes, watchers, links to Git, sprint membership; the clone starts in the initial status, unresolved, reported by %s", a.Label)
	return s.audit(ctx, tx, a, "issue.clone", r.Key, map[string]any{"keys": p.NewKeys})
}

func (s *Service) cloneAttachments(ctx context.Context, tx pgx.Tx, from, to string) (int, error) {
	rows, err := tx.Query(ctx, `SELECT blob_id,name,declared_mime,type_flag,uploader_id,thumb_blob_id,thumb_state,created_at FROM attachment WHERE issue_id=$1 ORDER BY created_at,id`, from)
	if err != nil {
		return 0, err
	}
	type at struct {
		blob, name, mime, flag, thumbState string
		up, thumb                          *string
		at                                 time.Time
	}
	var all []at
	for rows.Next() {
		var x at
		rows.Scan(&x.blob, &x.name, &x.mime, &x.flag, &x.up, &x.thumb, &x.thumbState, &x.at)
		all = append(all, x)
	}
	rows.Close()
	for _, x := range all {
		if _, err := tx.Exec(ctx, `UPDATE blob SET refcount=refcount+1, zero_since=NULL WHERE id=$1`, x.blob); err != nil {
			return 0, err
		}
		if x.thumb != nil {
			if _, err := tx.Exec(ctx, `UPDATE blob SET refcount=refcount+1, zero_since=NULL WHERE id=$1`, *x.thumb); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO attachment(issue_id,blob_id,name,declared_mime,type_flag,uploader_id,thumb_blob_id,thumb_state,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			to, x.blob, x.name, x.mime, x.flag, x.up, x.thumb, x.thumbState, x.at); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}

// ---------------------------------------------------------------- links

type Link struct {
	Type    string `json:"type"`
	Dir     string `json:"direction"` // out: this issue links to Key; in: Key links to this issue
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Deleted bool   `json:"deleted"` // a placeholder: the other issue is in the trash
}

var linkTypes = map[string]bool{"relates": true, "blocks": true, "duplicates": true, "clones": true}

func (s *Service) AddLink(ctx context.Context, a Actor, key, typ, target string) error {
	if !linkTypes[typ] {
		return invalid("unknown link type %q (relates, blocks, duplicates, clones)", typ)
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	to, _, err := s.access(ctx, a, target)
	if err != nil {
		return invalid("the linked issue %s was not found", target)
	}
	if to.ID == is.ID {
		return invalid("an issue cannot link to itself")
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO issue_link(from_id,to_id,type) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, is.ID, to.ID, typ)
	return err
}

func (s *Service) listLinks(ctx context.Context, issueID string) ([]Link, error) {
	rows, err := s.DB.Query(ctx, `SELECT l.type,'out',o.key,o.summary,o.deleted_at IS NOT NULL FROM issue_link l JOIN issue o ON o.id=l.to_id WHERE l.from_id=$1
		UNION ALL SELECT l.type,'in',o.key,o.summary,o.deleted_at IS NOT NULL FROM issue_link l JOIN issue o ON o.id=l.from_id WHERE l.to_id=$1 ORDER BY 1,3`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		var l Link
		rows.Scan(&l.Type, &l.Dir, &l.Key, &l.Summary, &l.Deleted)
		if l.Deleted {
			l.Summary = "" // a deleted issue shows only a placeholder
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
