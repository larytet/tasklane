package domain

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/mail"
)

// IMAP sources (MAIL-09). A mailbox is read by UID from a checkpoint; each message is handed to IngestMail (which stores the
// raw message before anything else and ignores a Message-ID it has seen), and the checkpoint moves only after that, so a crash
// at any point loses nothing and at worst reads one message twice, which the Message-ID makes harmless. A change of
// UIDVALIDITY means the old UIDs are meaningless: the mailbox is read again from the start, and the duplicates are ignored.

type IMAPIn struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLS      string `json:"tls"`
	User     string `json:"username"`
	Password string `json:"password"`
	Mailbox  string `json:"mailbox"`
	Trusted  bool   `json:"trusted"`
}

type IMAPAccount struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Host     string     `json:"host"`
	Port     int        `json:"port"`
	TLS      string     `json:"tls"`
	User     string     `json:"username"`
	Mailbox  string     `json:"mailbox"`
	Trusted  bool       `json:"trusted"`
	Enabled  bool       `json:"enabled"`
	LastUID  int64      `json:"last_uid"`
	Resets   int        `json:"uidvalidity_resets"`
	Received int64      `json:"received"`
	LastPoll *time.Time `json:"last_poll"`
	LastOK   *time.Time `json:"last_ok"`
	Error    string     `json:"last_error"`
}

func (s *Service) CreateIMAP(ctx context.Context, a Actor, in IMAPIn) (IMAPAccount, error) {
	if err := adminOnly(a); err != nil {
		return IMAPAccount{}, err
	}
	in.Name, in.Host, in.User = strings.TrimSpace(in.Name), strings.TrimSpace(in.Host), strings.TrimSpace(in.User)
	if in.Mailbox == "" {
		in.Mailbox = "INBOX"
	}
	if in.TLS == "" {
		in.TLS = "tls"
	}
	switch {
	case in.Name == "" || in.Host == "" || in.User == "" || in.Password == "":
		return IMAPAccount{}, invalid("a mailbox needs a name, host, user name and password")
	case in.Port < 1 || in.Port > 65535:
		return IMAPAccount{}, invalid("the port is between 1 and 65535")
	case in.TLS != "tls" && in.TLS != "none":
		return IMAPAccount{}, invalid("tls is tls or none (none is accepted only for a server on this machine)")
	case strings.ContainsAny(in.User+in.Password+in.Mailbox+in.Host, "\r\n\x00"):
		return IMAPAccount{}, invalid("a line break is not allowed in a setting")
	}
	sealed, err := s.seal(ctx, in.Password)
	if err != nil {
		return IMAPAccount{}, err
	}
	var id string
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO mail_imap(name,host,port,tls,username,password,mailbox,trusted) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			in.Name, in.Host, in.Port, in.TLS, in.User, sealed, in.Mailbox, in.Trusted).Scan(&id); err != nil {
			return invalid("a mailbox with that name exists")
		}
		return s.audit(ctx, tx, a, "mail.imap.create", id, map[string]any{"name": in.Name, "host": in.Host, "trusted": in.Trusted})
	})
	if err != nil {
		return IMAPAccount{}, err
	}
	return s.imapAccount(ctx, id)
}

const imapCols = `id,name,host,port,tls,username,mailbox,trusted,enabled,last_uid,resets,received,last_poll,last_ok,last_error`

func scanIMAP(r pgx.Row) (IMAPAccount, error) {
	var x IMAPAccount
	err := r.Scan(&x.ID, &x.Name, &x.Host, &x.Port, &x.TLS, &x.User, &x.Mailbox, &x.Trusted, &x.Enabled, &x.LastUID, &x.Resets, &x.Received, &x.LastPoll, &x.LastOK, &x.Error)
	return x, err
}

func (s *Service) imapAccount(ctx context.Context, id string) (IMAPAccount, error) {
	x, err := scanIMAP(s.DB.QueryRow(ctx, `SELECT `+imapCols+` FROM mail_imap WHERE id::text=$1`, id))
	if err != nil {
		return x, ErrNotFound
	}
	return x, nil
}

