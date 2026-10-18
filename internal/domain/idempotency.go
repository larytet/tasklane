package domain

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Idempotency keys (MCP-12). A call is claimed before it runs: the same key arriving again while it runs is told to wait,
// after it finished gets the stored result, and with different arguments is refused. A call that failed releases its key,
// so retrying it is not blocked by its own failure.

const idempotencyTTL = 24 * time.Hour
const idempotencyStale = 5 * time.Minute

// IdempotencyBegin claims the key. state is "new" (go ahead), "replay" (stored is the first result), "inflight" (the first
// call has not finished) or "mismatch" (the key was used with other arguments).
func (s *Service) IdempotencyBegin(ctx context.Context, userID, key, reqHash string) (state string, stored []byte, err error) {
	s.DB.Exec(ctx, `DELETE FROM idempotency_key WHERE created_at < $1`, s.Now().Add(-idempotencyTTL))
	tag, err := s.DB.Exec(ctx, `INSERT INTO idempotency_key(user_id,key,request_hash,created_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, userID, key, reqHash, s.Now())
	if err != nil {
		return "", nil, err
	}
	if tag.RowsAffected() == 1 {
		return "new", nil, nil
	}
	var h, st string
	var res []byte
	var at time.Time
	if err := s.DB.QueryRow(ctx, `SELECT request_hash,state,result,created_at FROM idempotency_key WHERE user_id=$1 AND key=$2`, userID, key).Scan(&h, &st, &res, &at); err == pgx.ErrNoRows {
		return s.IdempotencyBegin(ctx, userID, key, reqHash) // it was released between the two statements
	} else if err != nil {
		return "", nil, err
	}
	switch {
	case h != reqHash:
		return "mismatch", nil, nil
	case st == "done":
		return "replay", res, nil
	case s.Now().Sub(at) > idempotencyStale:
		// the first call never finished (the server stopped): take the key over
		tag, err := s.DB.Exec(ctx, `UPDATE idempotency_key SET created_at=$3 WHERE user_id=$1 AND key=$2 AND state='pending' AND created_at=$4`, userID, key, s.Now(), at)
		if err == nil && tag.RowsAffected() == 1 {
			return "new", nil, nil
		}
	}
	return "inflight", nil, nil
}

func (s *Service) IdempotencyFinish(ctx context.Context, userID, key string, result []byte) {
	s.DB.Exec(ctx, `UPDATE idempotency_key SET state='done',result=$3 WHERE user_id=$1 AND key=$2`, userID, key, result)
}

func (s *Service) IdempotencyAbort(ctx context.Context, userID, key string) {
	s.DB.Exec(ctx, `DELETE FROM idempotency_key WHERE user_id=$1 AND key=$2 AND state='pending'`, userID, key)
}
