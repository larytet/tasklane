-- phase: expand
-- Screens, screen schemes, field configurations and behavior rules (SPEC 8): which fields appear on create, edit and view
-- per issue type, per-scope overrides of required, hidden and help text, and conditional show, hide and require rules.
CREATE TABLE screen (
  id          bigserial PRIMARY KEY,
  name        text NOT NULL,
  description text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX screen_name_uq ON screen (lower(name));
CREATE TABLE screen_tab (
  id        bigserial PRIMARY KEY,
  screen_id bigint NOT NULL REFERENCES screen(id) ON DELETE CASCADE,
  name      text NOT NULL,
  position  int NOT NULL DEFAULT 0
);
CREATE TABLE screen_field (
  id        bigserial PRIMARY KEY,
  tab_id    bigint NOT NULL REFERENCES screen_tab(id) ON DELETE CASCADE,
  field_key text NOT NULL,                 -- a built-in key (description, priority, assignee, labels) or customfield_<id>
  position  int NOT NULL DEFAULT 0
);
CREATE TABLE screen_scheme (
  id   bigserial PRIMARY KEY,
  name text NOT NULL
);
CREATE UNIQUE INDEX screen_scheme_name_uq ON screen_scheme (lower(name));
CREATE TABLE screen_scheme_entry (
  id            bigserial PRIMARY KEY,
  scheme_id     bigint NOT NULL REFERENCES screen_scheme(id) ON DELETE CASCADE,
  operation     text NOT NULL CHECK (operation IN ('create','edit','view')),
  issue_type_id uuid REFERENCES issue_type(id) ON DELETE CASCADE,   -- null: the default for every type
  screen_id     bigint NOT NULL REFERENCES screen(id)
);
CREATE UNIQUE INDEX screen_scheme_entry_uq ON screen_scheme_entry (scheme_id, operation, COALESCE(issue_type_id, '00000000-0000-0000-0000-000000000000'));
ALTER TABLE project ADD COLUMN screen_scheme_id bigint REFERENCES screen_scheme(id);
CREATE TABLE field_config (
  id            bigserial PRIMARY KEY,
  project_id    uuid REFERENCES project(id) ON DELETE CASCADE,
  issue_type_id uuid REFERENCES issue_type(id) ON DELETE CASCADE,
  field_key     text NOT NULL,
  required      boolean,                   -- null: inherit
  hidden        boolean,
  help_text     text
);
CREATE UNIQUE INDEX field_config_uq ON field_config (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'), COALESCE(issue_type_id, '00000000-0000-0000-0000-000000000000'), field_key);
CREATE TABLE behavior_rule (
  id            bigserial PRIMARY KEY,
  name          text NOT NULL DEFAULT '',
  project_id    uuid REFERENCES project(id) ON DELETE CASCADE,
  issue_type_id uuid REFERENCES issue_type(id) ON DELETE CASCADE,
  position      int NOT NULL DEFAULT 0,
  enabled       boolean NOT NULL DEFAULT true,
  when_field    text NOT NULL DEFAULT '',  -- empty: the rule always applies
  when_op       text NOT NULL DEFAULT 'eq' CHECK (when_op IN ('eq','ne','in','empty','notempty')),
  when_value    jsonb,
  when_role     text NOT NULL DEFAULT '',  -- empty: any role; else viewer, member or admin
  action        text NOT NULL CHECK (action IN ('show','hide','require')),
  target_field  text NOT NULL
);
