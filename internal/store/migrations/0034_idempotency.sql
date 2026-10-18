-- phase: expand
-- Idempotency keys for agent writes (MCP-12): a retried call with the same key and arguments returns the first result
-- and does not do it again. Keys are kept for a day.
CREATE TABLE idempotency_key (
  user_id      uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  key          text NOT NULL,
  request_hash text NOT NULL,
  state        text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done')),
  result       jsonb,
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, key)
);
CREATE INDEX idempotency_key_age_idx ON idempotency_key (created_at);
