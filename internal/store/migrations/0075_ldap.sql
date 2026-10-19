-- phase: expand
-- widens: sso_provider_protocol_check
-- LDAP sign-in (Active Directory, OpenLDAP) as a third kind of sign-in provider. People type their directory name and password into
-- Tasklane's own sign-in form; Tasklane finds them with the search account and checks the password by binding as them. For an LDAP
-- provider `issuer` holds the server addresses (space-separated, tried in order) and `client_secret_enc` the search account's
-- password; the rest is here.
ALTER TABLE sso_provider DROP CONSTRAINT IF EXISTS sso_provider_protocol_check;
ALTER TABLE sso_provider ADD CONSTRAINT sso_provider_protocol_check CHECK (protocol IN ('oidc','saml','ldap'));
ALTER TABLE sso_provider ADD COLUMN ldap_bind_dn text NOT NULL DEFAULT '';
ALTER TABLE sso_provider ADD COLUMN ldap_base_dn text NOT NULL DEFAULT '';
ALTER TABLE sso_provider ADD COLUMN ldap_user_filter text NOT NULL DEFAULT '';
ALTER TABLE sso_provider ADD COLUMN ldap_start_tls boolean NOT NULL DEFAULT false;
ALTER TABLE sso_provider ADD COLUMN ldap_skip_verify boolean NOT NULL DEFAULT false;
ALTER TABLE sso_provider ADD COLUMN ldap_ca_pem text NOT NULL DEFAULT '';
ALTER TABLE sso_provider ADD COLUMN ldap_email_attr text NOT NULL DEFAULT '';
ALTER TABLE sso_provider ADD COLUMN ldap_name_attr text NOT NULL DEFAULT '';
