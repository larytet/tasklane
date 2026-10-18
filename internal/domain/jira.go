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

func (s *Service) jiraClient(ctx context.Context, connID string) (*jira.Client, string, error) {
	var site, email string
	var tokEnc []byte
	var priv bool
	var by *string
	if err := s.DB.QueryRow(ctx, `SELECT site_url,email,token_enc,allow_private,created_by::text FROM jira_connection WHERE id::text=$1`, connID).Scan(&site, &email, &tokEnc, &priv, &by); err != nil {
		return nil, "", ErrNotFound
	}
	tok, err := s.unseal(ctx, tokEnc)
	if err != nil {
		return nil, "", err
	}
	actor := ""
	if by != nil {
		actor = *by
	}
	return &jira.Client{Site: site, Email: email, Token: tok, HTTP: s.jiraEgress(priv).Client(), MaxWait: 30 * time.Second, MinGap: 0, Now: s.Now}, actor, nil
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
	if _, err := cl.Myself(ctx); err != nil {
		var st *jira.ErrStatus
		if errors.As(err, &st) && (st.Status == 401 || st.Status == 403) {
			return JiraConnection{}, "", invalid("Jira rejected the credentials: check the email and the API token")
		}
		return JiraConnection{}, "", invalid("cannot reach Jira at %s: %v", in.SiteURL, err)
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	secret := hex.EncodeToString(raw)
	tokEnc, err := s.seal(ctx, in.Token)
	if err != nil {
		return JiraConnection{}, "", err
	}
	whEnc, err := s.seal(ctx, secret)
	if err != nil {
		return JiraConnection{}, "", err
	}
	c := JiraConnection{Name: in.Name, SiteURL: in.SiteURL, Email: in.Email, TokenFP: fingerprint(in.Token), WebhookFP: fingerprint(secret), AllowPrivate: in.AllowPrivate}
	err = s.DB.QueryRow(ctx, `INSERT INTO jira_connection(name,site_url,email,token_enc,token_fp,webhook_secret_enc,webhook_fp,allow_private,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
		c.Name, c.SiteURL, c.Email, tokEnc, c.TokenFP, whEnc, c.WebhookFP, c.AllowPrivate, nilIfEmpty(a.ID)).Scan(&c.ID)
	if err != nil {
		if strings.Contains(err.Error(), "jira_connection_name_key") {
			return c, "", invalid("a connection called %q already exists", c.Name)
		}
		return c, "", err
	}
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
}

func (s *Service) mirrorInfo(ctx context.Context, projectKey string) *MirrorInfo {
	var m MirrorInfo
	var synced *time.Time
	err := s.DB.QueryRow(ctx, `SELECT c.site_url,jp.jira_key,jp.synced_at,jp.last_error FROM project p JOIN jira_project jp ON jp.project_id=p.id JOIN jira_connection c ON c.id=jp.connection_id WHERE p.key=$1`, strings.ToUpper(projectKey)).
		Scan(&m.SiteURL, &m.JiraKey, &synced, &m.Error)
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
}

// JiraStatus is the mirror's Admin tab: lag, errors, permission freshness and warnings (MIR-12).
func (s *Service) JiraStatus(ctx context.Context, a Actor) ([]JiraProjectStatus, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT p.id::text,p.key,jp.jira_key,c.name,jp.watermark,jp.synced_at,jp.perm_synced_at,jp.issues_synced,jp.backfilled,jp.last_error
		FROM jira_project jp JOIN project p ON p.id=jp.project_id JOIN jira_connection c ON c.id=jp.connection_id ORDER BY p.key`)
	if err != nil {
		return nil, err
	}
	type row struct {
		pid string
		st  JiraProjectStatus
	}
	var all []row
	for rows.Next() {
		var r row
		rows.Scan(&r.pid, &r.st.Project, &r.st.JiraKey, &r.st.Connection, &r.st.Watermark, &r.st.SyncedAt, &r.st.PermAt, &r.st.Issues, &r.st.Backfilled, &r.st.LastError)
		all = append(all, r)
	}
	rows.Close()
	out := []JiraProjectStatus{}
	for _, r := range all {
		st := r.st
		if st.SyncedAt != nil {
			st.LagSeconds = s.Now().Sub(*st.SyncedAt).Seconds()
		}
		st.PermFresh = st.PermAt != nil && s.Now().Sub(*st.PermAt) < permFreshFor
		st.Warnings = []string{}
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
	return s.tx(ctx, func(tx pgx.Tx) error {
		var pid string
		if err := tx.QueryRow(ctx, `SELECT p.id::text FROM project p JOIN jira_project jp ON jp.project_id=p.id WHERE p.key=$1 FOR UPDATE OF p`, strings.ToUpper(projectKey)).Scan(&pid); err != nil {
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
}
