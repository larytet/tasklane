package domain

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Managed upgrades and support levels for the hosted edition (SPEC 17.9). The operator upgrades and maintains tenants; a plan says how: the support level
// (which sets how much notice a tenant gets), the upgrade channel (stable, or early for tenants who want new releases first) and the weekly window, in UTC,
// in which the operator may work on this tenant. A maintenance notice is a scheduled piece of work: it must lie inside the window, give the notice the
// support level promises, be short, and not overlap another; the tenant's people see it in the app until it is over. The notice announces; the work itself
// is done with the pause (docs/BACKUP.md) and the upgrade procedure (docs/UPGRADE.md).

var supportLeadTime = map[string]time.Duration{"community": 0, "standard": 24 * time.Hour, "premium": 72 * time.Hour}

const maxMaintenance = 8 * time.Hour

type ServiceTerms struct {
	SupportLevel   string             `json:"support_level"`
	UpgradeChannel string             `json:"upgrade_channel"`
	Window         *MaintenanceWindow `json:"window"` // nil: any time
}

type MaintenanceWindow struct {
	Weekday   int `json:"weekday"` // 0 is Sunday, UTC
	StartHour int `json:"start_hour"`
	Hours     int `json:"hours"`
}

// SetServiceTerms sets the support level, upgrade channel and window of a plan (the operator).
func (s *Service) SetServiceTerms(ctx context.Context, a Actor, orgID string, in ServiceTerms) (ServiceTerms, error) {
	if err := s.hostedOnly(a); err != nil {
		return in, err
	}
	if in.SupportLevel == "" {
		in.SupportLevel = "community"
	}
	if in.UpgradeChannel == "" {
		in.UpgradeChannel = "stable"
	}
	if _, ok := supportLeadTime[in.SupportLevel]; !ok {
		return in, invalid("the support level is community, standard or premium")
	}
	if in.UpgradeChannel != "stable" && in.UpgradeChannel != "early" {
		return in, invalid("the upgrade channel is stable or early")
	}
	var wd, sh any
	hours := 2
	if w := in.Window; w != nil {
		if w.Weekday < 0 || w.Weekday > 6 || w.StartHour < 0 || w.StartHour > 23 || w.Hours < 1 || w.Hours > 12 {
			return in, invalid("a window is a weekday 0-6 (0 is Sunday, UTC), a start hour 0-23 and 1 to 12 hours")
		}
		wd, sh, hours = w.Weekday, w.StartHour, w.Hours
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE org_plan SET support_level=$2,upgrade_channel=$3,window_weekday=$4,window_start_hour=$5,window_hours=$6,updated_at=now() WHERE org_id::text=$1`,
			orgID, in.SupportLevel, in.UpgradeChannel, wd, sh, hours)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return invalid("give the organization a plan before its service terms")
		}
		return s.audit(ctx, tx, a, "org.service_terms", orgID, map[string]any{"support": in.SupportLevel, "channel": in.UpgradeChannel, "window": in.Window})
	})
	return in, err
}

func (s *Service) termsOf(ctx context.Context, orgID string) (ServiceTerms, bool) {
	var t ServiceTerms
	var wd, sh *int
	var hours int
	if s.DB.QueryRow(ctx, `SELECT support_level,upgrade_channel,window_weekday,window_start_hour,window_hours FROM org_plan WHERE org_id::text=$1`, orgID).Scan(&t.SupportLevel, &t.UpgradeChannel, &wd, &sh, &hours) != nil {
		return t, false
	}
	if wd != nil && sh != nil {
		t.Window = &MaintenanceWindow{Weekday: *wd, StartHour: *sh, Hours: hours}
	}
	return t, true
}

// GetServiceTerms returns the terms; the operator reads any, an organization's administrator their own.
func (s *Service) GetServiceTerms(ctx context.Context, a Actor, orgID string) (ServiceTerms, error) {
	if !s.Hosted {
		return ServiceTerms{}, ErrNotHosted
	}
	if !s.mayReadInvoices(a, orgID) {
		return ServiceTerms{}, ErrForbidden
	}
	t, ok := s.termsOf(ctx, orgID)
	if !ok {
		return t, ErrNotFound
	}
	return t, nil
}

type MaintenanceNotice struct {
	ID        string     `json:"id"`
	OrgID     string     `json:"org_id"`
	Kind      string     `json:"kind"`
	Starts    time.Time  `json:"starts_at"`
	Ends      time.Time  `json:"ends_at"`
	Message   string     `json:"message"`
	CreatedBy string     `json:"created_by,omitempty"`
	Cancelled *time.Time `json:"cancelled_at,omitempty"`
}

type MaintenanceIn struct {
	Kind    string    `json:"kind"`
	Starts  time.Time `json:"starts_at"`
	Ends    time.Time `json:"ends_at"`
	Message string    `json:"message"`
}

// insideWindow reports whether the whole span lies within one occurrence of the weekly window.
func insideWindow(w MaintenanceWindow, from, to time.Time) bool {
	from, to = from.UTC(), to.UTC()
	// the occurrence that begins on from's day, or the one before it that may still be running
	for back := 0; back <= 1; back++ {
		d := from.AddDate(0, 0, -back)
		if int(d.Weekday()) != w.Weekday {
			continue
		}
		start := time.Date(d.Year(), d.Month(), d.Day(), w.StartHour, 0, 0, 0, time.UTC)
		end := start.Add(time.Duration(w.Hours) * time.Hour)
		if !from.Before(start) && !to.After(end) {
			return true
		}
	}
	return false
}

// ScheduleMaintenance announces work on a tenant (the operator).
func (s *Service) ScheduleMaintenance(ctx context.Context, a Actor, orgID string, in MaintenanceIn) (MaintenanceNotice, error) {
	if err := s.hostedOnly(a); err != nil {
		return MaintenanceNotice{}, err
	}
	if !s.orgExists(ctx, orgID) {
		return MaintenanceNotice{}, ErrNotFound
	}
	t, ok := s.termsOf(ctx, orgID)
	if !ok {
		return MaintenanceNotice{}, invalid("give the organization a plan first")
	}
	if in.Kind == "" {
		in.Kind = "upgrade"
	}
	if in.Kind != "upgrade" && in.Kind != "maintenance" {
		return MaintenanceNotice{}, invalid("the kind is upgrade or maintenance")
	}
	in.Message = strings.TrimSpace(in.Message)
	if len([]rune(in.Message)) > 500 {
		return MaintenanceNotice{}, invalid("the message is at most 500 characters")
	}
	switch {
	case !in.Ends.After(in.Starts):
		return MaintenanceNotice{}, invalid("the work must end after it starts")
	case in.Ends.Sub(in.Starts) > maxMaintenance:
		return MaintenanceNotice{}, invalid("planned work is at most %d hours", int(maxMaintenance.Hours()))
	case in.Starts.Before(s.Now().Add(supportLeadTime[t.SupportLevel])):
		return MaintenanceNotice{}, invalid("the %s support level promises %d hours of notice", t.SupportLevel, int(supportLeadTime[t.SupportLevel].Hours()))
	case t.Window != nil && !insideWindow(*t.Window, in.Starts, in.Ends):
		return MaintenanceNotice{}, invalid("the work must lie inside the tenant's window (weekday %d from %02d:00 UTC for %d hours)", t.Window.Weekday, t.Window.StartHour, t.Window.Hours)
	}
	var n MaintenanceNotice
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var clash int
		tx.QueryRow(ctx, `SELECT count(*) FROM maintenance_notice WHERE org_id::text=$1 AND cancelled_at IS NULL AND starts_at < $3 AND ends_at > $2`, orgID, in.Starts, in.Ends).Scan(&clash)
		if clash > 0 {
			return &ConflictError{"other work is already announced for that time"}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO maintenance_notice(org_id,kind,starts_at,ends_at,message,created_by) VALUES($1::uuid,$2,$3,$4,$5,$6) RETURNING id::text`,
			orgID, in.Kind, in.Starts, in.Ends, in.Message, a.Label).Scan(&n.ID); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "maintenance.scheduled", n.ID, map[string]any{"org": orgID, "kind": in.Kind, "starts": in.Starts, "ends": in.Ends})
	})
	if err != nil {
		return n, err
	}
	return s.loadNotice(ctx, n.ID)
}

