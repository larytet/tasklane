package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Scopes an agent token can carry (SPEC 18.3). A scope is a ceiling; the permission scheme is enforced as well.
var AllScopes = []string{"read", "comment", "edit-fields", "create", "transition", "claim", "branch"}

const defaultLease = 30 * time.Minute

func validScopes(sc []string) error {
	ok := map[string]bool{}
	for _, s := range AllScopes {
		ok[s] = true
	}
	for _, s := range sc {
		if !ok[s] {
			return invalid("unknown scope %q (%s)", s, strings.Join(AllScopes, ", "))
		}
	}
	return nil
}

// CreateAgent makes a dedicated agent account (a user of type agent, no password), the recommended setup.
func (s *Service) CreateAgent(ctx context.Context, a Actor, name string) (User, error) {
	if !a.Admin && !a.OrgAdmin { // an organization administrator makes agents for their own organization
		return User{}, ErrForbidden
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return User{}, invalid("an agent needs a name of up to 60 characters")
	}
	slug := strings.ToLower(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, name))
	domainPart := "agents.invalid"
	if a.OrgID() != DefaultOrg { // names are per organization: another organization's agent of the same name is not a clash
		domainPart = a.OrgID()[:8] + "." + domainPart
	}
	u := User{Email: "agent-" + slug + "@" + domainPart, Name: name, Kind: "agent"}
	err := s.DB.QueryRow(ctx, `INSERT INTO app_user(email,name,kind,org_id) VALUES($1,$2,'agent',$3) RETURNING id`, u.Email, name, a.OrgID()).Scan(&u.ID)
	if err != nil && strings.Contains(err.Error(), "app_user_email_key") {
		return User{}, invalid("an agent with this name already exists")
	}
	return u, err
}

// CreateScopedToken returns a new token for the user; only its hash is stored. A nil scope list is a plain REST token.
func (s *Service) CreateScopedToken(ctx context.Context, a Actor, userID, name string, scopes []string, ttl time.Duration) (string, error) {
	if !a.Admin && a.ID != userID {
		var same bool // an organization administrator may issue a token for a person or agent of their own organization
		if a.OrgAdmin {
			s.DB.QueryRow(ctx, `SELECT org_id::text=$2 FROM app_user WHERE id::text=$1`, userID, a.OrgID()).Scan(&same)
		}
		if !same {
			return "", ErrForbidden
		}
	}
	if err := validScopes(scopes); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	tok := "tl_" + hex.EncodeToString(raw)
	var exp any
	if ttl > 0 {
		exp = s.Now().Add(ttl)
	}
	var sc any
	if scopes != nil {
		sc = scopes
	}
	_, err := s.DB.Exec(ctx, `INSERT INTO api_token(token_hash,user_id,name,scopes,expires_at) VALUES($1,$2,$3,$4,$5)`, hashToken(tok), userID, name, sc, exp)
	return tok, err
}

