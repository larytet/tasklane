# Tasklane - Implementation Status

Living record of what is built against SPEC section 22, the milestones inside each phase, issues found on the way and
how they were resolved. Updated at the end of every milestone, together with the "Implementation status" table in
TESTPLAN.md, after the suite is green and before the commit and push.

**Working rule.** A milestone is closed only when: its tests are written and pass (`make test`), TESTPLAN.md is updated,
this file is updated, and the work is committed and pushed.

## Phase overview

| Phase | State | Notes |
|---|---|---|
| M1a Tracker | **built, exit gate not passed** | milestones M1a.1 to M1a.9 below; see the exit gate audit for what is still open |
| M1b Lifecycle and exchange | not started | |
| M1c Agents | not started | |
| M2 to M5 | not started | |

## M1a milestones

| Milestone | Scope | State |
|---|---|---|
| M1a.1 Foundation | Go module, config, PostgreSQL store, migration framework (advisory lock, per-migration tx), three-way merge engine, test harness with per-test schema | done |
| M1a.2 Tracker core | users, sessions, API tokens, projects and roles, issue types, standard workflow, issues with keys, scalar fields with per-field versions, transitions, history, outbox | done |
| M1a.3 Text and concurrency | `text_seq` compare-and-set, idempotent saves, anchored patch, conditional and bare replace, versions and restore, drafts, comments, advisory presence, sanitization | done |
| M1a.4 HTTP API and server | REST + UI bootstrap endpoints, cookie and bearer auth with CSRF guard, security headers, `Server-Timing`, ETag revalidation, `tasklane serve`, `migrate`, `doctor`, `create-admin`, health and readiness | done |
| M1a.5 TQL and search | TQL parser (positioned errors, depth and size limits, fuzzed), compiler with mandatory permission scope, `issue_search` and `comment_search` maintained by an outbox indexer that skips unchanged text, ranking, typeahead, `WAS` and `CHANGED` on status, priority and assignee, `tasklane reindex`, 100,000-issue benchmark and plan-shape tests | done |
| M1a.6 Attachments | content-addressed `fs` blob store (no user input in paths), single-request and resumable uploads with constant memory, dedup with refcount and GC, type sniffing and spoof flag, class limits, zip-bomb refusal, built-in EICAR scanning, asynchronous thumbnails with failure fallback, range and ETag downloads, short-lived signed links, forced download for risky types, bounded upload capacity, staging janitor, `fsck` | done |
| M1a.7 UI | embedded dependency-free SPA (about 20 KB gzipped): sign-in, inbox with TQL search and error positions, thread view with presence, in-place subject, transitions, priority, assignee and labels with per-field conflict toast and reapply, description editor with local (2 s) and server (10 s) draft autosave, restored-draft banner, changed-under-you banner, three-column conflict dialog with per-block chooser and the four actions, attachments (paste, drop, resumable upload, thumbnails, signed download), comments, activity lines, dark and light themes, admin user creation endpoint | done, phone layout not yet verified |
| M1a.8 Email and packaging | SMTP transport (none, STARTTLS, implicit TLS, verification always on, no plaintext credentials), shared in-process SMTP mock with transcript recording and scripted failures, per-issue threaded notifications and account mail from an outbox cursor, queue with lease, exponential backoff, idempotency and send-time permission check, admin delivery log, Dockerfile and Compose built and run for a fresh install, configuration reference | done |
| M1a.9 Exit gate | Audit of every M1a exit id (TESTPLAN section 7) and closing the gaps that can be closed without a realtime channel or a browser harness: reliability mini-campaign with a reconciler that is itself tested, server-side latency, configuration schema with precedence and generated reference, `doctor` faults, licence scan, NOTICE and no-phone-home guard, systemd unit, reproducible tarballs | done; gaps listed below |
| M1a.10 Realtime and browser harness | WebSocket gateway (single-use tickets, origin check, per-channel authorization with revocation, frame, rate and subscription limits, presence on state change only, change events carrying `version` and `text_seq`), UI on the socket with polling as the fallback, then: rich-text schema and sanitizer, Playwright E2E with axe, throttled phone profile | realtime done; the rest next, needed to pass the M1a gate |

## Deviations from the spec (deliberate, to be revisited)

