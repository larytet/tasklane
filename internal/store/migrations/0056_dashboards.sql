-- phase: expand
-- Saved filters and dashboards (SPEC 11). A filter is a named TQL query; a dashboard is a layout of gadgets, each over a query or a
-- filter, computed for whoever looks at it (never for the owner). Both are the owner's, optionally shared with the organization.
CREATE TABLE saved_filter (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id     uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id),
  owner_id   uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  name       text NOT NULL,
  tql        text NOT NULL,
  shared     text NOT NULL DEFAULT 'private' CHECK (shared IN ('private','org')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX saved_filter_owner_name_uq ON saved_filter (owner_id, lower(name));

CREATE TABLE dashboard (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id          uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id),
  owner_id        uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  name            text NOT NULL,
  columns         int NOT NULL DEFAULT 2 CHECK (columns BETWEEN 1 AND 3),
  shared          text NOT NULL DEFAULT 'private' CHECK (shared IN ('private','org')),
  refresh_seconds int NOT NULL DEFAULT 0 CHECK (refresh_seconds = 0 OR refresh_seconds BETWEEN 30 AND 3600),
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dashboard_owner_idx ON dashboard (owner_id);
CREATE TABLE dashboard_gadget (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  dashboard_id uuid NOT NULL REFERENCES dashboard(id) ON DELETE CASCADE,
  col          int NOT NULL DEFAULT 0,
  position     int NOT NULL DEFAULT 0,
  kind         text NOT NULL,
  title        text NOT NULL DEFAULT '',
  config       jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX dashboard_gadget_dash_idx ON dashboard_gadget (dashboard_id, col, position);
-- a person's own marks on dashboards: favourites and the one they open by default
CREATE TABLE dashboard_favourite (
  user_id      uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  dashboard_id uuid NOT NULL REFERENCES dashboard(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, dashboard_id)
);
CREATE TABLE user_default_dashboard (
  user_id      uuid PRIMARY KEY REFERENCES app_user(id) ON DELETE CASCADE,
  dashboard_id uuid NOT NULL REFERENCES dashboard(id) ON DELETE CASCADE
);
