package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Retention of mail (SPEC 17.3, MAIL-18). Three things are kept for a limited time:
//   - the original of an inbound message, its text, and who it was from: MailRawDays after it arrived (default 90; 0 keeps them);
//   - bounce records and cleared suppressions: MailBounceDays (default 365; 0 keeps them);
//   - an address that is still suppressed is never removed by age, because forgetting it would start mailing a dead address.
//
// A message still waiting for an administrator's decision is not purged (the decision needs the text), and neither is mail
// attached to an issue under legal hold. A purged row stays, without personal data, so a redelivery of the same message is still
// recognised as a duplicate.

const (
	defaultMailRawDays    = 90
	defaultMailBounceDays = 365
)

type MailRetentionResult struct {
	Messages    int `json:"messages_purged"`
	Blobs       int `json:"originals_removed"`
	Bounces     int `json:"bounce_records_removed"`
	Suppression int `json:"suppressions_removed"`
}

func (s *Service) mailRawDays() int {
	if s.MailRawDays < 0 {
		return 0
	}
	if s.MailRawDays == 0 && !s.MailRetentionSet {
		return defaultMailRawDays
	}
	return s.MailRawDays
}

func (s *Service) mailBounceDays() int {
	if s.MailBounceDays < 0 {
		return 0
	}
	if s.MailBounceDays == 0 && !s.MailRetentionSet {
		return defaultMailBounceDays
	}
	return s.MailBounceDays
}

// forgetMessageID replaces a Message-ID by a hash of it: duplicates are still recognised, but the id (which names a host and often
// a person's machine) is gone.
func forgetMessageID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "purged:" + hex.EncodeToString(sum[:16])
}

