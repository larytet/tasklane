package domain

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The hosted edition's plans, quotas and metering (SPEC 17.9). The same build runs a self-hosted installation and a hosted service; everything here
// is inactive unless the operator turns hosting on (`hosted=true`): without it the plan screens and endpoints do not exist, no snapshot is taken, and
// no limit applies. Where hosting is on, an organization with no plan has no limits and one with a plan is held to it: seats (active staff; agents,
// portal accounts and customers are not seats) and projects by the database, so every path meets them, storage when a file is registered. A tenant
// is placed in a region, and a deployment serves one.

// ErrNotHosted is returned by hosted-only operations on a self-hosted installation; it reads as not found.
var ErrNotHosted = ErrNotFound

// PlanLimitError says which limit a plan stopped (HTTP 402).
type PlanLimitError struct {
	Resource string // seats, projects or storage
	Limit    int64
	Used     int64
	Add      int64
}

func (e *PlanLimitError) Error() string {
	switch e.Resource {
	case "storage":
		return fmt.Sprintf("your plan's storage of %s is full (%s used, this needs %s): free some space or ask for a larger plan", human(e.Limit), human(e.Used), human(e.Add))
	case "seats":
		return fmt.Sprintf("your plan has %d seat(s) and all are in use: deactivate someone or ask for a larger plan", e.Limit)
	}
	return fmt.Sprintf("your plan allows %d %s and you have %d: ask for a larger plan", e.Limit, e.Resource, e.Used)
}

var planLimitRe = regexp.MustCompile(`plan limit: (\w+) \((\d+) of (\d+)\)`)

// planErr turns the database's refusal of a limit into a PlanLimitError; anything else passes through.
func planErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "TL001" {
		if m := planLimitRe.FindStringSubmatch(pe.Message); m != nil {
			used, _ := strconv.ParseInt(m[2], 10, 64)
			lim, _ := strconv.ParseInt(m[3], 10, 64)
			return &PlanLimitError{Resource: m[1], Limit: lim, Used: used}
		}
	}
	return err
}

type OrgPlan struct {
	OrgID         string     `json:"org_id"`
	Plan          string     `json:"plan"`
	MaxSeats      *int       `json:"max_seats"`
	MaxProjects   *int       `json:"max_projects"`
	MaxStorageMiB *int64     `json:"max_storage_mib"`
	Region        string     `json:"region"`
	Updated       *time.Time `json:"updated,omitempty"`
	Prices        *Prices    `json:"prices,omitempty"`
}

func (s *Service) hostedOnly(a Actor) error {
	if !s.Hosted {
		return ErrNotHosted
	}
	return adminOnly(a)
}

