package domain

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/larytet/tasklane/internal/git"
)

// ---------- secrets at rest ----------

// seal encrypts a credential with AES-256-GCM under the instance secret key; the output is nonce || ciphertext.
func (s *Service) seal(ctx context.Context, plain string) ([]byte, error) {
	key, err := s.secretKey(ctx)
	if err != nil {
		return nil, err
	}
	blockc, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blockc)
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (s *Service) unseal(ctx context.Context, b []byte) (string, error) {
	if len(b) == 0 {
		return "", nil
	}
	key, err := s.secretKey(ctx)
	if err != nil {
		return "", err
	}
	blockc, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(blockc)
	if len(b) < gcm.NonceSize() {
		return "", errors.New("the stored credential is damaged")
	}
	p, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("the stored credential cannot be decrypted (was the secret key changed?)")
	}
	return string(p), nil
}

// secretKey is the configured key, or one generated once and kept in the database (a weaker arrangement that the
// documentation says so: a database dump then holds the key too).
func (s *Service) secretKey(ctx context.Context) ([]byte, error) {
	if len(s.SecretKey) == 32 {
		return s.SecretKey, nil
	}
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if len(s.derived) == 32 {
		return s.derived, nil
	}
	var v string
	err := s.DB.QueryRow(ctx, `SELECT value FROM setting WHERE key='secret_key'`).Scan(&v)
	if err == pgx.ErrNoRows {
		raw := make([]byte, 32)
		rand.Read(raw)
		if _, err := s.DB.Exec(ctx, `INSERT INTO setting(key,value) VALUES('secret_key',$1) ON CONFLICT (key) DO NOTHING`, hex.EncodeToString(raw)); err != nil {
			return nil, err
		}
		err = s.DB.QueryRow(ctx, `SELECT value FROM setting WHERE key='secret_key'`).Scan(&v)
	}
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(v)
	if err != nil || len(b) != 32 {
		return nil, errors.New("the stored secret key is damaged")
	}
	s.derived = b
	return b, nil
}

func fingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])[:8]
}

// ---------- connections and repositories ----------

type GitConnection struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	BaseURL      string   `json:"base_url"`
	TokenFP      string   `json:"token_fingerprint"`
	WebhookFP    string   `json:"webhook_fingerprint"`
	AllowPrivate bool     `json:"allow_private"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	BackfillDays int      `json:"backfill_days"`
}

type GitConnIn struct {
	Name         string   `json:"name"`
	Provider     string   `json:"provider"`
	BaseURL      string   `json:"base_url"`
	Token        string   `json:"token"`
	AllowPrivate bool     `json:"allow_private"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	BackfillDays *int     `json:"backfill_days"`
}

type GitRepo struct {
	ID         string     `json:"id"`
	Connection string     `json:"connection"`
	FullName   string     `json:"full_name"`
	Projects   []string   `json:"projects"`
	LastSync   *time.Time `json:"last_sync_at,omitempty"`
	SyncError  string     `json:"sync_error,omitempty"`
}

func adminOnly(a Actor) error {
	if !a.Admin {
		return ErrForbidden
	}
	return nil
}

