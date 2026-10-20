-- phase: expand
-- Undo instead of confirm (SPEC 4.6): a reversible action (transition, archive, delete to the trash) leaves a single-use token for the
-- person who did it. Undo reverts only if nobody changed the issue since: items record each issue's version and the database time
-- of the action. Tokens expire after a few minutes and are cleaned up hourly.
CREATE TABLE undo_token (
  id         text PRIMARY KEY,
  user_id    uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  op         text NOT NULL CHECK (op IN ('transition','archive','delete')),
  items      jsonb NOT NULL,
  acted_at   timestamptz NOT NULL,
  expires_at timestamptz NOT NULL,
  used_at    timestamptz
);
CREATE INDEX undo_token_expires ON undo_token (expires_at);
