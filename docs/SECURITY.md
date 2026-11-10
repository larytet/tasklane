# Security specification and attack-surface map

Status: working document, produced by a static read of the repository at commit `3385408` (branch `main`). Nothing was run,
no server was started, no code was changed, and no secret value was read: where a file holds secrets only its path and kind
are recorded (section 9). Every claim cites `path:line` where possible. Items that were read but not proven by a test or a
run are marked **unverified**.

Companion documents: `docs/PERMISSIONS.md`, `docs/IDENTITY.md`, `docs/ORGANIZATIONS.md`, `docs/MCP.md`, `docs/EMAIL.md`,
`docs/S3.md`, `docs/HOSTED.md`, `docs/PRIVACY.md`, `docs/AUDIT.md`, `docs/BACKUP.md`, `docs/CLUSTER.md`, `docs/CONFIGURATION.md`.

Contents: 1 Scope and assumptions, 2 Assets, 3 Trust boundaries and data flow, 4 Attack surface inventory, 5 Controls that
already work, 6 Threats, 7 Findings, 8 Possible fixes / remediation plan, 9 Sensitive files (kind only), 10 Gaps in this review.

---

## 1. Scope and assumptions

**In scope.** The Go server (`cmd/tasklane`, `internal/*`), its browser UI (`internal/web/static`), the customer portal, the MCP
server and OAuth authorization server, inbound and outbound email, the importers and the Jira mirror, the Git integration,
blob storage (filesystem and S3), the automation and workflow engines, backup and restore, the cluster pieces (Redis bus,
OpenSearch), the hosted-edition features, the Android app (`android/`), the Dockerfile, Compose, Helm and systemd deployment
files, the build and release tooling, and the dependency set. Copies under `.claude/worktrees` are ignored.

**Deployment model assumed.** One or more Tasklane nodes behind a TLS-terminating reverse proxy, a PostgreSQL 16 database that
is the single source of truth, optional Redis (event bus), optional OpenSearch, optional S3 or MinIO. Either one organization
(self-hosted) or several organizations on one installation (hosted, `hosted=true`).

**Actors and trust.**

| Actor | Trust |
|---|---|
| Anonymous internet user | none; reaches only the endpoints in section 4 marked "public" |
| Portal customer | low; own session cookie (`tl_portal`), sees only their requests |
| Staff user (viewer, member, project admin) | authenticated, scoped by project role and permission scheme |
| Organization administrator | administers one tenant; project admin in each of its projects |
| Instance administrator (`Actor.Admin`) | the operator; sees every tenant (`docs/ORGANIZATIONS.md`) |
| Agent / MCP client / API token holder | acts as a person or an agent, limited by scopes (MCP only) |
| External systems (IdP, Git provider, Jira, mail relay) | trusted for what they sign; everything else they send is data |
| Database / Redis / OpenSearch / S3 operators | fully trusted; compromise of them is out of scope except where noted |

**Out of scope / assumed.** The operating system, the container runtime, the reverse proxy configuration, PostgreSQL's own
security, the Jira and Git providers' security, physical access. A hostile instance administrator is out of scope except in
the hosted edition, where the operator is the platform owner and tenants are the adversary population.

## 2. Assets

| Id | Asset | Where it lives | Impact if lost, leaked or altered |
|---|---|---|---|
| A1 | Issue content: summaries, descriptions, comments, history, internal comments | PostgreSQL (`issue`, `comment`, text versions) | confidentiality breach of customer and project data |
| A2 | Attachments | blob store (`internal/blob`), optionally AES-256-GCM sealed (`blob/encrypt.go`) | confidentiality, malware delivery |
| A3 | Credentials of people: password hashes (bcrypt), TOTP secrets, recovery codes, passkeys | `app_user`, `user_mfa`, `webauthn_credential` | account takeover |
| A4 | Sessions and tokens: session cookie, API tokens, agent tokens, OAuth grants, SCIM tokens, portal sessions | tables `session`, `api_token`, `oauth_*`, `scim_token`; hashed with SHA-256 (`domain/users.go:97`) | impersonation |
| A5 | Stored integration secrets: Git tokens and webhook secrets, SSO client secrets, LDAP bind password, IMAP passwords, automation webhook secrets, mail reply keys | DB, sealed with AES-256-GCM under the instance secret key (`domain/git.go:28-58`) | pivot into Git, IdP, mail, internal networks |
| A6 | The instance secret key and link secret | env/file, or **the database** (`domain/git.go:72-95`, `domain/attachments.go:90`) | decrypts A5, forges signed links and reply addresses |
| A7 | Jira service and personal tokens | memory only (`internal/credvault`) | Jira access |
| A8 | Audit log (hash-chained, append-only) | `audit_event` | loss of accountability |
| A9 | Personal data of staff and customers (email, IP, user agent, mail bodies) | many tables, `mail_message`, `session`, `login_attempt` | privacy / GDPR |
| A10 | Tenant isolation | `effective_member` view, triggers, app checks (`docs/ORGANIZATIONS.md`) | cross-tenant leakage in the hosted edition |
| A11 | Backups and exports | backup directories, bundle zips | full copy of A1 to A9 |
| A12 | Availability of the service | process, DB pool, disk, mail relay | outage |
| A13 | Release integrity | Ed25519-signed tarballs, SBOM, NOTICE (`internal/release`) | supply-chain compromise of operators |
| A14 | The Android app and its stored token | device app-private storage | mobile account takeover |

## 3. Trust boundaries and data flow

```
 Internet ──TLS──► reverse proxy (operator) ──HTTP──► tasklane node(s) :8080  (http.Server, cmd/tasklane/main.go:430)
   │                                                  ├─ /            static UI (CSP, no inline script)
   │ browsers, Android app, API clients               ├─ /api/v1/*    REST (cookie+X-Requested-With, or Bearer)
   │ portal customers                                 ├─ /api/v1/portal/*  portal (separate cookie)
   │ MCP/agent clients (OAuth 2.1 + PKCE)             ├─ /mcp, /oauth/*    agents
   │ IdPs (OIDC/SAML POST), SCIM clients              ├─ /scim/v2/*        provisioning
   │ Git/Jira/mail providers (signed webhooks)        ├─ /api/v1/{git,jira,mail}/…  signed webhooks (public routes)
   │ SMTP senders ─────────TCP 25────────────────────►└─ smtp_listen        built-in MX receiver
   │
   └─ tasklane node ──► PostgreSQL (all state; single role)           [trust: full]
                    ──► Redis/Valkey (events, presence, no signing)   [trust: full]
                    ──► OpenSearch (ids only, rechecked in PG)        [trust: full]
                    ──► S3/MinIO or local disk (blobs)                [trust: full]
                    ──► SMTP relay (outbound, TLS verified)           [trust: full]
                    ──► egress.Policy client ──► Git, Jira, OIDC, LDAP, IMAP, webhooks, billing   [untrusted peers]
```

Boundaries that matter:

1. **Internet to node.** Everything unauthenticated must be treated as hostile: portal, login, SSO callbacks, OAuth, the signed
   webhooks, SMTP, `/files/{id}` signed links, health endpoints.
2. **Session to role.** `Authenticate` (`domain/users.go:106`) yields an `Actor`; `access()` (`domain/issues.go:311`) and
   `role()` decide per project. There is no row-level security in PostgreSQL; tenant and project isolation is application code
   plus the `effective_member` view and triggers.
3. **Tenant to tenant.** Hosted organizations share one database, one global config set, global project keys, instance-level SSO,
   SCIM, Git, mail and audit (`docs/ORGANIZATIONS.md`, "What is shared").
4. **Server to outside.** Server-side fetches go through `internal/egress` (`egress.go:43-53,123`); the S3 client and SMTP client
   are operator-configured and do not.
5. **Content to renderers.** User text reaches the browser (DOM built with `h()`, never `innerHTML`), email (HTML-escaped in
   `domain/notify.go:398`, `customer_mail.go:134-138`), exports (CSV formula neutralization, `domain/export.go:100`), Jira
   (Markdown/ADF), and MCP clients (marked as data, `mcp/server.go:~210`).
6. **Automation to authority.** Rules, workflow post-functions and Git-automation run as system actors with `Admin: true`
   (finding F-05).

## 4. Attack surface inventory

