package domain_test

import (
	"bytes"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"testing"
	"time"

	"github.com/larytet/tasklane/internal/domain"
	"github.com/larytet/tasklane/internal/testutil"
)

func hostedEnv(t *testing.T) (*testutil.Env, string, domain.Actor) {
	e := testutil.New(t)
	e.WithBlobs()
	e.Svc.Hosted, e.Svc.HostedRegion = true, "eu-west"
	e.Clock.T = time.Now()
	o, err := e.Svc.CreateOrganization(ctx, e.Admin, "Globex", "boss@globex.example", "Boss", "password123")
	if err != nil {
		t.Fatal(err)
	}
	var bid string
	e.Store.Pool.QueryRow(ctx, `SELECT id::text FROM app_user WHERE email='boss@globex.example'`).Scan(&bid)
	boss, _ := e.Svc.ActorFor(ctx, bid)
	return e, o.ID, boss
}

func planLimit(err error) *domain.PlanLimitError {
	var pl *domain.PlanLimitError
	if errors.As(err, &pl) {
		return pl
	}
	return nil
}

func pv[T any](v T) *T { return &v }

// HOST-01: a self-hosted installation has none of it: no endpoints' worth of behaviour, no limits, no snapshots.
func TestHOST01_SelfHostedHasNoPlansNoLimitsNoMetering(t *testing.T) {
	e := testutil.New(t)
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, domain.DefaultOrg, domain.OrgPlan{MaxSeats: pv(1)}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a plan on a self-hosted installation: %v", err)
	}
	if _, err := e.Svc.OrgUsage(ctx, e.Admin, domain.DefaultOrg); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("usage on a self-hosted installation: %v", err)
	}
	if _, err := e.Svc.RecordUsage(ctx); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a snapshot on a self-hosted installation: %v", err)
	}
	// even a plan row that is somehow present limits nothing when hosting is off only for storage; seats and projects are the database's and a
	// self-hosted installation has no rows: prove the unlimited path
	for i := 0; i < 5; i++ {
		if _, err := e.Svc.CreateUserAs(ctx, e.Admin, "u"+string(rune('a'+i))+"@example.com", "U", "password123", false, ""); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	e.Store.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM org_plan)+(SELECT count(*) FROM usage_snapshot)`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d plan or usage rows on a self-hosted installation", rows)
	}
}

func TestHOST01_PlanAdministrationAndRegion(t *testing.T) {
	e, org, boss := hostedEnv(t)
	if _, err := e.Svc.SetOrgPlan(ctx, boss, org, domain.OrgPlan{MaxSeats: pv(3)}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant set its own plan: %v", err)
	}
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{MaxSeats: pv(-1)}); !isInvalid(err) {
		t.Fatalf("a negative limit: %v", err)
	}
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{Region: "us-east"}); !isInvalid(err) || !strings.Contains(err.Error(), "eu-west") {
		t.Fatalf("a tenant in another region: %v", err)
	}
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, "00000000-0000-0000-0000-00000000dead", domain.OrgPlan{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unknown organization: %v", err)
	}
	p, err := e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{Plan: "team", MaxSeats: pv(3), MaxProjects: pv(2), MaxStorageMiB: pv(int64(1))})
	if err != nil || p.Region != "eu-west" || *p.MaxStorageMiB != 1 || p.Plan != "team" {
		t.Fatalf("%+v %v", p, err)
	}
	if got, _ := e.Svc.GetOrgPlan(ctx, e.Admin, org); got.MaxSeats == nil || *got.MaxSeats != 3 {
		t.Fatalf("%+v", got)
	}
	var audited int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_event WHERE action='org.plan_set'`).Scan(&audited)
	if audited != 1 {
		t.Fatalf("%d audit rows", audited)
	}
	if err := e.Svc.ClearOrgPlan(ctx, e.Admin, org); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.ClearOrgPlan(ctx, e.Admin, org); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("clearing twice: %v", err)
	}
}

