-- phase: expand
-- Automation (SPEC 15): rules of trigger, conditions and actions, with a run log. A run is recorded once per rule,
-- issue and triggering event, so an event processed twice does nothing twice.
CREATE TABLE automation_rule (
  id          bigserial PRIMARY KEY,
  name        text NOT NULL,
  project_id  uuid REFERENCES project(id) ON DELETE CASCADE,   -- null: every project (instance administrators only)
  enabled     boolean NOT NULL DEFAULT true,
  trigger     jsonb NOT NULL,
  conditions  jsonb NOT NULL DEFAULT '[]',
  actions     jsonb NOT NULL,
  last_tick   bigint NOT NULL DEFAULT 0,                       -- scheduled rules: the last tick that ran
  created_by  uuid REFERENCES app_user(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX automation_rule_project_idx ON automation_rule (project_id) WHERE enabled;
CREATE TABLE automation_run (
  id         bigserial PRIMARY KEY,
  rule_id    bigint NOT NULL REFERENCES automation_rule(id) ON DELETE CASCADE,
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  event_id   bigint NOT NULL,                                   -- the outbox event, or minus the tick for a scheduled run
  state      text NOT NULL CHECK (state IN ('running','done','failed','skipped','loop','throttled')),
  detail     jsonb NOT NULL DEFAULT '{}',
  started_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (rule_id, issue_id, event_id)
);
CREATE INDEX automation_run_rule_idx ON automation_run (rule_id, id DESC);