"Exposure": **public** = reachable without a session; **auth** = needs a staff session or token; **admin** = instance or
organization administrator; **signed** = public route protected by an HMAC or token; **ops** = operator only.

| Id | Surface | Location | Exposure | Current controls |
|---|---|---|---|---|
| S-01 | Browser UI shell and assets | `internal/web/web.go`, `internal/web/static/*.js`, `index.html` | public | CSP `default-src 'self'` with no inline script, `frame-ancestors 'none'`, `base-uri 'none'`, nosniff, DENY framing, same-origin referrer (`api/api.go:~620-645`); DOM built by `h()` helpers, no `innerHTML`/`eval` found in `static/*.js`; linkify only http/https/mailto (`static/lib.js` `inline`). Gaps: `style-src 'unsafe-inline'`, no HSTS |
| S-02 | Password / MFA / passkey login | `api/api.go:124-126,773-830`; `domain/security.go:79-181,356-412`; `domain/users.go` | public | per-email lockout 5 failures / 15 min (`security.go:30-31,79`), constant-ish time dummy bcrypt (`:146`), MFA pending token 5 attempts / 5 min, TOTP step claimed atomically, org IP allow-list and session cap (`newSessionVia`), generic failure text. Gaps: no per-IP throttle (F-02), no CSRF header on login (F-07) |
| S-03 | Staff REST API (about 400 routes) | `api/*.go`; `s.auth` `api/api.go:700-735` | auth | session cookie (HttpOnly, SameSite=Lax, Secure iff `base_url` is https, `api.go:754`) plus mandatory `X-Requested-With` on unsafe cookie requests; Bearer path skips CSRF; policy enforcement per request (`EnforcePolicy`); route registry plus authorization matrix test `api/matrix_test.go:1277`; cross-tenant sweep test (docs/ORGANIZATIONS.md); scoped agent tokens refused on REST (`users.go:~140`); body cap 3 MiB (`api.go:938`) |
| S-04 | Administration API | `api/{security,sso,groups,fields,screens,workflows,quota,search_admin,exchange,privacy}.go` | admin | `Actor.Admin` / `OrgAdmin` checks in domain; every non-ordinary mutating route auto-audited (`api/audit.go:82-107`) |
| S-05 | Customer portal | `api/portal.go:73-370`; `domain/customers.go`, `portal_captcha.go`, `portal_files.go`, `requests.go` | public + portal cookie | separate cookie `tl_portal` and middleware; CSRF header (`portal.go:46`); allow/block lists; proof-of-work captcha (HMAC-signed challenge); honeypot; per-address and per-network code limits; 1 h lock after failures; same answer for unknown addresses; export/erase (GDPR); customers see only non-internal comments (`requests.go:562`) |
| S-06 | OIDC / SAML / LDAP sign-in | `api/sso.go:81-190`; `domain/sso.go`, `sso_ldap.go`, `internal/saml`, `internal/ldap` | public | PKCE S256, state (10 min, single use), nonce, RS256 only, JWKS rotation, issuer match; SAML: RSA-SHA2 only, exclusive C14N, no DOCTYPE, single assertion, signature-wrapping checks; LDAP: filter escaping, search-then-bind, exactly one entry, empty password refused, TLS enforced; all through `egress`. Gaps: state not bound to browser (F-07), `ldap_skip_verify` exists (`sso_ldap.go:101`) |
| S-07 | SCIM 2.0 provisioning | `api/scim.go`; `domain/scim.go:80-95` | signed (bearer SCIM token) | token stored hashed, constant-time compare, separate from API tokens, last-admin protection, audit. Gap: token is instance-wide, not per organization (F-19) |
| S-08 | OAuth 2.1 authorization server (for MCP) | `api/oauth.go:62-160`; `domain/oauth.go` | public | PKCE required, exact redirect match, code single use and spent on first try, refresh rotation with reuse detection that revokes the grant (`oauth.go:278`), public clients only. Gaps: open dynamic registration (F-04), consent shows attacker-chosen client name |
| S-09 | MCP server | `mcp/server.go:123-330`, `tools.go`, `bulk.go`, `subscribe.go`; `domain/agents.go:98-125` | auth (bearer) | Origin check against DNS rebinding (`server.go:102`), scopes ceiling plus permission scheme, per-agent token bucket and hourly write budget (in memory), idempotency keys, bulk dry-run default, kill switch and pause for agents, own call log, 1 MiB body cap, "treat issue text as data" instruction. Gaps: F-18 |
| S-10 | WebSocket (live updates, presence) | `rt/hub.go`; `api/api.go:435-444` | auth (single-use 30 s ticket) | ticket redeemed once, same-origin Accept, 4 KiB frames, 30 msg/s, 50 subscriptions, per-subscribe `CanRead`, periodic re-auth. Gaps: no per-user connection cap, ticket in query string, possible map race (F-20) |
| S-11 | File download | `api/attachments.go:150-190`; `domain/attachments.go:510-600` | auth or signed link | links are HMAC(attachment, expiry, variant) valid 5 min, issued only after permission check; non-previewable types forced to download as `application/octet-stream`; `nosniff`; `sandbox` CSP; inline only for png/jpeg/gif/webp/bmp/pdf/text/audio/video (`blob/sniff.go:126`). Link secret is in the DB (F-01) |
| S-12 | Upload endpoints (multipart, raw, resumable, direct-to-S3) | `api/attachments.go:16-150`; `domain/attachments*.go`; `blob/s3_direct.go` | auth | per-class size limits, sniffed type vs declared (`TypeFlag`), zip bomb checks (`blob/sniff.go:77-103`), content-addressed storage (name = SHA-256, `blob.go:56-62` validates hex), 8 concurrent upload slots (`ErrBusy`), project storage quota, plan quota, filename sanitizing (`blob.go:~275-290`). Gap: "malware scan" is an EICAR stub (F-10); raw POST has no `MaxBytesReader` |
| S-13 | Inbound mail webhook and bounce webhook | `api/inbound_mail.go:14-40,91`; `domain/inbound.go:340`, `bounce.go:182` | signed | HMAC-SHA256 over the body with constant-time compare, 26 MiB cap, dedupe by Message-ID, raw stored first, per-sender and per-domain and per-issue limits, no auto-replies (no backscatter), known sender only, own DKIM verification, agent senders refused, quarantine. Gaps: F-08 |
| S-14 | Built-in SMTP receiver and IMAP poller | `mail/smtpd.go` (limits `:52-80`), `mail/imap.go`, `domain/imap.go`, `domain/smtpd.go` | public (TCP 25, if `smtp_listen`) / outbound | only own domain, 25 MiB, 50 recipients, 100 connections, 10 per IP, 60 s idle, optional STARTTLS, never trusted without DKIM, IMAP over verified TLS and `egress` |
| S-15 | Git and Jira inbound webhooks | `api/git.go:13-30`, `api/jira.go:12`; `domain/git.go:317-360`, `domain/jira.go:411-414`; `git/git.go:139-170` | signed | provider HMAC (GitHub `X-Hub-Signature-256`, etc.), constant-time compare, per-connection secret sealed, delivery-id dedupe, optional source CIDR allow-list, `MaxWebhookBytes`. Gaps: CIDR check uses raw peer address (F-09), no rate limit |
| S-16 | Outbound HTTP, LDAP, IMAP | `internal/egress/egress.go`; `domain/jira.go:73`, `git_dev.go:283`, `sso.go:66`, `trules.go:439`, `billing.go:89`; `search/os_client.go:75`; `ldap/ldap.go:197`; `mail/imap.go:58` | server-side | resolve once and connect to the checked IP (rebinding-safe), every resolved address checked, redirects limited to 3 with credential stripping across hosts, metadata addresses always blocked, private ranges blocked unless the connection allows, size and time bounds, `Proxy: nil`. Gaps: F-14 |
| S-17 | Automation rules and workflow post-functions | `domain/automation.go`, `trules.go`, `git_auto.go`; docs/AUTOMATION.md | admin (project admin writes rules) | no code execution: no `os/exec`, templates, scripting engine or expression evaluator anywhere in runtime code (only `internal/licenses` shells out to `go list` at build time); fixed action set; `{{from}}`-style `strings.Replacer` only (`trules.go:~346`); loop guard, 3-level cap, 300 runs/rule/hour, 100 rules/project, once-only runs; webhook through `egress` with HMAC and sealed secret. Gap: F-05 |
| S-18 | Importers and Jira mirror | `api/exchange.go:56-130`; `domain/import*.go`, `import_tools.go`, `import_csv.go`, `jira*.go`; `internal/jira` | admin | admin-only; bundle files verified against manifest SHA-256 before use (`import.go:62-100`); no extraction to disk (entries read in memory); dry run; rollback; Jira creds in memory vault only (`credvault`), HMAC on Jira webhooks; `jiraKey` rejects quotes/space (`jira.go:236-240`). Gaps: unbounded `io.ReadAll` of zip entries (`import.go:51-58`) and 4 GiB temp file in `os.TempDir` (`exchange.go:55-63`) |
| S-19 | Export, backup, restore | `domain/export.go`; `cmd/tasklane/backup.go`; `domain/backup.go:156-610` | ops / admin | repeatable-read snapshot, manifest with per-file SHA-256, 0750/0640 files, restore only into an empty DB or with `--replace` (pause, safety backup, one transaction), audit chain verified after restore. Gaps: F-11 |
| S-20 | Search (TQL to SQL), OpenSearch | `search/tql`, `search/compile*.go`, `aggregate.go`, `search/os_*.go` | auth | TQL parsed to AST, values bound as parameters (`c.arg`), identifiers come from fixed sets or numeric field ids, query length and depth limits (`tql.go:17,186`), OpenSearch returns ids only and each id is rechecked in PostgreSQL (`docs/OPENSEARCH.md:31`), visibility scope applied in the matcher. Gap: no DB statement timeout (F-03) |
| S-21 | Rich text, Markdown, ADF converters | `richtext/*.go`; `api` `POST /api/v1/richtext/convert`; `domain/sanitize.go` | auth | control characters stripped; markdown links limited to http/https/mailto and `/` or `#` (`sanitize.go:53-56`); stored text is plain text; ADF opaque blocks kept as data. Gaps: F-21 |
| S-22 | PostgreSQL | `internal/store`, `internal/store/migrations` (81 migrations), `domain/*.go` | trusted backend | all values bound with `$n`; dynamic SQL fragments reviewed (list below); append-only triggers on `audit_event`; org triggers; one DB role for everything (no RLS) |
| S-23 | Redis / Valkey bus | `internal/redis/redis.go`; `domain/bus*.go`; `docs/CLUSTER.md` | internal | optional `rediss://` with TLS 1.2+; carries hints only (issue key, actor name, versions); messages are not signed. Gap: F-20 |
| S-24 | Blob stores and tiering | `internal/blob/*` | internal | AES-256-GCM chunked with per-object key and wrapped under rotating master keys (`encrypt.go`), SHA-256 verification on commit, S3 SigV4 (`s3sign.go`), SSE option, presigned multipart (`s3_direct.go`), read-back compare on tiering |
| S-25 | Configuration and secret handling | `internal/config/config.go`, `cmd/tasklane/main.go:254`; `domain/git.go:28-95`; `credvault` | ops | `*_FILE` variants for secrets, `config print` masks, `Cred` type redacts in every fmt verb. Gap: F-01 |
| S-26 | Health and status | `api/api.go:101-123` | public | `/healthz` constant; `/readyz` and `/api/v1/system/status`. Gap: `/readyz` prints degraded details unauthenticated (F-22) |
| S-27 | Hosted / multi-tenant features | `domain/hosted.go`, `orgs.go`, `billing.go`, `managed.go`; `docs/HOSTED.md` | admin / operator | plans, seats and project limits enforced in the database, org-bound triggers, per-org security policy, signed billing webhook through `egress`. Gaps: F-06, F-19 |
| S-28 | CLI and release tooling | `cmd/tasklane/*`, `cmd/relsign`, `internal/release/release.go` | ops | `create-admin`, `backup`, `restore`, `blobs`, `upgrade`; Ed25519 signature over `SHA256SUMS`, archive hashes, SBOM and NOTICE consistency checks, tar path-traversal guard (`release.go:257`) |
| S-29 | Android app | `android/app/src/main/AndroidManifest.xml`, `res/xml/network_security_config.xml`, `data/Session.kt`, `app/build.gradle.kts` | device | `allowBackup=false`, single launcher activity, Bearer session token. Gaps: F-12 |
| S-30 | Container and deployment | `Dockerfile`; `deploy/compose/docker-compose.yml`; `deploy/helm/tasklane/*`; `deploy/systemd/tasklane.service`; `deploy/tasklane.conf.example` | ops | distroless `static-debian12:nonroot`, `CGO_ENABLED=0`, `-trimpath`; systemd hardening (NoNewPrivileges, ProtectSystem=strict, MemoryDenyWriteExecute, address-family restriction); Helm: `runAsNonRoot`, secrets via `existingSecret`, NetworkPolicy, PDB. Gaps: F-17 |
| S-31 | CI and release pipeline | `.github/workflows/pages.yml` (only workflow), `Makefile`, `scripts/*.sh` | ops | tests, vet, e2e, interop, chaos, upgrade and soak scripts exist as Make targets; signed releases with SBOM. Gap: F-16 |
| S-32 | Dependencies | `go.mod` (4 direct: coder/websocket, pgx v5, x/crypto, x/text), `e2e/package.json`, `android/**/build.gradle.kts`, `dkimpy-1.1.8.tar.gz`, `dnspython-2.8.0-py3-none-any.whl` | build | small direct set, NOTICE generated and tested, licence package. Gaps: F-16 |
| S-33 | Audit, logging, PII | `api/audit.go`, `domain/audit.go`, `domain/privacy.go`, `domain/customer_privacy.go`, `mcp/server.go` (call log) | admin | hash-chained append-only audit, CSV export formula-safe, erase with chain recompute, retention job, legal hold. Gaps: F-15 |
| S-34 | Outbound notifications | `mail/mail.go:136-232`, `domain/notify.go`, `customer_mail.go`, `inbox.go` | server-side | header values CR/LF stripped (`hdr`, `mail.go:193`), subject Q-encoded, HTML bodies escaped, STARTTLS required unless `none`, return-path and reply tokens HMAC-signed and recipient-bound (`inbound.go:210`), suppression list, bounce handling |
| S-35 | Dashboard iframe gadgets | `domain/dashboards.go:~400`; CSP `frame-src` (`api.go:~615`) | admin to auth | https only, host allow-list from config, no userinfo; CSP limits frames to those hosts |
| S-36 | Jira write-through | `domain/jira_write.go`, `credvault` | auth | per-person token held in memory for the session only, wiped on logout |