func (s *Service) ListIMAP(ctx context.Context, a Actor) ([]IMAPAccount, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT `+imapCols+` FROM mail_imap ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IMAPAccount{}
	for rows.Next() {
		x, err := scanIMAP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Service) DeleteIMAP(ctx context.Context, a Actor, id string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM mail_imap WHERE id::text=$1`, id)
		if err != nil || tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "mail.imap.delete", id, nil)
	})
}

type imapRow struct {
	IMAPAccount
	pass     []byte
	validity int64
}

func (s *Service) imapConfig(ctx context.Context, r imapRow) (mail.IMAPConfig, error) {
	pw, err := s.unseal(ctx, r.pass)
	if err != nil {
		return mail.IMAPConfig{}, err
	}
	var tc *tls.Config
	if s.IMAPTLS != nil {
		tc = s.IMAPTLS
	}
	return mail.IMAPConfig{Host: r.Host, Port: r.Port, TLS: r.TLS, User: r.User, Pass: pw, Mailbox: r.Mailbox, AllowPrivate: s.IMAPAllowPrivate, TLSConfig: tc}, nil
}

func (s *Service) loadIMAP(ctx context.Context, id string) (imapRow, error) {
	var r imapRow
	err := s.DB.QueryRow(ctx, `SELECT `+imapCols+`,password,uidvalidity FROM mail_imap WHERE id::text=$1`, id).Scan(
		&r.ID, &r.Name, &r.Host, &r.Port, &r.TLS, &r.User, &r.Mailbox, &r.Trusted, &r.Enabled, &r.LastUID, &r.Resets, &r.Received, &r.LastPoll, &r.LastOK, &r.Error, &r.pass, &r.validity)
	if err != nil {
		return r, ErrNotFound
	}
	return r, nil
}

// CheckIMAP connects, logs in and opens the mailbox without reading anything: what "Test" in Admin runs.
func (s *Service) CheckIMAP(ctx context.Context, a Actor, id string) (map[string]any, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	r, err := s.loadIMAP(ctx, id)
	if err != nil {
		return nil, err
	}
	cfg, err := s.imapConfig(ctx, r)
	if err != nil {
		return nil, err
	}
	c, err := mail.DialIMAP(ctx, cfg)
	if err != nil {
		return nil, invalid("%v", err)
	}
	defer c.Close()
	return map[string]any{"messages": c.Count, "idle": c.Idle, "uidvalidity": c.Valid}, nil
}

func (s *Service) imapLease(ctx context.Context, id string) bool {
	tag, err := s.DB.Exec(ctx, `UPDATE mail_imap SET leased_until=$2 WHERE id::text=$1 AND enabled AND (leased_until IS NULL OR leased_until<$3)`, id, s.Now().Add(2*time.Minute), s.Now())
	return err == nil && tag.RowsAffected() == 1
}

func (s *Service) imapDone(ctx context.Context, id string, err error) {
	if err != nil {
		s.DB.Exec(ctx, `UPDATE mail_imap SET last_poll=$2,last_error=$3,leased_until=NULL WHERE id::text=$1`, id, s.Now(), err.Error())
		return
	}
	s.DB.Exec(ctx, `UPDATE mail_imap SET last_poll=$2,last_ok=$2,last_error='',leased_until=NULL WHERE id::text=$1`, id, s.Now())
}

const imapBatch = 500

// imapPass reads what is new on an open connection: at most one batch, reporting whether more is waiting.
func (s *Service) imapPass(ctx context.Context, r imapRow, c *mail.IMAPConn) (n int, more bool, err error) {
	if int64(c.Valid) != r.validity {
		reset := ""
		if r.validity != 0 {
			reset = ",resets=resets+1"
		}
		if _, err := s.DB.Exec(ctx, `UPDATE mail_imap SET uidvalidity=$2,last_uid=0`+reset+` WHERE id::text=$1`, r.ID, int64(c.Valid)); err != nil {
			return 0, false, err
		}
		r.validity, r.LastUID = int64(c.Valid), 0
	}
	if err := c.Noop(); err != nil {
		return 0, false, err
	}
	uids, err := c.UIDsAfter(uint32(r.LastUID))
	if err != nil {
		return 0, false, err
	}
	limit := s.inboundLimits().MaxBytes
	for i, uid := range uids {
		if i >= imapBatch {
			return n, true, nil
		}
		raw, err := c.Fetch(uid, limit)
		if errors.Is(err, mail.ErrIMAP) { // the message was removed under us, or the server refuses it: move past it
			s.DB.Exec(ctx, `UPDATE mail_imap SET last_uid=$2 WHERE id::text=$1`, r.ID, int64(uid))
			continue
		}
		if err != nil { // the connection died: the checkpoint stays where it was
			return n, false, err
		}
		if _, err := s.IngestMail(ctx, "", r.Trusted, raw); err != nil { // not kept (the blob store, the database): do not move on
			return n, false, fmt.Errorf("message %d: %w", uid, err)
		}
		if _, err := s.DB.Exec(ctx, `UPDATE mail_imap SET last_uid=$2,received=received+1,leased_until=$3 WHERE id::text=$1`, r.ID, int64(uid), s.Now().Add(2*time.Minute)); err != nil {
			return n, false, err
		}
		n++
	}
	return n, false, nil
}

// PollIMAP reads what is new in a mailbox once, over a fresh connection, and returns how many messages it handled. It
// skips a mailbox that another node is reading.
func (s *Service) PollIMAP(ctx context.Context, id string) (int, error) {
	if !s.imapLease(ctx, id) {
		return 0, nil
	}
	r, err := s.loadIMAP(ctx, id)
	if err != nil {
		return 0, err
	}
	total, err := s.pollOnce(ctx, r)
	s.imapDone(ctx, id, err)
	return total, err
}

func (s *Service) pollOnce(ctx context.Context, r imapRow) (int, error) {
	cfg, err := s.imapConfig(ctx, r)
	if err != nil {
		return 0, err
	}
	c, err := mail.DialIMAP(ctx, cfg)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	total := 0
	for {
		n, more, err := s.imapPass(ctx, r, c)
		total += n
		if err != nil || !more {
			return total, err
		}
		if r, err = s.loadIMAP(ctx, r.ID); err != nil {
			return total, err
		}
	}
}

// RunIMAP keeps one reader per enabled mailbox: it holds the connection, waits with IDLE when the server has it (or polls
// every thirty seconds when not), and reconnects with a growing pause after an error.
func (s *Service) RunIMAP(ctx context.Context, logf func(string, ...any)) {
	running := map[string]context.CancelFunc{}
	var mu sync.Mutex
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		rows, err := s.DB.Query(ctx, `SELECT id::text FROM mail_imap WHERE enabled`)
		want := map[string]bool{}
		if err == nil {
			for rows.Next() {
				var id string
				rows.Scan(&id)
				want[id] = true
			}
			rows.Close()
			mu.Lock()
			for id := range want {
				if _, ok := running[id]; !ok {
					cctx, cancel := context.WithCancel(ctx)
					running[id] = cancel
					go s.imapLoop(cctx, id, logf)
				}
			}
			for id, cancel := range running {
				if !want[id] {
					cancel()
					delete(running, id)
				}
			}
			mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) imapLoop(ctx context.Context, id string, logf func(string, ...any)) {
	pause := 10 * time.Second
	for ctx.Err() == nil {
		err := s.imapSession(ctx, id)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logf("imap %s: %v (retrying in %s)", id, err, pause)
			s.imapDone(context.WithoutCancel(ctx), id, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		if pause *= 2; pause > 5*time.Minute {
			pause = 5 * time.Minute
		}
	}
}

// imapSession holds one connection until it fails or the context ends.
func (s *Service) imapSession(ctx context.Context, id string) error {
	r, err := s.loadIMAP(ctx, id)
	if err != nil {
		return err
	}
	cfg, err := s.imapConfig(ctx, r)
	if err != nil {
		return err
	}
	c, err := mail.DialIMAP(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	idle, poll := s.IMAPIdle, s.IMAPPoll
	if idle <= 0 {
		idle = 25 * time.Minute
	}
	if poll <= 0 {
		poll = 30 * time.Second
	}
	for ctx.Err() == nil {
		if !s.imapLease(ctx, id) { // another node holds it: wait and look again
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(poll):
			}
			continue
		}
		if r, err = s.loadIMAP(ctx, id); err != nil {
			return err
		}
		n, more, err := s.imapPass(ctx, r, c)
		s.imapDone(ctx, id, err)
		if err != nil {
			return err
		}
		if more || n > 0 {
			continue
		}
		if c.Idle {
			if _, err := c.WaitNew(ctx, idle); err != nil && ctx.Err() == nil {
				return err
			}
		} else {
			select {
			case <-ctx.Done():
			case <-time.After(poll):
			}
		}
	}
	return nil
}