| Area | Spec | Built | Reason |
|---|---|---|---|
| Rich text | ProseMirror JSON canonical (DESIGN Part II) | Plain text with Markdown-style blocks separated by blank lines; merge works per block, then per word | Keeps M1a testable end to end; the merge, `text_seq` and history layers are format-agnostic. The editor schema and converters land with the UI milestone or M3 |
| History | Written asynchronously from the outbox | Written in the same transaction as the change | Removes a class of lost-history bugs (E1 and HIST-06 risk) at no cost on one node. The outbox still carries the events; an async history writer can replace this later |
| Tenancy | Organization root | Single organization | Multi-org is optional in the spec; a migration adds it |
| Presence and change events | WebSocket channel, Redis for 2+ nodes | WebSocket gateway and in-process presence and fan-out (single node); pages fall back to polling the view with `If-None-Match` when the socket is down | Redis fan-out is for the multi-node profile (M4); on one node the semantics are complete |
| Presence (domain) | WebSocket channel | In-process store with a heartbeat API | WebSocket gateway arrives with the UI milestone; the semantics (10 s heartbeat, 30 s expiry, appear-offline) are implemented and tested |

| Malware scanning | Pluggable scanner (ClamAV class), asynchronous `scanning` state | Built-in streaming EICAR signature check at commit, so a blob is `clean` or `blocked` immediately | The `scanning` state, the download refusal and the scan column exist; an external scanner replaces the signature check behind the same call |
| Upload modes | Proxy and direct-to-S3 | Proxy only, `fs` backend | S3 and direct upload are M2 |

## Issues discovered and how they were addressed

| # | Found in | Issue | Resolution |
|---|---|---|---|
| 1 | M1a.3 | Link sanitizer let `[x](javascript:alert(1))` leave a stray `)` because a regex stopped at the first closing parenthesis | Replaced with a balanced-parenthesis scanner; `TestLIVE13_Sanitization` is the regression test |
| 2 | M1a.4 | Test helper decoded every response into a JSON object, so the presence endpoint (a JSON array) looked empty | Test-only fault; assertion replaced by a status check. The endpoint was correct |
| 3 | M1a.5 | `assignee != X` silently dropped unassigned issues: `NOT (NULL)` is NULL in SQL, JQL includes EMPTY | All negations go through a NULL-safe `negate()` helper; `TestTQL01_GoldenQueries` covers `!=` on assignee and `currentUser()` |
| 4 | M1a.5 | `ORDER BY priority` produced a SQL syntax error: the order expression was split on `, ` which cut through an `ARRAY[...]` literal | Separator changed to `\|`; ordering by priority, key and summary is in the golden test |
| 5 | M1a.5 | Text search took 550 to 930 ms at 100,000 issues (target 400 ms). An `OR` of `EXISTS` subqueries blocked index use and ran a per-row subplan over the whole issue table | Rewritten as one `i.id IN (SELECT ... UNION ALL SELECT ...)`; p95 now 60 to 140 ms for every text query. `make perf` guards it |
| 6 | M1a.5 | `CREATE EXTENSION` without a schema installed `pg_trgm` into the first schema on the search path; in the test harness that schema was dropped, breaking every later migration | Both extensions are created `SCHEMA public` in the migrations. Pre-release, so migration 0001 was edited in place; from the first release migrations are immutable |
| 7 | M1a.5 | First plan-shape test used a term present in every row, where a sequential scan is the correct plan | Test changed to selective terms; the assertion is about indexes being usable, not forced |
| 8 | M1a.5 | PostgreSQL 16 `unaccent` expands `ß` to `ss`, so a documented limitation (no expansion) was wrong | The test pins the real behaviour; the contract is per PostgreSQL version (TQL-22) |
| 9 | M1a.6 | GC and the janitor compared the injected service clock with `now()` from the database and with file mtimes, so blobs were collected inside the retention window in tests | Blob `zero_since` is stamped with the service clock; the janitor test uses real time for file mtimes. One clock source per decision |
| 10 | M1a.6 | Test image was a smooth gradient that compresses below the class limit under test, so the limit never triggered | Test limit lowered; the limit code was correct |
| 11 | M1a.6 | Minting a link for a blocked file returned 500: `NotDownloadable` was mapped only on the serving path | Mapped centrally to 423 in the error translation; `TestHTTP_BLOB05_SignedLinksAndBlockedFiles` is the regression test |
| 12 | M1a.7 | The "changed under you" banner never appeared while the cursor was in the editor: the change poll skipped re-rendering to protect typing and the banner was part of the render | Found by driving two users in a real browser. The banner now updates in place without touching the editor text |
| 13 | M1a.7 | Conflict panel showed three unlabeled blocks | Added Original, Theirs and Mine labels |
| 14 | M1a.7 | The sandbox test window could not be resized, so the phone layout is unverified | Open item: add a headless-browser E2E suite (LIVE-02, LIVE-03, UI-01..05, A11Y-01..02) in a later milestone; verified by hand so far: sign-in, list, ticket, restored draft, conflict dialog, merge, light theme, unsaved-changes guard |
| 15 | M1a.8 | The container exited at start: it runs as a non-root user but a fresh named volume at `/data` is root-owned, so the blob directory could not be created | Found by a real Compose install. The image now creates `/data/blobs` owned by the runtime user, which a fresh volume inherits |
| 16 | M1a.8 | Mail recipients were stored as a bare address, losing the display name in `To`; first attempt used Go quoting, which is not RFC 5322 | Recipients are stored as `net/mail` addresses (correct encoding of names with quotes and non-ASCII characters) |
| 17 | M1a.9 | A campaign client failed to get a save acknowledged in 60 attempts, which looked like a lost save | Not a server fault: with 12 clients retrying instantly, one starved (needed 43 to 47 attempts). The simulated client now backs off with jitter like a real one; the final counts show no E1, E2 or E4. Starvation is also a reminder for the UI: a person saving under heavy contention could see the dialog repeat several times |
| 18 | M1a.9 | List page p95 was 42 to 48 ms against a 50 ms budget: the capped total ran after the page query | Page and total now run concurrently (DESIGN 11.1 parallel fan-in): 28 and 37 ms |
| 19 | M1a.9 | Benchmark setup used documents above the 1 MiB text limit and projects the user was not a member of; the server correctly answered 400 and 404 | Test data fixed; both responses were right |

