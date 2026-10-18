-- phase: expand
-- Custom fields (SPEC 8): definitions, contexts (project set x issue type set) with options, defaults and required flags,
-- and typed values on issues.
CREATE TABLE custom_field (
  id            bigserial PRIMARY KEY,                    -- customfield_<id>
  name          text NOT NULL,
  description   text NOT NULL DEFAULT '',
  help_text     text NOT NULL DEFAULT '',
  type          text NOT NULL,
  unit          text NOT NULL DEFAULT '',
  searchable    boolean NOT NULL DEFAULT true,
  show_on_card  boolean NOT NULL DEFAULT false,
  unique_values boolean NOT NULL DEFAULT false,
  min_value     numeric,
  max_value     numeric,
  regex         text NOT NULL DEFAULT '',
  copy_on_clone boolean NOT NULL DEFAULT false,
  track_history boolean NOT NULL DEFAULT true,
  view_roles    text[] NOT NULL DEFAULT '{}',             -- empty: everyone who can see the issue
  edit_roles    text[] NOT NULL DEFAULT '{}',             -- empty: everyone who can edit the issue
  trashed_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE custom_field_context (
  id            bigserial PRIMARY KEY,
  field_id      bigint NOT NULL REFERENCES custom_field(id) ON DELETE CASCADE,
  name          text NOT NULL DEFAULT '',
  is_global     boolean NOT NULL DEFAULT false,           -- every project (and, with no type rows, every type)
  required      boolean NOT NULL DEFAULT false,
  default_value jsonb,
  position      int NOT NULL DEFAULT 0
);
CREATE INDEX custom_field_context_field ON custom_field_context (field_id);
CREATE TABLE custom_field_context_project (
  context_id bigint NOT NULL REFERENCES custom_field_context(id) ON DELETE CASCADE,
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  PRIMARY KEY (context_id, project_id)
);
CREATE TABLE custom_field_context_type (
  context_id    bigint NOT NULL REFERENCES custom_field_context(id) ON DELETE CASCADE,
  issue_type_id uuid NOT NULL REFERENCES issue_type(id) ON DELETE CASCADE,
  PRIMARY KEY (context_id, issue_type_id)
);
CREATE TABLE custom_field_option (
  id         bigserial PRIMARY KEY,
  context_id bigint NOT NULL REFERENCES custom_field_context(id) ON DELETE CASCADE,
  value      text NOT NULL,
  color      text NOT NULL DEFAULT '',
  position   int NOT NULL DEFAULT 0,
  archived   boolean NOT NULL DEFAULT false,              -- kept on issues that have it, not offered for new values
  UNIQUE (context_id, value)
);
CREATE TABLE issue_field_value (
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  field_id   bigint NOT NULL REFERENCES custom_field(id),
  value      jsonb NOT NULL,                              -- the canonical typed value
  text_value text,                                        -- lower-cased text for equality, substring and sorting
  num_value  numeric,                                     -- number, integer, float, duration (seconds), rating
  date_value timestamptz,                                 -- date and datetime
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issue_id, field_id)
);
CREATE INDEX issue_field_value_text ON issue_field_value (field_id, text_value);
CREATE INDEX issue_field_value_num ON issue_field_value (field_id, num_value) WHERE num_value IS NOT NULL;
CREATE INDEX issue_field_value_date ON issue_field_value (field_id, date_value) WHERE date_value IS NOT NULL;
CREATE UNIQUE INDEX custom_field_name_live ON custom_field (lower(name)) WHERE trashed_at IS NULL;
