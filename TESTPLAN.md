# Tasklane - Test Plan

Version 0.1 (draft). Companion to [SPEC.md](SPEC.md) (what is built) and [DESIGN.md](DESIGN.md) (how it is built).
This plan says what is tested, how, with which data, and what must pass before a milestone ships.

## 1. Goals and priorities

Risk-ordered. Effort follows the order, not the size of the feature.

1. **Concurrent editing stays within the reliability rates of SPEC 21** (lost acknowledged save, undetected overwrite and silent
   content change at most 1 per 10^8 to 10^9 saves; lost draft at most 1 per 10^6 editing sessions; DESIGN 12.9). Conflict
   handling, drafts, durability. A test that says "no lost save" means zero observed in a campaign of the size in LIVE-09.
2. **Latency targets hold** (SPEC 21): usable content under 300 ms p95 on-prem; editing targets in DESIGN 12.10.
3. **No data loss or leak across permissions**: security levels, field restrictions, channel authorization.
4. **Migration is trustworthy**: Jira and third-party import/export round-trips (SPEC 18.2), Jira mirror mode (SPEC 18.1).
5. **Core tracker behavior**: issues, workflows, boards, sprints, custom fields, search.
6. **Experience quality**: dark/light themes, accessibility, responsive layouts, offline.

Out of scope for v1 testing: third-party plugin runtimes (none in v1), customer-portal SLA edge cases beyond M5 acceptance.

## 2. Test levels and tooling

Tooling is a recommendation; the contract is the level and the gate, not the product name.

| Level | Scope | Tooling (suggested) | Runs |
|---|---|---|---|
| Unit | Pure Go and TS logic: TQL parser, workflow engine, rank, permission evaluation, converters | `go test`, Vitest | Every commit |
| Property / fuzz | Three-way merge properties, TQL parser, ADF and Markdown converters, import mappers | `testing/quick`/`rapid`, `go test -fuzz`, fast-check | Every commit (short), nightly (long) |
| Integration | Handlers + real PostgreSQL (+ Redis for multi-node tests), outbox workers, WS gateway | testcontainers, `httptest` | Every commit |
| Real-service interoperability (smoke) | A few end-to-end scenarios against a **real Redis or Valkey and a real OpenSearch** started by Docker Compose (section 3.3): the stack comes up, search runs on OpenSearch, two app nodes share events and presence through Redis. Basic interoperability only, no edge cases | Compose `interop` profile, Go integration tests with the real binary, one Playwright scenario | PR, nightly |
| Contract / API compatibility | `/rest/api/3` subset vs recorded Jira responses; webhook payloads; exchange formats | Golden files, schema validation | Every commit |
| End to end (UI) | Real browser against a real server, multi-user scenarios | Playwright, multiple browser contexts | PR, nightly |
| Visual regression | Components and screens in dark, light, compact, comfortable | Playwright screenshots, per-theme baselines | PR |
| Accessibility | WCAG 2.2 AA automated + manual passes | axe in CI, manual screen reader scripts | PR (auto), release (manual) |
| Performance / load | Latency budgets, throughput, 5M-issue dataset, editor fan-out | k6 (HTTP/WS), custom WS load client, browser traces | Nightly, release |
| Resilience / chaos | Node kill, partitions, restarts, slow disks, dependency loss | Fault injection (toxiproxy-style), scripted | Nightly, release |
| Security | AuthZ matrix, OWASP ASVS L2 checks, dependency and secret scanning, WS abuse | Scanners, scripted abuse tests (external pentest postponed, see SEC-11) | PR (scanners), release |
| Upgrade / DR | Zero-downtime migrations, backup/restore, RPO/RTO | Scripted environments | Release |
| Jira mirror | Sync, permission mapping and read-only enforcement against `jira-fake` (recorded Jira behavior with fault injection) | Recorded fixtures, `jira-fake` | Every PR and nightly |
| Real Jira verification | The same mirror, import and export behavior against a **real Jira Cloud test site**, plus a check that the fake still matches it (section 5.17) | Dedicated sandbox site | **Occasionally, manually triggered, before a major release** (not per PR, not nightly) |

**Real by default, mock only where real is impossible or unsafe.** A dependency is replaced by a mock only when (a) it
cannot be run or reached in CI (paid or per-message provider APIs, a mailbox on the public internet), (b) the test needs
a fault the real system will not produce on demand, or (c) the case is a pure unit of our own code. Every mock is held
to the real system by a contract check (recorded exchanges replayed against the real thing nightly), so a mock cannot
drift unnoticed. Section 3.2 lists the choice per dependency.

## 3. Environments and data

| Environment | Description |
|---|---|
| `dev` | Docker Compose `single` profile on every developer machine (section 3.1) |
| `ci` | Ephemeral Docker Compose project per run: `single`, and `multi` or `ha` with the `chaos`, fake and runner overlays (section 3.1) |
| `perf` | Fixed hardware profile (documented cores, RAM, disk) running the Compose `single` and `multi` profiles with `obs`, so numbers are comparable run to run |
| `jira-sandbox` | A Jira Cloud site owned by the project; test accounts only. Used **only** by the real Jira verification run (section 5.17), which happens once in a while before a major release. Credentials from the CI vault, never available to PR or nightly jobs |

**Datasets** (generated deterministically from a seed, checked into the generator, not the data):

| Name | Content | Used for |
|---|---|---|
| `tiny` | 1 project, 50 issues, 5 users | Unit and integration |
| `team` | 5 projects, 20k issues, 100 users, comments, attachments metadata | E2E, visual, accessibility |
| `search100k` | Generated corpus for PostgreSQL search: 100,000 issues (and the 10,000 to 250,000 sizes), realistic text, comments, several languages, Zipf terms | PostgreSQL search design and PERF-16..17 |
| `org` | 50 projects, 1M issues, 5k users, 10 custom field contexts | Perf (typical enterprise) |
| `scale` | 5M issues, 20M comments, 20k users | Search and list latency targets |
| `merge-corpus` | Base, theirs and mine document triples with expected merges and conflict hunks (paragraphs, lists, tables, marks, mentions, Unicode, RTL) | Merge and conflict dialog tests |
| `jira-export` | Anonymized Jira CSV, backup XML and REST fixtures with awkward cases (ADF edge nodes, repeated columns, deleted users) | Import tests |

Rules: no real customer data; test accounts only on external services; secrets come from the CI vault.

### 3.1 Test deployment with Docker Compose

Every environment above except the real external sandboxes is a **Docker Compose project**. The base files are the
shipped `deploy/compose/` files; test overlays add fault injection, fakes and runners. One command brings a profile up,
waits for health, seeds data, runs the suite and tears down (`docker compose -p tl-$RUN_ID ... up --wait`, then
`down -v`); a unique project name and random published ports let CI run many projects in parallel.

**Layout** (the target; today only `docker-compose.yml`, `docker-compose.test.yml` and the new, self-contained
`docker-compose.interop.yml` of section 3.3 exist or are required)

```
deploy/compose/
  compose.base.yml        app image (the Go binary), postgres, shared networks, healthchecks
  compose.single.yml      one app node, local filesystem blob volume
  compose.multi.yml       3 app nodes, load balancer (WebSocket aware), redis, minio (S3), shared blob volume option
  compose.ha.yml          postgres primary + synchronous standby, redis replica + sentinel
  compose.test.yml        test overlay: toxiproxy, mailpit, fakes, runners, fake clock, test-only endpoints
  profiles.env            image digests pinned, seeds, resource limits per profile
```

**Services and profiles** (Compose `profiles` select what starts)

| Profile | Services | Used by |
|---|---|---|
| `single` | `app`, `postgres` | Unit-integration, E2E smoke, latency reference (PERF), visual, a11y |
| `multi` | `app1..3`, `lb`, `postgres`, `redis`, `minio` + bucket init | DUR-06, PRES-07, LOAD-04, BLOB (S3), cross-node PERF-12 |
| `ha` | `postgres-primary`, `postgres-standby` (synchronous), `redis` + replica + sentinel | CHAOS-01..05, DUR-03, RPO tests, backup and restore |
| `chaos` | `toxiproxy` between app and postgres, redis, minio, and between runner and `lb`; `docker compose kill/pause/stop`; size-limited `tmpfs` volume for disk-full | CHAOS, DUR, LIVE-09 |
| `sso` | `keycloak` (OIDC and SAML), SCIM test client | BLOB-18 | partial (project quota and report; limits by class and concurrent uploads were already there; no bandwidth limits) | `TestBLOB18_StorageQuotaIsEnforcedWithAClearMessageAndFreedByDeleting` |
| BLOB-14 | done (partial: a command, no progress screen) | `TestBLOB14_MigrateFSToS3AndBackWithVerifiedChecksums` (27 objects incl. multipart and empty, back again), `_MigrationIsResumableAndNeverCopiesADamagedObject`, `_FallbackStoreServesBothWhileTheMoveRuns`, `_ConfigurationOfTheFallback`, domain `TestBLOB14_AttachmentsSurviveTheMoveFromFilesystemToS3` |
| BLOB-15 | done (partial: no promotion, no per-project rules, latency against a mock) | blob: `TestBLOB15_NewObjectsAreHotAndMoveToColdWithTransparentReads` (incl. a multipart cold upload and ranged reads), `_AMoveThatIsInterruptedOrWrongNeverLosesTheObject` (cold tier failing, interrupted, a differing cold copy), `_DeduplicationRemovalAndWritesAcrossTiers`, `_EncryptedObjectsMoveAsCiphertextAndRewrapStaysInTheirTier`, `_ReadersNeverMissAnObjectWhileItMoves`, `_ColdReadLatencyIsMeasured` (1 MiB: hot about 0.1 ms, cold about 0.5 ms against a mock); domain: `TestBLOB15_RulesMoveByClassAgeAndSizeAndReadsStayTransparent`, `_UnreferencedAndUnscannedObjectsAreLeftAlone`, `_RulesAreValidatedAdministeredAndAudited`, `_AColdTierThatFailsLeavesObjectsHotAndTheNextRunFinishes`, `_TwoNodesMoveEachObjectOnce`, `_GarbageCollectionRemovesColdObjectsToo`, `_EncryptedObjectsTierWithoutAKey`; the real binary `TestBLOB15_CLITierMovesObjectsByRuleAndVerifySeesBothTiers` |
| BLOB-16 | done on the filesystem and over S3 (partial: keys from configuration, no KMS or per-project keys, S3 transport security not tested against a real service) | `TestBLOB16_RoundTripAtEveryChunkBoundaryAndNothingReadableAtRest`, `_AlteredCutOffSwappedOrForeignObjectsAreRefused`, `_RotationRewritesTheHeaderOnlyAndTheOldKeyCanGo`, `_ObjectsStoredBeforeEncryptionAreServedAndThenEncrypted`, `_DeduplicationAndThumbnailsGoThroughEncryption`, `_ConfigurationOfTheKeys`; the real binary: `TestBLOB14_16_CLIMigrateEncryptRewrapVerify` (migrate into an encrypted store, verify, fsck, rotate, retire the old key), over S3 `TestBLOB16_EncryptionOverS3` and `TestBLOB16_RotationOverS3ReplacesInOneStep` |
| PERM (SPEC 17.2) | done for groups, five permissions and custom roles (partial: no security levels) | `TestPERM01_GroupsGrantProjectRolesAndTheHighestRoleWins`, `_GroupAdministrationByProjectAdministratorsOnly`, `_PermissionSchemeChangesWhoMayDoWhat`; custom roles `TestPERM02_*` (who may define them, validation, reserved and duplicate names, another organization's roles invisible and unusable, deletion refused while in use, a Reporter/Triager/Talker/Mover each able to do exactly what they name, a plain viewer for contrast, the scheme still limiting members while a grant holds, grants adding up across a person's own and group roles, edits and a raised base applying at once, audit) and e2e `PERM-02` (the Roles tab and a real person using a role; mutation-tested) |
| SEC-10 | done (partial: no screen, free text limits) | `TestSEC10_ExportCoversEverythingHeldAboutAPerson`, `_ErasingAPersonLeavesNoTraceAndKeepsTheWork` (every table searched; the audit chain still verifies), `_DeleteContentRemovesWhatThePersonWrote`, `_LegalHoldAndGuardsStopAnErase`, `_CleanExpiredRemovesWhatOutlivedItsPurpose` |
| MCP-04 | done (partial: public clients only) | `TestMCP04_AuthorizationCodeWithPKCEGivesAScopedTokenForThePerson`, `_CodeExchangeChecksEverythingItWasIssuedFor`, `_AuthorizationRequestsAreValidatedAndRedirectsAreExact`, `_RefreshTokensRotateAndReuseRevokesTheGrant`, `_RevocationEndsAccessAtOnce`, `_AClientNeverExceedsThePerson`, `_HTTPDiscoveryAndTokenEndpoint`, e2e `account.spec.js` (the whole flow in a browser) |
| MCP-12 | done (partial: no forced race test) | `TestMCP12_IdempotencyKeysMakeRetriesSafe`, `_BulkUpdateDryRunIsExactAndChangesNothing`, `_BulkReportsEachIssueAndRespectsLimitsAndGates`, `_BulkTransitionHonoursTheWorkflowAndHumanGates` |
| MCP-20 | partial (chain and per-issue limit; no real webhook cycle) | `TestCORE10_AutomationLoopsAreCut`, `TestMCP20_ACycleThroughAnAgentIsStoppedByThePerIssueLimit` |
| SEC-04 |
| `mail` | `mailpit` (SMTP **mock**: sink with API, optional auth and TLS, scripted failures), `greenmail` (real IMAP and SMTP server for inbound mailboxes), provider-webhook fake, raw-message injector | Notifications, MAIL cases |
| `upgrade` | Previous and next release images side by side, scripted rolling replace and offline N-2 upgrade | UPG cases |
| `av` | `clamav` | BLOB-08 |
| `scm-fake` | `scm-fake`: replays recorded GitHub and GitLab webhooks and serves a subset of each REST and GraphQL API | GIT tests on every PR |
| `scm-gitlab` | `gitlab-ce` (self-managed, seeded group and project; needs about 4 GB RAM and a few minutes to start) | GIT tests, nightly |
| `jira-fake` | `jira-fake`: recorded Jira REST, webhook and permission fixtures; scripted 429 with `Retry-After`, 5xx, latency, resets, truncated bodies, and dropped, delayed, duplicated and reordered webhooks; exotic permission schemes and grant types, deleted and closed accounts, a 10,000-issue project | MIR, IMP, EXP on every PR and nightly, local dev |
| `search` | `opensearch` (one node in CI, three nodes for performance runs) | TQL-08..20, PERF-05 |
| `interop` | `app1`, `app2`, `postgres`, `redis` (or `valkey`), `opensearch` (one node, security off in CI), all pinned by digest, healthchecks, no fault injection; defined in the one file `deploy/compose/docker-compose.interop.yml` and started by `make interop` | INTEROP-01..06 (section 3.3) |
| `egress` | `evil-sink` on a private range plus an internal-only network | SSRF tests |
| `obs` | `prometheus`, `otel-collector`, `grafana` | PERF and LOAD runs |
| `runners` | `playwright` (official image), `k6`, the simulated-user fleet for LIVE-09 | All E2E, load and campaign runs |

Real external systems stay outside Compose and are used with a request budget: AWS S3 (nightly) and GitHub (a
dedicated test org and repositories, nightly). There is no self-hosted GitHub in the free tier, so GitHub in CI is the
`scm-fake`, kept honest by the nightly run against the real org. The Jira Cloud sandbox is used far less often: only in
the real Jira verification before a major release (section 5.17).

**Rules**

- **Pinned and reproducible:** every image is pinned by digest; seeds and the dataset generator version are fixed;
  resource limits (`cpus`, `mem_limit`) come from the profile so numbers are comparable.
- **Readiness, not sleeps:** every service has a healthcheck, `depends_on` uses `condition: service_healthy`, and the
  runners wait for `/readyz` of every app node, Postgres and Redis.
- **Time is injectable:** a test overlay sets a fake clock for the app (build-tagged test endpoints, absent in release
  images) so sprint rollover, retention, lease and pause timeouts are tested without sleeping.
- **Upgrade tests:** a project starts `tasklane:$PREV`, loads data, then rolls the services to `tasklane:$NEXT`
  (single and multi), and runs the compatibility and migration checks.
- **Performance runs:** a dedicated host with fixed cores, the `obs` profile, runners on the same bridge network. The
  container networking overhead is measured once and recorded in every report; absolute PERF numbers are labeled with
  the profile. A bare-metal confirmation run happens before each release.
- **Isolation and safety:** test secrets are fixtures only; app containers have no internet egress except to named fakes
  (internal network), which is also what the SSRF tests rely on.
- **Artifacts on failure:** `docker compose logs`, Postgres dump, and the Playwright
  traces and videos are uploaded; the project is torn down either way.
- **Local use:** developers run the same profiles (`make env-up PROFILE=multi`), so a CI failure reproduces locally.

### 3.2 Real versus mock, per dependency

