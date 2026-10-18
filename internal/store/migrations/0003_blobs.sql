-- phase: expand
-- Metadata only: bytes live in the blob store (SPEC 17.6). No table holds file bytes.
CREATE TABLE blob (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  sha256      text NOT NULL UNIQUE CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  backend     text NOT NULL DEFAULT 'fs',
  size        bigint NOT NULL,
  mime        text NOT NULL,                 -- sniffed
  class       text NOT NULL,                 -- image | video | log | document | archive
  scan_state  text NOT NULL DEFAULT 'scanning' CHECK (scan_state IN ('scanning','clean','blocked')),
  scan_detail text NOT NULL DEFAULT '',
  refcount    bigint NOT NULL DEFAULT 0,
  zero_since  timestamptz,                   -- when refcount last reached 0 (GC retention window)
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE attachment (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  issue_id      uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  blob_id       uuid NOT NULL REFERENCES blob(id),
  name          text NOT NULL,
  declared_mime text NOT NULL DEFAULT '',
  type_flag     text NOT NULL DEFAULT '',    -- non-empty when the declared type disagrees with the sniffed one
  uploader_id   uuid REFERENCES app_user(id),
  thumb_blob_id uuid REFERENCES blob(id),
  thumb_state   text NOT NULL DEFAULT 'none' CHECK (thumb_state IN ('none','pending','ready','failed')),
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachment_issue_idx ON attachment (issue_id, created_at);
CREATE INDEX attachment_blob_idx ON attachment (blob_id);

CREATE TABLE upload_session (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  name       text NOT NULL,
  declared_mime text NOT NULL DEFAULT '',
  total_size bigint NOT NULL,
  received   bigint NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE setting (
  key   text PRIMARY KEY,
  value text NOT NULL
);
