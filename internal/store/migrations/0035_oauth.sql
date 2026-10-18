-- phase: expand
-- OAuth 2.1 for MCP clients (MCP-04): dynamic client registration, authorization code with PKCE, rotating refresh tokens.
-- A grant is one person's consent to one client; the tokens it issues are the person's scoped tokens, so what a client
-- can do is the intersection of the scopes granted and what the person may do, and revoking the grant ends them all.
CREATE TABLE oauth_client (
  id            text PRIMARY KEY,
  name          text NOT NULL,
  redirect_uris text[] NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE oauth_code (
  code_hash    bytea PRIMARY KEY,
  client_id    text NOT NULL REFERENCES oauth_client(id) ON DELETE CASCADE,
  user_id      uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  redirect_uri text NOT NULL,
  scopes       text[] NOT NULL,
  challenge    text NOT NULL,
  expires_at   timestamptz NOT NULL
);
CREATE TABLE oauth_grant (
  id                 bigserial PRIMARY KEY,
  client_id          text NOT NULL REFERENCES oauth_client(id) ON DELETE CASCADE,
  user_id            uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  scopes             text[] NOT NULL,
  refresh_hash       bytea,
  old_refresh_hashes bytea[] NOT NULL DEFAULT '{}', -- the recent ones already used: presenting any of them again means a copy was stolen
  refresh_expires_at timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  last_used_at       timestamptz,
  revoked_at         timestamptz
);
CREATE INDEX oauth_grant_user_idx ON oauth_grant (user_id);
ALTER TABLE api_token ADD COLUMN oauth_grant_id bigint REFERENCES oauth_grant(id) ON DELETE CASCADE;
