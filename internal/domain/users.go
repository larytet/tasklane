package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/larytet/tasklane/internal/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Admin bool   `json:"is_admin"`
}

func (s *Service) CreateUser(ctx context.Context, email, name, password string, admin bool) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || name == "" {
		return User{}, invalid("email and name are required")
	}
	if err := s.checkPassword(email, password); err != nil {
		return User{}, err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	u := User{Email: email, Name: name, Kind: "human", Admin: admin}
	err = s.DB.QueryRow(ctx, `INSERT INTO app_user(email,name,password_hash,is_admin) VALUES($1,$2,$3,$4) RETURNING id`,
		email, name, string(h), admin).Scan(&u.ID)
	if err != nil && strings.Contains(err.Error(), "app_user_email_key") {
		// an inactive placeholder with no credentials (created by an import or the Jira mirror for someone who had no
		// account yet) is claimed by the person it stood for: their history stays attached
		cerr := s.DB.QueryRow(ctx, `UPDATE app_user SET name=$2,password_hash=$3,is_admin=$4,active=true WHERE email=$1 AND NOT active AND password_hash IS NULL AND kind='human' RETURNING id`,
			email, name, string(h), admin).Scan(&u.ID)
		if cerr != nil {
			return User{}, invalid("a user with this email already exists")
		}
		err = nil
	}
	if err == nil {
		s.queueAccountMail(ctx, u)
	}
	return u, err
}

func hashToken(t string) []byte { h := sha256.Sum256([]byte(t)); return h[:] }

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) Logout(ctx context.Context, tok string) {
	s.DB.Exec(ctx, `DELETE FROM session WHERE token_hash=$1`, hashToken(tok))
}

// Authenticate resolves a session or API token to an actor.
func (s *Service) Authenticate(ctx context.Context, tok string) (Actor, error) {
	if tok == "" {
		return Actor{}, ErrForbidden
	}
	var u User
	var label string
	err := s.DB.QueryRow(ctx, `SELECT u.id,u.email,u.name,u.kind,u.is_admin FROM session s JOIN app_user u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.expires_at>$2 AND u.active`, hashToken(tok), s.Now()).Scan(&u.ID, &u.Email, &u.Name, &u.Kind, &u.Admin)
	if err == nil {
		s.DB.Exec(ctx, `UPDATE session SET last_seen_at=$2 WHERE token_hash=$1 AND (last_seen_at IS NULL OR last_seen_at < $2::timestamptz - interval '5 minutes')`, hashToken(tok), s.Now())
		return Actor{ID: u.ID, Label: u.Name, Admin: u.Admin, Kind: u.Kind}, nil
	}
	if err != pgx.ErrNoRows {
		return Actor{}, err
	}
	var scoped bool
	var exp, revoked *time.Time
	err = s.DB.QueryRow(ctx, `SELECT u.id,u.name,u.kind,u.is_admin,t.name,t.scopes IS NOT NULL,t.expires_at,t.revoked_at FROM api_token t JOIN app_user u ON u.id=t.user_id
		WHERE t.token_hash=$1 AND u.active`, hashToken(tok)).Scan(&u.ID, &u.Name, &u.Kind, &u.Admin, &label, &scoped, &exp, &revoked)
	if err == pgx.ErrNoRows {
		return Actor{}, ErrForbidden
	}
	if err != nil {
		return Actor{}, err
	}
	// an expired or revoked token is dead everywhere; a scoped (agent) token is for the MCP server, whose scopes it
	// enforces, and is not a general REST credential
	if revoked != nil || exp != nil && !exp.After(s.Now()) || scoped {
		return Actor{}, ErrForbidden
	}
	s.DB.Exec(ctx, `UPDATE api_token SET last_used_at=$2 WHERE token_hash=$1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '5 minutes')`, hashToken(tok), s.Now())
	return Actor{ID: u.ID, Label: "API: " + label, Admin: u.Admin, Kind: "bot"}, nil
}

// CreateAPIToken returns a new token; only its hash is stored.
func (s *Service) CreateAPIToken(ctx context.Context, userID, name string) (string, error) {
	tok := "tl_" + newToken()
	_, err := s.DB.Exec(ctx, `INSERT INTO api_token(token_hash,user_id,name) VALUES($1,$2,$3)`, hashToken(tok), userID, name)
	return tok, err
}

func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.DB.Query(ctx, `SELECT id,email,name,kind,is_admin FROM app_user WHERE active ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		rows.Scan(&u.ID, &u.Email, &u.Name, &u.Kind, &u.Admin)
		out = append(out, u)
	}
	return out, rows.Err()
}

func asPermanent(err error, target **mail.Permanent) bool { return errors.As(err, target) }
