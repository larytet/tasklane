# tasklane

An open source, self-hosted issue and project tracker with a Jira-like UI, written in Go.

Tasklane aims to give teams the parts of Jira they actually use - boards, backlogs, weekly sprints, rich ticket views,
custom fields, workflows - without the weight, and with the enterprise basics (SSO, audit, permissions) built in.

## Design stance

- **Dark mode first.** Dark is the default and reference theme; light is derived from the same tokens.
- **Gmail-compact ticket reading.** A ticket is a thread: compact message rows, collapsed history, an attachment chip
  strip, a sticky reply bar. Reading a ticket and its attachments needs nothing else on screen.
- **Management in dedicated tabs.** Workflows, fields, schemes, permissions and SSO live in Admin tabs, out of the way.

- **Safe concurrent editing, without locks.** Anyone can edit at any time. You see who else is editing a field, drafts are autosaved
  so nothing typed is lost, and a save that would overwrite someone else's change opens a merge dialog instead of clobbering it.
- **Built for agents.** A built-in MCP server lets AI coding agents pick up tickets, report progress, link PRs and hand
  work to a human for review, under the same permissions and audit as people.

## What it looks like

- **Board** - Scrum and Kanban boards with swimlanes, quick filters, drag and drop between workflow columns, WIP limits.
- **Backlog** - ranked list with sprint panels, drag to plan, story point totals per sprint.
- **Sprints** - weekly cadence by default, auto-created and auto-rolled, burndown and velocity. The week follows your work
  calendar (Monday to Friday, Sunday to Thursday, or custom start day, working days and holidays).
- **Ticket view** - full page and side-panel layout: summary, description, custom fields, subtasks, links, comments,
  activity, attachments, time tracking, watchers.
- **Search** - a query language (TQL, JQL-compatible subset), saved filters, list and table views, bulk edit.
- **Dashboards** - gadgets backed by saved filters.
- **Admin** - projects, workflows, schemes, custom fields, permissions, SSO, audit log.

## Mockups

Wireframes in dark mode (the reference theme). Tokens, type and spacing are in [DESIGN.md](DESIGN.md).

### Issues inbox (phone)

```
┌──────────────────────────────────────┐
│ ☰  🔍 Search or TQL…            (AM) │
├──────────────────────────────────────┤
│ (Assigned to me) Reported Watching ▸ │  filter chips
├──────────────────────────────────────┤
│ ● ▣ ABC-142  Checkout fails on 3DS…  │  ● = unread, summary bold
│     [In Progress]  ⇧  (JK)   2m  📎2 │
├──────────────────────────────────────┤
│   ▣ ABC-139  Add CSV export          │
│     [To Do]        =  (AM)   1h  💬4 │
├──────────────────────────────────────┤
│   ▣ ABC-131  Sprint rollover job     │
│     [Done]         ⇩  (RS)   3d      │
├──────────────────────────────────────┤
│   ◀ swipe: transition   assign ▶     │
├──────────────────────────────────────┤
│  ⌂ Home   ☰ Issues   ▦ Boards   ⋯    │  bottom bar
└──────────────────────────────────────┘
```

### Ticket as a thread, with presence

```
┌────────────────────────────────────────────────────────────┐
│ ←  ABC-142   ☆     (JK)(AM)●(RS)  3 here        ‹  ›   ⋮  │  presence stack, prev/next
├────────────────────────────────────────────────────────────┤
│ Checkout fails on 3DS challenge                  Saved ✓   │  subject, editable in place
│ [In Progress ▾]  ⇧ High  (JK) Jana K.   bug  payments      │
│ ▸ Details · Sprint Week 2026-W43 · Fix 2.4 · 5 pts         │  collapsed metadata
├────────────────────────────────────────────────────────────┤
│ (AM) Arkady M.  · 2 days ago        ◔ Jana K. is editing   │  advisory: nobody is locked out
│  After the issuer redirects back, the order stays in       │
│  “pending”. Steps to reproduce:                            │
│  1. Use test card 4000 …                                   │
│ ┌────────┐ ┌────────┐ ┌──────────────────┐                 │
│ │ ▨ img  │ │ ▨ img  │ │ 📄 trace.log 48K │  3 attachments  │  chip strip
│ └────────┘ └────────┘ └──────────────────┘   ⬇ download all│
├────────────────────────────────────────────────────────────┤
│ ▸ 3 earlier comments                                       │
│ ·· Jana changed status To Do → In Progress · 1h            │  slim activity line
│ (RS) Ravi S. · 20m                                         │
│  Reproduced on staging. Looks like the return URL is lost. │
│  👍 2                                                       │
├────────────────────────────────────────────────────────────┤
│ ▸ Subtasks 2/3 ███████░░░   ▸ Linked issues (1)            │
├────────────────────────────────────────────────────────────┤
│ ┌────────────────────────────────────────────────────────┐ │
│ │ Reply…    @ mention   📎      □ Internal   ▸ Send      │ │  sticky reply bar
│ └────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────┘
```

### Conflict dialog (two people saved the same description)

