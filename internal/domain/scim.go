package domain

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SCIM 2.0 provisioning of users (RFC 7643, 7644): the identity provider creates, updates and deactivates accounts. A
// deactivated account's sessions and API tokens end at once. SCIM never makes an administrator and never removes data.

type SCIMToken struct {
	ID      int64      `json:"id"`
	Name    string     `json:"name"`
	Created time.Time  `json:"created"`
	Revoked *time.Time `json:"revoked,omitempty"`
}

func (s *Service) CreateSCIMToken(ctx context.Context, a Actor, name string) (string, SCIMToken, error) {
	if err := adminOnly(a); err != nil {
		return "", SCIMToken{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", SCIMToken{}, invalid("a token needs a name of up to 100 characters")
	}
	tok := "scim_" + newToken()
	var t SCIMToken
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO scim_token(name,token_hash) VALUES($1,$2) RETURNING id,name,created_at`, name, hashToken(tok)).Scan(&t.ID, &t.Name, &t.Created); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "scim.token_created", fmt.Sprint(t.ID), map[string]any{"name": name})
	})
	return tok, t, err
}

func (s *Service) ListSCIMTokens(ctx context.Context, a Actor) ([]SCIMToken, error) {
	if err := adminOnly(a); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT id,name,created_at,revoked_at FROM scim_token ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SCIMToken{}
	for rows.Next() {
		var t SCIMToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Created, &t.Revoked); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) RevokeSCIMToken(ctx context.Context, a Actor, id int64) error {
	if err := adminOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE scim_token SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL`, id, s.Now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "scim.token_revoked", fmt.Sprint(id), map[string]any{})
	})
}

// AuthenticateSCIM accepts a SCIM bearer token only; an ordinary session or API token is not one.
func (s *Service) AuthenticateSCIM(ctx context.Context, tok string) error {
	if !strings.HasPrefix(tok, "scim_") {
		return ErrForbidden
	}
	var h []byte
	var rev *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT token_hash,revoked_at FROM scim_token WHERE token_hash=$1`, hashToken(tok)).Scan(&h, &rev); err != nil || rev != nil {
		return ErrForbidden
	}
	if subtle.ConstantTimeCompare(h, hashToken(tok)) != 1 {
		return ErrForbidden
	}
	return nil
}

type SCIMUser struct {
	ID          string
	UserName    string // the email address
	ExternalID  string
	DisplayName string
	Active      bool
	Created     time.Time
}

const scimCols = `id::text,email,COALESCE(external_id,''),name,active,created_at`

func scanSCIM(row pgx.Row) (SCIMUser, error) {
	var u SCIMUser
	err := row.Scan(&u.ID, &u.UserName, &u.ExternalID, &u.DisplayName, &u.Active, &u.Created)
	if err == pgx.ErrNoRows {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Service) SCIMGet(ctx context.Context, id string) (SCIMUser, error) {
	return scanSCIM(s.DB.QueryRow(ctx, `SELECT `+scimCols+` FROM app_user WHERE id::text=$1 AND kind='human'`, id))
}

// SCIMList returns users, optionally those with one userName or externalId (the filters identity providers use).
func (s *Service) SCIMList(ctx context.Context, attr, value string, start, count int) ([]SCIMUser, int, error) {
	where, args := "kind='human'", []any{}
	switch attr {
	case "":
	case "userName":
		where += " AND lower(email)=lower($1)"
		args = append(args, value)
	case "externalId":
		where += " AND external_id=$1"
		args = append(args, value)
	default:
		return nil, 0, invalid("only userName and externalId filters are supported")
	}
	var total int
	if err := s.DB.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if start < 1 {
		start = 1
	}
	if count < 0 || count > 100 {
		count = 100
	}
	rows, err := s.DB.Query(ctx, `SELECT `+scimCols+` FROM app_user WHERE `+where+` ORDER BY created_at,id LIMIT `+strconv.Itoa(count)+` OFFSET `+strconv.Itoa(start-1), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []SCIMUser{}
	for rows.Next() {
		u, err := scanSCIM(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

type SCIMIn struct {
	UserName    string
	ExternalID  string
	DisplayName string
	Active      *bool
}

var scimActor = Actor{Label: "SCIM", Kind: "bot"}

// SCIMCreate makes an account, or takes over one that exists: an inactive one is reactivated and an existing one is
// linked to the identity provider's id. An account that is already linked to a different external id is a conflict.
func (s *Service) SCIMCreate(ctx context.Context, in SCIMIn) (SCIMUser, bool, error) {
	email := strings.ToLower(strings.TrimSpace(in.UserName))
	if !strings.Contains(email, "@") {
		return SCIMUser{}, false, invalid("userName must be an email address")
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" {
		name = email[:strings.Index(email, "@")]
	}
	active := in.Active == nil || *in.Active
	var out SCIMUser
	created := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		var ext *string
		err := tx.QueryRow(ctx, `SELECT id::text,external_id FROM app_user WHERE email=$1 FOR UPDATE`, email).Scan(&id, &ext)
		if err == pgx.ErrNoRows {
			if err := tx.QueryRow(ctx, `INSERT INTO app_user(email,name,source,external_id,active) VALUES($1,$2,'scim',NULLIF($3,''),$4) RETURNING id::text`, email, name, in.ExternalID, active).Scan(&id); err != nil {
				return err
			}
			created = true
		} else if err != nil {
			return err
		} else {
			if ext != nil && *ext != "" && in.ExternalID != "" && *ext != in.ExternalID {
				return &ConflictError{"this account is linked to a different identity"}
			}
			if _, err := tx.Exec(ctx, `UPDATE app_user SET external_id=COALESCE(NULLIF($2,''),external_id),active=$3 WHERE id::text=$1`, id, in.ExternalID, active); err != nil {
				return err
			}
			if !active {
				if err := s.endAccess(ctx, tx, id); err != nil {
					return err
				}
			}
		}
		if err := s.audit(ctx, tx, scimActor, "scim.user_provisioned", id, map[string]any{"email": email, "created": created, "active": active}); err != nil {
			return err
		}
		out, err = scanSCIM(tx.QueryRow(ctx, `SELECT `+scimCols+` FROM app_user WHERE id::text=$1`, id))
		return err
	})
	return out, created, err
}

// endAccess ends every session and API token of an account (deprovisioning).
func (s *Service) endAccess(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM session WHERE user_id::text=$1`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE api_token SET revoked_at=$2 WHERE user_id::text=$1 AND revoked_at IS NULL`, userID, s.Now()); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE oauth_grant SET revoked_at=$2 WHERE user_id::text=$1 AND revoked_at IS NULL`, userID, s.Now())
	return err
}

