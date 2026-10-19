package domain

import (
	"context"
	"fmt"
	"html"
	netmail "net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Email to customers (SPEC 15.1). A customer is told, by email, when they are answered, when their request is resolved, and when a
// colleague on a shared request writes; and once when a request is received. Every message carries only what the portal would show them
// (the public text, never a staff name or an internal note) and a link to the portal, and replies to it come back as their comment
// (inbound_customer.go). Only customers who have signed in at least once (so have proved the address is theirs) are written to; the
// check that they still see the request is made again when the message is sent.

// customersOf lists the verified, unblocked customers who see a request: its owner, the people added to it, and (when shared) the
// owner's customer organization.
func (s *Service) customersOf(ctx context.Context, tx pgx.Tx, issueID string) ([]recipient, error) {
	rows, err := tx.Query(ctx, `SELECT c.id::text,c.name,c.email FROM portal_request pr JOIN customer c ON c.verified_at IS NOT NULL AND NOT c.blocked AND
		(c.id=pr.customer_id OR EXISTS (SELECT 1 FROM request_participant rp WHERE rp.issue_id=pr.issue_id AND rp.customer_id=c.id)
		 OR (pr.shared_with_org AND c.customer_org_id IS NOT NULL AND c.customer_org_id=(SELECT customer_org_id FROM customer WHERE id=pr.customer_id)))
		WHERE pr.issue_id::text=$1 ORDER BY c.email`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recipient
	for rows.Next() {
		var r recipient
		rows.Scan(&r.id, &r.name, &r.email)
		out = append(out, r)
	}
	return out, rows.Err()
}

// customerMayReceive: the customer still sees the request (and is not blocked) now, at send time.
func (s *Service) customerMayReceive(ctx context.Context, customerID, issueID string) bool {
	var ok bool
	s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM portal_request pr JOIN issue i ON i.id=pr.issue_id AND i.deleted_at IS NULL JOIN customer c ON c.id::text=$1 AND NOT c.blocked
		WHERE pr.issue_id::text=$2 AND (c.id=pr.customer_id OR EXISTS (SELECT 1 FROM request_participant rp WHERE rp.issue_id=pr.issue_id AND rp.customer_id=c.id)
		 OR (pr.shared_with_org AND c.customer_org_id IS NOT NULL AND c.customer_org_id=(SELECT customer_org_id FROM customer WHERE id=pr.customer_id))))`, customerID, issueID).Scan(&ok)
	return ok
}

func (s *Service) queueCustomerMail(ctx context.Context, tx pgx.Tx, e notifyEvent) (int, error) {
	if s.Mail == nil || e.Payload.Key == "" {
		return 0, nil
	}
	switch e.Type {
	case "comment.added", "issue.transitioned", "customer.commented", "customer.requested":
	default:
		return 0, nil
	}
	var id, pid, summary, pkey, status, category string
	var mirrored bool
	err := tx.QueryRow(ctx, `SELECT i.id::text,i.project_id::text,i.summary,p.key,st.name,st.category,p.mirror_of IS NOT NULL FROM issue i JOIN project p ON p.id=i.project_id JOIN status st ON st.id=i.status_id
		JOIN portal_request pr ON pr.issue_id=i.id WHERE i.key=$1 AND i.deleted_at IS NULL`, e.Payload.Key).Scan(&id, &pid, &summary, &pkey, &status, &category, &mirrored)
	if err == pgx.ErrNoRows {
		return 0, nil // not a request raised through a portal
	}
	if err != nil {
		return 0, err
	}
	if mirrored {
		return 0, nil
	}
	var what, body string
	skip := ""
	switch e.Type {
	case "comment.added": // a staff comment that was not marked internal
		var internal bool
		if err := tx.QueryRow(ctx, `SELECT body,internal FROM comment WHERE id=$1`, e.Payload.Comment).Scan(&body, &internal); err != nil || internal {
			return 0, nil
		}
		what = "Support replied to your request:"
	case "issue.transitioned":
		if category != "done" || !strings.EqualFold(status, e.Payload.To) {
			return 0, nil
		}
		what = "Your request was marked " + status + "."
	case "customer.commented": // a colleague on a shared request: not told about their own comment
		body, skip = e.Payload.Text, e.Payload.Customer
		who := strings.TrimSuffix(e.Payload.Actor, " (customer)")
		what = who + " wrote on a request you share:"
	case "customer.requested":
		what, skip = "We received your request. We will reply here and by email.", "" // everyone who sees it
	}
	rs, err := s.customersOf(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rs {
		if r.id == skip && skip != "" {
			continue
		}
		if e.Type == "customer.requested" && e.Payload.Customer != "" && r.id != e.Payload.Customer {
			continue // the confirmation is for the person who made it
		}
		m := s.composeCustomer(ctx, tx, id, e, e.Payload.Key, pkey, summary, what, body, r)
		tag, err := tx.Exec(ctx, `INSERT INTO mail_message(idempotency_key,issue_id,recipient_id,customer_id,recipient,internal,subject,message_id,in_reply_to,refs,body_text,body_html,kind,reply_to,next_attempt_at)
			VALUES($1,$2,NULL,$3,$4,false,$5,$6,$7,$8,$9,$10,'customer',$11,$12) ON CONFLICT (idempotency_key) DO NOTHING`,
			fmt.Sprintf("cust:%d:%s", e.ID, r.id), id, r.id, (&netmail.Address{Name: r.name, Address: r.email}).String(), m.Subject, m.MessageID, m.InReplyTo, m.Refs, m.Text, m.HTML, m.ReplyTo, s.Now())
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

func (s *Service) composeCustomer(ctx context.Context, tx pgx.Tx, issueID string, e notifyEvent, key, project, summary, what, body string, r recipient) mailMsg {
	dom := s.Mail.Domain
	root := fmt.Sprintf("<portal.%s@%s>", issueID, dom)
	var prev string
	tx.QueryRow(ctx, `SELECT message_id FROM mail_message WHERE issue_id::text=$1 AND customer_id::text=$2 ORDER BY id DESC LIMIT 1`, issueID, r.id).Scan(&prev)
	m := mailMsg{Subject: fmt.Sprintf("[%s] %s", key, summary), MessageID: fmt.Sprintf("<portal.%s.%d.%s@%s>", issueID, e.ID, r.id[:8], dom), InReplyTo: root, Refs: root}
	if prev != "" {
		m.InReplyTo, m.Refs = prev, root+" "+prev
	}
	link := strings.TrimRight(s.Mail.BaseURL, "/") + "/portal/" + project + "#/r/" + key
	m.ReplyTo = s.replyAddress(ctx, tx, issueID, r.id)
	var t strings.Builder
	t.WriteString("-- Reply above this line --\r\n\r\n" + what + "\r\n")
	if body != "" {
		t.WriteString("\r\n" + strings.TrimSpace(body) + "\r\n")
	}
	t.WriteString("\r\nOpen your request: " + link + "\r\nYou can answer by replying to this email.\r\n")
	m.Text = t.String()
	var h strings.Builder
	h.WriteString(`<div style="font-family:system-ui,sans-serif;font-size:14px"><p>` + html.EscapeString(what) + `</p>`)
	if body != "" {
		h.WriteString(`<blockquote style="margin:0;padding:8px 12px;border-left:3px solid #ccc;white-space:pre-wrap">` + html.EscapeString(strings.TrimSpace(body)) + `</blockquote>`)
	}
	h.WriteString(`<p><a href="` + html.EscapeString(link) + `">Open your request</a>. You can answer by replying to this email.</p></div>`)
	m.HTML = h.String()
	return m
}

// mailMsg is the parts of a message the notifier stores.
type mailMsg struct {
	Subject, MessageID, InReplyTo, Refs, ReplyTo, Text, HTML string
}
