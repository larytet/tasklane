package domain

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/larytet/tasklane/internal/mail"
)

// Replies from customers (SPEC 15.1, 13.1). A message from an address that is not a staff account but is a customer's is a reply to
// something we sent that customer: the signed reply address (issued to that customer, so a forwarded message does not carry its power),
// the threading headers, or the request key in the subject say which request. It follows the same rules as every inbound message: proof
// of origin (a trusted receiver's verdict or a DKIM signature) or it waits in quarantine; limits per sender and per request; automatic
// mail is ignored. An accepted reply becomes that customer's comment, provided they still see the request. A customer cannot open a
// request by email.

func (s *Service) classifyCustomerReply(ctx context.Context, p *mail.Parsed, raw []byte, trusted bool, addr, fromDomain, body string, now time.Time, lim InboundLimits) (verdict, bool) {
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM customer WHERE lower(email)=$1 AND NOT blocked`, addr)
	if err != nil {
		return verdict{}, false
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return verdict{}, false
	}
	h := p.Header
	v := verdict{body: body}
	// which request, and which of the customers with this address it was sent to
	var issueID, customerID string
	re := regexp.MustCompile(`(?i)reply\+([A-Za-z0-9_-]{20,80})@` + regexp.QuoteMeta(s.mailDomain()))
	var addrs []string
	for _, k := range []string{"To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To"} {
		addrs = append(addrs, h[netmailCanon(k)]...)
	}
	if m := re.FindStringSubmatch(strings.Join(addrs, ",")); m != nil {
		var last error
		for _, id := range ids {
			iss, err := s.verifyReply(ctx, m[1], id)
			if err == nil {
				issueID, customerID = iss, id
				break
			}
			last = err
		}
		if issueID == "" {
			v.outcome, v.reason = "quarantined", last.Error()
			return v, true
		}
	}
	if issueID == "" {
		mids := reMsgID.FindAllString(h.Get("In-Reply-To")+" "+h.Get("References"), -1)
		for i := len(mids) - 1; i >= 0 && issueID == ""; i-- {
			for _, id := range ids {
				var iss *string
				if s.DB.QueryRow(ctx, `SELECT issue_id::text FROM mail_message WHERE message_id=$1 AND customer_id::text=$2 AND issue_id IS NOT NULL LIMIT 1`, mids[i], id).Scan(&iss) == nil && iss != nil {
					issueID, customerID = *iss, id
					break
				}
			}
		}
	}
	if issueID == "" {
		if m := reSubjKey.FindStringSubmatch(h.Get("Subject")); m != nil {
			for _, id := range ids {
				var iss string
				if s.DB.QueryRow(ctx, `SELECT i.id::text FROM issue i JOIN portal_request pr ON pr.issue_id=i.id WHERE i.key=$1 AND i.deleted_at IS NULL`, strings.ToUpper(m[1])).Scan(&iss) == nil && s.customerMayReceive(ctx, id, iss) {
					issueID, customerID = iss, id
					break
				}
			}
		}
	}
	if issueID == "" {
		return ignore("not a reply to a message about a request"), true
	}
	s.DB.QueryRow(ctx, `SELECT key FROM issue WHERE id::text=$1`, issueID).Scan(&v.issueKey)
	v.issueID, v.customerID = issueID, customerID
	proven := trusted && authenticated(h, fromDomain)
	why := "the sender's domain was not verified by a trusted receiver and the message carries no DKIM signature"
	if !proven && s.DNS != nil {
		proven, why = s.dkimProof(ctx, raw, fromDomain, now)
	}
	if !proven {
		v.outcome, v.reason = "quarantined", "no proof of origin: "+why
		return v, true
	}
	if !s.customerMayReceive(ctx, customerID, issueID) {
		v.outcome, v.reason = "quarantined", "the sender does not see that request"
		return v, true
	}
	var mirrored bool
	s.DB.QueryRow(ctx, `SELECT p.mirror_of IS NOT NULL FROM project p JOIN issue i ON i.project_id=p.id WHERE i.id::text=$1`, issueID).Scan(&mirrored)
	if mirrored {
		v.outcome, v.reason = "quarantined", "mirrored project"
		return v, true
	}
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE issue_id::text=$1 AND outcome='accepted' AND received_at>$2`, issueID, now.Add(-time.Hour)).Scan(&n)
	if n >= lim.PerIssueHour {
		return ignore("rate limit: too many messages for this request"), true
	}
	if body == "" {
		return ignore("the message has no text"), true
	}
	v.outcome = "accepted"
	return v, true
}

// customerCommentFromMail writes an accepted customer reply into the request's thread.
func (s *Service) customerCommentFromMail(ctx context.Context, customerID, key, body string, inboundID int64) error {
	c, blocked, err := s.customerRow(ctx, s.DB, `c.id::text=$1`, customerID)
	if err != nil || blocked {
		return ErrForbidden
	}
	body, _ = Sanitize(strings.TrimSpace(body))
	if body == "" {
		return invalid("the message has no text")
	}
	if len([]rune(body)) > 10000 {
		body = string([]rune(body)[:10000])
	}
	end, perr := s.admit(ctx, strings.Split(key, "-")[0])
	if perr != nil {
		return perr
	}
	defer end()
	if err := s.insertCustomerComment(ctx, c, key, body, inboundID); err != nil {
		return err
	}
	s.emit(Event{Type: "comment.added", Issue: key, Actor: c.Email})
	return nil
}
