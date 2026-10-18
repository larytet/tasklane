-- phase: expand
-- Workflow automation from code events, smart commits and provider identities (SPEC 18.4).

-- which provider account is which person: set by an administrator, who is the one who can vouch for it
CREATE TABLE git_identity (
  id         bigserial PRIMARY KEY,
  provider   text NOT NULL CHECK (provider IN ('github','gitlab')),
  login      text NOT NULL,                       -- lower case
  user_id    uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  created_by uuid REFERENCES app_user(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, login)
);
CREATE INDEX git_identity_user_idx ON git_identity (user_id);

ALTER TABLE git_repo ADD COLUMN default_branch text NOT NULL DEFAULT '';

-- smart commits are off unless a project turns them on
ALTER TABLE project ADD COLUMN git_smart_commits text NOT NULL DEFAULT 'off' CHECK (git_smart_commits IN ('off','default_branch','any'));

-- rules: an event in a repository mapped to the project moves or comments on the linked issues
CREATE TABLE git_rule (
  id         bigserial PRIMARY KEY,
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  name       text NOT NULL,
  trigger    text NOT NULL CHECK (trigger IN ('branch_created','pr_opened','pr_merged','build_failed')),
  transition text NOT NULL DEFAULT '',            -- a workflow transition by name; empty: none
  comment    text NOT NULL DEFAULT '',            -- a comment to post; empty: none
  mode       text NOT NULL DEFAULT 'dry_run' CHECK (mode IN ('off','dry_run','on')),
  created_by uuid REFERENCES app_user(id) ON DELETE SET NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- one run per rule, code object and issue: a repeated, replayed or re-opened event never runs a rule twice (loop protection)
CREATE TABLE git_rule_run (
  id        bigserial PRIMARY KEY,
  rule_id   bigint NOT NULL REFERENCES git_rule(id) ON DELETE CASCADE,
  object_id uuid NOT NULL REFERENCES git_object(id) ON DELETE CASCADE,
  issue_id  uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  trigger   text NOT NULL,
  outcome   text NOT NULL CHECK (outcome IN ('applied','skipped','dry_run','failed')),
  detail    text NOT NULL DEFAULT '',
  at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (rule_id, object_id, issue_id)
);

-- what each smart-commit command did, or why it did not
CREATE TABLE git_smart_run (
  id        bigserial PRIMARY KEY,
  object_id uuid NOT NULL REFERENCES git_object(id) ON DELETE CASCADE,
  issue_key text NOT NULL,
  idx       int NOT NULL,
  command   text NOT NULL,
  outcome   text NOT NULL CHECK (outcome IN ('applied','ignored','failed')),
  reason    text NOT NULL DEFAULT '',
  user_id   uuid REFERENCES app_user(id) ON DELETE SET NULL,
  at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (object_id, issue_key, idx)
);
