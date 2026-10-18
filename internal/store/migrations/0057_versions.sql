-- phase: expand
-- Versions (releases) per project (SPEC 12) and the links from issues: an issue is fixed in, or affects, any number of versions.
CREATE TABLE version (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id   uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  name         text NOT NULL,
  description  text NOT NULL DEFAULT '',
  start_date   date,
  release_date date,
  released     boolean NOT NULL DEFAULT false,
  released_at  timestamptz,
  archived     boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX version_project_name_uq ON version (project_id, lower(name));
CREATE TABLE issue_version (
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  version_id uuid NOT NULL REFERENCES version(id) ON DELETE CASCADE,
  kind       text NOT NULL CHECK (kind IN ('fix','affects')),
  PRIMARY KEY (issue_id, version_id, kind)
);
CREATE INDEX issue_version_version_idx ON issue_version (version_id, kind);
