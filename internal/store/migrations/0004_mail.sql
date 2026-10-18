-- phase: expand
-- Per-consumer outbox cursors: each consumer (notifier, later webhooks and automation) reads the same
-- event stream independently. The search indexer keeps using processed_at.
CREATE TABLE outbox_cursor (
  consumer text PRIMARY KEY,
  last_id  bigint NOT NULL DEFAULT 0
);

-- The delivery log and the send queue in one table: one row per event and recipient.
CREATE TABLE mail_message (
  id              bigserial PRIMARY KEY,
  idempotency_key text NOT NULL UNIQUE,         -- 'ev:<outbox id>:<recipient id>': retries never duplicate
  issue_id        uuid REFERENCES issue(id) ON DELETE SET NULL,
  recipient_id    uuid REFERENCES app_user(id) ON DELETE SET NULL,
  recipient       text NOT NULL,
  internal        boolean NOT NULL DEFAULT false, -- derived from an internal comment: needs write access at send time
  subject         text NOT NULL,
  message_id      text NOT NULL,
  in_reply_to     text NOT NULL DEFAULT '',
  refs            text NOT NULL DEFAULT '',
  body_text       text NOT NULL,
  body_html       text NOT NULL,
  status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed','suppressed')),
  attempts        int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  last_error      text NOT NULL DEFAULT '',
  created_at      timestamptz NOT NULL DEFAULT now(),
  sent_at         timestamptz
);
CREATE INDEX mail_due_idx ON mail_message (next_attempt_at) WHERE status = 'pending';
CREATE INDEX mail_issue_idx ON mail_message (issue_id, id);
