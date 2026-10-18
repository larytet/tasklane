package domain

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	netmail "net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/mail"
)

// Inbound email (SPEC 13.1). A raw message arrives from a source, is stored whole before anything else, is recorded once
// (by Message-ID), and ends in exactly one outcome: accepted (a comment), ignored (with the reason), quarantined (for an
// administrator) or rejected. Nothing is ever sent back to the sender, so there is no backscatter.

// InboundLimits bounds what one sender, domain, issue or message may cost. Zero values take the defaults.
type InboundLimits struct {
	MaxBytes      int64 // one message, 25 MiB
	PerSenderHour int   // 30
	PerDomainHour int   // 300
	PerIssueHour  int   // 100
}

func (s *Service) inboundLimits() InboundLimits {
	l := InboundLimits{}
	if s.Mail != nil {
		l = s.Mail.Inbound
	} else {
		l = s.InboundLimits
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = 25 << 20
	}
	if l.PerSenderHour <= 0 {
		l.PerSenderHour = 30
	}
	if l.PerDomainHour <= 0 {
		l.PerDomainHour = 300
	}
	if l.PerIssueHour <= 0 {
		l.PerIssueHour = 100
	}
	return l
}

const replyTokenDays = 30

// mailDomain is the domain of our reply addresses: the outbound settings' domain, or InboundDomain.
func (s *Service) mailDomain() string {
	if s.Mail != nil && s.Mail.Domain != "" {
		return s.Mail.Domain
	}
	return s.InboundDomain
}

// ---------- reply addresses ----------

type replyKey struct {
	kid byte
	key []byte
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// replyKeys returns the unrevoked keys, newest first; the first signs, all verify. The first key is created on first use.
func (s *Service) replyKeys(ctx context.Context, q queryer) ([]replyKey, error) {
	load := func() ([]replyKey, error) {
		rows, err := q.Query(ctx, `SELECT kid,key FROM mail_reply_key WHERE revoked_at IS NULL ORDER BY kid DESC`)
		if err != nil {
			return nil, err
		}
		var raw []struct {
			kid int16
			key []byte
		}
		for rows.Next() {
			var r struct {
				kid int16
				key []byte
			}
			if err := rows.Scan(&r.kid, &r.key); err != nil {
				rows.Close()
				return nil, err
			}
			raw = append(raw, r)
		}
		rows.Close()
		var out []replyKey
		for _, r := range raw {
			k, err := s.unseal(ctx, r.key)
			if err != nil {
				return nil, err
			}
			kb, _ := hex.DecodeString(k)
			out = append(out, replyKey{kid: byte(r.kid), key: kb})
		}
		return out, nil
	}
	ks, err := load()
	if err != nil || len(ks) > 0 {
		return ks, err
	}
	if _, err := s.newReplyKey(ctx, q); err != nil {
		return nil, err
	}
	return load()
}

func (s *Service) newReplyKey(ctx context.Context, q queryer) (int, error) {
	k := make([]byte, 32)
	rand.Read(k)
	sealed, err := s.seal(ctx, hex.EncodeToString(k))
	if err != nil {
		return 0, err
	}
	var kid int
	err = q.QueryRow(ctx, `INSERT INTO mail_reply_key(kid,key) VALUES((SELECT COALESCE(MAX(kid),0)+1 FROM mail_reply_key),$1)
		ON CONFLICT DO NOTHING RETURNING kid`, sealed).Scan(&kid)
	if errors.Is(err, pgx.ErrNoRows) { // another node created the first key at the same moment
		return 0, nil
	}
	return kid, err
}

func uuidBytes(id string) []byte {
	b, _ := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return b
}

func uuidString(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func replyMAC(key []byte, head, issue, user []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("tasklane reply v1"))
	m.Write(head)
	m.Write(issue)
	m.Write(user)
	return m.Sum(nil)[:10]
}

// replyAddress is "reply+TOKEN@domain": the token names the issue and carries a signature over the issue, the recipient and
// the expiry, so a forwarded notification does not give its power to whoever received the forward.
func (s *Service) replyAddress(ctx context.Context, q queryer, issueID, userID string) string {
	if s.Mail == nil {
		return ""
	}
	ks, err := s.replyKeys(ctx, q)
	if err != nil || len(ks) == 0 {
		return ""
	}
	head := make([]byte, 3)
	head[0] = ks[0].kid
	binary.BigEndian.PutUint16(head[1:], uint16(s.Now().Add(replyTokenDays*24*time.Hour).Unix()/86400))
	issue := uuidBytes(issueID)
	tok := append(append(append([]byte{}, head...), issue...), replyMAC(ks[0].key, head, issue, uuidBytes(userID))...)
	return "reply+" + base64.RawURLEncoding.EncodeToString(tok) + "@" + s.Mail.Domain
}

var (
	errTokenBad     = errors.New("the reply address is not valid")
	errTokenExpired = errors.New("the reply address has expired")
	errTokenRevoked = errors.New("the reply address was signed with a key that has been revoked")
	errTokenSender  = errors.New("the reply address was not issued to this sender (a forwarded notification, or a forged address)")
)

// verifyReply returns the issue the token names, if it was issued to this user, has not expired and its key stands.
func (s *Service) verifyReply(ctx context.Context, token, userID string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 3+16+10 {
		return "", errTokenBad
	}
	head, issue, mac := raw[:3], raw[3:19], raw[19:]
	ks, err := s.replyKeys(ctx, s.DB)
	if err != nil {
		return "", err
	}
	var key []byte
	for _, k := range ks {
		if k.kid == head[0] {
			key = k.key
		}
	}
	if key == nil {
		return "", errTokenRevoked
	}
	if int64(binary.BigEndian.Uint16(head[1:]))*86400 < s.Now().Unix() {
		return "", errTokenExpired
	}
	if !hmac.Equal(mac, replyMAC(key, head, issue, uuidBytes(userID))) {
		return "", errTokenSender
	}
	return uuidString(issue), nil
}

// RotateReplyKey adds a signing key. Addresses signed by earlier keys keep working until those keys are revoked.
func (s *Service) RotateReplyKey(ctx context.Context, a Actor) (int, error) {
	if !a.Admin {
		return 0, ErrForbidden
	}
	var kid int
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := s.replyKeys(ctx, tx); err != nil {
			return err
		}
		var err error
		if kid, err = s.newReplyKey(ctx, tx); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "mail.reply_key.rotate", fmt.Sprint(kid), nil)
	})
	return kid, err
}

