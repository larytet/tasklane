-- phase: expand
-- Passkeys and security keys as a second factor (WebAuthn). The public key is kept; the private key never leaves the device.
CREATE TABLE webauthn_credential (
  id           bigserial PRIMARY KEY,
  user_id      uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  cred_id      bytea NOT NULL UNIQUE,
  public_key   bytea NOT NULL,                       -- PKIX DER
  alg          int NOT NULL,                         -- COSE algorithm: -7 ES256, -8 EdDSA, -257 RS256
  sign_count   bigint NOT NULL DEFAULT 0,
  name         text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz
);
CREATE INDEX webauthn_credential_user_idx ON webauthn_credential (user_id);
-- one registration in progress per person
CREATE TABLE webauthn_challenge (
  user_id    uuid PRIMARY KEY REFERENCES app_user(id) ON DELETE CASCADE,
  challenge  bytea NOT NULL,
  expires_at timestamptz NOT NULL
);
ALTER TABLE mfa_pending ADD COLUMN wa_challenge bytea;
