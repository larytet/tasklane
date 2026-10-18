# Accounts and sign-in

- **Sign-in.** Email and password. Five failed attempts for an address lock it for fifteen minutes (a `429` with `Retry-After`); the lock applies to addresses that have no account in exactly the same way, so it reveals nothing. A success clears the count. Passwords are at least eight characters (`password_min_length` raises it) and cannot be the email address.
- **Two-factor authentication** (TOTP, RFC 6238; any authenticator app). Account → Set up: add the key to the app, confirm with a code, and keep the ten **recovery codes** (shown once; each works once). Signing in then takes the password and a code. A code is good once: the time step it belongs to is claimed atomically, one step of clock drift is tolerated, five wrong codes end the pending sign-in, and the pending sign-in expires after five minutes. Turning it off needs the password; an administrator can reset it for someone who lost their device and recovery codes (that also ends their sessions). Other sessions end when it is switched on. The secret is stored encrypted.
- **Sessions.** Account → Sessions lists each device (address, browser, last active); each can be ended, or all but this one. Sessions and tokens are checked against the database on every request, so ending one, changing a password, resetting two-factor authentication or deactivating the account takes effect on the next request.
- **API tokens.** Account → API tokens: a token has a name and an optional lifetime (up to five years), is shown once, is stored hashed, shows when it was last used, and stops working when it expires or is revoked. Scoped agent tokens (MCP) are not accepted by the REST API, so a token limited to reading cannot be used there with the person's full rights.
- **Offboarding.** An administrator ends every session of a person, and with `tokens` also their API tokens (`POST /api/v1/users/{id}/revoke-access`); deactivating the account stops everything as well.
- **Audit.** Two-factor changes, password changes, token creation and revocation, recovery-code use and access revocation are recorded in the audit log.

## Single sign-on (OpenID Connect)

**Admin → Identity → Add a provider.** Register `<your address>/api/v1/auth/sso/callback` as the redirect address at the identity provider and enter its issuer, the client id and (if it has one) the client secret. The sign-in page then offers a button per enabled provider.

- The flow is the authorization code flow with PKCE (S256), a random `state` (single use, ten minutes) and a `nonce`. The server never sees a password.
- The id token must be signed RS256 by a key in the provider's published key set (a rotated key is picked up), carry the right issuer and audience, not be expired, and carry this sign-in's nonce. `none`, symmetric algorithms and other keys are refused. The provider's own configuration document must name the issuer it was fetched for.
- A person is matched by the provider's subject, then by a **verified** email (an address the provider does not vouch for is not an identity); an account that already exists with that address is linked to the provider. Provider settings can limit sign-in to some email domains, and switch off creating accounts at first sign-in (`jit`). A deactivated account stays out whatever the provider says.
- Every call to the provider goes through the egress policy (private and loopback addresses are refused unless `sso_allow_private` is on). The client secret is stored encrypted and never shown. A refused sign-in sends the browser back to the sign-in page without saying why.
- Not built: SAML, group-to-role mapping, enforcing single sign-on instead of passwords.

## SCIM 2.0 provisioning

**Admin → Identity → Create a SCIM token**, then point the identity provider at `<your address>/scim/v2` with that token (a different kind of token from the API tokens: an ordinary session or token is refused there).

- `Users`: `GET` (with `filter=userName eq "..."` or `externalId eq "..."`, `startIndex`, `count`), `POST`, `GET`, `PUT`, `PATCH` (`replace` and `add` of `active`, `userName`, `displayName`, `name.formatted`, `externalId`, with or without a path) and `DELETE`.
- `POST` for an address that already has an account takes the account over (an inactive one is reactivated; one linked to a different external id is a conflict). Accounts made this way have no password and are never administrators.
- Setting `active` to false, or `DELETE`, **deactivates** the account: nothing is removed, and its sessions and API tokens end at once. The last active administrator cannot be deactivated from outside.
- Provisioning is recorded in the audit log by "SCIM".
- Not built: `Groups`, bulk operations, sorting.