func (s *Service) orgExists(ctx context.Context, id string) bool {
	var ok bool
	s.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization WHERE id::text=$1)`, id).Scan(&ok)
	return ok
}

// GetOrgPlan returns an organization's plan (the operator's view); an organization with none has no limits.
func (s *Service) GetOrgPlan(ctx context.Context, a Actor, orgID string) (OrgPlan, error) {
	if err := s.hostedOnly(a); err != nil {
		return OrgPlan{}, err
	}
	if !s.orgExists(ctx, orgID) {
		return OrgPlan{}, ErrNotFound
	}
	return s.orgPlan(ctx, orgID), nil
}

func (s *Service) orgPlan(ctx context.Context, orgID string) OrgPlan {
	p := OrgPlan{OrgID: orgID}
	var bytes *int64
	var upd time.Time
	if s.DB.QueryRow(ctx, `SELECT plan,max_seats,max_projects,max_storage_bytes,region,updated_at FROM org_plan WHERE org_id::text=$1`, orgID).Scan(&p.Plan, &p.MaxSeats, &p.MaxProjects, &bytes, &p.Region, &upd) == nil {
		p.Updated = &upd
		if pr, ok := s.pricesOf(ctx, orgID); ok {
			p.Prices = &pr
		}
		if bytes != nil {
			m := *bytes >> 20
			p.MaxStorageMiB = &m
		}
	}
	return p
}

// SetOrgPlan sets or replaces a plan. Lowering a limit below what is used does not remove anything: it stops new use until the tenant is under it.
func (s *Service) SetOrgPlan(ctx context.Context, a Actor, orgID string, in OrgPlan) (OrgPlan, error) {
	if err := s.hostedOnly(a); err != nil {
		return OrgPlan{}, err
	}
	if !s.orgExists(ctx, orgID) {
		return OrgPlan{}, ErrNotFound
	}
	in.Plan = strings.TrimSpace(in.Plan)
	in.Region = strings.ToLower(strings.TrimSpace(in.Region))
	if len(in.Plan) > 60 || len(in.Region) > 40 {
		return OrgPlan{}, invalid("a plan name is at most 60 characters and a region at most 40")
	}
	if (in.MaxSeats != nil && *in.MaxSeats < 0) || (in.MaxProjects != nil && *in.MaxProjects < 0) || (in.MaxStorageMiB != nil && (*in.MaxStorageMiB < 0 || *in.MaxStorageMiB > 1<<30)) {
		return OrgPlan{}, invalid("limits are zero or more")
	}
	if s.HostedRegion != "" && in.Region != "" && in.Region != s.HostedRegion {
		return OrgPlan{}, invalid("this deployment serves the region %q: a tenant in %q belongs on a deployment there", s.HostedRegion, in.Region)
	}
	if in.Region == "" {
		in.Region = s.HostedRegion
	}
	var bytes any
	if in.MaxStorageMiB != nil {
		bytes = *in.MaxStorageMiB << 20
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO org_plan(org_id,plan,max_seats,max_projects,max_storage_bytes,region) VALUES($1::uuid,$2,$3,$4,$5,$6)
			ON CONFLICT (org_id) DO UPDATE SET plan=$2,max_seats=$3,max_projects=$4,max_storage_bytes=$5,region=$6,updated_at=now()`, orgID, in.Plan, in.MaxSeats, in.MaxProjects, bytes, in.Region); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "org.plan_set", orgID, map[string]any{"plan": in.Plan, "seats": in.MaxSeats, "projects": in.MaxProjects, "storage_mib": in.MaxStorageMiB, "region": in.Region})
	})
	if err != nil {
		return OrgPlan{}, err
	}
	return s.orgPlan(ctx, orgID), nil
}

// ClearOrgPlan removes a plan: the organization has no limits again.
func (s *Service) ClearOrgPlan(ctx context.Context, a Actor, orgID string) error {
	if err := s.hostedOnly(a); err != nil {
		return err
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM org_plan WHERE org_id::text=$1`, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return s.audit(ctx, tx, a, "org.plan_cleared", orgID, map[string]any{})
	})
}

// Usage is what an organization uses, as billing counts it.
type Usage struct {
	OrgID           string   `json:"org_id"`
	Day             string   `json:"day,omitempty"`
	Seats           int      `json:"seats"`
	Projects        int      `json:"projects"`
	Issues          int64    `json:"issues"`
	StorageBytes    int64    `json:"storage_bytes"`
	ActiveCustomers int      `json:"active_customers"` // customers who signed in during the last 30 days: metered apart from seats
	Plan            *OrgPlan `json:"plan,omitempty"`
}

const activeCustomerWindow = 30 * 24 * time.Hour

func (s *Service) usageOf(ctx context.Context, orgID string) (Usage, error) {
	u := Usage{OrgID: orgID}
	err := s.DB.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM app_user WHERE org_id::text=$1 AND active AND kind='human'),
		(SELECT count(*) FROM project WHERE org_id::text=$1 AND NOT archived),
		(SELECT count(*) FROM issue i JOIN project p ON p.id=i.project_id WHERE p.org_id::text=$1 AND i.deleted_at IS NULL),
		(SELECT COALESCE(sum(b.size),0) FROM attachment a JOIN blob b ON b.id=a.blob_id JOIN issue i ON i.id=a.issue_id JOIN project p ON p.id=i.project_id WHERE p.org_id::text=$1),
		(SELECT count(*) FROM customer c WHERE c.org_id::text=$1 AND c.erased_at IS NULL AND c.last_seen > $2)`, orgID, s.Now().Add(-activeCustomerWindow)).
		Scan(&u.Seats, &u.Projects, &u.Issues, &u.StorageBytes, &u.ActiveCustomers)
	return u, err
}