## M1a exit gate audit (TESTPLAN section 7)

The gate lists the ids below. **Done** means an automated test passes in this repository. **Partial** means the core is tested and the listed part is not. **Open** means not built or not tested.

| Ids | State | What is missing |
|---|---|---|
| LIVE-01, 05, 07, 14 | done | |
| LIVE-02, 03, 16 | partial | exercised by hand in Chrome with two users; no automated browser run |
| LIVE-04 | partial | merge unit tests exist, the triple corpus (lists, tables, marks, RTL, 1 MB) and the browser runner do not |
| LIVE-06 | open | rich-text round trip needs the ProseMirror schema and the Markdown and ADF converters; text is plain with blank-line blocks (deviation 1) |
| LIVE-10, 11 | partial | server drafts private and stale-aware, local drafts restored by hand; 30-day expiry and per-user cap, tab kill, offline and eviction not tested |
| LIVE-12 | partial | the reconciler detects every injected class; injection into a running system and production counters are not built |
| LIVE-13 | partial | link schemes, control characters and size are sanitized and reported; no node or attribute schema, no fuzzing |
| LIVE-15, 17 | partial | `text_seq` on every REST read and on every text event; events are best effort (a dead subscriber never blocks writers, saves work with no hub at all); a hidden tab refetches when shown. Dropped-event fault injection and Redis loss are not tested |
| DUR-01 | done for lost acknowledgements | real process kill not scripted |
| DUR-02, 03, 05, 06 | open | restart during editing, Postgres restart or failover, validator change, 3-node profile |
| DUR-04 | not applicable yet | history is written in the same transaction (deviation 2) |
| PRES-01..04 | partial | over the socket and the HTTP fallback: change broadcast, leave on tab close, expiry sweep, appear-offline flag, never blocks a save; the avatar stack and field notes were checked by hand in Chrome; accessibility of the stack is unchecked |
| PRES-05, 06, 07 | partial | channel refusal is identical for forbidden and nonexistent issues, revocation stops events within the 5 s authorization window, 50 connections on one ticket send nothing for unchanged heartbeats; field and issue-security restrictions do not exist yet, 1,000 idle connections per node and cross-node presence (needs Redis) are not tested |
| HIST-01, 02, 08 | partial | before and after, restore as a new change; no computed diff, no deduplication of stored versions |
| HIST-03, 04, 05, 07 | open | mentions, notification batching, per-field tracking flag, GDPR redaction and legal hold |
| PERF-01, 03, 07, 10, 11 | done (server time) | browser-side budgets not measured |
| PERF-02, 04, 06, 08, 09 | open | cold data, board (M2), first visit timing, prefetch, editor on the phone profile |
| PERF-12, 13 | done (single node) | presence change p95 2.8 ms and saved-change event p95 6.6 ms to another open socket, against 1 s |
| PERF-16, 17 | done | single machine; reference `perf` hardware undefined |
| CHAOS-01 | partial | lost acknowledgements injected; no node kill |
| CHAOS-02 | open | Redis is not implemented (single node only) |
| CHAOS-03 | partial | lost acknowledgements only; latency and packet loss need the `chaos` profile |
| CORE-01, 02 | partial | create, edit, keys, standard workflow templates; move, clone, delete are M1b; no conditions, validators or post-functions; hierarchy rules minimal |
| CORE-03 | open | custom fields are M3 (the gate text in section 7 says M1a; the spec milestone says M3; needs a decision) |
| CORE-07, 08 | done / partial | attachment previewers per type are not built (images use thumbnails, others download) |
| CORE-09 | partial | comments and visibility; reactions, collapse and filters not built |
| LIFE-01, 02 | partial | transition atomicity and concurrent-transition semantics tested at the field-version level; admin-defined types, per-type workflows and schemes are M3 |
| TQL-01, 02, 03, 06, 21, 22, 24, 26 | done or partial as listed in section 12 | no real-world JQL corpus |
| TQL-04, 23, 25, 27 | partial or open | custom fields (M3); English text configuration and online switch; combined-plan matrix; GIN pending-list behaviour under a million updates |
| SEC-01 | partial | permission checks tested per feature; the generated route-by-role matrix is not built |
| SEC-02 | done | forged, missing, expired and reused tickets, cross-origin handshake, oversized frames, malformed JSON, binary frames, flooding, unknown operations and bad channel names are refused or disconnected and the server stays healthy |
| SEC-03 | partial | comment visibility across REST, search and mail; no issue security levels |
| SEC-05 | partial | XSS-safe rendering (no HTML sinks, escaped mail, CSP), sanitized links, parameterized TQL; CSV export injection is M1b |
| UI-01 | partial | structural tests only; no visual regression |
| UI-02, 03, 04, 05 | open | contrast checker, `system` theme and server-side setting, responsive layout verified, Gmail-style collapse and unread |
| A11Y-01, 02 | open | no axe run; keyboard flows only partly considered (focus trap, labels) |
| BLOB-01, 02, 04..07, 09, 12, 20, 23 | done or partial as listed in section 12 | BLOB-08 external scanner; BLOB-20 mixed-load run |
| MAIL-01..03 | done or partial | see section 12 |
| INST-01 | partial | Compose install verified by hand; tarball and systemd unit built and checked structurally, not installed on a clean host; Helm is M4 |
| INST-02 | partial | PostgreSQL version and extensions, SMTP login and TLS, clock, file descriptors, blob directory; Redis, IMAP, proxy WebSocket and egress checks need those features |
| INST-03 | partial | amd64 and arm64 both build; the suite ran on PostgreSQL 16 only (15 and 17 untested), no Valkey or S3 matrix |
| INST-05 | partial | reproducible archives proven by building twice; signing and SBOM matching the archive are release-pipeline work |
| CFG-01, 02 | done | |
| CFG-03 | open | hot reload of the safe subset |
| CFG-04 | partial | secrets masked in `config print` and absent from error text; no crash-dump or diagnostics-bundle check |
| UPG-01 | partial | advisory lock, idempotency, numbering; expand and contract lint, `migrate plan`, checksum mismatch, lock-time measurement |
| LIC-01, 02, 05 | done | |

