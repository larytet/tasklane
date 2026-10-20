# Goal: working fast in the issue list and issue view

The UX features planned for the next milestones. Each feature has a stable identifier; the behavior is in
[SPEC.md](SPEC.md) (section in the table) and the tests are in [TESTPLAN.md](TESTPLAN.md) section 5.18.

## Features

| Id | Feature | SPEC | Tests |
|---|---|---|---|
| UX-PREF | Per-person UI settings, stored server side and following the person across devices (`GET`/`PUT /api/v1/me/ui-prefs`) | 4.6 | PREF-01..04 |
| UX-SETTINGS | Settings dialog: theme, density, short or long view, columns and sort, font size, reduced motion, high contrast, shortcuts on or off | 4.6 | PREF-05 |
| UX-KEY | Keyboard shortcuts: `c` create, `/` search, `j`/`k` move, `o` and Enter open, `x` select, `e` close or resolve, `a` assign, `g` `i` inbox, `g` `b` boards; can be turned off | 4.6, 20 | KEY-01..09, KEY-11 |
| UX-HELP | `?` shortcut cheat sheet, built from the same table as the shortcuts | 4.6 | KEY-10 |
| UX-SEND | Ctrl/Cmd+Enter sends a comment and saves a field being edited | 4.2, 4.6 | KEY-08 |
| UX-PAL | Command palette on Ctrl/Cmd+K: jump to an issue, project, board or setting; run actions such as "New project" | 4.6 | PAL-01..07 |
| UX-UNDO | Undo toasts instead of confirm dialogs ("Issue moved to Done. Undo", 5 s), reverted by the server | 4.6 | UNDO-01..09 |
| UX-BULK | Checkboxes, select the page, sticky bulk bar: assign, transition, label, move, archive, delete; plan first, undo after | 4.1 | BULK-01, BULK-03..09 |
| UX-BULK-ALL | "Select all N matching" as a resumable background job (later milestone) | 4.1, 9.1 | BULK-02, LIFE-16 |
| UX-HOVER | Row hover actions: assign, mark done, watch, without opening the issue | 4.1 | HOVER-01..04 |
| UX-DENSITY | Density: compact, default, comfortable, next to the short/long view | 3, 4.1 | LIST-01, LIST-06 |
| UX-COLUMNS | Column chooser and sort, saved per person | 4.1 | LIST-02..04 |
| UX-PAGER | Sticky pager on desktop instead of page numbers at the bottom; infinite scroll stays on phones | 4.1 | LIST-05 |
| UX-INLINE | Editing every field in place, without an Edit button | 4.2 | EDIT-01..04 |
| UX-REACT | Emoji reactions on comments (a fixed set of eight), so "+1" replies stop adding noise | 4.2 | REACT-01..06 |
| UX-A11Y | Font size, reduced motion and high contrast as personal settings | 3, 20 | THEME-01..04 |
| UX-HINTS | Onboarding hints: one tooltip each for `c`, the view buttons and Settings | 4.6 | HINT-01..03 |

## Decisions

- **Undo of a transition** always returns the issue to its previous status, even when the workflow has no way back or a
  condition would refuse it. History and audit record it as an undo; automation that already ran is not reversed.
- **Paging:** sticky pager on desktop (pages of 100, page in the URL); infinite scroll only on phones.
- **Bulk scope:** the first bulk bar acts on up to one page (100 issues). "Select all N matching" (UX-BULK-ALL) follows
  in a later milestone with background jobs.
- **Reactions:** a fixed set of eight emoji: 👍 👎 😄 🎉 😕 ❤️ 🚀 👀.

## Order of work

1. Foundations: UX-PREF, UX-SETTINGS, the shortcut registry, the current row and selection in the list.
2. UX-KEY, UX-HELP, UX-SEND.
3. UX-PAL.
4. UX-UNDO (server undo tokens first).
5. UX-BULK (REST endpoint sharing the MCP bulk code, adding move and archive), UX-HOVER.
6. UX-DENSITY, UX-COLUMNS, UX-PAGER.
7. UX-INLINE.
8. UX-REACT, UX-A11Y, UX-HINTS.
9. Later: UX-BULK-ALL.

## Done when

Every feature works as its SPEC section describes, its TESTPLAN rows pass (Playwright in both themes with axe, Go tests
against PostgreSQL, new routes in the authorization matrix), and STATUS.md and TESTPLAN.md section 12 record it.