// CreateGitConnection stores a connection. It returns the webhook secret once; afterwards only a fingerprint is shown.
func (s *Service) CreateGitConnection(ctx context.Context, a Actor, in GitConnIn) (GitConnection, string, error) {
	if err := adminOnly(a); err != nil {
		return GitConnection{}, "", err
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 80 {
		return GitConnection{}, "", invalid("a connection needs a name of up to 80 characters")
	}
	if in.Provider != "github" && in.Provider != "gitlab" {
		return GitConnection{}, "", invalid("the provider is github or gitlab")
	}
	if in.BaseURL == "" {
		if in.Provider != "github" {
			return GitConnection{}, "", invalid("a GitLab connection needs the base URL of the instance")
		}
		in.BaseURL = "https://api.github.com"
	}
	in.BaseURL = strings.TrimSuffix(in.BaseURL, "/")
	if u := strings.ToLower(in.BaseURL); !strings.HasPrefix(u, "https://") && !(in.AllowPrivate && strings.HasPrefix(u, "http://")) {
		return GitConnection{}, "", invalid("the base URL must use https (plain http is for private hosts with allow_private)")
	}
	for _, c := range in.AllowedCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return GitConnection{}, "", invalid("%q is not a CIDR range such as 192.30.252.0/22", c)
		}
	}
	days := 90
	if in.BackfillDays != nil {
		days = *in.BackfillDays
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	secret := hex.EncodeToString(raw)
	whEnc, err := s.seal(ctx, secret)
	if err != nil {
		return GitConnection{}, "", err
	}
	var tokEnc []byte
	if in.Token != "" {
		if tokEnc, err = s.seal(ctx, in.Token); err != nil {
			return GitConnection{}, "", err
		}
	}
	c := GitConnection{Name: in.Name, Provider: in.Provider, BaseURL: in.BaseURL, TokenFP: fingerprint(in.Token), WebhookFP: fingerprint(secret), AllowPrivate: in.AllowPrivate, AllowedCIDRs: in.AllowedCIDRs, BackfillDays: days}
	if c.AllowedCIDRs == nil {
		c.AllowedCIDRs = []string{}
	}
	err = s.DB.QueryRow(ctx, `INSERT INTO git_connection(name,provider,base_url,token_enc,token_fp,webhook_secret_enc,webhook_fp,allow_private,allowed_cidrs,backfill_days,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id::text`, c.Name, c.Provider, c.BaseURL, tokEnc, c.TokenFP, whEnc, c.WebhookFP, c.AllowPrivate, c.AllowedCIDRs, days, nilIfEmpty(a.ID)).Scan(&c.ID)
	if err != nil {
		if strings.Contains(err.Error(), "git_connection_name_key") {
			return c, "", invalid("a connection called %q already exists", c.Name)
		}
		return c, "", err
	}
	return c, secret, nil
}

func (s *Service) ListGitConnections(ctx context.Context, a Actor) ([]GitConnection, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text,name,provider,base_url,token_fp,webhook_fp,allow_private,allowed_cidrs,backfill_days FROM git_connection ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitConnection{}
	for rows.Next() {
		var c GitConnection
		rows.Scan(&c.ID, &c.Name, &c.Provider, &c.BaseURL, &c.TokenFP, &c.WebhookFP, &c.AllowPrivate, &c.AllowedCIDRs, &c.BackfillDays)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Service) DeleteGitConnection(ctx context.Context, a Actor, id string) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM git_connection WHERE id::text=$1`, id)
	if err != nil && strings.Contains(err.Error(), "invalid input syntax") {
		return ErrNotFound
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$`)

// MapGitRepo registers a repository on a connection and sets the projects whose issue keys it links to.
func (s *Service) MapGitRepo(ctx context.Context, a Actor, connID, fullName string, projectKeys []string) (GitRepo, error) {
	if err := adminOnly(a); err != nil {
		return GitRepo{}, err
	}
	if !repoRe.MatchString(fullName) {
		return GitRepo{}, invalid("a repository is written owner/name (or group/subgroup/project)")
	}
	if len(projectKeys) == 0 {
		return GitRepo{}, invalid("map the repository to at least one project")
	}
	r := GitRepo{Connection: connID, FullName: fullName, Projects: []string{}}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO git_repo(connection_id,full_name) VALUES($1::uuid,$2) ON CONFLICT (connection_id,full_name) DO UPDATE SET full_name=EXCLUDED.full_name RETURNING id::text`, connID, fullName).Scan(&r.ID); err != nil {
			if strings.Contains(err.Error(), "violates foreign key") || strings.Contains(err.Error(), "invalid input syntax") {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM git_repo_project WHERE repo_id::text=$1`, r.ID); err != nil {
			return err
		}
		for _, k := range projectKeys {
			k = strings.ToUpper(strings.TrimSpace(k))
			tag, err := tx.Exec(ctx, `INSERT INTO git_repo_project(repo_id,project_id) SELECT $1::uuid,id FROM project WHERE key=$2`, r.ID, k)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return invalid("there is no project %q", k)
			}
			r.Projects = append(r.Projects, k)
		}
		return nil
	})
	return r, err
}

