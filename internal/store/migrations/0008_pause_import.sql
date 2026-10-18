-- phase: expand
-- The pause lease (DESIGN 11.8): one row per active pause, the single source of truth for every node. It expires at its
-- deadline, so a crashed job cannot leave the system paused.
CREATE TABLE pause_lease (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  scope      text NOT NULL,                 -- 'instance' or 'project:KEY'
  owner      text NOT NULL,
  mode       text NOT NULL CHECK (mode IN ('pause')),
  state      text NOT NULL CHECK (state IN ('draining','paused')),
  started_at timestamptz NOT NULL,
  deadline   timestamptz NOT NULL,
  marker     jsonb
);

-- one row per import run: what was created, so that a run can be rolled back by id
CREATE TABLE import_job (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  source     text NOT NULL,
  actor      text NOT NULL,
  state      text NOT NULL,
  summary    jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);

-- the identity of a record in the system it came from, so that re-importing the same bundle updates instead of duplicating
ALTER TABLE issue   ADD COLUMN external_id text;
ALTER TABLE issue   ADD COLUMN import_id uuid;
ALTER TABLE comment ADD COLUMN external_id text;
ALTER TABLE comment ADD COLUMN import_id uuid;
