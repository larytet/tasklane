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

## Passkeys and security keys (WebAuthn)

A passkey or a hardware security key can finish a sign-in after the password, beside an authenticator app. Nothing replaces the password: a key is a second factor.

- **Adding one:** Account and security → *Passkeys and security keys* → add. Naming it and entering the password again are required, so a stolen session cannot plant a key of its own. Removing one asks for the password too. Each add and remove is in the audit log.
- **Signing in:** after the password, a person with a key is offered **Use passkey or security key** (and the code field too, if they also have an authenticator app). A person with no key signs in as before.
- **What is checked:** the challenge (single use, bound to that sign-in), the origin and the relying party id (a key made for another site, or answered on another origin, does not work), that the person was present (and verified, if `webauthn_require_user_verification` is on), the signature (ES256, EdDSA and RS256), and the signature counter: a counter that does not move forward means the credential may have been cloned, the sign-in is refused and `security.passkey_clone_suspected` is written to the audit log. Keys that never count (counter 0) are accepted. Five wrong answers end the sign-in.
- **The relying party** is the host of `base_url` (override with `webauthn_rp_id` and `webauthn_origins`). **It must be a host name, never an IP address**: with an address the page says passkeys are unavailable. A key registered for one id does not work for another, so changing `base_url` to a different host orphans the keys (people sign in with their other factor and add the key again).
- **Not verified:** attestation. Tasklane asks for none and does not check who made the key; a key is trusted because the person proved their password when they added it.
- **Lost keys:** an administrator's *reset two-factor* removes the person's keys with the authenticator app and ends their sessions. Erasing a person removes their keys; the personal-data export lists them (name and dates, never key material).
- Single sign-on sign-ins do not ask for a key: the identity provider is the one that authenticates.
