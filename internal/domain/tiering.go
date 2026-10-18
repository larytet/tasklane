package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/blob"
)

// Tiering (BLOB-15): attachments move from the hot tier to the cold one by rule (class, age, size), and reads stay transparent.
// A rule matches an object when every criterion it sets holds; an object is moved when any enabled rule matches. The move
// copies the object, reads the copy back and compares it, and only then removes the hot copy (see blob.Tiered), so a failure
// at any point leaves the object where it was.

type TierRule struct {
	ID           int64   `json:"id,omitempty"`
	Name         string  `json:"name"`
	Class        *string `json:"class"`
	MinAgeDays   int     `json:"min_age_days"`
	MinSizeBytes int64   `json:"min_size_bytes"`
	Enabled      bool    `json:"enabled"`
}

var tierClasses = map[string]bool{"image": true, "video": true, "log": true, "document": true, "archive": true}

func (s *Service) TierRules(ctx context.Context, a Actor) ([]TierRule, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	return s.tierRules(ctx, false)
}

func (s *Service) tierRules(ctx context.Context, onlyEnabled bool) ([]TierRule, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,name,class,min_age_days,min_size_bytes,enabled FROM tier_rule WHERE ($1 OR enabled) ORDER BY id`, !onlyEnabled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TierRule{}
	for rows.Next() {
		var r TierRule
		if err := rows.Scan(&r.ID, &r.Name, &r.Class, &r.MinAgeDays, &r.MinSizeBytes, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetTierRules replaces the rules. A rule that sets no criterion would move everything, so it is refused.
func (s *Service) SetTierRules(ctx context.Context, a Actor, rules []TierRule) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	if len(rules) > 50 {
		return invalid("at most 50 rules")
	}
	for i, r := range rules {
		r.Name = strings.TrimSpace(r.Name)
		switch {
		case r.Name == "" || len(r.Name) > 80:
			return invalid("rule %d needs a name of up to 80 characters", i+1)
		case r.Class != nil && !tierClasses[*r.Class]:
			return invalid("rule %q: the class is image, video, log, document or archive", r.Name)
		case r.MinAgeDays < 0 || r.MinAgeDays > 36500 || r.MinSizeBytes < 0:
			return invalid("rule %q: age and size are not negative", r.Name)
		case r.Class == nil && r.MinAgeDays == 0 && r.MinSizeBytes == 0:
			return invalid("rule %q sets no class, age or size, so it would move everything", r.Name)
		}
		rules[i] = r
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM tier_rule`); err != nil {
			return err
		}
		for _, r := range rules {
			if _, err := tx.Exec(ctx, `INSERT INTO tier_rule(name,class,min_age_days,min_size_bytes,enabled) VALUES($1,$2,$3,$4,$5)`, r.Name, r.Class, r.MinAgeDays, r.MinSizeBytes, r.Enabled); err != nil {
				return err
			}
		}
		return s.audit(ctx, tx, a, "blob.tier_rules", "", map[string]any{"rules": len(rules)})
	})
}

type TierRun struct {
	At     time.Time `json:"at"`
	Moved  int       `json:"moved"`
	Failed int       `json:"failed"`
	Bytes  int64     `json:"bytes"`
	Error  string    `json:"error,omitempty"`
}

type TierUse struct {
	Tier    string `json:"tier"`
	Objects int64  `json:"objects"`
	Bytes   int64  `json:"bytes"`
}

type TierStatus struct {
	Configured bool      `json:"configured"`
	Use        []TierUse `json:"use"`
	LastRun    *TierRun  `json:"last_run,omitempty"`
	HotReads   int64     `json:"hot_reads"`
	ColdReads  int64     `json:"cold_reads"`
	HotAvgMs   float64   `json:"hot_open_avg_ms"`
	ColdAvgMs  float64   `json:"cold_open_avg_ms"`
}

func (s *Service) TierStatus(ctx context.Context, a Actor) (TierStatus, error) {
	if err := adminOnly(a); err != nil {
		return TierStatus{}, err
	}
	st := TierStatus{Use: []TierUse{}}
	if s.Blobs != nil {
		if t := blob.AsTiered(s.Blobs.Store); t != nil {
			st.Configured = true
			x := t.Stats()
			st.HotReads, st.ColdReads = x.HotReads, x.ColdReads
			st.HotAvgMs, st.ColdAvgMs = float64(x.HotAvg)/1e6, float64(x.ColdAvg)/1e6
		}
	}
	rows, err := s.DB.Query(ctx, `SELECT tier,count(*),COALESCE(sum(size),0) FROM blob GROUP BY tier ORDER BY tier`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var u TierUse
		rows.Scan(&u.Tier, &u.Objects, &u.Bytes)
		st.Use = append(st.Use, u)
	}
	rows.Close()
	var r TierRun
	if s.DB.QueryRow(ctx, `SELECT at,moved,failed,bytes,error FROM tier_run ORDER BY id DESC LIMIT 1`).Scan(&r.At, &r.Moved, &r.Failed, &r.Bytes, &r.Error) == nil {
		st.LastRun = &r
	}
	return st, nil
}