Dynamic SQL fragments reviewed and found safe by construction: `search/search.go:102,189,219` (numeric LIMIT, parameters),
`search/aggregate.go:71` (column expressions chosen from a fixed set), `search/compile_versions.go:23` (`kind` is a constant),
`domain/trules.go:355,364-427` (field key is a constant or matches `fieldKeyRe`, `fields.go:93`), `domain/backup.go:207,542`
(identifiers quoted with `pgx.Identifier.Sanitize`), `store/store.go:256` (integer). Command injection: no `os/exec` in runtime
packages. Path traversal: blob paths require a 64-hex name (`blob.go:23,56-62`); attachment names are sanitized
(`blob.go:~275-290`) and sent as `filename*=UTF-8''<escaped>` (`api/attachments.go:~187`).

## 5. Controls that already work (do not regress)

- Authorization matrix test over every registered route and role, plus a cross-tenant sweep (`api/matrix_test.go`,
  `docs/ORGANIZATIONS.md`); audit-coverage test keeps the "ordinary work" list honest (`api/audit_test.go:70`).
- Sessions and tokens are hashed, looked up on every request, and revoked immediately (`domain/users.go:97-160`).
- Strong OIDC and SAML verification, LDAP injection-safe filter, SSRF-resistant egress client, OAuth reuse detection.
- No scripting or code execution in automation; no shelling out in the server.
- XSS posture: DOM built from nodes, strict CSP without inline script, attachments sandboxed and forced to download.
- Inbound mail: DKIM proof or trusted verdict, quarantine, single-From rule, signed recipient-bound reply tokens, no backscatter.
- Tamper-evident audit log with DB-level append-only triggers; GDPR export and erase tooling.
- Release signing (Ed25519), SBOM, NOTICE, distroless non-root image, systemd and Helm hardening.

