package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Personal data (SEC-10): an administrator can export everything the system holds about a person, and erase them. Erasing
// replaces the person with "Deleted user" everywhere they are named, removes their credentials, sessions, notifications and
// mail log, can delete what they wrote, scrubs their name and address from other people's text, redacts the audit log
// (re-chaining it so it still verifies) and has the derived search data rebuilt. Issues are never deleted. A legal hold
// stops an erase that would touch held records.

const erasedName = "Deleted user"

type EraseOpts struct {
	DeleteContent bool `json:"delete_content"` // also delete the comments and attachments the person wrote
	ScrubMentions bool `json:"scrub_mentions"` // replace their name and address in other people's comments, descriptions and summaries
}

type EraseResult struct {
	Erased bool           `json:"erased"` // false when the person was already erased
	Counts map[string]int `json:"counts"`
}

// ExportPersonalData returns what is held about a person, grouped by kind. Only administrators; the export is audited.
func (s *Service) ExportPersonalData(ctx context.Context, a Actor, userID string) (map[string]any, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	out := map[string]any{}
	var email, name, kind, source string
	var created time.Time
	var active, admin bool
	var ext *string
	if err := s.DB.QueryRow(ctx, `SELECT email,name,kind,source,created_at,active,is_admin,external_id FROM app_user WHERE id::text=$1`, userID).Scan(&email, &name, &kind, &source, &created, &active, &admin, &ext); err != nil {
		return nil, ErrNotFound
	}
	out["profile"] = map[string]any{"id": userID, "email": email, "name": name, "kind": kind, "source": source, "created": created, "active": active, "administrator": admin, "external_id": ext}
	list := func(key, q string, cols int) error {
		rows, err := s.DB.Query(ctx, q, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		items := []map[string]any{}
		names := rows.FieldDescriptions()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return err
			}
			m := map[string]any{}
			for i, v := range vals {
				m[string(names[i].Name)] = v
			}
			items = append(items, m)
		}
		out[key] = items
		return rows.Err()
	}
	for _, q := range []struct{ key, sql string }{
		{"projects", `SELECT p.key AS project, m.role FROM project_member m JOIN project p ON p.id=m.project_id WHERE m.user_id::text=$1`},
		{"issues_reported", `SELECT key, summary, created_at FROM issue WHERE reporter_id::text=$1 ORDER BY number`},
		{"issues_assigned", `SELECT key, summary FROM issue WHERE assignee_id::text=$1 ORDER BY number`},
		{"comments", `SELECT i.key AS issue, c.body, c.internal, c.created_at FROM comment c JOIN issue i ON i.id=c.issue_id WHERE c.author_id::text=$1 ORDER BY c.created_at`},
		{"attachments", `SELECT i.key AS issue, a.name, a.created_at FROM attachment a JOIN issue i ON i.id=a.issue_id WHERE a.uploader_id::text=$1 ORDER BY a.created_at`},
		{"history", `SELECT i.key AS issue, h.field, h.before, h.after, h.at FROM issue_history h JOIN issue i ON i.id=h.issue_id WHERE h.actor_id::text=$1 ORDER BY h.at`},
		{"notifications", `SELECT i.key AS issue, n.kind, n.what, n.state, n.created_at FROM notification n LEFT JOIN issue i ON i.id=n.issue_id WHERE n.user_id::text=$1 ORDER BY n.id`},
		{"watching", `SELECT i.key AS issue, w.muted FROM issue_watch w JOIN issue i ON i.id=w.issue_id WHERE w.user_id::text=$1`},
		{"sessions", `SELECT created_at, last_seen_at, ip, user_agent FROM session WHERE user_id::text=$1 ORDER BY id`},
		{"passkeys", `SELECT name, created_at, last_used_at FROM webauthn_credential WHERE user_id::text=$1 ORDER BY id`},
		{"api_tokens", `SELECT name, created_at, expires_at, revoked_at, last_used_at FROM api_token WHERE user_id::text=$1`},
		{"connected_apps", `SELECT c.name AS client, g.scopes, g.created_at, g.revoked_at FROM oauth_grant g JOIN oauth_client c ON c.id=g.client_id WHERE g.user_id::text=$1`},
		{"inbound_mail", `SELECT m.kind, m.subject, m.received_at, m.outcome, m.reason FROM mail_inbound m WHERE m.purged_at IS NULL AND (m.user_id::text=$1 OR lower(m.sender)=(SELECT lower(email) FROM app_user WHERE id::text=$1)) ORDER BY m.id`},
		{"bounce_records", `SELECT b.kind, b.status, b.source, b.at FROM mail_bounce b WHERE lower(b.email)=(SELECT lower(email) FROM app_user WHERE id::text=$1) ORDER BY b.id`},
		{"mail_sent_to_them", `SELECT subject, kind, status, created_at FROM mail_message WHERE recipient_id::text=$1 ORDER BY id`},
		{"git_accounts", `SELECT provider, login, created_at FROM git_identity WHERE user_id::text=$1 ORDER BY id`},
		{"git_activity", `SELECT o.kind, r.full_name AS repo, o.title, o.state, o.provider_at FROM git_object o JOIN git_repo r ON r.id=o.repo_id WHERE o.author_user::text=$1 ORDER BY o.provider_at`},
		{"git_commands", `SELECT issue_key, command, outcome, reason, at FROM git_smart_run WHERE user_id::text=$1 ORDER BY id`},
		{"audit", `SELECT action, target, detail, at FROM audit_event WHERE actor_id::text=$1 ORDER BY id`},
	} {
		if err := list(q.key, q.sql, 0); err != nil {
			return nil, fmt.Errorf("%s: %w", q.key, err)
		}
	}
	var prefs []byte
	if s.DB.QueryRow(ctx, `SELECT prefs FROM notification_pref WHERE user_id::text=$1`, userID).Scan(&prefs) == nil {
		out["notification_preferences"] = string(prefs)
	}
	var ui []byte
	if s.DB.QueryRow(ctx, `SELECT settings FROM user_setting WHERE user_id::text=$1`, userID).Scan(&ui) == nil {
		out["ui_settings"] = string(ui)
	}
	var mfa bool
	s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_mfa WHERE user_id::text=$1 AND enabled)`, userID).Scan(&mfa)
	out["two_factor_enabled"] = mfa
	if _, err := s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,'privacy.export',$3,'{}')`, nilIfEmpty(a.ID), a.Label, userID); err != nil {
		return nil, err
	}
	return out, nil
}

