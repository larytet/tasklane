package search

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The index holds a derived copy of each issue. Every write carries an external version: the id of the outbox event that
// caused it (or, for a rebuild, the newest event id at the moment the rows were read). A late, duplicated or replayed
// write is therefore rejected by the index itself and can never overwrite a newer one.

const consumerOS = "search_os"

// OSIndex maintains the index: the outbox consumer, the online rebuild, and the reconcile job.
type OSIndex struct {
	DB    *pgxpool.Pool
	C     *OSClient
	Alias string // default "tasklane-issues"
	Now   func() time.Time
	Batch int
	// Throttle pauses between pages of a rebuild or reconcile so interactive latency is unaffected.
	Throttle time.Duration
	Sleep    func(time.Duration)
	// Shards and Replicas configure new indexes.
	Shards, Replicas int
	failures         map[int64]int
}

func (x *OSIndex) alias() string {
	if x.Alias == "" {
		return "tasklane-issues"
	}
	return x.Alias
}

func (x *OSIndex) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

func (x *OSIndex) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	if x.Sleep != nil {
		x.Sleep(d)
		return
	}
	time.Sleep(d)
}

// Mapping is the index definition: strict, so an unexpected field fails loudly instead of being mapped by guesswork.
func (x *OSIndex) Mapping() map[string]any {
	shards, replicas := x.Shards, x.Replicas
	if shards == 0 {
		shards = 1
	}
	kw := map[string]any{"type": "keyword"}
	text := map[string]any{"type": "text", "analyzer": "tl_text"}
	return map[string]any{
		"settings": map[string]any{"number_of_shards": shards, "number_of_replicas": replicas,
			"analysis": map[string]any{"analyzer": map[string]any{"tl_text": map[string]any{"tokenizer": "standard", "filter": []string{"lowercase", "asciifolding"}}}}},
		"mappings": map[string]any{"dynamic": "strict", "properties": map[string]any{
			"id": kw, "key": kw, "aliases": kw, "project": kw, "project_id": kw, "summary": text, "summary_norm": kw, "description": text,
			"comments": text, "comments_internal": text, "status": kw, "status_category": kw, "type": kw, "priority": kw, "priority_rank": map[string]any{"type": "integer"},
			"assignee": kw, "assignee_name": kw, "reporter": kw, "labels": kw, "parent": kw, "labels_count": map[string]any{"type": "integer"}, "number": map[string]any{"type": "long"},
			"created": map[string]any{"type": "date"}, "updated": map[string]any{"type": "date"}, "resolved": map[string]any{"type": "date"}, "h": kw,
		}},
	}
}

type osState struct {
	Active, Building, LastID, Previous string
	Started                            *time.Time
}

func (x *OSIndex) state(ctx context.Context) (osState, error) {
	var s osState
	err := x.DB.QueryRow(ctx, `SELECT active_index,building_index,build_last_id,previous_index,build_started FROM search_os_state WHERE id=1`).Scan(&s.Active, &s.Building, &s.LastID, &s.Previous, &s.Started)
	return s, err
}

// Ensure creates the first index and the alias when there is none, or adopts an alias that already exists.
func (x *OSIndex) Ensure(ctx context.Context) error {
	s, err := x.state(ctx)
	if err != nil {
		return err
	}
	if s.Active != "" {
		return nil
	}
	if t, err := x.C.AliasTarget(ctx, x.alias()); err != nil {
		return err
	} else if t != "" {
		_, err = x.DB.Exec(ctx, `UPDATE search_os_state SET active_index=$1 WHERE id=1`, t)
		return err
	}
	name := x.alias() + "-v1"
	if err := x.C.CreateIndex(ctx, name, x.Mapping()); err != nil {
		return err
	}
	if err := x.C.SwapAlias(ctx, x.alias(), "", name); err != nil {
		return err
	}
	_, err = x.DB.Exec(ctx, `UPDATE search_os_state SET active_index=$1 WHERE id=1`, name)
	return err
}

type docRow struct {
	ID  string
	Key string
	Doc map[string]any
}

