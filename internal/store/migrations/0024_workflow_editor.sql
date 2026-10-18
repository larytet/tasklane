-- phase: expand
-- Workflow editor (SPEC 9): drafts, versions, publishing with in-flight migration.
ALTER TABLE workflow ADD COLUMN version int NOT NULL DEFAULT 1;
ALTER TABLE workflow ADD COLUMN published_at timestamptz;
-- a retired status is being migrated away from: no transition leads to it, issues in it still work until the job moves them
ALTER TABLE status ADD COLUMN retired boolean NOT NULL DEFAULT false;
ALTER TABLE status ADD COLUMN color text NOT NULL DEFAULT '';
ALTER TABLE status ADD COLUMN pos_x int;
ALTER TABLE status ADD COLUMN pos_y int;
-- a system transition lets issues in a retired status behave like the status they are being moved to; the editor never shows it
ALTER TABLE transition ADD COLUMN system boolean NOT NULL DEFAULT false;
CREATE TABLE workflow_draft (
  workflow_id  uuid PRIMARY KEY REFERENCES workflow(id) ON DELETE CASCADE,
  base_version int NOT NULL,
  definition   jsonb NOT NULL,
  updated_by   uuid REFERENCES app_user(id),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE workflow_version (
  id           bigserial PRIMARY KEY,
  workflow_id  uuid NOT NULL REFERENCES workflow(id) ON DELETE CASCADE,
  version      int NOT NULL,
  definition   jsonb NOT NULL,
  note         text NOT NULL DEFAULT '',
  published_by uuid REFERENCES app_user(id),
  published_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workflow_id, version)
);
CREATE TABLE workflow_migration (
  id          bigserial PRIMARY KEY,
  workflow_id uuid NOT NULL REFERENCES workflow(id) ON DELETE CASCADE,
  version     int NOT NULL,
  status_from uuid NOT NULL,                -- the retired status
  status_name text NOT NULL,
  status_to   uuid NOT NULL REFERENCES status(id) ON DELETE CASCADE,
  total       int NOT NULL,
  done        int NOT NULL DEFAULT 0,
  state       text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done','failed')),
  error       text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX workflow_migration_open_idx ON workflow_migration (id) WHERE state = 'pending';
