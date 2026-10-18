package domain

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/larytet/tasklane/internal/search/tql"
)

// Saved filters (SPEC 10, 11): a named TQL query. Private to its owner, or shared with the owner's organization (read-only for the
// others). A filter is only a query: whoever runs it gets what THEY may see, so sharing one never shares data.

type SavedFilter struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	TQL     string    `json:"tql"`
	Shared  string    `json:"shared"` // private or org
	Owner   string    `json:"owner"`
	Mine    bool      `json:"mine"`
	Updated time.Time `json:"updated_at"`
	InUse   int       `json:"used_by_dashboards"`
}

type FilterIn struct {
	Name   string `json:"name"`
	TQL    string `json:"tql"`
	Shared string `json:"shared"`
}

func (in *FilterIn) clean() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		return invalid("a filter needs a name of up to 100 characters")
	}
	in.TQL = strings.TrimSpace(in.TQL)
	if len(in.TQL) > 2000 {
		return invalid("the query is at most 2000 characters")
	}
	if _, err := tql.Parse(in.TQL); err != nil { // a filter that cannot run is refused when it is saved, not when someone opens it
		return invalid("the query is not valid: %v", err)
	}
	if in.Shared == "" {
		in.Shared = "private"
	}
	if in.Shared != "private" && in.Shared != "org" {
		return invalid("shared is private or org")
	}
	return nil
}

func (s *Service) ListFilters(ctx context.Context, a Actor) ([]SavedFilter, error) {
	rows, err := s.DB.Query(ctx, `SELECT f.id::text,f.name,f.tql,f.shared,u.name,f.owner_id::text=$1,f.updated_at,
		(SELECT count(DISTINCT g.dashboard_id) FROM dashboard_gadget g WHERE g.config->>'filter_id'=f.id::text)
		FROM saved_filter f JOIN app_user u ON u.id=f.owner_id
		WHERE f.owner_id::text=$1 OR (f.shared='org' AND f.org_id::text=$2) ORDER BY lower(f.name)`, a.ID, a.OrgID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedFilter{}
	for rows.Next() {
		var f SavedFilter
		if err := rows.Scan(&f.ID, &f.Name, &f.TQL, &f.Shared, &f.Owner, &f.Mine, &f.Updated, &f.InUse); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// filterFor loads a filter the actor may use (their own, or shared in their organization).
func (s *Service) filterFor(ctx context.Context, a Actor, id string) (SavedFilter, string, error) {
	var f SavedFilter
	var owner string
	err := s.DB.QueryRow(ctx, `SELECT f.id::text,f.name,f.tql,f.shared,u.name,f.owner_id::text,f.updated_at FROM saved_filter f JOIN app_user u ON u.id=f.owner_id
		WHERE f.id::text=$1 AND (f.owner_id::text=$2 OR (f.shared='org' AND f.org_id::text=$3))`, id, a.ID, a.OrgID()).Scan(&f.ID, &f.Name, &f.TQL, &f.Shared, &f.Owner, &owner, &f.Updated)
	if err != nil {
		return f, "", ErrNotFound
	}
	f.Mine = owner == a.ID
	return f, owner, nil
}

func (s *Service) CreateFilter(ctx context.Context, a Actor, in FilterIn) (SavedFilter, error) {
	if err := in.clean(); err != nil {
		return SavedFilter{}, err
	}
	var f SavedFilter
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO saved_filter(org_id,owner_id,name,tql,shared) VALUES($1,$2,$3,$4,$5) RETURNING id::text,updated_at`, a.OrgID(), a.ID, in.Name, in.TQL, in.Shared).Scan(&f.ID, &f.Updated); err != nil {
			if strings.Contains(err.Error(), "saved_filter_owner_name_uq") {
				return invalid("you already have a filter called %q", in.Name)
			}
			return err
		}
		return nil
	})
	f.Name, f.TQL, f.Shared, f.Mine = in.Name, in.TQL, in.Shared, true
	return f, err
}

// UpdateFilter changes a filter. The owner and the organization's administrators may. A filter that an organization-wide dashboard
// uses cannot be made private (the dashboard's other viewers would lose what it shows).
func (s *Service) UpdateFilter(ctx context.Context, a Actor, id string, in FilterIn) error {
	if err := in.clean(); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var owner, org string
		if err := tx.QueryRow(ctx, `SELECT owner_id::text,org_id::text FROM saved_filter WHERE id::text=$1 FOR UPDATE`, id).Scan(&owner, &org); err != nil {
			return ErrNotFound
		}
		if org != a.OrgID() && !a.Admin {
			return ErrNotFound
		}
		if owner != a.ID && !a.OrgAdmin && !a.Admin {
			return ErrForbidden
		}
		if in.Shared == "private" {
			var n int
			tx.QueryRow(ctx, `SELECT count(*) FROM dashboard_gadget g JOIN dashboard d ON d.id=g.dashboard_id WHERE g.config->>'filter_id'=$1 AND d.shared='org'`, id).Scan(&n)
			if n > 0 {
				return &ConflictError{Msg: "an organization-wide dashboard uses this filter; make the dashboard private first"}
			}
		}
		_, err := tx.Exec(ctx, `UPDATE saved_filter SET name=$2,tql=$3,shared=$4,updated_at=now() WHERE id::text=$1`, id, in.Name, in.TQL, in.Shared)
		if err != nil && strings.Contains(err.Error(), "saved_filter_owner_name_uq") {
			return invalid("there is already a filter called %q", in.Name)
		}
		return err
	})
}

// DeleteFilter removes a filter nothing uses.
func (s *Service) DeleteFilter(ctx context.Context, a Actor, id string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var owner, org string
		if err := tx.QueryRow(ctx, `SELECT owner_id::text,org_id::text FROM saved_filter WHERE id::text=$1 FOR UPDATE`, id).Scan(&owner, &org); err != nil {
			return ErrNotFound
		}
		if org != a.OrgID() && !a.Admin {
			return ErrNotFound
		}
		if owner != a.ID && !a.OrgAdmin && !a.Admin {
			return ErrForbidden
		}
		var n int
		tx.QueryRow(ctx, `SELECT count(DISTINCT dashboard_id) FROM dashboard_gadget WHERE config->>'filter_id'=$1`, id).Scan(&n)
		if n > 0 {
			return &ConflictError{Msg: "dashboards still use this filter; change or remove those gadgets first"}
		}
		_, err := tx.Exec(ctx, `DELETE FROM saved_filter WHERE id::text=$1`, id)
		return err
	})
}
