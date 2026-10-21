# Accounts and sign-in

- **Sign-in.** Email and password, or a directory name and password when an LDAP provider is set up (below). Five failed attempts for an address lock it for fifteen minutes (a `429` with `Retry-After`); the lock applies to addresses that have no account in exactly the same way, so it reveals nothing. A success clears the count. Passwords are at least eight characters (`password_min_length` raises it) and cannot be the email address.
- **Two-factor authentication** (TOTP, RFC 6238; any authenticator app). Settings → Security (the name in the top bar) → Set up: scan the QR code (drawn by Tasklane itself, so the secret is not sent to an image service) or type the key into the app, confirm with a code, and keep the ten **recovery codes** (shown once; each works once). Signing in then takes the password and a code. A code is good once: the time step it belongs to is claimed atomically, one step of clock drift is tolerated, five wrong codes end the pending sign-in, and the pending sign-in expires after five minutes. Turning it off needs the password; an administrator can reset it for someone who lost their device and recovery codes (that also ends their sessions). Other sessions end when it is switched on. The secret is stored encrypted.
- **Sessions.** Settings → Sessions lists each device (address, browser, last active); each can be ended, or all but this one. Sessions and tokens are checked against the database on every request, so ending one, changing a password, resetting two-factor authentication or deactivating the account takes effect on the next request.
- **API tokens.** Settings → API tokens: a token has a name and an optional lifetime (up to five years), is shown once, is stored hashed, shows when it was last used, and stops working when it expires or is revoked. Scoped agent tokens (MCP) are not accepted by the REST API, so a token limited to reading cannot be used there with the person's full rights.
- **Offboarding.** An administrator ends every session of a person, and with `tokens` also their API tokens (`POST /api/v1/users/{id}/revoke-access`); deactivating the account stops everything as well.
- **Audit.** Two-factor changes, password changes, token creation and revocation, recovery-code use and access revocation are recorded in the audit log.

## Single sign-on (OpenID Connect)

**Admin → Identity → Add a provider.** Register `<your address>/api/v1/auth/sso/callback` as the redirect address at the identity provider and enter its issuer, the client id and (if it has one) the client secret. The sign-in page then offers a button per enabled provider.

- The flow is the authorization code flow with PKCE (S256), a random `state` (single use, ten minutes) and a `nonce`. The server never sees a password.
- The id token must be signed RS256 by a key in the provider's published key set (a rotated key is picked up), carry the right issuer and audience, not be expired, and carry this sign-in's nonce. `none`, symmetric algorithms and other keys are refused. The provider's own configuration document must name the issuer it was fetched for.
- A person is matched by the provider's subject, then by a **verified** email (an address the provider does not vouch for is not an identity); an account that already exists with that address is linked to the provider. Provider settings can limit sign-in to some email domains, and switch off creating accounts at first sign-in (`jit`). A deactivated account stays out whatever the provider says.
- Every call to the provider goes through the egress policy (private and loopback addresses are refused unless `sso_allow_private` is on). The client secret is stored encrypted and never shown. A refused sign-in sends the browser back to the sign-in page without saying why.
- Not built: group-to-role mapping, enforcing single sign-on instead of passwords, single logout.

## Single sign-on (SAML 2.0)

**Admin → Identity → Add a provider → SAML 2.0.** Paste the identity provider's **metadata XML** (it fills in the entity id, the sign-on address and the signing certificate), or enter those three by hand. Then give the identity provider what the provider's *Edit* dialog shows: our **metadata** address (`<your address>/api/v1/auth/saml/<name>/metadata`, a document it can import), our **entity id** (that same address, unless you set another) and the **assertion consumer service** (`<your address>/api/v1/auth/saml/acs`, HTTP-POST binding). The sign-in page then offers a button for it like any other provider.