// purgeInbound removes the personal content of the given inbound rows inside tx and returns the hashes of the originals it let go.
// A hash's blob row is released here; the bytes are removed by dropOriginals after the transaction commits.
func (s *Service) purgeInbound(ctx context.Context, tx pgx.Tx, where string, args ...any) (rows int, shas []string, err error) {
	rs, err := tx.Query(ctx, `SELECT id,raw_sha256 FROM mail_inbound WHERE purged_at IS NULL AND `+where, args...)
	if err != nil {
		return 0, nil, err
	}
	var ids []int64
	for rs.Next() {
		var id int64
		var sha *string
		if err := rs.Scan(&id, &sha); err != nil {
			rs.Close()
			return 0, nil, err
		}
		ids = append(ids, id)
		if sha != nil {
			shas = append(shas, *sha)
		}
	}
	rs.Close()
	if rs.Err() != nil {
		return 0, nil, rs.Err()
	}
	now := s.Now()
	for _, id := range ids {
		var mid string
		if err := tx.QueryRow(ctx, `SELECT message_id FROM mail_inbound WHERE id=$1`, id).Scan(&mid); err != nil {
			return 0, nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE mail_inbound SET raw_sha256=NULL, sender='', subject='', body='', detail='{}', message_id=$2, user_id=NULL, purged_at=$3 WHERE id=$1`, id, forgetMessageID(mid), now); err != nil {
			return 0, nil, err
		}
	}
	for _, sha := range shas { // let go of the reference; zero_since far in the past means the next sweep removes it at once
		if _, err := tx.Exec(ctx, `UPDATE blob SET refcount=refcount-1, zero_since=CASE WHEN refcount-1<=0 THEN 'epoch'::timestamptz ELSE zero_since END WHERE sha256=$1`, sha); err != nil {
			return 0, nil, err
		}
	}
	return len(ids), shas, nil
}

// dropOriginals removes the bytes of originals nobody references any more, now and not after the usual retention window.
func (s *Service) dropOriginals(ctx context.Context) int {
	if s.Blobs == nil {
		return 0
	}
	n, _ := s.GC(ctx)
	return n
}

// RunMailRetention applies the retention periods. It is safe to repeat and runs from the worker and on demand.
func (s *Service) RunMailRetention(ctx context.Context) (MailRetentionResult, error) {
	var res MailRetentionResult
	if days := s.mailRawDays(); days > 0 {
		cut := s.Now().AddDate(0, 0, -days)
		err := s.tx(ctx, func(tx pgx.Tx) error {
			n, _, err := s.purgeInbound(ctx, tx, `received_at<$1 AND outcome NOT IN ('pending','quarantined')
				AND (issue_id IS NULL OR NOT EXISTS (SELECT 1 FROM issue i WHERE i.id=mail_inbound.issue_id AND i.legal_hold))`, cut)
			res.Messages = n
			return err
		})
		if err != nil {
			return res, err
		}
		if res.Messages > 0 {
			res.Blobs = s.dropOriginals(ctx)
		}
	}
	if days := s.mailBounceDays(); days > 0 {
		cut := s.Now().AddDate(0, 0, -days)
		tag, err := s.DB.Exec(ctx, `DELETE FROM mail_bounce WHERE at<$1 AND email NOT IN (SELECT email FROM mail_suppression WHERE cleared_at IS NULL)`, cut)
		if err != nil {
			return res, err
		}
		res.Bounces = int(tag.RowsAffected())
		tag, err = s.DB.Exec(ctx, `DELETE FROM mail_suppression WHERE cleared_at IS NOT NULL AND cleared_at<$1`, cut)
		if err != nil {
			return res, err
		}
		res.Suppression = int(tag.RowsAffected())
	}
	if res.Messages+res.Bounces+res.Suppression > 0 { // the system acts: audited with counts only, never with the content
		_, err := s.DB.Exec(ctx, `INSERT INTO audit_event(actor_label,action,target,detail) VALUES('system','mail.retention','mail',$1)`, mustJSON(res))
		return res, err
	}
	return res, nil
}

// RunMailRetentionNow is the administrator's "apply now".
func (s *Service) RunMailRetentionNow(ctx context.Context, a Actor) (MailRetentionResult, error) {
	if !a.Admin {
		return MailRetentionResult{}, ErrForbidden
	}
	return s.RunMailRetention(ctx)
}

// RunMailRetentionWorker applies retention every interval until the context ends.
func (s *Service) RunMailRetentionWorker(ctx context.Context, every time.Duration, logf func(string, ...any)) {
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if !s.GateOn(ctx, MailRetentionG) { // an older node would not recognise the hashed Message-IDs a purge leaves
			// not yet: wait until every live node qualifies
		} else if r, err := s.RunMailRetention(ctx); err != nil && ctx.Err() == nil {
			logf("mail retention: %v", err)
		} else if r.Messages+r.Bounces+r.Suppression > 0 {
			logf("mail retention: %d message(s), %d original(s), %d bounce record(s), %d suppression(s) removed", r.Messages, r.Blobs, r.Bounces, r.Suppression)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// erasePersonMail removes what the mail system holds about a person's address: the messages they sent us (and those matched to
// them), their bounce records, and their suppression entry.
func (s *Service) erasePersonMail(ctx context.Context, tx pgx.Tx, userID, email string, counts map[string]int) (bool, error) {
	email = strings.ToLower(email)
	n, _, err := s.purgeInbound(ctx, tx, `(user_id::text=$1 OR lower(sender)=$2)`, userID, email)
	if err != nil {
		return false, err
	}
	if n > 0 {
		counts["inbound_mail_purged"] = n
	}
	for _, q := range []struct{ count, sql string }{
		{"bounce_records_removed", `DELETE FROM mail_bounce WHERE lower(email)=$1`},
		{"suppressions_removed", `DELETE FROM mail_suppression WHERE lower(email)=$1`},
	} {
		tag, err := tx.Exec(ctx, q.sql, email)
		if err != nil {
			return false, err
		}
		if c := int(tag.RowsAffected()); c > 0 {
			counts[q.count] += c
		}
	}
	return n > 0, nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