func (s *Service) loadNotice(ctx context.Context, id string) (MaintenanceNotice, error) {
	var n MaintenanceNotice
	err := s.DB.QueryRow(ctx, `SELECT id::text,org_id::text,kind,starts_at,ends_at,message,created_by,cancelled_at FROM maintenance_notice WHERE id::text=$1`, id).
		Scan(&n.ID, &n.OrgID, &n.Kind, &n.Starts, &n.Ends, &n.Message, &n.CreatedBy, &n.Cancelled)
	if err != nil {
		return n, ErrNotFound
	}
	return n, nil
}

// CancelMaintenance withdraws a notice (the operator), with a reason.
func (s *Service) CancelMaintenance(ctx context.Context, a Actor, orgID, id, reason string) (MaintenanceNotice, error) {
	if err := s.hostedOnly(a); err != nil {
		return MaintenanceNotice{}, err
	}
	n, err := s.loadNotice(ctx, id)
	if err != nil || n.OrgID != orgID {
		return MaintenanceNotice{}, ErrNotFound
	}
	if strings.TrimSpace(reason) == "" {
		return n, invalid("say why the work is cancelled")
	}
	if n.Cancelled != nil {
		return n, nil
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE maintenance_notice SET cancelled_at=$2,cancel_reason=$3 WHERE id::text=$1`, id, s.Now(), reason); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, "maintenance.cancelled", id, map[string]any{"org": orgID, "reason": reason})
	})
	if err != nil {
		return n, err
	}
	return s.loadNotice(ctx, id)
}

func (s *Service) noticesWhere(ctx context.Context, orgID string, upcomingOnly bool) ([]MaintenanceNotice, error) {
	q := `SELECT id::text FROM maintenance_notice WHERE org_id::text=$1`
	args := []any{orgID}
	if upcomingOnly {
		q += ` AND cancelled_at IS NULL AND ends_at > $2 AND starts_at < $3`
		args = append(args, s.Now(), s.Now().AddDate(0, 0, 14))
	}
	rows, err := s.DB.Query(ctx, q+` ORDER BY starts_at`, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := []MaintenanceNotice{}
	for _, id := range ids {
		n, err := s.loadNotice(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// ListMaintenance lists all the notices of an organization (the operator, or that organization's administrator).
func (s *Service) ListMaintenance(ctx context.Context, a Actor, orgID string) ([]MaintenanceNotice, error) {
	if !s.Hosted {
		return nil, ErrNotHosted
	}
	if !s.mayReadInvoices(a, orgID) {
		return nil, ErrForbidden
	}
	return s.noticesWhere(ctx, orgID, false)
}

// UpcomingMaintenance is what a signed-in person is told is coming (their organization's, within two weeks, not cancelled); empty unless hosted.
func (s *Service) UpcomingMaintenance(ctx context.Context, a Actor) []MaintenanceNotice {
	if !s.Hosted || a.OrgID() == "" {
		return []MaintenanceNotice{}
	}
	n, err := s.noticesWhere(ctx, a.OrgID(), true)
	if err != nil {
		return []MaintenanceNotice{}
	}
	return n
}
