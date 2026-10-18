package domain

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The audit log (SEC-07). Rows are written by the domain (inside the transaction of what they record) and by the API for
// administrative requests; the database makes them append-only and chains them by hash.

type AuditEntry struct {
	ID     int64           `json:"id"`
	Seq    *int64          `json:"seq,omitempty"`
	Actor  string          `json:"actor"`
	ActorI string          `json:"actor_id,omitempty"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail"`
	At     time.Time       `json:"at"`
	Hash   string          `json:"hash,omitempty"`
}

type AuditQuery struct {
	Actor  string // a name or id fragment
	Action string // prefix, such as "workflow." or an exact action
	Target string
	From   time.Time
	To     time.Time
	Limit  int
	Before int64 // paging: entries with an id below this
}

// AuditAPI records an administrative request that succeeded. detail carries what the route knows (status, address).
func (s *Service) AuditAPI(ctx context.Context, a Actor, action, target string, detail map[string]any) error {
	b, _ := json.Marshal(detail)
	_, err := s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,$3,$4,$5)`, nilIfEmpty(a.ID), a.Label, action, target, b)
	return err
}

func (s *Service) QueryAudit(ctx context.Context, a Actor, q AuditQuery) ([]AuditEntry, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}
	where, args := []string{"true"}, []any{}
	arg := func(v any) string { args = append(args, v); return "$" + strconv.Itoa(len(args)) }
	if q.Actor != "" {
		where = append(where, "(actor_label ILIKE '%'||"+arg(q.Actor)+"||'%' OR actor_id::text = "+arg(q.Actor)+")")
	}
	if q.Action != "" {
		where = append(where, "(action = "+arg(q.Action)+" OR action LIKE "+arg(escapeLike(q.Action)+"%")+")")
	}
	if q.Target != "" {
		where = append(where, "target = "+arg(q.Target))
	}
	if !q.From.IsZero() {
		where = append(where, "at >= "+arg(q.From))
	}
	if !q.To.IsZero() {
		where = append(where, "at < "+arg(q.To))
	}
	if q.Before > 0 {
		where = append(where, "id < "+arg(q.Before))
	}
	rows, err := s.DB.Query(ctx, `SELECT id,seq,COALESCE(actor_id::text,''),actor_label,action,target,detail,at,hash FROM audit_event WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT `+strconv.Itoa(q.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Seq, &e.ActorI, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.At, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

type AuditCheck struct {
	OK        bool   `json:"ok"`
	Entries   int64  `json:"entries"`   // entries in the chain
	Unchained int64  `json:"unchained"` // older entries written before the chain existed
	BrokenAt  *int64 `json:"broken_at,omitempty"`
	Problem   string `json:"problem,omitempty"`
}

// VerifyAudit walks the chain: every row must hash to what it says, point at the row before it, and follow it without a gap.
func (s *Service) VerifyAudit(ctx context.Context, a Actor) (AuditCheck, error) {
	var c AuditCheck
	if err := adminOnly(a); err != nil {
		return c, err
	}
	s.DB.QueryRow(ctx, `SELECT count(*) FILTER (WHERE seq IS NOT NULL), count(*) FILTER (WHERE seq IS NULL) FROM audit_event`).Scan(&c.Entries, &c.Unchained)
	var seq int64
	var problem string
	err := s.DB.QueryRow(ctx, `
		SELECT seq, CASE WHEN seq <> COALESCE(pseq, 0) + 1 THEN 'an entry is missing before this one'
		                 WHEN prev_hash <> COALESCE(phash, '') THEN 'this entry does not follow the one before it'
		                 ELSE 'this entry was changed' END
		FROM (SELECT seq, prev_hash, hash, lag(seq) OVER w AS pseq, lag(hash) OVER w AS phash,
		             audit_row_hash(prev_hash, id, actor_id, actor_label, action, target, detail, at) AS want
		      FROM audit_event WHERE seq IS NOT NULL WINDOW w AS (ORDER BY seq)) x
		WHERE seq <> COALESCE(pseq, 0) + 1 OR prev_hash <> COALESCE(phash, '') OR hash <> want ORDER BY seq LIMIT 1`).Scan(&seq, &problem)
	switch {
	case err == nil:
		c.BrokenAt, c.Problem = &seq, problem
		return c, nil
	case errors.Is(err, pgx.ErrNoRows):
		c.OK = true
		return c, nil
	}
	return c, err
}

// ExportAudit writes the matching entries as CSV (administrators). The export is itself recorded.
func (s *Service) ExportAudit(ctx context.Context, a Actor, w io.Writer, q AuditQuery) (int, error) {
	q.Limit = 1000
	var all []AuditEntry
	for {
		page, err := s.QueryAudit(ctx, a, q)
		if err != nil {
			return 0, err
		}
		all = append(all, page...)
		if len(page) < q.Limit || len(all) >= 100000 {
			break
		}
		q.Before = page[len(page)-1].ID
	}
	cw := csv.NewWriter(w)
	cw.Write([]string{"seq", "id", "at", "actor", "actor_id", "action", "target", "detail", "hash"})
	for _, e := range all {
		seq := ""
		if e.Seq != nil {
			seq = fmt.Sprint(*e.Seq)
		}
		cw.Write([]string{seq, fmt.Sprint(e.ID), e.At.UTC().Format(time.RFC3339Nano), csvSafe(e.Actor), e.ActorI, csvSafe(e.Action), csvSafe(e.Target), csvSafe(string(e.Detail)), e.Hash})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return 0, err
	}
	if err := s.AuditAPI(ctx, a, "audit.export", "", map[string]any{"entries": len(all)}); err != nil {
		return len(all), err
	}
	return len(all), nil
}
