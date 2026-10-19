package domain

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"html"
	netmail "net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// The customer portal's identity (SPEC 15.1). A customer is someone who raises and follows requests: an email-based identity with a few
// profile fields, separate from staff accounts, with sessions that cannot reach the staff app, the REST API or MCP (they are a different
// table and a different cookie, and nothing in staff authentication looks at them). Customers are never seats.
//
// Signing in: a six digit code and a link are mailed to the address (single use, ten minutes, five tries); a customer may also set a
// password. Nothing says whether an address is known. Limits: codes per address and per network address, failures per address, a block
// list and an allow list per portal, and a hidden form field that people never fill.

const (
	portalCodeTTL      = 10 * time.Minute
	portalSessionTTL   = 12 * time.Hour
	portalCodeMaxTries = 5
	portalCodesPerHour = 5  // per address
	portalCodesPerIP   = 30 // per network address
	portalFailsPerHour = 10 // wrong codes or passwords per address
)

type Customer struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Language string `json:"language,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Org      string `json:"organization,omitempty"` // the customer organization (company), when linked
	HasPass  bool   `json:"has_password"`
	OrgID    string `json:"-"` // the organization the customer belongs to
}

type PortalInfo struct {
	Project string           `json:"project"`
	Name    string           `json:"name"`
	Welcome string           `json:"welcome,omitempty"`
	Privacy string           `json:"privacy_notice,omitempty"`
	Consent string           `json:"consent_text,omitempty"`
	Files   *PortalFileRules `json:"files,omitempty"` // present when customers may add files
}

// PortalFileRules is what the portal tells a customer about the files it takes.
type PortalFileRules struct {
	MaxMB    int      `json:"max_mb"`
	MaxFiles int      `json:"max_files"`
	Classes  []string `json:"classes"`
}

type PortalPolicy struct {
	Enabled bool     `json:"enabled"`
	Welcome string   `json:"welcome"`
	Privacy string   `json:"privacy_notice"`
	Consent string   `json:"consent_text"`
	Allow   []string `json:"allow"`
	Block   []string `json:"block"`
	// the rules for files customers add to their requests (off by default)
	AttachEnabled  bool     `json:"attach_enabled"`
	AttachMaxMB    int      `json:"attach_max_mb"`
	AttachMaxFiles int      `json:"attach_max_files"` // customer files per request
	AttachClasses  []string `json:"attach_classes"`   // image, document or log; archives and video are never accepted from customers
}

func randomToken(n int) string { return randURLToken(n) }

func sixDigits() string {
	b := make([]byte, 4)
	rand.Read(b)
	n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1000000
	return fmt.Sprintf("%06d", n)
}

func normEmail(e string) (string, error) {
	e = strings.TrimSpace(e)
	if len(e) > 254 {
		return "", invalid("that is not an email address")
	}
	a, err := netmail.ParseAddress(e)
	if err != nil || a.Address != e || !strings.Contains(e, "@") {
		return "", invalid("that is not an email address")
	}
	return strings.ToLower(e), nil
}

// portalOf finds the organization and project of an enabled portal; a portal that is off, or a project that does not exist, are the
// same: not found.
func (s *Service) portalOf(ctx context.Context, projectKey string) (org, pid string, set PortalPolicy, name string, err error) {
	var allow, block []string
	err = s.DB.QueryRow(ctx, `SELECT p.org_id::text,p.id::text,p.name,ps.enabled,ps.welcome,ps.privacy_notice,ps.consent_text,ps.allow,ps.block,ps.attach_enabled,ps.attach_max_mb,ps.attach_max_files,ps.attach_classes FROM project p JOIN portal_setting ps ON ps.project_id=p.id
		WHERE p.key=$1 AND NOT p.archived`, strings.ToUpper(projectKey)).Scan(&org, &pid, &name, &set.Enabled, &set.Welcome, &set.Privacy, &set.Consent, &allow, &block, &set.AttachEnabled, &set.AttachMaxMB, &set.AttachMaxFiles, &set.AttachClasses)
	if err != nil || !set.Enabled {
		return "", "", set, "", ErrNotFound
	}
	set.Allow, set.Block = allow, block
	return org, pid, set, name, nil
}

// PortalInfoFor is what the portal's sign-in page shows: the name, a welcome, the privacy notice and the consent text.
func (s *Service) PortalInfoFor(ctx context.Context, projectKey string) (PortalInfo, error) {
	_, _, set, name, err := s.portalOf(ctx, projectKey)
	if err != nil {
		return PortalInfo{}, err
	}
	info := PortalInfo{Project: strings.ToUpper(projectKey), Name: name, Welcome: set.Welcome, Privacy: set.Privacy, Consent: set.Consent}
	if set.AttachEnabled {
		info.Files = &PortalFileRules{MaxMB: set.AttachMaxMB, MaxFiles: set.AttachMaxFiles, Classes: set.AttachClasses}
	}
	return info, nil
}

func listed(list []string, email string) bool {
	at := strings.LastIndex(email, "@")
	for _, p := range list {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == email || (strings.HasPrefix(p, "@") && at >= 0 && email[at:] == p) {
			return true
		}
	}
	return false
}

func (p PortalPolicy) permits(email string) bool {
	if listed(p.Block, email) {
		return false
	}
	return len(p.Allow) == 0 || listed(p.Allow, email)
}

func normList(in []string) ([]string, error) {
	out := []string{}
	for _, x := range in {
		x = strings.ToLower(strings.TrimSpace(x))
		if x == "" {
			continue
		}
		if strings.HasPrefix(x, "@") {
			if len(x) < 3 || strings.Contains(x[1:], "@") || strings.ContainsAny(x, " ,;") {
				return nil, invalid("%q is not a domain (write @example.com)", x)
			}
		} else if _, err := normEmail(x); err != nil {
			return nil, invalid("%q is not an address or a domain", x)
		}
		out = append(out, x)
	}
	if len(out) > 500 {
		return nil, invalid("a list holds at most 500 entries")
	}
	return out, nil
}

// GetPortalPolicy and SetPortalPolicy are the portal's settings (project administrators).
func (s *Service) GetPortalPolicy(ctx context.Context, a Actor, projectKey string) (PortalPolicy, error) {
	pid, err := s.projectID(ctx, projectKey)
	if err != nil {
		return PortalPolicy{}, err
	}
	switch s.role(ctx, a, pid) {
	case "":
		return PortalPolicy{}, ErrNotFound
	case "admin":
	default:
		return PortalPolicy{}, ErrForbidden
	}
	p := PortalPolicy{Allow: []string{}, Block: []string{}, AttachMaxMB: 10, AttachMaxFiles: 5, AttachClasses: []string{"image", "document", "log"}}
	err = s.DB.QueryRow(ctx, `SELECT enabled,welcome,privacy_notice,consent_text,allow,block,attach_enabled,attach_max_mb,attach_max_files,attach_classes FROM portal_setting WHERE project_id=$1`, pid).
		Scan(&p.Enabled, &p.Welcome, &p.Privacy, &p.Consent, &p.Allow, &p.Block, &p.AttachEnabled, &p.AttachMaxMB, &p.AttachMaxFiles, &p.AttachClasses)
	if err != nil && err != pgx.ErrNoRows {
		return p, err
	}
	return p, nil
}

func (s *Service) SetPortalPolicy(ctx context.Context, a Actor, projectKey string, in PortalPolicy) (PortalPolicy, error) {
	if _, err := s.GetPortalPolicy(ctx, a, projectKey); err != nil {
		return PortalPolicy{}, err
	}
	pid, _ := s.projectID(ctx, projectKey)
	var err error
	if in.Allow, err = normList(in.Allow); err != nil {
		return PortalPolicy{}, err
	}
	if in.Block, err = normList(in.Block); err != nil {
		return PortalPolicy{}, err
	}
	for _, t := range []string{in.Welcome, in.Privacy, in.Consent} {
		if len([]rune(t)) > 5000 {
			return PortalPolicy{}, invalid("a portal text is at most 5000 characters")
		}
	}
	if in.AttachMaxMB == 0 {
		in.AttachMaxMB = 10
	}
	if in.AttachMaxFiles == 0 {
		in.AttachMaxFiles = 5
	}
	if in.AttachClasses == nil {
		in.AttachClasses = []string{"image", "document", "log"}
	}
	if in.AttachMaxMB < 1 || in.AttachMaxMB > 100 {
		return PortalPolicy{}, invalid("the file size limit is between 1 and 100 MB")
	}
	if in.AttachMaxFiles < 1 || in.AttachMaxFiles > 20 {
		return PortalPolicy{}, invalid("the number of files per request is between 1 and 20")
	}
	for _, c := range in.AttachClasses {
		if c != "image" && c != "document" && c != "log" {
			return PortalPolicy{}, invalid("customers may be allowed images, documents and logs; %q is not one of them", c)
		}
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO portal_setting(project_id,enabled,welcome,privacy_notice,consent_text,allow,block,attach_enabled,attach_max_mb,attach_max_files,attach_classes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (project_id) DO UPDATE SET enabled=$2,welcome=$3,privacy_notice=$4,consent_text=$5,allow=$6,block=$7,attach_enabled=$8,attach_max_mb=$9,attach_max_files=$10,attach_classes=$11`,
		pid, in.Enabled, in.Welcome, in.Privacy, in.Consent, in.Allow, in.Block, in.AttachEnabled, in.AttachMaxMB, in.AttachMaxFiles, in.AttachClasses)
	return in, err
}

