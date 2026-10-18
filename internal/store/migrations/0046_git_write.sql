-- phase: expand
-- Git write actions, connection health and disconnect (SPEC 18.4, GIT-14, GIT-18).
ALTER TABLE git_connection ADD COLUMN write_enabled boolean NOT NULL DEFAULT false;      -- create branches, comment on pull requests: off until an administrator says so
ALTER TABLE git_connection ADD COLUMN disabled boolean NOT NULL DEFAULT false;           -- disconnected but kept read-only: no ingest, no sync, no writes; links stay
ALTER TABLE git_connection ADD COLUMN health text NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown','ok','token_rejected','no_write_permission','error'));
ALTER TABLE git_connection ADD COLUMN health_detail text NOT NULL DEFAULT '';
ALTER TABLE git_connection ADD COLUMN health_at timestamptz;

-- what Tasklane wrote to a provider, and whether it worked
CREATE TABLE git_write_log (
  id            bigserial PRIMARY KEY,
  connection_id uuid NOT NULL REFERENCES git_connection(id) ON DELETE CASCADE,
  kind          text NOT NULL CHECK (kind IN ('branch','backlink')),
  issue_key     text NOT NULL,
  ref           text NOT NULL DEFAULT '',        -- the branch name, or the pull request number
  outcome       text NOT NULL CHECK (outcome IN ('done','failed')),
  detail        text NOT NULL DEFAULT '',
  user_id       uuid REFERENCES app_user(id) ON DELETE SET NULL,
  at            timestamptz NOT NULL DEFAULT now()
);

-- one back-link comment per pull request and issue
CREATE TABLE git_backlink (
  object_id uuid NOT NULL REFERENCES git_object(id) ON DELETE CASCADE,
  issue_id  uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (object_id, issue_id)
);
