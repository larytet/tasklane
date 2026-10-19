-- phase: expand
-- Managed upgrades and support levels for the hosted edition (SPEC 17.9). A plan names a support level, an upgrade channel and the weekly window in which the
-- operator may upgrade the tenant; the operator schedules maintenance inside that window with notice that grows with the support level, and the tenant's people
-- see it coming. Inactive unless `hosted` is on.
ALTER TABLE org_plan ADD COLUMN support_level text NOT NULL DEFAULT 'community' CHECK (support_level IN ('community','standard','premium'));
ALTER TABLE org_plan ADD COLUMN upgrade_channel text NOT NULL DEFAULT 'stable' CHECK (upgrade_channel IN ('stable','early'));
ALTER TABLE org_plan ADD COLUMN window_weekday integer CHECK (window_weekday BETWEEN 0 AND 6);        -- 0 is Sunday, in UTC
ALTER TABLE org_plan ADD COLUMN window_start_hour integer CHECK (window_start_hour BETWEEN 0 AND 23);
ALTER TABLE org_plan ADD COLUMN window_hours integer NOT NULL DEFAULT 2 CHECK (window_hours BETWEEN 1 AND 12);

CREATE TABLE maintenance_notice (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id       uuid NOT NULL REFERENCES organization(id) ON DELETE CASCADE,
  kind         text NOT NULL CHECK (kind IN ('upgrade','maintenance')),
  starts_at    timestamptz NOT NULL,
  ends_at      timestamptz NOT NULL CHECK (ends_at > starts_at),
  message      text NOT NULL DEFAULT '',
  created_by   text NOT NULL DEFAULT '',
  created_at   timestamptz NOT NULL DEFAULT now(),
  cancelled_at timestamptz,
  cancel_reason text NOT NULL DEFAULT ''
);
CREATE INDEX maintenance_notice_org ON maintenance_notice (org_id, starts_at);