func TestHOST01_SeatsAndProjectsAreLimitsEveryPathMeets(t *testing.T) {
	e, org, boss := hostedEnv(t)
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{MaxSeats: pv(2), MaxProjects: pv(1)}); err != nil {
		t.Fatal(err)
	}
	// the boss is seat one; seat two goes through
	second, err := e.Svc.CreateUserAs(ctx, boss, "two@globex.example", "Two", "password123", false, "")
	if err != nil {
		t.Fatal(err)
	}
	// the third is refused with the numbers, on the screen's path
	_, err = e.Svc.CreateUserAs(ctx, boss, "three@globex.example", "Three", "password123", false, "")
	if pl := planLimit(err); pl == nil || pl.Resource != "seats" || pl.Limit != 2 {
		t.Fatalf("a third person: %v", err)
	}
	// and on the others: provisioning, and reactivation of a deactivated person
	if _, err := e.Store.Pool.Exec(ctx, `UPDATE app_user SET active=false WHERE id::text=$1`, second.ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := e.Svc.CreateUserAs(ctx, boss, "three@globex.example", "Three", "password123", false, ""); err != nil {
		t.Fatalf("a freed seat was not usable: %v", err)
	}
	_, err = e.Store.Pool.Exec(ctx, `UPDATE app_user SET active=true WHERE id::text=$1`, second.ID)
	if var_pe := (*pgconn.PgError)(nil); !errors.As(err, &var_pe) || var_pe.Code != "TL001" {
		t.Fatalf("a reactivation took a seat that is not free: %v", err)
	}
	// agents, and customers, are not seats
	for i := 0; i < 3; i++ {
		if _, err := e.Svc.CreateAgent(ctx, boss, "bot"+string(rune('a'+i))); err != nil {
			t.Fatalf("an agent counted as a seat: %v", err)
		}
	}
	// another organization is not held to this one's plan
	if _, err := e.Svc.CreateUserAs(ctx, e.Admin, "x1@example.com", "X", "password123", false, ""); err != nil {
		t.Fatal(err)
	}
	// projects
	if _, err := e.Svc.CreateProject(ctx, boss, "GA", "First"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.CreateProject(ctx, boss, "GB", "Second"); planLimit(err) == nil || planLimit(err).Resource != "projects" {
		t.Fatalf("a second project: %v", err)
	}
	// clearing the plan lifts both
	e.Svc.ClearOrgPlan(ctx, e.Admin, org)
	if _, err := e.Svc.CreateProject(ctx, boss, "GB", "Second"); err != nil {
		t.Fatalf("after the plan was cleared: %v", err)
	}
}

func TestHOST01_StorageQuotaAndMetering(t *testing.T) {
	e, org, boss := hostedEnv(t)
	if _, err := e.Svc.CreateProject(ctx, boss, "GS", "Storage"); err != nil {
		t.Fatal(err)
	}
	is, err := e.Svc.CreateIssue(ctx, boss, domain.CreateIn{Project: "GS", Summary: "files"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{MaxStorageMiB: pv(int64(1))}); err != nil {
		t.Fatal(err)
	}
	small := bytes.Repeat([]byte("a"), 600<<10)
	if _, err := e.Svc.AttachStream(ctx, boss, is.Key, "a.txt", "text/plain", bytes.NewReader(small)); err != nil {
		t.Fatal(err)
	}
	other := bytes.Repeat([]byte("b"), 600<<10)
	_, err = e.Svc.AttachStream(ctx, boss, is.Key, "b.txt", "text/plain", bytes.NewReader(other))
	if pl := planLimit(err); pl == nil || pl.Resource != "storage" || pl.Used != int64(len(small)) {
		t.Fatalf("a file past the plan's storage: %v", err)
	}
	// with hosting off the storage limit is not applied (the plan is data the installation does not read)
	e.Svc.Hosted = false
	if _, err := e.Svc.AttachStream(ctx, boss, is.Key, "off.txt", "text/plain", bytes.NewReader(other)); err != nil {
		t.Fatalf("storage limited while hosting is off: %v", err)
	}
	e.Svc.Hosted = true
	e.Store.Pool.Exec(ctx, `DELETE FROM attachment WHERE name='off.txt'`)
	// metering: the numbers are the facts. Two staff would count as seats; agents, customers who are away and the Default organization's rows do not
	if _, err := e.Svc.CreateUserAs(ctx, boss, "m2@globex.example", "M2", "password123", false, ""); err != nil {
		t.Fatal(err)
	}
	e.Svc.CreateAgent(ctx, boss, "meterbot")
	var cid string
	e.Store.Pool.QueryRow(ctx, `INSERT INTO customer(org_id,email,verified_at,last_seen) VALUES($1,'near@c.example',now(),$2) RETURNING id::text`, org, e.Svc.Now().Add(-24*time.Hour)).Scan(&cid)
	e.Store.Pool.Exec(ctx, `INSERT INTO customer(org_id,email,verified_at,last_seen) VALUES($1,'far@c.example',now(),$2)`, org, e.Svc.Now().Add(-90*24*time.Hour))
	u, err := e.Svc.OrgUsage(ctx, boss, org)
	if err != nil {
		t.Fatal(err)
	}
	if u.Seats != 2 || u.Projects != 1 || u.Issues != 1 || u.StorageBytes != int64(len(small)) || u.ActiveCustomers != 1 || u.Plan == nil || *u.Plan.MaxStorageMiB != 1 {
		t.Fatalf("usage: %+v", u)
	}
	if _, err := e.Svc.OrgUsage(ctx, e.Alice, org); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member read another organization's usage: %v", err)
	}
	if _, err := e.Svc.OrgUsage(ctx, boss, domain.DefaultOrg); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("an organization's administrator read another's usage: %v", err)
	}
	// snapshots: one per organization per day, rewritten while the day is open, history newest first
	n, err := e.Svc.RecordUsage(ctx)
	if err != nil || n < 2 {
		t.Fatalf("%d %v", n, err)
	}
	e.Svc.AttachStream(ctx, boss, is.Key, "c.txt", "text/plain", strings.NewReader("tiny")) // storage changes during the day
	if _, err := e.Svc.RecordUsage(ctx); err != nil {
		t.Fatal(err)
	}
	e.Clock.T = e.Clock.T.Add(24 * time.Hour)
	if _, err := e.Svc.RecordUsage(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := e.Svc.UsageHistory(ctx, boss, org, 7)
	if err != nil || len(h) != 2 {
		t.Fatalf("history: %+v %v", h, err)
	}
	if h[0].StorageBytes != int64(len(small))+4 || h[1].StorageBytes != int64(len(small))+4 || h[0].Day <= h[1].Day {
		t.Fatalf("the snapshots: %+v", h)
	}
	var rows int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM usage_snapshot WHERE org_id::text=$1`, org).Scan(&rows)
	if rows != 2 {
		t.Fatalf("%d rows for two days: a day's snapshot was written twice", rows)
	}
}

// SCIM provisions into the default organization: its plan holds there too.
func TestHOST01_ProvisioningMeetsTheSeatLimit(t *testing.T) {
	e := testutil.New(t)
	e.Svc.Hosted = true
	var seats int
	e.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE org_id::text=$1 AND active AND kind='human'`, domain.DefaultOrg).Scan(&seats)
	if _, err := e.Svc.SetOrgPlan(ctx, e.Admin, domain.DefaultOrg, domain.OrgPlan{MaxSeats: pv(seats + 1)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.SCIMCreate(ctx, domain.SCIMIn{UserName: "p1@example.com", DisplayName: "P1"}); err != nil {
		t.Fatalf("the last seat: %v", err)
	}
	if _, _, err := e.Svc.SCIMCreate(ctx, domain.SCIMIn{UserName: "p2@example.com", DisplayName: "P2"}); planLimit(err) == nil {
		t.Fatalf("provisioning past the plan: %v", err)
	}
}

// HOST-03: managed upgrades: the service terms, the window, the notice a support level promises, and what the tenant's people are told.
func TestHOST03_ManagedMaintenance(t *testing.T) {
	e, org, boss := hostedEnv(t)
	// 2026-10-19 is a Monday; the clock of this test is fixed to a known moment
	e.Clock.T = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	in := func(day, hour, hours int) domain.MaintenanceIn {
		s := time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC)
		return domain.MaintenanceIn{Kind: "upgrade", Starts: s, Ends: s.Add(time.Duration(hours) * time.Hour), Message: "Moving to the new release."}
	}
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, in(25, 2, 2)); !isInvalid(err) {
		t.Fatalf("maintenance before a plan: %v", err)
	}
	e.Svc.SetOrgPlan(ctx, e.Admin, org, domain.OrgPlan{Plan: "team"})
	if _, err := e.Svc.SetServiceTerms(ctx, boss, org, domain.ServiceTerms{SupportLevel: "premium"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant set its own terms: %v", err)
	}
	for _, bad := range []domain.ServiceTerms{{SupportLevel: "gold"}, {UpgradeChannel: "nightly"}, {Window: &domain.MaintenanceWindow{Weekday: 7, StartHour: 2, Hours: 2}}, {Window: &domain.MaintenanceWindow{Weekday: 0, StartHour: 24, Hours: 2}}, {Window: &domain.MaintenanceWindow{Weekday: 0, StartHour: 2, Hours: 13}}} {
		if _, err := e.Svc.SetServiceTerms(ctx, e.Admin, org, bad); !isInvalid(err) {
			t.Fatalf("bad terms %+v: %v", bad, err)
		}
	}
	// Sundays from 02:00 UTC for two hours, standard support (a day's notice)
	if _, err := e.Svc.SetServiceTerms(ctx, e.Admin, org, domain.ServiceTerms{SupportLevel: "standard", UpgradeChannel: "early", Window: &domain.MaintenanceWindow{Weekday: 0, StartHour: 2, Hours: 2}}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.Svc.GetServiceTerms(ctx, boss, org); err != nil || got.SupportLevel != "standard" || got.UpgradeChannel != "early" || got.Window == nil || got.Window.Weekday != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := e.Svc.GetServiceTerms(ctx, boss, domain.DefaultOrg); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another organization's terms: %v", err)
	}
	// the refusals
	for name, c := range map[string]domain.MaintenanceIn{
		"outside the window (a Saturday)":             in(24, 2, 2),
		"outside the window (too late on the Sunday)": in(25, 3, 2),
		"too long":              {Kind: "upgrade", Starts: time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC), Ends: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)},
		"ends before it starts": {Kind: "upgrade", Starts: time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC), Ends: time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)},
		"unknown kind":          {Kind: "party", Starts: time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC), Ends: time.Date(2026, 10, 25, 4, 0, 0, 0, time.UTC)},
	} {
		if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, c); !isInvalid(err) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	// notice: a standard tenant gets a day; work tomorrow morning is refused, a Sunday this week is fine
	e.Clock.T = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) // Saturday noon: the Sunday 02:00 window is 14 hours away
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, in(25, 2, 2)); !isInvalid(err) || !strings.Contains(err.Error(), "24 hours") {
		t.Fatalf("less than the promised notice: %v", err)
	}
	e.Clock.T = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	n, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, in(25, 2, 2))
	if err != nil || n.Kind != "upgrade" || n.Cancelled != nil {
		t.Fatalf("%+v %v", n, err)
	}
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, in(25, 3, 1)); err == nil || !strings.Contains(err.Error(), "already announced") {
		t.Fatalf("overlapping work: %v", err)
	}
	if _, err := e.Svc.ScheduleMaintenance(ctx, boss, org, in(1, 2, 2)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a tenant scheduled work: %v", err)
	}
	// who is told
	if up := e.Svc.UpcomingMaintenance(ctx, boss); len(up) != 1 || up[0].ID != n.ID {
		t.Fatalf("the tenant's people are told: %+v", up)
	}
	if up := e.Svc.UpcomingMaintenance(ctx, e.Alice); len(up) != 0 {
		t.Fatalf("another organization is told: %+v", up)
	}
	if l, err := e.Svc.ListMaintenance(ctx, boss, org); err != nil || len(l) != 1 {
		t.Fatalf("%+v %v", l, err)
	}
	if _, err := e.Svc.ListMaintenance(ctx, boss, domain.DefaultOrg); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another organization's notices: %v", err)
	}
	// work more than two weeks away is not announced yet; over, it is no longer shown; cancelled, it is withdrawn with a reason
	e.Clock.T = e.Clock.T.AddDate(0, 0, 8)
	if up := e.Svc.UpcomingMaintenance(ctx, boss); len(up) != 0 {
		t.Fatalf("work that is over is still announced: %+v", up)
	}
	e.Clock.T = time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	if _, err := e.Svc.CancelMaintenance(ctx, e.Admin, org, n.ID, ""); !isInvalid(err) {
		t.Fatalf("cancel without a reason: %v", err)
	}
	if _, err := e.Svc.CancelMaintenance(ctx, e.Admin, domain.DefaultOrg, n.ID, "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cancelled under another organization: %v", err)
	}
	c, err := e.Svc.CancelMaintenance(ctx, e.Admin, org, n.ID, "postponed")
	if err != nil || c.Cancelled == nil {
		t.Fatalf("%+v %v", c, err)
	}
	if up := e.Svc.UpcomingMaintenance(ctx, boss); len(up) != 0 {
		t.Fatalf("cancelled work is announced: %+v", up)
	}
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, in(25, 3, 1)); err != nil {
		t.Fatalf("the slot of cancelled work was not free again: %v", err)
	}
	// community has no promised notice and an organization with no window may be worked on whenever
	e.Svc.SetServiceTerms(ctx, e.Admin, org, domain.ServiceTerms{SupportLevel: "community"})
	soon := domain.MaintenanceIn{Kind: "maintenance", Starts: e.Svc.Now().Add(time.Hour), Ends: e.Svc.Now().Add(2 * time.Hour)}
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, soon); err != nil {
		t.Fatalf("community, no window: %v", err)
	}
	long := domain.MaintenanceIn{Kind: "maintenance", Starts: e.Svc.Now().AddDate(0, 0, 3), Ends: e.Svc.Now().AddDate(0, 0, 3).Add(9 * time.Hour)}
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, org, long); !isInvalid(err) || !strings.Contains(err.Error(), "at most 8 hours") {
		t.Fatalf("nine hours of planned work with no window: %v", err)
	}
}

func TestHOST03_SelfHostedHasNoManagedUpgrades(t *testing.T) {
	e := testutil.New(t)
	if _, err := e.Svc.ScheduleMaintenance(ctx, e.Admin, domain.DefaultOrg, domain.MaintenanceIn{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if up := e.Svc.UpcomingMaintenance(ctx, e.Admin); len(up) != 0 {
		t.Fatalf("%+v", up)
	}
}