// OrgUsage reports an organization's use now, with its plan. The operator may ask about any organization and an organization's administrator about their own.
func (s *Service) OrgUsage(ctx context.Context, a Actor, orgID string) (Usage, error) {
	if !s.Hosted {
		return Usage{}, ErrNotHosted
	}
	if !(a.Admin || (a.OrgAdmin && a.OrgID() == orgID)) {
		return Usage{}, ErrForbidden
	}
	if !s.orgExists(ctx, orgID) {
		return Usage{}, ErrNotFound
	}
	u, err := s.usageOf(ctx, orgID)
	if err != nil {
		return u, err
	}
	p := s.orgPlan(ctx, orgID)
	u.Plan = &p
	return u, nil
}

// UsageHistory returns the daily snapshots of the last days (newest first).
func (s *Service) UsageHistory(ctx context.Context, a Actor, orgID string, days int) ([]Usage, error) {
	if !s.Hosted {
		return nil, ErrNotHosted
	}
	if !(a.Admin || (a.OrgAdmin && a.OrgID() == orgID)) {
		return nil, ErrForbidden
	}
	if days <= 0 || days > 400 {
		days = 31
	}
	rows, err := s.DB.Query(ctx, `SELECT day::text,seats,projects,issues,storage_bytes,active_customers FROM usage_snapshot WHERE org_id::text=$1 AND day > $2::date ORDER BY day DESC`, orgID, s.Now().AddDate(0, 0, -days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Usage{}
	for rows.Next() {
		u := Usage{OrgID: orgID}
		if err := rows.Scan(&u.Day, &u.Seats, &u.Projects, &u.Issues, &u.StorageBytes, &u.ActiveCustomers); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecordUsage writes today's snapshot for every organization (rewriting it while the day is open) and returns how many it wrote.
func (s *Service) RecordUsage(ctx context.Context) (int, error) {
	if !s.Hosted {
		return 0, ErrNotHosted
	}
	rows, err := s.DB.Query(ctx, `SELECT id::text FROM organization`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	day := s.Now().UTC().Format("2006-01-02")
	for _, id := range ids {
		u, err := s.usageOf(ctx, id)
		if err != nil {
			return 0, err
		}
		if _, err := s.DB.Exec(ctx, `INSERT INTO usage_snapshot(org_id,day,seats,projects,issues,storage_bytes,active_customers) VALUES($1::uuid,$2::date,$3,$4,$5,$6,$7)
			ON CONFLICT (org_id,day) DO UPDATE SET seats=$3,projects=$4,issues=$5,storage_bytes=$6,active_customers=$7`, id, day, u.Seats, u.Projects, u.Issues, u.StorageBytes, u.ActiveCustomers); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// RunMetering takes a snapshot every hour (hosted installations only).
func (s *Service) RunMetering(ctx context.Context, logf func(string, ...any)) {
	if !s.Hosted {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, err := s.RecordUsage(ctx); err != nil && ctx.Err() == nil {
			logf("metering: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkOrgStorage refuses a file that would take the project's organization past its plan's storage.
func (s *Service) checkOrgStorage(ctx context.Context, projectKey string, add int64) error {
	if !s.Hosted {
		return nil
	}
	var org string
	var lim *int64
	if s.DB.QueryRow(ctx, `SELECT p.org_id::text, pl.max_storage_bytes FROM project p LEFT JOIN org_plan pl ON pl.org_id=p.org_id WHERE p.key=$1`, strings.ToUpper(projectKey)).Scan(&org, &lim) != nil || lim == nil {
		return nil
	}
	u, err := s.usageOf(ctx, org)
	if err != nil {
		return err
	}
	if u.StorageBytes+add > *lim {
		return &PlanLimitError{Resource: "storage", Limit: *lim, Used: u.StorageBytes, Add: add}
	}
	return nil
}