func (s *Service) ListGitRepos(ctx context.Context, a Actor) ([]GitRepo, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT r.id::text,r.connection_id::text,r.full_name,r.last_sync_at,r.sync_error,COALESCE(array_agg(p.key ORDER BY p.key) FILTER (WHERE p.key IS NOT NULL),'{}')
		FROM git_repo r LEFT JOIN git_repo_project rp ON rp.repo_id=r.id LEFT JOIN project p ON p.id=rp.project_id GROUP BY r.id ORDER BY r.full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitRepo{}
	for rows.Next() {
		var r GitRepo
		rows.Scan(&r.ID, &r.Connection, &r.FullName, &r.LastSync, &r.SyncError, &r.Projects)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- webhooks ----------

const MaxWebhookBytes = 5 << 20

type WebhookResult struct {
	Duplicate bool `json:"duplicate"`
}

// ReceiveGitWebhook verifies and stores a delivery and returns at once; processing is asynchronous. A delivery that was
// seen before (same delivery id, or same content when the provider sends none) is acknowledged and ignored.
func (s *Service) ReceiveGitWebhook(ctx context.Context, connID, remoteIP string, h http.Header, body []byte) (WebhookResult, error) {
	var provider string
	var whEnc []byte
	var cidrs []string
	err := s.DB.QueryRow(ctx, `SELECT provider,webhook_secret_enc,allowed_cidrs FROM git_connection WHERE id::text=$1`, connID).Scan(&provider, &whEnc, &cidrs)
	if err == pgx.ErrNoRows || (err != nil && strings.Contains(err.Error(), "invalid input syntax")) {
		return WebhookResult{}, ErrNotFound
	}
	if err != nil {
		return WebhookResult{}, err
	}
	if len(body) > MaxWebhookBytes {
		return WebhookResult{}, &PayloadTooLarge{Limit: MaxWebhookBytes}
	}
	if len(cidrs) > 0 && !ipInAny(remoteIP, cidrs) {
		return WebhookResult{}, ErrForbidden
	}
	secret, err := s.unseal(ctx, whEnc)
	if err != nil {
		return WebhookResult{}, err
	}
	p, _ := git.ByName(provider)
	d, err := p.Verify(h, body, secret)
	if err != nil {
		if errors.Is(err, git.ErrBadSignature) {
			return WebhookResult{}, ErrForbidden
		}
		return WebhookResult{}, invalid("%v", err)
	}
	tag, err := s.DB.Exec(ctx, `INSERT INTO git_inbox(connection_id,delivery_id,event,payload) VALUES($1::uuid,$2,$3,$4) ON CONFLICT (connection_id,delivery_id) DO NOTHING`, connID, d.ID, d.Event, body)
	if err != nil {
		return WebhookResult{}, err
	}
	return WebhookResult{Duplicate: tag.RowsAffected() == 0}, nil
}

// PayloadTooLarge is returned for a webhook body over the limit.
type PayloadTooLarge struct{ Limit int }

func (e *PayloadTooLarge) Error() string {
	return fmt.Sprintf("the payload is larger than %d bytes", e.Limit)
}

func ipInAny(ip string, cidrs []string) bool {
	host, _, err := net.SplitHostPort(ip)
	if err != nil {
		host = ip
	}
	p := net.ParseIP(host)
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil && p != nil && n.Contains(p) {
			return true
		}
	}
	return false
}

// ---------- processing ----------

// ProcessGitInbox applies stored deliveries in arrival order. A delivery that cannot be applied is retried a few times and
// then kept with its error, so nothing silently disappears.
func (s *Service) ProcessGitInbox(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT i.id,i.connection_id::text,c.provider,i.event,i.payload FROM git_inbox i JOIN git_connection c ON c.id=i.connection_id
		WHERE i.processed_at IS NULL AND i.attempts<3 ORDER BY i.id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type item struct {
		id            int64
		conn, prov, e string
		payload       []byte
	}
	var items []item
	for rows.Next() {
		var it item
		rows.Scan(&it.id, &it.conn, &it.prov, &it.e, &it.payload)
		items = append(items, it)
	}
	rows.Close()
	n := 0
	for _, it := range items {
		p, _ := git.ByName(it.prov)
		evs, perr := p.Parse(it.e, it.payload)
		if perr == nil {
			perr = s.applyGitEvents(ctx, it.conn, evs, true)
		}
		if perr != nil {
			s.DB.Exec(ctx, `UPDATE git_inbox SET attempts=attempts+1,error=$2 WHERE id=$1`, it.id, trimErr(perr))
			continue
		}
		// the payload held commit messages and file lists: once applied, keep only the delivery id
		s.DB.Exec(ctx, `UPDATE git_inbox SET processed_at=$2,payload='',error='' WHERE id=$1`, it.id, s.Now())
		n++
	}
	return n, nil
}

func trimErr(err error) string {
	m := err.Error()
	if len(m) > 300 {
		m = m[:300]
	}
	return m
}

type repoInfo struct {
	id            string
	projects      map[string]string // project key -> project id
	provider      string
	defaultBranch string
}

func (s *Service) repoFor(ctx context.Context, connID, full string) (*repoInfo, error) {
	var r repoInfo
	err := s.DB.QueryRow(ctx, `SELECT r.id::text,c.provider,r.default_branch FROM git_repo r JOIN git_connection c ON c.id=r.connection_id WHERE r.connection_id::text=$1 AND lower(r.full_name)=lower($2)`, connID, full).Scan(&r.id, &r.provider, &r.defaultBranch)
	if err == pgx.ErrNoRows {
		return nil, nil // an event for a repository nobody mapped: ignored
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT p.key,p.id::text FROM git_repo_project rp JOIN project p ON p.id=rp.project_id WHERE rp.repo_id::text=$1`, r.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	r.projects = map[string]string{}
	for rows.Next() {
		var k, id string
		rows.Scan(&k, &id)
		r.projects[k] = id
	}
	return &r, nil
}

// applyGitEvents stores the events. With live (a webhook delivery, not a backfill or reconcile) the rules and smart commits the
// events trigger run afterwards: replaying a repository's history must never move issues that finished long ago.
func (s *Service) applyGitEvents(ctx context.Context, connID string, evs []git.Event, live bool) error {
	repos := map[string]*repoInfo{}
	touched := map[string]bool{}
	for _, e := range evs {
		ri, ok := repos[strings.ToLower(e.Repo)]
		if !ok {
			var err error
			if ri, err = s.repoFor(ctx, connID, e.Repo); err != nil {
				return err
			}
			repos[strings.ToLower(e.Repo)] = ri
		}
		if ri == nil {
			continue
		}
		if e.DefaultBranch != "" && e.DefaultBranch != ri.defaultBranch {
			s.DB.Exec(ctx, `UPDATE git_repo SET default_branch=$2 WHERE id::text=$1`, ri.id, e.DefaultBranch)
			ri.defaultBranch = e.DefaultBranch
		}
		keys, ap, err := s.applyGitEvent(ctx, ri, e)
		if err != nil {
			return err
		}
		for _, k := range keys {
			touched[k] = true
		}
		if live && ap != nil {
			s.runGitRules(ctx, ri, *ap)
			s.runSmartCommits(ctx, ri, *ap)
		}
	}
	for k := range touched {
		s.emit(Event{Type: "issue.updated", Issue: k, Fields: []string{"development"}, Actor: "Git", ActorID: ""})
	}
	return nil
}

// applyGitEvent upserts one object (the provider's newer version wins) and recomputes its automatic links. It returns the
// keys of issues whose development panel changed.
func (s *Service) applyGitEvent(ctx context.Context, r *repoInfo, e git.Event) ([]string, *gitApplied, error) {
	var objID string
	var affected []string
	var applied *gitApplied
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if e.Deleted {
			rows, err := tx.Query(ctx, `SELECT i.key FROM git_object o JOIN git_link l ON l.object_id=o.id JOIN issue i ON i.id=l.issue_id WHERE o.repo_id::text=$1 AND o.kind=$2 AND o.ext_id=$3`, r.id, e.Kind, e.ExtID)
			if err != nil {
				return err
			}
			for rows.Next() {
				var k string
				rows.Scan(&k)
				affected = append(affected, k)
			}
			rows.Close()
			_, err = tx.Exec(ctx, `DELETE FROM git_object WHERE repo_id::text=$1 AND kind=$2 AND ext_id=$3`, r.id, e.Kind, e.ExtID)
			return err
		}
		// who did it: a linked provider account, or an email the provider vouches for. A name or email written in a commit never maps.
		var user any
		if uid := s.gitUser(ctx, tx, r.provider, e.Login, e.VerifiedEmail); uid != nil {
			user = *uid
		}
		var prev string
		existed := tx.QueryRow(ctx, `SELECT state FROM git_object WHERE repo_id::text=$1 AND kind=$2 AND ext_id=$3`, r.id, e.Kind, e.ExtID).Scan(&prev) == nil
		meta := map[string]any{}
		for k, v := range e.Extra {
			meta[k] = v
		}
		metaJSON, _ := json.Marshal(meta)
		at := e.At
		zero := at.IsZero()
		if zero {
			at = time.Unix(1, 0)
		}
		reviewOnly := e.Extra != nil && e.Extra["review_only"] == true
		tag, err := tx.Exec(ctx, `INSERT INTO git_object(repo_id,kind,ext_id,title,state,review,author,author_user,url,sha,branch,base,env,meta,provider_at,updated_at)
			VALUES($1::uuid,$2,$3,$4,$5,CASE WHEN $6='none' THEN '' ELSE $6 END,$7,$8::uuid,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (repo_id,kind,ext_id) DO UPDATE SET
			  title=CASE WHEN $17 THEN git_object.title ELSE EXCLUDED.title END,
			  state=CASE WHEN $17 THEN git_object.state ELSE EXCLUDED.state END,
			  review=CASE WHEN $6='none' THEN '' WHEN $6<>'' THEN $6 ELSE git_object.review END,
			  author=CASE WHEN $17 OR EXCLUDED.author='' THEN git_object.author ELSE EXCLUDED.author END,
			  author_user=COALESCE(EXCLUDED.author_user,git_object.author_user),
			  url=CASE WHEN $17 OR EXCLUDED.url='' THEN git_object.url ELSE EXCLUDED.url END,
			  sha=CASE WHEN $17 OR EXCLUDED.sha='' THEN git_object.sha ELSE EXCLUDED.sha END,
			  branch=CASE WHEN $17 OR EXCLUDED.branch='' THEN git_object.branch ELSE EXCLUDED.branch END,
			  base=CASE WHEN $17 OR EXCLUDED.base='' THEN git_object.base ELSE EXCLUDED.base END,
			  env=CASE WHEN EXCLUDED.env='' THEN git_object.env ELSE EXCLUDED.env END,
			  meta=EXCLUDED.meta,provider_at=EXCLUDED.provider_at,updated_at=EXCLUDED.updated_at
			WHERE git_object.provider_at <= EXCLUDED.provider_at AND NOT ($18 AND git_object.provider_at > EXCLUDED.provider_at)`,
			r.id, e.Kind, e.ExtID, e.Title, e.State, e.Review, e.Author, user, e.URL, e.SHA, e.Branch, e.Base, e.Env, metaJSON, at, s.Now(), reviewOnly, zero)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // an older version arrived after a newer one: the stored state already wins
		}
		if err := tx.QueryRow(ctx, `SELECT id::text FROM git_object WHERE repo_id::text=$1 AND kind=$2 AND ext_id=$3`, r.id, e.Kind, e.ExtID).Scan(&objID); err != nil {
			return err
		}
		if !reviewOnly {
			applied = &gitApplied{objID: objID, ev: e, existed: existed, prev: prev}
		}
		if e.Kind == git.KindPR || e.Kind == git.KindBranch || e.Kind == git.KindCommit {
			text := e.Title + "\n" + e.Body
			if e.Kind == git.KindPR {
				text += "\n" + e.Branch
			}
			if reviewOnly { // a review changes the review state only; keep the links the PR text produced
				return nil
			}
			var err error
			affected, err = s.relink(ctx, tx, r, objID, text)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return affected, applied, nil
}

// issueKeyRe builds the matcher for the keys of the projects a repository is mapped to: case-insensitive, and never
// inside a longer word, so UTF-8 does not link unless UTF is a mapped project.
func issueKeyRe(projects map[string]string) *regexp.Regexp {
	if len(projects) == 0 {
		return nil
	}
	ks := make([]string, 0, len(projects))
	for k := range projects {
		ks = append(ks, regexp.QuoteMeta(k))
	}
	sort.Strings(ks)
	// the character after the key is checked by keyHits, not consumed here: otherwise "ABC-1 ABC-2" with one space between would
	// lose the second key, whose leading separator the first match had taken
	return regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(` + strings.Join(ks, "|") + `)-(\d{1,9})`)
}

type keyHit struct {
	key string // upper case, "ABC-12"
	end int    // where the key ends in the text
}

// keyHits finds the issue keys in text: case-insensitive, never inside a longer word or number (UTF-8, ABC-12x, ABC-1234567890).
func keyHits(re *regexp.Regexp, text string) []keyHit {
	var out []keyHit
	for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
		if m[1] < len(text) {
			c := text[m[1]]
			if c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
				continue
			}
		}
		out = append(out, keyHit{key: strings.ToUpper(text[m[2]:m[3]]) + "-" + text[m[4]:m[5]], end: m[1]})
	}
	return out
}

