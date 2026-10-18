-- phase: expand
-- SAML 2.0 single sign-on (SPEC 17.1) as a second kind of sign-in provider beside OpenID Connect. For a SAML provider `issuer` is the identity
-- provider's entity id and `client_id` is our entity id; the rest is here. An assertion is accepted once: its id is remembered until it expires.
ALTER TABLE sso_provider ADD COLUMN protocol text NOT NULL DEFAULT 'oidc' CHECK (protocol IN ('oidc','saml'));
ALTER TABLE sso_provider ADD COLUMN saml_sso_url text NOT NULL DEFAULT '';
ALTER TABLE sso_provider ADD COLUMN saml_certs text[] NOT NULL DEFAULT '{}';
ALTER TABLE sso_provider ADD COLUMN saml_email_attr text NOT NULL DEFAULT '';
ALTER TABLE sso_provider ADD COLUMN saml_name_attr text NOT NULL DEFAULT '';
CREATE TABLE saml_assertion (
  provider_id  bigint NOT NULL REFERENCES sso_provider(id) ON DELETE CASCADE,
  assertion_id text NOT NULL,
  expires_at   timestamptz NOT NULL,
  PRIMARY KEY (provider_id, assertion_id)
);
