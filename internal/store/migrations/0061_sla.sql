-- phase: expand
-- Service level clocks (SPEC 15): a request type carries a policy (goals for the first response and the resolution, the working calendar
-- they count in, the statuses that stop the clock); each request gets a clock per goal, which keeps a snapshot of the policy it started
-- under. A clock's state is derived from facts (the status history, the first public reply, the resolution) so it can always be
-- recomputed; the row is the cache that lists, search and notices read.
ALTER TABLE request_type ADD COLUMN sla jsonb NOT NULL DEFAULT '{}';
CREATE TABLE sla_clock (
  issue_id       uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  kind           text NOT NULL CHECK (kind IN ('first_response','resolution')),
  goal_minutes   int  NOT NULL CHECK (goal_minutes > 0),
  calendar       jsonb,                                  -- null: every minute counts
  pause_statuses text[] NOT NULL DEFAULT '{}',
  started_at     timestamptz NOT NULL,
  state          text NOT NULL DEFAULT 'running' CHECK (state IN ('running','paused','met','breached')),
  elapsed_min    double precision NOT NULL DEFAULT 0,
  due_at         timestamptz,
  met_at         timestamptz,
  breached_at    timestamptz,
  notified_at    timestamptz,
  updated_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issue_id, kind)
);
CREATE INDEX sla_clock_open_idx ON sla_clock (state) WHERE state IN ('running','paused','breached');
