# Goal: working fast in the issue list and issue view

The UX features still to build. Each feature has a stable identifier; the behavior is in [SPEC.md](SPEC.md) (section
in the table) and the tests are in [TESTPLAN.md](TESTPLAN.md) section 5.18. The features already built (UX-PREF to
UX-HINTS, M5.66 to M5.73) are recorded in [STATUS.md](STATUS.md).

## Features

| Id | Feature | SPEC | Tests |
|---|---|---|---|
| UX-BULK-ALL | "Select all N matching" as a resumable background job | 4.1, 9.1 | BULK-02, LIFE-16 |

## Decisions

- **Bulk scope:** the bulk bar acts on up to one page (100 issues). "Select all N matching" (UX-BULK-ALL) extends it
  with background jobs.

## Done when

Every feature works as its SPEC section describes, its TESTPLAN rows pass (Playwright in both themes with axe, Go tests
against PostgreSQL, new routes in the authorization matrix), and STATUS.md and TESTPLAN.md section 12 record it.