const docSelect = `SELECT i.id::text,i.key,p.key,p.id::text,i.summary,COALESCE(d.body,''),lower(st.name),st.category,lower(t.name),i.priority,COALESCE(i.assignee_id::text,''),COALESCE(asg.name,''),
	i.reporter_id::text,i.labels,i.created_at,i.updated_at,i.resolved_at,COALESCE(par.key,''),i.number,
	COALESCE((SELECT array_agg(a.old_key) FROM issue_key_alias a WHERE a.issue_id=i.id),'{}'),
	COALESCE((SELECT string_agg(left(c.body,20000),E'\n') FROM comment c WHERE c.issue_id=i.id AND NOT c.internal),''),
	COALESCE((SELECT string_agg(left(c.body,20000),E'\n') FROM comment c WHERE c.issue_id=i.id AND c.internal),'')
	FROM issue i JOIN project p ON p.id=i.project_id JOIN issue_type t ON t.id=i.issue_type_id JOIN status st ON st.id=i.status_id LEFT JOIN issue_text d ON d.issue_id=i.id AND d.field='description'
	LEFT JOIN app_user asg ON asg.id=i.assignee_id LEFT JOIN issue par ON par.id=i.parent_id WHERE i.deleted_at IS NULL AND i.archived_at IS NULL AND NOT i.restricted`

var priorityRank = map[string]int{"highest": 5, "high": 4, "medium": 3, "low": 2, "lowest": 1}

func hashDoc(d map[string]any) string {
	raw, _ := json.Marshal(d) // maps marshal with sorted keys: the same content always gives the same bytes
	sum := md5.Sum(raw)
	return hex.EncodeToString(sum[:])
}