// ErasePerson removes a person's identity from the system. It refuses to touch records under legal hold, the last
// administrator, and the caller's own account. It can be repeated: a person already erased stays erased.
func (s *Service) ErasePerson(ctx context.Context, a Actor, userID string, o EraseOpts) (EraseResult, error) {
	res := EraseResult{Counts: map[string]int{}}
	if err := adminOnly(a); err != nil {
		return res, err
	}
	if a.ID == userID {
		return res, invalid("ask another administrator to erase your account")
	}
	var email, name string
	var admin, active bool
	if err := s.DB.QueryRow(ctx, `SELECT email,name,is_admin,active FROM app_user WHERE id::text=$1`, userID).Scan(&email, &name, &admin, &active); err != nil {
		return res, ErrNotFound
	}
	if strings.HasSuffix(email, "@erased.invalid") {
		return res, nil // already done
	}
	if admin && active {
		var others int
		s.DB.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE is_admin AND active AND id::text<>$1`, userID).Scan(&others)
		if others == 0 {
			return res, &ConflictError{"the last administrator cannot be erased"}
		}
	}
	// legal hold: records that must be kept keep their authors
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT i.key FROM issue i WHERE i.legal_hold AND (i.reporter_id::text=$1 OR i.assignee_id::text=$1
		OR EXISTS (SELECT 1 FROM comment c WHERE c.issue_id=i.id AND c.author_id::text=$1)
		OR EXISTS (SELECT 1 FROM attachment at WHERE at.issue_id=i.id AND at.uploader_id::text=$1)
		OR EXISTS (SELECT 1 FROM issue_history h WHERE h.issue_id=i.id AND h.actor_id::text=$1)
		OR EXISTS (SELECT 1 FROM mail_inbound m WHERE m.issue_id=i.id AND m.purged_at IS NULL AND (m.user_id::text=$1 OR lower(m.sender)=lower($2)))) ORDER BY 1`, userID, email)
	if err != nil {
		return res, err
	}
	var held []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		held = append(held, k)
	}
	rows.Close()
	if len(held) > 0 {
		return res, &ConflictError{"records under legal hold involve this person (" + strings.Join(held, ", ") + "); release the hold first"}
	}
	short := userID
	if len(short) > 8 {
		short = short[:8]
	}
	newEmail := "erased-" + short + "@erased.invalid"
	needSweep := false           // originals of mail from this person were let go: their bytes are removed after the commit
	touched := map[string]bool{} // issues whose text or participants changed: their derived data is rebuilt
	err = s.tx(ctx, func(tx pgx.Tx) error {
		mark := func(q string, args ...any) error {
			rs, err := tx.Query(ctx, q, args...)
			if err != nil {
				return err
			}
			defer rs.Close()
			for rs.Next() {
				var k string
				rs.Scan(&k)
				touched[k] = true
			}
			return rs.Err()
		}
		if err := mark(`SELECT key FROM issue WHERE reporter_id::text=$1 OR assignee_id::text=$1 UNION SELECT i.key FROM comment c JOIN issue i ON i.id=c.issue_id WHERE c.author_id::text=$1`, userID); err != nil {
			return err
		}
		exec := func(count, q string, args ...any) error {
			tag, err := tx.Exec(ctx, q, args...)
			if err != nil {
				return err
			}
			if n := int(tag.RowsAffected()); n > 0 {
				res.Counts[count] += n
			}
			return nil
		}
		if o.DeleteContent {
			if err := exec("comments_deleted", `DELETE FROM comment WHERE author_id::text=$1`, userID); err != nil {
				return err
			}
			// each attachment's file loses a reference; the blob sweep removes files nobody uses
			if err := exec("attachments_deleted", `WITH d AS (DELETE FROM attachment WHERE uploader_id::text=$1 RETURNING blob_id)
				UPDATE blob SET refcount=refcount-1, zero_since=CASE WHEN refcount-1<=0 THEN $2::timestamptz ELSE zero_since END WHERE id IN (SELECT blob_id FROM d)`, userID, s.Now()); err != nil {
				return err
			}
		}
		if o.ScrubMentions {
			local := email
			if i := strings.Index(email, "@"); i > 0 {
				local = email[:i]
			}
			type needle struct{ plain, pattern string }
			needles := []needle{{email, regexpQuote(email)}, {name, regexpQuote(name)}}
			if len(local) >= 2 { // the @name that mentions a person
				needles = append(needles, needle{"@" + local, "@" + regexpQuote(local) + `(?![A-Za-z0-9._-])`})
			}
			for _, nd := range needles {
				if len(nd.plain) < 3 {
					continue
				}
				if err := mark(`SELECT i.key FROM issue_text t JOIN issue i ON i.id=t.issue_id WHERE position(lower($1) in lower(t.body))>0
					UNION SELECT i.key FROM comment c JOIN issue i ON i.id=c.issue_id WHERE position(lower($1) in lower(c.body))>0
					UNION SELECT key FROM issue WHERE position(lower($1) in lower(summary))>0`, nd.plain); err != nil {
					return err
				}
				for _, q := range []struct{ count, sql string }{
					{"texts_scrubbed", `UPDATE issue_text SET body=regexp_replace(body, $2, '[deleted user]', 'gi') WHERE position(lower($1) in lower(body))>0`},
					{"texts_scrubbed", `UPDATE text_version SET body=regexp_replace(body, $2, '[deleted user]', 'gi') WHERE position(lower($1) in lower(body))>0`},
					{"comments_scrubbed", `UPDATE comment SET body=regexp_replace(body, $2, '[deleted user]', 'gi') WHERE position(lower($1) in lower(body))>0`},
					{"summaries_scrubbed", `UPDATE issue SET summary=regexp_replace(summary, $2, '[deleted user]', 'gi') WHERE position(lower($1) in lower(summary))>0`},
					{"history_scrubbed", `UPDATE issue_history SET before=regexp_replace(before, $2, '[deleted user]', 'gi'), after=regexp_replace(after, $2, '[deleted user]', 'gi') WHERE position(lower($1) in lower(COALESCE(before,'')||' '||COALESCE(after,'')))>0`},
					{"notifications_scrubbed", `UPDATE notification SET what=regexp_replace(what, $2, '[deleted user]', 'gi'), body=regexp_replace(body, $2, '[deleted user]', 'gi') WHERE position(lower($1) in lower(what||' '||body))>0`},
					{"events_scrubbed", `UPDATE outbox SET payload=regexp_replace(payload::text, $2, 'Deleted user', 'gi')::jsonb WHERE position(lower($1) in lower(payload::text))>0`},
					{"git_titles_scrubbed", `UPDATE git_object SET title=regexp_replace(title, $2, '[deleted user]', 'gi') WHERE position(lower($1) in lower(title))>0`},
				} {
					if err := exec(q.count, q.sql, nd.plain, nd.pattern); err != nil {
						return err
					}
				}
			}
		}
		for _, q := range []struct{ count, sql string }{
			{"history_relabelled", `UPDATE issue_history SET actor_label=$2 WHERE actor_id::text=$1`},
			{"events_relabelled", `UPDATE outbox SET payload=jsonb_set(payload,'{actor}',to_jsonb($2::text)) WHERE payload->>'actor_id'=$1 AND payload ? 'actor'`},
			{"versions_relabelled", `UPDATE text_version SET actor_label=$2 WHERE actor_id::text=$1`},
		} {
			if err := exec(q.count, q.sql, userID, erasedName); err != nil {
				return err
			}
		}
		// what the Git integration kept about them: the author on code objects (by the person, or by an account linked to them), the
		// linked accounts, and who ran which command
		if err := exec("git_authors_erased", `UPDATE git_object o SET author=$2, author_user=NULL WHERE o.author_user::text=$1 OR EXISTS (SELECT 1 FROM git_identity g JOIN git_repo r ON r.id=o.repo_id JOIN git_connection c ON c.id=r.connection_id
			WHERE g.user_id::text=$1 AND g.provider=c.provider AND lower(o.author)=g.login)`, userID, erasedName); err != nil {
			return err
		}
		for _, q := range []struct{ count, sql string }{
			{"git_accounts_unlinked", `DELETE FROM git_identity WHERE user_id::text=$1`},
			{"git_commands_detached", `UPDATE git_smart_run SET user_id=NULL WHERE user_id::text=$1`},
			{"git_writes_detached", `UPDATE git_write_log SET user_id=NULL WHERE user_id::text=$1`},
		} {
			if err := exec(q.count, q.sql, userID); err != nil {
				return err
			}
		}
		for _, q := range []struct{ count, sql string }{
			{"sessions_ended", `DELETE FROM session WHERE user_id::text=$1`},
			{"tokens_removed", `DELETE FROM api_token WHERE user_id::text=$1`},
			{"grants_removed", `DELETE FROM oauth_grant WHERE user_id::text=$1`},
			{"identities_removed", `DELETE FROM sso_identity WHERE user_id::text=$1`},
			{"notifications_removed", `DELETE FROM notification WHERE user_id::text=$1`},
			{"watches_removed", `DELETE FROM issue_watch WHERE user_id::text=$1`},
			{"mail_removed", `DELETE FROM mail_message WHERE recipient_id::text=$1`},
			{"memberships_removed", `DELETE FROM project_member WHERE user_id::text=$1`},
			{"two_factor_removed", `DELETE FROM user_mfa WHERE user_id::text=$1`},
			{"passkeys_removed", `DELETE FROM webauthn_credential WHERE user_id::text=$1`},
			{"passkey_challenges_removed", `DELETE FROM webauthn_challenge WHERE user_id::text=$1`},
			{"pending_sign_ins_removed", `DELETE FROM mfa_pending WHERE user_id::text=$1`},
			{"preferences_removed", `DELETE FROM notification_pref WHERE user_id::text=$1`},
			{"settings_removed", `DELETE FROM user_setting WHERE user_id::text=$1`},
			{"undo_tokens_removed", `DELETE FROM undo_token WHERE user_id::text=$1`},
			{"idempotency_keys_removed", `DELETE FROM idempotency_key WHERE user_id::text=$1`},
		} {
			if err := exec(q.count, q.sql, userID); err != nil {
				return err
			}
		}
		if err := exec("notices_relabelled", `UPDATE notification SET actor_label=$2 WHERE actor_label=$1`, name, erasedName); err != nil {
			return err
		}
		if o.DeleteContent { // a deleted file's name is content too
			if err := exec("history_cleared", `UPDATE issue_history SET before=CASE WHEN before IS NULL THEN NULL ELSE '[deleted]' END, after=CASE WHEN after IS NULL THEN NULL ELSE '[deleted]' END WHERE actor_id::text=$1 AND field='attachment'`, userID); err != nil {
				return err
			}
		}
		if purged, err := s.erasePersonMail(ctx, tx, userID, email, res.Counts); err != nil {
			return err
		} else if purged {
			needSweep = true
		}
		if err := exec("sign_in_records_removed", `DELETE FROM login_attempt WHERE email=$1`, strings.ToLower(email)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE app_user SET email=$2,name=$3,password_hash=NULL,active=false,external_id=NULL,is_admin=false,appear_offline=true,password_changed_at=NULL WHERE id::text=$1`, userID, newEmail, erasedName); err != nil {
			return err
		}
		var redacted int
		if err := tx.QueryRow(ctx, `SELECT audit_redact_actor($1::uuid)`, userID).Scan(&redacted); err != nil {
			return err
		}
		res.Counts["audit_entries_redacted"] = redacted
		for k := range touched { // rebuild what is derived from this text: the search index follows these events
			if err := outbox(ctx, tx, "issue:"+k, "issue.updated", map[string]any{"key": k, "fields": []string{"text"}, "actor": "privacy", "reason": "erase"}); err != nil {
				return err
			}
		}
		res.Counts["issues_reindexed"] = len(touched)
		return s.audit(ctx, tx, a, "privacy.erased", userID, map[string]any{"delete_content": o.DeleteContent, "scrub_mentions": o.ScrubMentions, "counts": res.Counts})
	})
	if err != nil {
		return EraseResult{Counts: map[string]int{}}, err
	}
	if needSweep {
		res.Counts["originals_removed"] = s.dropOriginals(ctx)
	}
	res.Erased = true
	return res, nil
}

// regexpQuote escapes a literal for use inside a POSIX regular expression.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// CleanExpired removes records that outlived their purpose: ended sessions and sign-ins, spent codes and states, old
// read notifications, finished automation runs and webhook jobs. It runs with the hourly lifecycle job.
func (s *Service) CleanExpired(ctx context.Context) (map[string]int64, error) {
	now := s.Now()
	out := map[string]int64{}
	for _, q := range []struct{ name, sql string }{
		{"sessions", `DELETE FROM session WHERE expires_at < $1`},
		{"pending_sign_ins", `DELETE FROM mfa_pending WHERE expires_at < $1`},
		{"oauth_codes", `DELETE FROM oauth_code WHERE expires_at < $1`},
		{"sso_states", `DELETE FROM sso_state WHERE expires_at < $1`},
		{"undo_tokens", `DELETE FROM undo_token WHERE expires_at < $1`},
		{"oauth_grants", `DELETE FROM oauth_grant WHERE revoked_at < $1::timestamptz - interval '30 days'`},
		{"notifications", `DELETE FROM notification WHERE state IN ('read','archived') AND created_at < $1::timestamptz - interval '180 days'`},
		{"automation_runs", `DELETE FROM automation_run WHERE started_at < $1::timestamptz - interval '90 days'`},
		{"webhook_jobs", `DELETE FROM workflow_job WHERE (state='done' AND done_at < $1::timestamptz - interval '30 days') OR (state='failed' AND created_at < $1::timestamptz - interval '90 days')`},
		{"sign_in_records", `DELETE FROM login_attempt WHERE at < $1::timestamptz - interval '1 day'`},
		{"revoked_tokens", `DELETE FROM api_token WHERE (revoked_at < $1::timestamptz - interval '90 days') OR (expires_at < $1::timestamptz - interval '90 days')`},
	} {
		tag, err := s.DB.Exec(ctx, q.sql, now)
		if err != nil {
			return out, fmt.Errorf("%s: %w", q.name, err)
		}
		out[q.name] = tag.RowsAffected()
	}
	return out, nil
}