func (s *Service) portalCount(ctx context.Context, kind, subject string, within time.Duration) int {
	var n int
	s.DB.QueryRow(ctx, `SELECT count(*) FROM portal_event WHERE kind=$1 AND subject=$2 AND at > $3`, kind, subject, s.Now().Add(-within)).Scan(&n)
	return n
}

func (s *Service) portalNote(ctx context.Context, kind, subject string) {
	s.DB.Exec(ctx, `INSERT INTO portal_event(kind,subject,at) VALUES($1,$2,$3)`, kind, subject, s.Now())
	s.DB.Exec(ctx, `DELETE FROM portal_event WHERE at < $1`, s.Now().Add(-24*time.Hour))
}

// PortalRequestCode mails a sign-in code and link to the address. The answer is the same whether or not the address is known or allowed;
// only a limit says so (429), and a filled honeypot gets the same answer as success and sends nothing.
func (s *Service) PortalRequestCode(ctx context.Context, projectKey, email, honeypot string, meta ClientMeta) error {
	org, _, set, name, err := s.portalOf(ctx, projectKey)
	if err != nil {
		return err
	}
	addr, err := normEmail(email)
	if err != nil {
		return err
	}
	if honeypot != "" {
		return nil
	}
	if s.portalCount(ctx, "code", addr, time.Hour) >= portalCodesPerHour || (meta.IP != "" && s.portalCount(ctx, "code-ip", meta.IP, time.Hour) >= portalCodesPerIP) {
		return &LockedError{Until: s.Now().Add(time.Hour)}
	}
	s.portalNote(ctx, "code", addr)
	if meta.IP != "" {
		s.portalNote(ctx, "code-ip", meta.IP)
	}
	if !set.permits(addr) {
		return nil
	}
	var blocked bool
	s.DB.QueryRow(ctx, `SELECT blocked FROM customer WHERE org_id::text=$1 AND lower(email)=$2`, org, addr).Scan(&blocked)
	if blocked {
		return nil
	}
	if s.Mail == nil {
		return invalid("this server cannot send email, so the portal cannot sign anyone in")
	}
	code, link := sixDigits(), randomToken(24)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE customer_code SET used_at=$3 WHERE org_id::text=$1 AND lower(email)=$2 AND used_at IS NULL`, org, addr, s.Now()); err != nil {
		return err
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO customer_code(org_id,email,code_hash,link_hash,expires_at) VALUES($1,$2,$3,$4,$5) RETURNING id`, org, addr, hashSecret(code), hashSecret(link), s.Now().Add(portalCodeTTL)).Scan(&id); err != nil {
		return err
	}
	url := strings.TrimRight(s.Mail.BaseURL, "/") + "/portal/" + strings.ToUpper(projectKey) + "?token=" + link
	text := fmt.Sprintf("-- Reply above this line --\r\n\r\nYour code for the %s portal is %s\r\nIt works once and expires in 10 minutes.\r\nOr open this link: %s\r\n\r\nIf you did not ask for this, ignore this message.\r\n", name, code, url)
	htm := `<div style="font-family:system-ui,sans-serif;font-size:14px"><p>Your code for the ` + html.EscapeString(name) + ` portal is <b>` + code + `</b>. It works once and expires in 10 minutes.</p><p><a href="` + html.EscapeString(url) + `">Sign in with this link</a></p><p>If you did not ask for this, ignore this message.</p></div>`
	if _, err := tx.Exec(ctx, `INSERT INTO mail_message(idempotency_key,recipient,subject,message_id,body_text,body_html,kind) VALUES($1,$2,$3,$4,$5,$6,'portal-code') ON CONFLICT DO NOTHING`,
		fmt.Sprintf("portal-code:%d", id), addr, "Your sign-in code", fmt.Sprintf("<portal-code.%d@%s>", id, s.Mail.Domain), text, htm); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) customerRow(ctx context.Context, q querier, where string, args ...any) (Customer, bool, error) {
	var c Customer
	var pass *string
	var blocked bool
	err := q.QueryRow(ctx, `SELECT c.id::text,c.email,c.name,c.language,c.timezone,COALESCE(o.name,''),c.password_hash,c.blocked,c.org_id::text FROM customer c LEFT JOIN customer_org o ON o.id=c.customer_org_id WHERE `+where, args...).Scan(&c.ID, &c.Email, &c.Name, &c.Language, &c.Timezone, &c.Org, &pass, &blocked, &c.OrgID)
	if err == pgx.ErrNoRows {
		return c, false, ErrNotFound
	}
	c.HasPass = pass != nil
	return c, blocked, err
}