func (x *OSIndex) docs(ctx context.Context, q pgx.Tx, where string, args ...any) ([]docRow, error) {
	var rows pgx.Rows
	var err error
	if q != nil {
		rows, err = q.Query(ctx, docSelect+where, args...)
	} else {
		rows, err = x.DB.Query(ctx, docSelect+where, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []docRow
	for rows.Next() {
		var id, key, project, projectID, summary, desc, status, cat, typ, prio, assignee, assigneeName, reporter, parent, comments, internal string
		var labels, aliases []string
		var created, updated time.Time
		var resolved *time.Time
		var number int64
		if err := rows.Scan(&id, &key, &project, &projectID, &summary, &desc, &status, &cat, &typ, &prio, &assignee, &assigneeName, &reporter, &labels, &created, &updated, &resolved, &parent, &number, &aliases, &comments, &internal); err != nil {
			return nil, err
		}
		if labels == nil {
			labels = []string{}
		}
		sort.Strings(aliases)
		d := map[string]any{"id": id, "key": key, "aliases": aliases, "project": strings.ToUpper(project), "project_id": projectID, "summary": summary, "summary_norm": strings.ToLower(summary),
			"description": desc, "comments": comments, "comments_internal": internal, "status": status, "status_category": cat, "type": typ, "priority": prio, "priority_rank": priorityRank[prio],
			"assignee": assignee, "assignee_name": strings.ToLower(assigneeName), "reporter": reporter, "labels": labels, "parent": parent, "labels_count": len(labels), "number": number,
			"created": created.UTC().Format(time.RFC3339Nano), "updated": updated.UTC().Format(time.RFC3339Nano)}
		if resolved != nil {
			d["resolved"] = resolved.UTC().Format(time.RFC3339Nano)
		}
		d["h"] = hashDoc(d)
		out = append(out, docRow{ID: id, Key: key, Doc: d})
	}
	return out, rows.Err()
}

func (x *OSIndex) targets(ctx context.Context) ([]string, error) {
	s, err := x.state(ctx)
	if err != nil {
		return nil, err
	}
	t := []string{}
	if s.Active != "" {
		t = append(t, s.Active)
	}
	if s.Building != "" {
		t = append(t, s.Building) // dual write while a rebuild is running
	}
	if len(t) == 0 {
		return nil, errors.New("the search index is not set up: run Ensure first")
	}
	return t, nil
}

// OSBacklog is how far behind the index is.
type OSBacklog struct {
	Events int64
	Lag    time.Duration // age of the oldest event the index has not applied
}

func (x *OSIndex) Backlog(ctx context.Context) (OSBacklog, error) {
	var b OSBacklog
	var oldest *time.Time
	err := x.DB.QueryRow(ctx, `SELECT count(*), min(created_at) FROM outbox WHERE id > COALESCE((SELECT last_id FROM outbox_cursor WHERE consumer=$1),0) AND aggregate LIKE 'issue:%'`, consumerOS).Scan(&b.Events, &oldest)
	if err != nil {
		return b, err
	}
	if oldest != nil {
		b.Lag = x.now().Sub(*oldest)
		if b.Lag < 0 {
			b.Lag = 0
		}
	}
	return b, nil
}

// DeadLetters counts writes the index rejected that nobody has resolved; an operator alarm.
func (x *OSIndex) DeadLetters(ctx context.Context) (int, error) {
	var n int
	err := x.DB.QueryRow(ctx, `SELECT count(*) FROM search_os_dead WHERE NOT resolved`).Scan(&n)
	return n, err
}

// RunOnce applies one batch of outbox events to the index. An outage (the cluster unreachable or answering with a server
// error) returns an error and leaves the cursor where it was, so the backlog simply grows and drains afterwards. A write the
// index rejects is retried and then parked in the dead-letter table, so one bad document never blocks the rest.
func (x *OSIndex) RunOnce(ctx context.Context) (int, error) {
	if x.Batch == 0 {
		x.Batch = 500
	}
	if x.failures == nil {
		x.failures = map[int64]int{}
	}
	targets, err := x.targets(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := x.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_cursor(consumer) VALUES($1) ON CONFLICT DO NOTHING`, consumerOS); err != nil {
		return 0, err
	}
	var last int64
	if err := tx.QueryRow(ctx, `SELECT last_id FROM outbox_cursor WHERE consumer=$1 FOR UPDATE`, consumerOS).Scan(&last); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id,payload FROM outbox WHERE id > $1 AND aggregate LIKE 'issue:%' ORDER BY id LIMIT $2`, last, x.Batch)
	if err != nil {
		return 0, err
	}
	maxFor := map[string]int64{}
	var top int64
	n := 0
	for rows.Next() {
		var id int64
		var raw []byte
		rows.Scan(&id, &raw)
		var p struct {
			Key string `json:"key"`
		}
		json.Unmarshal(raw, &p)
		if p.Key != "" {
			maxFor[strings.ToUpper(p.Key)] = id
		}
		top = id
		n++
	}
	rows.Close()
	if n == 0 {
		return 0, nil
	}
	var ops []BulkOp
	var byKey []string // purged issues have no row to resolve: removed by key
	for key, ver := range maxFor {
		var id string
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT i.id::text, (i.deleted_at IS NOT NULL OR i.archived_at IS NOT NULL OR i.restricted) FROM issue i WHERE i.key=$1 OR i.id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1) ORDER BY (i.key=$1) DESC LIMIT 1`, key).Scan(&id, &deleted)
		if err == pgx.ErrNoRows {
			byKey = append(byKey, key)
			continue
		}
		if err != nil {
			return 0, err
		}
		if deleted {
			for _, t := range targets {
				ops = append(ops, BulkOp{Index: t, ID: id, Version: 2 * ver, Delete: true})
			}
			continue
		}
		ds, err := x.docs(ctx, tx, ` AND i.id::text=$1`, id)
		if err != nil {
			return 0, err
		}
		for _, d := range ds {
			for _, t := range targets {
				ops = append(ops, BulkOp{Index: t, ID: d.ID, Version: 2 * ver, Doc: d.Doc})
			}
		}
	}
	res, err := x.C.Bulk(ctx, ops)
	if err != nil {
		return 0, err // an outage: nothing is advanced
	}
	if res.Failed > 0 {
		x.failures[top]++
		if x.failures[top] < 3 {
			return 0, fmt.Errorf("the index rejected %d writes (%s); will retry", res.Failed, res.FirstError)
		}
		for key, ver := range maxFor {
			if _, err := tx.Exec(ctx, `INSERT INTO search_os_dead(event_id,issue_key,error) VALUES($1,$2,$3)`, ver, key, res.FirstError); err != nil {
				return 0, err
			}
		}
		delete(x.failures, top)
	}
	for _, key := range byKey {
		for _, t := range targets {
			if _, err := x.C.DeleteByQuery(ctx, t, map[string]any{"term": map[string]any{"key": key}}); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox_cursor SET last_id=$2 WHERE consumer=$1`, consumerOS, top); err != nil {
		return 0, err
	}
	return n, tx.Commit(ctx)
}

// Run applies events until the context ends, backing off while the cluster is down.
func (x *OSIndex) Run(ctx context.Context, logf func(string, ...any)) {
	delay := time.Second
	for ctx.Err() == nil {
		n, err := x.RunOnce(ctx)
		switch {
		case err != nil:
			logf("search index: %v (retrying in %s)", err, delay)
			x.sleep(delay)
			if delay < 30*time.Second {
				delay *= 2
			}
		case n == 0:
			delay = time.Second
			x.sleep(500 * time.Millisecond)
		default:
			delay = time.Second
		}
	}
}

// snapshotVersion is the version a repair or rebuild writes at: above every event already in the outbox (event versions are
// 2*id) and below the next one (2*(id+1)), and accepted when equal (external_gte), so a repair can replace a drifted
// document written at the same moment but never a newer event's write.
func (x *OSIndex) snapshotVersion(ctx context.Context) (int64, error) {
	var v int64
	err := x.DB.QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM outbox`).Scan(&v)
	return 2*v + 1, err
}

// ReindexProject rewrites one project's documents into the live (and building) index and removes the ones that no longer
// exist, without a new index.
func (x *OSIndex) ReindexProject(ctx context.Context, project string) (int, error) {
	targets, err := x.targets(ctx)
	if err != nil {
		return 0, err
	}
	ver, err := x.snapshotVersion(ctx)
	if err != nil {
		return 0, err
	}
	ds, err := x.docs(ctx, nil, ` AND upper(p.key)=$1`, strings.ToUpper(project))
	if err != nil {
		return 0, err
	}
	want := map[string]bool{}
	var ops []BulkOp
	for _, d := range ds {
		want[d.ID] = true
		for _, t := range targets {
			ops = append(ops, BulkOp{Index: t, ID: d.ID, Version: ver, GTE: true, Doc: d.Doc})
		}
	}
	have, err := x.scan(ctx, targets[0], strings.ToUpper(project))
	if err != nil {
		return 0, err
	}
	for id := range have {
		if !want[id] {
			for _, t := range targets {
				ops = append(ops, BulkOp{Index: t, ID: id, Version: ver, GTE: true, Delete: true})
			}
		}
	}
	for i := 0; i < len(ops); i += 500 {
		end := i + 500
		if end > len(ops) {
			end = len(ops)
		}
		if res, err := x.C.Bulk(ctx, ops[i:end]); err != nil {
			return 0, err
		} else if res.Failed > 0 {
			return 0, fmt.Errorf("the index rejected %d writes: %s", res.Failed, res.FirstError)
		}
		x.sleep(x.Throttle)
	}
	return len(ds), nil
}

// scan lists id -> content hash of every document of a project (all projects when empty) in an index.
func (x *OSIndex) scan(ctx context.Context, index, project string) (map[string]string, error) {
	out := map[string]string{}
	var after any
	for {
		body := map[string]any{"size": 500, "_source": []string{"h"}, "sort": []any{map[string]any{"id": "asc"}}, "track_total_hits": false}
		if project != "" {
			body["query"] = map[string]any{"term": map[string]any{"project": project}}
		}
		if after != nil {
			body["search_after"] = []any{after}
		}
		sr, err := x.C.Search(ctx, index, body)
		if err != nil {
			return nil, err
		}
		if len(sr.Hits) == 0 {
			return out, nil
		}
		for _, h := range sr.Hits {
			hh, _ := h.Src["h"].(string)
			out[h.ID] = hh
			after = h.ID
		}
		x.sleep(x.Throttle)
	}
}

// RebuildReport says what an online rebuild did.
type RebuildReport struct {
	Index     string
	Documents int
	Resumed   bool
	Verified  bool
	Swapped   bool
}

// Rebuild builds a new versioned index while writes continue (dual write), loads every issue from PostgreSQL in keyset
// pages, verifies count and content hashes, and moves the alias in one atomic step. It is resumable: progress is stored
// after every page, so a killed run continues where it stopped. The previous index is kept as the rollback window.
func (x *OSIndex) Rebuild(ctx context.Context, stopAfterPages int) (RebuildReport, error) {
	var rep RebuildReport
	if err := x.Ensure(ctx); err != nil {
		return rep, err
	}
	s, err := x.state(ctx)
	if err != nil {
		return rep, err
	}
	if s.Building == "" {
		name := fmt.Sprintf("%s-v%d", x.alias(), x.now().UnixNano())
		if err := x.C.CreateIndex(ctx, name, x.Mapping()); err != nil {
			return rep, err
		}
		if _, err := x.DB.Exec(ctx, `UPDATE search_os_state SET building_index=$1,build_last_id='',build_started=$2 WHERE id=1`, name, x.now()); err != nil {
			return rep, err
		}
		s.Building, s.LastID = name, ""
	} else {
		rep.Resumed = true
	}
	rep.Index = s.Building
	pages := 0
	for {
		ver, err := x.snapshotVersion(ctx) // read the version first, then the rows: the rows are at least that new
		if err != nil {
			return rep, err
		}
		ds, err := x.docs(ctx, nil, ` AND i.id::text > $1 ORDER BY i.id LIMIT $2`, s.LastID, 500)
		if err != nil {
			return rep, err
		}
		if len(ds) == 0 {
			break
		}
		ops := make([]BulkOp, 0, len(ds))
		for _, d := range ds {
			ops = append(ops, BulkOp{Index: s.Building, ID: d.ID, Version: ver, GTE: true, Doc: d.Doc})
		}
		res, err := x.C.Bulk(ctx, ops)
		if err != nil {
			return rep, err
		}
		if res.Failed > 0 {
			return rep, fmt.Errorf("the new index rejected %d writes: %s", res.Failed, res.FirstError)
		}
		s.LastID = ds[len(ds)-1].ID
		rep.Documents += len(ds)
		if _, err := x.DB.Exec(ctx, `UPDATE search_os_state SET build_last_id=$1 WHERE id=1`, s.LastID); err != nil {
			return rep, err
		}
		if pages++; stopAfterPages > 0 && pages >= stopAfterPages {
			return rep, errStopped
		}
		x.sleep(x.Throttle)
	}
	// verify: the new index must equal PostgreSQL, document by document
	if err := x.verify(ctx, s.Building); err != nil {
		return rep, err
	}
	rep.Verified = true
	if err := x.C.Refresh(ctx, s.Building); err != nil {
		return rep, err
	}
	if err := x.C.SwapAlias(ctx, x.alias(), s.Active, s.Building); err != nil {
		return rep, err
	}
	if _, err := x.DB.Exec(ctx, `UPDATE search_os_state SET active_index=building_index,previous_index=$1,building_index='',build_last_id='',build_started=NULL WHERE id=1`, s.Active); err != nil {
		return rep, err
	}
	rep.Swapped = true
	return rep, nil
}

var errStopped = errors.New("rebuild paused")

// IsPaused reports whether err is the deliberate pause of a rebuild (tests use it to simulate a kill).
func IsPaused(err error) bool { return errors.Is(err, errStopped) }

func (x *OSIndex) verify(ctx context.Context, index string) error {
	if err := x.C.Refresh(ctx, index); err != nil {
		return err
	}
	have, err := x.scan(ctx, index, "")
	if err != nil {
		return err
	}
	ds, err := x.docs(ctx, nil, "")
	if err != nil {
		return err
	}
	if len(ds) != len(have) {
		return fmt.Errorf("verification failed: PostgreSQL has %d issues, the new index %d documents", len(ds), len(have))
	}
	for _, d := range ds {
		if have[d.ID] != d.Doc["h"] {
			return fmt.Errorf("verification failed: %s differs between PostgreSQL and the new index", d.Key)
		}
	}
	return nil
}

// Rollback points the alias back at the previous index (the rollback window after a swap).
func (x *OSIndex) Rollback(ctx context.Context) error {
	s, err := x.state(ctx)
	if err != nil {
		return err
	}
	if s.Previous == "" {
		return errors.New("there is no previous index to go back to")
	}
	if err := x.C.SwapAlias(ctx, x.alias(), s.Active, s.Previous); err != nil {
		return err
	}
	_, err = x.DB.Exec(ctx, `UPDATE search_os_state SET active_index=$1,previous_index=$2 WHERE id=1`, s.Previous, s.Active)
	return err
}

// DropPrevious deletes the rollback index once the new one is trusted.
func (x *OSIndex) DropPrevious(ctx context.Context) error {
	s, err := x.state(ctx)
	if err != nil || s.Previous == "" {
		return err
	}
	if err := x.C.DeleteIndex(ctx, s.Previous); err != nil {
		return err
	}
	_, err = x.DB.Exec(ctx, `UPDATE search_os_state SET previous_index='' WHERE id=1`)
	return err
}

// ReconcileReport says what drift was found and repaired.
type ReconcileReport struct {
	Project  string   `json:"project,omitempty"`
	Checked  int      `json:"checked"`
	Missing  []string `json:"missing"`
	Stale    []string `json:"stale"`
	Orphans  []string `json:"orphans"`
	Repaired int      `json:"repaired"`
}

func (r ReconcileReport) Clean() bool { return len(r.Missing)+len(r.Stale)+len(r.Orphans) == 0 }

// Reconcile compares PostgreSQL and the live index (for one project, or all) and repairs missing, stale and orphaned
// documents, so silent drift is bounded and visible. Repairs carry the current event version, so a newer write still wins.
func (x *OSIndex) Reconcile(ctx context.Context, project string) (ReconcileReport, error) {
	rep := ReconcileReport{Project: project, Missing: []string{}, Stale: []string{}, Orphans: []string{}}
	targets, err := x.targets(ctx)
	if err != nil {
		return rep, err
	}
	ver, err := x.snapshotVersion(ctx)
	if err != nil {
		return rep, err
	}
	where, args := "", []any{}
	if project != "" {
		where, args = ` AND upper(p.key)=$1`, []any{strings.ToUpper(project)}
	}
	ds, err := x.docs(ctx, nil, where, args...)
	if err != nil {
		return rep, err
	}
	have, err := x.scan(ctx, targets[0], strings.ToUpper(project))
	if err != nil {
		return rep, err
	}
	var ops []BulkOp
	want := map[string]bool{}
	for _, d := range ds {
		want[d.ID] = true
		rep.Checked++
		h, ok := have[d.ID]
		switch {
		case !ok:
			rep.Missing = append(rep.Missing, d.Key)
		case h != d.Doc["h"]:
			rep.Stale = append(rep.Stale, d.Key)
		default:
			continue
		}
		for _, t := range targets {
			ops = append(ops, BulkOp{Index: t, ID: d.ID, Version: ver, GTE: true, Doc: d.Doc})
		}
	}
	for id := range have {
		if !want[id] {
			rep.Orphans = append(rep.Orphans, id)
			for _, t := range targets {
				ops = append(ops, BulkOp{Index: t, ID: id, Version: ver, GTE: true, Delete: true})
			}
		}
	}
	sort.Strings(rep.Orphans)
	for i := 0; i < len(ops); i += 500 {
		end := i + 500
		if end > len(ops) {
			end = len(ops)
		}
		res, err := x.C.Bulk(ctx, ops[i:end])
		if err != nil {
			return rep, err
		}
		rep.Repaired += res.Written
		x.sleep(x.Throttle)
	}
	raw, _ := json.Marshal(rep)
	x.DB.Exec(ctx, `UPDATE search_os_state SET last_reconcile=$1,reconciled_at=$2 WHERE id=1`, raw, x.now())
	return rep, nil
}

// PendingKeys lists the issues that have events the index has not applied yet: the only ones whose indexed copy can be
// stale, so the only ones a person's own-edit overlay needs to look at.
func (x *OSIndex) PendingKeys(ctx context.Context) ([]string, error) {
	rows, err := x.DB.Query(ctx, `SELECT DISTINCT upper(payload->>'key') FROM outbox WHERE id > COALESCE((SELECT last_id FROM outbox_cursor WHERE consumer=$1),0) AND aggregate LIKE 'issue:%' AND payload->>'key' IS NOT NULL`, consumerOS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		out = append(out, k)
	}
	return out, rows.Err()
}
