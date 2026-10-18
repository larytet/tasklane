-- phase: expand
-- Single sign-on with OpenID Connect and SCIM provisioning (SPEC 17.1).
CREATE TABLE sso_provider (
  id                bigserial PRIMARY KEY,
  name              text NOT NULL,                       -- the short name in the sign-in address
  label             text NOT NULL,                       -- what the button says
  issuer            text NOT NULL,
  client_id         text NOT NULL,
  client_secret_enc bytea,
  scopes            text NOT NULL DEFAULT 'openid email profile',
  allowed_domains   text[] NOT NULL DEFAULT '{}',        -- empty: any verified address the provider vouches for
  jit               boolean NOT NULL DEFAULT true,       -- create an account at first sign-in
  enabled           boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX sso_provider_name_uq ON sso_provider (lower(name));
CREATE TABLE sso_state (
  state       text PRIMARY KEY,
  provider_id bigint NOT NULL REFERENCES sso_provider(id) ON DELETE CASCADE,
  nonce       text NOT NULL,
  verifier    text NOT NULL,
  redirect    text NOT NULL DEFAULT '/',
  expires_at  timestamptz NOT NULL
);
CREATE TABLE sso_identity (
  provider_id bigint NOT NULL REFERENCES sso_provider(id) ON DELETE CASCADE,
  subject     text NOT NULL,
  user_id     uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (provider_id, subject)
);
CREATE TABLE scim_token (
  id         bigserial PRIMARY KEY,
  name       text NOT NULL,
  token_hash bytea NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
);
ALTER TABLE app_user ADD COLUMN external_id text;                       -- the identity provider's id for the person (SCIM externalId)
ALTER TABLE app_user ADD COLUMN source text NOT NULL DEFAULT 'local';   -- local, sso or scim