// RunTiering moves up to limit objects that match a rule from the hot tier to the cold one. Without a cold tier configured, or
// without rules, it does nothing. Several nodes may run it: an object is claimed by a row lock, and one already claimed is skipped.
func (s *Service) RunTiering(ctx context.Context, limit int) (TierRun, error) {
	run := TierRun{At: s.Now()}
	if s.Blobs == nil {
		return run, nil
	}
	t := blob.AsTiered(s.Blobs.Store)
	if t == nil {
		return run, nil
	}
	rules, err := s.tierRules(ctx, true)
	if err != nil || len(rules) == 0 {
		return run, err
	}
	if limit <= 0 {
		limit = 200
	}
	var cond []string
	var args []any
	args = append(args, s.Now())
	for _, r := range rules {
		c := []string{"TRUE"}
		if r.Class != nil {
			args = append(args, *r.Class)
			c = append(c, fmt.Sprintf("b.class=$%d", len(args)))
		}
		if r.MinAgeDays > 0 {
			args = append(args, r.MinAgeDays)
			c = append(c, fmt.Sprintf("b.created_at <= $1::timestamptz - make_interval(days => $%d)", len(args)))
		}
		if r.MinSizeBytes > 0 {
			args = append(args, r.MinSizeBytes)
			c = append(c, fmt.Sprintf("b.size >= $%d", len(args)))
		}
		cond = append(cond, "("+strings.Join(c, " AND ")+")")
	}
	args = append(args, limit)
	// referenced and clean objects only: an unreferenced one is the garbage collector's, and one still scanning may be rejected
	rows, err := s.DB.Query(ctx, `SELECT b.sha256 FROM blob b WHERE b.tier='hot' AND b.refcount>0 AND b.scan_state='clean' AND (`+strings.Join(cond, " OR ")+`)
		ORDER BY b.created_at LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return run, err
	}
	var shas []string
	for rows.Next() {
		var sha string
		rows.Scan(&sha)
		shas = append(shas, sha)
	}
	rows.Close()
	var lastErr error
	for _, sha := range shas {
		if ctx.Err() != nil {
			break
		}
		moved, size, err := s.tierOne(ctx, t, sha)
		switch {
		case err != nil:
			run.Failed++
			lastErr = fmt.Errorf("%s: %w", sha[:12], err)
		case moved:
			run.Moved++
			run.Bytes += size
		}
	}
	if lastErr != nil {
		run.Error = lastErr.Error()
	}
	s.DB.Exec(ctx, `INSERT INTO tier_run(at,moved,failed,bytes,error) VALUES($1,$2,$3,$4,$5)`, run.At, run.Moved, run.Failed, run.Bytes, run.Error)
	s.DB.Exec(ctx, `DELETE FROM tier_run WHERE id < (SELECT COALESCE(max(id),0)-100 FROM tier_run)`)
	return run, nil
}

// tierOne claims one object (a row lock another node's mover skips), moves it and records where it is.
func (s *Service) tierOne(ctx context.Context, t *blob.Tiered, sha string) (moved bool, size int64, err error) {
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id::text,size FROM blob WHERE sha256=$1 AND tier='hot' AND refcount>0 FOR UPDATE SKIP LOCKED`, sha).Scan(&id, &size); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // gone, already moved, or being moved by another node
			}
			return err
		}
		if err := t.MoveCold(sha); err != nil {
			if errors.Is(err, blob.ErrNotFound) {
				return nil
			}
			return err
		}
		moved = true
		_, err := tx.Exec(ctx, `UPDATE blob SET tier='cold', tiered_at=$2 WHERE id::text=$1`, id, s.Now())
		return err
	})
	return moved, size, err
}

// RunTierWorker applies the rules every interval until the context ends.
func (s *Service) RunTierWorker(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		every = 10 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if r, err := s.RunTiering(ctx, 200); err != nil && ctx.Err() == nil {
			logf("tiering: %v", err)
		} else if r.Moved+r.Failed > 0 {
			logf("tiering: moved %d object(s), %d failed %s", r.Moved, r.Failed, r.Error)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
