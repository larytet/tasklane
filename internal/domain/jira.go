package domain

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/credvault"
	"github.com/larytet/tasklane/internal/egress"
	"github.com/larytet/tasklane/internal/jira"
)

// ReadOnlyMirror is returned by every write path for a mirrored project: the UI, REST, MCP, automation and email all
// refuse the same way, and say where the change can be made (SPEC 18.1).
type ReadOnlyMirror struct {
	Project string
	SiteURL string
}

func (e *ReadOnlyMirror) Error() string {
	return fmt.Sprintf("project %s is mirrored from Jira and is read-only here: make the change in Jira (%s)", e.Project, e.SiteURL)
}

// MirrorRefusal is the typed refusal for a write to a mirrored project, or nil for a native one. Entry points that look things up
// before they write (an agent tool resolving a transition by name) call it first, so the answer is the reason and the Jira address, not
// a symptom of the mirror (an empty list of transitions).
func (s *Service) MirrorRefusal(ctx context.Context, projectKey string) error {
	if site := s.mirrorSite(ctx, projectKey); site != "" {
		return &ReadOnlyMirror{Project: strings.ToUpper(projectKey), SiteURL: site}
	}
	return nil
}

// mirrorSite returns the Jira site of a mirrored project ("" for native projects).
func (s *Service) mirrorSite(ctx context.Context, projectKey string) string {
	var site string
	s.DB.QueryRow(ctx, `SELECT c.site_url FROM project p JOIN jira_connection c ON c.id=p.mirror_of WHERE p.key=$1`, strings.ToUpper(projectKey)).Scan(&site)
	return site
}

type JiraConnection struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	SiteURL      string `json:"site_url"`
	Email        string `json:"email"`
	TokenFP      string `json:"token_fingerprint"`
	WebhookFP    string `json:"webhook_fingerprint"`
	AllowPrivate bool   `json:"allow_private"`
	// Locked: no service credential is held in memory on this node (WT-03). LiveTokens counts the people who hold a personal token for
	// this connection here; it is a count and never names anyone (WT-17).
	Locked     bool `json:"locked"`
	LiveTokens int  `json:"live_tokens"`
	// TokenPage is where the account's API token is created, for the unlock dialog.
	TokenPage string `json:"token_page"`
}

type JiraConnIn struct {
	Name         string `json:"name"`
	SiteURL      string `json:"site_url"`
	Email        string `json:"email"`
	Token        string `json:"token"`
	AllowPrivate bool   `json:"allow_private"`
}

func (s *Service) jiraEgress(allowPrivate bool) egress.Policy {
	pol := egress.Policy{AllowPrivate: allowPrivate, MaxBody: 64 << 20}
	if s.GitEgress != nil {
		pol = s.GitEgress(pol)
	}
	return pol
}

// vault is the memory-only holder of Jira credentials (SPEC 18.1.1, WT-01 to WT-06). Nothing in it is ever written to the database.
func (s *Service) vault() *credvault.Vault {
	s.vaultOnce.Do(func() { s.vlt = credvault.New(s.Now, JiraUserTokenTTL) })
	return s.vlt
}

// ErrJiraLocked is returned when a connection has no service credential in memory: after a start, a restart or a node taking over,
// an administrator has to unlock it (WT-03). The sync waits and the mirror keeps being served, marked stale.
type ErrJiraLocked struct{ Connection string }

func (e *ErrJiraLocked) Error() string {
	return fmt.Sprintf("the Jira connection %q is locked: waiting for an administrator to supply its credentials (POST /api/v1/jira/connections/{id}/unlock)", e.Connection)
}

type jiraConnInfo struct {
	id, name, site, email, actor string
	allowPrivate                 bool
}

func (s *Service) jiraConn(ctx context.Context, connID string) (jiraConnInfo, error) {
	c := jiraConnInfo{id: connID}
	var by *string
	if err := s.DB.QueryRow(ctx, `SELECT name,site_url,email,allow_private,created_by::text FROM jira_connection WHERE id::text=$1`, connID).Scan(&c.name, &c.site, &c.email, &c.allowPrivate, &by); err != nil {
		return c, ErrNotFound
	}
	if by != nil {
		c.actor = *by
	}
	return c, nil
}

// clientFor builds a client for one credential. A person's token is never put into a client that the sync shares.
func (s *Service) clientFor(c jiraConnInfo, cr credvault.Cred) *jira.Client {
	return &jira.Client{Site: c.site, Email: cr.Email, Token: cr.Token, HTTP: s.jiraEgress(c.allowPrivate).Client(), MaxWait: 30 * time.Second, MinGap: 0, Now: s.Now}
}