```
┌──────────────────────────────────────────────────────────────────────────┐
│ Jana K. saved changes to the description while you were editing   (2 min)│
│ 2 changes by Jana, 1 block changed by both                               │
├───────────────────────┬───────────────────────────┬──────────────────────┤
│ THEIR CHANGES         │ MERGED RESULT (editable)  │ YOUR CHANGES         │
│ + added a retry note  │  Steps to reproduce:      │ + step 3 clarified   │
│ - removed old step 4  │  1. Use test card 4000 …  │                      │
│                       │ ⚠ Block 2 changed by both │ ⚠ block 2: "Expected │
│ ⚠ block 2: "Expected  │  [ theirs | mine | edit ] │   result: order …"   │
│   result: pending"    │                           │                      │
├───────────────────────┴───────────────────────────┴──────────────────────┤
│ [Save merged result]  [Keep mine]  [Keep theirs]  [Copy mine and reload] │
└──────────────────────────────────────────────────────────────────────────┘
   Keeping mine never destroys their version: it stays in history and can be restored.
```

### Board (desktop)

```
┌────┬───────────────────────────────────────────────────────────────────┐
│ ⌂  │ Payments · Week 2026-W43   [Only mine] [Recent]   Complete sprint │
│ ☰  ├───────────────────┬───────────────────┬───────────────────────────┤
│ ▦  │ TO DO        3    │ IN PROGRESS  2/2  │ DONE                 5    │
│ ⏱  ├───────────────────┼───────────────────┼───────────────────────────┤
│ 📊 │▌Retry webhooks    │▌Checkout fails    │ Sprint rollover job       │
│ ⚙  │ ▣ ABC-150  ⇧ (AM) │ ▣ ABC-142  ⇧ (JK) │ ▣ ABC-131  ✓   (RS)       │
│    │                   │ 5 pts             │                           │
│    │ Add CSV export    │ Fix card form     │ Dark theme tokens         │
│    │ ▣ ABC-139  = (AM) │ ▣ ABC-144  = (RS) │ ▣ ABC-120  ✓   (AM)       │
│    │                   │  ⚠ at WIP limit   │                           │
│    │ + Create          │                   │                           │
└────┴───────────────────┴───────────────────┴───────────────────────────┘
      ▌ = epic color      dragging a card shows a drop line; blocked moves explain why
```

### Backlog and weekly sprints

```
┌────────────────────────────────────────────────────────────────────┐
│ ▾ Week 2026-W43   Oct 19 – Oct 25   ● active    12 issues          │
│   Goal: ship 3DS fix and CSV export                                │
│   [To Do 8][In Progress 5][Done 13]   capacity ███████░░ 26/32 pts │
│   ⋮⋮ ▣ ABC-142  Checkout fails on 3DS challenge  [In Progress] (JK) │
│   ⋮⋮ ▣ ABC-139  Add CSV export                   [To Do]       (AM) │
│   + Create issue                                [Complete sprint]  │
├────────────────────────────────────────────────────────────────────┤
│ ▾ Week 2026-W44   Oct 26 – Nov 1   future (auto-created)  4 issues │
│   ⋮⋮ ▣ ABC-155  Audit log export                 [To Do]       —    │
├────────────────────────────────────────────────────────────────────┤
│ ▾ Backlog   38 issues                                               │
│   ⋮⋮ ▣ ABC-160  Saved filter subscriptions       [To Do]       —    │
│   ⋮⋮ ▣ ABC-161  Bulk transition preview          [To Do]       —    │
└────────────────────────────────────────────────────────────────────┘
```

## Goals

- Familiar to anyone who has used Jira: same vocabulary (project, issue type, workflow, sprint, epic, filter).
- A single Go binary plus PostgreSQL; one install, one command. No JVM, no plugins runtime required to get started.
- Custom fields and workflows are data, not code: admins change them in the UI with no deploy.
- Work with Jira, not instead of it at first: a read-only, one-way mirror of Jira into Tasklane gives a fast UI over your
  Jira data (edits stay in Jira), and the same machinery imports a project when you are ready to switch. Export to Jira is by
  CSV; write-back to Jira is a later add-on.
- Open exchange formats: a JSON bundle and a CSV bundle for the full project, plus basic importers for GitHub Issues,
  Trello, Linear, Asana and generic CSV. No lock-in.
- Email in and out: threaded notifications, reply by email, create issues by email, with spoofing and loop protection.
- A real issue lifecycle: custom workflows, convert, move (old keys keep working), clone, trash and restore.
- Install with a container, Compose, Helm or a tarball; documented configuration; rolling upgrades; no license keys.

## Status

Specification phase. See [SPEC.md](SPEC.md) for the functional specification and [DESIGN.md](DESIGN.md) for the
design specification (tokens, typography, components, screens, backend and frontend architecture), and
[TESTPLAN.md](TESTPLAN.md) for the test plan.

## Stack

- Go (API server, workers), PostgreSQL, optional OpenSearch (an additional service) for search at scale
- REST API (Jira-compatible subset under `/rest/api/3`) plus webhooks
- Web UI: server-delivered SPA

## License

Apache-2.0 (proposed, to be confirmed before the first public release; see SPEC section 17.9).
