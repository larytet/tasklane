-- phase: expand
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public;

CREATE TABLE app_user (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email text NOT NULL UNIQUE,
  name text NOT NULL,
  kind text NOT NULL DEFAULT 'human',       -- human | bot | agent | automation
  password_hash text,
  is_admin boolean NOT NULL DEFAULT false,
  active boolean NOT NULL DEFAULT true,
  appear_offline boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE session (
  token_hash bytea PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_token (
  token_hash bytea PRIMARY KEY,
  user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workflow (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE
);
CREATE TABLE status (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id uuid NOT NULL REFERENCES workflow(id) ON DELETE CASCADE,
  name text NOT NULL,
  category text NOT NULL CHECK (category IN ('todo','in_progress','done')),
  position int NOT NULL DEFAULT 0,
  UNIQUE (workflow_id, name)
);
CREATE TABLE transition (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id uuid NOT NULL REFERENCES workflow(id) ON DELETE CASCADE,
  name text NOT NULL,
  from_status uuid REFERENCES status(id) ON DELETE CASCADE,   -- NULL = from any status
  to_status uuid NOT NULL REFERENCES status(id) ON DELETE CASCADE
);

CREATE TABLE issue_type (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL UNIQUE,
  level int NOT NULL,                  -- 1 epic, 0 story/task/bug, -1 subtask
  icon text NOT NULL DEFAULT ''
);

CREATE TABLE project (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  key text NOT NULL UNIQUE CHECK (key ~ '^[A-Z][A-Z0-9]{1,9}$'),
  name text NOT NULL,
  type text NOT NULL DEFAULT 'software',
  lead_id uuid REFERENCES app_user(id),
  workflow_id uuid NOT NULL REFERENCES workflow(id),
  next_number bigint NOT NULL DEFAULT 1,
  archived boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE project_member (
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  role text NOT NULL DEFAULT 'member' CHECK (role IN ('viewer','member','admin')),
  PRIMARY KEY (project_id, user_id)
);

CREATE TABLE issue (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES project(id),
  number bigint NOT NULL,
  key text NOT NULL UNIQUE,
  issue_type_id uuid NOT NULL REFERENCES issue_type(id),
  status_id uuid NOT NULL REFERENCES status(id),
  priority text NOT NULL DEFAULT 'medium',
  reporter_id uuid NOT NULL REFERENCES app_user(id),
  assignee_id uuid REFERENCES app_user(id),
  labels text[] NOT NULL DEFAULT '{}',
  parent_id uuid REFERENCES issue(id),
  rank text NOT NULL DEFAULT '',
  version bigint NOT NULL DEFAULT 1,
  field_ver jsonb NOT NULL DEFAULT '{}',
  summary text NOT NULL,
  summary_seq bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  resolved_at timestamptz,
  deleted_at timestamptz,
  UNIQUE (project_id, number)
);
CREATE INDEX issue_list_idx ON issue (project_id, updated_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX issue_assignee_idx ON issue (assignee_id) WHERE deleted_at IS NULL;
CREATE INDEX issue_status_idx ON issue (status_id) WHERE deleted_at IS NULL;
CREATE INDEX issue_labels_idx ON issue USING gin (labels);

-- long text fields (description); summary is on the issue row
CREATE TABLE issue_text (
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  field text NOT NULL,
  body text NOT NULL DEFAULT '',
  text_seq bigint NOT NULL DEFAULT 1,
  updated_by uuid REFERENCES app_user(id),
  updated_at timestamptz NOT NULL DEFAULT now(),
  last_save_id text,
  PRIMARY KEY (issue_id, field)
);

-- every saved version of a text field, so an overwritten version can always be restored
CREATE TABLE text_version (
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  field text NOT NULL,
  seq bigint NOT NULL,
  body text NOT NULL,
  actor_id uuid REFERENCES app_user(id),
  actor_label text NOT NULL DEFAULT '',
  mode text NOT NULL DEFAULT 'save',      -- save | keep_mine | merge | patch | replace | restore
  at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issue_id, field, seq)
);

CREATE TABLE draft (
  user_id uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  field text NOT NULL,
  body text NOT NULL,
  base_seq bigint NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, issue_id, field)
);

CREATE TABLE comment (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  author_id uuid NOT NULL REFERENCES app_user(id),
  body text NOT NULL,
  internal boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(),
  edited_at timestamptz
);
CREATE INDEX comment_issue_idx ON comment (issue_id, created_at);

CREATE TABLE issue_history (
  id bigserial PRIMARY KEY,
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  field text NOT NULL,
  before text,
  after text,
  actor_id uuid REFERENCES app_user(id),
  actor_label text NOT NULL DEFAULT '',
  at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX issue_history_idx ON issue_history (issue_id, id);

CREATE TABLE outbox (
  id bigserial PRIMARY KEY,
  aggregate text NOT NULL,
  type text NOT NULL,
  payload jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz
);
CREATE INDEX outbox_pending_idx ON outbox (id) WHERE processed_at IS NULL;

CREATE TABLE audit_event (
  id bigserial PRIMARY KEY,
  actor_id uuid,
  actor_label text NOT NULL DEFAULT '',
  action text NOT NULL,
  target text NOT NULL DEFAULT '',
  detail jsonb NOT NULL DEFAULT '{}',
  at timestamptz NOT NULL DEFAULT now()
);

-- idempotency receipts: a retried save (same save id) is acknowledged once and never applied twice
CREATE TABLE save_receipt (
  save_id text PRIMARY KEY,
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  field text NOT NULL,
  seq bigint NOT NULL,
  at timestamptz NOT NULL DEFAULT now()
);
