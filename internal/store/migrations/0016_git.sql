-- phase: expand
-- Git integration (SPEC 18.4), read-only: connections, repositories mapped to projects, a webhook inbox, normalized
-- objects (branches, commits, pull requests, builds, deployments) and their links to issues. No source code is stored.
CREATE TABLE git_connection (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL UNIQUE,
  provider      text NOT NULL CHECK (provider IN ('github','gitlab')),
  base_url      text NOT NULL,                 -- the API base: https://api.github.com or https://gitlab.example.com
  token_enc     bytea,                          -- AES-GCM, never returned
  token_fp      text NOT NULL DEFAULT '',       -- fingerprint shown to administrators
  webhook_secret_enc bytea NOT NULL,
  webhook_fp    text NOT NULL DEFAULT '',
  allow_private boolean NOT NULL DEFAULT false, -- egress policy: loopback and private ranges
  allowed_cidrs text[] NOT NULL DEFAULT '{}',   -- optional source-IP allowlist for webhooks
  backfill_days int NOT NULL DEFAULT 90 CHECK (backfill_days BETWEEN 0 AND 3650),
  created_by    uuid REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE git_repo (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  connection_id uuid NOT NULL REFERENCES git_connection(id) ON DELETE CASCADE,
  full_name     text NOT NULL,                  -- owner/name or group/project
  last_sync_at  timestamptz,
  sync_error    text NOT NULL DEFAULT '',
  etags         jsonb NOT NULL DEFAULT '{}',
  UNIQUE (connection_id, full_name)
);
-- many repositories per project, one repository to several projects
CREATE TABLE git_repo_project (
  repo_id    uuid NOT NULL REFERENCES git_repo(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  PRIMARY KEY (repo_id, project_id)
);
CREATE TABLE git_inbox (
  id           bigserial PRIMARY KEY,
  connection_id uuid NOT NULL REFERENCES git_connection(id) ON DELETE CASCADE,
  delivery_id  text NOT NULL,
  event        text NOT NULL,
  payload      bytea NOT NULL,
  received_at  timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz,
  attempts     int NOT NULL DEFAULT 0,
  error        text NOT NULL DEFAULT '',
  UNIQUE (connection_id, delivery_id)           -- a replay or duplicate delivery is ignored
);
CREATE INDEX git_inbox_pending ON git_inbox (id) WHERE processed_at IS NULL;
CREATE TABLE git_object (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  repo_id     uuid NOT NULL REFERENCES git_repo(id) ON DELETE CASCADE,
  kind        text NOT NULL CHECK (kind IN ('pr','branch','commit','build','deployment')),
  ext_id      text NOT NULL,
  title       text NOT NULL DEFAULT '',
  state       text NOT NULL DEFAULT '',
  review      text NOT NULL DEFAULT '',
  author      text NOT NULL DEFAULT '',
  author_user uuid REFERENCES app_user(id),
  url         text NOT NULL DEFAULT '',
  sha         text NOT NULL DEFAULT '',
  branch      text NOT NULL DEFAULT '',
  base        text NOT NULL DEFAULT '',
  env         text NOT NULL DEFAULT '',
  meta        jsonb NOT NULL DEFAULT '{}',
  provider_at timestamptz NOT NULL DEFAULT 'epoch',  -- the provider's timestamp of the stored version: out-of-order events lose
  updated_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (repo_id, kind, ext_id)
);
CREATE INDEX git_object_sha ON git_object (repo_id, sha) WHERE sha <> '';
CREATE TABLE git_link (
  issue_id  uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  object_id uuid NOT NULL REFERENCES git_object(id) ON DELETE CASCADE,
  manual    boolean NOT NULL DEFAULT false,      -- a person added it: automatic relinking never removes it
  auto      boolean NOT NULL DEFAULT false,      -- the key appears in the branch, message or title
  PRIMARY KEY (issue_id, object_id)
);
CREATE INDEX git_link_object ON git_link (object_id);