## 6. Threats

Severity uses likelihood x impact for a typical self-hosted install, and is raised by one step where the hosted
multi-tenant edition makes it worse (marked "H+"). Ids T-xx map to findings F-xx in section 7.

| Id | Threat (STRIDE) | Entry | Severity | Findings |
|---|---|---|---|---|
| T-01 | Credential stuffing and password guessing across many accounts; targeted lockout of a victim (DoS) | S-02, S-05 | High | F-02 |
| T-02 | Resource exhaustion by unauthenticated or low-privilege clients (slow bodies, state-row floods, large uploads, expensive searches) | S-02, S-06, S-08, S-12, S-13, S-15, S-20 | High (H+) | F-03, F-04 |
| T-03 | Database or backup disclosure decrypts every stored integration secret | S-19, S-22, S-25 | High | F-01, F-11 |
| T-04 | Privilege confusion: a project admin's rule executes with instance-admin rights; cross-tenant references | S-17, S-27 | High (H+) | F-05 |
| T-05 | Cross-tenant data leak through an application bug (no RLS, shared global config, global keys, instance-level SCIM/SSO/audit) | S-03, S-27 | Medium (H+: High) | F-06, F-19 |
| T-06 | Forged or replayed inbound email creates comments as a real user | S-13, S-14 | Medium | F-08 |
| T-07 | Malicious attachment delivered while scan says "clean" | S-12, S-11 | Medium | F-10 |
| T-08 | Login CSRF and session fixation-like attacks via SSO | S-02, S-06 | Medium | F-07 |
| T-09 | Token theft from device or network (Android cleartext, plain storage; long-lived sessions) | S-29, S-03 | Medium | F-12, F-13 |
| T-10 | SSRF into internal networks via admin-configured URLs and automation webhooks | S-16, S-17 | Medium | F-14 |
| T-11 | Undetected abuse because failures and source addresses are not logged | S-33 | Medium | F-15 |
| T-12 | Supply-chain compromise or vulnerable dependency shipped unnoticed | S-31, S-32 | Medium | F-16 |
| T-13 | Insecure default deployment (plain HTTP port, key in DB, secrets in env) | S-30 | Medium | F-17 |
| T-14 | OAuth/MCP abuse: client-name phishing, registration exhaustion, over-broad personal tokens, prompt injection via issue text | S-08, S-09 | Medium | F-18 |
| T-15 | Stored XSS or unsafe link through Markdown/dev links | S-01, S-21 | Low | F-21 |
| T-16 | Crash by deeply nested input or concurrent map access | S-10, S-21 | Low to Medium (unverified) | F-20 |
| T-17 | Information disclosure through health and error surfaces | S-26 | Low | F-22 |
| T-18 | Weak account hygiene (no password reset, min length 8, optional MFA) | S-02 | Low | F-23 |

## 7. Findings

Severity: Critical, High, Medium, Low, Info. "Verified" means the code path was read end to end; "unverified" means plausible
from the code but not proven by a test or a run. No Critical issue and no unauthenticated remote code execution was found:
there is no code-execution surface, no command execution, and SQL is parameterized.

### F-01 Instance secret key defaults to a copy stored in the database (High, verified)
- `domain/git.go:72-95` returns the configured `secret_key`, otherwise generates one and stores it in `setting` (`key='secret_key'`);
  the code comment itself calls this weaker. `link_secret` (signed file links) is likewise generated into the database
  (`domain/attachments.go:90-103`). `DerivedKey` (`git.go:62`) derives the MCP session key and the captcha and reply-address keys
  from the same secret.
- The Compose quick start and `deploy/tasklane.conf.example` do not set it; `tasklane doctor` does not warn (`internal/doctor/doctor.go`
  has no `secret_key` check). A database dump, a read replica, a SQL injection anywhere, or a backup then yields both ciphertext and key
  for every Git token, SSO client secret, LDAP bind password, IMAP password and automation webhook secret (A5), and lets an attacker
  forge file links and reply addresses.
- One key for all purposes, no key id in the ciphertext, no rotation tool, no AAD binding of a ciphertext to its row (`git.go:28-58`).

### F-02 Login protection is per email only (High, verified)
- Lockout counts failures per email (`security.go:79-96`, 5 per 15 minutes). There is no per-IP or global throttle, so one source
  can try one password against every account indefinitely (credential stuffing) and each attempt costs a bcrypt compare
  (`users.go:57`, `DefaultCost`).
- The same lockout lets anyone lock any known user out for 15 minutes by sending five bad attempts (targeted DoS); it applies to the
  administrator too, and the organization IP allow-list does not protect the lock counter (the check precedes the policy).
- Failed attempts are stored without an address limit and purged only on the next write (`security.go:98-101`).
- Portal sign-in has per-address and per-network limits (`customers.go`), which is better, but "per network" depends on a correct
  `trusted_proxies` (see F-09).

### F-03 No server-level timeouts, body caps or database statement limits (High for hosted, Medium otherwise, verified)
- `http.Server` sets only `ReadHeaderTimeout: 10s` (`cmd/tasklane/main.go:430`): no `ReadTimeout`, `WriteTimeout`, `IdleTimeout` or
  `MaxHeaderBytes`; slow-body clients hold connections and goroutines.
- `POST /api/v1/issues/{key}/attachments` streams `r.Body` without `http.MaxBytesReader` (`api/attachments.go:17-45`); only the
  per-class limit in the domain stops it after bytes have been written to staging. `POST /api/v1/import` accepts 4 GiB into
  `os.TempDir()` (`api/exchange.go:55-63`), admin only. Webhook endpoints read up to 26 MiB before any authentication
  (`api/inbound_mail.go:15-20`) and the Git webhook does a DB lookup per request (`domain/git.go:322`).
- No `statement_timeout` or pool tuning (`store/store.go:39`, default `pgxpool`): a few expensive TQL or aggregate queries
  (`search/aggregate.go:71`) can exhaust the pool. **unverified** for a concrete slow query; the absence of limits is verified.
- Rate limiting exists only for login (per email), portal codes, MCP calls (in memory, per node, `mcp/server.go:~260`), inbound mail
  per sender, SMTP connections and WebSocket frames. There is no per-user or per-IP limit on the REST API, search, exports or
  uploads, and no per-organization fairness in the hosted edition.

### F-04 Unauthenticated endpoints that write state or consume shared capacity (Medium, verified)
- `GET /api/v1/auth/sso/{name}/start` inserts an `sso_state` row per request (`domain/sso.go:474`); rows live 10 minutes and are purged
  hourly (`docs/PRIVACY.md`).
- `POST /oauth/register` is open by design but capped globally (`domain/oauth.go:79-86`, `maxOAuthClients`, `maxRegPerHour`): an
  attacker can exhaust the quota and block legitimate MCP clients from registering.
- `POST /api/v1/portal/{key}/code` sends mail (captcha and limits apply), `GET /api/v1/portal/{key}/challenge` signs on demand.
- No per-source limit on any of these (see F-02/F-03).

### F-05 System actors run with `Admin: true` (High in hosted, Medium otherwise, verified in code; exploitability unverified)
- `autoActor` returns `Actor{... Admin: true ...}` (`domain/automation.go:602`), `codeActor` the same (`domain/git_auto.go:363`), and the
  `create_subtask` job builds `Actor{Admin: true}` (`domain/trules.go:553`). In this code base `Actor.Admin` means *instance*
  administrator (`users.go:109`, `actorFor`).
- Rules are written by project admins (including tenant admins). They run with instance-admin authority, bypassing permission
  schemes and organization checks inside each domain call, constrained only by "a project's rule acts on that project only"
  (`automation.go:884`) and by the workflow checks.
- If the rule's creator is missing, the actor falls back to the oldest active administrator of the whole instance
  (`automation.go:598`, `git_auto.go:358`), which is normally an instance administrator of the Default organization, not the tenant.
- Actions that take a person by email or name (`assign`, `set_field` user fields, `notify`) are resolved under that authority;
  whether they can reference a person of another organization was **not verified**. `ReleaseIssue` for agent leases also uses
  `Admin: true` (`domain/agents.go:443`).