// RevokeReplyKey stops a key verifying: every address it signed stops working. The newest key cannot be revoked.
func (s *Service) RevokeReplyKey(ctx context.Context, a Actor, kid int) error {
	if !a.Admin {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var newest int
		tx.QueryRow(ctx, `SELECT COALESCE(MAX(kid),0) FROM mail_reply_key WHERE revoked_at IS NULL`).Scan(&newest)
		if kid == newest {
			return invalid("the newest key signs new addresses; rotate first")
		}
		tag, err := tx.Exec(ctx, `UPDATE mail_reply_key SET revoked_at=$2 WHERE kid=$1 AND revoked_at IS NULL`, kid, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "mail.reply_key.revoke", fmt.Sprint(kid), nil)
	})
}

// ---------- sources ----------

type MailSource struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Trusted bool   `json:"trusted"`
	Enabled bool   `json:"enabled"`
	Secret  string `json:"secret,omitempty"` // shown once, at creation
}

func (s *Service) CreateMailSource(ctx context.Context, a Actor, name string, trusted bool) (MailSource, error) {
	if !a.Admin {
		return MailSource{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return MailSource{}, invalid("a source needs a name")
	}
	sec := make([]byte, 32)
	rand.Read(sec)
	secret := hex.EncodeToString(sec)
	sealed, err := s.seal(ctx, secret)
	if err != nil {
		return MailSource{}, err
	}
	m := MailSource{Name: name, Trusted: trusted, Enabled: true, Secret: secret}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO mail_source(name,secret,trusted) VALUES($1,$2,$3) RETURNING id`, name, sealed, trusted).Scan(&m.ID); err != nil {
			return invalid("a source with that name exists")
		}
		return s.audit(ctx, tx, a, "mail.source.create", m.ID, map[string]any{"name": name, "trusted": trusted})
	})
	return m, err
}

func (s *Service) ListMailSources(ctx context.Context, a Actor) ([]MailSource, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	rows, err := s.DB.Query(ctx, `SELECT id,name,trusted,enabled FROM mail_source ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MailSource{}
	for rows.Next() {
		var m MailSource
		if err := rows.Scan(&m.ID, &m.Name, &m.Trusted, &m.Enabled); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) DeleteMailSource(ctx context.Context, a Actor, id string) error {
	if !a.Admin {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM mail_source WHERE id=$1`, id)
		if err != nil {
			return ErrNotFound
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "mail.source.delete", id, nil)
	})
}

// ReceiveInboundMail is the webhook transport: the body is the raw message, signed with the source's secret
// ("sha256=" + HMAC-SHA256 hex in the signature header). Every refusal is the same ErrForbidden.
func (s *Service) ReceiveInboundMail(ctx context.Context, sourceID, signature string, raw []byte) (InboundResult, error) {
	var sealed []byte
	var trusted, enabled bool
	if err := s.DB.QueryRow(ctx, `SELECT secret,trusted,enabled FROM mail_source WHERE id::text=$1`, sourceID).Scan(&sealed, &trusted, &enabled); err != nil || !enabled {
		return InboundResult{}, ErrForbidden
	}
	secret, err := s.unseal(ctx, sealed)
	if err != nil {
		return InboundResult{}, ErrForbidden
	}
	if !hmac.Equal([]byte(signature), []byte(SignInbound(secret, raw))) {
		return InboundResult{}, ErrForbidden
	}
	return s.IngestMail(ctx, sourceID, trusted, raw)
}

// ---------- ingest ----------

type InboundResult struct {
	ID        int64  `json:"id"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

type verdict struct {
	outcome, reason    string
	issueID, userID    string
	body               string
	issueKey, userName string
	userKind           string
	userAdmin          bool
}

var (
	reMsgID   = regexp.MustCompile(`<[^<>\s]{1,250}>`)
	reSubjKey = regexp.MustCompile(`\[([A-Za-z][A-Za-z0-9]*-[0-9]+)\]`)
	reOOO     = regexp.MustCompile(`(?i)^\s*(automatic reply|auto( |-)?reply|auto:|out of office|abwesenheitsnotiz|réponse automatique|respuesta automática)`)
)

func (s *Service) storeRaw(ctx context.Context, raw []byte) (string, error) {
	if err := s.blobsReady(); err != nil {
		return "", err
	}
	sha, err := s.Blobs.Store.Put(raw)
	if err != nil {
		return "", err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO blob(sha256,size,mime,class,scan_state,refcount) VALUES($1,$2,'message/rfc822','other','clean',1)
		ON CONFLICT (sha256) DO UPDATE SET refcount=blob.refcount+1, zero_since=NULL`, sha, len(raw))
	return sha, err
}

// IngestMail records and classifies one raw message. trusted says that the receiving path is ours, so the
// Authentication-Results header may be believed; without it no message has proof of origin and every one is quarantined.
func (s *Service) IngestMail(ctx context.Context, sourceID string, trusted bool, raw []byte) (InboundResult, error) {
	if s.mailDomain() == "" {
		return InboundResult{}, invalid("email is not configured")
	}
	now := s.Now()
	lim := s.inboundLimits()
	sum := sha256.Sum256(raw)
	short := hex.EncodeToString(sum[:])[:32]
	var src any
	if sourceID != "" {
		src = sourceID
	}
	record := func(mid, outcome, reason string) (InboundResult, error) { // a message we will not parse or keep
		var id int64
		err := s.DB.QueryRow(ctx, `INSERT INTO mail_inbound(source_id,message_id,received_at,outcome,reason) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT (message_id) DO UPDATE SET dupes=mail_inbound.dupes+1 RETURNING id`, src, mid, now, outcome, reason).Scan(&id)
		return InboundResult{ID: id, Outcome: outcome, Reason: reason}, err
	}
	if int64(len(raw)) > lim.MaxBytes {
		return record("<too-large-"+short+">", "rejected", "the message is larger than the limit")
	}
	p, perr := mail.Parse(raw)
	if perr != nil {
		return record("<unparsed-"+short+">", "rejected", perr.Error())
	}
	mid := reMsgID.FindString(p.Header.Get("Message-Id"))
	if mid == "" {
		mid = "<noid-" + short + ">"
	}
	from, ferr := singleSender(p.Header)
	sender, subject := "", p.Header.Get("Subject")
	if ferr == nil {
		sender = strings.ToLower(from.Address)
	}
	if d, err := (&mime.WordDecoder{}).DecodeHeader(subject); err == nil {
		subject = d
	}
	if len(subject) > 500 {
		subject = subject[:500]
	}
	// claim: the first delivery of a Message-ID processes it; a repeat is counted and ignored, unless the first one died
	// half way (still pending after two minutes), when the repeat finishes the job
	var id int64
	var fresh bool
	var prevOutcome string
	var prevAt time.Time
	if err := s.DB.QueryRow(ctx, `INSERT INTO mail_inbound(source_id,message_id,sender,subject,received_at) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (message_id) DO UPDATE SET dupes=mail_inbound.dupes+1
		RETURNING id,(xmax=0),outcome,received_at`, src, mid, sender, subject, now).Scan(&id, &fresh, &prevOutcome, &prevAt); err != nil {
		return InboundResult{}, err
	}
	if !fresh && !(prevOutcome == "pending" && prevAt.Before(now.Add(-2*time.Minute))) {
		return InboundResult{ID: id, Outcome: "ignored", Reason: "duplicate of a message already received", Duplicate: true}, nil
	}
	if sha, err := s.storeRaw(ctx, raw); err != nil { // before anything else: if this fails the sender is told to retry
		s.DB.Exec(ctx, `DELETE FROM mail_inbound WHERE id=$1 AND outcome='pending'`, id)
		return InboundResult{}, err
	} else {
		s.DB.Exec(ctx, `UPDATE mail_inbound SET raw_sha256=$2 WHERE id=$1`, id, sha)
	}
	v := s.classifyInbound(ctx, p, trusted, from, ferr, id, now, lim)
	if v.outcome == "accepted" {
		act := Actor{ID: v.userID, Label: v.userName, Admin: v.userAdmin && v.userKind != "agent", Kind: v.userKind}
		c, err := s.AddComment(ctx, act, v.issueKey, v.body, false)
		if err != nil {
			v.outcome, v.reason = "quarantined", "the sender may not comment on that issue"
			if !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
				v.reason = "the comment was refused: " + err.Error()
			}
		} else {
			s.DB.Exec(ctx, `UPDATE comment SET mail_inbound_id=$2 WHERE id=$1`, c.ID, id)
			s.DB.Exec(ctx, `UPDATE mail_inbound SET comment_id=$2 WHERE id=$1`, id, c.ID)
		}
	}
	var iss, usr any
	if v.issueID != "" {
		iss = v.issueID
	}
	if v.userID != "" {
		usr = v.userID
	}
	keep := ""
	if v.outcome == "quarantined" {
		keep = v.body
	}
	_, err := s.DB.Exec(ctx, `UPDATE mail_inbound SET outcome=$2,reason=$3,issue_id=$4,user_id=$5,body=$6 WHERE id=$1`, id, v.outcome, v.reason, iss, usr, keep)
	return InboundResult{ID: id, Outcome: v.outcome, Reason: v.reason}, err
}

func singleSender(h netmail.Header) (*netmail.Address, error) {
	as, err := h.AddressList("From")
	if err != nil || len(as) != 1 {
		return nil, errors.New("the message has no single sender address")
	}
	return as[0], nil
}

func ignore(reason string) verdict { return verdict{outcome: "ignored", reason: reason} }

// classifyInbound decides what to do with a parsed message. It changes nothing.
func (s *Service) classifyInbound(ctx context.Context, p *mail.Parsed, trusted bool, from *netmail.Address, ferr error, id int64, now time.Time, lim InboundLimits) verdict {
	h := p.Header
	// our own and automatic mail first: these never get a reply and never act
	switch {
	case h.Get("X-Tasklane-Loop") != "" || h.Get("X-Tasklane-Instance") != "":
		return ignore("mail sent by a Tasklane installation (loop protection)")
	case h.Get("Auto-Submitted") != "" && !strings.EqualFold(strings.TrimSpace(h.Get("Auto-Submitted")), "no"):
		return ignore("automatically generated (Auto-Submitted)")
	case h.Get("X-Auto-Response-Suppress") != "" || h.Get("X-Autoreply") != "" || h.Get("X-Autorespond") != "":
		return ignore("automatic reply")
	case regexp.MustCompile(`(?i)^(bulk|junk|list|auto_reply)$`).MatchString(strings.TrimSpace(h.Get("Precedence"))):
		return ignore("bulk or list mail (Precedence)")
	case h.Get("List-Id") != "" || h.Get("X-Mailing-List") != "":
		return ignore("mailing-list mail")
	case strings.TrimSpace(h.Get("Return-Path")) == "<>":
		return ignore("null return path (a bounce or an automatic message)")
	case strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "multipart/report"):
		return ignore("a delivery report (handled as a bounce)")
	case reOOO.MatchString(h.Get("Subject")):
		return ignore("out-of-office reply")
	}
	if ferr != nil {
		return verdict{outcome: "rejected", reason: ferr.Error()}
	}
	addr := strings.ToLower(from.Address)
	local, domain, _ := strings.Cut(addr, "@")
	if local == "mailer-daemon" || local == "postmaster" {
		return ignore("a bounce (sent by the mail system)")
	}
	// rate limits per sender and per sender domain; the current message is already counted
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE sender=$1 AND received_at>$2`, addr, now.Add(-time.Hour)).Scan(&n)
	if n > lim.PerSenderHour {
		return ignore("rate limit: too many messages from this sender")
	}
	s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE sender LIKE '%@'||$1 AND received_at>$2`, domain, now.Add(-time.Hour)).Scan(&n)
	if n > lim.PerDomainHour {
		return ignore("rate limit: too many messages from this domain")
	}
	body, _ := mail.StripQuote(p.Text)
	body = strings.TrimSpace(body)
	// who is it
	var v verdict
	err := s.DB.QueryRow(ctx, `SELECT id,name,kind,is_admin FROM app_user WHERE lower(email)=$1 AND active AND kind<>'agent'`, addr).Scan(&v.userID, &v.userName, &v.userKind, &v.userAdmin)
	if err != nil {
		return verdict{outcome: "quarantined", reason: "the sender is not a known user"}
	}
	v.body = body
	// proof of origin: only the receiving path's own verdict counts, and only from a source marked trusted
	if !trusted || !authenticated(h, domain) {
		v.outcome, v.reason = "quarantined", "no proof of origin: the sender's domain was not verified by a trusted receiver"
		s.locateIssue(ctx, p, &v, false)
		return v
	}
	if why := s.locateIssue(ctx, p, &v, true); why != "" {
		v.outcome, v.reason = "quarantined", why
		return v
	}
	if v.issueID == "" {
		return ignore("not a reply to a notification, and the subject names no issue")
	}
	var mirrored bool
	s.DB.QueryRow(ctx, `SELECT p.mirror_of IS NOT NULL FROM project p JOIN issue i ON i.project_id=p.id WHERE i.id=$1`, v.issueID).Scan(&mirrored)
	if mirrored {
		v.outcome, v.reason = "quarantined", "mirrored project"
		return v
	}
	s.DB.QueryRow(ctx, `SELECT count(*) FROM mail_inbound WHERE issue_id=$1 AND outcome='accepted' AND received_at>$2`, v.issueID, now.Add(-time.Hour)).Scan(&n)
	if n >= lim.PerIssueHour {
		return ignore("rate limit: too many messages for this issue")
	}
	if body == "" {
		return ignore("the message has no text")
	}
	v.outcome = "accepted"
	return v
}

// locateIssue finds the issue a message is about: the signed address first, then the threading headers, then the key in the
// subject. With strict it returns the reason when a token is present but wrong; without it the issue is only noted (for a
// message that is going to quarantine anyway).
func (s *Service) locateIssue(ctx context.Context, p *mail.Parsed, v *verdict, strict bool) string {
	h := p.Header
	dom := regexp.QuoteMeta(s.mailDomain())
	re := regexp.MustCompile(`(?i)reply\+([A-Za-z0-9_-]{20,80})@` + dom)
	var addrs []string
	for _, k := range []string{"To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To"} {
		addrs = append(addrs, h[netmailCanon(k)]...)
	}
	var tok string
	if m := re.FindStringSubmatch(strings.Join(addrs, ",")); m != nil {
		tok = m[1]
	}
	set := func(issueID string) bool {
		return s.DB.QueryRow(ctx, `SELECT id,key FROM issue WHERE id=$1 AND deleted_at IS NULL`, issueID).Scan(&v.issueID, &v.issueKey) == nil
	}
	if tok != "" {
		issue, err := s.verifyReply(ctx, tok, v.userID)
		if err != nil {
			return err.Error() // a forwarded or forged address, an expired one, a revoked key: quarantined, never acted on
		}
		if !set(issue) {
			return "the issue no longer exists"
		}
		return ""
	}
	ids := reMsgID.FindAllString(h.Get("In-Reply-To")+" "+h.Get("References"), -1)
	for i := len(ids) - 1; i >= 0; i-- {
		var issue *string
		if s.DB.QueryRow(ctx, `SELECT issue_id FROM mail_message WHERE message_id=$1 AND issue_id IS NOT NULL LIMIT 1`, ids[i]).Scan(&issue) == nil && issue != nil && set(*issue) {
			return ""
		}
	}
	if m := reSubjKey.FindStringSubmatch(h.Get("Subject")); m != nil {
		var iid string
		if s.DB.QueryRow(ctx, `SELECT id FROM issue WHERE (key=$1 OR id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1)) AND deleted_at IS NULL LIMIT 1`, strings.ToUpper(m[1])).Scan(&iid) == nil {
			set(iid)
		}
	}
	return ""
}

func netmailCanon(k string) string { return textproto.CanonicalMIMEHeaderKey(k) }

var reAuth = regexp.MustCompile(`(?i)(dkim|dmarc)=pass([^;]*)`)
var reHeaderD = regexp.MustCompile(`(?i)header\.(d|from)=([A-Za-z0-9.-]+)`)

// authenticated reads the Authentication-Results headers the trusted receiver added. DMARC pass counts as it stands (the
// receiver checked alignment); a DKIM pass counts only when the signing domain is the sender's domain or a parent of it
// on a label boundary, so a lookalike (example.com.evil.net, notexample.com) does not align.
func authenticated(h netmail.Header, fromDomain string) bool {
	for _, v := range h[netmailCanon("Authentication-Results")] {
		for _, m := range reAuth.FindAllStringSubmatch(v, -1) {
			if strings.EqualFold(m[1], "dmarc") {
				return true
			}
			if d := reHeaderD.FindStringSubmatch(m[2]); d != nil {
				dd := strings.ToLower(d[2])
				if dd == fromDomain || strings.HasSuffix(fromDomain, "."+dd) {
					return true
				}
			}
		}
	}
	return false
}

// ---------- administration ----------

type InboundRow struct {
	ID       int64     `json:"id"`
	Received time.Time `json:"received"`
	Sender   string    `json:"sender"`
	Subject  string    `json:"subject"`
	Outcome  string    `json:"outcome"`
	Reason   string    `json:"reason"`
	Issue    string    `json:"issue,omitempty"`
	Dupes    int       `json:"duplicates"`
	Raw      bool      `json:"raw"`
}

func (s *Service) ListInbound(ctx context.Context, a Actor, outcome string, limit int) ([]InboundRow, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT m.id,m.received_at,m.sender,m.subject,m.outcome,m.reason,COALESCE(i.key,''),m.dupes,m.raw_sha256 IS NOT NULL
		FROM mail_inbound m LEFT JOIN issue i ON i.id=m.issue_id WHERE ($1='' OR m.outcome=$1) ORDER BY m.id DESC LIMIT $2`, outcome, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboundRow{}
	for rows.Next() {
		var r InboundRow
		if err := rows.Scan(&r.ID, &r.Received, &r.Sender, &r.Subject, &r.Outcome, &r.Reason, &r.Issue, &r.Dupes, &r.Raw); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InboundRaw returns the stored original message (the .eml) for an administrator.
func (s *Service) InboundRaw(ctx context.Context, a Actor, id int64) ([]byte, error) {
	if !a.Admin {
		return nil, ErrForbidden
	}
	var sha *string
	if err := s.DB.QueryRow(ctx, `SELECT raw_sha256 FROM mail_inbound WHERE id=$1`, id).Scan(&sha); err != nil || sha == nil {
		return nil, ErrNotFound
	}
	o, err := s.OpenBlob(*sha)
	if err != nil {
		return nil, ErrNotFound
	}
	defer o.Close()
	return io.ReadAll(o)
}

// ReleaseInbound turns a quarantined message into its comment, once, as the user it was matched to and with that user's
// permissions: releasing never widens access. A message with no matched user or issue cannot be released, only deleted.
func (s *Service) ReleaseInbound(ctx context.Context, a Actor, id int64) (Comment, error) {
	if !a.Admin {
		return Comment{}, ErrForbidden
	}
	var issueID, userID, body string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE mail_inbound SET outcome='accepted',reason='released by an administrator',handled_by=$2,handled_at=$3
			WHERE id=$1 AND outcome='quarantined' AND issue_id IS NOT NULL AND user_id IS NOT NULL AND body<>'' RETURNING issue_id,user_id,body`, id, a.ID, s.Now()).Scan(&issueID, &userID, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			return invalid("only a quarantined message with a known sender, an issue and some text can be released")
		}
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "mail.inbound.release", fmt.Sprint(id), nil)
	})
	if err != nil {
		return Comment{}, err
	}
	revert := func(reason string) {
		s.DB.Exec(ctx, `UPDATE mail_inbound SET outcome='quarantined',reason=$2,handled_by=NULL,handled_at=NULL WHERE id=$1`, id, reason)
	}
	var key, name, kind string
	var admin bool
	if err := s.DB.QueryRow(ctx, `SELECT i.key,u.name,u.kind,u.is_admin FROM issue i, app_user u WHERE i.id=$1 AND u.id=$2 AND u.active`, issueID, userID).Scan(&key, &name, &kind, &admin); err != nil {
		revert("the sender or the issue no longer exists")
		return Comment{}, invalid("the sender or the issue no longer exists")
	}
	c, err := s.AddComment(ctx, Actor{ID: userID, Label: name, Admin: admin && kind != "agent", Kind: kind}, key, body, false)
	if err != nil {
		revert("release failed: the sender may not comment on that issue")
		return Comment{}, err
	}
	s.DB.Exec(ctx, `UPDATE comment SET mail_inbound_id=$2 WHERE id=$1`, c.ID, id)
	s.DB.Exec(ctx, `UPDATE mail_inbound SET comment_id=$2 WHERE id=$1`, id, c.ID)
	return c, nil
}

// DeleteInbound discards a quarantined message: its text is dropped, the row and the outcome stay for the record.
func (s *Service) DeleteInbound(ctx context.Context, a Actor, id int64) error {
	if !a.Admin {
		return ErrForbidden
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE mail_inbound SET outcome='rejected',reason='deleted by an administrator',body='',handled_by=$2,handled_at=$3 WHERE id=$1 AND outcome='quarantined'`, id, a.ID, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid("only a quarantined message can be deleted")
		}
		return s.audit(ctx, tx, a, "mail.inbound.delete", fmt.Sprint(id), nil)
	})
}

// SignInbound is the signature a source puts in X-Tasklane-Signature: "sha256=" and the HMAC-SHA256 of the body in hex.
func SignInbound(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}