// openCustomer finds or creates the customer for a verified address and starts their session.
func (s *Service) openCustomer(ctx context.Context, tx pgx.Tx, org, addr string) (string, Customer, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO customer(org_id,email,verified_at,last_seen) VALUES($1,$2,$3,$3) ON CONFLICT (org_id, lower(email)) DO UPDATE SET verified_at=COALESCE(customer.verified_at,$3), last_seen=$3 RETURNING id::text`, org, addr, s.Now()).Scan(&id)
	if err != nil {
		return "", Customer{}, err
	}
	c, blocked, err := s.customerRow(ctx, tx, `c.id::text=$1`, id)
	if err != nil {
		return "", c, err
	}
	if blocked {
		return "", c, ErrForbidden
	}
	tok := randomToken(32)
	if _, err := tx.Exec(ctx, `INSERT INTO customer_session(token_hash,customer_id,expires_at) VALUES($1,$2,$3)`, hashSecret(tok), id, s.Now().Add(portalSessionTTL)); err != nil {
		return "", c, err
	}
	return tok, c, nil
}

var errPortalSignIn = &InvalidError{Msg: "that code is not right, or it has expired"}

// PortalVerify redeems a code for a session. A code works once, for ten minutes, and five wrong tries spoil it.
func (s *Service) PortalVerify(ctx context.Context, projectKey, email, code string, meta ClientMeta) (string, Customer, error) {
	org, _, set, _, err := s.portalOf(ctx, projectKey)
	if err != nil {
		return "", Customer{}, err
	}
	addr, err := normEmail(email)
	if err != nil {
		return "", Customer{}, errPortalSignIn
	}
	if s.portalCount(ctx, "fail", addr, time.Hour) >= portalFailsPerHour {
		return "", Customer{}, &LockedError{Until: s.Now().Add(time.Hour)}
	}
	if !set.permits(addr) {
		s.portalNote(ctx, "fail", addr)
		return "", Customer{}, errPortalSignIn
	}
	var tok string
	var c Customer
	var bad bool
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var id int64
		var hash []byte
		var tries int
		e := tx.QueryRow(ctx, `SELECT id,code_hash,attempts FROM customer_code WHERE org_id::text=$1 AND lower(email)=$2 AND used_at IS NULL AND expires_at > $3 ORDER BY id DESC LIMIT 1 FOR UPDATE`, org, addr, s.Now()).Scan(&id, &hash, &tries)
		if e == pgx.ErrNoRows {
			bad = true
			return nil
		}
		if e != nil {
			return e
		}
		if subtle.ConstantTimeCompare(hash, hashSecret(strings.TrimSpace(code))) != 1 {
			if tries+1 >= portalCodeMaxTries {
				tx.Exec(ctx, `UPDATE customer_code SET used_at=$2, attempts=attempts+1 WHERE id=$1`, id, s.Now())
			} else {
				tx.Exec(ctx, `UPDATE customer_code SET attempts=attempts+1 WHERE id=$1`, id)
			}
			bad = true
			return nil
		}
		if _, e := tx.Exec(ctx, `UPDATE customer_code SET used_at=$2 WHERE id=$1`, id, s.Now()); e != nil {
			return e
		}
		tok, c, e = s.openCustomer(ctx, tx, org, addr)
		return e
	})
	if bad {
		s.portalNote(ctx, "fail", addr)
		return "", Customer{}, errPortalSignIn
	}
	return tok, c, err
}

// PortalVerifyLink redeems the link token from the email.
func (s *Service) PortalVerifyLink(ctx context.Context, token string, meta ClientMeta) (string, Customer, error) {
	var org, addr string
	var id int64
	err := s.DB.QueryRow(ctx, `SELECT cc.id,cc.org_id::text,cc.email FROM customer_code cc WHERE cc.link_hash=$1 AND cc.used_at IS NULL AND cc.expires_at > $2`, hashSecret(token), s.Now()).Scan(&id, &org, &addr)
	if err != nil {
		if ip := meta.IP; ip != "" {
			if s.portalCount(ctx, "fail-ip", ip, time.Hour) >= portalFailsPerHour*3 {
				return "", Customer{}, &LockedError{Until: s.Now().Add(time.Hour)}
			}
			s.portalNote(ctx, "fail-ip", ip)
		}
		return "", Customer{}, errPortalSignIn
	}
	var tok string
	var c Customer
	err = s.tx(ctx, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `UPDATE customer_code SET used_at=$2 WHERE id=$1 AND used_at IS NULL`, id, s.Now())
		if e != nil {
			return e
		}
		if tag.RowsAffected() == 0 {
			return errPortalSignIn
		}
		tok, c, e = s.openCustomer(ctx, tx, org, addr)
		return e
	})
	return tok, c, err
}

// PortalLoginPassword signs in with a password the customer chose; the failure limit is the same as for codes and nothing says whether
// the address exists.
func (s *Service) PortalLoginPassword(ctx context.Context, projectKey, email, password string, meta ClientMeta) (string, Customer, error) {
	org, _, set, _, err := s.portalOf(ctx, projectKey)
	if err != nil {
		return "", Customer{}, err
	}
	addr, err := normEmail(email)
	if err != nil {
		return "", Customer{}, errPortalSignIn
	}
	if s.portalCount(ctx, "fail", addr, time.Hour) >= portalFailsPerHour {
		return "", Customer{}, &LockedError{Until: s.Now().Add(time.Hour)}
	}
	var hash *string
	var blocked bool
	s.DB.QueryRow(ctx, `SELECT password_hash,blocked FROM customer WHERE org_id::text=$1 AND lower(email)=$2`, org, addr).Scan(&hash, &blocked)
	if hash == nil {
		bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5BUAdfHnVbCySeS0W5iuEQ2sYD8mS"), []byte(password)) // the same time whether or not there is a customer
	}
	if hash == nil || blocked || !set.permits(addr) || bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)) != nil {
		s.portalNote(ctx, "fail", addr)
		return "", Customer{}, &InvalidError{Msg: "the address or password is not right"}
	}
	var tok string
	var c Customer
	err = s.tx(ctx, func(tx pgx.Tx) error {
		tok, c, err = s.openCustomer(ctx, tx, org, addr)
		return err
	})
	return tok, c, err
}

// CustomerByToken is the portal's authentication: a customer session, and nothing else, opens it.
func (s *Service) CustomerByToken(ctx context.Context, token string) (Customer, string, error) {
	if token == "" {
		return Customer{}, "", ErrForbidden
	}
	var id, org string
	err := s.DB.QueryRow(ctx, `SELECT c.id::text,c.org_id::text FROM customer_session cs JOIN customer c ON c.id=cs.customer_id WHERE cs.token_hash=$1 AND cs.expires_at > $2 AND NOT c.blocked`, hashSecret(token), s.Now()).Scan(&id, &org)
	if err != nil {
		return Customer{}, "", ErrForbidden
	}
	c, _, err := s.customerRow(ctx, s.DB, `c.id::text=$1`, id)
	c.OrgID = org
	return c, org, err
}

func (s *Service) PortalLogout(ctx context.Context, token string) {
	s.DB.Exec(ctx, `DELETE FROM customer_session WHERE token_hash=$1`, hashSecret(token))
}

// UpdateCustomer changes the profile: name, language, time zone.
func (s *Service) UpdateCustomer(ctx context.Context, c Customer, name, language, timezone string) (Customer, error) {
	if len([]rune(name)) > 100 || len(language) > 20 || len(timezone) > 60 {
		return c, invalid("that is too long")
	}
	if timezone != "" {
		if _, err := time.LoadLocation(timezone); err != nil {
			return c, invalid("that is not a time zone (for example Europe/Prague)")
		}
	}
	if _, err := s.DB.Exec(ctx, `UPDATE customer SET name=$2,language=$3,timezone=$4 WHERE id::text=$1`, c.ID, strings.TrimSpace(name), language, timezone); err != nil {
		return c, err
	}
	out, _, err := s.customerRow(ctx, s.DB, `c.id::text=$1`, c.ID)
	return out, err
}

// SetCustomerPassword sets (or, with an empty password, removes) the customer's own password; the password policy applies.
func (s *Service) SetCustomerPassword(ctx context.Context, c Customer, password string) error {
	if password == "" {
		_, err := s.DB.Exec(ctx, `UPDATE customer SET password_hash=NULL WHERE id::text=$1`, c.ID)
		return err
	}
	if err := s.checkPassword(c.Email, password); err != nil {
		return err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE customer SET password_hash=$2 WHERE id::text=$1`, c.ID, string(h))
	return err
}

// BlockCustomer stops a customer signing in and ends their sessions (project administrators of a portal in the customer's organization).
func (s *Service) BlockCustomer(ctx context.Context, a Actor, projectKey, email string, block bool) error {
	if _, err := s.GetPortalPolicy(ctx, a, projectKey); err != nil {
		return err
	}
	addr, err := normEmail(email)
	if err != nil {
		return err
	}
	pid, _ := s.projectID(ctx, projectKey)
	tag, err := s.DB.Exec(ctx, `UPDATE customer SET blocked=$3 WHERE lower(email)=$2 AND org_id=(SELECT org_id FROM project WHERE id::text=$1)`, pid, addr, block)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if block {
		s.DB.Exec(ctx, `DELETE FROM customer_session WHERE customer_id IN (SELECT id FROM customer WHERE lower(email)=$1 AND org_id=(SELECT org_id FROM project WHERE id::text=$2))`, addr, pid)
	}
	return nil
}
