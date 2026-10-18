-- phase: expand
-- The cluster version gate (SPEC 17.8): nodes register the version they run; a behaviour that older nodes cannot handle stays off
-- until every live node reports at least the version it needs.
CREATE TABLE cluster_node (
  id         text PRIMARY KEY,
  version    text NOT NULL,
  started_at timestamptz NOT NULL DEFAULT now(),
  seen_at    timestamptz NOT NULL DEFAULT now(),
  draining   boolean NOT NULL DEFAULT false
);
CREATE TABLE feature_gate (
  name         text PRIMARY KEY,
  min_version  text NOT NULL,
  risky        boolean NOT NULL DEFAULT false,
  state        text NOT NULL DEFAULT 'off' CHECK (state IN ('off','pending','on')),  -- pending: qualifies, waits for an administrator
  activated_at timestamptz,
  confirmed_by uuid REFERENCES app_user(id) ON DELETE SET NULL
);
