-- phase: expand
-- Bounces, complaints and the suppression list (SPEC 13.1). Suppression applies to email only, never to the in-app inbox.
CREATE TABLE mail_bounce (
  id              bigserial PRIMARY KEY,
  mail_message_id bigint,                         -- the message the report answers, when known
  email           text NOT NULL,
  kind            text NOT NULL CHECK (kind IN ('hard','soft','complaint')),
  status          text NOT NULL DEFAULT '',        -- the enhanced status code or the provider's word
  source          text NOT NULL DEFAULT 'dsn',     -- dsn, feedback, provider
  at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX mail_bounce_email_idx ON mail_bounce (email, at);

CREATE TABLE mail_suppression (
  email      text PRIMARY KEY,                    -- lowercase
  kind       text NOT NULL CHECK (kind IN ('hard','soft','complaint')),
  reason     text NOT NULL DEFAULT '',
  since      timestamptz NOT NULL DEFAULT now(),
  cleared_at timestamptz,
  cleared_by uuid REFERENCES app_user(id) ON DELETE SET NULL
);