### F-06 Tenant isolation is application-level; issue security levels do not exist (Medium; High in hosted, verified)
- No PostgreSQL row-level security anywhere (`grep ROW LEVEL SECURITY` in `internal/store` finds nothing); one DB role does
  everything. README says isolation is "enforced by the database", but `docs/ORGANIZATIONS.md` describes the `effective_member`
  view and triggers on memberships only. Every query that reads issues, comments, attachments, history or exports must repeat the
  scope; the sweep test mitigates but cannot cover queries added later.
- Issue security levels are "not built" (`docs/PERMISSIONS.md:12`); the only per-issue restriction is the Jira-mirror `restricted`
  flag (`domain/issues.go:332-337`). Visibility is therefore per project role, so sensitive tickets need a separate project.
- Shared across tenants (`docs/ORGANIZATIONS.md`): workflows, fields, screens, issue types, project keys (a taken key is
  reported to a tenant that tries it, which is an existence oracle), groups, SSO, SCIM, Git connections, mail sources, OpenSearch,
  and the audit log; the instance administrator sees all.

### F-07 Login CSRF and unbound SSO state (Medium, verified)
- `POST /api/v1/login` and the MFA/passkey routes carry no `X-Requested-With` requirement (`api/api.go:124-126`) and `decode`
  ignores `Content-Type` (`api.go:937-945`), so a cross-site form can sign a victim's browser in as an attacker-chosen account
  (the portal sign-in routes do require the header, `portal.go:46`).
- The OIDC/SAML `state` or RelayState is a database row, not tied to the browser that started the flow (no cookie set in
  `SSOStart`, `domain/sso.go:474-490`; none checked in `SSOCallback`). An attacker can start a flow, authenticate at the IdP as
  themself, and feed the callback URL to a victim, who is then signed in as the attacker (login CSRF); the victim's later input is
  captured in the attacker's account.

### F-08 Inbound-email proof of origin can be replayed (Medium, partly unverified)
- Own DKIM check requires only that `From` is signed, no `l=`, not expired, key length at least 1024 bits (`mail/dkim.go:134`, From
  check near `:195`). It does not require `Subject`, `To`, `Date` or `Message-ID` to be signed, has no maximum message age when the
  signature has no `x=`, and consults neither DMARC nor SPF (`docs/EMAIL.md`).
- Anyone who has ever received one DKIM-signed message from a user can re-deliver it (and, where unsigned headers decide the
  target, such as `X-Original-To`, an unsigned `Subject` key or a re-written `Message-ID`) to make it appear as that user's comment
  on a chosen issue. The Message-ID dedupe (`domain/inbound.go:444-452`) does not help if the Message-ID is unsigned.
  Recipient-bound reply tokens (`inbound.go:210`) limit this for reply addresses. **unverified**: end-to-end exploitation.
- RSA 1024-bit keys are accepted; trusted-source `Authentication-Results` is believed as is (documented operator responsibility).

### F-09 Client-address handling is inconsistent behind a proxy (Medium, verified)
- Most of the API uses `Svc.ClientIP` with `trusted_proxies` (`security_policy.go:301-321`), but the audit record writes the raw peer
  address (`api/audit.go:94`), the Git webhook CIDR allow-list is evaluated against `r.RemoteAddr` (`api/git.go:19`, `domain/git.go:333`), and
  the SMTP per-IP limit uses the peer. Behind a reverse proxy the audit log then records the proxy for every action, and a webhook
  allow-list either blocks the provider or, if the proxy range is listed, admits everyone who can reach the proxy.
- Without `trusted_proxies` set (the default, and `doctor` does not warn), every request seems to come from the proxy, so the
  per-email-per-network portal limits and any allowed-networks policy are wrong in both directions.

### F-10 Malware scanning is a stub (Medium, verified)
- `blob.Scanner` only looks for the 68-byte EICAR test string (`internal/blob/sniff.go:107-124`; its comment says it stands in for an
  external scanner "behind the same call"). Everything else is stored with `scan_state='clean'` (`domain/attachments.go:~395`) and is
  downloadable. `docs/S3.md` and the UI describe a "malware signature" check, which can mislead operators.
- Mitigations that do work: forced download for all but a short list of types, `sandbox` CSP, `nosniff`, content-type sniffing, zip
  bomb limits (zip only: gzip and nested archives are not inspected).

### F-11 Backups and exports are unencrypted and carry everything (Medium, verified)
- `docs/BACKUP.md`: a backup holds password hashes, sealed integration secrets, the audit log and attachments in clear (even when the live
  store encrypts them). Combined with F-01 the sealed secrets are decryptable from the same directory (the key is in the `setting`
  table copy). Files are mode 0640 (`domain/backup.go:163,202,286`) and the directory mode 0750, i.e. group-readable.
- `restore` trusts the manifest's table and column names (quoted, `backup.go:207,542`), so a tampered backup can overwrite any table
  (users, admin flags). The manifest has per-file hashes but no signature, so a backup is only as trustworthy as its storage. Operator-only.
- Bundle import reads zip members fully into memory (`import.go:51-58`); admin-only.

### F-12 Android app stores and sends the session token weakly (Medium, verified)
- `network_security_config.xml:3`: `cleartextTrafficPermitted="true"` for all domains (so a typo or downgrade leaks the bearer token).
- The token is kept in plain `SharedPreferences` (`data/Session.kt:7-17`), not the Android Keystore or `EncryptedSharedPreferences`; no
  screen lock or biometric gate; release build is not minified or obfuscated (`app/build.gradle.kts:39`), no certificate pinning.
- The token is a full 14-day staff session (the same token as the cookie, `README` in `android/`), not a scoped or device-bound token.
  Logout revokes it only when sent as a cookie (documented workaround).
- Distribution: sideloaded APK with a `.sha256` published next to it, so the checksum offers no authenticity beyond the download origin.

### F-13 Session and token lifecycle (Low to Medium, verified)
- Default session lifetime is 14 days (`session_ttl=336h`), absolute only, no idle timeout, no rotation after privilege changes, and no
  binding to IP or user agent (`security.go:109-130`). An organization may cap it ("Longest session").
- The session cookie has no `__Host-` prefix and `Secure` depends on `base_url` starting with `https://` (`api.go:754`; default
  `http://localhost:8080`); no `Strict-Transport-Security` header anywhere. A TLS-terminating proxy with an `http` `base_url` yields
  non-Secure cookies; `doctor` does not warn.
- Unscoped API tokens act with the owner's full rights including administrator and last up to five years (`users.go:150-160`); they
  have no per-token network restriction.
- Passwords: minimum 8, no breach or common-password check, MFA optional (organization policy can require it), passkeys are only a second factor.

### F-14 Egress policy gaps (Low to Medium, verified)
- `Policy.Check` (`egress.go:43-53`) blocks loopback, link-local, RFC1918, ULA, multicast, unspecified and three metadata addresses.
  It does not block CGNAT `100.64.0.0/10`, benchmark `198.18.0.0/15`, `192.0.0.0/24`, Azure wire server `168.63.129.16`, or IPv6
  transition ranges (NAT64 `64:ff9b::/96`, 6to4). Cloud-internal services in those ranges are reachable by any configured URL.
- `webhook_allow_private`, `sso_allow_private`, `imap_allow_private`, `opensearch_allow_private` are instance-wide flags. With
  `webhook_allow_private=true` any project administrator (including a tenant admin) can make the server POST issue data to any internal
  address (blind SSRF with attacker-influenced body and an HMAC header).
- Automation webhooks send issue payloads to any public URL by design (exfiltration path for a malicious project admin).
- The Helm NetworkPolicy has `policyTypes: [Ingress]` only (`deploy/helm/tasklane/templates/networkpolicy.yaml`); there is no egress
  policy to back up the application-level one.
- The S3 client uses `ProxyFromEnvironment` and no egress check (`blob/s3.go:77`); acceptable because the endpoint is operator config.

### F-15 Detection and forensics are thin (Medium, verified)
- The API audit records only successful (2xx) non-ordinary mutations (`api/audit.go:96-98`); logins, failed logins, 401/403, MCP
  denials and read access are not audited (login attempts live in `login_attempt` and are purged after 24 hours,
  `security.go:99-100`). A failed audit write only logs a line (`audit.go:104`) and the request still succeeds.
