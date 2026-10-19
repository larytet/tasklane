package domain

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// errBulkItemTaken: another worker already finished this item; the operation rolls back and nothing is done twice.
var errBulkItemTaken = errors.New("bulk item already finished")

// Bulk lifecycle operations (LIFE-16): delete, restore, clone, move or convert many issues as one job that survives the death of the node that
// runs it. The job is rows: a list of items and a lease. A worker takes the lease, does the next pending items one at a time, and each item is
// marked done in the same transaction that does the operation (see Lifecycle), so no node can do an item twice and a crash costs at most the
// item in flight. Items that fail are marked failed with the reason and the job goes on; a pause on a project makes the worker stop where it
// is and take the job up again after the pause.

const (
	maxBulkItems  = 50000
	bulkLeaseFor  = time.Minute
	bulkOpAllowed = "delete restore clone move convert"
)

// BulkIn is what the caller asks for: the operation (with its options, as for one issue) and the issues, as keys or as a TQL query.
type BulkIn struct {
	Keys  []string    `json:"keys"`
	Query string      `json:"query"`
	Op    LifecycleIn `json:"op"`
}

// BulkJob is a job and how far it has got.
type BulkJob struct {
	ID         int64      `json:"id"`
	Op         string     `json:"op"`
	State      string     `json:"state"`
	Total      int        `json:"total"`
	Done       int        `json:"done"`
	Failed     int        `json:"failed"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// ETA is the estimated end, from the rate since the job started; absent until something is done.
	ETA    *time.Time   `json:"eta,omitempty"`
	Errors []BulkFailed `json:"errors,omitempty"`
}

type BulkFailed struct {
	Key   string `json:"key"`
	Error string `json:"error"`
}

// bulkMark is what Lifecycle needs to finish an item in its own transaction.
type bulkMark struct {
	job int64
	idx int
}

// StartBulk records the job. Nothing is done to any issue yet; a worker does that.
func (s *Service) StartBulk(ctx context.Context, a Actor, in BulkIn) (BulkJob, error) {
	op := strings.ToLower(in.Op.Op)
	if !strings.Contains(" "+bulkOpAllowed+" ", " "+op+" ") {
		return BulkJob{}, invalid("a bulk job can delete, restore, clone, move or convert (purge stays one issue at a time)")
	}
	keys := in.Keys
	if in.Query != "" {
		if s.Matcher == nil {
			return BulkJob{}, invalid("searching is not available here")
		}
		found, err := s.Matcher.Keys(ctx, in.Query, maxBulkItems+1)
		if err != nil {
			return BulkJob{}, invalid("the query is not valid: %v", err)
		}
		keys = append(keys, found...)
	}
	seen := map[string]bool{}
	var uniq []string
	for _, k := range keys {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k != "" && !seen[k] {
			seen[k] = true
			uniq = append(uniq, k)
		}
	}
	if len(uniq) == 0 {
		return BulkJob{}, invalid("no issues given")
	}
	if len(uniq) > maxBulkItems {
		return BulkJob{}, invalid("at most %d issues in one job", maxBulkItems)
	}
	var unknown []string // not found and not visible look the same, so the answer teaches nothing about other people's issues
	for _, k := range uniq {
		if !s.CanRead(ctx, a, k) {
			if len(unknown) < 5 {
				unknown = append(unknown, k)
			}
		}
	}
	if len(unknown) > 0 {
		return BulkJob{}, invalid("not found: %s", strings.Join(unknown, ", "))
	}
	in.Op.DryRun = false
	var id int64
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO bulk_job(op,params,created_by,total) VALUES($1,$2,$3,$4) RETURNING id`, op, jsonMarshal(in.Op), nilIfEmpty(a.ID), len(uniq)).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO bulk_job_item(job_id,idx,key) SELECT $1, ord-1, k FROM unnest($2::text[]) WITH ORDINALITY AS t(k,ord)`, id, uniq)
		return err
	})
	if err != nil {
		return BulkJob{}, err
	}
	return s.BulkStatus(ctx, a, id)
}

// BulkStatus reports a job to the person who started it and to administrators.
func (s *Service) BulkStatus(ctx context.Context, a Actor, id int64) (BulkJob, error) {
	var j BulkJob
	var by *string
	err := s.DB.QueryRow(ctx, `SELECT id,op,state,total,created_at,started_at,finished_at,created_by::text FROM bulk_job WHERE id=$1`, id).
		Scan(&j.ID, &j.Op, &j.State, &j.Total, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &by)
	if err != nil || (!a.Admin && (by == nil || *by != a.ID)) {
		return BulkJob{}, ErrNotFound
	}
	s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='done'), count(*) FILTER (WHERE state='failed') FROM bulk_job_item WHERE job_id=$1`, id).Scan(&j.Done, &j.Failed)
	if j.StartedAt != nil && j.State == "running" && j.Done > 0 {
		el := s.Now().Sub(*j.StartedAt)
		if el > 0 {
			rest := time.Duration(float64(el) * float64(j.Total-j.Done-j.Failed) / float64(j.Done+j.Failed))
			eta := s.Now().Add(rest)
			j.ETA = &eta
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT key,error FROM bulk_job_item WHERE job_id=$1 AND state='failed' ORDER BY idx LIMIT 50`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var f BulkFailed
			rows.Scan(&f.Key, &f.Error)
			j.Errors = append(j.Errors, f)
		}
	}
	return j, nil
}

// CancelBulk stops a job after the item in flight; what was done stays done.
func (s *Service) CancelBulk(ctx context.Context, a Actor, id int64) error {
	if _, err := s.BulkStatus(ctx, a, id); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, `UPDATE bulk_job SET state='cancelled',finished_at=$2 WHERE id=$1 AND state IN ('pending','running')`, id, s.Now())
	return err
}

// ProcessBulk takes one job that is open and not leased by a live worker, and does up to limit of its pending items. It returns how many items
// it finished (done or failed). A job whose items are all finished is marked done.
func (s *Service) ProcessBulk(ctx context.Context, limit int) (int, error) {
	var id int64
	var op string
	var params []byte
	var by *string
	err := s.DB.QueryRow(ctx, `UPDATE bulk_job SET state='running',started_at=COALESCE(started_at,$1),leased_until=$2 WHERE id=(
			SELECT id FROM bulk_job WHERE state IN ('pending','running') AND (leased_until IS NULL OR leased_until<$1) ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id,op,params,created_by::text`, s.Now(), s.Now().Add(bulkLeaseFor)).Scan(&id, &op, &params, &by)
	if err != nil {
		return 0, nil // nothing to do
	}
	var base LifecycleIn
	jsonUnmarshal(params, &base)
	var a Actor
	if by != nil {
		a, _ = s.actorFor(ctx, *by)
	}
	if a.ID == "" {
		s.DB.Exec(ctx, `UPDATE bulk_job SET state='cancelled',finished_at=$2 WHERE id=$1`, id, s.Now())
		return 0, nil // the person who started it is gone
	}
	done := 0
	for done < limit {
		var idx int
		var key string
		if err := s.DB.QueryRow(ctx, `SELECT idx,key FROM bulk_job_item WHERE job_id=$1 AND state='pending' ORDER BY idx LIMIT 1`, id).Scan(&idx, &key); err != nil {
			s.DB.Exec(ctx, `UPDATE bulk_job SET state='done',finished_at=$2,leased_until=NULL WHERE id=$1 AND state='running'`, id, s.Now())
			return done, nil
		}
		var state string
		s.DB.QueryRow(ctx, `SELECT state FROM bulk_job WHERE id=$1`, id).Scan(&state)
		if state != "running" {
			return done, nil // cancelled
		}
		in := base
		in.bulk = &bulkMark{job: id, idx: idx}
		_, err := s.Lifecycle(ctx, a, key, in)
		var paused *ErrPaused
		switch {
		case errors.As(err, &paused) || ctx.Err() != nil:
			s.DB.Exec(ctx, `UPDATE bulk_job SET leased_until=NULL WHERE id=$1`, id)
			return done, nil // a pause (or a stop): the job is taken up again later
		case errors.Is(err, errBulkItemTaken):
			continue
		case err != nil:
			msg := err.Error()
			var bl *BlockedError
			if errors.As(err, &bl) && len(bl.Plan.Blockers) > 0 {
				msg = strings.Join(bl.Plan.Blockers, "; ")
			}
			if len(msg) > 300 {
				msg = msg[:300]
			}
			s.DB.Exec(ctx, `UPDATE bulk_job_item SET state='failed',error=$3,done_at=$4 WHERE job_id=$1 AND idx=$2 AND state='pending'`, id, idx, msg, s.Now())
		}
		done++
		s.DB.Exec(ctx, `UPDATE bulk_job SET leased_until=$2 WHERE id=$1`, id, s.Now().Add(bulkLeaseFor))
	}
	s.DB.Exec(ctx, `UPDATE bulk_job SET leased_until=NULL WHERE id=$1`, id) // more to do: the next call (on any node) carries on
	return done, nil
}

// RunBulk is the worker loop.
func (s *Service) RunBulk(ctx context.Context, logf func(string, ...any)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		for {
			n, err := s.ProcessBulk(ctx, 100)
			if err != nil && ctx.Err() == nil {
				logf("bulk jobs: %v", err)
			}
			if n == 0 || ctx.Err() != nil {
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