// jiraClient returns the sync's client: the connection's service credential from memory, or ErrJiraLocked.
func (s *Service) jiraClient(ctx context.Context, connID string) (*jira.Client, string, error) {
	c, err := s.jiraConn(ctx, connID)
	if err != nil {
		return nil, "", err
	}
	cr, ok := s.vault().Service(connID)
	if !ok {
		return nil, "", &ErrJiraLocked{Connection: c.name}
	}
	return s.clientFor(c, cr), c.actor, nil
}

// UnlockJiraConnection supplies the service credential of a connection (administrators): it is checked with Jira, must belong to the
// connection's account, and is then kept in memory only. The sync of the connection's projects wakes at once.
func (s *Service) UnlockJiraConnection(ctx context.Context, a Actor, connID, token string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	c, err := s.jiraConn(ctx, connID)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return invalid("the API token is required")
	}
	me, err := s.clientFor(c, credvault.Cred{Email: c.email, Token: token}).Myself(ctx)
	if err != nil {
		var st *jira.ErrStatus
		if errors.As(err, &st) && (st.Status == 401 || st.Status == 403) {
			return invalid("Jira rejected the credentials: check the API token")
		}
		return invalid("cannot reach Jira at %s: %v", c.site, err)
	}
	if me.Email != "" && !strings.EqualFold(me.Email, c.email) {
		return invalid("this token belongs to %s, not to the connection's account %s", me.Email, c.email)
	}
	s.vault().PutService(connID, c.email, me.AccountID, token)
	_, err = s.DB.Exec(ctx, `UPDATE jira_project SET wake=true WHERE connection_id::text=$1`, connID)
	return err
}