- **What is supported.** Service-provider-initiated Web SSO: the button sends an `AuthnRequest` (HTTP-Redirect binding) and the identity provider answers by POST to the assertion consumer service. Assertions signed with **RSA** (SHA-256, SHA-384 or SHA-512) over **exclusive canonicalization**, either the assertion or the whole response (or both). NameID of any format; the email is read from the attribute you name, then from the usual ones (`email`, `mail`, `emailaddress`, the Microsoft and LDAP names), then from the NameID when that is an address.
- **What is refused**, each with the reason recorded in the audit log (`security.saml_refused`) and nothing said to the person: no signature, a signature that does not verify or that covers something else, a certificate that is not the one configured or has expired, SHA-1, any other canonicalization or transform, a document with a DOCTYPE, more than one assertion or two elements with the same ID (signature wrapping), encrypted assertions (turn encryption off at the identity provider: the response is signed and the channel is TLS), a response that does not answer the request this sign-in sent (**unsolicited responses and identity-provider-initiated sign-in are not accepted**), a wrong destination, recipient, audience or issuer, an assertion outside its validity (two minutes of clock difference are tolerated), a missing conditions or audience element, and an assertion that was already used (each assertion id is remembered until it expires).
- **Accounts** work as for OpenID Connect: matched by the NameID, then by the address; created at first sign-in unless switched off; limited to some email domains if you say so; a deactivated account stays out; the provider vouches for its people's addresses.
- The signature checker is about 600 lines of standard-library Go (`internal/saml`), written against fixtures produced by a different implementation (lxml, whose canonicalization is libxml2's) in many shapes, plus tampered and signature-wrapping variants; see `internal/saml/testdata/gen.py`.
- Not built: single logout, signed `AuthnRequest`s, encrypted assertions, identity-provider-initiated sign-in, attribute-to-role mapping, metadata fetched from a URL and refreshed (paste it, and change the certificate here when the identity provider rotates it), customer (portal) SAML sign-in.

## LDAP directory (Active Directory, OpenLDAP)

**Admin → Identity → Add a provider → LDAP.** People then sign in on the ordinary form with their **directory name** (`jana.k`,
`CORP\jana.k`, `jana.k@corp.example`) or their address, and their **directory password**. There is no button: when no local
password matches, Tasklane asks each enabled LDAP provider in name order.

- **How a password is checked.** Tasklane connects, binds as the search account, searches under the base DN with the user filter
  (`{user}` is what the person typed, escaped so it can never change the filter), and requires **exactly one** entry. It then binds
  as that entry with the password typed. Nothing is cached: a person disabled or removed in the directory is refused at once. An
  empty password is refused before anything is sent (many servers would accept it as an anonymous bind).
- **Settings.**
  - Server addresses: `ldaps://dc1.example.com` (port 636) or `ldap://dc1.example.com` (port 389) with **StartTLS**; several,
    tried in order.
  - A password is never sent unencrypted: plain `ldap://` without StartTLS is refused, except to this machine.
  - The server certificate is verified against the system authorities, or the **CA certificate** you paste. "Do not verify" exists
    for directories whose CA you cannot give, as Active Directory often runs; prefer pasting the CA.
  - Search account DN and password: the password is stored encrypted and never shown. Without a search account the search is
    anonymous.
  - The default user filter is `(|(sAMAccountName={user})(userPrincipalName={user})(uid={user})(mail={user}))`.
  - The email attribute defaults to `mail`; the name attribute to `displayName`, then `cn`.
- **Who may sign in** is written into the user filter, as Active Directory expresses it. Only members of a group, including nested
  groups, and not disabled accounts:
  `(&(sAMAccountName={user})(memberOf:1.2.840.113556.1.4.1941:=CN=Tasklane,OU=Groups,DC=corp,DC=example)(!(userAccountControl:1.2.840.113556.1.4.803:=2)))`.
- **Accounts** work as for single sign-on. A person is matched by the directory's identifier (`objectGUID`, `entryUUID`, else the DN),
  then by the address. Accounts are created at first sign-in unless that is switched off, and sign-in can be limited to some email
  domains. A deactivated account stays out. An entry without an address is refused.
- **What still applies:**
  - Two-factor authentication: a directory password is a password.
  - Network rules and the organization's "require a second factor" policy.
  - The **lockout**: five failures lock the name typed for fifteen minutes and stop the guessing before it reaches the directory, so
    Tasklane does not help lock the person's directory account.
  - Local passwords keep working beside the directory.
  - Sessions record `via = ldap`.
- **Trying it.** The provider's *Edit* dialog has **Try a sign-in** (`POST /api/v1/admin/sso/providers/{id}/check`). It asks the
  directory with a name and password and says what it found: the DN, the address, and whether the account exists or would be
  created. It signs nobody in and creates nothing.
- **Network.** Connections go through the egress policy: a directory on a private or loopback address, the usual case, needs
  `sso_allow_private` (`TASKLANE_SSO_ALLOW_PRIVATE=true`). Why a directory refused is logged on the server; the person only sees
  that sign-in failed.
- The client is the part of LDAP v3 a sign-in needs, written in standard-library Go (`internal/ldap`: BER, filters, simple bind,
  StartTLS, subtree search). It does not follow referrals or page results, and it has no SASL or Kerberos.
- Not built:
  - group-to-role mapping (administrators are made in Tasklane)
  - ending existing sessions when the directory disables someone (their next sign-in is refused; open sessions run out)
  - SCIM-style sync of the whole directory
  - LDAP sign-in for portal customers

## SCIM 2.0 provisioning

**Admin → Identity → Create a SCIM token**, then point the identity provider at `<your address>/scim/v2` with that token (a different kind of token from the API tokens: an ordinary session or token is refused there).

- `Users`: `GET` (with `filter=userName eq "..."` or `externalId eq "..."`, `startIndex`, `count`), `POST`, `GET`, `PUT`, `PATCH` (`replace` and `add` of `active`, `userName`, `displayName`, `name.formatted`, `externalId`, with or without a path) and `DELETE`.
- `POST` for an address that already has an account takes the account over (an inactive one is reactivated; one linked to a different external id is a conflict). Accounts made this way have no password and are never administrators.
- Setting `active` to false, or `DELETE`, **deactivates** the account: nothing is removed, and its sessions and API tokens end at once. The last active administrator cannot be deactivated from outside.
- Provisioning is recorded in the audit log by "SCIM".
- Not built: `Groups`, bulk operations, sorting.

## Passkeys and security keys (WebAuthn)

A passkey or a hardware security key can finish a sign-in after the password, beside an authenticator app. Nothing replaces the password: a key is a second factor.

- **Adding one:** Settings → Security → *Passkeys and security keys* → add. Naming it and entering the password again are required, so a stolen session cannot plant a key of its own. Removing one asks for the password too. Each add and remove is in the audit log.
- **Signing in:** after the password, a person with a key is offered **Use passkey or security key** (and the code field too, if they also have an authenticator app). A person with no key signs in as before.
- **What is checked:** the challenge (single use, bound to that sign-in), the origin and the relying party id (a key made for another site, or answered on another origin, does not work), that the person was present (and verified, if `webauthn_require_user_verification` is on), the signature (ES256, EdDSA and RS256), and the signature counter: a counter that does not move forward means the credential may have been cloned, the sign-in is refused and `security.passkey_clone_suspected` is written to the audit log. Keys that never count (counter 0) are accepted. Five wrong answers end the sign-in.
- **The relying party** is the host of `base_url` (override with `webauthn_rp_id` and `webauthn_origins`). **It must be a host name, never an IP address**: with an address the page says passkeys are unavailable. A key registered for one id does not work for another, so changing `base_url` to a different host orphans the keys (people sign in with their other factor and add the key again).
- **Not verified:** attestation. Tasklane asks for none and does not check who made the key; a key is trusted because the person proved their password when they added it.
- **Lost keys:** an administrator's *reset two-factor* removes the person's keys with the authenticator app and ends their sessions. Erasing a person removes their keys; the personal-data export lists them (name and dates, never key material).
- Single sign-on sign-ins do not ask for a key: the identity provider is the one that authenticates.

## Security policy for an organization

Administration → **Security** (and `GET`/`PUT /api/v1/admin/security-policy`) sets three rules for the administrator's own organization (an instance administrator may name another with `?org=`). Each is audited (`security.policy_changed`) and takes effect at once, for people already signed in too (other nodes read it within five seconds).

- **Require a second factor.** A person with an authenticator app or a passkey signs in as usual. A person without one can still sign in with the password, but every request except *who am I*, sign-out and the two-factor and passkey pages is refused with `403` and the code `second_factor_required`; the page takes them to the Account page and says why. As soon as they add one they are released. Single sign-on sessions are not asked (the identity provider is the authority), API tokens are not affected (a token is an explicit credential made while the person had a session), and other organizations are not touched. *You cannot require a second factor before you have one yourself* (the change would hold you at the enrolment page).
- **Allowed networks.** One address or network per line (`203.0.113.0/24`, `198.51.100.7`). Empty means anywhere. When set, sign-in, every browser request, every API token and the MCP endpoint must come from them; anything else gets `403` `ip_not_allowed` (and a sign-in from outside fails with the same words as a wrong password, so it does not say why). *You cannot save a list that leaves out your own address.* The instance administrator is not subject to any organization's list, so a mistaken list can always be repaired.
- **Longest session.** Hours, 0 for no limit. A session never outlives it, however long the server's session time is.

**Behind a reverse proxy**, tell Tasklane which networks are your proxies with `trusted_proxies`. Only from those is the `X-Forwarded-For` header believed (the nearest address in it that is not itself a proxy is the visitor); from anyone else it is ignored, because a visitor can write anything in it. Without it, every request seems to come from the proxy, and an allowed-networks list is either useless or locks everyone out. This also fixes the address recorded for sign-ins and used by lockouts.

### Allowed email domains

In the same Security policy, **Allowed email domains** limits the addresses people of the organization can have. One domain per line (exact domains such as `example.com`; no wildcards, so subdomains are listed separately). Empty means any address.

- It applies to **every way an account is made or switched on**: the Users form and API, SCIM provisioning, just-in-time single sign-on accounts, and an imported or mirrored placeholder when it is claimed or activated. The check is made by the database, so a new path cannot forget it.
- **People who already have another address stay.** They are counted in the Security tab ("N active people have an address outside these domains"), keep working, and can be renamed or deactivated, but cannot be re-activated once deactivated.
- A refusal says which domains are allowed. Placeholders that cannot sign in (made by an import or the Jira mirror) are not accounts yet and are not refused.
- **Not done:** proving that your organization owns a domain (domain verification), wildcard domains, and a managed-account mode that takes over existing accounts of a domain.
