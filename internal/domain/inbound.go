package domain

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
	create             *createSpec // set when the message would open a new issue instead of commenting
}

// createSpec is what a message addressed to a project would create. It is kept with a quarantined message so a release does the same.
type createSpec struct {
	Project string   `json:"project"`
	Type    string   `json:"type"`
	Labels  []string `json:"labels,omitempty"`
	Summary string   `json:"summary"`
	CC      []string `json:"cc,omitempty"` // user ids who will watch the new issue
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
	v := s.classifyInbound(ctx, p, raw, trusted, from, ferr, id, now, lim)
	if v.outcome == "accepted" && v.create != nil {
		act := Actor{ID: v.userID, Label: v.userName, Admin: v.userAdmin && v.userKind != "agent", Kind: v.userKind}
		key, err := s.createFromMail(ctx, act, *v.create, v.body)
		if err != nil {
			v.outcome, v.reason = "quarantined", "the issue was refused: "+err.Error()
		} else {
			v.issueKey = key
			s.DB.QueryRow(ctx, `SELECT id FROM issue WHERE key=$1`, key).Scan(&v.issueID)
		}
	} else if v.outcome == "accepted" {
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
	kind, detail := "reply", []byte("{}")
	if v.create != nil {
		kind = "create"
		detail, _ = json.Marshal(v.create)
	}
	_, err := s.DB.Exec(ctx, `UPDATE mail_inbound SET outcome=$2,reason=$3,issue_id=$4,user_id=$5,body=$6,kind=$7,detail=$8 WHERE id=$1`, id, v.outcome, v.reason, iss, usr, keep, kind, detail)
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
func (s *Service) classifyInbound(ctx context.Context, p *mail.Parsed, raw []byte, trusted bool, from *netmail.Address, ferr error, id int64, now time.Time, lim InboundLimits) verdict {
	h := p.Header
	// a bounce or a complaint is handled as one, before anything that would ignore it as automatic mail
	if bv, ok := s.classifyBounce(ctx, p, raw); ok {
		return bv
	}
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
		if pk, policy := s.projectAddressed(ctx, h); pk != "" && policy == "reject" {
			return verdict{outcome: "rejected", reason: "the sender is not a known user and project " + pk + " rejects mail from unknown senders"}
		}
		return verdict{outcome: "quarantined", reason: "the sender is not a known user"}
	}
	v.body = body
	// proof of origin: only the receiving path's own verdict counts, and only from a source marked trusted
	proven := trusted && authenticated(h, domain)
	why := "the sender's domain was not verified by a trusted receiver and the message carries no DKIM signature"
	if !proven && s.DNS != nil { // Tasklane checks the signature itself, whatever the transport
		proven, why = s.dkimProof(ctx, raw, domain, now)
	}
	if !proven {
		v.outcome, v.reason = "quarantined", "no proof of origin: "+why
		s.locateIssue(ctx, p, &v, false)
		if v.issueID == "" {
			s.locateCreate(ctx, p, &v)
		}
		return v
	}
	if why := s.locateIssue(ctx, p, &v, true); why != "" {
		v.outcome, v.reason = "quarantined", why
		return v
	}
	if v.issueID == "" {
		if !s.locateCreate(ctx, p, &v) {
			return ignore("not a reply to a notification, the subject names no issue and no project address was used")
		}
		var mirrored bool
		s.DB.QueryRow(ctx, `SELECT mirror_of IS NOT NULL FROM project WHERE key=$1`, v.create.Project).Scan(&mirrored)
		if mirrored {
			v.outcome, v.reason = "quarantined", "mirrored project"
			return v
		}
		v.outcome = "accepted"
		return v
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
		issue = nil // a message that created an issue is the root of its thread: replies to it continue there
		if s.DB.QueryRow(ctx, `SELECT issue_id FROM mail_inbound WHERE message_id=$1 AND issue_id IS NOT NULL AND kind='create' AND outcome='accepted'`, ids[i]).Scan(&issue) == nil && issue != nil && set(*issue) {
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
	var issueID, userID, body, kind string
	var detail []byte
	err := s.tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE mail_inbound SET outcome='accepted',reason='released by an administrator',handled_by=$2,handled_at=$3
			WHERE id=$1 AND outcome='quarantined' AND user_id IS NOT NULL AND ((kind='reply' AND issue_id IS NOT NULL AND body<>'') OR (kind='create' AND detail<>'{}'))
			RETURNING COALESCE(issue_id::text,''),user_id,body,kind,detail`, id, a.ID, s.Now()).Scan(&issueID, &userID, &body, &kind, &detail)
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
	var name, ukind string
	var admin bool
	if kind == "create" {
		if err := s.DB.QueryRow(ctx, `SELECT name,kind,is_admin FROM app_user WHERE id=$1 AND active`, userID).Scan(&name, &ukind, &admin); err != nil {
			revert("the sender no longer exists")
			return Comment{}, invalid("the sender no longer exists")
		}
		var spec createSpec
		json.Unmarshal(detail, &spec)
		key, err := s.createFromMail(ctx, Actor{ID: userID, Label: name, Admin: admin && ukind != "agent", Kind: ukind}, spec, body)
		if err != nil {
			revert("release failed: " + err.Error())
			return Comment{}, err
		}
		s.DB.Exec(ctx, `UPDATE mail_inbound SET issue_id=(SELECT id FROM issue WHERE key=$2) WHERE id=$1`, id, key)
		return Comment{Body: key}, nil // the issue's key is what a release of a new-issue message returns
	}
	var key string
	if err := s.DB.QueryRow(ctx, `SELECT i.key,u.name,u.kind,u.is_admin FROM issue i, app_user u WHERE i.id=$1 AND u.id=$2 AND u.active`, issueID, userID).Scan(&key, &name, &ukind, &admin); err != nil {
		revert("the sender or the issue no longer exists")
		return Comment{}, invalid("the sender or the issue no longer exists")
	}
	c, err := s.AddComment(ctx, Actor{ID: userID, Label: name, Admin: admin && ukind != "agent", Kind: ukind}, key, body, false)
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

// ---------- create issue by email ----------

var reAddrLocal = regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9]{0,9})((?:\+[A-Za-z0-9_-]+)*)@`)

// projectAddressed finds the project a message is addressed to ("KEY@domain", "KEY+tag@domain") and its policy for unknown senders.
func (s *Service) projectAddressed(ctx context.Context, h netmail.Header) (key, policy string) {
	k, _, _, pol := s.projectAddress(ctx, h)
	return k, pol
}

func (s *Service) projectAddress(ctx context.Context, h netmail.Header) (key string, tags []string, cc []string, policy string) {
	dom := s.mailDomain()
	var addrs []*netmail.Address
	for _, f := range []string{"To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To"} {
		for _, v := range h[netmailCanon(f)] {
			if l, err := netmail.ParseAddressList(v); err == nil {
				addrs = append(addrs, l...)
			}
		}
	}
	for _, a := range addrs {
		local, d, ok := strings.Cut(a.Address, "@")
		if !ok || !strings.EqualFold(d, dom) {
			continue
		}
		m := reAddrLocal.FindStringSubmatch(local + "@")
		if m == nil || strings.EqualFold(m[1], "reply") {
			continue
		}
		var pk, pol string
		if s.DB.QueryRow(ctx, `SELECT key,mail_policy FROM project WHERE key=$1 AND NOT archived`, strings.ToUpper(m[1])).Scan(&pk, &pol) != nil {
			continue
		}
		for _, t := range strings.Split(strings.TrimPrefix(m[2], "+"), "+") {
			if t != "" {
				tags = append(tags, t)
			}
		}
		return pk, tags, nil, pol
	}
	return "", nil, nil, ""
}

// locateCreate decides, for a message that is not a reply, whether it opens an issue: the plus-tags give the type (the first one that
// names an issue type) and the labels (the rest), the subject is the summary, and the Cc addresses that are users become watchers.
func (s *Service) locateCreate(ctx context.Context, p *mail.Parsed, v *verdict) bool {
	pk, tags, _, _ := s.projectAddress(ctx, p.Header)
	if pk == "" {
		return false
	}
	spec := &createSpec{Project: pk, Type: "Task"}
	typed := false
	for _, t := range tags {
		var name string
		if !typed && s.DB.QueryRow(ctx, `SELECT name FROM issue_type WHERE lower(name)=lower($1)`, t).Scan(&name) == nil {
			spec.Type, typed = name, true
			continue
		}
		spec.Labels = append(spec.Labels, strings.ToLower(t))
	}
	sub := strings.TrimSpace(p.Header.Get("Subject"))
	if d, err := (&mime.WordDecoder{}).DecodeHeader(sub); err == nil {
		sub = d
	}
	sub = strings.Join(strings.Fields(sub), " ")
	for { // "Fwd: Re: x" opens an issue called x
		l := strings.ToLower(sub)
		if strings.HasPrefix(l, "re:") || strings.HasPrefix(l, "fw:") {
			sub = strings.TrimSpace(sub[3:])
		} else if strings.HasPrefix(l, "fwd:") {
			sub = strings.TrimSpace(sub[4:])
		} else {
			break
		}
	}
	if sub == "" {
		sub = "(no subject)"
	}
	if r := []rune(sub); len(r) > 200 {
		sub = string(r[:200])
	}
	spec.Summary = sub
	seen := map[string]bool{v.userID: true}
	for _, f := range []string{"To", "Cc"} {
		for _, hv := range p.Header[netmailCanon(f)] {
			l, err := netmail.ParseAddressList(hv)
			if err != nil {
				continue
			}
			for _, a := range l {
				var uid string
				if s.DB.QueryRow(ctx, `SELECT id FROM app_user WHERE lower(email)=lower($1) AND active AND kind<>'agent'`, a.Address).Scan(&uid) == nil && !seen[uid] {
					seen[uid] = true
					spec.CC = append(spec.CC, uid)
				}
			}
		}
	}
	v.create = spec
	v.body = strings.TrimSpace(p.Text) // a new message is kept whole: there is no history to strip
	return true
}

// createFromMail opens the issue as the sender (their Create Issues permission applies), makes the Cc'd users watchers (each only if
// they can see the issue) and returns its key.
func (s *Service) createFromMail(ctx context.Context, a Actor, c createSpec, body string) (string, error) {
	is, err := s.CreateIssue(ctx, a, CreateIn{Project: c.Project, Type: c.Type, Summary: c.Summary, Description: body, Labels: c.Labels})
	if err != nil {
		return "", err
	}
	for _, uid := range c.CC {
		var name, kind string
		var admin bool
		if s.DB.QueryRow(ctx, `SELECT name,kind,is_admin FROM app_user WHERE id=$1 AND active`, uid).Scan(&name, &kind, &admin) == nil {
			s.SetWatch(ctx, Actor{ID: uid, Label: name, Admin: admin && kind != "agent", Kind: kind}, is.Key, true)
		}
	}
	return is.Key, nil
}

// ---------- project mail settings ----------

type ProjectMail struct {
	Address string `json:"address"`
	Policy  string `json:"unknown_sender_policy"`
}

// ProjectMail is the address that opens issues in a project and what happens to mail from people who are not users.
func (s *Service) ProjectMail(ctx context.Context, a Actor, key string) (ProjectMail, error) {
	pid, err := s.projectID(ctx, key)
	if err != nil {
		return ProjectMail{}, ErrNotFound
	}
	if s.role(ctx, a, pid) == "" {
		return ProjectMail{}, ErrNotFound
	}
	var pm ProjectMail
	if err := s.DB.QueryRow(ctx, `SELECT key,mail_policy FROM project WHERE id::text=$1`, pid).Scan(&pm.Address, &pm.Policy); err != nil {
		return pm, err
	}
	if d := s.mailDomain(); d != "" {
		pm.Address = strings.ToLower(pm.Address) + "@" + d
	} else {
		pm.Address = ""
	}
	return pm, nil
}

// SetMailPolicy chooses what mail from an unknown sender does: "quarantine" (an administrator decides) or "reject".
func (s *Service) SetMailPolicy(ctx context.Context, a Actor, key, policy string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	if policy != "quarantine" && policy != "reject" {
		return invalid("the policy for unknown senders is quarantine or reject")
	}
	pid, err := s.projectID(ctx, key)
	if err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE project SET mail_policy=$2 WHERE id::text=$1`, pid, policy); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "project.mail_policy", strings.ToUpper(key), map[string]any{"policy": policy})
	})
}

// dkimProof verifies the DKIM signatures on the stored raw message. Proof is a valid signature whose domain is aligned with the
// sender's domain. When there is none, the reason says whether there was no signature or what was wrong with the ones present.
func (s *Service) dkimProof(ctx context.Context, raw []byte, fromDomain string, now time.Time) (bool, string) {
	rs := mail.VerifyDKIM(ctx, raw, s.DNS, now)
	if len(rs) == 0 {
		return false, "the sender's domain was not verified by a trusted receiver and the message carries no DKIM signature"
	}
	why := ""
	for _, r := range rs {
		switch {
		case r.Pass && mail.Aligned(r.Domain, fromDomain):
			return true, ""
		case r.Pass:
			why += fmt.Sprintf("; the signature by %s is valid but is not aligned with the sender's domain %s", r.Domain, fromDomain)
		default:
			why += fmt.Sprintf("; the DKIM signature by %s failed: %s", r.Domain, r.Reason)
		}
	}
	return false, strings.TrimPrefix(why, "; ")
}
