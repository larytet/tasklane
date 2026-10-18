package domain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Customers as data subjects (SPEC 15.1): export and erase, on request, by an administrator or by the customer; and promotion of a customer
// to a staff account that keeps their history. It follows the rules for staff (docs/PRIVACY.md): requests are the tracker's records and are
// never deleted, only the person is removed from them; an erase refuses records under legal hold; it can be repeated; the audit log records
// that it happened and nothing personal.

type CustomerRow struct {
	ID       string     `json:"id"`
	Email    string     `json:"email"`
	Name     string     `json:"name"`
	Company  string     `json:"company,omitempty"`
	Created  time.Time  `json:"created"`
	Verified bool       `json:"verified"`
	Blocked  bool       `json:"blocked"`
	Erased   *time.Time `json:"erased,omitempty"`
	Staff    string     `json:"promoted_to,omitempty"` // the staff account the customer was promoted to
}

// customerAdmin checks that the caller may administer a customer: an instance administrator, or an administrator of the customer's own
// organization. A malformed or unknown id, and a customer of another organization, are not found.
func (s *Service) customerAdmin(ctx context.Context, a Actor, id string) (email, name, org string, err error) {
	if !a.Admin && !a.OrgAdmin {
		return "", "", "", ErrForbidden
	}
	var erased *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT email,name,org_id::text,erased_at FROM customer WHERE id::text=$1`, id).Scan(&email, &name, &org, &erased); err != nil {
		return "", "", "", ErrNotFound
	}
	if !a.Admin && org != a.OrgID() {
		return "", "", "", ErrNotFound
	}
	return email, name, org, nil
}

// ListCustomers finds customers by the start of their address or name (administrators).
func (s *Service) ListCustomers(ctx context.Context, a Actor, q string, limit int) ([]CustomerRow, error) {
	if !a.Admin && !a.OrgAdmin {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q = strings.ToLower(strings.TrimSpace(q))
	rows, err := s.DB.Query(ctx, `SELECT c.id::text,c.email,c.name,COALESCE(o.name,''),c.created_at,c.verified_at IS NOT NULL,c.blocked,c.erased_at,COALESCE(u.email,'')
		FROM customer c LEFT JOIN customer_org o ON o.id=c.customer_org_id LEFT JOIN app_user u ON u.id=c.promoted_to
		WHERE ($1 OR c.org_id::text=$2) AND ($3='' OR lower(c.email) LIKE $3||'%' OR lower(c.name) LIKE $3||'%') ORDER BY lower(c.email) LIMIT $4`, a.Admin, a.OrgID(), q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CustomerRow{}
	for rows.Next() {
		var r CustomerRow
		if err := rows.Scan(&r.ID, &r.Email, &r.Name, &r.Company, &r.Created, &r.Verified, &r.Blocked, &r.Erased, &r.Staff); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExportCustomerData returns everything held about a customer, for an administrator; the export is audited.
func (s *Service) ExportCustomerData(ctx context.Context, a Actor, id string) (map[string]any, error) {
	if _, _, _, err := s.customerAdmin(ctx, a, id); err != nil {
		return nil, err
	}
	out, err := s.customerExport(ctx, id)
	if err != nil {
		return nil, err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO audit_event(actor_id,actor_label,action,target,detail) VALUES($1,$2,'privacy.export.customer',$3,'{}')`, nilIfEmpty(a.ID), a.Label, id)
	return out, err
}