// relink replaces the object's automatic links with the issues named in text. Manual links stay. It returns the keys
// of every issue whose panel changed (added or removed).
func (s *Service) relink(ctx context.Context, tx pgx.Tx, r *repoInfo, objID, text string) ([]string, error) {
	want := map[string]string{} // issue id -> key
	if re := issueKeyRe(r.projects); re != nil {
		for _, h := range keyHits(re, text) {
			key := h.key
			var id, cur string
			// the key may be an old one: resolve through aliases, and ignore trashed or missing issues
			err := tx.QueryRow(ctx, `SELECT i.id::text,i.key FROM issue i WHERE i.deleted_at IS NULL AND (i.key=$1 OR i.id IN (SELECT issue_id FROM issue_key_alias WHERE old_key=$1)) LIMIT 1`, key).Scan(&id, &cur)
			if err == nil {
				want[id] = cur
			}
		}
	}
	rows, err := tx.Query(ctx, `SELECT l.issue_id::text,i.key,l.manual FROM git_link l JOIN issue i ON i.id=l.issue_id WHERE l.object_id::text=$1`, objID)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	manual := map[string]bool{}
	keyOf := map[string]string{}
	for rows.Next() {
		var id, k string
		var man bool
		rows.Scan(&id, &k, &man)
		have[id], manual[id], keyOf[id] = true, man, k
	}
	rows.Close()
	var changed []string
	for id, k := range want {
		if !have[id] {
			changed = append(changed, k)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO git_link(issue_id,object_id,auto) VALUES($1::uuid,$2::uuid,true) ON CONFLICT (issue_id,object_id) DO UPDATE SET auto=true`, id, objID); err != nil {
			return nil, err
		}
	}
	for id := range have {
		if _, still := want[id]; still {
			continue
		}
		if manual[id] { // a person's link stays, but it is no longer also an automatic one
			tx.Exec(ctx, `UPDATE git_link SET auto=false WHERE issue_id::text=$1 AND object_id::text=$2`, id, objID)
			continue
		}
		changed = append(changed, keyOf[id])
		if _, err := tx.Exec(ctx, `DELETE FROM git_link WHERE issue_id::text=$1 AND object_id::text=$2`, id, objID); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// SealForTest exposes the credential encryption to tests that need to plant a stored credential.
func (s *Service) SealForTest(ctx context.Context, plain string) ([]byte, error) {
	return s.seal(ctx, plain)
}
