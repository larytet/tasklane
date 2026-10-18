-- phase: expand
-- Account security (SPEC 17.1, SEC-04): numbered sessions and tokens that people can list and revoke, two-factor
-- authentication (TOTP) with recovery codes, a record of sign-in attempts for lockout, and the time a password changed.
ALTER TABLE session ADD COLUMN id bigserial UNIQUE;
ALTER TABLE session ADD COLUMN user_agent text NOT NULL DEFAULT '';
ALTER TABLE session ADD COLUMN ip text NOT NULL DEFAULT '';
ALTER TABLE session ADD COLUMN last_seen_at timestamptz;
ALTER TABLE api_token ADD COLUMN last_used_at timestamptz;
ALTER TABLE app_user ADD COLUMN password_changed_at timestamptz;
CREATE TABLE user_mfa (
  user_id         uuid PRIMARY KEY REFERENCES app_user(id) ON DELETE CASCADE,
  secret_enc      bytea NOT NULL,                       -- the TOTP secret, encrypted like every stored credential
  enabled         boolean NOT NULL DEFAULT false,       -- false while the person is still enrolling
  last_step       bigint NOT NULL DEFAULT 0,            -- the last accepted time step: a code is good once
  recovery_hashes text[] NOT NULL DEFAULT '{}',         -- one-use recovery codes, hashed
  created_at      timestamptz NOT NULL DEFAULT now()
);
-- a password was right and a second factor is due: this token proves the first step for five minutes
CREATE TABLE mfa_pending (
  token_hash bytea PRIMARY KEY,
  user_id    uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  attempts   int NOT NULL DEFAULT 0,
  ttl_secs   int NOT NULL,
  user_agent text NOT NULL DEFAULT '',
  ip         text NOT NULL DEFAULT ''
);
CREATE TABLE login_attempt (
  id    bigserial PRIMARY KEY,
  email text NOT NULL,
  ip    text NOT NULL DEFAULT '',
  ok    boolean NOT NULL,
  at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX login_attempt_email_idx ON login_attempt (email, at DESC);
