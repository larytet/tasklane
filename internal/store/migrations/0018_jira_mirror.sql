-- phase: expand
-- Jira mirror (SPEC 18.1): one-way, read-only. A mirrored project is a native-shaped project that records where it came
-- from; the sync writes it with its own SQL while every service write path refuses it.
CREATE TABLE jira_connection (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL UNIQUE,
  site_url      text NOT NULL,                     -- https://example.atlassian.net
  email         text NOT NULL,                     -- the service account
  token_enc     bytea NOT NULL,                    -- AES-GCM, never returned
  token_fp      text NOT NULL DEFAULT '',
  webhook_secret_enc bytea NOT NULL,
  webhook_fp    text NOT NULL DEFAULT '',
  allow_private boolean NOT NULL DEFAULT false,
  created_by    uuid REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE project ADD COLUMN mirror_of uuid REFERENCES jira_connection(id);
CREATE TABLE jira_project (
  project_id     uuid PRIMARY KEY REFERENCES project(id),
  connection_id  uuid NOT NULL REFERENCES jira_connection(id) ON DELETE CASCADE,
  jira_key       text NOT NULL,
  jql            text NOT NULL DEFAULT '',          -- optional extra scope, ANDed with the project
  watermark      timestamptz,                       -- newest Jira `updated` seen; polling goes back a little from here
  synced_at      timestamptz,                       -- when a sync last completed
  perm_synced_at timestamptz,                       -- when roles and groups were last read: stale means fail closed
  backfilled     boolean NOT NULL DEFAULT false,
  issues_synced  int NOT NULL DEFAULT 0,
  last_error     text NOT NULL DEFAULT '',
  wake           boolean NOT NULL DEFAULT false,    -- a webhook hinted that something changed
  UNIQUE (connection_id, jira_key)
);
CREATE TABLE jira_warning (
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  kind       text NOT NULL,
  detail     text NOT NULL,
  at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, kind)
);
CREATE TABLE jira_adf (
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  field    text NOT NULL,                           -- description, or comment:<id>
  adf      jsonb NOT NULL,                          -- the original document: nothing is dropped
  PRIMARY KEY (issue_id, field)
);
ALTER TABLE app_user ADD COLUMN jira_account_id text;
ALTER TABLE issue ADD COLUMN origin_updated timestamptz;      -- Jira's `updated` for the stored version: older syncs lose
ALTER TABLE issue ADD COLUMN origin_key text;                    -- the issue's key in Jira, for the "Edit in Jira" link
ALTER TABLE issue ADD COLUMN origin_parent text;                 -- Jira's id of the parent, resolved after the batch (the parent may arrive later)
ALTER TABLE issue ADD COLUMN origin_links jsonb;                -- Jira's issue links, resolved after the batch (the other issue may arrive later)
ALTER TABLE issue ADD COLUMN restricted boolean NOT NULL DEFAULT false;   -- a Jira security level: hidden from everyone but administrators
ALTER TABLE issue_history ADD COLUMN external_id text;
ALTER TABLE issue_history ADD COLUMN source text NOT NULL DEFAULT '';

-- Membership as the permission engine must see it: a mirrored project whose roles and groups were not read recently is
-- visible to nobody (administrators aside). A failed or stale permission sync fails closed.
CREATE VIEW effective_member AS
  SELECT m.project_id, m.user_id, m.role FROM project_member m JOIN project p ON p.id = m.project_id
  WHERE p.mirror_of IS NULL
     OR EXISTS (SELECT 1 FROM jira_project jp WHERE jp.project_id = p.id AND jp.perm_synced_at > now() - interval '15 minutes');
