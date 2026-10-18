-- phase: expand
-- browser-to-store uploads (SPEC 17.6): a row exists from the moment URLs are handed out until the upload is completed,
-- aborted or expires; nothing about the file is visible to anyone else in the meantime
CREATE TABLE direct_upload (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  issue_id     uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  name         text NOT NULL,
  declared_mime text NOT NULL DEFAULT '',
  size         bigint NOT NULL CHECK (size > 0),
  sha256       text NOT NULL DEFAULT '',          -- optional: the browser's own hash, checked at completion
  multipart_id text NOT NULL DEFAULT '',
  parts        int NOT NULL DEFAULT 0,
  expires_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX direct_upload_expiry ON direct_upload (expires_at);