**Conclusion.** The tracker core, the editing-safety model, search, attachments, mail, packaging and configuration are built and tested. The M1a gate needs the realtime channel and a browser test harness (accessibility, visual, responsive, presence in a browser), the rich-text schema, and several durability and upgrade drills before it can be declared passed. M1a.10 covers the realtime channel and the harness. M1b and M1c are not started.
| 20 | M1a.10 | The licence gate failed on a new dependency (`coder/websocket`, ISC) because the classifier matched one wording of the ISC text and the file wraps lines differently | The classifier normalizes whitespace and knows both ISC wordings; still keyed on licence text, not on package names. The gate working as designed: a new dependency is reviewed before it ships |
| 21 | M1a.10 | The page did not refresh from an event while its tab was hidden: the refetch was skipped for hidden tabs and never retried | Found in Chrome (the automation window is a background tab). A hidden tab now marks itself stale and refetches when it becomes visible (SPEC: refetch on focus) |
| 22 | M1a.10 | WebSocket upgrades would have failed behind the security-headers middleware, which wrapped the response writer and hid `Hijacker` | The wrapper exposes `Unwrap`, `Hijack` and `Flush`; every gateway test goes through the full handler stack |
| 23 | M1a.10 | HTTP-fallback presence updated the store but not the socket clients, so mixed clients saw different state | The HTTP presence endpoints broadcast on change; `TestPRES_HTTPFallbackPresenceReachesSocketClients` |