func (s *Service) RevokeToken(ctx context.Context, a Actor, tok string) error {
	if !a.Admin {
		return ErrForbidden
	}
	_, err := s.DB.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE token_hash=$1`, hashToken(tok), s.Now())
	return err
}

// AuthenticateAgent resolves a bearer token to an actor and its scopes. Expired and revoked tokens fail at once.
func (s *Service) AuthenticateAgent(ctx context.Context, tok string) (Actor, []string, error) {
	var u User
	var name string
	var scopes []string
	var paused bool
	var exp, rev *time.Time
	var inst bool
	var org string
	err := s.DB.QueryRow(ctx, `SELECT u.id,u.name,u.kind,u.is_admin,u.instance_admin,u.org_id::text,u.agent_paused,t.name,t.scopes,t.expires_at,t.revoked_at FROM api_token t JOIN app_user u ON u.id=t.user_id WHERE t.token_hash=$1 AND u.active`, hashToken(tok)).
		Scan(&u.ID, &u.Name, &u.Kind, &u.Admin, &inst, &org, &paused, &name, &scopes, &exp, &rev)
	if err == pgx.ErrNoRows {
		return Actor{}, nil, ErrForbidden
	}
	if err != nil {
		return Actor{}, nil, err
	}
	if killed, _ := s.AgentsKilled(ctx); killed && u.Kind == "agent" {
		return Actor{}, nil, ErrForbidden
	}
	if paused && u.Kind == "agent" {
		return Actor{}, nil, ErrForbidden
	}
	if rev != nil || (exp != nil && !exp.After(s.Now())) {
		return Actor{}, nil, ErrForbidden
	}
	if scopes == nil { // a personal token acts with everything the person may do
		scopes = AllScopes
	}
	label := "Agent: " + u.Name
	if u.Kind != "agent" {
		label = "Agent: " + name + " (via " + u.Name + ")"
	}
	// an agent never exceeds the permission scheme, and never gets administrator rights through a token
	return Actor{ID: u.ID, Label: label, Admin: u.Admin && inst && u.Kind != "agent", OrgAdmin: u.Admin && u.Kind != "agent", Org: org, Kind: "agent"}, scopes, nil
}

// LeaseHeld is returned when another agent holds a live claim (HTTP-style conflict).
type LeaseHeld struct {
	Holder string
	Until  time.Time
}

func (e *LeaseHeld) Error() string {
	return fmt.Sprintf("the issue is claimed by %s until %s", e.Holder, e.Until.UTC().Format(time.RFC3339))
}

type LeaseInfo struct {
	Holder  string    `json:"holder"`
	Until   time.Time `json:"until"`
	Stalled bool      `json:"stalled"` // the lease ran out while the agent still holds the assignment
}

func (s *Service) lease(ctx context.Context, issueID string) *LeaseInfo {
	var l LeaseInfo
	var holderID string
	if err := s.DB.QueryRow(ctx, `SELECT u.name,u.id,l.expires_at FROM issue_lease l JOIN app_user u ON u.id=l.holder_id WHERE l.issue_id=$1`, issueID).Scan(&l.Holder, &holderID, &l.Until); err != nil {
		return nil
	}
	if !l.Until.After(s.Now()) {
		var assigned bool
		s.DB.QueryRow(ctx, `SELECT assignee_id=$2 FROM issue WHERE id=$1`, issueID, holderID).Scan(&assigned)
		l.Stalled = assigned
	}
	return &l
}

// Lease reports the claim on an issue, if any.
func (s *Service) Lease(ctx context.Context, a Actor, key string) (*LeaseInfo, error) {
	is, _, err := s.access(ctx, a, key)
	if err != nil {
		return nil, err
	}
	return s.lease(ctx, is.ID), nil
}

// touchLease renews the holder's lease on any write by that agent (SPEC 18.3).
func (s *Service) touchLease(ctx context.Context, a Actor, issueID string) {
	if a.Kind != "agent" {
		return
	}
	s.DB.Exec(ctx, `UPDATE issue_lease SET expires_at=$3, heartbeat_at=$4 WHERE issue_id=$1 AND holder_id=$2`, issueID, a.ID, s.Now().Add(defaultLease), s.Now())
}

// ClaimIssue assigns the issue to the agent and moves it to an in-progress status in one transaction, taking a lease.
// Two agents racing for one issue: exactly one wins. Claiming again as the holder renews the lease (the heartbeat).
func (s *Service) ClaimIssue(ctx context.Context, a Actor, key string, ttl time.Duration) (Issue, error) {
	if ttl <= 0 {
		ttl = defaultLease
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return is, err
	}
	if !canWrite(role) {
		return is, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return is, perr
	}
	defer endWrite()
	pid, _ := s.projectID(ctx, is.Project)
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var statusID, cat string
		var assignee *string
		var ver int64
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT i.status_id,st.category,i.assignee_id,i.version,i.field_ver FROM issue i JOIN status st ON st.id=i.status_id WHERE i.id=$1 AND i.deleted_at IS NULL FOR UPDATE OF i`, is.ID).Scan(&statusID, &cat, &assignee, &ver, &raw); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrGone // only a row that is not there means "deleted": a lock timeout or a dropped connection is an error to report as one
			}
			return err
		}
		if cat == "done" {
			return invalid("the issue is already done; reopen it before claiming")
		}
		var holder string
		var holderName string
		var until time.Time
		if tx.QueryRow(ctx, `SELECT l.holder_id,u.name,l.expires_at FROM issue_lease l JOIN app_user u ON u.id=l.holder_id WHERE l.issue_id=$1 FOR UPDATE OF l`, is.ID).Scan(&holder, &holderName, &until) == nil {
			if holder != a.ID && until.After(s.Now()) {
				return &LeaseHeld{Holder: holderName, Until: until}
			}
		}
		if assignee != nil && *assignee != a.ID && holder == "" {
			// a human or another agent is assigned without a lease: the claim does not take it over
			var an string
			tx.QueryRow(ctx, `SELECT name FROM app_user WHERE id=$1`, *assignee).Scan(&an)
			return &LeaseHeld{Holder: an + " (assigned)", Until: s.Now()}
		}
		if err := s.checkAssignable(ctx, tx, pid, a.ID); err != nil {
			return err
		}
		fv := map[string]int64{}
		jsonUnmarshal(raw, &fv)
		next := ver + 1
		if cat == "todo" {
			var to, toName, fromName string
			err := tx.QueryRow(ctx, `SELECT t.to_status,st.name,(SELECT name FROM status WHERE id=$2) FROM transition t JOIN status st ON st.id=t.to_status
				WHERE t.workflow_id=(SELECT workflow_id FROM project WHERE id=$1) AND (t.from_status IS NULL OR t.from_status=$2) AND st.category='in_progress' ORDER BY st.position LIMIT 1`, pid, statusID).Scan(&to, &toName, &fromName)
			if err != nil {
				return invalid("the workflow has no transition from this status to an in-progress status")
			}
			statusID = to
			fv["status"] = next
			s.hist(ctx, tx, is.ID, a, "status", &fromName, &toName)
		}
		fv["assignee"] = next
		if assignee == nil || *assignee != a.ID {
			s.hist(ctx, tx, is.ID, a, "assignee", sp(""), sp(a.Label))
		}
		fvj := jsonMarshal(fv)
		if _, err := tx.Exec(ctx, `UPDATE issue SET status_id=$2, assignee_id=$3, version=$4, field_ver=$5, updated_at=now() WHERE id=$1`, is.ID, statusID, a.ID, next, fvj); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO issue_lease(issue_id,holder_id,expires_at,heartbeat_at) VALUES($1,$2,$3,$4)
			ON CONFLICT (issue_id) DO UPDATE SET holder_id=$2, expires_at=$3, heartbeat_at=$4`, is.ID, a.ID, s.Now().Add(ttl), s.Now()); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+is.Key, "issue.updated", map[string]any{"key": is.Key, "fields": []string{"status", "assignee"}, "version": next, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return is, err
	}
	out, gerr := s.getIssue(ctx, key)
	if gerr == nil {
		s.emit(Event{Type: "issue.updated", Issue: out.Key, Fields: []string{"status", "assignee"}, Version: out.Version, Actor: a.Label, ActorID: a.ID})
	}
	return out, gerr
}

// ReleaseIssue frees the claim: the lease goes, the agent is unassigned, and an in-progress issue returns to To Do.
func (s *Service) ReleaseIssue(ctx context.Context, a Actor, key string) (Issue, error) {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return is, err
	}
	if !canWrite(role) {
		return is, ErrForbidden
	}
	endWrite, perr := s.admit(ctx, is.Project)
	if perr != nil {
		return is, perr
	}
	defer endWrite()
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var holder *string
		tx.QueryRow(ctx, `SELECT holder_id::text FROM issue_lease WHERE issue_id=$1 FOR UPDATE`, is.ID).Scan(&holder)
		if holder == nil {
			return invalid("the issue has no claim to release")
		}
		if *holder != a.ID && !a.Admin {
			return ErrForbidden // only the holder (or an administrator) releases a claim
		}
		var ver int64
		var raw []byte
		var statusID, cat, statusName string
		tx.QueryRow(ctx, `SELECT i.version,i.field_ver,i.status_id,st.category,st.name FROM issue i JOIN status st ON st.id=i.status_id WHERE i.id=$1 FOR UPDATE OF i`, is.ID).Scan(&ver, &raw, &statusID, &cat, &statusName)
		fv := map[string]int64{}
		jsonUnmarshal(raw, &fv)
		next := ver + 1
		if cat == "in_progress" {
			var to, toName string
			if tx.QueryRow(ctx, `SELECT t.to_status,st.name FROM transition t JOIN status st ON st.id=t.to_status WHERE t.workflow_id=(SELECT workflow_id FROM project WHERE key=$1)
				AND (t.from_status IS NULL OR t.from_status=$2) AND st.category='todo' ORDER BY st.position LIMIT 1`, is.Project, statusID).Scan(&to, &toName) == nil {
				statusID = to
				fv["status"] = next
				s.hist(ctx, tx, is.ID, a, "status", &statusName, &toName)
			}
		}
		fv["assignee"] = next
		s.hist(ctx, tx, is.ID, a, "assignee", sp(a.Label), sp(""))
		if _, err := tx.Exec(ctx, `UPDATE issue SET status_id=$2, assignee_id=NULL, version=$3, field_ver=$4, updated_at=now() WHERE id=$1`, is.ID, statusID, next, jsonMarshal(fv)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM issue_lease WHERE issue_id=$1`, is.ID); err != nil {
			return err
		}
		return outbox(ctx, tx, "issue:"+is.Key, "issue.updated", map[string]any{"key": is.Key, "fields": []string{"status", "assignee"}, "version": next, "actor": a.Label, "actor_id": a.ID})
	})
	if err != nil {
		return is, err
	}
	return s.getIssue(ctx, key)
}