// CustomerExportSelf is the same export, for the customer asking for their own data.
func (s *Service) CustomerExportSelf(ctx context.Context, c Customer) (map[string]any, error) {
	out, err := s.customerExport(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO audit_event(actor_label,action,target,detail) VALUES('Customer portal','privacy.export.customer',$1,'{}')`, c.ID)
	return out, err
}

func (s *Service) customerExport(ctx context.Context, id string) (map[string]any, error) {
	out := map[string]any{}
	var email, name, lang, tz, company string
	var created time.Time
	var verified, seen, erased *time.Time
	var blocked, hasPass bool
	var promoted *string
	if err := s.DB.QueryRow(ctx, `SELECT c.email,c.name,c.language,c.timezone,COALESCE(o.name,''),c.created_at,c.verified_at,c.last_seen,c.erased_at,c.blocked,c.password_hash IS NOT NULL,c.promoted_to::text
		FROM customer c LEFT JOIN customer_org o ON o.id=c.customer_org_id WHERE c.id::text=$1`, id).Scan(&email, &name, &lang, &tz, &company, &created, &verified, &seen, &erased, &blocked, &hasPass, &promoted); err != nil {
		return nil, ErrNotFound
	}
	out["profile"] = map[string]any{"id": id, "email": email, "name": name, "language": lang, "time_zone": tz, "company": company, "created": created,
		"verified": verified, "last_seen": seen, "erased": erased, "blocked": blocked, "has_password": hasPass, "promoted_to_staff": promoted != nil}
	list := func(key, q string, args ...any) error {
		rows, err := s.DB.Query(ctx, q, args...)
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
	for _, q := range []struct {
		key, sql string
		args     []any
	}{
		{"requests", `SELECT i.key AS request, i.summary, COALESCE(t.body,'') AS description, st.name AS status, i.created_at, COALESCE(rt.name,'') AS request_type, pr.shared_with_org AS shared_with_company
			FROM portal_request pr JOIN issue i ON i.id=pr.issue_id AND i.deleted_at IS NULL JOIN status st ON st.id=i.status_id LEFT JOIN request_type rt ON rt.id=pr.request_type_id
			LEFT JOIN issue_text t ON t.issue_id=i.id AND t.field='description' WHERE pr.customer_id::text=$1 ORDER BY i.created_at`, []any{id}},
		{"your_comments", `SELECT i.key AS request, cc.body, cc.created_at FROM customer_comment cc JOIN issue i ON i.id=cc.issue_id WHERE cc.customer_id::text=$1 ORDER BY cc.created_at`, []any{id}},
		{"replies_to_your_requests", `SELECT i.key AS request, c.body, c.created_at FROM comment c JOIN issue i ON i.id=c.issue_id JOIN portal_request pr ON pr.issue_id=i.id WHERE pr.customer_id::text=$1 AND NOT c.internal ORDER BY c.created_at`, []any{id}},
		{"shared_with_you", `SELECT i.key AS request, i.summary FROM request_participant rp JOIN issue i ON i.id=rp.issue_id WHERE rp.customer_id::text=$1 ORDER BY i.key`, []any{id}},
		{"people_you_shared_with", `SELECT i.key AS request, cu.email FROM request_participant rp JOIN issue i ON i.id=rp.issue_id JOIN customer cu ON cu.id=rp.customer_id WHERE rp.added_by::text=$1 ORDER BY i.key`, []any{id}},
		{"sessions", `SELECT created_at, expires_at FROM customer_session WHERE customer_id::text=$1 ORDER BY created_at`, []any{id}},
		{"sign_in_codes", `SELECT created_at, used_at, attempts FROM customer_code WHERE lower(email)=lower($1) ORDER BY id`, []any{email}},
		{"mail_sent_to_you", `SELECT subject, kind, status, created_at FROM mail_message WHERE customer_id::text=$1 OR (kind='portal-code' AND lower(recipient)=lower($2)) ORDER BY id`, []any{id, email}},
		{"mail_from_you", `SELECT kind, subject, received_at, outcome, reason FROM mail_inbound WHERE purged_at IS NULL AND (customer_id::text=$1 OR lower(sender)=lower($2)) ORDER BY id`, []any{id, email}},
		{"bounce_records", `SELECT kind, status, source, at FROM mail_bounce WHERE lower(email)=lower($1) ORDER BY id`, []any{email}},
	} {
		if err := list(q.key, q.sql, q.args...); err != nil {
			return nil, fmt.Errorf("%s: %w", q.key, err)
		}
	}
	return out, nil
}

// EraseCustomer removes a customer's identity from the system (an administrator of the customer's organization, or the instance).
func (s *Service) EraseCustomer(ctx context.Context, a Actor, id string, o EraseOpts) (EraseResult, error) {
	if _, _, _, err := s.customerAdmin(ctx, a, id); err != nil {
		return EraseResult{Counts: map[string]int{}}, err
	}
	return s.eraseCustomer(ctx, a, id, o)
}

// CustomerEraseSelf is the customer's own request: they confirm by typing their address.
func (s *Service) CustomerEraseSelf(ctx context.Context, c Customer, confirm string, o EraseOpts) (EraseResult, error) {
	if !strings.EqualFold(strings.TrimSpace(confirm), c.Email) {
		return EraseResult{Counts: map[string]int{}}, invalid("type your email address to confirm that you want your data erased")
	}
	return s.eraseCustomer(ctx, Actor{Label: "Customer portal"}, c.ID, o)
}

func (s *Service) eraseCustomer(ctx context.Context, a Actor, id string, o EraseOpts) (EraseResult, error) {
	res := EraseResult{Counts: map[string]int{}}
	var email, name string
	var erased *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT email,name,erased_at FROM customer WHERE id::text=$1`, id).Scan(&email, &name, &erased); err != nil {
		return res, ErrNotFound
	}
	if erased != nil {
		return res, nil // already done
	}
	// legal hold: a request that must be kept keeps its author
	rows, err := s.DB.Query(ctx, `SELECT DISTINCT i.key FROM issue i WHERE i.legal_hold AND (EXISTS (SELECT 1 FROM portal_request pr WHERE pr.issue_id=i.id AND pr.customer_id::text=$1)
		OR EXISTS (SELECT 1 FROM customer_comment cc WHERE cc.issue_id=i.id AND cc.customer_id::text=$1)
		OR EXISTS (SELECT 1 FROM mail_inbound m WHERE m.issue_id=i.id AND m.purged_at IS NULL AND (m.customer_id::text=$1 OR lower(m.sender)=lower($2)))) ORDER BY 1`, id, email)
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
		return res, &ConflictError{"requests under legal hold involve this customer (" + strings.Join(held, ", ") + "); release the hold first"}
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	newEmail, erasedName := "erased+"+short+"@erased.invalid", "Deleted customer"
	needSweep := false
	touched := map[string]bool{}
	err = s.tx(ctx, func(tx pgx.Tx) error {
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
		// the requests they raised: their keys, for reindexing and for deleting content
		krows, err := tx.Query(ctx, `SELECT i.key,i.id::text,COALESCE(rt.fields::text,'[]') FROM portal_request pr JOIN issue i ON i.id=pr.issue_id LEFT JOIN request_type rt ON rt.id=pr.request_type_id WHERE pr.customer_id::text=$1`, id)
		if err != nil {
			return err
		}
		type req struct{ key, id, fields string }
		var reqs []req
		for krows.Next() {
			var r req
			krows.Scan(&r.key, &r.id, &r.fields)
			reqs = append(reqs, r)
			touched[r.key] = true
		}
		krows.Close()
		if o.DeleteContent {
			if err := exec("comments_deleted", `DELETE FROM customer_comment WHERE customer_id::text=$1`, id); err != nil {
				return err
			}
			for _, r := range reqs {
				if err := exec("requests_emptied", `UPDATE issue SET summary='Request from a deleted customer' WHERE id::text=$1`, r.id); err != nil {
					return err
				}
				if err := exec("texts_emptied", `UPDATE issue_text SET body='' WHERE issue_id::text=$1 AND field='description'`, r.id); err != nil {
					return err
				}
				if err := exec("text_versions_emptied", `UPDATE text_version SET body=CASE WHEN field='summary' THEN 'Request from a deleted customer' ELSE '' END WHERE issue_id::text=$1 AND field IN ('summary','description')`, r.id); err != nil {
					return err
				}
				if err := exec("mail_emptied", `UPDATE mail_message SET subject='['||$2||'] Request from a deleted customer', body_text='', body_html='' WHERE issue_id::text=$1`, r.id, r.key); err != nil {
					return err
				}
				if err := exec("form_answers_removed", `DELETE FROM issue_field_value WHERE issue_id::text=$1 AND field_id IN (SELECT (regexp_replace(f->>'key','\D','','g'))::bigint FROM jsonb_array_elements($2::jsonb) f)`, r.id, r.fields); err != nil {
					return err
				}
			}
		}
		if o.ScrubMentions {
			for _, term := range []string{email, name} {
				term = strings.TrimSpace(term)
				if len(term) < 3 {
					continue
				}
				pat := regexpQuote(term)
				for _, q := range []struct{ count, sql string }{
					{"texts_scrubbed", `UPDATE issue_text SET body=regexp_replace(body, $2, '[deleted customer]', 'gi') WHERE position(lower($1) in lower(body))>0`},
					{"text_versions_scrubbed", `UPDATE text_version SET body=regexp_replace(body, $2, '[deleted customer]', 'gi') WHERE position(lower($1) in lower(body))>0`},
					{"comments_scrubbed", `UPDATE comment SET body=regexp_replace(body, $2, '[deleted customer]', 'gi') WHERE position(lower($1) in lower(body))>0`},
					{"customer_comments_scrubbed", `UPDATE customer_comment SET body=regexp_replace(body, $2, '[deleted customer]', 'gi') WHERE position(lower($1) in lower(body))>0`},
					{"summaries_scrubbed", `UPDATE issue SET summary=regexp_replace(summary, $2, '[deleted customer]', 'gi') WHERE position(lower($1) in lower(summary))>0`},
					{"history_scrubbed", `UPDATE issue_history SET before=regexp_replace(before, $2, '[deleted customer]', 'gi'), after=regexp_replace(after, $2, '[deleted customer]', 'gi') WHERE position(lower($1) in lower(COALESCE(before,'')||' '||COALESCE(after,'')))>0`},
					{"notifications_scrubbed", `UPDATE notification SET what=regexp_replace(what, $2, '[deleted customer]', 'gi'), body=regexp_replace(body, $2, '[deleted customer]', 'gi') WHERE position(lower($1) in lower(what||' '||body))>0`},
					{"mail_scrubbed", `UPDATE mail_message SET subject=regexp_replace(subject, $2, '[deleted customer]', 'gi'), body_text=regexp_replace(body_text, $2, '[deleted customer]', 'gi'), body_html=regexp_replace(body_html, $2, '[deleted customer]', 'gi')
						WHERE position(lower($1) in lower(subject||' '||body_text||' '||body_html))>0`},
					{"events_scrubbed", `UPDATE outbox SET payload=regexp_replace(payload::text, $2, 'Deleted customer', 'gi')::jsonb WHERE position(lower($1) in lower(payload::text))>0`},
				} {
					if err := exec(q.count, q.sql, term, pat); err != nil {
						return err
					}
				}
			}
		}
		for _, q := range []struct{ count, sql string }{
			{"events_relabelled", `UPDATE outbox SET payload=jsonb_set(payload,'{actor}',to_jsonb('Deleted customer (customer)'::text)) WHERE payload->>'customer'=$1 AND payload ? 'actor'`},
			{"notices_relabelled", `UPDATE notification SET actor_label='Deleted customer (customer)' WHERE lower(actor_label) IN (lower($1)||' (customer)', lower($2)||' (customer)')`},
			{"sessions_ended", `DELETE FROM customer_session WHERE customer_id::text=$1`},
			{"sign_in_codes_removed", `DELETE FROM customer_code WHERE lower(email)=lower($2) AND $1::text IS NOT NULL`},
			{"participations_removed", `DELETE FROM request_participant WHERE customer_id::text=$1`},
			{"shares_detached", `UPDATE request_participant SET added_by=NULL WHERE added_by::text=$1`},
			{"counters_removed", `DELETE FROM portal_event WHERE subject=$1 OR subject=lower($2)`},
			{"mail_removed", `DELETE FROM mail_message WHERE customer_id::text=$1 OR (kind='portal-code' AND lower(recipient)=lower($2))`},
		} {
			args := []any{id, email}
			if q.count == "notices_relabelled" {
				args = []any{email, name}
			}
			switch q.count {
			case "events_relabelled", "sessions_ended", "participations_removed", "shares_detached":
				args = []any{id}
			}
			if err := exec(q.count, q.sql, args...); err != nil {
				return err
			}
		}
		// mail from them: the content goes, the rows stay; bounce and suppression records for the address go
		n, err := s.erasePersonMail(ctx, tx, "", email, res.Counts)
		if err != nil {
			return err
		}
		needSweep = n
		if _, err := tx.Exec(ctx, `UPDATE mail_inbound SET customer_id=NULL WHERE customer_id::text=$1`, id); err != nil {
			return err
		}
		if err := exec("customer_anonymised", `UPDATE customer SET email=$2,name=$3,language='',timezone='',customer_org_id=NULL,password_hash=NULL,blocked=true,erased_at=$4,last_seen=NULL WHERE id::text=$1`, id, newEmail, erasedName, s.Now()); err != nil {
			return err
		}
		for k := range touched { // rebuild what is derived from the text: the index follows these events
			if err := outbox(ctx, tx, "issue:"+k, "issue.updated", map[string]any{"key": k, "fields": []string{"text"}, "actor": "privacy", "reason": "erase"}); err != nil {
				return err
			}
		}
		res.Counts["requests_reindexed"] = len(touched)
		return s.audit(ctx, tx, a, "privacy.erased.customer", id, map[string]any{"delete_content": o.DeleteContent, "scrub_mentions": o.ScrubMentions, "counts": res.Counts})
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

type PromoteIn struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// PromoteCustomer makes a staff account for a customer, with their address. The customer record stays (and is linked to the account): their
// requests, comments and the history of both are untouched, and they can still use the portal as a customer. Their account counts as a
// staff seat; a customer never does.
func (s *Service) PromoteCustomer(ctx context.Context, a Actor, id string, in PromoteIn) (User, error) {
	email, name, org, err := s.customerAdmin(ctx, a, id)
	if err != nil {
		return User{}, err
	}
	var erased *time.Time
	var promoted *string
	s.DB.QueryRow(ctx, `SELECT erased_at,promoted_to::text FROM customer WHERE id::text=$1`, id).Scan(&erased, &promoted)
	if erased != nil {
		return User{}, invalid("this customer was erased")
	}
	if promoted != nil {
		return User{}, &ConflictError{"this customer was already promoted to a staff account"}
	}
	n := strings.TrimSpace(in.Name)
	if n == "" {
		n = name
	}
	if n == "" {
		n = strings.Split(email, "@")[0]
	}
	u, err := s.CreateUserAs(ctx, a, email, n, in.Password, false, org)
	if err != nil {
		return User{}, err
	}
	if _, err := s.DB.Exec(ctx, `UPDATE customer SET promoted_to=$2, name=CASE WHEN name='' THEN $3 ELSE name END WHERE id::text=$1`, id, u.ID, n); err != nil {
		return u, err
	}
	return u, nil
}