// SCIMUpdate changes the given fields of an account. Deactivating it ends its access; the last administrator cannot be deactivated.
func (s *Service) SCIMUpdate(ctx context.Context, id string, in SCIMIn) (SCIMUser, error) {
	var out SCIMUser
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var admin, active bool
		if err := tx.QueryRow(ctx, `SELECT is_admin,active FROM app_user WHERE id::text=$1 AND kind='human' FOR UPDATE`, id).Scan(&admin, &active); err != nil {
			return ErrNotFound
		}
		if in.Active != nil && !*in.Active && admin && active {
			var others int
			tx.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE is_admin AND active AND id::text<>$1`, id).Scan(&others)
			if others == 0 {
				return &ConflictError{"the last administrator cannot be deactivated"}
			}
		}
		if in.UserName != "" {
			em := strings.ToLower(strings.TrimSpace(in.UserName))
			if !strings.Contains(em, "@") {
				return invalid("userName must be an email address")
			}
			if _, err := tx.Exec(ctx, `UPDATE app_user SET email=$2 WHERE id::text=$1`, id, em); err != nil {
				if strings.Contains(err.Error(), "app_user_email_key") {
					return &ConflictError{"another account has this userName"}
				}
				return err
			}
		}
		if in.DisplayName != "" {
			if _, err := tx.Exec(ctx, `UPDATE app_user SET name=$2 WHERE id::text=$1`, id, strings.TrimSpace(in.DisplayName)); err != nil {
				return err
			}
		}
		if in.ExternalID != "" {
			if _, err := tx.Exec(ctx, `UPDATE app_user SET external_id=$2 WHERE id::text=$1`, id, in.ExternalID); err != nil {
				return err
			}
		}
		if in.Active != nil {
			if _, err := tx.Exec(ctx, `UPDATE app_user SET active=$2 WHERE id::text=$1`, id, *in.Active); err != nil {
				return err
			}
			if !*in.Active {
				if err := s.endAccess(ctx, tx, id); err != nil {
					return err
				}
			}
		}
		if err := s.audit(ctx, tx, scimActor, "scim.user_updated", id, map[string]any{"active": in.Active}); err != nil {
			return err
		}
		var err error
		out, err = scanSCIM(tx.QueryRow(ctx, `SELECT `+scimCols+` FROM app_user WHERE id::text=$1`, id))
		return err
	})
	return out, err
}

// SCIMDelete deactivates the account: nothing is removed, so history and what the person owned stay.
func (s *Service) SCIMDelete(ctx context.Context, id string) error {
	f := false
	_, err := s.SCIMUpdate(ctx, id, SCIMIn{Active: &f})
	return err
}