// CreateJiraConnection stores a read-only service-account connection after proving the credentials work. The webhook
// secret is returned once.
func (s *Service) CreateJiraConnection(ctx context.Context, a Actor, in JiraConnIn) (JiraConnection, string, error) {
	if err := adminOnly(a); err != nil {
		return JiraConnection{}, "", err
	}
	in.Name, in.SiteURL, in.Email = strings.TrimSpace(in.Name), strings.TrimSuffix(strings.TrimSpace(in.SiteURL), "/"), strings.TrimSpace(in.Email)
	if in.Name == "" || len(in.Name) > 80 {
		return JiraConnection{}, "", invalid("a connection needs a name of up to 80 characters")
	}
	if u := strings.ToLower(in.SiteURL); !strings.HasPrefix(u, "https://") && !(in.AllowPrivate && strings.HasPrefix(u, "http://")) {
		return JiraConnection{}, "", invalid("the Jira site URL must use https (plain http is for private hosts with allow_private)")
	}
	if in.Email == "" || in.Token == "" {
		return JiraConnection{}, "", invalid("the service account's email and API token are required")
	}
	cl := &jira.Client{Site: in.SiteURL, Email: in.Email, Token: in.Token, HTTP: s.jiraEgress(in.AllowPrivate).Client(), Now: s.Now}
	me, err := cl.Myself(ctx)
	if err != nil {
		var st *jira.ErrStatus
		if errors.As(err, &st) && (st.Status == 401 || st.Status == 403) {
			return JiraConnection{}, "", invalid("Jira rejected the credentials: check the email and the API token")
		}
		return JiraConnection{}, "", invalid("cannot reach Jira at %s: %v", in.SiteURL, err)
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	secret := hex.EncodeToString(raw)
	whEnc, err := s.seal(ctx, secret) // the webhook secret authenticates Jira to Tasklane, not Tasklane to Jira: it stays stored
	if err != nil {
		return JiraConnection{}, "", err
	}
	// the token itself is never stored (SPEC 18.1.1, WT-01): the row keeps the account and a short fingerprint, the process keeps the token
	c := JiraConnection{Name: in.Name, SiteURL: in.SiteURL, Email: in.Email, TokenFP: fingerprint(in.Token), WebhookFP: fingerprint(secret), AllowPrivate: in.AllowPrivate}
	err = s.DB.QueryRow(ctx, `INSERT INTO jira_connection(name,site_url,email,token_enc,token_fp,webhook_secret_enc,webhook_fp,allow_private,created_by) VALUES($1,$2,$3,''::bytea,$4,$5,$6,$7,$8) RETURNING id::text`,
		c.Name, c.SiteURL, c.Email, c.TokenFP, whEnc, c.WebhookFP, c.AllowPrivate, nilIfEmpty(a.ID)).Scan(&c.ID)
	if err != nil {
		if strings.Contains(err.Error(), "jira_connection_name_key") {
			return c, "", invalid("a connection called %q already exists", c.Name)
		}
		return c, "", err
	}
	s.vault().PutService(c.ID, in.Email, me.AccountID, in.Token)
	c.Locked = false
	return c, secret, nil
}

func (s *Service) ListJiraConnections(ctx context.Context, a Actor) ([]JiraConnection, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,name,site_url,email,token_fp,webhook_fp,allow_private FROM jira_connection ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []JiraConnection{}
	for rows.Next() {
		var c JiraConnection
		rows.Scan(&c.ID, &c.Name, &c.SiteURL, &c.Email, &c.TokenFP, &c.WebhookFP, &c.AllowPrivate)
		_, unlocked := s.vault().Service(c.ID)
		c.Locked, c.LiveTokens, c.TokenPage = !unlocked, s.vault().Users(c.ID), JiraTokenPageFor(c.SiteURL)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MirrorJiraProject starts mirroring a Jira project (optionally narrowed by JQL) into a read-only Tasklane project.
func (s *Service) MirrorJiraProject(ctx context.Context, a Actor, connID, jiraKey, key, name, jql string) (Project, error) {
	if err := adminOnly(a); err != nil {
		return Project{}, err
	}
	jiraKey = strings.ToUpper(strings.TrimSpace(jiraKey))
	if key = strings.ToUpper(strings.TrimSpace(key)); key == "" {
		key = jiraKey
	}
	if !keyRe.MatchString(key) {
		return Project{}, invalid("the Tasklane project key must be 2-10 letters and digits starting with a letter (Jira's key %q does not fit: choose another)", jiraKey)
	}
	if jiraKey == "" || strings.ContainsAny(jiraKey, `"' \`) {
		return Project{}, invalid("name the Jira project by its key")
	}
	if strings.TrimSpace(name) == "" {
		name = jiraKey
	}
	cl, _, err := s.jiraClient(ctx, connID)
	if err != nil {
		return Project{}, err
	}
	if _, err := cl.Myself(ctx); err != nil {
		return Project{}, invalid("cannot read Jira: %v", err)
	}
	scope := fmt.Sprintf(`project = "%s"`, jiraKey)
	if jql = strings.TrimSpace(jql); jql != "" {
		scope += " AND (" + jql + ")"
	}
	if _, err := cl.Search(ctx, scope, []string{"summary"}, "", "", 1); err != nil {
		var st *jira.ErrStatus
		if errors.As(err, &st) && st.Status == 400 {
			return Project{}, invalid("Jira does not accept this scope (is the project key right, and can the account see it?): %s", st.Body)
		}
		return Project{}, err
	}
	p := Project{Key: key, Name: name, Type: "software"}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var wf string
		if err := tx.QueryRow(ctx, `INSERT INTO workflow(name) VALUES($1) RETURNING id::text`, "Jira "+key).Scan(&wf); err != nil {
			return invalid("a workflow named %q already exists", "Jira "+key)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO project(key,name,lead_id,workflow_id,mirror_of) VALUES($1,$2,$3,$4::uuid,$5::uuid) RETURNING id::text`, key, name, nilIfEmpty(a.ID), wf, connID).Scan(&p.ID); err != nil {
			if strings.Contains(err.Error(), "project_key_key") {
				return invalid("project key %s is already in use", key)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO jira_project(project_id,connection_id,jira_key,jql) VALUES($1::uuid,$2::uuid,$3,$4)`, p.ID, connID, jiraKey, jql); err != nil {
			if strings.Contains(err.Error(), "jira_project_connection_id_jira_key_key") {
				return invalid("Jira project %s is already mirrored on this connection", jiraKey)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO project_member(project_id,user_id,role) VALUES($1::uuid,$2::uuid,'admin')`, p.ID, a.ID); err != nil {
			return err
		}
		return outbox(ctx, tx, "project:"+p.Key, "project.created", p)
	})
	return p, err
}

// MirrorInfo is what a mirrored issue or project shows: where it lives, and how fresh the copy is.
type MirrorInfo struct {
	SiteURL    string     `json:"site_url"`
	JiraKey    string     `json:"jira_key"`
	IssueKey   string     `json:"issue_key,omitempty"` // this issue's key in Jira, for "Edit in Jira" (set on an issue view)
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
	LagSeconds float64    `json:"lag_seconds"`
	Stale      bool       `json:"stale"` // the last sync failed or is older than a few minutes: "synced N min ago"
	Error      string     `json:"error,omitempty"`
	// Mode is read_only or write_through: the page offers its editors only for write_through, where a save goes to Jira (SPEC 18.1.1).
	Mode string `json:"mode"`
}

func (s *Service) mirrorInfo(ctx context.Context, projectKey string) *MirrorInfo {
	var m MirrorInfo
	var synced *time.Time
	err := s.DB.QueryRow(ctx, `SELECT c.site_url,jp.jira_key,jp.synced_at,jp.last_error,jp.mode FROM project p JOIN jira_project jp ON jp.project_id=p.id JOIN jira_connection c ON c.id=jp.connection_id WHERE p.key=$1`, strings.ToUpper(projectKey)).
		Scan(&m.SiteURL, &m.JiraKey, &synced, &m.Error, &m.Mode)
	if err != nil {
		return nil
	}
	m.SyncedAt = synced
	if synced != nil {
		m.LagSeconds = s.Now().Sub(*synced).Seconds()
		if m.LagSeconds < 0 {
			m.LagSeconds = 0
		}
	}
	m.Stale = synced == nil || m.LagSeconds > 300 || m.Error != ""
	return &m
}

type JiraProjectStatus struct {
	Project    string     `json:"project"`
	JiraKey    string     `json:"jira_key"`
	Connection string     `json:"connection"`
	Watermark  *time.Time `json:"watermark,omitempty"`
	SyncedAt   *time.Time `json:"synced_at,omitempty"`
	LagSeconds float64    `json:"lag_seconds"`
	PermAt     *time.Time `json:"permissions_synced_at,omitempty"`
	PermFresh  bool       `json:"permissions_fresh"`
	Issues     int        `json:"issues"`
	Backfilled bool       `json:"backfilled"`
	LastError  string     `json:"last_error,omitempty"`
	Warnings   []string   `json:"warnings"`
	// Mode is read_only or write_through (SPEC 18.1.1); Locked says that this node holds no service credential for the connection, so the
	// sync is waiting for an administrator (WT-03); LiveTokens counts the people holding a personal token for the connection here (WT-17).
	Mode       string `json:"mode"`
	Locked     bool   `json:"locked"`
	LiveTokens int    `json:"live_tokens"`
	// Mail says what email does for this project: Tasklane sends none while it is mirrored (Jira keeps sending its own) and inbound
	// mail addressed to it waits in quarantine. Tasklane cannot see Jira's notification settings, so it cannot say whether they are on.
	Mail string `json:"mail"`
}

// JiraStatus is the mirror's Admin tab: lag, errors, permission freshness and warnings (MIR-12).
func (s *Service) JiraStatus(ctx context.Context, a Actor) ([]JiraProjectStatus, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT p.id::text,p.key,jp.jira_key,c.name,jp.watermark,jp.synced_at,jp.perm_synced_at,jp.issues_synced,jp.backfilled,jp.last_error,jp.mode,c.id::text
		FROM jira_project jp JOIN project p ON p.id=jp.project_id JOIN jira_connection c ON c.id=jp.connection_id ORDER BY p.key`)
	if err != nil {
		return nil, err
	}
	type row struct {
		pid, conn string
		st        JiraProjectStatus
	}
	var all []row
	for rows.Next() {
		var r row
		rows.Scan(&r.pid, &r.st.Project, &r.st.JiraKey, &r.st.Connection, &r.st.Watermark, &r.st.SyncedAt, &r.st.PermAt, &r.st.Issues, &r.st.Backfilled, &r.st.LastError, &r.st.Mode, &r.conn)
		all = append(all, r)
	}
	rows.Close()
	out := []JiraProjectStatus{}
	for _, r := range all {
		st := r.st
		if st.SyncedAt != nil {
			st.LagSeconds = s.Now().Sub(*st.SyncedAt).Seconds()
		}
		_, unlocked := s.vault().Service(r.conn)
		st.Locked, st.LiveTokens = !unlocked, s.vault().Users(r.conn)
		st.PermFresh = st.PermAt != nil && s.Now().Sub(*st.PermAt) < permFreshFor
		st.Warnings = []string{}
		st.Mail = "Tasklane sends no email for this project (Jira sends its own, and Tasklane cannot see whether those are on); mail sent to the project\u2019s address is held in quarantine. Email starts after cut-over to native."
		wr, _ := s.DB.Query(ctx, `SELECT detail FROM jira_warning WHERE project_id::text=$1 ORDER BY kind`, r.pid)
		for wr.Next() {
			var d string
			wr.Scan(&d)
			st.Warnings = append(st.Warnings, d)
		}
		wr.Close()
		out = append(out, st)
	}
	return out, nil
}

const (
	permFreshFor = 15 * time.Minute // matches the effective_member view: past this a mirrored project is invisible
	permRefresh  = 5 * time.Minute
)

// ReceiveJiraWebhook verifies a webhook and only wakes the sync (a hint): the source of truth is polling `updated`, so a
// missed, duplicated or reordered webhook can never lose a change. The signature header is `X-Hub-Signature`.
func (s *Service) ReceiveJiraWebhook(ctx context.Context, connID string, h http.Header, body []byte) error {
	var whEnc []byte
	err := s.DB.QueryRow(ctx, `SELECT webhook_secret_enc FROM jira_connection WHERE id::text=$1`, connID).Scan(&whEnc)
	if err == pgx.ErrNoRows || (err != nil && strings.Contains(err.Error(), "invalid input syntax")) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if len(body) > MaxWebhookBytes {
		return &PayloadTooLarge{Limit: MaxWebhookBytes}
	}
	secret, err := s.unseal(ctx, whEnc)
	if err != nil {
		return err
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(h.Get("X-Hub-Signature"))) {
		return ErrForbidden
	}
	_, err = s.DB.Exec(ctx, `UPDATE jira_project SET wake=true WHERE connection_id::text=$1`, connID)
	return err
}

// CutOverJira turns a mirrored project into a native one (administrators): the sync stops, the project becomes writable,
// keys and history stay, and Jira is no longer updated by Tasklane. The mirror can be rebuilt up to this point; after it the
// Tasklane copy is the only one. Issues hidden for a Jira security level stay hidden until an administrator reviews them.
func (s *Service) CutOverJira(ctx context.Context, a Actor, projectKey string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	var connID string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var pid string
		if err := tx.QueryRow(ctx, `SELECT p.id::text,jp.connection_id::text FROM project p JOIN jira_project jp ON jp.project_id=p.id WHERE p.key=$1 FOR UPDATE OF p`, strings.ToUpper(projectKey)).Scan(&pid, &connID); err != nil {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM jira_project WHERE project_id::text=$1`, pid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE project SET mirror_of=NULL WHERE id::text=$1`, pid); err != nil {
			return err
		}
		return outbox(ctx, tx, "project:"+strings.ToUpper(projectKey), "project.cutover", map[string]any{"project": projectKey, "actor": a.Label, "actor_id": a.ID})
	})
	if err == nil {
		s.dropTokensIfNoWriteThrough(ctx, connID) // WT-16
	}
	return err
}

// dropTokensIfNoWriteThrough forgets every person's token for a connection once none of its projects writes through any more.
func (s *Service) dropTokensIfNoWriteThrough(ctx context.Context, connID string) {
	var n int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM jira_project WHERE connection_id::text=$1 AND mode='write_through'`, connID).Scan(&n); err == nil && n == 0 {
		s.vault().DropUsers(connID)
	}
}

// SetJiraProjectMode chooses read_only or write_through for a mirrored project (administrators, WT-15). Leaving write_through drops the
// people's tokens held for the connection when no other project of it still writes through (WT-16).
func (s *Service) SetJiraProjectMode(ctx context.Context, a Actor, projectKey, mode string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	if mode != "read_only" && mode != "write_through" {
		return invalid("mode is read_only or write_through")
	}
	var connID string
	err := s.DB.QueryRow(ctx, `UPDATE jira_project SET mode=$2 WHERE project_id=(SELECT id FROM project WHERE key=$1) RETURNING connection_id::text`, strings.ToUpper(projectKey), mode).Scan(&connID)
	if err != nil {
		return ErrNotFound
	}
	if mode == "read_only" {
		s.dropTokensIfNoWriteThrough(ctx, connID)
	} else if pid, err := s.projectID(ctx, projectKey); err == nil {
		s.DB.Exec(ctx, anyStatusTransitions, pid) // the statuses already mirrored get their "Move" now; new ones get it from the sync
	}
	s.auditDirect(ctx, a, "jira.mode", strings.ToUpper(projectKey), map[string]any{"mode": mode})
	return nil
}