// SetTransitionHuman marks a workflow transition as one only a person may take (administrators).
func (s *Service) SetTransitionHuman(ctx context.Context, a Actor, transitionID string, human bool) error {
	if !a.Admin {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `UPDATE transition SET requires_human=$2 WHERE id=$1`, transitionID, human)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) inputRequest(ctx context.Context, issueID string) *InputRequest {
	var r InputRequest
	var q *string
	if s.DB.QueryRow(ctx, `SELECT COALESCE(u.name,''),COALESCE(i.input_question,''),i.input_requested_at FROM issue i LEFT JOIN app_user u ON u.id=i.input_requested_by WHERE i.id=$1 AND i.input_requested_at IS NOT NULL`, issueID).Scan(&r.By, &r.Question, &r.At) != nil {
		return nil
	}
	_ = q
	return &r
}

// RequestHumanInput flags an issue as waiting for a person, with the question as a comment (which notifies the
// usual recipients). A person's next comment clears the flag; so does ClearHumanInput.
func (s *Service) RequestHumanInput(ctx context.Context, a Actor, key, question string) error {
	question = strings.TrimSpace(question)
	if question == "" {
		return invalid("say what you need from the person")
	}
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	if _, err := s.AddComment(ctx, a, key, "Input needed: "+question, false); err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE issue SET input_requested_at=now(),input_requested_by=$2,input_question=$3 WHERE id=$1`, is.ID, a.ID, question)
	if err == nil {
		s.emit(Event{Type: "issue.updated", Issue: is.Key, Actor: a.Label, ActorID: a.ID})
	}
	return err
}

// ClearHumanInput removes the flag without a comment.
func (s *Service) ClearHumanInput(ctx context.Context, a Actor, key string) error {
	is, role, err := s.access(ctx, a, key)
	if err != nil {
		return err
	}
	if !canWrite(role) {
		return ErrForbidden
	}
	_, err = s.DB.Exec(ctx, `UPDATE issue SET input_requested_at=NULL,input_requested_by=NULL,input_question=NULL WHERE id=$1`, is.ID)
	return err
}

// AgentsKilled reports the instance-wide kill switch.
func (s *Service) AgentsKilled(ctx context.Context) (bool, error) {
	var k bool
	err := s.DB.QueryRow(ctx, `SELECT killed FROM agent_switch WHERE id=1`).Scan(&k)
	return k, err
}

// KillAgents turns every agent off at once (administrators): tokens stop working on the next call and live claims
// are released. Turning it off again restores access; claims are not restored.
func (s *Service) KillAgents(ctx context.Context, a Actor, on bool) error {
	if !a.Admin {
		return ErrForbidden
	}
	if _, err := s.DB.Exec(ctx, `UPDATE agent_switch SET killed=$1,by_id=$2,at=now() WHERE id=1`, on, a.ID); err != nil {
		return err
	}
	if on {
		return s.releaseLeases(ctx, a, "")
	}
	return nil
}

// PauseAgent stops one agent account and releases its claims.
func (s *Service) PauseAgent(ctx context.Context, a Actor, agentID string, paused bool) error {
	if !a.Admin {
		return ErrForbidden
	}
	tag, err := s.DB.Exec(ctx, `UPDATE app_user SET agent_paused=$2 WHERE id=$1 AND kind='agent'`, agentID, paused)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if paused {
		return s.releaseLeases(ctx, a, agentID)
	}
	return nil
}

// releaseLeases frees live claims (of one holder, or all), unassigning and returning the issue to To Do, as a release does.
func (s *Service) releaseLeases(ctx context.Context, admin Actor, holder string) error {
	rows, err := s.DB.Query(ctx, `SELECT p.key||'-'||i.number, l.holder_id::text FROM issue_lease l JOIN issue i ON i.id=l.issue_id JOIN project p ON p.id=i.project_id WHERE $1='' OR l.holder_id::text=$1`, holder)
	if err != nil {
		return err
	}
	type lease struct{ key, holder string }
	var ls []lease
	for rows.Next() {
		var l lease
		rows.Scan(&l.key, &l.holder)
		ls = append(ls, l)
	}
	rows.Close()
	for _, l := range ls {
		var name string
		s.DB.QueryRow(ctx, `SELECT name FROM app_user WHERE id=$1`, l.holder).Scan(&name)
		if _, err := s.ReleaseIssue(ctx, Actor{ID: l.holder, Label: "Agent: " + name, Kind: "agent", Admin: true}, l.key); err != nil {
			return err
		}
	}
	return nil
}
