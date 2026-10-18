package search

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Indexer is an outbox consumer that maintains issue_search and comment_search. It recomputes a
// document from the current PostgreSQL state, so duplicates, retries and out-of-order delivery
// cannot regress it, and it skips the write when nothing searchable changed (TQL-27).
type Indexer struct {
	DB    *pgxpool.Pool
	Batch int
}

// RunOnce processes one batch of pending events; it returns how many events were consumed.
func (ix *Indexer) RunOnce(ctx context.Context) (int, error) {
	if ix.Batch == 0 {
		ix.Batch = 200
	}
	tx, err := ix.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id, payload FROM outbox WHERE processed_at IS NULL AND aggregate LIKE 'issue:%' ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, ix.Batch)
	if err != nil {
		return 0, err
	}
	var ids []int64
	keys := map[string]bool{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		var p struct {
			Key string `json:"key"`
		}
		json.Unmarshal(raw, &p)
		if p.Key != "" {
			keys[p.Key] = true
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}
	for k := range keys {
		if err := indexIssue(ctx, tx, k); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET processed_at=now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

const issueSearchSelect = `
INSERT INTO issue_search(issue_id, project_id, key_text, summary_norm, tsv, text_hash, updated_at)
SELECT i.id, i.project_id, i.key, lower(unaccent(i.summary)),
  setweight(to_tsvector('simple', unaccent(i.summary)), 'A') ||
  setweight(to_tsvector('simple', unaccent(array_to_string(i.labels, ' '))), 'B') ||
  setweight(to_tsvector('simple', unaccent(COALESCE(d.body, ''))), 'C'),
  decode(md5(i.project_id::text || '|' || i.key || '|' || i.summary || '|' || array_to_string(i.labels, ',') || '|' || COALESCE(d.body, '')), 'hex'),
  now()
FROM issue i LEFT JOIN issue_text d ON d.issue_id = i.id AND d.field = 'description'
WHERE i.deleted_at IS NULL`

const issueSearchConflict = `
ON CONFLICT (issue_id) DO UPDATE SET project_id = EXCLUDED.project_id, key_text = EXCLUDED.key_text,
  summary_norm = EXCLUDED.summary_norm, tsv = EXCLUDED.tsv, text_hash = EXCLUDED.text_hash, updated_at = now()
WHERE issue_search.text_hash <> EXCLUDED.text_hash`

const upsertIssueSearch = issueSearchSelect + ` AND i.key = $1` + issueSearchConflict

// ReindexAll rebuilds every search document from PostgreSQL in one set-based pass (used by
// `tasklane reindex` and by tests that load large datasets). Unchanged documents are not rewritten.
func ReindexAll(ctx context.Context, db *pgxpool.Pool) error {
	if _, err := db.Exec(ctx, issueSearchSelect+issueSearchConflict); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `
INSERT INTO comment_search(comment_id, issue_id, project_id, internal, tsv, created_at)
SELECT c.id, c.issue_id, i.project_id, c.internal, to_tsvector('simple', unaccent(c.body)), c.created_at
FROM comment c JOIN issue i ON i.id = c.issue_id WHERE i.deleted_at IS NULL
ON CONFLICT (comment_id) DO UPDATE SET internal = EXCLUDED.internal, tsv = EXCLUDED.tsv, project_id = EXCLUDED.project_id`); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `DELETE FROM issue_search s WHERE NOT EXISTS (SELECT 1 FROM issue i WHERE i.id=s.issue_id AND i.deleted_at IS NULL)`); err != nil {
		return err
	}
	_, err := db.Exec(ctx, `ANALYZE issue_search; ANALYZE comment_search; ANALYZE issue`)
	return err
}

func indexIssue(ctx context.Context, tx pgx.Tx, key string) error {
	var id string
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT id, deleted_at IS NOT NULL FROM issue WHERE key=$1`, key).Scan(&id, &deleted)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if deleted { // trashed issues leave the index; restore re-adds them
		_, err = tx.Exec(ctx, `DELETE FROM issue_search WHERE issue_id=$1`, id)
		return err
	}
	if _, err := tx.Exec(ctx, upsertIssueSearch, key); err != nil {
		return err
	}
	// comments: one small row each, with their own visibility
	if _, err := tx.Exec(ctx, `
INSERT INTO comment_search(comment_id, issue_id, project_id, internal, tsv, created_at)
SELECT c.id, c.issue_id, i.project_id, c.internal, to_tsvector('simple', unaccent(c.body)), c.created_at
FROM comment c JOIN issue i ON i.id = c.issue_id WHERE c.issue_id = $1
ON CONFLICT (comment_id) DO UPDATE SET internal = EXCLUDED.internal, tsv = EXCLUDED.tsv, project_id = EXCLUDED.project_id`, id); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM comment_search WHERE issue_id=$1 AND comment_id NOT IN (SELECT id FROM comment WHERE issue_id=$1)`, id)
	return err
}

// Run polls until the context ends; used by the server. Failures are logged and retried.
func (ix *Indexer) Run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		for {
			n, err := ix.RunOnce(ctx)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("indexer: %v", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Backlog reports unprocessed events and the age of the oldest, for the lag indicator.
func (ix *Indexer) Backlog(ctx context.Context) (int, time.Duration, error) {
	var n int
	var age *time.Duration
	var oldest *time.Time
	err := ix.DB.QueryRow(ctx, `SELECT count(*), min(created_at) FROM outbox WHERE processed_at IS NULL AND aggregate LIKE 'issue:%'`).Scan(&n, &oldest)
	if oldest != nil {
		d := time.Since(*oldest)
		age = &d
	}
	if age == nil {
		return n, 0, err
	}
	return n, *age, err
}
