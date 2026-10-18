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
| M1a.4 HTTP API and server | REST + UI bootstrap endpoints, auth middleware, `tasklane serve`, `migrate`, `doctor`, health | next |
| M1a.5 TQL and search | TQL parser and compiler, PostgreSQL full-text and trigram, issue list read model | planned |
| M1a.6 Attachments | blob store interface, local filesystem backend, uploads, hash, size limits | planned |
| M1a.7 UI | embedded SPA: inbox, thread view, conflict dialog, drafts, presence, dark and light themes | planned |
| M1a.8 Email and packaging | SMTP outbound, Dockerfile, Compose, tarball, configuration reference, exit gate | planned |

## Deviations from the spec (deliberate, to be revisited)

| Area | Spec | Built | Reason |
|---|---|---|---|
| Rich text | ProseMirror JSON canonical (DESIGN Part II) | Plain text with Markdown-style blocks separated by blank lines; merge works per block, then per word | Keeps M1a testable end to end; the merge, `text_seq` and history layers are format-agnostic. The editor schema and converters land with the UI milestone or M3 |
| History | Written asynchronously from the outbox | Written in the same transaction as the change | Removes a class of lost-history bugs (E1 and HIST-06 risk) at no cost on one node. The outbox still carries the events; an async history writer can replace this later |
| Tenancy | Organization root | Single organization | Multi-org is optional in the spec; a migration adds it |
| Presence | WebSocket channel | In-process store with a heartbeat API | WebSocket gateway arrives with the UI milestone; the semantics (10 s heartbeat, 30 s expiry, appear-offline) are implemented and tested |

## Issues discovered and how they were addressed

| # | Found in | Issue | Resolution |
|---|---|---|---|
| 1 | M1a.3 | Link sanitizer let `[x](javascript:alert(1))` leave a stray `)` because a regex stopped at the first closing parenthesis | Replaced with a balanced-parenthesis scanner; `TestLIVE13_Sanitization` is the regression test |