- No access log, request id, or structured logging (standard `log.Printf` only); no security-event export to a SIEM, and the chain is
  tamper-evident, not tamper-proof (a DB superuser can rewrite and re-chain; `docs/AUDIT.md` says so). Audit entries carry no
  organization (`docs/ORGANIZATIONS.md`), so tenants cannot see their own.
- PII held: IP and user agent per session (`security.go:119-125`), emails in `login_attempt`, raw inbound mail 90 days
  (`mail_raw_retention_days`), MCP call log arguments (`mcp/server.go:~351`, can contain issue text). Erasure and retention
  tooling exist (`docs/PRIVACY.md`); free-text mentions beyond name, address and `@name` are not found.

### F-16 CI, dependency and supply-chain hygiene (Medium, verified)
- `.github/workflows/pages.yml` is the only workflow: no automated test, vet, lint, `govulncheck`, SAST, secret scanning, dependency
  review, container scan or SBOM verification on pull requests. Tests are Make targets (`Makefile`) run by hand. (The change log in
  the task context mentions CI commits that are not in this repository's history; any CI that exists is outside this repo.)
- No Dependabot or Renovate configuration; Docker base images are tag-pinned only (`golang:1.27`, `postgres:16`, `busybox:1.36`,
  `gcr.io/distroless/static-debian12:nonroot`), not digest-pinned; Compose builds from the tree.
- Binary third-party archives are committed at the repository root (`dkimpy-1.1.8.tar.gz`, `dnspython-2.8.0-py3-none-any.whl`) with no
  recorded provenance or hash; `e2e` pulls an npm tree and the Android build pulls Gradle dependencies, neither covered by the NOTICE or
  licence gate for vulnerabilities.
- Test private keys are committed under `internal/saml/testdata/` (kind: PEM keys and certificates). They are fixtures, but they must
  never be reused anywhere else; add a secret-scanner allow-list so they do not mask real leaks.
- Positive: only four direct Go dependencies, Ed25519-signed releases with SBOM (`internal/release`), reproducible archives.

### F-17 Default deployment is not production-safe (Medium, verified)
- `deploy/compose/docker-compose.yml` publishes `8080:8080` over plain HTTP, sets no `TASKLANE_SECRET_KEY` (F-01), passes the DB
  password and the first administrator's password as environment variables (visible via `docker inspect` and `/proc`), has no
  `read_only`, `cap_drop` or `security_opt`, and the quick start in `docs/CONFIGURATION.md` uses example passwords on the command line.
- `admin_email` and `admin_password` bootstrap remain active if left in the environment; the documentation says to remove them.
- Helm is better (`existingSecret` for DB, admin, secret key, SMTP, S3; `runAsNonRoot`; NetworkPolicy; PDB), but `networkPolicy.ingressFrom`
  empty means "from anywhere" and the chart does not require `secretKey.existingSecret`. Values for `securityContext` were not
  checked line by line (**unverified**).
- systemd unit is well hardened; the `EnvironmentFile` (`/etc/tasklane/tasklane.env`) must be mode 0600 (not enforced).

### F-18 OAuth and MCP details (Medium, mixed)
- The consent screen shows an attacker-chosen `client_name` and only the redirect scheme and host (`api/oauth.go:~108`); custom URI
  schemes are accepted for redirects (`domain/oauth.go:47-62`). Phishing a user into granting scopes to a look-alike client is feasible
  (scopes are limited to read, comment, edit-fields, create, transition, claim).
- A personal (non-agent) token or OAuth grant gets `AllScopes` (`agents.go:~115`). Because `Admin` is computed as
  `u.Admin && inst && u.Kind != "agent"` where `u.Kind` is the account kind, a *human* administrator's token yields an administrator
  actor in MCP despite the comment "never gets administrator rights through a token" (`agents.go:~125`). **unverified**: which MCP
  tools honour `Admin`.
- `DELETE /mcp` removes the session id named in the header without checking it belongs to the caller (`mcp/server.go:156-162`); ids are
  signed (`server.go:~681`), so exploitation needs the id. Rate and write budgets are in-memory per node, so N nodes allow N times the budget.
- Prompt injection: tool output carries issue and comment text; the server tells clients to treat it as data (`server.go:~210`) and marks
  authors and internal flags, but cannot enforce client behaviour; write tools (`create`, `transition`, bulk) are the blast radius.

### F-19 Hosted-edition isolation limits and noisy neighbours (High for a real multi-tenant SaaS, verified from docs)
- SCIM tokens, SSO providers, Git and mail connections are instance-level; SCIM- or SSO-provisioned people join the Default
  organization (`docs/ORGANIZATIONS.md`). A SCIM token can create, take over and deactivate any account, including tenants' (`docs/IDENTITY.md`).
- Per-tenant plans limit seats, projects and storage only. There are no per-tenant API, search, automation or mail rate budgets
  (automation: 300 runs per rule per hour only), so one tenant can degrade others (F-03).
- The operator (instance admin) can read all tenant data and the single audit log; `docs/ORGANIZATIONS.md` states this.

### F-20 Concurrency and resource bugs worth a test (Low to Medium, unverified)
- `rt/hub.go:219` reads `c.subs[key]` without holding `c.mu` while other paths mutate `c.subs` under the lock (`unsubscribe`, re-authentication):
  a data race that can become a fatal `concurrent map read and map write` and crash the node (denial of service by an authenticated user).
- No per-user WebSocket connection cap (`hub.go`), and the single-use ticket travels in the query string (`hub.go:112`), which proxies may log.
- Redis bus messages are unauthenticated JSON (`domain/domain.go:229-232`, `bus_redis.go`); anyone with Redis write access can spoof events or
  force cache drops of the security policy (5-second staleness window documented). Default scheme is plain `redis://`.

### F-21 Link and parser edge cases (Low, mixed)
- `schemeOK` accepts any destination starting with `/`, including protocol-relative `//host` (`domain/sanitize.go:53-56`; the web UI
  linkifies only http/https/mailto so it is not exploitable there, but Jira write-through, exports and email HTML consumers may follow it). **unverified** downstream.
- Development-link URLs (`i.url`) from the Git provider payload are rendered as `href` (`static/app.js:~1517`); no scheme allow-list was
  seen on the server side for provider URLs (`git/git.go:ghPR` copies `html_url`). Exploit needs a provider or webhook-secret holder. **unverified**.
- `richtext` Markdown and ADF parsers have no explicit nesting-depth bound (`internal/richtext/*.go`); the 3 MiB JSON cap and
  `encoding/json` depth limit bound ADF, but deeply nested blockquotes or lists in Markdown are recursive. A fatal Go stack overflow
  cannot be recovered. **unverified**.
- `dashboard` iframe gadgets are host allow-listed and https only (good); the CSP uses `style-src 'unsafe-inline'`.

### F-22 Information disclosure on unauthenticated endpoints (Low, verified)
- `GET /readyz` answers `ready, degraded: the cluster event bus (redis_url) is unreachable ...` without authentication
  (`api/api.go:115-121`, `cmd/tasklane/main.go:~420`), and `/api/*` responses carry `X-Tasklane-Server: <version>` (`api.go:~640`).
  Portal key existence is exposed by `GET /api/v1/portal/{key}` (by design). Inbound SMTP `RCPT` reveals which project keys exist.

### F-23 Account hygiene gaps (Low, verified)
- No self-service password reset exists (smaller attack surface, but operational reliance on administrators and SSO).
- Recovery codes are SHA-256 hashes without salt (`security.go:~385`), adequate only if the codes carry enough entropy
  (`newRecoveryCodes`, `security.go:265`, not reviewed for length: **unverified**).
- Passkey attestation is not verified (documented); SSO sessions are not forced to MFA by design; no SSO enforcement (password login stays open).

## 8. Possible fixes / remediation plan

Priority: P0 fix now (exploitable and severe), P1 next release, P2 planned, P3 opportunistic. Effort: S under 1 day, M 1 to 5 days,
L over 5 days. No P0 item was found.

| Finding | Concrete fix | Priority | Effort | Verification / test idea |
|---|---|---|---|---|
| F-01 secret key in DB | Make `secret_key` mandatory when any secret-bearing feature is configured (refuse to start, or require `--allow-db-secret-key`); keep the DB fallback only for `localhost` dev. Add a `doctor` failure/warning. Add a key id prefix and a `tasklane secrets rotate` command that re-seals all `*_enc` columns; bind ciphertext to `table:id` as AAD. Move `link_secret` to a `DerivedKey("file-link")`. Document KMS/secret-manager injection; set it in Compose and Helm templates by default | P1 | M | Unit test: start without key and with an integration secret fails; migration test re-seals fixtures; test that swapped ciphertexts between rows fail to open; `doctor` test |
| F-02 login throttling | Add a second lock dimension: per source IP (sliding window, e.g. 20 failures/10 min across all emails) and a global per-IP request rate for `/api/v1/login*`; make the per-email lock a progressive delay or require MFA/captcha challenge instead of a hard lock for the admin; keep generic responses. Move counters to Postgres or Redis for cluster correctness. Add exponential delay after N failures. Optionally lower bcrypt cost impact by checking the throttle before hashing (already done for per-email) | P1 | M | Tests: one IP, 100 emails, 1 attempt each is throttled; lockout of a victim does not block their MFA-authenticated session or an allow-listed IP; load test the login path |
| F-03 timeouts and limits | Set `ReadTimeout` (30-60 s with per-route deadlines via `http.ResponseController` for uploads), `IdleTimeout` 120 s, `MaxHeaderBytes` 64 KiB on `http.Server`; wrap every handler's body with `http.MaxBytesReader` by route class (small for JSON, large only for upload/import routes); set `statement_timeout` (for example 15 s, longer for backup/reindex roles) and `idle_in_transaction_session_timeout` on the pool; add a generic token-bucket middleware per IP and per actor with a hosted per-org budget; cap search `limit` and aggregate cost; use a bounded import temp dir | P1 | M | Slowloris test (`ReadTimeout`), oversize raw upload returns 413 before staging, a pathological TQL returns an error within the timeout, k6/hey load profile with limits on |
| F-04 unauthenticated state writes | Rate-limit `/sso/{name}/start`, `/oauth/register`, portal `/code` and `/challenge` per IP; cap `sso_state` rows per IP and purge expired rows opportunistically; make registration quota per source and keep a reserved allowance for known redirect schemes; optional initial-access token for DCR in hosted mode | P2 | S | Test floods stay under the cap and legitimate registration still works after a flood from one IP |
| F-05 admin system actors | Replace `Admin: true` with a dedicated `Kind: system` actor carrying the rule's project and organization and the creator's role (cap at the creator's rights, never above project admin); run each operation through the normal permission checks with `OrgID` set; remove the "oldest admin" fallback, disable the rule when its creator is gone; pin the actor's `Org` to the rule's project's organization; add org checks to user/field resolution in actions; for `create_subtask` use the transitioning person | P1 | M | Cross-tenant test: a tenant project admin's rule cannot assign, notify, or set a user field to a person of another org; rule run as a viewer-created rule cannot do admin-only changes; extend `TestSEC09` sweep to automation |
| F-06 tenant isolation | Add PostgreSQL row-level security as defence in depth: `app.org_id`/`app.user_id` set per transaction (`SET LOCAL`), policies on `project`, `issue`, `comment`, `attachment`, `app_user`; run the application as a non-owner role without `BYPASSRLS`; keep the instance-admin path as an explicit role switch. Longer term: org-level key namespaces and per-org global config (workflows, fields). Decide on issue security levels (SPEC); until then document that sensitive work needs a separate project | P2 (P1 for hosted) | L | Run the SEC09 sweep with RLS enabled and with the app-layer scope temporarily removed to prove RLS catches it; lint that new tables declare a policy |
| F-07 login CSRF, SSO state binding | Require `X-Requested-With` (or `Content-Type: application/json` enforced in `decode`) on login routes; set an HttpOnly, SameSite=Lax, short-lived `__Host-sso_state` cookie holding a nonce stored in `sso_state` and compare it in the callback and SAML ACS (SAML POST from the IdP needs SameSite=None or an extra GET hop) | P2 | S | Test: callback with a valid code but without the cookie fails; cross-site form POST to `/login` is refused |
| F-08 inbound-mail replay | Require signed `Subject`, `To` or `Date` and a `Message-ID` in `h=` for own-DKIM proof; reject signatures older than N days (use `t=`/`Date`, default 7 days) when no `x=`; reject RSA below 2048 bits (config for legacy); consult DMARC alignment and SPF for non-trusted sources; keep recipient-bound reply tokens as the preferred target path; log "proof strength" in the quarantine reason | P2 | M | Replay test: a stored signed message with modified unsigned headers is quarantined; message older than the window is quarantined; unit tests in `mail/dkim_test.go` |
| F-09 client IP consistency | Use `Svc.ClientIP` in `audit.go:94`, in the Git/Jira webhook CIDR check, and in the SMTP `MaxPerIP` accounting where a proxy protocol is used; add `trusted_proxies` and `base_url` https checks to `doctor` (warn when unset and `X-Forwarded-For` is seen) | P2 | S | Test behind a trusted proxy: audit shows the visitor address, allow-list works with the real source |
| F-10 malware scan | Integrate an external scanner behind the existing `Scanner` interface (ClamAV `clamd` over `INSTREAM` with size limits, or an ICAP/HTTP scanner), with `scan_state='pending'` until verdict (the state machine and 423 already exist), fail-closed or quarantine on scanner outage per setting; correct the wording in `docs/S3.md`/UI; add nested archive and gzip inspection limits | P1 | M | Integration test with clamd container and EICAR; outage test; downloads blocked while pending |
| F-11 backups | Offer `tasklane backup --encrypt` (age/AES-GCM with a key file) and sign the manifest (HMAC under a derived key or Ed25519); create directories 0700 and files 0600; refuse to restore a manifest whose table names are not in the compiled schema; document storing the secret key apart from backups; bound zip member sizes on import (`io.LimitReader` by manifest bytes) | P2 | M | Tamper test (manifest table renamed, file changed) fails; restore of an encrypted backup works; permission test |
| F-12 Android | `network_security_config`: `cleartextTrafficPermitted="false"` globally with a per-domain opt-in (or a runtime warning and an in-app "I understand" for LAN HTTP); store the token with the Android Keystore (`EncryptedSharedPreferences` or a Keystore-wrapped key); require biometric/screen-lock for app open (optional); server side: issue a scoped, revocable, device-named mobile token (OAuth device or `POST /api/v1/me/tokens` with `read,comment` scopes) instead of a 14-day session; enable R8 minification; consider certificate or SPKI pinning option; publish a signed checksum (Sigstore or the release Ed25519 key) and the APK signing certificate fingerprint | P2 | M | Instrumented test fails on `http://` unless opted in; `adb run-as` cannot read plaintext token; token revoked from Settings logs the device out |
| F-13 sessions | Add idle timeout (for example 12 h, configurable) and rotate the session id on MFA or password change; default `session_ttl` to a shorter value for admins; set the `__Host-` cookie prefix when `Secure`; add HSTS (`max-age`, `includeSubDomains` opt-in) when `base_url` is https; make `doctor` fail when `base_url` is http and `X-Forwarded-Proto: https` is seen; scope API tokens (read, write) with optional CIDR; raise minimum password length to 12 for admins, add a common-password list | P2 | M | Tests for idle expiry and cookie attributes; token scope tests on REST |
| F-14 egress | Extend `Policy.Check` with `100.64.0.0/10`, `198.18.0.0/15`, `192.0.0.0/24`, `168.63.129.16`, NAT64 and 6to4 unwrapping (check the embedded IPv4); make `*_allow_private` an allow-list of CIDRs per feature instead of a boolean; add an egress NetworkPolicy template (deny all, allow DNS, DB, Redis, S3, SMTP, listed hosts); offer an "outbound allow-list of webhook hosts" setting for hosted mode | P2 | S | Unit tests in `egress_test.go` per range; Helm template test |
| F-15 detection | Audit failed logins, lockouts, 401/403 on admin routes, MCP denials and policy refusals (rate-limited), with `ip` from `ClientIP`; add org id to audit entries and a tenant-scoped view; emit structured JSON security logs (request id, actor, route, status, latency) and an optional audit export to stdout/syslog/S3 object-lock on a schedule; fail closed (or alarm) when an audit write fails for admin routes; keep `login_attempt` aggregates longer than 24 h for analysis | P2 | M | Tests: failed login produces an event; killed audit table makes admin mutation fail; log schema test |
| F-16 CI and supply chain | Add a CI workflow: `go vet`, `go test ./...` with a Postgres service, `govulncheck ./...`, `staticcheck`/`gosec`, `gitleaks` with an allow-list for `internal/saml/testdata`, `npm audit` for `e2e`, Gradle dependency check for `android`, `trivy` on the image, SBOM generation; enable Dependabot/Renovate (gomod, docker, npm, gradle, actions); pin base images by digest; move `dkimpy`/`dnspython` to a documented `testdata` fetch step with hashes or remove; pin GitHub Actions to commit SHAs; require branch protection and signed tags | P1 | M | CI run on a PR; seeded vulnerable dependency or fake key is caught |
| F-17 deployment | Compose: use Docker secrets/`*_FILE` for DB and admin passwords, set `TASKLANE_SECRET_KEY_FILE`, bind to `127.0.0.1` or add a Caddy/nginx TLS service, add `read_only: true`, `cap_drop: [ALL]`, `security_opt: [no-new-privileges]`, a tmpfs `/tmp`; make Helm fail when `secretKey.existingSecret` is empty (schema `required`), default `networkPolicy` to a restrictive `ingressFrom`; systemd: install the env file with `0600 root:tasklane`; document removing `admin_*` after first start and add an automatic "bootstrap done" marker | P2 | S | `helm lint` + schema test; `docker compose config` check; `doctor` warnings |
| F-18 OAuth / MCP | Show the client's redirect host and registration age on the consent page, mark clients "unverified", cap scopes for dynamic clients by default, let admins pre-approve clients; require admin approval for registrations in hosted mode; make administrator personal tokens non-admin in MCP (set `Admin=false` for any token path unless a scope `admin` exists) and add a test; check `Mcp-Session-Id` ownership on DELETE; move rate/write buckets to Postgres or Redis for cluster-wide budgets; add per-tool allow-lists per token | P2 | M | MCP test: an admin's personal token cannot call admin-gated tools; session DELETE by another agent is 404; cluster budget test |
| F-19 hosted limits | Per-org rate budgets (API, search, mail, automation), per-org SCIM tokens and SSO providers with org binding, org-level groups and audit views, make the operator's access to tenant data explicit and logged (break-glass with reason and tenant notice); consider per-tenant database or schema for high-assurance customers | P2 (P1 before hosted launch) | L | Multi-tenant load test showing one tenant cannot starve another; SCIM token of org A cannot touch org B |
| F-20 concurrency | Protect `c.subs` reads with `c.mu` (`rt/hub.go:219`) and run `go test -race` over `rt` and `domain` in CI; add a per-user connection cap; move the ticket into the `Sec-WebSocket-Protocol` header; sign Redis bus messages (HMAC with a derived key) and prefer `rediss://` with AUTH in docs and `doctor` | P2 | S | Race test with concurrent sub/unsub; reject unsigned bus message |
| F-21 parsers and links | Reject `//` prefixes in `schemeOK`; validate provider URLs server-side (https/http only, host equals the connection base URL) before storing or rendering; add a nesting depth cap (for example 64) and a node-count cap to Markdown and ADF parsers and fuzz them (`go test -fuzz`); consider a CSP with hashed styles to drop `'unsafe-inline'` | P3 | S | Fuzz targets, unit tests for `//evil` and `javascript:` in provider payloads |
| F-22 info disclosure | Return a bare `ready` on `/readyz` to anonymous callers (details only for authenticated admins or from loopback/trusted proxy); drop `X-Tasklane-Server` from anonymous responses or round it to major version; make the SMTP receiver answer a uniform `250` for unknown project keys and rely on dropping later | P3 | S | Response tests |
| F-23 account hygiene | Add optional organization "enforce SSO" and "block password login for domain"; verify recovery codes carry at least 64 bits and store them with a keyed hash (HMAC with a derived key); consider attestation for admin passkeys; add a password-reset flow only if needed, with single-use hashed tokens, rate limits and audit | P3 | M | Unit tests for code entropy and keyed hash migration |

### Suggested sequencing

1. **Week 1 (P1 cheap wins):** F-03 server timeouts and body caps, F-01 `doctor` warning and Compose/Helm default secret key,
   F-16 CI with `govulncheck`, `go test -race`, `gitleaks`.
2. **Next:** F-02 per-IP throttling, F-10 real scanner, F-05 system-actor rework (with the cross-tenant automation test).
3. **Then:** F-07/F-09/F-13 browser and proxy hardening, F-08 DKIM tightening, F-12 Android token and cleartext.
4. **Before any real multi-tenant offering:** F-06 row-level security, F-19 per-org budgets and SCIM/SSO binding, F-15 tenant audit.

## 9. Sensitive files (path and kind only; no values were read or copied)

| Path | Kind |
|---|---|
| `android/local.properties` | local Android SDK configuration (git-ignored); may reference local paths only |
| `~/Android/tasklane-release.properties` (outside the repo, referenced by `android/app/build.gradle.kts:29-36`, or `$TASKLANE_SIGNING`) | Android release signing properties (keystore path, passwords, alias) |
| `deploy/tasklane.conf.example` | configuration example (no secrets) |
| `/etc/tasklane/tasklane.env`, `/etc/tasklane/tasklane.conf` (referenced by `deploy/systemd/tasklane.service`) | operator environment and config files; will hold database URL, `secret_key`, SMTP, S3 and OpenSearch credentials |
| `deploy/helm/tasklane/values.yaml` keys `database.existingSecret`, `admin.existingSecret`, `secretKey.existingSecret`, S3 and SMTP secret refs | Kubernetes Secret references (names only) |
| `internal/saml/testdata/idp.key`, `internal/saml/testdata/other.key` | test private keys (PEM), committed fixtures |
| `internal/saml/testdata/idp.pem`, `internal/saml/testdata/other.pem` | test certificates, committed fixtures |
| `internal/domain/testdata/backups/v1/backup/data/*.copy` (for example `scim_token.copy`, `webauthn_credential.copy`) | database dump fixtures of credential tables |
| `data/blobs/` and `data/` | local attachment store (git-ignored) |
| `interop-logs/compose.log`, `e2e/test-results/` | test logs (git-ignored); may contain tokens in failure output |
| Database table `setting` rows `secret_key`, `link_secret` | generated instance secrets stored in PostgreSQL (see F-01) |
| `dkimpy-1.1.8.tar.gz`, `dnspython-2.8.0-py3-none-any.whl` | vendored third-party archives in the repo root |

No `.env`, keystore, `.jks`, `.pem` private key outside testdata, or token file is tracked in git (checked with `git ls-files`).

## 10. Gaps in this review and next steps

- Static read only: nothing was executed, so races (F-20), DKIM replay (F-08), the automation cross-tenant question (F-05) and
  the MCP admin-token question (F-18) need tests before being treated as confirmed.
- Not read line by line: `internal/saml` (xmldsig canonicalization), `internal/webauthn`, `internal/ldap` (BER parser), `internal/qr`,
  `internal/blob/s3sign.go`, `domain/lifecycle*.go`, `domain/sla.go`, `domain/boards.go`, `domain/dashboards.go` (beyond iframe),
  `search/os_*.go`, the Helm `values.yaml` security context block, and `scripts/*`. These are the best candidates for a second
  pass and for fuzzing (SAML XML, LDAP BER, ADF/Markdown, MIME, zip).
- Suggested dynamic work: authenticated scan with ZAP against a throwaway stack, `go test -race ./...`, `govulncheck`, a Playwright
  XSS corpus through comments, descriptions and custom fields, an SSRF test matrix against `egress`, and a multi-tenant sweep with
  RLS removed to measure how much isolation depends on application code alone.
- Keep this file in step with `docs/PERMISSIONS.md`, `docs/IDENTITY.md` and `docs/ORGANIZATIONS.md`; update the inventory when a
  route is added (the authorization matrix and audit-coverage tests already force a decision for each new route).