| Dependency | Commit (`go test`) | PR (Compose) | Nightly / release | Why |
|---|---|---|---|---|
| PostgreSQL | real (testcontainers) | real | real, plus `ha` | Cheap to run; mocking it hides the bugs that matter (locks, outbox, GIN plans) |
| Redis or Valkey | in-process `redismock` is allowed for fast tests of the cluster code; **at least the INTEROP scenarios run against the real server** | **real** (`interop` profile, two app nodes) | real, plus `ha` | A mock written by the same author as the client shares its misreadings of the protocol; the real server is cheap, so the basic cross-node behavior is always proved against it (section 3.3) |
| **Outbound SMTP** | **mock:** in-process Go SMTP server in the test (scripted greeting, EHLO extensions, AUTH, STARTTLS with a throwaway CA, 4xx and 5xx at each stage, drop after DATA, slow reads) | **mock:** `mailpit` (inspect rendered mail by API, no delivery) | mock, plus a real relay smoke: one message through a real provider to a seeded test mailbox, verified by IMAP | No test may send real mail; the protocol and failure paths are what we test, and only a mock can fail on demand. The real relay run proves deliverability and DKIM (MAIL-20) once a night |
| Inbound mail (IMAP, SMTP listener) | in-process fake for parser units | **real server:** `greenmail` | real | A real IMAP server behaves like one (UIDVALIDITY, IDLE); the message content comes from the `mail-replies` and `mail-hostile` corpora |
| Mail provider APIs (SES, SendGrid, Mailgun, Postmark) | mock (`httptest`, payloads and signature schemes from each provider's documentation) | mock: provider-webhook fake | real sandbox or test-token send where the provider offers one free (Postmark test token, Mailgun sandbox, SES sandbox to verified addresses); recorded exchanges replayed to detect drift | Per-message cost and verified-sender setup; contract check keeps the mock honest |
| **Jira Cloud** (mirror, import, export) | recorded fixtures for the ADF reader, mappers and permission evaluation | `jira-fake` | `jira-fake`; the **real sandbox only in the pre-major-release verification** (5.17) | Real Jira is slow, rate limited and shared state, so day-to-day runs use the fake; because the mirror is correct only if it matches what Jira really returns, the real run before a major release is a required check and refreshes the fake's recordings |
| GitHub | recorded fixtures | `scm-fake` | real test org (nightly, budgeted); fake replayed against it for drift | No self-hosted GitHub; rate limits |
| GitLab | recorded fixtures | `scm-fake` | real `gitlab-ce` container | Free to self-host, heavy to start |
| S3 | `fs` backend and in-process fake | real MinIO | real MinIO and AWS S3 (budgeted) | MinIO is real S3 semantics for free; AWS differences caught nightly |
| ClamAV | built-in EICAR check | real `clamav` | real | Container is free |
| OIDC and SAML identity provider | in-process fake IdP for token units | real `keycloak` | real | Real flows, redirects and signatures |
| OpenSearch | in-process `osmock` is allowed for translator, indexer and fallback units; **at least the INTEROP scenarios run against the real cluster** | **real** (`interop` profile, one node; engine enabled) | real, 1 and 3 nodes | Needed for TQL-05 and fan-out; the mock must never be the only evidence that a query or a mapping is accepted by OpenSearch (section 3.3) |
| Webhook targets (outbound webhooks, automation) | `httptest` receiver | `webhook-sink` container recording requests | same | Receiver behavior is ours to script |
| DNS (SPF, DKIM, DMARC checks) | in-process resolver stub | a test DNS zone container | real lookups against test domains we own | MAIL-20 |
| Outbound egress to unknown hosts | blocked | blocked (internal network; named exceptions only) | blocked, `egress` profile | SSRF suite SEC-12, no accidental calls |

Rules for the mocks:

- **One mock per system, shared** by every test that needs it (`internal/testutil/smtpmock`, `jira-fake`, `scm-fake`),
  never an ad hoc stub per test file; a mock that fails a scripted case is a test failure, not a skip.
- **A mock records what it received** (raw SMTP transcript and message, request log) and tests assert on that record, so
  "no message sent" and "exactly one message" are checkable.
- **Contract check:** exchanges recorded from the real system (GitHub, provider webhooks) are replayed against the
  mock in CI and against the real system nightly; a difference fails the nightly and blocks the mock update. For Jira
  the same check runs in the real Jira verification (5.17), not nightly.
- **Real-system tests are budgeted and polite:** a request budget per run, backoff on `429`, no more than one run per
  sandbox at a time, unique names per run, teardown in all cases, and a janitor for leftovers. A run that exhausts the
  budget reports "budget exhausted" and fails, never silently passes.
- **Credentials** for real services are test accounts only, held in the CI vault, absent from pull requests from forks.

### 3.3 Real-service interoperability smoke (INTEROP)

**Purpose.** Everything else in this plan may run against in-process doubles. These few end-to-end scenarios prove that
the shipped binary works with the **real Redis (or Valkey) and the real OpenSearch** it is documented to work with.
They cover **basic interoperability only**: the happy path of each integration. Edge cases, failures, scale and upgrades
stay with the existing cases (TQL-16, CHAOS-02, DUR-06, PERF-05) and are not part of this section for now.

**Environment.** The Compose `interop` profile (section 3.1), started with `make interop`. For now it is the **only new
Compose layout**: one self-contained file, `deploy/compose/docker-compose.interop.yml`, next to the existing
`docker-compose.yml`. The five-file layout above (`compose.base`, `single`, `multi`, `ha`, `test`) is the target for
later and is not built as part of this work. The file holds two app nodes, PostgreSQL,
Redis or Valkey 7 or newer, and one OpenSearch node (the supported version range's lowest and highest minor are run
nightly, one of them per PR). The app is started with `search_engine=opensearch`, the OpenSearch URL, and the Redis URL
set, so the test runs the real code path, not a fallback. Images are pinned by digest; readiness uses healthchecks and
`/readyz`. No fault injection, no toxiproxy, no fake clock.

| Id | Scenario (real services only) | Pass when |
|---|---|---|
| INTEROP-01 | Bring the profile up; `tasklane doctor` and `/readyz` on both nodes | Both nodes ready; `doctor` reports Redis and OpenSearch reachable with their versions; the OpenSearch index and alias exist with the expected mapping |
| INTEROP-02 | Through the API create a project and a few dozen issues with text, comments and custom fields; wait for the indexer to catch up; run about ten ordinary TQL and text searches (field equals, text match, `ORDER BY`, a permission-restricted user) | Every query returns the same result set as the PostgreSQL engine on the same data; the response states OpenSearch served it (no fallback); the restricted user sees only what they may see |
| INTEROP-03 | Update, transition and delete issues and comments; wait | The index follows each change (new text is found, deleted issues disappear, a moved issue is found under its new project); `tasklane reindex` rebuilds the index and the same queries still pass |
| INTEROP-04 | Two users on **different nodes** open the same issue; one edits and saves, then the other saves from the old version | Presence of each user appears on the other's node; the save event reaches the other node's socket; the second save gets the normal `TextConflict`, not data loss |
| INTEROP-05 | Pause a project (the barrier of UPG-03 and PAUSE) from node 1 while a writer on node 2 saves | Node 2 acknowledges and then refuses writes with the pause response; resume clears it on both nodes |
| INTEROP-06 | One Playwright scenario, two browser contexts on different nodes: sign in, open an issue, see the other user's presence, type in the search box | The presence indicator shows, and the search results come from OpenSearch (checked by the response header or the `doctor`-visible engine in use) |

**Status (M5.25).** INTEROP-01..06 are built and pass: `make interop` (`scripts/interop.sh`, `deploy/compose/docker-compose.interop.yml`) runs `internal/interop` (build tag `interop`, Go, real binary in two containers) and `e2e/interop` (Playwright, `playwright.interop.config.js`). Run once here: the stack was up in 36 s and the six scenarios took 12 s. Checked not to be vacuous: with `redis_url` blank INTEROP-01 and INTEROP-04 fail, with the PostgreSQL engine INTEROP-01 and INTEROP-02 fail. The compose file pins the images by digest (PostgreSQL 16, Redis 7, OpenSearch 2.17.1); the matrix of M5.26 (`make interop-matrix`, `scripts/interop-matrix.sh`) runs the same scenarios against Redis 7 with OpenSearch 2.17.1, Valkey 8 with OpenSearch 2.11.1 and Valkey 7.2 with OpenSearch 3.0.0, all passing; `make interop` runs the first line only.

**Rules**

- **Real services must be real.** The suite fails, not skips, if Redis or OpenSearch is unreachable; a test that
  silently falls back to PostgreSQL or to local-only presence is a failure (assert the engine and the bus in use).
- **Basic only.** A scenario that needs a killed connection, a red cluster, a slow node, TLS or credential rotation, or
  a large dataset belongs to the chaos and search cases, not here. New INTEROP cases need a reason that a mock cannot
  give the evidence.
- **Mocks stay, and are checked.** Where the same scenario also runs against `redismock` and `osmock`, a difference in
  the result is a mock bug and is fixed in the mock (contract check, section 3.2).
- **Budget:** the profile starts in under three minutes and the six scenarios finish in under ten, so the suite can run
  on every pull request. Artifacts on failure follow section 3.1.
- **Local use:** `make interop` runs the same profile and suite on a developer machine with Docker.

## 4. Traceability (spec area to tests)

| SPEC area | Primary test levels | Section here |
|---|---|---|
| 4.1 to 4.4 Ticket view, list, attachments | E2E, visual, a11y, integration | 5.5, 5.8 |
| 4.5 Concurrent editing (presence, drafts, optimistic save) | Concurrency, merge corpus, E2E two-user, draft crash tests, chaos, load | 5.1 |
| 5 Admin tabs, 17 enterprise | Integration, E2E, security | 5.7 |
| 6, 7 Boards, backlog, weekly sprints | Integration, E2E, time-travel tests | 5.5 |
| 8, 9 Custom fields, workflows | Unit, integration, contract | 5.5 |
| 10 Search, TQL | Unit, fuzz, conformance, perf | 5.4 |
| 13, 16 Notifications, automation | Integration (outbox), E2E | 5.5 |
| 18.1 Jira mirror | Sync, permission mapping and read-only enforcement tests; chaos (Jira down, 429) on `jira-fake`; real Jira verification before a major release | 5.6, 5.17 |
| 18.2 Import/export | Round-trip, golden, scale | 5.6 |
| 18.3 MCP and agent-driven development | Protocol conformance, authorization matrix, red-team, E2E agent loop | 5.9 |
| 17.6 Blob storage | Integration on fs, MinIO and AWS S3, security, load isolation, fsck | 5.10 |
| 18.4 Git integration | Provider conformance, webhook and rate-limit tests, rules, UI, nightly real GitHub and GitLab | 5.12 |
| 13.1 Email | Mail corpora, spoofing and loop tests, quote-stripping accuracy, Compose `mail` profile | 5.13 |
| 17.7 to 17.9 Install, upgrade, licensing | Fresh installs, rolling and offline upgrades, skew tests, licence scans | 5.14 |
| 9.1 Issue lifecycle, 7.2 work calendars | Plan versus result, alias resolution, races, trash and purge, calendar matrix | 5.15 |
| 15.1 Portal identity | Authentication, isolation, visibility, abuse controls | 5.16 |
| 18.2 Pause state, consistency model | Integration under live load, chaos, restore round trip | 5.11 |
| 19 Mobile, offline | E2E with network control, a11y | 5.8 |
| 20 A11y, i18n | axe, manual, pseudo-locale | 5.8 |
| 21 Non-functional targets | Perf, chaos, security | 5.2, 5.7 |
| DESIGN Part I tokens | Visual regression, contrast checker | 5.8 |
| DESIGN Part II architecture | Convergence, load, chaos | 5.1, 5.2, 5.3 |

## 5. Test areas

Test case ids are stable (`AREA-NN`) so they can be referenced from issues and CI reports. Each row is a family of
automated cases, not a single check.

### 5.1 Concurrent editing: presence, drafts and optimistic save (highest priority)

**Saves, conflicts and merging**

| Id | Case | Method | Pass criterion |
|---|---|---|---|
| LIVE-01 | Compare-and-set: 2 to 50 simulated users save the same field with the same `base_seq` through the real API | Concurrency test | Exactly one save succeeds, the rest get `409` with the current document; no acknowledged save lost; `text_seq` monotonic without gaps |
| LIVE-02 | Two people edit the same description in two browser contexts: non-overlapping and overlapping changes | Deterministic E2E | A saves, B sees the banner and later the conflict dialog; non-overlapping changes merge automatically; overlaps are chosen per block; both versions in history |
| LIVE-03 | Conflict dialog actions: Save merged result, Keep mine, Keep theirs, Copy mine and reload; a third save during resolution | E2E | Each does exactly what the label says; "keep mine" is recorded and restorable; the dialog repeats when needed; keyboard and screen reader operable |
| LIVE-04 | Three-way merge corpus: base, theirs and mine triples (paragraphs, lists, tables, marks, mentions, Unicode, RTL, large documents) | Browser test runner against the real merge code | Expected merged output and hunks; merge with an unchanged side is the identity; stable when sides are swapped; merge-size limit offers only keep or copy |
| LIVE-05 | Idempotent retry: lost acknowledgement, double click, retry after a timeout | Fault injection | The same `save_id` applies once and returns success again; a new `save_id` after a conflict is a new save |
| LIVE-06 | Rich-text round trip: marks, lists, tables, mentions, issue-key links, inline attachments to Markdown and ADF and back | Property and golden tests | No loss for supported nodes; unsupported ADF kept as opaque blocks |
| LIVE-07 | API, automation and agent text writes: anchored patch (unique, missing, ambiguous `old_text`), conditional replace (fresh and stale `text_seq`), bare replace | Integration | Patch applies once or fails with the current context; stale conditional replace returns `412` with the current text; bare replace is last write wins, recorded and restorable; a person with the field open sees the banner and their own save conflicts normally |
| LIVE-08 | Bulk edits and imports replacing text fields while people have them open | Integration | Conflict-checked per issue and reported per issue; affected editors get a notice |
| LIVE-09 | **Reliability campaign:** simulated users (2 to 50) doing saves, conflicts, retries, API writes and restores through the real API, with injected node kills mid-save, packet loss, delay and duplication, Postgres restart, Redis loss, clock skew and pauses; plus a draft model of editing sessions with injected crashes | Fleet of simulated clients, per-save counters, the reconciler of DESIGN 12.9 | At least 3x10^8 saves with zero E1, E2 or E4, and at least 3x10^6 editing sessions with the unrecoverable-draft rate within E3 (rule of three, 95% confidence for SPEC 21); a shorter campaign does not count |
| LIVE-10 | Server drafts: 10 s autosave, restore, two tabs of one user, 30-day expiry, per-user cap, purge on issue purge and GDPR erase | Integration | Private to the user, never indexed or visible to others; newest tab wins with a clear banner; removed when it should be |
| LIVE-11 | Local drafts: forced tab kill, crash, reload, offline period, storage eviction, private mode, base version changed meanwhile | Browser E2E with fault control | Draft restored with a banner (at most 2 s of typing lost); eviction and private mode warn; a changed base opens the conflict dialog |
| LIVE-12 | Measurement self-test: inject a missing acknowledged save, a stale save accepted, a destroyed draft and a mutated stored document | Fault injection | The reconciler and counters detect 100% of injected E1, E2, E3 and E4 cases |
| LIVE-13 | Validation and sanitization on save: disallowed nodes, marks and attributes, `javascript:` links, deep nesting, huge documents, malformed JSON; one shared schema definition for client and server; golden documents | Property, fuzz and golden tests | Rejected or sanitized and reported to the saver; no silent change of valid content (E4); client and server validators agree; the CSP blocks inline script |
| LIVE-14 | Counters: 50 users saving text while REST clients update scalar fields with `If-Match` on `version` | Load and integration | Text saves never bump `version`; no false `412` (zero in the run); a real scalar conflict still returns `412` |
| LIVE-15 | `text_seq` on every read (REST, MCP, UI bootstrap) and on events | Integration | Present everywhere; bumped by each save; a `version` gap triggers a refetch; `text_seq` gaps are harmless |
| LIVE-16 | Scalar fields: two people change the same field and different fields | E2E | Different fields merge silently; the same field gives a conflict notice with a reapply action; optimistic changes roll back with an explanation |
| LIVE-17 | Events are best effort: dropped events, Redis loss, socket down | Fault injection | Pages refetch on focus or on a gap; saves and conflict detection are unaffected |
| LIVE-18 | Editing during lifecycle operations: move, convert, delete, restore, archive while an editor is open | E2E | The editor shows a notice and keeps the draft; a save after a move follows the issue; a save after a delete returns `410` and the text stays in the draft |

**Durability and recovery**

| Id | Case | Method | Pass criterion |
|---|---|---|---|
| DUR-01 | Kill the app node after the commit but before the acknowledgement of a save | Chaos | The retried save (same `save_id`) is applied exactly once; nothing lost |
| DUR-02 | Server restart during editing sessions | Chaos | Drafts and open editors unaffected; saves retry and succeed |
| DUR-03 | Postgres restart or failover | Chaos | Saves fail clearly and retry; an acknowledged save is never lost; RPO per profile (synchronous standby gives 0); recovery within RTO |
| DUR-04 | History writer or outbox consumer lag or failure | Fault injection | Saved content unaffected; history rebuilt exactly once; reconcile repairs; lag alert fires |
| DUR-05 | Validator error or schema version change during a session | Integration | The draft is preserved; the user gets a clear message |
| DUR-06 | Saves through different nodes of a 3-node profile | 3-node profile | The row lock gives one winner; events reach all nodes; with Redis lost, presence is local only and saves are unaffected |
| DUR-07 | Corruption of `issue_text` or `text_version` rows | Fault injection | The checksum scrubber detects it, alerts, and the row is repaired from a replica or backup |
| DUR-08 | Draft expiry and cleanup | Integration | Expired drafts removed; saved content never touched |

**Presence and permissions**

| Id | Case | Pass criterion |
|---|---|---|
| PRES-01 | Avatar stack, "is editing" field note, warning on opening an editor on a field in use, 10 s heartbeat, 30 s expiry | Appears, updates and expires as specified; names always shown; accessible; never blocks |
| PRES-02 | "Appear offline in editors" and the org-wide off switch | No presence emitted or received |
| PRES-03 | Presence or realtime unavailable (socket, Redis, gateway down) | Editing and saving work; presence disappears; conflicts still caught by version |
| PRES-04 | User without Edit Issues | Can view and see who is editing; cannot open an editor or save |
| PRES-05 | Field or issue-security restriction | Channel refused; no leak of presence or content; revoked on permission change |
| PRES-06 | 50 viewers on one ticket and 1,000 idle connections per node | Presence messages only on state change and heartbeat (no per-keystroke traffic, asserted by counting); cost within bounds |
| PRES-07 | Presence across nodes | Converges within 1 s typical; local-only when Redis is down |

**Notifications and history**

| Id | Case | Pass criterion |
|---|---|---|
| HIST-01 | One saved change by A | One history record per tracked field per save; before, after and diff correct; actor is the saver |
| HIST-02 | Restore a version | A normal save with the compare-and-set, recorded; people with the field open see the banner |
| HIST-03 | Mentions in saved text | Mentioned user notified once |
| HIST-04 | Notification batching | Saves by the same person within a few minutes give one "edited by A" notice |
| HIST-05 | Tracked-field setting: tracked, untracked, flag changed later | Untracked fields leave no history; a flag change applies going forward |
| HIST-06 | Eventual correctness: pause or kill the history writer during saves, backlog, restart; inject a missing record | Every tracked change recorded exactly once with correct before and after and order; reconcile replays the gap; lag metric and alert work |
| HIST-07 | Erasure and legal hold | GDPR redaction replaces stored before and after text and keeps who, when and field; legal hold blocks deletion |
| HIST-08 | Diff and storage: long descriptions, many saves, rich text (marks, tables), concurrent API edit | Versions deduplicated and size bounded; diffs correct; restoring an old version yields the expected content and is itself recorded |

### 5.2 Latency and performance

Measurement definitions (so numbers are comparable):

- **Time to usable content** (UI): `navigationStart` (or route change) to the first paint that contains real data for the
  screen, from a `performance.mark` in the app, collected by browser automation (lab) and RUM (field).
- **Server time**: `Server-Timing` header (handler start to last byte queued), measured at the app, excluding client.
- **Save latency**: server time from the `Server-Timing` header; user-perceived time from the click on Save to the confirmation
  render, from a `performance.mark`. **Presence and event latency**: a test-only timestamp in the message, captured when the
  receiving page renders it; clocks aligned on the same host or via a measured offset.
- Lab runs use warm app shell, `perf` hardware, localhost or LAN; each target is stated at p95 over at least 1,000
  samples after a warm-up; results report p50, p95, p99 and the sample count.

| Id | Case | Target (SPEC 21, DESIGN 11, 12.8) |
|---|---|---|
| PERF-01 | Open ticket, warm cache, `org` data | Usable content under 300 ms p95; server under 50 ms p95 |
| PERF-02 | Open ticket, cold data, warm shell | Under 300 ms p95 |
| PERF-03 | Issue list page, `scale` data | Server under 50 ms p95; usable content under 300 ms |
| PERF-04 | Board view, 500 issues | Server under 100 ms p95; usable content under 300 ms |
| PERF-05 | Search, `scale` data, OpenSearch engine (and PostgreSQL up to 100,000 issues, PERF-16) | Under 400 ms p95 including the permission pre-filter and final check; typeahead under 150 ms |
| PERF-06 | First visit, empty cache | Under 1 s; initial JS under 200 KB gzipped; editor chunk under 150 KB |
| PERF-07 | Quick action (transition, assign, comment) | Optimistic result immediate; confirmed within 300 ms |
| PERF-08 | Prev/next ticket in list with prefetch | Served from local store, under 100 ms |
| PERF-09 | Open the editor (editor chunk warm) and type in a 1 MB document on the throttled phone profile | Editor opens in under 100 ms; no main-thread block over 16 ms while typing or autosaving |
| PERF-10 | Save a text field: server time and user-perceived time | Server under 50 ms p95; click to confirmation under 300 ms p95 |
| PERF-11 | Conflict: `409` response time and merge time in the browser for typical (100 KB) and large (1 MB) documents | Response under 100 ms p95; the dialog opens without another request; merge under 200 ms on the phone profile for 100 KB |
| PERF-12 | Presence change visible to other people, same node and across nodes | Under 1 s p95 |
| PERF-13 | Scalar change and text-saved event visible on other open pages | Under 1 s p95 |
| PERF-14 | Throttled phone profile (4x CPU slowdown, mid-range network) | Compact list scrolls at 60 fps; long tasks under 50 ms |
| PERF-15 | Mirrored project screens (ticket, list, board) on a mirror the size of `team` | Same budgets as native (300 ms p95) because reads come from PostgreSQL; stale marker once sync lag passes the threshold |
| PERF-16 | **PostgreSQL search size curve.** `search100k` generator at 10,000, 25,000, 50,000, 100,000, 150,000 and 250,000 issues (descriptions averaging 2 KB, five comments per issue, Zipf term distribution, several languages). Query mix: text only, text with project, text with status and assignee, rare and common terms, two-term AND, phrase, key lookup, prefix; ACL sets of 1, 50 and 1,000 projects; 20 concurrent users with 50 editors typing in the background; warm and cold cache | Search p95 at most 400 ms at 100,000 issues in both cache states; the size where p95 crosses 400 ms recorded as headroom; index sizes, build times, autovacuum and write cost per 100,000 issues published; warning thresholds (80,000 and 100,000) set from the result |
| PERF-17 | Typeahead curve on the same sizes (key, prefix, trigram, recently viewed) | p95 at most 150 ms at 100,000 issues |

**Load and capacity**

| Id | Case | Pass criterion |
|---|---|---|
| LOAD-01 | 50 concurrent users with editors open, saving text and scalar fields at human rates, presence on | PERF-10 to PERF-13 hold |
| LOAD-02 | One ticket with 50 viewers and 10 open editors, saves racing | Conflicts handled correctly; presence cost bounded; presence-entry limit enforced |
| LOAD-03 | 1,000 idle connections per node | Memory and CPU within documented bounds; heartbeat cost stable |
| LOAD-04 | 3 nodes, 20 tickets, 50 users: presence and events across nodes | Cross-node presence and event latency under 1 s p95 |
| LOAD-05 | Slow consumer on a busy ticket | Events coalesced or dropped for that socket, then it refetches; other users unaffected |
| LOAD-06 | Soak: 24 hours mixed workload | No memory growth, goroutine leaks, or latency drift; outbox lag bounded |
| LOAD-07 | Write storm (bulk transition of 10k issues) | Interactive latency of other users stays within budget |

**Regression gates:** a PR that moves any PERF p95 more than 10% worse, or breaks a budget, fails the build; budgets
live in a version-controlled file so changes are reviewed.

### 5.3 Resilience (chaos)

| Id | Fault | Expected behavior |
|---|---|---|
| CHAOS-01 | Kill an app node (single and multi-node) mid-save | Clients reconnect within seconds; no acknowledged save lost; retried saves applied once; drafts intact; UI shows state |
| CHAOS-02 | Redis down, then restored (multi-node) | Readiness degraded; presence and events local only and cache invalidation on TTL; saves unaffected; full recovery on return | | **covered (M5.24)** `TestCHAOS02_*` against `redismock` (saves on both nodes unaffected and fast while down; each node's own events and presence continue, the other's missing; the link status and cluster status show it; events and presence resume on return; nothing late; every save in the database) and `TestRedisGoesAwayAndComesBack`; `/readyz` degraded note. The real-Redis evidence is INTEROP-04 (section 3.3)
| CHAOS-03 | Packet loss and latency (5% loss, 300 ms) | Editing and saving stay usable; retries idempotent; conflicts still caught | | **covered at the HTTP level (M5.31)** `make chaos` (`TestCHAOS03_NetworkFaultsAndNodeFailures`): two real nodes behind a balancer, 300 ms latency each way, 5% of requests lost, 5% of answers lost after the commit, 3% of writes delivered twice, 12 people on 3 shared texts, a node killed mid-save, Redis stopped, PostgreSQL restarted; reconciler E1=E2=E4=0 and no gap on all three texts, about 90 acknowledged appends per text, 62 to 67 requests and 63 to 64 answers lost per run; the same run with a client that does not reuse its save id fails (E4 4 to 5 per text and gaps). M5.35 adds the TCP level (`TestCHAOS05_PacketLevelFaults`, toxiproxy between the people and each node: 120 ms of latency with 100 ms jitter in both directions, the answer stream sliced into 64-byte pieces, 5% of connections reset as they open, 5% of answers cut after 40 bytes, 8% of plain requests failing; the same node kill, Redis stop and PostgreSQL restart; one new connection per request): about 125 acknowledged appends per text with E1=E2=E4=0 and no gap. Not covered: packet reordering and corruption (the toxics act on TCP streams), clock skew between real nodes (CHAOS-08 covers it in process)
| CHAOS-04 | Network partition between nodes | Saves on either side stay correct (the database decides); presence local; events resume on heal | | **covered (M5.24)** `TestCHAOS04_*` and `TestOneNodeCutOffFromRedis` (a node cut off from the bus: two edits from the same base, one on each side, give exactly one winner and one conflict; events and presence stay on their side; the cut-off node reads the truth from the database; heal resumes both); mutation-tested
| CHAOS-05 | Slow or full disk | Backpressure, clear error, no corruption | **covered** `TestCHAOS05_*` (a full disk: 507 `storage_full` with a plain message, nothing left behind, other work unaffected, recovery; a stuck disk: the next upload refused at once as busy, interactive p95 unchanged, clean failure and slot release; the 507 mapping mutation-tested). The database's own disk-full error is mapped but not exercised
| CHAOS-06 | Realtime layer disabled | Ticket loads; saves work through REST; presence absent; conflicts still caught | **covered** `TestCHAOS06_*` (no hub: the ticket answers 404 `realtime_disabled`, presence by HTTP, a stale save caught) and e2e `CHAOS-06` (the page asks once and stops, edits, saves, and gets the conflict dialog)
| CHAOS-07 | Deploy with graceful drain | Clients reconnect to another node in under 1 s; no lost save; drafts untouched | **covered** `TestCHAOS07_*`: the hub (24 pages told to reconnect with spread delays, sockets closed, new ones refused) and the real binary on SIGTERM (readiness fails within a second while liveness stays up, an open page gets the hint, a request in flight when the signal arrives is still answered, clean exit; each step mutation-tested). Not covered: a real load balancer and a multi-node rolling restart
| CHAOS-08 | Clock skew between nodes | Ordering by the database compare-and-set and assigned versions, never node clocks; timestamps sane | **covered** `TestCHAOS08_*` (nodes an hour ahead and behind: comment and history order follow the database, versions have no gaps, a stale save is refused; lease expiry across skewed nodes, with a control showing the hazard before the correction; the offset is measured and reported) **Real nodes (M5.36):** `TestCHAOS08_ClockSkewBetweenRealNodes` (`make chaos`): two containers with `fault_clock_skew` +1h and -1h; offsets reported and warned; sessions and tokens cross nodes; a pause holds both and ends on time for both; about 700 acknowledged appends per text with E1=E2=E4=0; every stored time within 45 s of the database's clock; the check fails when the node does not correct its clock
| CHAOS-09 | Very large documents and merge-size limits | Over-limit documents refused or offered keep/copy only, with a clear message; the server stays up; others unaffected |

### 5.4 Search and TQL

| Id | Case | Pass criterion |
|---|---|---|
| TQL-01 | Grammar and semantics per SPEC 10 (fields, operators, functions, `ORDER BY`, text search) | Golden query set with expected results |
| TQL-02 | Parser fuzzing | No panics, no hangs, bounded memory |
| TQL-03 | JQL compatibility subset | A corpus of real-world JQL (anonymized) parses or fails with a clear error; no silent wrong results |
| TQL-04 | Custom fields queryable by id and name, typed operators (`was`, `changed`, `is EMPTY`) | Correct results across contexts |
| TQL-05 | Same TQL suite against PostgreSQL and OpenSearch implementations, after the index has caught up | Identical result sets |
| TQL-06 | Permission filtering in search | Never returns issues the user cannot see |
| TQL-07 | Index freshness | A saved edit is searchable within the stated delay |
| TQL-08 | Routing: boards, backlog, rules, bulk targets, permission checks, exports and single-issue reads versus search boxes, lists, gadgets and typeahead; queries with `was` and `changed` | Exact consumers always use PostgreSQL; search consumers use the index when enabled; history clauses route to PostgreSQL |
| TQL-09 | Freshness and lag: normal load, write storm (bulk transition of 10,000 issues), indexer paused; lag indicator; own-edit overlay | Freshness p95 within 5 s; indicator shown past the threshold; own changes visible in lists; backlog drains; alarm at 60 s |
| TQL-10 | Indexer correctness: duplicates, retries, out-of-order delivery, crash mid-bulk, dead letters | External versioning prevents regression; no lost or duplicated document; dead letters alerted |
| TQL-11 | Online reindex under live writes: dual write, count and checksum verification, alias swap, resume after kill, mapping change, per-project reindex | Index equals PostgreSQL after the swap; interactive p95 unaffected; rollback window works |
| TQL-12 | Reconcile and drift: inject missing, stale and orphaned documents (including a purged issue) | Detected and repaired within the reconcile interval; visible in Admin |
| TQL-13 | **Permission filtering matrix:** many-project users, security levels, private projects, group membership revoked during lag, issue moved to a restricted project during lag, issue trashed or re-secured during lag, internal comments, restricted fields, attachment text, typeahead and facets | No issue, text, count or suggestion the user cannot see is returned; the final PostgreSQL check removes every stale hit; snippets come only from authorized PostgreSQL text |
| TQL-14 | User above the term limit | Exclusion filter, then PostgreSQL fallback with a warning; still no leak |
| TQL-15 | Totals and paging: approximate totals flagged, deep paging with stable cursors, sorting by typed custom fields | Cursors stable under concurrent writes; "about N" shown above the threshold |
| TQL-16 | OpenSearch outage, slow node, red cluster, network partition | Writes never blocked; fallback or "search is limited" per instance size; no timeouts; backlog capped and drained on recovery |
| TQL-17 | Multi-organization isolation | The organization filter cannot be omitted by any query path; no cross-tenant hit (SEC-09) |
| TQL-18 | Operations: rolling OpenSearch upgrade, TLS and credential rotation, optional snapshot restore, `doctor` checks, Compose `search` profile on one and three nodes | Search stays available; misconfiguration named by `doctor` |
| TQL-19 | Scale: `scale` dataset (5M issues) with OpenSearch; the PostgreSQL engine verified at 100,000 issues with measured headroom above it | Search p95 within 400 ms including pre-filter and final check; typeahead within 150 ms; the PostgreSQL limit confirmed and reindex time recorded in the report |
| TQL-20 | Purge and GDPR erase against the index | Removed from the index within the stated delay and confirmed by reconcile |
| TQL-21 | Index design in use: `EXPLAIN` plan-shape assertions per query class (text only, text and project, text and several filters, key lookup, typeahead, comment match) on the `search100k` dataset | GIN scans on `issue_search` and `comment_search`, trigram GIN for typeahead, btree for keys; no sequential scan of the search tables; plan regressions fail CI |
| TQL-22 | Tokenization contract: `ABC-123` and old keys, keys inside text, `UTF-8`, versions, `ERR_CONN_RESET`, paths, URLs, emails, CamelCase, accents, mixed case, CJK, emoji | Behavior pinned per PostgreSQL version; key tokens resolve by exact lookup and are listed first; documented limitations match results |
| TQL-23 | Text configuration: language-neutral default versus an English configuration; switching configuration on a live instance | Recall as documented for each; online rebuild without downtime or wrong results during the switch |
| TQL-24 | Ranking: summary outranks description outranks comment; recency tie-break; rank window (2,000) against full ranking on small sets; explicit `ORDER BY`; "sort by updated"; paging with the snapshot token | Order as specified; paging stable under concurrent edits; documented window behavior |
| TQL-25 | Combined plans: rare term with broad filters, common term with narrow filters, both broad, both narrow; ACL sets of 1, 50 and 1,000 projects; caps and the "limited" marker; "10,000+" totals | Correct results; each case stays within the latency budget; the plan chosen matches the rule; no unbounded scans |
| TQL-26 | Comments: separate table, internal-comment visibility, edits and deletes reflected, issues with thousands of comments | Internal text never matched for unentitled users; counts of matches correct |
| TQL-27 | Write path: 50 users saving text, bulk edits, saving back to the same text; GIN pending list and autovacuum behavior; 1 million updates | Writes skipped when `text_hash` is unchanged; index size and bloat bounded; interactive latency and `issue` row update performance unaffected; `REINDEX CONCURRENTLY` works under load |

### 5.5 Core tracker behavior

| Id | Area | Key checks |
|---|---|---|
| CORE-01 | Issues and hierarchy | Create, edit, move, clone, delete; Initiative/Epic/Story/Subtask rules; keys unique and stable |
| CORE-02 | Workflows | Conditions, validators, post-functions; invalid transitions explain why; draft/publish and in-flight migration |
| CORE-03 | Custom fields | Each type, contexts, defaults, required, behavior rules, field-level permissions, trash and restore |
| CORE-04 | Boards | Drag to transition with enforcement, WIP breach, swimlanes, quick filters, rank persistence |
| CORE-05 | Backlog and sprints | Drag between panels, estimates, inline create, complete-sprint dialog |
| CORE-06 | Weekly sprints | Auto-create next N, auto-rollover, week boundaries and timezone, holidays, dry run, carry-over count, scope change |
| CORE-07 | Concurrency of scalar fields | Two users change the same and different fields at once; per-field merge; optimistic rollback with explanation |
| CORE-08 | Attachments | Upload (paste, drag, chunked, resume), scan states, previewer for each type, permissions, limits |
| CORE-09 | Comments and activity | Visibility restrictions, reactions, collapse behavior, filters |
| SEC-04 | done for OIDC, SCIM, tokens, sessions, 2FA, lockout and password (partial: no SAML, WebAuthn, enforcement policy, IP allow-list) | SSO: `TestSEC04_SSOSignInCreatesAndRecognisesAccounts`, `_SSOAuthorizationRequestUsesPKCEStateAndNonce`, `_SSORefusesForgedAndStaleSignIns` (ten forgeries, replay, expiry, stolen code, key rotation, egress), `_SSOAccountMatchingAndPolicy`; SCIM: `TestSEC04_SCIMProvisioningLifecycle`; e2e `account.spec.js`; earlier:  `TestSEC04_TokensExpireAndRevokeAtOnceAndScopedTokensAreNotRESTCredentials`, `_SessionsAreListedAndRevokedAtOnce`, `_TwoFactorEnrolmentSignInReplayAndRecovery`, `_LockoutAfterRepeatedFailuresWithoutRevealingAccounts`, `_PasswordChangeAndPolicy`, `TestSEC04_HTTPSignInTwoFactorLockoutAndTokens`, e2e `account.spec.js` |
| CORE-10 | Notifications and automation | Outbox delivery exactly once, batching, rule loops prevented, dry run |
| CORE-11 | Time-dependent behavior | Sprint rollover and schedules tested with a controllable clock, including DST changes and leap days |
| CORE-12 | Dashboards, reports, releases | Gadget data matches filter results; report math (burndown, velocity) against hand-computed fixtures. Dashboards: DASH01..05 (domain: per-viewer scope, org sharing, validation, iframe hosts) and the e2e `CORE-12` (build, text shown as text, favourite, axe). Versions: VER01..05 (domain: administration, per-project names, history, progress per viewer, release warning and move, TQL fixVersion and releasedVersions(), notes, delete and move) and the e2e `CORE-12` versions test (add, set on an issue, progress, warning, move and release, notes, axe). Approvals: APR01..06 (rules all, any and n; final and restricted decisions, agents refused; approver sets from users, groups and derived; thread, history and the waiting list; the workflow condition; notifications and the one reminder) and the e2e `CORE-12` approvals test |

### 5.6 Import, export and Jira

| Id | Case | Pass criterion |
|---|---|---|
| IMP-01 | Jira REST import against `jira-fake` (and again against the real sandbox in 5.17, JREAL-03) | Counts and field values match; original dates, authors and keys (or aliases) preserved |
| IMP-02 | Jira CSV import with repeated columns, multi-line cells, unusual encodings | All rows mapped or listed in the error report |
| IMP-03 | Jira backup XML (best effort) | Imports supported content; unmappable items reported |
| IMP-04 | ADF to document model round trip | No content lost; unknown nodes preserved opaque |
| IMP-05 | Dry run | Predicts counts, gaps and duration; commits nothing |
| IMP-06 | Idempotency | Re-importing the same source updates in place; no duplicates |
| IMP-07 | Rollback by import id | Removes only records created by that import |
| IMP-08 | Rate limits (`429`, `Retry-After`) during import, scripted in `jira-fake` | Job slows, resumes, completes |
| IMP-09 | **M5.13: GitHub Issues, Trello, Linear and Asana built (IMP01..05 on a fixture per tool, e2e); generic CSV earlier. M5.17: generic JSON with a JSONPath mapping built (IMP10, `internal/jsonpath` tests, e2e).** Third-party presets: GitHub Issues, Trello, Linear, Asana, generic CSV and JSON | Mapped fields correct on a fixture per tool; mapping templates reusable |
| EXP-01 | JSON bundle and CSV bundle round trip (export, import into empty instance) | Semantically identical project; attachments verified by checksum |
| EXP-02 | Large export (`org` data) | Streams, resumable, bounded memory, manifest checksums valid |
| EXP-03 | Export respects permissions, security levels, field restrictions | Restricted data absent unless an admin runs an audited full export |
| EXP-04 | Jira-importable CSV export (write-through push to Jira is deferred with the outbound half of the mirror) | The CSV imports correctly through Jira's CSV importer on the sandbox site (JREAL-04, real Jira verification only) |
| EXP-05 | Format stability | Golden bundles from each released version still import (backward compatibility) |

**Jira mirror mode** (one-way inbound; runs against `jira-fake` on every PR and nightly, including all fault cases; the same behavior is confirmed against a real Jira Cloud test site only in the real Jira verification before a major release, section 5.17)

| Id | Case | Pass criterion |
|---|---|---|
| MIR-01 | Backfill of a 10,000-issue project with custom fields, comments, attachments, links, sprints, versions and history; kill and resume | Counts and field values equal Jira; resumes without duplicates; interactive latency unaffected |
| MIR-02 | Incremental sync: webhook-hinted and polling paths, overlap window, duplicate, reordered and lost webhooks, reconcile | No lost change; freshness within 10 s p95 (webhook) and 60 s (polling fallback); drift repaired |
| MIR-03 | Deletes and moves in Jira: deleted issue, key change by move, archived or deleted project, closed user account | Deleted issues removed within the lag; key change recorded as an alias; accounts handled |
| MIR-04 | Change history: Jira changelog to `issue_history` (before, after, actor, source `Jira`); replay and reordering | Idempotent by changelog id; matches Jira's history |
| MIR-05 | ADF reading: all supported nodes, unsupported nodes as opaque blocks, hostile content, text extraction for search | Renders safely; nothing dropped; searchable |
| MIR-06 | Permission mapping: roles, groups, browse grants (user, group, role, reporter, assignee, project lead, any logged-in user), security levels, comment restrictions, unsupported grant types (fail closed), "verify with Jira" mode, revocation lag | No issue, comment or attachment visible beyond Jira's rules; revocation reaches Tasklane within 5 minutes; unsupported grants hidden and listed in Admin; failed permission sync fails closed |
| MIR-07 | Read-only enforcement: UI, REST, MCP, automation, email and Git rules attempt writes on mirrored issues | Every path returns the typed `read_only_mirror` error with the Jira link; Tasklane-side stars, unread, saved filters and dashboards still work |
| MIR-08 | Feature matrix on mirrored data: lists, search, ticket view, boards and backlog with drag disabled, sprints, reports, dashboards, development panel, notifications inbox | All read features work; unavailable ones are absent or explained with a link to Jira |
| MIR-09 | Jira rate limits (`429`, `Retry-After`), slow or unavailable Jira | Throttled correctly; stale marker shown; backfill never slows incremental sync or the UI |
| MIR-10 | Attachments: lazy fetch into the blob store, size caps, antivirus, deletion in Jira, per-download authorization | Only permitted users download; deleted files lose their reference |
| MIR-11 | Cut-over to native | Sync stops, project writable, keys and history preserved, no data loss; mirror rebuild possible before cut-over |
| MIR-12 | Mirror Admin tab and status: lag, backlog, webhook health, permission warnings, rate-limit usage | Accurate; alerts fire on lag and failures |
| MIR-13 | Pause and restore with mirrored projects | Pause drains the batch and records the watermark; restore resumes from it and reconciles |
| MIR-14 | Erasure and account closures reported by Jira; retention of mirrored data | Data removed from the mirror, search index and blob store within the stated delay |


### 5.7 Security and compliance

| Id | Case | Pass criterion |
|---|---|---|
| SEC-01 | Authorization matrix generated from the permission scheme: every API route and every WS channel x every role | Denied where expected; no escalation; no information leak in errors |
| SEC-02 | WS abuse: bad origin, forged ticket, oversized or malformed frames, flooding, subscription to others' channels | Rejected or disconnected; server stable |
| SEC-03 | Issue security levels and comment visibility across REST, search, notifications, exports, previewer, presence | Consistent enforcement everywhere |
| SEC-04 | **M5.14: SAML built (SAML01..04 in the domain, `internal/saml` fixtures from lxml with tampering, wrapping and replay variants, e2e); a real `keycloak` run is open.** SSO (SAML, OIDC), SCIM, 2FA, session revoke, API token scopes and expiry | Flows work; revoked access ends within the stated time | **Organization policy covered:** `TestSEC04_ThePolicyIs...` (who may set it, another organization refused, validation, both lock-out guards, audit, over HTTP), `_AnAllowList...` (sign-in and use refused from other networks, tokens, MCP check, a forged header ignored, the proxy chain, other organizations and the instance administrator exempt, loosening at once), `_ClientIPFollows...`, `_ASessionNeverOutlives...`, `_ARequiredSecondFactor...` (held at once, exempt pages, tokens and other organizations free, enrolling releases, single sign-on exempt, switching off releases) with each enforcement mutation-tested, and e2e `SEC-04` (the Security tab and the held member). **Email domains covered:** `TestSEC04_AnEmailDomainRestriction...` (normalising and refusing domains, creation inside and outside through the form and HTTP, the database refusing a direct insert and an activation, placeholders allowed, existing people left alone and counted, another organization unaffected, audit with the list) and `_SCIMCannotProvision...`, both mutation-tested against the trigger **WebAuthn covered:** `internal/webauthn` tests (three algorithms; each wrong origin, relying party, challenge, presence, verification, signature, algorithm, off-curve key, truncation; counter rules; a bounded CBOR reader; a fuzz target run for 1.7 million executions), domain `TestSEC04_*Passkey*` (registration with the password, spent challenges, duplicate keys, second step, another person's key, phishing origin, wrong relying party, five failures, expiry, clone alert, TOTP beside keys, reset, erase, export, IP address refused) and e2e `SEC-04` in Chrome with a virtual authenticator
| SEC-05 | Injection and XSS (rich text, attachments, smart links, TQL, CSV export formula injection) | Escaped or neutralized; CSP blocks inline script |
| SEC-06 | Attachment handling: type spoofing, zip bombs, antivirus states, separate origin | Blocked or quarantined |
| SEC-07 | Audit log completeness and immutability | Every admin action, permission change, export download logged; entries cannot be altered |
| SEC-08 | Dependency, secret and SBOM scanning, signed release verification | No critical findings at release |
| SEC-09 | Multi-org isolation | No cross-tenant read or write via any path, including WS and exports |
| SEC-10 | Retention, legal hold, GDPR export and erase | Completed within the stated time; erased data not recoverable from derived stores |
| SEC-11 | External penetration test | **Postponed.** Not a gate for any milestone or release for now. When scheduled, criticals and highs must be fixed before the release it covers. Until then SEC-01..10 and SEC-12, the scanners and the internal abuse tests carry the security assurance. |
| SEC-12 | SSRF through every outbound feature (webhooks, Jira and Git base URLs, import from URL, attachment fetch, unfurl): private and metadata addresses, DNS rebinding, redirects to internal hosts, oversized or slow responses, credential forwarding across hosts; run in the Compose `egress` profile | Blocked unless the connection explicitly allows the range; no internal service reachable; redirects re-checked; limits enforced |

### 5.8 UI, accessibility, themes, offline

| Id | Case | Pass criterion |
|---|---|---|
| UI-01 | Visual regression of every component and screen in dark, light, compact and comfortable | No unreviewed diff |
| UI-02 | Contrast checker over all token pairs (DESIGN 1) | 4.5:1 text and 3:1 UI in both themes; a failing pair blocks the build |
| UI-03 | Theme: default dark, `system`, no flash on first paint | No white flash; setting stored server side |
| UI-04 | Responsive: phone, tablet, desktop widths and the 600 and 1024 breakpoints | Correct pane layout and navigation |
| UI-05 | Gmail-style ticket reading: collapse, unread, expand, attachment strip, sticky reply, scroll restoration | Behaves per SPEC 4 |
| A11Y-01 | axe automated scan on every screen | Zero serious or critical issues |
| A11Y-02 | Keyboard-only run of the main flows (create, transition, comment, board drag alternative, previewer) | Completable; focus visible; no traps |
| A11Y-03 | Screen reader scripts (editor, conflict dialog, presence notes announced politely, board, previewer) | Understandable; presence not noisy |
| A11Y-04 | 200% text scale, reduced motion, high contrast | No clipping; motion replaced by fades; borders raised |
| I18N-01 | Pseudo-locale, long strings, RTL, locale-specific date and number formats, timezones | No truncation of controls; layout mirrors correctly |
| OFF-01 | Offline: read recent issues, queue comments and transitions, reconnect | Queue replays in order; conflict notices shown |
| OFF-02 | PWA install, service worker update, stale shell | Safe upgrade; no mixed-version breakage |
| OFF-03 | Cache hygiene: logout clears data; per-user namespaces | No data visible to the next user |

### 5.9 MCP server and agent-driven development

| Id | Case | Pass criterion |
|---|---|---|
| MCP-01 | Protocol conformance on stdio and Streamable HTTP against a pinned MCP spec revision, with a reference client and Claude Code | Initialize, list tools/resources/prompts, call, error and cancellation behave per spec |
| MCP-02 | Tool schema snapshot tests | Any breaking change to a tool schema fails CI; additive changes pass |
| MCP-03 | Authorization matrix: every tool x scope x role x issue security level | Denied where expected; same decisions as REST for the same identity; no information leak in errors |
| MCP-04 | OAuth 2.1 + PKCE flow, token scopes, expiry, revoke, on-behalf-of intersection | Revoked or expired access ends immediately; agent never exceeds the user |
| MCP-05 | `claim_issue` lease: two agents race for one issue; lease expiry; heartbeat; stalled display | Exactly one winner; expired lease frees the issue; status and assignee consistent |
| MCP-06 | `edit_issue_text` while a human types in the same field | Edit merges; non-unique or stale `old_text` fails without changes; actor shown in presence and history |
| MCP-07 | Human gates: agent attempts transition to Done where `actor is human` is required | Refused with a clear reason; Review transition allowed |
| MCP-08 | `request_human_input` flag, notification, human reply resumes | Flag visible on list and board; owner notified; unflag clears |
| MCP-09 | Prompt-injection resilience: issue and comment text containing instructions, hidden text, markdown tricks, attachment text | Tool results carry provenance tags and are inert data; red-team suite shows no tool is triggered by content alone (client-side behavior documented) | **prompts covered** `TestMCP09_IssueTextInAPromptIsFencedAndCannotEscapeTheFence` (hostile text inside every prompt sits inside the fence, cannot close or reopen it, and the message says only its own instructions count; mutation-tested) and `TestMCP_Prompts*` (list, arguments, read-only, authorization, validation). The full red-team suite over tools and attachments is not built
| MCP-10 | Secrets and sensitive fields | Never returned; excluded fields absent from agent views |
| MCP-11 | Rate limits, write budgets, result size caps, pagination cursors, bulk caps | Enforced with clear errors; no unbounded response |
| MCP-12 | `dry_run` on bulk tools; `idempotency_key` retries | Dry run changes nothing and previews exactly; retry does not duplicate |
| MCP-13 | Audit completeness | Every call recorded with agent, on-behalf-of, client, tool, redacted args, outcome; entries immutable |
| MCP-14 | Kill switch and per-agent pause | Takes effect immediately; in-flight leases released; subsequent calls fail cleanly |
| MCP-15 | Resource subscriptions | Updates delivered for comment, status, assignment and filter matches within 1 s; unsubscribe works | **covered** `TestMCP15_*` (an issue subscription announced within a second for a comment, an assignment, a transition and a text edit and not for other issues or after unsubscribing; a filter subscription announced for matching issues, new and changed, not for ones that no longer match; the development resource announced only by code changes; access lost and token revoked end the notifications, each mutation-tested; stream and subscribe rules, session isolation, the 100 limit; a subscriber that never reads does not slow writers; stdio notifications)
| MCP-16 | Mirror mode: read tools work, write tools return the read-only mirror error with the Jira link | Supported tools work against `jira-fake` (and the sandbox in 5.17); unsupported ones return a clear error and appear in the compatibility check |
| MCP-17 | Latency | `get_issue_context` warm under 100 ms server p95; read tools within UI endpoint budgets; no regression of UI latency under MCP load |
| MCP-18 | Token efficiency | Median `get_issue_context` for the `team` dataset stays under a set size budget; long threads collapse as in the ticket view |
| MCP-19 | End-to-end agent scenario with a scripted agent: pick from "Agent ready", claim, plan comment, subtask, link PR, record tests, move to Review, human approves | Full loop completes; history, notifications and audit are correct |
| MCP-20 | Loop protection | An agent cannot trigger itself through automation or webhook cycles |

### 5.10 Blob storage

Datasets: `blobs` (generated: screenshots, 50 MB to 2 GB videos, 1 GB and 5 GB logs, zip bombs, type-spoofed files,
EICAR test file). Runs against `fs`, MinIO (CI) and a real AWS S3 bucket (nightly, with a request budget).

| Id | Case | Pass criterion |
|---|---|---|
| BLOB-01 | Paste screenshot, small single-request upload on `fs` and `s3` | Attachment visible, thumbnail appears asynchronously, original intact |
| BLOB-02 | 2 GiB resumable upload, connection dropped and resumed | Completes; SHA-256 matches; app memory stays flat (constant buffers) |
| BLOB-03 | Direct-to-S3 presigned multipart | Completion validated server side; tampered, truncated or wrong-type uploads rejected; nothing visible until committed |
| BLOB-04 | Range requests: video seek, log tail, conditional GET, `ETag` | Correct partial content; seeking works in browser players; 304 on revalidation |
| BLOB-05 | Download authorization: link minted only after permission checks; expiry; revoked user; risky types forced to download; separate origin | No access after expiry or revocation beyond the TTL; no inline execution of active content |
| BLOB-06 | Deduplication and refcount: same file attached twice, one deleted | Stored once; other attachment unaffected; bytes removed only after zero refs and retention |
| BLOB-23 | Reference integrity: every `blob` locator resolves to an object, no table holds file bytes, locators cannot be built from user input (path traversal, key injection) | `fsck` finds no dangling or orphaned references; traversal attempts rejected |
| BLOB-07 | Abandoned and failed uploads | Staging debris removed by the janitor within 24 hours; no orphan visible |
| BLOB-08 | Type sniffing vs declared, zip and decompression bombs, EICAR | Spoofed types flagged; bombs refused with a clear error; infected file `blocked` and not downloadable; `scanning` not downloadable |
| BLOB-09 | Derived data: thumbnail, video poster, optional transcode | Generated asynchronously, regenerable, never delays upload; failure shows fallback and retry |
| BLOB-10 | Large logs (1 GB and 5 GB) | Compressed at rest; last 1,000 lines shown in under 300 ms; paging, in-file search and ANSI render work; main search index capped |
| BLOB-11 | Store failure: S3 throttling, 403, outage; full disk | Retries with backoff; clear UI state; uploads resumable; readiness reflects health; no partial blob committed |
| BLOB-12 | Integrity: bit-rot, deleted or truncated object, `fsck` | Checksum mismatch detected on read or scan; `fsck` reports missing, orphan, corrupt; repair works |
| BLOB-13 | Backup and restore ordering | Restored database never references a missing blob; `fsck` clean after restore |
| BLOB-14 | Online migration `fs` to `s3` and back | Zero downtime; checksums verified; resumable; rollback works |
| BLOB-15 | Tiering policy | Cold objects move by rule; read-through correct; latency measured and documented |
| BLOB-16 | Encryption at rest and key rotation; TLS enforced to S3; SSE-KMS configuration | Data unreadable without keys; rotation without re-upload |
| BLOB-17 | Multi-node: `fs` on node-local storage, `fs` on shared storage, `s3` | Node-local multi-node refused at startup; shared and `s3` serve from any node |
| BLOB-18 | Quotas, class size limits, concurrent upload limits, bandwidth limits | Enforced with clear messages; usage reports correct |
| BLOB-19 | Retention, legal hold, GDPR erase | Held blobs survive; erased user's blobs and derived files removed when unreferenced |
| BLOB-20 | Latency isolation: concurrent large uploads and downloads during a mixed interactive load | Interactive p95 stays within SPEC 21 budgets (PERF-01..04); thumbnails never delay data paint |
| BLOB-21 | S3 compatibility: AWS S3 and MinIO conformance suite (multipart, presign, range, SSE, custom endpoint, path-style) | Identical behavior on both |
| BLOB-22 | MCP and import/export use the store | `attach_file` and bundles respect caps, quotas, scan, and stream without loading files in memory |

### 5.11 Consistency and the pause state

| Id | Case | Pass criterion |
|---|---|---|
| PAUSE-01 | Snapshot-mode export while 50 users save | Contains every save acknowledged before the marker and nothing after it; no write hold; when asked to wait for the history writer, history in the export is complete |
| PAUSE-02 | Backup and restore round trip with active users | Restored instance holds every acknowledged save up to the marker; history, projections and search rebuilt from the outbox; clients holding newer versions get a conflict instead of a silent overwrite | **covered** `TestPAUSE02_*` (a backup of an instance with data restores as the same instance: every issue, comment, history and version row, the trash, the people and their passwords, the audit chain, sequences continuing; eight writers saving during the backup, and what comes back is a consistent prefix with at least everything acknowledged before it began; a client ahead of the restored version is stopped; sessions not carried; mutation-tested against read-committed isolation and against leaving the guarding triggers on) M5.21: and over a running instance (`TestPAUSE02_RestoreOver*`: writers held with 503 and never broken, data becomes the backup's, safety backup is a real backup and undoes the restore, sessions gone, pause ended, audit recorded, refusals change nothing; `restore --replace` on the real binary)  and the real-binary commands
| PAUSE-03 | Derived data bounds without a pause | Saved text immediately visible in REST; history within seconds; search within about 5 s; projections within 1 s; nothing lost |
| PAUSE-04 | Writers during `draining` and `paused`: browser editors, REST, MCP, automation | Editors keep drafts and retry; API and MCP get `503` with `Retry-After`; nothing dropped or duplicated (idempotent `save_id`) |
| PAUSE-05 | `pause` mode import into existing issues | Affected editors get a notice and their saves conflict normally; no lost save; other projects unaffected |
| PAUSE-06 | Deadline and abort | A drain that cannot finish in time aborts, the scope returns to `running`, no data change, audit event written |
| PAUSE-07 | Crash of the job, the coordinating node, or Redis during a pause | Lease expiry or reconnect clears the pause; no node stays paused |
| PAUSE-08 | Overlapping scopes (instance vs project, two jobs) | Rejected or queued deterministically; never two conflicting pauses |
| PAUSE-09 | Backup ordering with the blob store | Database snapshot first, blobs after; restored database never references a missing blob; `fsck` clean | **covered** `TestPAUSE09_*` (an attachment that becomes garbage after the snapshot is still in the backup because the collector is held off, and is collected after; the restored database has no row without its file, fsck clean; mutation-tested against removing the hold)
| PAUSE-10 | `none` mode | Manifest records that derived data may trail by seconds; no pause applied |
| PAUSE-11 | Mirror mode | Pause drains each project's sync batch and holds new applies; the marker records the Jira sync watermark; resume continues from it | **covered** `TestPAUSE11_*` (120 issues in pages of 50: a pause arriving after the first page leaves exactly that page applied and the second held with a 503; the marker names the stored watermark; a project scope elsewhere does not name it and an instance scope does; after release the sync queries from the watermark and the mirror ends with all 120 once each; mutation-tested against removing the per-batch gate and against dropping the watermark from the marker)
| PAUSE-12 | Manifest | Contains mode, marker (log position, outbox position), time; restore verifies it | **covered** `TestPAUSE12_*` (the manifest carries mode, marker, version, secret check; refusals for an interrupted backup, a newer format, another format, a newer schema, a changed table, a changed attachment, a missing table, a wrong row count, a marker that does not match the restored log; a different secret key is reported)

### 5.12 Git integration (GitHub and GitLab)

Runs against `scm-fake` on every PR (both providers, recorded fixtures), against `gitlab-ce` in Compose and a real GitHub
test org nightly. Fixtures cover real payload shapes including forks, squash and rebase merges, force-pushes, draft PRs,
reviews, checks and pipelines.

| Id | Case | Pass criterion |
|---|---|---|
| GIT-01 | Connect GitHub App or token and GitLab token; scope check; invalid or expired credentials | Connection healthy with least privilege; read-only by default; bad credentials give a clear admin error |
| GIT-02 | Webhook verification: valid and invalid signature or secret, replay, duplicate delivery, oversized payload | Bad signatures rejected; duplicates and replays ignored; endpoint answers in under 100 ms |
| GIT-03 | Key detection in branch, commit, PR title and body; case; several keys; unknown project; `UTF-8`-style false positives; keys in moved issues | Only configured project keys link; aliases resolve; false positives do not link |
| GIT-04 | PR and MR lifecycle: open, edit title, draft, ready, review requested, approved, changes requested, merged (merge, squash, rebase), closed, reopened, force-push, fork | Dev panel state equals the provider state after each event; edits add and remove automatic links, manual links stay |
| GIT-05 | Build state: GitHub checks and statuses, GitLab pipelines and jobs; re-runs; aggregation | Worst-of state correct per PR and per issue; re-run replaces the old result |
| GIT-06 | Out-of-order, duplicated and lost webhooks; backfill and reconcile | Stored state converges to the provider's; reconcile repairs a deliberately dropped event |
| GIT-07 | Rate limits (GitHub primary and secondary, GitLab 429), `Retry-After`, conditional requests | Throttled correctly; backfill does not raise interactive p95 (PERF-01..04) |
| GIT-08 | Automation rules: branch to In Progress, PR to In Review, merge to Done; disallowed transition; loop protection; dry run | Valid moves applied and attributed ("moved by GitHub, PR #12"); invalid skipped and reported; no loops |
| GIT-09 | Smart commits: comment, time, transition by name; unmapped author; command in a PR body by a non-committer; default-branch-only setting; disabled by default | Run only as the mapped committer within their permissions; others ignored and reported; every run audited |
| GIT-10 | Identity mapping by verified email and linked provider identity; unmatched authors | Mapped correctly; unmatched shown by provider name; no wrong-user attribution |
| GIT-11 | Visibility: default (issue viewers) and restricted (private repo access required) | Restricted setting hides private-repo details from users without access; revocation takes effect | **covered** `TestGIT11_*` (domain; fail closed, link and list, revocation, unlink, stale list, TQL, automation, GitLab, override, audit)
| GIT-12 | Development panel UI: summary chips, expand, empty state, stale state, board and list icons, dark and light, keyboard and screen reader | Counts and states correct; matches DESIGN 5.8; no color-only meaning; a11y checks pass |
| GIT-13 | Latency: ticket bootstrap with dev summary; webhook to UI | Server p95 within the 50 ms budget with no provider call on the read path; event visible within 2 s p95 of receipt |
| GIT-14 | Write actions: create branch from issue (template, base, conflicts, permissions), PR back-link comment | Works on both providers; failures surfaced; disabled without the write scope |
| GIT-15 | Self-hosted: GitHub Enterprise Server shape and GitLab self-managed (`gitlab-ce`), custom CA, base URL, API version differences | Works with a private CA; private addresses only when the connection allows them (SSRF suite in `egress`) |
| GIT-16 | Large events: force-push of 1,000 commits, monorepo, 100k-commit backfill | Capped and coalesced; memory and queue bounded; no effect on interactive latency |
| GIT-17 | Data minimization: no source or diffs stored; message truncation; secret-looking strings; GDPR erase of an author | Only the documented fields stored; erase removes the author's data |
| GIT-18 | Disconnect, uninstall, token revoked | Webhooks removed; keep-read-only or purge honored; health warning shown instead of silent failure |
| GIT-19 | TQL development fields with permission filtering | Correct results; no issue returned that the user cannot see |
| GIT-20 | MCP: `list_development`, `create_branch`, resource `tasklane://issue/{key}/development`; PR text in results | Respect scopes and gates; PR and commit text tagged as untrusted provenance |
| GIT-21 | Import and export bundles with dev links | Links round trip; no tokens or secrets in bundles; re-scan after Jira import rebuilds links |
| GIT-22 | Mirror mode | Git links attach to mirrored issues by Jira id, key and aliases; rules and smart commits never change mirrored issues; nothing written to Jira |
| GIT-23 | Deployments and releases: environments, merged PRs on a version, deployment as condition or trigger | Correct per environment; release notes include the right PRs |
| GIT-24 | Provider conformance: equivalent fixtures produce the same neutral model and UI for GitHub and GitLab | Identical results from one shared contract suite; a new provider must pass it |

### 5.13 Email (outbound and inbound)

**SMTP is mocked.** Outbound cases run against an in-process mock SMTP server in Go tests (every commit: TLS modes, AUTH,
certificate failure, 4xx and 5xx at each stage, connection drop after `DATA`, slow server; the test asserts on the
recorded transcript and raw message) and against `mailpit` in the Compose `mail` profile (PR: rendering, threading and
the full notification path through the app, inspected by the mailpit API). No test sends real mail. Once a night one
message goes through a real relay to a seeded test mailbox and is read back by IMAP, to cover deliverability and DKIM
(MAIL-20) and to check the mock against reality. Inbound uses a real IMAP server (`greenmail`), not a mock.

Runs in the Compose `mail` profile: `mailpit` (SMTP mock with API), `greenmail` (IMAP and SMTP server for inbound mailboxes),
a provider-webhook fake for SES, SendGrid, Mailgun and Postmark shapes, and an injector that sends crafted raw messages
(including DKIM-signed ones). Corpora: `mail-replies` (recorded replies from Gmail, Outlook desktop, web and mobile, Apple
Mail, Yahoo, Thunderbird, plain text, in several languages, top- and bottom-posted, with signatures and disclaimers),
`mail-hostile` (spoofed, malformed, oversized, looping and auto-generated messages).

| Id | Case | Pass criterion |
|---|---|---|
| MAIL-01 | Outbound configuration: SMTP with implicit TLS, STARTTLS and no TLS, authentication, certificate verification failure, wrong credentials; provider API mailers; test-send | Clear errors, no plaintext credentials downgrade, working send on each transport |
| MAIL-02 | Threading per issue in recorded Gmail, Outlook and Apple Mail threading rules; summary rename; issue moved to another project | One thread per issue, no split after rename or move; headers (`Message-ID`, `In-Reply-To`, `References`) as specified |
| MAIL-03 | Content: plain and HTML parts, locales and time zones, dark-safe HTML, no remote images or tracking, `List-Unsubscribe`, `Auto-Submitted`, reply marker | Renders in the client matrix; passes an accessibility and spam-score check |
| MAIL-04 | Recipient rules, preferences, quiet hours, digests; internal comments and restricted issues never sent to those without access; permission change between event and send | No leak; visibility evaluated at send time |
| MAIL-05 | Queue: retries with backoff, provider outage, rate limits, duplicate suppression after crash and retry | Exactly one message per event and recipient; recovers after outage |
| MAIL-06 | Bounces: hard, soft and complaint via DSN to the VERP address and via provider webhooks; suppression, admin and user indicators, clearing | Correct classification; suppression applies to email only; actions audited |
| MAIL-07 | Reply by email: valid, expired, forged and wrong-recipient tokens; sender mismatch; forwarded notification; token key rotation | Comment only for token plus matching sender; others rejected or quarantined |
| MAIL-08 | Create issue by email: project address, plus-tags for type and labels, reporter mapping, `Cc` watchers, attachments; unknown sender policies (reject, quarantine, external reporter) | Issue created as specified under each policy; thread continues on reply |
| MAIL-09 | Inbound transports: IMAP polling and IDLE, UIDVALIDITY change, provider webhooks (signature check), SMTP listener; duplicates; backlog of 10,000 messages; crash during processing | No message lost or processed twice; raw message stored before acknowledgement |
| MAIL-10 | Spoofing: forged From, failed or missing DKIM, DMARC misalignment, lookalike domains, display-name tricks, forged `Authentication-Results` from an untrusted path, header injection | Quarantined or rejected per policy; no tracker change; no backscatter reply |
| MAIL-11 | Loop protection: out-of-office, `Auto-Submitted`, `Precedence`, mailing lists, bounces, our own mail (instance header and counter), two Tasklane instances replying to each other | No loop; per-sender, per-domain, per-issue and per-project limits enforced |
| MAIL-12 | **Quote stripping** on the `mail-replies` corpus | At least 98% of replies yield exactly the new text; false-strip rate (new text removed) at most 0.5%; inline replies kept whole; raw message always linked |
| MAIL-13 | Content safety: HTML sanitization, scripts, remote content, `cid` images, oversized and nested MIME, zip bombs, EICAR, type spoofing | Safe subset only; limits enforced; infected attachments blocked |
| MAIL-14 | Limits: message and attachment size, attachment count, mail bomb from one sender | Rejected with a recorded outcome; service stays responsive |
| MAIL-15 | Encodings: UTF-8, ISO-8859-x, Shift-JIS, GBK, RFC 2047 headers, quoted-printable, base64, malformed MIME, huge headers | Correct text or a clean rejection; no crash or unbounded memory |
| MAIL-16 | Permissions: inbound comment as a user without Add Comments, issue invisible to the sender, closed or archived project, security level | Refused or quarantined; never widens access |
| MAIL-17 | Latency: outbound leaves within 30 s of the event; inbound visible within 10 s p95 (IDLE, webhook) | Meets targets under mixed load; request path never waits on SMTP | **covered** `TestMAIL17_*` (request path with a 400 ms relay; a 300-message burst through a 100 ms relay in 5 s; inbound p95 by webhook and by IDLE under outbound load) and `TestMAIL17_PERF_AnIMAPBacklogOfTenThousandMessages` (`make perf`: 10,000 messages in 2 m 41 s). Real-world latency depends on the relay and the mailbox server, which the doubles only approximate |
| MAIL-18 | Retention and GDPR: raw message purge, user erase, suppression retention | Removed from blob store and indexes; audit kept | **covered** `TestMAIL18_*` (purge after the period and not before, blob removed, row keeps recognising a redelivery, quarantine and legal hold exempt, bounce and suppression expiry with active ones kept, erase at once with originals removed and the audit kept, export, erase refused under hold) |
| MAIL-19 | Mirror mode: Tasklane email off for mirrored projects; inbound mail to a mirrored project | Works; warning shown when Jira email is also on | **covered** `TestMAIL19_*` (no Tasklane email while mirrored with the inbox intact, a notice in the Jira status, email after cut-over; mail to a mirrored project held as "mirrored project", not releasable, nothing sent back). The warning is a standing notice, because Jira's own setting cannot be read |
| MAIL-20 | Deliverability: DNS checker for SPF, DKIM, DMARC; Tasklane DKIM signing verified by an independent verifier | Checker correct on valid and broken records; signatures verify |
| MAIL-21 | Quarantine and admin: review, release, delete, audit, outcome log | Release creates the comment or issue once; deletion audited |
| MAIL-22 | Service desk senders: unverified customer by email, customer reply, SLA effects | Follows PORT rules; customer-visible content only |

### 5.14 Install, configuration, upgrade and licensing

Uses the Compose `upgrade` profile for rolling and offline upgrades, a clean VM for the tarball, and `kind` (Kubernetes in
containers) nightly for the Helm chart.

| Id | Case | Pass criterion |
|---|---|---|
| INST-01 | Fresh install from each artifact: container with Compose, tarball with systemd, Helm on `kind` | `init` completes, administrator created, migrations applied, health green, smoke E2E passes |
| INST-02 | `tasklane doctor` against each fault: old PostgreSQL, missing `pg_trgm` or `unaccent`, no Redis with 2+ nodes, unwritable blob directory, SMTP and IMAP login failure, clock skew, low descriptor limit, proxy dropping WebSockets, egress blocked | Each fault named with a fix; a healthy system reports clean |
| INST-03 | Prerequisite matrix: current PostgreSQL majors 15 and newer, Redis and Valkey, S3 and MinIO, amd64 and arm64 | Suite passes on each supported combination |
| INST-04 | Air-gapped install and idle run | No outbound connection except configured targets; update check and telemetry off |
| INST-05 | Artifact integrity: signature verification, SBOM and NOTICE present and matching, reproducible build (two builds, identical hash) | Verification succeeds; mismatch fails the release |
| INST-06 | Sizing guide: the documented small and medium hardware profiles | Meet the targets (50 concurrent users, 1,000 idle connections per node, PERF-01..04) |
| CFG-01 | Precedence defaults, file, environment, flags; `*_FILE` secrets; unknown key with suggestion; type and unit errors; `config print --redacted` | Documented precedence; startup fails clearly; secrets never printed |
| CFG-02 | Generated configuration reference versus the schema | No drift (CI check) |
| CFG-03 | Hot reload of the safe subset; restart-only keys; invalid reload | Safe keys apply without restart; invalid reload rejected, old snapshot kept; restart-only keys say so |
| CFG-04 | Secrets in logs, crash dumps, diagnostics bundle, error pages | Never present |
| UPG-01 | Migration framework: expand and contract lint, `migrate plan` accuracy, single runner via advisory lock, checksum mismatch, lock times on the `scale` dataset | No long locks or rewrites in `expand`; maximum lock wait under 1 s; plan matches result |
| UPG-02 | Rolling upgrade N-1 to N under live load (50 users, API, MCP, Git webhooks, mail, an import) | Reliability rates hold (LIVE-09 counters), PERF within budget, jittered reconnect, no lost edit | | **partial (M5.27)** `make upgrade` (`internal/upgrade`, tag `upgrade`, `deploy/compose/docker-compose.upgrade.yml`): the previous release (commit 7cb3b49, built from a checkout, version 0.0.9) and the current one (0.1.0) as real containers behind a readiness-checking balancer; 12 simulated people (45% idempotent text saves, reads, searches, comments) and 4 jittered-reconnect sockets; expand migration under the old nodes, node-by-node replacement with a graceful stop; asserted: at least 99.5% of requests succeed after one retry, p95 under 1 s, no acknowledged save or comment lost, sockets back within 15 s, two versions live and the gate off in the mixed cluster, a pause working there, the gate on by itself afterwards with acknowledgements from both nodes. Run once: 5,759 requests, none failed, p95 6 ms, 7 socket reconnections, longest outage 6 s, 65 s in total. `make upgrade-bus` (M5.28) repeats it with the previous release that has the event bus (91a5aec), a shared Redis, 50 people and a node killed rather than stopped, adds an import during the mixed phase and checks in both directions that events and presence cross between the two releases: run once, 31,531 requests, none failed, p95 17 ms, 5 socket reconnections, longest outage 1.6 s, 102 s in total; without the shared Redis it fails. M5.29 adds to `make upgrade-bus`: six MCP agents through the balancer, a Redis outage of 6 s in the mixed phase (nodes ready, links return, events resume), 60 account emails each required exactly once through a relay both releases reach, and a PostgreSQL restart under the crowd (recovery asserted, failures in the window counted apart); run once: 58,396 requests, 2 failed after a retry (99.997%, p95 21 ms), 3,385 MCP calls, 8 socket reconnections; the same run found the indexer deadlock and the per-node MCP sessions (both fixed, below). M5.30 adds the LIVE-09 campaign (6 people appending to one shared text with conflicts and same-id retries; the SPEC 21 reconciler over the final text) and a Git provider sending signed push deliveries with retries and repeats through the balancer: over 3 runs of about 125 s each, 1,700 to 2,300 acknowledged appends with E1=E2=E4=0 and no gap, and about 1,800 acknowledged deliveries all stored and applied once. Not covered: the full 3x10^8-save sample size, delay and packet loss on the network, clock skew
| UPG-03 | Mixed N-1 and N cluster: cross-node fan-out, doc sync, cache invalidation, pause barrier, MCP sessions | Interoperate; unknown fields ignored; new behaviors stay off until `cluster_min_version` allows | | **partial (M5.23)** the pause barrier is covered by `TestUPG03_*`: a pause waits for another node's in-flight write and is reported only after it ends (the requesting node already refuses new writes), a node that never acknowledges aborts the pause at its deadline naming it, a dead node is not waited for, acknowledgements go with the lease; beside an older node the gate is off and the pause behaves as before, and turns on when that node leaves; mutation-tested. Event fan-out and presence across two releases are covered by `make upgrade-bus` (M5.28: releases 91a5aec and the current one, one shared Redis, both directions, a real import in the mixed cluster, a killed node); MCP sessions across nodes are covered by `TestMCP_SessionsContinueOnAnotherNode` and the upgrade run (M5.29: signed session ids any node accepts; a session of the old release is started again, which the protocol allows); cache invalidation (M5.40): `TestUPG03_AnotherNodesPolicyChangeIsHonoredAtOnce` (bus: within 2 s, set and cleared; mutation-checked), `TestUPG03_WithoutTheBusThePolicyCacheTimeOutIsTheBound` (no bus: in force after 5 s), `TestUPG03_UnknownBusMessagesAreIgnored` (future message kinds); document sync across nodes is the compare-and-set on the database row (INTEROP-04, `make upgrade-bus` crowd) and has no version-specific path to test; the second-factor cache is covered by `TestUPG03_AnotherNodesSecondFactorResetIsHonoredAtOnce` (reset on one node, the other holds the person to enrolment within 2 s; mutation-checked)
| UPG-04 | Feature gate activation (automatic and admin-confirmed); rolling a node back after activation | Activation only when all nodes qualify; unsafe rollback refused with a reason | **covered** `TestUPG04_*` (version ordering; a gate off until every live node qualifies and then on by itself; a stopped node and a node that left do not hold it back; nobody registered means off; a risky gate pending until an administrator confirms, back to off when an old node joins; audited; an active gate stays on) and the real binary `TestUPG06_...` step 5 (the server refuses to start older than an active gate, naming it). UPG-03 (a real mixed N-1/N cluster) is not covered: no earlier release exists |
| UPG-05 | N-2 to N offline upgrade; N-3 attempt; contract migrations only in `finalize` | Supported path works; unsupported refused with the stepping path |
| UPG-06 | Rollback inside the expand-only window; after `finalize`; node outside its schema range | Previous version runs inside the window; after `finalize` requires restore (pause mode); out-of-range node refuses to start |
| UPG-07 | Rich-text schema version change across N-1 and N | Documents saved by N-1 validate on N and the reverse within the skew window; sanitizer differences never change stored content silently |
| UPG-08 | Client skew: cached N-1 PWA against N server; client below the minimum; offline mutation queue created on N-1 | N-1 works in the window; too-old client told to reload; queued mutations replay or surface "needs attention", none dropped | **partly covered** `TestUPG08_*` (previous client served, older one gets 426 with the reload hint, requests without the header and the page itself never refused, headers on every answer; mutation-tested) and e2e `UPG-08` (the page shows the reload prompt, keeps the draft, restores it). Not covered: a cached PWA shell (there is no service worker) and an offline queue of mutations (the page keeps one draft per field, not a queue of changes)
| UPG-09 | Data format compatibility: bundles, backups, update log and snapshot encodings from the previous two releases | Import, restore and read succeed | **bundles covered** `TestUPG09_*` (a fixture for every `BundleVersion`, so raising it without one fails; every released version's JSON and CSV bundle still imports and reads back as the file says; additions from a later release (a field, a column, a manifest key, an unlisted file) are ignored; a newer version is refused before anything changes; a tampered file is refused; mutation-tested against a strict reader). **Backups covered too:** a committed version-1 backup that every release must restore (`TestUPG09_EveryBackupVersionHasAFixtureAndStillRestores`), a simulation of an older-schema backup restoring into a newer schema with an added column and table (`TestUPG09_ABackupFromAnOlderSchemaRestoresIntoANewerOne`), and a guard that raising `BackupVersion` needs a fixture. There is no update log or separate snapshot encoding; only version 1 exists, so "the previous two releases" is not yet exercised across a real change
| UPG-10 | Single-node upgrade | Pause under 30 s; editors keep drafts and saves retry; banner shown | **partly covered** `TestUPG10_*` (status announces a pause, a snapshot job is not announced, writes get 503 with Retry-After and the paused code, reads continue, the same write goes through after) and e2e `UPG-10` (banner, "Paused for maintenance, draft kept", the text stays in the editor, the banner clears and the save completes by itself). Not covered: the 30 s pause of a real single-node upgrade (nothing yet takes the pause during `migrate` or a restart)
| UPG-11 | Renamed or removed configuration keys | Mapped with a warning for one minor release |
| UPG-12 | Failure injection: migration fails midway, node crash during drain, database unavailable | No partial schema; documented recovery works; no lost edit |
| LIC-01 | Dependency licence scan (Go modules, npm, Cargo, base images) against the allowlist and denylist | Build fails on any violation |
| LIC-02 | NOTICE file and SBOM generation | Present in every artifact and complete |
| LIC-03 | Bundled versus external audit | No GPL, AGPL, SSPL, BUSL, Elastic or RSAL code linked or bundled; PostgreSQL, Redis or Valkey, MinIO, ffmpeg, ClamAV, GitLab CE never shipped |
| LIC-04 | Fonts and icons licence and attribution | Licences recorded; attribution present |
| LIC-05 | Self-hosted build has no licence-key check, seat enforcement or phone-home | Zero outbound connections at idle; hosted-only code inactive |
| LIC-06 | Names and trademarks in UI and packages | Third-party marks used only nominatively |
| HOST-01 | Hosted configuration: tenant isolation, plan quotas, metering accuracy, limit errors, region selection | Isolation per SEC-09; metering within tolerance; clear errors | **covered (M5.22)** `TestHOST01_*`: a self-hosted installation has no plans, limits, snapshots or endpoints; plan administration, region refusal, audit; seats and projects enforced by the database on every path (screens, SCIM, reactivation; agents and customers are not seats; another organization unaffected; a freed seat is usable; clearing lifts them); storage limit with numbers and inactive when hosting is off; metering equals the facts (seats, projects, issues, storage, active customers by 30-day window), one snapshot a day rewritten while open; mutation-tested. M5.32 adds prices and invoices (`TestHOST02_*`: a month's snapshots priced exactly with half-up rounding per line and the average of seats and storage and the peak of customers, another month excluded, prices kept with the invoice, one live invoice a month and a void one remade, signed delivery with an idempotency key that can be retried, access, absent on a self-hosted installation; mutation-tested). M5.33 adds support levels, upgrade channels, maintenance windows and notices (`TestHOST03_*`: terms validated, notice lead time by level, the weekly window, length limit, overlap, cancellation freeing the slot, only the tenant's own people told and only within two weeks, absent on a self-hosted installation; mutation-tested). M5.34 makes both do what they announce (`TestHOST04_*`, `TestHOST05_*`, `TestPAUSE_OrganizationScope`): maintenance holds the tenant's organization (and no other) from its start to its end, once on three simultaneous nodes, lets go when cancelled, is announce-only on request and retries a blocked claim with the reason; the month's invoice is made and delivered once it is over, retried when delivery failed, not repeated; the organization pause scope and its overlap rules; mutation-tested. A payment provider integration, a plan and invoice screen and real multi-region operation are not built |

### 5.15 Issue lifecycle and work calendars

| Id | Case | Pass criterion |
|---|---|---|
| LIFE-01 | Customize the lifecycle: new issue types with hierarchy, statuses, transitions, per-type workflows through schemes | Issues follow the configured workflow; screens and fields per type apply |
| LIFE-02 | Transition semantics: conditions, validators, in-transaction and asynchronous post-functions; failure rollback; concurrent transitions on one issue | Atomic; one concurrent transition wins and the other gets a clear error |
| LIFE-03 | Publish a workflow with in-flight migration (10,000 issues, editors active); mapping for removed statuses; abandon a draft | No stuck issue; not-yet-migrated issues keep working; progress shown; history correct |
| LIFE-04 | Convert type: status mapping, dropped-field values kept, hierarchy rules, bulk | Plan equals result; converting back restores removed values |
| LIFE-05 | Move one issue: new key, `OLD-12` resolves in URL redirect, REST, TQL, MCP, Git linking, email subject and token, webhooks | Alias works everywhere; history, notifications and dev links correct |
| LIFE-06 | Bulk move with subtasks, epic, sprint, security level, components and versions, permissions; partial failures; dry run | Plan equals result; failures reported per issue; none half-applied |
| LIFE-07 | Move while others edit | Editors get a notice and keep their drafts; a save after the move follows the issue; no lost text (counted in LIVE-09) |
| LIFE-08 | Clone option matrix: attachments shared by reference, independent copy of the text, subtask recursion and cap, clone into another project, status reset, nothing copied that must not be | Matches SPEC 9.1; blob refcounts correct |
| LIFE-09 | Delete to trash: hidden from lists, boards, backlog, search, reports, dashboards, counts and TQL; key reserved; link placeholders; subtask and epic handling; open editors get a notice and keep drafts | Invisible everywhere; numbers never reused |
| LIFE-10 | Restore: previous state and key, sprint rule, permissions, subtasks | Matches SPEC 9.1 |
| LIFE-11 | Purge: scheduled, manual with permission, legal hold, GDPR immediate path; children, documents, history and blobs released | Data gone and blobs collected; audit kept; agents cannot purge |
| LIFE-12 | Archive: read-only, hidden by default, found by filter, reversible, auto-archive after N days | Behaves per SPEC 9.1 |
| LIFE-13 | Races: move vs transition, delete vs edit, double move, clone of an issue being edited, convert vs move | Defined winner; no corrupted issue; documents consistent |
| LIFE-14 | Audit, events, webhooks, automation triggers, notifications for every operation; MCP exposes no delete or purge | Events complete and ordered |
| LIFE-15 | Mirror mode: convert, move, clone, delete and restore unavailable on mirrored issues; Jira-side delete and key change | Behavior and warnings as specified; compatibility check lists limits |
| LIFE-16 | Bulk move, delete and clone of 10,000 issues as a resumable job; node killed mid-job | Resumes without duplicates; interactive p95 within budgets; progress and ETA shown |
| LIFE-17 | Import and export: trash excluded by default and optional; aliases and key map round trip | Round trip preserves aliases and removed-value records |

**Work calendars** (extends CORE-06; rows are CORE ids)

| Id | Case | Pass criterion |
|---|---|---|
| CORE-13 | Calendars: Mon-Fri, Sun-Thu, Mon-Sat and custom working days; week start Sunday, Monday and Saturday; time zones including half-hour and 45-minute offsets and daylight-saving changes (forward, back, at midnight); year-boundary week numbering (ISO W53 and W01, first-day-of-year system, none); naming tokens; regional and custom holidays; per-user capacity across calendars; calendar change mid-sprint; burndown ideal line, days left and time-tracking hours; auto-sprint and rollover with the fake clock | Boundaries at local midnight on the correct start day; working-day counts and capacity correct; closed-sprint reports unchanged by later calendar edits |

### 5.16 Service desk portal identity

| Id | Case | Pass criterion |
|---|---|---|
| PORT-01 | **M5.6, code and link, optional password: built. M5.20: customer OIDC and SAML built per portal (PORT01_SSO_*: administration, SAML and OIDC sign-in ending in a customer session only, domain and portal rules, mutation-tested; e2e).** Customer authentication: one-time code and magic link (expiry, reuse, brute force limits), optional password, customer OIDC and SAML | Works as specified; throttled; codes single-use |
| PORT-02 | **M5.6: built** (domain, the matrix, e2e). Session isolation: customer session against staff UI, REST and MCP; staff session against the portal | Neither crosses; separate cookies and lifetimes |
| PORT-03 | **M5.7, M5.10: own requests, hidden fields and comments, key guessing, other-organization, customer organizations, sharing with the company and with named people, revoking at once (ORG01..03, e2e): built.** Visibility: own requests, shared requests, customer organization; internal fields, comments, approvals, worklogs and Git data; key and id guessing | Only customer-visible data; enumeration and direct-object attacks fail |
| PORT-04 | **M5.12: customers are never among the people or the seats, and promotion adds exactly one (PRIV04); hosted metering open.** Seats: customers in staff counts and hosted metering | Never counted as staff seats; active customers metered separately where configured |
| PORT-05 | **M5.6: limits, honeypot, block and allow lists built. M5.18: attachment rules built (PORT05 files, rules, erase; e2e); M5.19: sign-in proof-of-work check built (PORT05 SignInProofOfWork, JS hash checked against a reference, e2e); a third-party CAPTCHA widget is not built.** Abuse controls: email verification, rate limits, CAPTCHA, honeypot, block and allow lists, attachment rules | Spam and floods stopped; legitimate use unaffected |
| PORT-06 | **M5.12: built (PRIV01..04, e2e): export, erase with the search of every column, delete_content, legal hold, repeat, scope by organization, promotion that keeps history, customers never counted as seats.** Promotion to staff; customer GDPR export and erase | History kept on promotion; export and erase complete |
| PORT-07 | **M5.11: built (PORT07 domain tests, e2e): confirmation, public reply, resolved, shared-request mail, verified addresses only, send-time recheck, replies by signed address, threading and subject, proof of origin, release from quarantine, no request creation.** Notifications and email replies for customers | Customer-visible content only plus portal link; replies follow MAIL rules |
| PORT-08 | **M5.9 queues and canned responses built (QUE01..03, e2e). M5.7, M5.8: request types and forms (REQ01..03) and SLA clocks (SLA01..04, the calendar tests in `internal/calendar`, e2e) built; reports and warnings open.** Request types, forms and SLA clocks (start, pause, resume, breach, calendar-aware) with the fake clock | Clocks correct across pauses, calendars and holidays |

### 5.17 Real Jira verification (occasional, before a major release)

**When:** run on demand, once in a while, before a **major** release (and optionally after a Jira API change notice or a
large change to the mirror's Jira client). It is not part of any PR, nightly or minor-release pipeline. It is a
checklist item of the major-release gate (section 7): a major release does not ship until the latest run passed on
the release candidate, or an exception is recorded.

**Why:** everything else uses `jira-fake`. A fake can only be as right as its recordings, so this run answers two
questions the fake cannot: does Tasklane behave correctly against what Jira really returns today, and does the fake
still match Jira.

**Setup:** the `jira-sandbox` Jira Cloud site (test accounts only, credentials from the CI vault, available only to this
job). The run creates its own project (key `T<run id>`) with a seeded, small project set, works within a request budget,
backs off on `429`, and deletes everything on teardown; a janitor removes leftovers older than a day. Real Jira cannot
be made to fail on demand, so fault cases stay on `jira-fake`; here real limits and latencies are only observed.
Webhooks need a reachable URL: the job starts a small public webhook relay that forwards to the app; if the relay is
unavailable the webhook cases are reported as **not run** (the polling path is the source of truth) and the release
owner decides whether to wait.

| Id | Case | Pass criterion |
|---|---|---|
| JREAL-01 | Backfill of a seeded project (custom fields, comments, attachments, links, sprints, versions, changelog); a kill and resume | Counts and field values equal Jira (MIR-01 on real data, project of a few hundred issues, not 10,000); resumes without duplicates |
| JREAL-02 | Incremental sync: edit, comment, transition, delete and move issues in Jira during the run; webhook and polling paths | Changes arrive within the MIR-02 freshness targets; deletes and key changes handled (MIR-02, MIR-03) |
| JREAL-03 | Jira REST import into a native project; dry run, commit, re-import, rollback by import id | IMP-01, IMP-05..07 pass on real data |
| JREAL-04 | Jira-importable CSV export imported by Jira's own CSV importer | EXP-04 passes |
| JREAL-05 | Permission mapping with the real roles, groups, security levels and comment restrictions of the sandbox, using accounts with different access (a mirrored issue must never be visible beyond Jira's rules) | MIR-06 passes for every grant type the sandbox can express; unsupported grants hidden and listed |
| JREAL-06 | ADF from real Jira: a set of issues authored in Jira's editor covering every node type the editor can produce | MIR-05 and IMP-04 pass; no content dropped; unknown nodes opaque |
| JREAL-07 | Read-only enforcement and cut-over on a real mirrored project | MIR-07 and MIR-11 pass; nothing is written to Jira (verified from Jira's audit log and issue histories) |
| JREAL-08 | Attachments from real Jira: lazy fetch, size cap, deleted attachment | MIR-10 passes |
| JREAL-09 | Real rate limits and latency observed during the run | Tasklane honors any `429` and `Retry-After` it meets; the UI stays within budget; the observed limits are recorded in the report |
| JREAL-10 | **Fake versus real contract check:** replay the exchanges recorded from `jira-fake` (REST requests, webhook payloads, permission responses) against the real sandbox and compare shapes and values | No difference that changes Tasklane behavior; every difference is either fixed in the fake or recorded as an accepted deviation with an owner; the fake's recordings are refreshed from this run |

**Output:** a report attached to the release candidate: cases run, not run (with reason), requests used against the budget, differences found between fake and real, and fixes made to the fake. A defect found here is severity 1 or 2 by the usual rules (section 8) and gets a regression test against `jira-fake` before it is closed, so it is caught on every PR from then on.

## 6. Pipeline and gates

| Stage | Contents | Gate |
|---|---|---|
| Commit | Format, lint, unit, short property/fuzz, contract, merge corpus, bundle-size budgets | All green, under about 10 minutes |
| Pull request | Integration, E2E smoke (including a two-user conflict), **real-service interoperability smoke with Redis and OpenSearch (INTEROP-01..06, section 3.3)**, visual, axe, security scanners, mirror tests against `jira-fake`, mail against the SMTP mock | All green; visual diffs reviewed |
| Nightly | Long property/fuzz, full E2E, chaos subset, load subset on `perf`, real-relay mail smoke, real GitHub, AWS S3, mock contract checks, soak (weekly) | Trend report; regression opens an issue |
| Release candidate | Full perf (all PERF and LOAD), full chaos, upgrade and DR, manual a11y, security review, import/export compatibility | Exit criteria below |
| Major release candidate, extra | Real Jira verification (5.17), run manually once, in addition to the row above | JREAL-01..10 passed or excepted; see release criteria |

Flaky tests are quarantined within a day and fixed within a week; a quarantined test cannot cover a release criterion.

## 7. Entry and exit criteria

**Per milestone exit (SPEC 22)**

| Milestone | Must pass |
|---|---|
| M1a Tracker (audit in STATUS.md: not yet passed) | LIVE-01..07, LIVE-10..17, DUR-01..06, PRES-01..07, HIST-01..08, PERF-01..13, CHAOS-01..03, CORE-01..03, CORE-07..09, LIFE-01..02, TQL-01..04, TQL-06, TQL-21..27, PERF-16..17, SEC-01..03, SEC-05, UI-01..05, A11Y-01..02, BLOB-01..02, BLOB-04..09, BLOB-12, BLOB-20, BLOB-23, MAIL-01..03, INST-01..03, INST-05, CFG-01..04, UPG-01, LIC-01..02, LIC-05 |
| M1b Lifecycle and exchange | LIFE-04..11, LIFE-13..14, LIFE-16..17, LIVE-08, LIVE-18, EXP-01 and basic IMP (generic CSV), PAUSE-01..04, PAUSE-06, PAUSE-10, PAUSE-12 |
| M1c Agents | MCP-01..03, MCP-05..06, MCP-10..11, MCP-13, MCP-17 |
| M1 exit | M1a, M1b and M1c each passed their gate above, in that order; M1 is done only when all three have |
| M2 Agile | CORE-04..06, CORE-11..13 (reports and calendars), PERF-04, LOAD-01..04, BLOB-03, BLOB-10..11, BLOB-17, BLOB-21..22, GIT-01..07, GIT-12..13, GIT-15..16, GIT-24, TQL-08..13, TQL-15..17, MIR-01..14, PERF-15, LIFE-15, MCP-16 |
| M3 Fields | CORE-03 in full (all types, contexts, behaviors), TQL-04..05, EXP-05, LIFE-03, LIFE-12 |
| M4 Enterprise | TQL-14, TQL-18..20, MAIL-04..19, MAIL-21, UPG-02..12, INST-04, INST-06, LIC-03..04, SEC-12, GIT-08..11, GIT-14, GIT-17..20, PAUSE-02, PAUSE-05, PAUSE-07..09, PAUSE-11, BLOB-13..16, BLOB-18..19, MCP-04, MCP-07, MCP-09, MCP-12, MCP-14, MCP-20, SEC-04, SEC-07, SEC-09, SEC-10, CORE-10, CHAOS-04..08, LOAD-05..07, upgrade and DR |
| M5 Ecosystem | MAIL-20, MAIL-22, PORT-01..08, HOST-01, LIC-06, GIT-21..23, MCP-08, MCP-15, MCP-18..19, IMP-01..09, EXP-02..04, A11Y-03..04, I18N-01, OFF-01..03 |

**Release criteria (any release)**

- Zero open defects of severity 1 or 2; no regression against the previous release in any PERF p95 beyond the gate.
- All PERF targets met on `perf` hardware, or a documented, approved exception.
- Concurrency and merge suites (LIVE-01, LIVE-04, LIVE-05) clean on the long nightly run for 7 consecutive days.
- LIVE-09 campaign completed at the required size with zero E1, E2 and E4 errors and the draft model within E3 on the release candidate build; production
  E1 to E4 rates for the previous release within the 30 day budgets (SPEC 21).
- Backward compatibility: bundles and database migrations from the previous two releases apply cleanly.
- **Major releases only:** the real Jira verification (5.17) has passed on the release candidate, with the fake refreshed from
  it and every difference resolved or recorded; once the mirror ships, no major release goes out without it.

## 8. Defects

| Severity | Definition | Response |
|---|---|---|
| 1 | Data loss or corruption, security or permission leak, a save that silently overwrote someone else's newer change | Stop-ship, fix immediately, post-incident review |
| 2 | Target missed by more than 2x, feature unusable, import or export wrong results | Fix before release |
| 3 | Degraded behavior with a workaround, visual defects, single-locale issues | Scheduled |
| 4 | Cosmetic | Backlog |

Every severity 1 or 2 defect gets a regression test (a document triple added to `merge-corpus` for merge bugs) before it is closed.

## 9. Reporting

- A dashboard per run: pass rate by level, perf p50/p95/p99 vs budget with trend, convergence run counts, flake rate,
  coverage of the traceability table in section 4.
- Release report: criteria checklist, exceptions with owners and dates, open risks.
- Field telemetry (RUM) compared monthly with lab numbers; a gap over 30% triggers a lab-profile review.

## 10. Risks to the plan

| Risk | Mitigation |
|---|---|
| Conflicts happen more often than expected and the dialog is painful | Conflict rate and draft-recovery telemetry (DESIGN 12.9); presence warnings reduce collisions; real-time co-editing can be revisited, nothing in the data model prevents it |
| Lab latency does not predict real networks | Throttled profiles, RUM comparison, dedicated LAN `perf` environment |
| The fake drifts from real Jira between verifications | Fake recorded from the real sandbox; the pre-major-release run replays and compares, and must pass before a major release; run it also after any Jira API change notice or a change in the mirror's Jira client |
| Jira sandbox rate limits, outages or shared state | Real Jira is used rarely and on purpose; per-run project, request budget, a "budget exhausted" failure; a sandbox outage marks the run "not run", never green, and a major release waits for it |
| A mock drifts from the real system | Contract replay against the real system nightly; a failing replay blocks the mock update |
| Non-deterministic time-dependent tests (sprints) | Injectable clock everywhere; no sleeps in tests |
| Large datasets slow CI | Generate from seed on cached volumes; `scale` only on `perf` |
| Visual baseline churn | Per-theme baselines, review required, tolerance only for font rendering |

## 11. Open items

- Choose the concrete tools per level (section 2) at the start of M1; keep the gates, not the products.
- Decide the reference `perf` hardware profile and the reference mid-range phone profile.
- External penetration test (SEC-11) is postponed; decide when to schedule it, and revisit before any public or multi-tenant hosted offering.

### 11.1 Usage budget stop (rule for the implementing agent)

The account has a weekly usage limit. A status-line script (`~/usage-statusline.sh`) records it in
`~/.claude/usage.log` and keeps the flag file `~/.claude/usage-pause.flag` while weekly use is at or above 50%.

- **Before starting each milestone**, check whether the flag file exists (`test -e ~/.claude/usage-pause.flag`).
- **If it exists**, do not start the milestone. Finish and commit only what is already in progress, and only with the tests
  green (never commit on red to beat the limit). Then write one line in STATUS.md ("paused at weekly N%, next: ...") and
  stop, telling the user that it is waiting. Do not work around the flag, delete it, or edit the script.
- **If the file is missing**, continue as usual. A missing or stale `usage.log` means the status line is not installed,
  not that the budget is fine; mention that once in STATUS.md.
- The flag clears on its own when the weekly window resets. The user can resume, or raise the limit with `USAGE_PAUSE_AT`.

## 12. Implementation status

Which planned tests exist as automated tests in the repository. Updated at the end of each milestone (see STATUS.md).
"Partial" means the case is covered at the domain level but not yet end to end through the HTTP API or the browser.

| Plan id | State | Test |
|---|---|---|
| LIVE-01 | partial (domain) | `TestLIVE01_CompareAndSetExactlyOneWinner` (50 users, one winner), `TestLIVE01_SequenceHasNoGapsUnderRetriesToFreshSeq` (no lost acknowledged save, gapless `text_seq`) |
| LIVE-02 | partial (domain) | `TestScalarFieldsMergeAcrossFields` |
| LIVE-03 | partial (domain) | `TestConflictFlow_KeepMineIsRestorableAndRecheckedAgainstLatest` (dialog payload, repeat on a third save, keep mine restorable) |
| LIVE-04 | partial | `internal/text` merge tests (block and word level, identity, disjoint inserts); corpus file not yet built |
| LIVE-05 | partial (domain) | `TestLIVE05_IdempotentRetry` |
| LIVE-07 | done (domain) | `TestLIVE07_APIWrites` |
| LIVE-10 | partial | `TestLIVE10_Drafts` (private, stale detection, removed on save); 10 s client cadence, expiry and cap pending |
| LIVE-13 | partial | `TestLIVE13_Sanitization` (text format only; no rich-text schema yet) |
| LIVE-14 | done (domain) | `TestLIVE14_TextSavesNeverBumpVersion` |
| LIVE-16 | partial (domain) | `TestScalarFieldsMergeAcrossFields` |
| HIST-01, HIST-02, HIST-08 | partial | `TestHIST_RecordsAndRestore` |
| PRES-01..04 | partial (domain) | `TestPRES_AdvisoryPresence` (heartbeat, expiry on an injected clock, appear offline, never blocks) |
| CORE-01 | done (domain) | `TestCreateAndKeys`, `TestKeysAreUniqueUnderConcurrency` |
| CORE-02 | done (domain) | `TestWorkflowTransitions` |
| CORE-07 | done (domain) | `TestComments` |
| SEC-01..03 | partial | `TestPermissions` (non-member invisible, viewer read-only, admin-only project creation) |
| SEC-05 | partial | `TestAuthAndTokens` (hashed sessions and API tokens, logout), `TestHTTP_LoginCookieAndCSRF` (HttpOnly, SameSite, CSRF header guard, security headers) |
| LIVE-01 (HTTP) | done | `TestHTTP_LIVE01_FiftyUsersOneWinner`: 50 concurrent PUTs through the real server, one 200 and 49 `409` carrying the current text |
| LIVE-07 (HTTP) | done | `TestHTTP_TextWriteModes`: patch, ambiguous patch (409), stale conditional replace (412 with current text), fresh and bare replace |
| LIVE-14, LIVE-16 (HTTP) | done | `TestHTTP_ScalarIfMatchAndMerge`: strict `If-Match` gives 412, `base_version` merges different fields and returns 409 for the same field |
| LIVE-15 | partial | `text_seq` and `version` returned by the view endpoint; MCP and events pending |
| PERF (server side) | partial | `Server-Timing` on every response and ETag revalidation (`TestHTTP_ViewETagAndPermissions`); no benchmark yet |
| PRES-01..04 (HTTP) | partial | `TestHTTP_PresenceAndDrafts` |
| UPG-01 | partial | `TestUPG01_MigrateIsIdempotentAndRecorded`, `TestUPG01_ConcurrentNodesMigrateOnce`, `TestMigrationsAreNumberedWithoutGaps`; upgrade from the previous release pending (no release yet) |
| UPG-05 (part) | partial (no floor yet: no release) | `TestUPG05_ADatabaseOlderThanTheFloorIsRefusedWithTheSteppingPath`, `TestUPG05_ContractMigrationsWaitForFinalizeAndKeepTheOrder` (held until finalize, order kept, phase recorded) |
| UPG-07 | done | `TestUPG07_PreflightAndTheFinalizeGuard`: preflight on fresh/migrated databases, an older live node named, a newer one blocks (exit 1); `migrate finalize` refuses beside a live older node, `--force` overrides, a node that stopped reporting does not block. `upgrade wait` times out naming an older node, returns once only current nodes are live. Guard and wait conditions mutation-checked |
| UPG-06 | done for a single binary (no pause mode, no real older binary) | `TestUPG06_ANodeOutsideItsSchemaRangeRefusesToStart` (empty, in step, behind, inside the expand-only window, finalized, unclassified), the real binary `TestUPG06_TheServerRefusesToStartOutsideItsSchemaRange` (serve and doctor in each state, `migrate finalize`, `migrate plan`) |
| UPG-11 | done (nothing renamed yet) | `TestUPG11_RenamedAndRemovedSettingsAreMappedWithAWarning` (file, environment, flag, `*_FILE` secret, new name wins, layers keep their precedence, removed keys, unknown after the window) |
| UPG-12 (part) | partial | `TestUPG12_AMigrationThatFailsMidwayLeavesNothingBehind` (no partial schema, not recorded, the fix applies, an unreachable database is an error with a reason); a crash during drain is not tested |
| CFG-01..04, INST-01 | partial | `TestCFG_DoctorReportsBrokenEnvironmentWithoutPanicking`, `TestCFG_DoctorHealthyInstall`; configuration reference document pending |
| TQL-01 | done | `TestTQL01_GoldenQueries`: about 60 queries over a fixed dataset (fields, operators, `currentUser()`, dates and functions, `ORDER BY`, text, `WAS`, `CHANGED`); `internal/search/tql` grammar tests |
| TQL-02 | done | `TestFuzzRandomInput` (2 s of random and mutated input, hang detection per input), `FuzzParse` (native fuzzer), `TestLimits` (nesting, clause count, length) |
| TQL-03 | partial | `TestParseErrorsArePositionedAndClear`, `TestTQL03_SemanticErrorsAreClear` (clear positioned errors, SQL injection attempts are values); a real-world JQL corpus is not yet collected |
| LIFE-02 | done (partial: no approvals, update-parent, fire-event, automation) | `TestLIFE02_ConditionsHideAndRefuse`, `_ValidatorsAndTransitionScreen`, `_PostFunctionsAreAtomic`, `_ConcurrentTransitionsOneWins` (12 racers, one wins), `_AsyncPostFunctions` (retry, signature, sealed secret, egress refusal, idempotent subtask) |
| LIFE-03 | done (partial: Jira mirror not covered) | `TestLIFE03_CheckingADraft`, `_PublishRenamesSwapsAndAddsKeepingIds`, `_RemovingAStatusMigratesInFlightIssues` (editors active, progress, resume), `_TenThousandIssuesMigrate`, `_TemplatesImportExport`, e2e `workflows.spec.js` |
| EXP (M3) | done for custom fields and workflow rules | `TestEXP_CustomFieldsAndWorkflowRulesRoundTrip` (json and csv, every type, contexts, roles, re-import), `TestEXP_FieldOfAnotherTypeIsReportedNotOverwritten`, `TestEXP_WorkflowRulesTravelWithTheWorkflow` |
| CORE-10 | done (partial: no manual or webhook triggers, no throttle test) | `TestCORE10_InboxRecipientsWatchersAndSelf` (recipients, watchers, mute, auto-watch, replay creates no duplicates), `_PreferencesMuteKindsAndChannels`, `_NotificationSchemePerProject`, `_InboxRespectsPermissions` (internal comments, revocation, trash), `_InboxStates`, `_QuietHoursDelayMailNotTheInbox`, e2e `inbox.spec.js`; automation: `TestCORE10_AutomationRunsActionsAsAutomationAndOnlyOnce` (conditions, smart values, actor label, replay), `_AutomationLoopsAreCut` (self loop and a two-rule cycle), `_AutomationFailureStopsTheRuleAndIsRecorded` (validators apply), `_AutomationDryRunChangesNothing`, `_AutomationScheduledRulesRunOncePerTick` (every N minutes, daily in a time zone), `_AutomationRulesPermissionsAndValidation` (scopes, sealed secret), e2e `workflows.spec.js` |
| SEC-07 | done (partial: erase path pending) | `TestSEC07_AuditLogIsAppendOnlyAndChained` (database refuses change; tamper and removal detected), `_ConcurrentWritersKeepOneChain` (40 writers), `_QueryFilterPageAndExport`, API `TestSEC07_EveryAdministrativeChangeIsAudited`, `_OrdinaryWorkListHasNoStaleEntries`, `_AuditRecordsWhoWhatAndHow`, e2e `workflows.spec.js` |
| LIFE-12 | done (partial: reports with archived issues not separately tested) | `TestLIFE12_ArchiveIsReadOnlyHiddenFindableAndReversible`, `_ViewersAndOutsidersCannotArchive`, `_AutoArchiveByProjectPolicy`, `_ArchiveSurvivesExportAndImport`, `TestLIFE12_ArchivedIssuesLeaveTheIndex`, e2e `fields.spec.js` |
| TQL-04 | done | `TestTQL04_CustomFieldQueries` (typed operators, quoted names, `cf[id]`, was, changed, ORDER BY, restricted fields); OpenSearch fallback in `TestTQL08_Routing` |
| CORE-03 | partial | `TestCORE03_*` (types, contexts, defaults, required, roles, uniqueness, history, conflicts, trash, merge, replace, warnings, convert and clone), `TestHTTP_CORE03_CustomFieldsEndToEnd`, e2e `fields.spec.js`; `TestCORE03_ScreensAndSchemes`, `TestCORE03_FieldConfigurationRequiredHiddenPrecedence`, `TestCORE03_BehaviorRules`, `TestRuleConditions`, e2e behavior rules; field-level permissions and trash are covered by the M3.1 tests |
| TQL-06 | done | `TestTQL06_PermissionFiltering`: scope injected into every compiled query, internal comments hidden from viewers (also through `!~`), revoked access effective at once, typeahead scoped |
| TQL-07, TQL-10, TQL-27 | partial | `TestTQL07_IndexFollowsEditsAndTrash`, `TestTQL10_27_IndexerIsIdempotentAndSkipsUnchangedText` (replay of all events changes nothing; unchanged text is not rewritten); GIN bloat and 1 million update runs pending |
| TQL-21 | done | `TestTQL21_PlanShapes` (`make perf`): GIN, trigram and btree plans on the 100,000-issue dataset |
| TQL-22 | done | `TestTQL22_TokenizationContract` (keys, UTF-8, error codes, paths, emails, versions, CamelCase, accents, CJK, emoji) with documented limitations pinned |
| TQL-24 | partial | `TestTQL24_RankingAndPaging`: summary above description, frequency, explicit order wins, deterministic paging; rank window and snapshot tokens pending |
| TQL-26 | partial | internal and public comment matching in `TestTQL06_PermissionFiltering`; thousands of comments per issue pending |
| PERF-16, PERF-17 | done (single machine) | `TestPERF16_17_SearchLatencyAt100kIssues` (`make perf`). Measured p95: text 62 to 137 ms, key lookup 2 ms, filter list 46 ms, comment match 74 ms, typeahead 66 ms; budgets 400 ms and 150 ms. Reference `perf` hardware not yet defined (TESTPLAN section 11) |
| BLOB-01 | done (fs) | `TestBLOB01_PasteScreenshotAndAsyncThumbnail`: path-free name, thumbnail generated by the worker, original intact |
| BLOB-02 | done (fs) | `TestBLOB02_ResumableUploadAfterDroppedConnection`, `TestHTTP_BLOB02_ResumableOverHTTP`, and `TestHTTP_BLOB02_TwoGiBConstantMemory` (`make perf`: 2 GiB dropped and resumed, SHA-256 verified, measured peak heap 3 MiB against a 128 MiB bound) |
| BLOB-04 | done | `TestHTTP_BLOB04_RangeETagAndHeaders`: tail and middle ranges, 416, ETag and 304 |
| BLOB-05 | done (fs) | `TestBLOB05_DownloadAuthorization`, `TestHTTP_BLOB05_SignedLinksAndBlockedFiles`: tamper, expiry, outsider, trashed issue, forced download and sandbox CSP for active content; the separate origin is configuration (`BlobOrigin`) and is not yet exercised |
| BLOB-06 | done | `TestBLOB06_DeduplicationAndRefcount` |
| BLOB-07 | done | `TestBLOB07_AbandonedUploadsAreRemoved`, and no staging debris after refusals in BLOB-08 |
| BLOB-08 | partial | `TestBLOB08_SpoofedTypesBombsAndEICAR`: spoofed type flagged, EICAR (also across a chunk boundary) blocked, zip bomb refused, class limits, empty file; an external scanner and the `scanning` state under load are pending |
| BLOB-09 | partial | `TestBLOB09_ThumbnailFailureFallsBackAndRetries`; video posters and log indexes are M2 |
| BLOB-12 | partial | `TestBLOB12_23_FsckAndReferenceIntegrity`: bit-rot, deleted object and orphan detected, repair; verify-on-read pending |
| BLOB-20 | partial | `TestBLOB20_UploadCapacityIsBounded` (bounded upload slots, interactive calls unaffected); the mixed-load latency run needs the load harness |
| BLOB-23 | done | same test: traversal and key-injection locators rejected, no `bytea` column in any blob table |
| LIVE-02, LIVE-03 (browser) | manual only | Driven by hand in Chrome with two users: non-overlapping change merged automatically, overlapping lines shown as a conflict block with a chooser, Save merged result stored as a `merge` version, the other user's version kept in history. Not yet automated |
| LIVE-11 (partial) | manual | Reload with a local draft restores it with a banner; the unsaved-changes guard blocks navigation. Tab kill, eviction and offline are untested |
| UI-01 (partial) | automated | `internal/web` tests: shell routing for client routes, ETag and 304, gzip, 404 for a missing asset, no `innerHTML`, `outerHTML`, `eval` or `document.write` in the app, bundle budget (SPEC 21: under 200 KB gzipped JavaScript), Node unit tests of the diff, choice application, relative time and link whitelist |
| MAIL-01 | done (mock) | `internal/mail` against the shared `internal/testutil/smtpmock`: none, STARTTLS and implicit TLS with a throwaway CA, AUTH PLAIN, STARTTLS as the default mode; untrusted certificate refused with nothing sent; a server that drops STARTTLS is refused (no plaintext fallback, no credentials in clear); plaintext AUTH to a non-local host refused by the client; wrong credentials permanent; 4xx retryable and 5xx permanent at greeting, MAIL, RCPT, DATA and end; bounded time against a hanging server; lost acknowledgement; connection refused. Provider API mailers and the admin test-send are pending (M5 and M4) |
| MAIL-02 | done (mock) | `TestMAIL02_ThreadingPerIssue`: root id, deterministic Message-ID, In-Reply-To the previous message of the issue, References starting with the root; rename keeps the thread. Recorded Gmail, Outlook and Apple Mail grouping and the move-to-another-project case are pending |
| MAIL-03 | partial (mock) | `TestMAIL03_NotificationContent`, `TestMAIL03_MessageFormat`, `TestMAIL03_HeaderInjectionIsImpossible`: plain and HTML parts, quoted-printable, UTF-8 subject, escaped HTML with no remote content, `Auto-Submitted`, `List-Unsubscribe`, reply marker, header injection impossible. Locales and time zones, the client matrix and the spam-score check are pending. The `mailpit` PR run (full path through the running app) is not set up yet; the mock covers the protocol and failures, mailpit will cover rendering |
| MAIL-04 (core) | partial | `TestMAIL_InternalCommentsNeverReachViewers`, `TestMAIL04_VisibilityIsRechecked_AtSendTime`, `TestMAIL_RecipientsAndSelfNotification`; preferences, quiet hours and digests are M4 |
| MAIL-05 (core) | partial | `TestMAIL05_QueueRetriesBackoffAndIdempotency`, `TestMAIL05_PermanentFailureAndGiveUp`, `TestMAIL05_ConcurrentSendersSendEachMessageOnce`: exponential backoff on the injected clock, give up after 8 attempts, one row per event and recipient under replay, five concurrent senders send each message once. Known limit: a lost acknowledgement after DATA is retried and the recipient can get a duplicate; SMTP cannot prevent that |
| MAIL-08 | partial (no attachments, no external reporter) | `TestMAIL08_CreateIssueByEmail` (summary, description, reporter, default type, plus-tags for type and labels, Cc watchers and a non-user Cc, thread continues on reply, empty and long subjects, an unknown project address, project address shown), `TestMAIL08_16_UnknownSendersAndPermissionsForNewIssues` (quarantine and reject policies, administrator-only setting, a viewer cannot create by mail or by release, release of an unproven message creates once as the sender) |
| SEC-09 | partial (see docs/ORGANIZATIONS.md: shared global configuration, instance-level SSO, SCIM, Jira, Git and audit, the operator sees all) | `TestSEC09_CrossTenantSweepEveryRouteAsAPersonOfAnotherOrganization` (every route of the matrix as a member and as the administrator of another organization: no data of A in any answer, no table of A changed), `TestSEC09_TheSweepCanFail` (the control), domain `TestSEC09_OrganizationsAreCreatedByTheInstanceAdministratorOnly`, `_NothingCrossesTheLineInMembershipListsOrAdministration` (membership, triggers, the view, lists, search scope, administration), `_PeopleAreCreatedInTheCreatorsOrganizationAndNeverBecomeInstanceAdministrators`, `_InboundMailDoesNotCrossOrganizations`, realtime `TestSEC09_WebSocketsDoNotCrossOrganizations`, MCP `TestSEC09_MCPAgentsDoNotCrossOrganizations` |
| TQL-17 | partial | the sweep runs every search and TQL route as a person of another organization (the issue list and typeahead among them); the OpenSearch tenancy filter of SPEC 10.1 is not built because organization is not yet a term in the index (the project filter, which is organization-bound, applies) |
| MAIL-06 | done (partial: one generic provider shape, no bounce-rate display) | `TestMAIL06_EveryMessageHasItsOwnReturnPathAndAHardBounceSuppressesTheAddress` (distinct VERP per message, hard bounce, held-back mail logged as suppressed, inbox untouched, user indicator, administrator-only audited clear and mail resuming), `_SoftBouncesCountInAWindowAndComplaintsSuppressAtOnce` (threshold, the window moving, delayed ignored, complaint), `_ForgedMisdirectedAndUnsignedReportsDoNothing` (altered token, report about another address, no return path, a person writing there, revoked key), `_ProviderEventsAreSignedAndOnlyForAddressesWeMailed`, `TestMAIL06_ParseReport` |
| MAIL-07 | done (partial: no `Delivered-To` forwarding case with a real mail client) | `TestMAIL07_ReplyByEmailCommentsAsTheSenderAndNothingElseDoes`, `_ForwardedForgedExpiredAndRevokedAddressesNeverComment` (forward, forged, expired, rotation, revoked key), `_FallbacksWhenTheAddressIsLost` |
| MAIL-09 | partial (webhook, IMAP and the SMTP receiver; no 10,000-message run, no real server or MTA) | webhook: `TestMAIL09_14_RawKeptBeforeAckLimitsAndRecovery`, `TestMAIL09_WebhookTransportVerifiesTheSignature`, API `TestMAIL09_21_InboundWebhookAndQuarantineOverHTTP`; IMAP client: `TestMAIL09_IMAPClientLoginSelectSearchFetch`, `_IMAPClientRefusesWhatIsUnsafe` (bad login, line breaks, egress policy, no password in clear to a remote host, TLS against plaintext), `_IMAPIdleWakesOnNewMailAndTimesOut`; IMAP source: `_IMAPReadsFromACheckpointAndEachMessageOnce`, `_IMAPUntrustedMailboxNeedsDKIMAndTheEgressPolicyApplies`, `_IMAPCrashesAndFailuresLoseNothingAndRepeatNothing` (dropped connection mid-fetch, re-read after a lost checkpoint, store failure, removed message, two nodes at once), `_IMAPUIDValidityChangeReadsAgainWithoutDuplicates`, `TestMAIL09_14_IMAPOversizedMessagesAndABacklog` (2,500 messages, about 68 per second), `TestMAIL09_17_IMAPIdleDeliversWithinSecondsAndStopsWhenRemoved` (52 ms), `_IMAPServerWithoutIdlePollsOnItsInterval`; e2e `MAIL-09: add an IMAP mailbox in the Email tab, see its state, remove it`; SMTP receiver: `TestMAIL09_SMTPListenerDeliversSignedMailAndAnswersEveryRefusalInTheConversation` (delivery, envelope headers, duplicate, a sender-written verdict ignored, relay and unknown mailbox refused, archived project), `_SMTPListenerHandlesBouncesAddressedToTheReturnPath`, `_SMTPListenerProtocolAbuse` (order, syntax, limits, error cap, long line), `_SMTPListenerSizeLimitDotStuffingAndKeepingTheConversationInStep`, `_SMTPListenerAcknowledgesOnlyWhatItKept`, `_SMTPListenerStartTLSAndConnectionLimits`, the real binary `TestMAIL09_TheServerListensForMailWhenConfigured` |
| MAIL-10 | partial (no DMARC policy, SPF or forged header from an untrusted *path*) | `TestMAIL10_NoProofOfOriginNoAction` (untrusted path, no verdict, failed DKIM verdict, lookalike and different domains, display-name trick, two senders, no backscatter), `TestMAIL10_DKIMVerifiesMessagesSignedByAnotherImplementation` (dkimpy fixtures: rsa and ed25519, simple/relaxed, folded headers, LF), `_DKIMRefusesEverythingThatIsNotProof` (body, header or From changed, damaged signature, no key, revoked, testing mode, wrong key, expired, `l=`, From not signed, no resolver), `_DKIMLimitsWorkAndAlignment`, domain `TestMAIL10_DKIMProvesOriginWithoutATrustedReceiver` (aligned accepted from an untrusted source, misaligned and altered quarantined, DNS failure, untrusted Authentication-Results ignored) |
| MAIL-11 | done | `TestMAIL11_AutomaticMailAndLoopsAreIgnored` (ten header cases, out-of-office subjects, mailer-daemon, our own notification fed back, per-sender limit and the window moving); per-domain and per-issue limits are in the code but have no test of their own |
| MAIL-12 | partial | `TestMAIL12_QuoteStrippingCorpus` (25 cases, 98% exact and no lost text required): the corpus is hand-written, the plan's `mail-replies` corpus does not exist yet |
| MAIL-13 | partial | `TestMAIL13_ContentIsReducedToSafeText`, `TestMAIL13_14_HostileStructureStaysBounded` (nesting, 5,000 parts, 3 MiB text, attachments counted and never read as text); EICAR and zip bombs need attachments, which are not kept yet |
| MAIL-14 | partial | `TestMAIL09_14_RawKeptBeforeAckLimitsAndRecovery` (oversized message and huge headers rejected with a recorded outcome); attachment size and count, and a bomb from one sender over time are open |
| MAIL-15 | done | `TestMAIL15_CharsetsAndTransferEncodings` (UTF-8, ISO-8859-1/15, Shift-JIS, GBK, quoted-printable, base64 with breaks and without padding, unknown charset, invalid UTF-8, bare LF), `_HeadersAreDecodedByTheHeaderReader` |
| MAIL-16 | done (partial: no security levels) | `TestMAIL16_InboundMailNeverWidensAccess` (viewer, no access, release by an administrator with the sender's rights, archived issue, unknown sender) |
| MAIL-21 | done (partial: no outcome-log export) | `TestMAIL07_...`/`TestMAIL10_...` (release once, delete, audit, non-administrators refused), API `TestMAIL09_21_...`, e2e `MAIL-21: review and release a quarantined email in the Email tab` |
| INST-01 | partial | A fresh install from the Compose file was run for real: image built, migrations applied, administrator created, `/readyz` green, `doctor` clean, login 200 (found and fixed the non-root volume permission bug). The Helm chart (`deploy/helm/tasklane`) is built and its rendering is tested in `internal/helmtest` (valid objects, install-time guards, settings the application defines, secrets from Secrets, hardened pod); it has not been installed on a real cluster (`kind`). Tarball with systemd is not built; the run is manual, not yet a CI job |
| MIR, IMP-01, IMP-08 | open (M2) | No Jira code yet. When `internal/jiramirror` lands: `jira-fake` first; the real Jira verification (5.17) is set up before the first major release that includes the mirror |
| UI-02, UI-03, A11Y-01 | open | Dark and light themes checked by eye on the ticket view; phone layout, keyboard-only flow, screen reader and axe runs are not done |
| LIVE-09 (mini) | partial | `TestLIVE09_MiniCampaign`: 12 simulated users, 180 saves through the real HTTP server, about 25% of acknowledgements dropped after the commit, retries with the same save id, conflicts resolved by re-saving on the current text. Reconciler result on every run: E1 = E2 = E4 = 0, gapless `text_seq`, every save applied exactly once (about 320 conflicts and 155 dropped acknowledgements per run). Since then: node kills, Postgres restart, delay, loss and duplication and clock skew on real nodes (M5.29 to M5.36), and **M5.37: `make soak DURATION=...`** (`TestLIVE09_Soak`, tag `soak`): rounds of 24 people on shared texts through the real HTTP server with a quarter of the acknowledgements lost, reconciled per round, plus editing sessions with server drafts, lost acknowledgements and random crashes judged on recovery (E3), with the rule-of-three bound printed; a 90 s run: 34,320 saves (39,896 conflicts, 48,749 dropped acknowledgements), 20,351 editing sessions, E1=E2=E3=E4=0, no gaps, rates below 8.7e-05 and 1.5e-04 at 95%, which is 0.011% and 0.68% of the SPEC 21 sample sizes; the campaign fails when the server's save-id idempotency is removed (E4 and gaps in every round). **M5.38: the editing-session draft model in a real browser** (`drafts_e3.spec.js`: typing without a pause, killed at random by tab close, renderer crash or reload, recovery compared with the editor's contents at every moment; loss windows 130 to 2,000 ms against an allowance of 3,500 ms; the same test on the previous page lost everything in 6 of 6 sessions). Still not run at 3x10^8 saves and 3x10^6 sessions: about nine days of one machine at 380 saves a second |
| LIVE-09 (E3 at scale, M5.42) | partial | `E3_SESSIONS=120 npx playwright test drafts_e3`: 120 browser sessions (close, crash, reload), none lost more than the 3.5 s allowance, none lost the draft; rule of three: under 2.5 % per session at 95 %. The SPEC 21 sample (3x10^6 sessions) is not run |
| LIVE-09 (soak, M5.43) | partial | `make soak DURATION=60m`: 1,045,440 saves, 589,765 editing sessions, zero E1/E2/E3/E4 errors and no gaps; below 2.87e-6 (saves) and 5.09e-6 (sessions) at 95 %; 0.35 % of the saves and 19.7 % of the sessions SPEC 21 asks for |
| LIVE-12 | done (reconciler) | `TestLIVE12_ReconcilerDetectsInjectedErrors`: injected lost save (E1), stale overwrite (E2), duplicate application (E4), sequence gap and extra application are all detected, with no false positive on a clean history. The injection into a live system with production counters is pending |
| DUR-01 | done (ack loss) | the dropped acknowledgements in the mini campaign are exactly "committed, not acknowledged"; the retry with the same `save_id` applies once. A real process kill is not yet scripted |
| CHAOS-03 | partial | lost acknowledgements only; latency and packet loss at the network level need the `chaos` Compose profile |
| PERF-01, 03, 07, 10, 11 (server time) | done (single machine) | `TestPERF_ServerSideLatencyAt100k` (`make perf`, in-process handler, 100,000 issues). Measured p95: ticket view 1.1 ms, ETag revalidation 1.0 ms, list page 28 ms (project filter) and 37 ms (empty query), transition 2.0 ms, assign 2.0 ms, comment 1.5 ms, text save 2.3 ms, `409` with merge 15 ms (100 KB) and 66 ms (about 1 MB). Budgets 50 ms and 100 ms. The browser-side halves (300 ms to usable content, PERF-02, 06, 08, 09, and 12, 13 which need the event channel) are not measured |
| CFG-01 | done | `internal/config` tests: precedence default, file, environment, flag; `_FILE` secrets; secrets masked in `config print` and absent from errors; unknown keys, variables and flags with a suggestion; type and unit errors, all reported at once |
| CFG-02 | done | `TestCFG02_ReferenceMatchesSchema`: the table in `docs/CONFIGURATION.md` is generated from the schema (`tasklane config docs`) and a test fails on drift |
| CFG-03, CFG-04 | open, partial | hot reload not built; secrets masked in output and errors, crash dumps and diagnostics bundle not checked |
| INST-02 | partial | `TestINST02_MissingExtensionsOnAFreshDatabase` (extensions, schema, PostgreSQL version), `TestINST02_SMTPFaultsAreNamedWithAFix` (untrusted certificate, wrong credentials, wrong TLS mode, relay down; probe sends no mail), `TestINST02_UnwritableBlobDirectoryAndHealthyReport`; each fault names its fix |
| INST-05 | partial (signature, hashes, SBOM and NOTICE matching and reproducibility done; no key distribution, no image signature, no SPDX) | `TestINST05_ReproducibleTarballs` (`make dist` twice, amd64 and arm64, identical hashes), `TestINST01_TarballContents` (normalized mtimes and owners, binary, NOTICE, SBOM, unit file, SHA256SUMS); signing: `TestINST05_AGoodReleaseVerifiesAndSaysWhatItChecked`, `_AnAlteredUnsignedOrMisattributedReleaseIsRefused` (flipped byte, replaced archive and list, no or junk or truncated or foreign signature, planted and missing archives, hostile checksum lists), `_ACorrectlySignedReleaseThatDoesNotMatchItsOwnContentsIsRefused` (SBOM or NOTICE not matching the binary, wrong architecture or version, missing files, links and escaping entries, two top directories, not a Go binary), `_KeysRoundTripAndTheSignatureIsStandardEd25519` (verified by OpenSSL), `_TheRealSignedReleaseVerifiesAndTamperedCopiesDoNot` (the real release, two builds signed identically, verified by the binary from its own archive) |
| LIC-01, LIC-02 | done | `TestLIC01_LinkedModulesAreAllowed` (every linked module classified against allow and deny lists; seven modules, MIT and BSD), `TestLIC02_NoticeIsCurrent` (`make notice`) |
| LIC-05 | done | `TestLIC05_NoPhoneHome`: no HTTP client, dial, telemetry or licence-key code in production sources apart from the configured SMTP relay; the web assets load nothing from another origin |
| PRES-01, PERF-12 | done (socket) | `internal/rt` tests: presence change reaches another person in p95 2.8 ms over 40 changes; leave on tab close and expiry on the injected clock are broadcast; the HTTP fallback reaches socket clients |
| PERF-13 | done (single node) | `TestPERF13_ChangesReachOpenPagesQuickly`: scalar change, comment and text-saved events to another open socket, p95 6.6 ms, each carrying actor, `version` and `text_seq` |
| SEC-02 | done | `TestSEC02_HandshakeAndAbuse`: forged, missing, expired, reused tickets; cross-origin handshake 403; oversized, malformed and binary frames; flood cut off by the rate limit; unknown operations and channel names; server healthy afterwards |
| PRES-05 (partial), SEC-01 (WS) | partial | `TestPRES05_SEC02_ChannelAuthorization`: forbidden and nonexistent channels answer identically, presence without a subscription refused, events stop after revocation |
| PRES-06 | partial | `TestPRES06_TrafficOnlyOnStateChange`: 50 connections, 250 unchanged heartbeats cause 0 messages; two joins cause 2 changes x 50 subscribers |
| LIVE-17 | partial | `TestLIVE17_EventsAreBestEffort_SavesNeverDependOnThem`: a dead subscriber never blocks 300 writes; saves work with no hub |
| LIVE-02, LIVE-03, PRES-01 (browser) | manual | the live path was driven by hand in Chrome: Arkady's presence ring, "is editing the description" note, new comment and priority change appeared on Jana's open page with no action; the background-tab refetch was found by this run and fixed |
| Browser suite | `make e2e` | `e2e/` (Playwright, system Chrome, the real binary on a fresh database, 25 tests, two runs in a row clean): `conflict.spec.js` (LIVE-02, 03, A11Y-02 dialog), `drafts_presence.spec.js` (LIVE-11, PRES-01, PRES-04, live updates, realtime fallback with the socket blocked), `ui.spec.js` (A11Y-01 axe in both themes on six screens, UI-03, UI-04 at three widths, keyboard-only flow, TQL in the UI, UI-01 baselines). Uncaught page errors fail every test. Not yet in CI; the plan's PR stage needs a job for it |
| DUR-01, DUR-02, CHAOS-01 | done (single node) | `TestCHAOS01_DUR01_KillTheServerMidSave` (`cmd/tasklane`, builds and runs the real binary): SIGKILL x3 mid-campaign, restart, same-id retries; E1 = E2 = E4 = 0, gapless `text_seq`, draft intact; about 250 client retries per run |
| UPG-01 | partial | `internal/store` tests: `TestUPG01_RealMigrationsPassTheLint`, `TestUPG01_LintRejectsDangerousExpandMigrations` (ten rejected patterns and six accepted), `TestUPG01_PlanMatchesWhatMigrateDoes`, `TestUPG01_ModifiedAppliedMigrationIsDetected` (and legacy adoption), `TestUPG01_MigrationFailsFastWhenTheLockIsHeld`, `TestUPG01_ConcurrentIndexMigrationRuns`, `TestUPG01_ConcurrentNodesMigrateOnce` |
| UI-02 | done | `TestUI02_TokenContrast`: 23 pairs per theme, text 4.5:1 and components 3:1, validated against black on white (21:1) and DESIGN's 12.3 and 16.2 |
| HIST-03 | done | `TestHIST03_MentionsNotifyOnce`: a comment mention gives one specific mail (not also the generic one), an email address and an unknown name are not mentions, a description mention notifies once and saving again keeps quiet, a person without access and an internal comment mentioning a viewer send nothing, a public mention of a viewer does |
| HIST-04 | done | `TestHIST04_EditNoticesAreBatchedPerPerson`: six saves in a burst give one notice after the window, nothing before it, a later save starts a new window, two editors give two notices, the editor is never told about their own edit |
| HIST-05 | done | `TestHIST05_TrackedFieldSetting`: admin only, known fields only, an untracked field leaves no record and keeps the old ones, re-enabling applies going forward, text versions survive for conflicts and restore |
| SEC-01 | done (REST) | `internal/api/matrix_test.go`: `TestSEC01_EveryRouteCoveredByTheMatrix`, `TestSEC01_AuthorizationMatrix` (230 checks), `TestSEC01_NoExistenceLeakInErrors`, `TestSEC01_ListsAreScoped`. The plan asks for a matrix generated from the permission scheme; the permission scheme here is the three project roles plus global administrator, and the table is the written form of it |
| LIFE-04..11, LIFE-13, LIFE-14 (core) | see STATUS.md, M1b.1 audit | `internal/domain/lifecycle_test.go` (hierarchy rules, `TestLIFE09_DeleteToTrash`, `TestLIFE09_WhoMayDelete`, `TestLIFE10_RestoreBringsBackEverything`, `TestLIFE11_PurgeAndRetention`, `TestLIFE04_ConvertType`, `TestLIFE05_06_MoveWithAliases`, `TestLIFE06_MovePermissionsAndBlockers`, `TestLIFE08_CloneOptionMatrix`, `TestLIFE08_CloneSubtaskCap`, `TestLIFE13_Races`, `TestLIFE14_AuditAndEvents`) and `e2e/tests/lifecycle.spec.js` (plan dialog, trash and restore, deletion under an open editor, move and old-key redirect, follow on move, clone, convert, viewers have no menu); the authorization matrix covers the new routes |
| EXP-01, EXP-03, LIFE-17, IMP-05..07, PAUSE-01 | done | `internal/domain/bundle_test.go`: `TestEXP01_RoundTripJSONAndCSV`, `TestLIFE17_TrashOptionalAndHistoryRoundTrip`, `TestPAUSE01_SnapshotExportWhileUsersSave` (50 writers; the export holds exactly the saves the outbox recorded up to its marker), `TestEXP03_ExportRespectsPermissions`, `TestEXP_CorruptBundlesAreRejectedWholesale`, `TestIMP05_06_07_DryRunIdempotencyRollbackAndCollisions` |
| IMP-09 (generic CSV), SEC-05 (CSV export) | partial | `internal/domain/import_csv_test.go`: mapping, row errors, idempotency, templates, formula-looking cells round trip as data |
| PAUSE-04, 06, 07, 08, 10, 12 | done | `internal/domain/pause_test.go` and `TestHTTP_PAUSE04_WritersGet503WithRetryAfter`; the authorization matrix covers every new route |
| MCP-01 | done | `TestMCP01_ProtocolConformance` (initialize, version negotiation, list, errors, transport rules, origin, auth, resources), `TestMCP01_StdioTransport`; no reference client or Claude Code run yet |
| MCP-02 | done | `TestMCP02_ToolSchemaSnapshot` against `internal/mcp/testdata/tools.golden.json` (`UPDATE_GOLDEN=1` after an additive change) |
| MCP-03 | partial | `TestMCP03_AuthorizationMatrix` (every tool x missing scope, wrong scope, outsider, viewer, member; outsider errors identical to not-found); security levels do not exist yet; REST equality is by shared domain calls |
| MCP-05 | done | `TestMCP05_ClaimLeaseRace` |
| MCP-06 | done | `TestMCP06_EditIssueTextWhileAHumanTypes` (presence display not covered) |
| MCP-10, 11, 13 | done | `TestMCP10_SecretsAndSensitiveFields`, `TestMCP11_LimitsBudgetsAndCaps`, `TestMCP13_AuditCompletenessAndImmutability` |
| MCP-17 | done | `TestMCP17_ContextLatency` (p95 under 100 ms including HTTP) |
| MCP-07, 08, 14 | done | `TestMCP07_HumanGates`, `TestMCP08_RequestHumanInput` (board visibility is search only; no board yet), `TestMCP14_KillSwitchAndAgentPause` |
| MCP-04, 12, 15, 16, 18, 19, 20 | open | not built |
| CORE-13 | partial (calendar plus sprints) | `internal/calendar` tests: presets and custom weeks, half-hour and 45-minute zones, DST forward and back, skipped midnight, ISO W53/W01 and first-day numbering, naming tokens, holidays, per-day hours, capacity across calendars. Open: regional holiday lists, calendar change mid-sprint, auto-sprint and rollover with the fake clock, burndown ideal line |
| PERF-04 | partial | `TestPERF04_BoardOf500Issues` (service layer: p95 about 40 ms for the board, about 6 ms per drag); usable-content time needs the UI |
| SPRINT (SPEC 7) | partial | `internal/domain/sprints_test.go`: auto-sprints and names, calendar change, lifecycle with scope and carry-over, dry-run equals result, completion destinations, rollover job with the fake clock (two nodes), ranking and planning rules, capacity and commitment across calendars, closed sprints never change, parallel sprints, estimate round trip; `TestHTTP_SprintFlow`; matrix rows for all sprint routes. Reports (burndown and the rest) are M2.4 |
| BOARD UI (SPEC 6, 7.1) | partial | `e2e/tests/agile.spec.js`: drag and keyboard moves, a blocked move's reason, WIP breach as text, quick filters, no-active-sprint hint, planning with the move control, inline create, start and complete with preview and carry-over badge, reports with data tables, axe on board, backlog and reports in both themes, no sideways page scroll at 390 px. Visual baselines, realtime updates and presence on cards are open |
| REPORT (SPEC 7.3) | partial | `internal/domain/reports_test.go`: a scripted week (plan, start, finish, add, remove) checks every burndown and burnup sample, the sprint report lists, velocity, a closed sprint staying put after later edits, cumulative flow, created vs resolved, control chart, workload, time in status, filters, trash, epic burndown; `TestHTTP_ReportsFollowTheBoardFilter`; matrix rows. Version burndown, report performance at scale, and rendering are open |
| BOARD (SPEC 6) | partial | `internal/domain/boards_test.go` (create, columns, WIP, unmapped, rank per board, rebalance, transitions, gates, lanes), `internal/api/boards_test.go` (TQL filter, quick filters per person, bad filter, move over HTTP), matrix rows for all board routes. The plan has no dedicated board ids yet; they should be added with the UI tests |
| BLOB-01 (s3), 06, 07, 08, 12 on S3 | partial | `internal/domain/blob_s3_test.go`: screenshot and thumbnail via ranged reads, dedupe and GC, 12 MiB multipart, outage leaves nothing visible and a retry succeeds, fsck finds rot, missing and orphans, EICAR and an S3-to-S3 bundle round trip; the API authorization matrix now runs on the S3 backend |
| BLOB-03 | partial | `internal/domain/blob_direct_test.go` (single and out-of-order multipart, nothing visible before completion, truncated, tampered, bad part, zip bomb and missing-upload refusals with cleanup, sniff, scan, dedupe, limits, authorization, expiry and abort, filesystem backend refusal), `e2e/tests/agile.spec.js` (the browser protocol and the fall back), CSP test. Browser-to-bucket CORS against a real bucket is open |
| BLOB-11 (S3 part) | partial | `internal/blob/s3_test.go` (throttling retries with backoff, 403 not retried, outage gives a clear error, signatures and clock skew checked by the double), `TestCFG_DoctorChecksTheS3Backend`; readiness and the UI state for a store outage are open |
| BLOB-21 | open | needs real MinIO and AWS; the in-process double is the unit tier only |
| GIT-01..03, 05, 06, 08, 09, 12 (partial) | partial | `internal/domain/git_test.go`: signatures and replays for both providers, size and source-address limits, credentials encrypted and shown as fingerprints, linking rules, panel summary builds and deployments, out-of-order convergence over every arrival order, large push cap, kept failures, payload dropped after use, GitHub and GitLab backfill with read-only requests, ETags, pagination and rate limits, egress policy; `internal/egress` tests; `TestHTTP_GitWebhookToPanel`; matrix rows; e2e `GIT` ticket section. Real providers, GitHub App, TQL fields, write actions, smart commits and automation are open |
| GIT-17 | done (partial: a safety net of known shapes, not a scanner) | `internal/git` `TestGIT17_RedactRemovesCredentialsAndLeavesOrdinaryTextAlone` (twelve secret shapes, twelve ordinary strings untouched); domain `TestGIT17_OnlyTheDocumentedFieldsAreStoredAndSecretsNeverAre` (no body, no payload, no secret in any Git table; truncation at 300; links survive redaction), `_ErasingAPersonRemovesWhatGitKnewAboutThem` (authorship by the person and by a linked account, accounts, commands, titles scrubbed, export) |
| GIT-19 | done (partial: not orderable, not indexed, no scale test, no private-repository filter) | `TestGIT19_DevelopmentFieldsInTQLWithPermissionFiltering` (every field and operator, re-run replacing a build, the newer failed deployment, combinations, error messages, an outsider finding nothing, a viewer seeing what a member sees, restricted and trashed issues); OpenSearch routing `os_test.go` (the fields go to PostgreSQL) |
| GIT-14 | done on the doubles (partial: real providers, App installations, no screen) | `TestGIT14_CreateABranchFromAnIssue` (off by default and nothing sent, template, base, numbered names, explicit conflict, name validation, permissions, webhook round trip, audit and log), `_SeveralRepositoriesNeedAChoiceAndGitLabWorksToo`, `_APullRequestGetsOneBackLinkAndNothingFromTheIssue` (off, a refused write retried, once per pull request and issue, nothing from the issue, never on a backfill) |
| GIT-18 | partial (no provider-side webhook removal: Tasklane never creates it) | `TestGIT14_18_ATokenThatCannotWriteOrWasRevokedIsAWarningNotASilentFailure` (no write permission, a read not hiding it, rejected token, the check), `TestGIT18_DisconnectKeepsLinksReadOnlyOrPurgesThem` |
| GIT-20 | done on the doubles (partial: no `prompts`) | MCP `TestGIT20_ListDevelopmentTagsProviderTextAsUntrustedAndRespectsAccess` (provenance, the banner before provider text, scopes, outsiders, the resource and its template), `_CreateBranchNeedsTheScopeTheConnectionSwitchAndPermission` (scope, switch, idempotent retry, viewer), the tool-schema snapshot |
| GIT-UI (admin) | done | e2e `GIT: the Git tab manages connections, visibility, linked accounts and rules` (add a connection and see the one-time secret, token never shown, map a repository, visibility and override, link and unlink an account, smart commits, a rule through off/dry run/on and delete, accessibility); matrix row for the new smart-commits read |
| GIT-08 | done (partial: no mirrored or archived test, no deployment or review triggers, workflow conditions for pull requests not added) | `TestGIT08_RulesMoveIssuesFromCodeEventsAndSayWhoDidIt`, `_ADisallowedMoveIsSkippedAndReportedNeverForced` (incl. rule validation and project-administrator-only), `_ARuleRunsOncePerCodeObjectAndIssueSoNothingLoops` (replays, close and reopen, draft, a manual move), `_DryRunOffAndBackfillChangeNothing`, `_ReplayingHistoryNeverRunsRules` |
| GIT-09 | done (partial: `#time` unavailable) | `TestGIT09_SmartCommitsAreOffByDefaultAndRunAsTheMappedCommitter`, `_SmartCommitsIgnoreAndReportWhatTheyMustNotRun` (unlinked and unverified authors, branch setting, unknown and empty commands, `#time`, a transition that does not apply, a pull request description, a viewer, several keys and commands, audit of every command, `any`, a merged title as its author), `_ReplayingHistoryNeverRunsCommands`, `_GitLabPushesAreAttributedToThePusherOnlyWhenTheCommitIsTheirs` |
| GIT-10 | done | `TestGIT10_IdentitiesAreLinkedByAnAdministratorAndMapOnlyWhatTheProviderVouchesFor` (unverified email, provider-vouched email, a linked account, unlink, duplicates, audit); GIT-03 regression `TestGIT03_KeysSeparatedByOneSpaceAllLinkAndLongerWordsStillDoNot` |
| TQL-08..13, 15, 16 | partial | `internal/search/os_test.go`: routing, translator parity with PostgreSQL (50 queries x 4 roles), indexer versions, retries, outage backlog and dead letters, permissions while the index lags (revoked access, moved, trashed, internal comments, typeahead), online reindex with live writes, resume, verification refusing a damaged index, rollback, project reindex, reconcile, outage fallback and recovery, lag warning and own-edit overlay, paging, term limit; `TestHTTP_OpenSearchEngineAndAdminEndpoints`; doctor and matrix rows. TQL-17 (organizations), real OpenSearch, performance at scale and cursor paging are open |
| MIR-01..09, 11, 12 (partial), 13 | partial | `internal/domain/jira_test.go` (backfill with ADF, paged comments and changelog, links, parents, idempotence, kill and resume, webhook hints, overlap window, lost webhooks, out-of-order versions, key change aliases, deletions and the empty-answer guard, placeholder users and claiming, history idempotence and attribution, roles and groups, revocation, stale permissions failing closed, security levels, every write path refused, rate limits and outages with the stale marker, pause, read features on mirrored data, cut-over); `internal/jira/adf_test.go` (nodes, unsupported, hostile content, garbage, deep nesting); `TestHTTP_MirroredProjectIsReadOnlyWithATypedError`; `TestMCP_MirroredIssuesAreReadOnlyForAgents`; e2e `JIRA`; matrix rows. 10,000-issue backfill, attachments (MIR-10), real Jira, notifications inbox and erasure (MIR-14) are open |

Run everything with `make db` (once) and `make test`. Tests skip, with a message, when PostgreSQL is not reachable.
