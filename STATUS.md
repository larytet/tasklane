# Tasklane - Implementation Status

Living record of what is built against SPEC section 22, the milestones inside each phase, issues found on the way and
how they were resolved. Updated at the end of every milestone, together with the "Implementation status" table in
TESTPLAN.md, after the suite is green and before the commit and push.

**Working rule.** A milestone is closed only when: its tests are written and pass (`make test`), TESTPLAN.md is updated,
this file is updated, and the work is committed and pushed.

## Phase overview

| Phase | State | Notes |
|---|---|---|
| M1a Tracker | in progress | milestones M1a.1 to M1a.8 below |
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
| M1a.7 UI | embedded SPA: inbox, thread view, conflict dialog, drafts, presence, dark and light themes | next |
| M1a.8 Email and packaging | SMTP outbound, Dockerfile, Compose (written in M1a.5, not yet exercised), tarball, configuration reference (written), exit gate | planned |

## Deviations from the spec (deliberate, to be revisited)

| Area | Spec | Built | Reason |
|---|---|---|---|
| Rich text | ProseMirror JSON canonical (DESIGN Part II) | Plain text with Markdown-style blocks separated by blank lines; merge works per block, then per word | Keeps M1a testable end to end; the merge, `text_seq` and history layers are format-agnostic. The editor schema and converters land with the UI milestone or M3 |
| History | Written asynchronously from the outbox | Written in the same transaction as the change | Removes a class of lost-history bugs (E1 and HIST-06 risk) at no cost on one node. The outbox still carries the events; an async history writer can replace this later |
| Tenancy | Organization root | Single organization | Multi-org is optional in the spec; a migration adds it |
| Presence | WebSocket channel | In-process store with a heartbeat API | WebSocket gateway arrives with the UI milestone; the semantics (10 s heartbeat, 30 s expiry, appear-offline) are implemented and tested |

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
