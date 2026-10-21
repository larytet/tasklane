# Tasklane - Functional Specification

Version 0.1 (draft). Language: Go. Scope: functionality first; implementation notes are kept minimal.

Sources for this spec: working knowledge of Jira Software/Cloud (issue, board, backlog, reports, approvals, dashboards,
filters, releases, calendar, forms, service management) and the compact conversation-style reading pattern of modern mail
clients. Only functionality and UX patterns are described; no Atlassian or Google code, assets or trademarks are reused.

## 1. Principles

1. **Dark mode first.** Dark is the default and the reference design. Light is derived from the same tokens (section 3).
2. **Reading is compact.** Viewing a ticket and its attachments follows the Gmail conversation model (section 4). All
   management (workflows, fields, schemes, permissions, SSO) lives in dedicated admin tabs, never in the reading flow.
3. **Data, not code.** Custom fields, workflows, screens, permissions are rows edited at runtime.
4. **Familiar vocabulary.** Project, issue, issue type, status, workflow, sprint, epic, filter, board, version.
5. **API parity where cheap.** A Jira-compatible subset of `/rest/api/3` for tooling and migration.
6. **Agents are first-class.** AI coding agents work from the tracker through a built-in MCP server (section 18.3), under
   the same permissions, audit and human review gates as people.

## 2. Information model

| Entity | Notes |
|---|---|
| Organization | Tenant root (enterprise: multiple orgs per instance optional). Owns users, groups, SSO, audit. |
| Project | Key (`ABC`), name, type (`software`, `business`, `service`), lead, default assignee, category, avatar, archived flag. |
| Issue | Key `ABC-123`, summary, description (rich text), issue type, status, priority, resolution, reporter, assignee, labels, components, fix versions, affects versions, parent, rank, created/updated/resolved, custom field values. |
| Issue type | Standard: Epic, Story, Task, Bug, Subtask. Admin-defined types with icon and hierarchy level. |
| Hierarchy | Levels: Initiative (optional) > Epic > Story/Task/Bug > Subtask. Level names configurable. |
| Workflow | Statuses, transitions, conditions, validators, post-functions. |
| Board | Scrum or Kanban, bound to a saved filter. |
| Sprint | Name, goal, start, end, state (`future`, `active`, `closed`), board. |
| Version / Release | Name, description, start, release date, released/archived. |
| Component | Name, lead, default assignee. |
| Filter | Saved query, owner, sharing, favourite. |
| Comment | Rich text, author, visibility restriction, created/edited, reactions. |
| Attachment | File (bytes in the blob store, section 17.6), uploader, size, MIME, thumbnail, virus-scan state. |
| Worklog | Author, started, time spent, comment. |
| Issue link | Typed (blocks / is blocked by, duplicates, relates to, custom). |
| Watcher / Vote | Per user per issue. |
| Approval | Approvers, decision rule (all / any / N), decision, comment. |
| Automation rule | Trigger, conditions, actions. |
| Audit event | Actor, action, target, before/after, IP, time. |
| Development link | Issue to branch, commit, pull request, build or deployment (provider-neutral), source (automatic key match or manual), cached title, state, author, URL. |
| Work calendar | Time zone, week start day, working days and hours, holiday set, week numbering system; scoped to org, project, board, team or user. |
| Team | Name, members, lead, associated projects; usable as assignee group, in filters and in notifications. |
| Dashboard / gadget | Layout (columns), gadgets with configuration and filter binding, owner, sharing, favourite, default flag. |
| Request type | Portal form mapped to an issue type: fields, help text, workflow entry, SLA policy, visibility. |
| SLA | Goal per request type (first response, resolution), calendar, pause conditions, clock state per issue (running, paused, breached, met). |
| Notification / subscription | Event, recipients, channel, state (unread, read, archived) for inbox items; subscriptions (watch, filter digest) with schedule. |
| Webhook | URL, events and filters, signing secret, delivery log with retries. |
| Integration connection | Git provider, Jira mirror, mail, chat: base URL, scopes, credentials as secrets, health, last sync. |
| Secret | Encrypted credential or key with scope, owner, rotation state; never returned by any API. |
| Blob | Object locator in the blob store, hash, size, class, tier, scan state, refcount (SPEC 17.6). |
| Key alias | Old issue or project key resolving to the current one; never reused. |
| Import / export job | Source, scope, consistency mode, mapping, state, progress, per-record report, manifest (SPEC 18.2). |
| Maintenance pause | Scope, owner job, mode, started, deadline; visible in Admin and the API (SPEC 18.2). |
| Customer / customer organization | Portal identity and its company group; not staff, not a seat (SPEC 15.1). |
| Trash entry / removed values | Soft-deleted issue with purge date; field values dropped by convert or move, kept for the retention period (SPEC 9.1). |
| Mail message | Direction, Message-ID, thread, linked issue or comment, status, raw message reference (blob store), authentication and classification result. |
| Mail connection | Outbound (SMTP or provider API) and inbound (IMAP, provider webhook, SMTP listener) settings, mapped addresses, sender policy, health. |
| Agent identity | Agent account or on-behalf-of binding, scopes, project access, token and OAuth client, lease state, usage counters. |

## 3. Visual design (dark mode first)

- Token based theme: `surface`, `surface-raised`, `surface-overlay`, `border`, `text`, `text-subtle`, `accent`,
  `success`, `warning`, `danger`, `info`. Dark values are authored first; light values are derived and checked for
  WCAG AA contrast (4.5:1 text, 3:1 UI).
- Dark palette is near-black blue-grey (not pure black), elevation expressed by lighter surfaces, not shadows.
- Theme setting: `dark` (default), `light`, `system`. Per-user, stored server side, applied before first paint (no flash).
- Status category colors (To Do grey, In Progress blue, Done green) tuned separately for dark and light.
- Attachments, images, code blocks and charts must remain legible in both themes (charts use a validated palette,
  images get no automatic inversion).
- Density: `compact`, `default` and `comfortable` (per person, section 4.6); compact is the default on mobile widths.
- Appearance and accessibility settings per person: font size (90, 100, 115, 130%), reduced motion (follows the system
  setting unless overridden) and high contrast (a token set with raised contrast and borders, in both themes). All are
  applied before first paint, like the theme.
- Responsive: phone (single pane), tablet (list + detail), desktop (nav rail + list + detail); keyboard-first on desktop.

## 4. Ticket view (Gmail-compact reading UX)

The ticket view is a conversation: the issue is the thread, the description and each comment are messages.

### 4.1 Issue list (inbox)

- One compact row per issue: type icon, key, summary (single line, ellipsized), status lozenge, assignee avatar,
  priority icon, updated-ago, attachment paperclip, comment count. Unread (updated since last seen) rows are bold.
- Swipe actions (mobile) and hover actions (desktop), configurable: assign, mark done, watch, transition, snooze,
  archive-from-list (hide from my view), star (favourite). Hover actions also appear when the row has keyboard focus, act
  without opening the issue, and are hidden (not failing) when the person may not take them. The first set is assign,
  mark done (the transition into the done category; a picker when there are several) and watch.
- Multi-select: a checkbox per row, `x` toggles the current row, shift-click selects a range, the header checkbox selects
  the page and then offers "Select all N matching". A sticky bulk bar shows the count and the actions assign, transition,
  label, move, archive and delete. Every bulk action is planned first (dry run, the same rules as MCP bulk tools, section
  18.3); the plan is shown only when some issues would fail or lose data, otherwise it applies and offers Undo
  (section 4.6). Failures are reported per issue. A selection of up to one page (100 issues) runs at once. When the whole
  page is selected and more issues match the query, the bar offers "Select all N matching": the same actions then run as a
  resumable background job (section 9.1) over every issue the person's own search matches, in their own scope, up to 50,000.
  It is confirmed first (the count, and that a job has no Undo); a panel under the bar shows progress and the estimated end,
  offers Cancel, lists the issues skipped with the reason, and comes back after a reload while the job is open. Any change
  of the selection or a new page withdraws "all matching".
- Column chooser and sort: a person picks which columns show (built-in and custom fields; key and summary always) and their
  order, and the sort column and direction. Saved in the person's settings (section 4.6). Restricted or trashed fields
  drop out of the list for people who cannot see them.
- Short and long views of the list, each at the three densities.
- Pull to refresh; a sticky pager (pages of 100, page in the URL) on desktop, infinite scroll on phones; sticky group
  headers (by status, assignee, sprint, epic) optional.
- Top search bar always present: free text and TQL, with recent searches and saved filter chips.
- Inline presets as chips: Assigned to me, Reported by me, Watching, Recently viewed, Open, Done this week.

### 4.2 Thread (issue) view

- Header: back, key, star, prev/next issue in the current list, overflow (share, copy link, clone, move, delete).
- Subject line: summary, editable in place. Under it: status lozenge (tap to transition), priority, assignee, labels as chips.
- Collapsed metadata strip: the remaining fields collapse into one "Details" row that expands in place (Gmail's
  "to me" expander). Custom fields appear here, grouped by their screen tab.
- Messages: description first, then comments chronologically. Older messages collapse into "N earlier comments";
  the newest comment and unread ones are expanded. Each message: avatar, name, relative time, body, reactions.
- Reactions: a fixed set of eight emoji on comments (👍 👎 😄 🎉 😕 ❤️ 🚀 👀), one per person per emoji, with counts and who reacted on hover. They replace
  "+1" replies: no notification, no change to the issue's version or updated time. Only people who can see the comment
  can react; reactions follow the comment through move, trash, export and erasure.
- Activity (field changes, transitions, links) is interleaved as slim one-line items, filterable: All, Comments, History, Work log.
- Quoted / long content is trimmed with "show more".
- Sticky reply bar at the bottom: tap opens an expanding composer with @mention, issue-key autocomplete, markdown and
  rich text, attachments, internal-only toggle, "also transition to" selector. Ctrl+Enter (Cmd+Enter) sends.
- Every field edits in place, without an Edit button: click (or Enter on) the summary, description, priority, assignee,
  labels or a custom field to edit it there. Escape cancels and keeps the draft, Ctrl+Enter or leaving the field saves.
  The draft, autosave and conflict rules of section 4.5 apply unchanged. Read-only fields show why.
- Swipe between issues in the list; back returns to the same scroll position.
- Subtasks and linked issues are a compact section with progress bar and per-row status, collapsed by default.

### 4.3 Attachments

- Attachment strip directly under the description: horizontal chips with thumbnail/icon, filename, size (Gmail style).
  A "N attachments" header with "download all" (zip).
- Tap opens a full-screen previewer with swipe between attachments: images (zoom/pan), PDF, video, audio, text/code
  with syntax highlighting, Office files via converter, archives as listings, others as download card.
- Attachments added in comments appear inside that comment and also in the issue-level strip.
- Upload: drag and drop, paste image from clipboard, camera/gallery on mobile; chunked and resumable; progress per file;
  configurable max size and allowed types; antivirus scan state shown (`scanning`, `clean`, `blocked`).
- Actions per attachment: download, copy link, rename, delete (permission controlled), set as cover.
- Images in descriptions/comments render inline and tap into the same previewer.

### 4.4 Quick actions in the ticket

Transition, assign, comment, log work, watch, vote, link, create subtask, flag/impediment, copy key. Everything else
(schemes, fields, permissions) is deliberately absent from this view.

### 4.6 Working fast: keyboard, command palette, undo, personal settings

- **Shortcuts** (desktop), inert while typing in any field or while a dialog is open:

  | Key | Action |
  |---|---|
  | `c` | Create issue |
  | `/` | Focus the search box |
  | `j` / `k` | Next / previous row in a list; next / previous issue of the current list on the issue page |
  | `o`, Enter | Open the current row |
  | `x` | Select the current row |
  | `e` | Close or resolve (the transition into the done category; a picker when there are several) |
  | `a` | Assign (people search, "me" first) |
  | `g` then `i` | Go to the inbox |
  | `g` then `b` | Go to boards |
  | `?` | Shortcut cheat sheet |
  | Ctrl/Cmd+K | Command palette |
  | Ctrl/Cmd+Enter | Send a comment, save a field being edited |

  Single-key shortcuts can be turned off in Settings (WCAG 2.2 SC 2.1.4). The cheat sheet lists every shortcut, grouped by
  screen, from the same table the app uses, so it never drifts.
- **Command palette** (Ctrl/Cmd+K, from anywhere): one box that jumps to an issue (by key, an old key after a move included,
  or text), a project, a board or a settings or admin page, and runs actions such as "New project", "Create issue" and
  every shortcut action. Recent items first. It never lists anything the person cannot see or do.
- **Undo instead of confirm:** reversible actions (transition, archive, delete to trash, and the same in bulk) happen at
  once and show a toast such as "Issue moved to Done. Undo" for 5 seconds (paused while hovered or focused). Undo is done
  by the server: it reverts only if nobody changed the issue since, otherwise it says so and changes nothing. Undoing a
  transition returns the issue to its previous status even when the workflow has no transition back or a condition would
  refuse it: it corrects the person's own action seconds ago and is not a new transition. The undo is recorded in history
  and audit as "undone". Automation that already ran is not reversed. Operations that can drop data (move, clone,
  convert, a delete whose plan has blockers or drops) keep their plan dialog.
- **Settings dialog** (top bar and palette): one place for the person's own settings, stored server side and following
  them across devices: theme, density, short or long list view, columns and sort, font size, reduced motion, high
  contrast, keyboard shortcuts on or off. The account's own security is in the same dialog, in the tabs **Security**
  (two-factor authentication, passkeys and security keys, the password), **Sessions**, and **API tokens** (connected
  applications and API tokens); the person's name in the top bar opens it on Security. There is no separate Account page:
  `#/account` is a short page that opens Settings on Security, which is where a person is sent when the organization requires
  a second factor and they have none (nothing behind it asks the server for anything, so a refusal cannot send them back in a
  loop). A dialog opened from Settings (a setup dialog) sits above it: the one below is inert and hidden from assistive
  technology until it closes, and Escape closes one dialog at a time.
- **Onboarding hints:** a first-time user sees one tooltip each for `c`, the view buttons and Settings, at most one per
  session; dismissed hints are remembered per person. Hints never cover a dialog or take focus, and the `c` hint is not
  shown when shortcuts are off or on touch devices.

### 4.5 Concurrent editing: advisory presence and optimistic save

Tasklane does **not** do Google-Docs-style simultaneous editing. Two people can edit the same ticket at the same time without
any lock, the system shows who else is working there, and a save that would overwrite someone else's change is caught and
resolved by the person saving. This is deliberately simple: no lockouts, no stale locks, no merge engine running during typing,
and nothing to repair behind the scenes. It is the model Confluence used before real-time editing and the one Wikipedia still uses.

**Principles**

- Anyone with permission can start editing at any time. There are no locks and nothing to wait for or unlock.
- The system tells you who else is editing, so you can talk to them before you both write.
- A conflicting save is never silently lost or silently overwritten: the person saving decides.
- Unsaved text is never lost: drafts are autosaved.

**Presence (advisory only)**

- An avatar stack in the ticket header shows who is viewing. When someone opens the editor on a text field (summary,
  description, a rich-text custom field), everyone else sees a note on that field: "Jana K. is editing the description
  (2 min)". A person about to edit a field someone else already has open sees a gentle warning, and can continue.
- Non-text fields (status, assignee, labels, selects) are saved immediately; changes by others appear in place within about a
  second with a short highlight and the person's name.
- Presence is ephemeral (never stored), refreshed every 10 s and expired after 30 s, and respects a per-user "appear offline in
  editors" privacy setting (admins can disable presence org-wide). Names are always shown, not just colors.
- It never blocks anything. If the realtime connection is unavailable, presence disappears and saving still works.

**Editing and saving**

- Editing a text field opens an editor (rich text with Markdown shortcuts, mentions and issue-key links). **Autosave drafts:** what
  you type is saved as a private draft locally every 2 s and on the server every 10 s, per user and per field, and restored with
  a banner ("Restored your unsaved draft") after a reload, crash or reconnect. Leaving with unsaved changes asks first.
- **Save** is optimistic: the client sends the field's version (`text_seq`) it started from. If it still matches, the text is saved and
  the version advances. If someone saved in between, the server answers with a conflict that carries the current text, the base
  text and who changed it.
- A save in a pending maintenance hold waits briefly, then keeps the draft with "Maintenance in progress, your draft is kept" and
  retries (section 18.2).

**Conflict dialog**

- It says, for example, "Jana K. saved changes to the description while you were editing" and shows three things: their
  changes (base to theirs), your changes (base to yours), and, when the changes do not overlap, an automatic merged result
  (three-way merge at block level: paragraph, list item, table cell, with a text-level merge inside a block that only one side
  changed). Overlapping changes are shown side by side per block for you to choose.
- Actions: **Save merged result**, **Keep mine** (overwrite theirs; the overwritten version stays in history and can be restored),
  **Keep theirs** (discard mine; your text stays in the draft and is copyable), **Copy mine and reload**.
- After you choose, the save is checked again against the latest version, so you can never overwrite a change you have not seen;
  the dialog repeats if someone saved again.

**Scalar fields and structure.** Status, assignee, priority, labels, selects, dates, rank and similar fields are saved
individually with the issue's `version` as the precondition. Different fields merge without any dialog; two people changing the
*same* field produce a small conflict notice ("Jana changed Status to Done; your change was not applied"), with a one-click reapply.

**Comments** are independent and never conflict with each other. Editing your own comment conflicts with nobody. Unsent comment
drafts are private and autosaved like any draft.

**Counters.** `version` counts scalar and structural changes and is what `If-Match` uses for them. Each text field has its own `text_seq`,
bumped by every saved change to that field. Every read (REST, MCP, UI) returns `text_seq` for each text field, and every event
carries both counters.

**History** is a separate store, fed asynchronously from the outbox. It follows Jira's model: for each saved change of a tracked
field, one record with who saved it, when, and the value **before and after** plus a computed diff. Restoring a version saves the
earlier value as a new change; history itself is never rewritten. Not every field is tracked: each field has a "track in
history" setting (on by default for standard and text fields; off for computed, formula, rank and counter fields). History is
eventually consistent (a record may appear a few seconds after the save and is rebuilt from the outbox if the writer lags;
correctness matters more than latency). It is ordinary rows, so GDPR erasure redacts the stored before and after text (keeping who,
when and which field) and legal hold blocks deletion (section 17.3). Notifications for description edits are batched: one
"edited by A" notice for saves by the same person within a few minutes, not one per save. `@mention` in saved text notifies the
mentioned user once.

**API, automation and integration writes** to a text field (the actor appears in history and presence as "Automation rule", "API: bot",
"Agent: claude"):

1. **Anchored patch (preferred, safe).** `old_text` and `new_text`: applied atomically to the current saved text only if `old_text`
   matches exactly once; otherwise it fails with the current context. Edits elsewhere in the field are preserved. This is what the
   MCP `edit_issue_text` tool uses (section 18.3).
2. **Full replace with `If-Match: text_seq` (conditional).** Applied only if the field is unchanged since that `text_seq`; otherwise
   `412 Precondition Failed` with the current text and `text_seq`, and the caller re-reads and retries.
3. **Bare full replace (no precondition).** Last write wins on the text, as in Jira: the field becomes exactly the text sent; edits made
   since the caller read it are overwritten but recorded in history with before and after, so they can be restored. This keeps
   Jira-style clients (a plain `PUT` with the description) working unchanged.

A person with the field open sees a banner ("The description was changed by Automation rule X. Reload, or see changes") and their own
save then goes through the normal conflict dialog. Scalar fields use `If-Match` on `version`. Bulk edits and imports that replace a text
field are conflict-checked per issue and reported per issue.

**Validation on save.** Saved rich text is validated on the server against the schema (allowed nodes, marks and attributes; link
schemes limited to `http`, `https` and `mailto`; maximum depth, node count and size) and sanitized; anything removed is reported to the
saver. The renderer also sanitizes and a strict content security policy applies, so stored content cannot carry script.

**Realtime channel.** One WebSocket per browser tab carries presence and field-change events for the issues, boards and inbox the
person has open; it carries no text synchronization. Events let open pages update in place; if the socket is down, pages refetch on
focus and saves still conflict-check by version.

**Permissions.** Saving needs Edit Issues (and any field restriction); people without it can view and see presence of editors. Presence and
events are authorized per channel like any read, including issue security levels and field restrictions.

**Mirrored Jira issues (section 18.1)** in `read_only` mode are edited in Jira, so they have no editor, no draft and no
presence of editors. In `write_through` mode (section 18.1.1) they have the editor and the drafts; the save is checked
against Jira's current value as well as the mirror's (WT-08), and the same conflict dialog resolves a collision.

**Failure and edge cases**

- Two saves at the same moment: one wins the compare-and-set on `text_seq`, the other receives the conflict dialog.
- Saving while offline or while the server is down: the draft is kept and retried; if the field changed meanwhile, the conflict
  dialog appears on reconnect.
- Browser crash or closed tab: the local draft and the server draft restore the text, at most a few seconds behind.
- Very long descriptions work like any other: nothing is synchronized per keystroke.
- A moved, deleted or converted issue: open editors get a notice and their drafts are kept; a save after a move follows the issue to
  its new key.

## 5. Navigation and management tabs

Primary navigation (rail/drawer): **Home, Issues, Boards, Backlog, Sprints, Filters, Dashboards, Reports, Releases, Admin.**

Management is split into dedicated tabs inside Admin (and per-project Settings), each a full page:

1. **Projects** - create, archive, templates, keys, categories, leads, defaults, lifecycle policies (trash retention,
   clone defaults, move rules, delete and purge permissions).
2. **Issue types and hierarchy**
3. **Workflows** - visual editor (section 9).
4. **Fields** - custom fields (section 8).
5. **Screens and layouts** - which fields appear on create/view/edit/transition, per issue type.
6. **Schemes** - issue type, workflow, field configuration, screen, notification, permission, priority; project-to-scheme mapping.
7. **Permissions and roles**
8. **Users and groups**
9. **Security** - SSO, SCIM, 2FA, IP allowlist, API tokens, session policy.
10. **Notifications and email** (section 13.1)
11. **Automation**
12. **Integrations** - webhooks, OAuth apps, Git hosting, chat, agents and MCP (section 18.3).
13. **Audit and compliance**
14. **Import / export** and **Storage** (section 17.6)
15. **Billing and limits** (hosted edition only, section 17.9)

## 6. Boards

- **Scrum board**: columns map to workflow statuses; shows the active sprint only.
- **Kanban board**: continuous flow, optional backlog column, no sprints.
- Board configuration (separate tab): filter query, columns and status mapping, swimlanes, quick filters, card layout,
  estimation field, working days, sub-filter, issue detail view fields.
- Columns: add, rename, reorder, delete, per-column min/max (WIP) limits with visual breach state.
- Swimlanes by: none, assignee, epic, query, stories (subtasks under parent), priority.
- Quick filters: named JQL-style clauses toggled as chips (Only my issues, Recently updated, custom).
- Cards: type icon, key, summary, assignee avatar, priority, labels, estimate, flag, epic color, due date, blocked
  indicator, subtask progress. Drag to transition (conditions and validators enforced; blocked moves explain why).
- Transition screen pops up when the workflow requires fields (resolution, comment).
- Rank is stored per board using lexorank-style strings; reorder inside a column persists.
- Real-time updates (SSE/WebSocket) with presence on cards.
- Complete sprint dialog: choose where open issues go (next sprint, backlog, new sprint).

## 7. Backlog and weekly sprints

### 7.1 Backlog

- Ranked list of issues not in a sprint, plus a panel per future/active sprint above it. Drag between panels and within.
- Per-sprint header: name, dates, goal, issue count, estimate totals by status category, start/edit/complete actions.
- Epics panel (filter by epic, epic progress), versions panel (assign issues by drag).
- Inline create at the bottom of any panel, with type picker. Multi-select drag. Send to top/bottom of backlog.
- Estimation: story points, original time estimate, or issue count (per board).

### 7.2 Weekly sprints (first-class)

- Cadence setting per board: weekly (default), bi-weekly, custom length. A week is defined by the board's **work calendar**
  (below), not by a fixed Monday to Friday: countries differ (Monday to Friday, Sunday to Thursday, Monday to Saturday,
  other patterns), so the week start day, the working days and the holidays are all settings.
- **Work calendar** (section 2): IANA time zone; **week start day** (any day, for example Monday or Sunday); **working days**
  (any subset, with presets Mon-Fri, Sun-Thu, Mon-Sat and custom); working hours per day (default 8, optional per
  day); **holidays** (a regional holiday calendar chosen by country or region, imported lists, or manual dates); and a
  **week numbering system** (ISO 8601 for Monday-start weeks, a first-day-of-year system for other starts, or none).
  Defined as an org default, overridable per project, board, team and user. The org default is preset from the locale
  chosen at setup.
- **Sprint boundaries:** a weekly sprint starts at 00:00 on the week start day and ends at 23:59:59 on the day before the
  next week start, in the board time zone; its working days and holidays come from the board's calendar. For example, a
  Sunday-start board with a Sun-Thu work week runs Sunday 00:00 to Saturday 23:59 and has five working days. Boundaries
  are computed in local time (a day may be 23 or 25 hours across a daylight-saving change) and stored as UTC instants
  together with a snapshot of the calendar, so reports for closed sprints never change.
- **Capacity and reports:** the burndown ideal line, working-day counts and "days left" use the board calendar. Capacity uses
  each person's own calendar (their working days, hours and regional holidays), so a team spread over several
  countries gets the right availability. Time tracking (section 14) uses the same calendars.
- A calendar change applies from the next sprint; the current sprint keeps its boundaries unless an admin recomputes it.
- **Auto-sprints**: a scheduler creates the next N future sprints (default 2). The default name is `Week {year}-W{week}`
  (for example `Week 2026-W43`) with ISO calendars and `Week of {start}` (for example `Week of 2026-10-18`) otherwise;
  the pattern is configurable with the tokens `{year}`, `{week}`, `{start}`, `{end}` and `{n}`.
- **Auto-rollover**: at sprint end, a job completes the sprint, moves unfinished issues to the next sprint (or backlog,
  per setting), and starts the next sprint. Manual override always available; dry-run preview.
- Capacity planning: per-user availability and hours for the week, over/under-commit indicator in the sprint header.
- Sprint goals, sprint retrospective notes (rich text page attached to the closed sprint).
- Sprint states: future > active > closed. Multiple active sprints per board optional (parallel sprints setting).
- Carry-over tracking: an issue shows "carried over x times" badge.
- Scope change tracking: issues added/removed after start are recorded and shown in the report.

### 7.3 Sprint reports

- Burndown (remaining estimate over time, ideal line, scope changes), burnup, velocity (committed vs completed per sprint),
  sprint report (completed, not completed, removed, added), cumulative flow, control chart, epic burndown,
  version burndown, created-vs-resolved, workload by assignee, time in status.

## 8. Custom fields

- **Types**: single-line text, multi-line (rich) text, number (decimal/integer, unit), date, date-time, checkbox group,
  radio, single select, multi select, cascading select, user picker, multi user, group picker, version picker,
  project picker, URL, labels (free tags), float, duration, issue picker (link to issue), table (rows of typed
  sub-columns), formula (read-only computed), geo/coordinates, file reference, rating, and plugin-defined types.
- **Configuration per field**: name, description, help text, type, options (ordered, color, default, archivable),
  default value, required, uniqueness (optional), min/max/regex validation, searchable, visible in list/board card.
- **Contexts**: each field has one or more contexts - (project set x issue type set) - each with its own options,
  default and required flag. Global context by default.
- **Screens**: fields are placed on screens (create, edit, view, transition) as ordered tabs/sections; screen
  schemes bind screens to operation and issue type; a project picks a screen scheme.
- **Field configurations**: per issue type/project override of required, hidden, description, renderer.
- **Behavior rules** (conditional): show/hide/require a field based on another field's value or on user role.
- **Permissions**: field-level edit/view restriction by role or group.
- **Search**: every custom field is queryable in TQL (`cf[10042]` and by name `"Customer Tier"`), sortable, usable
  in boards, filters, dashboards and CSV export. Typed operators per type (`~`, `in`, `is EMPTY`, `>=`, `was`, `changed`).
- **History**: every change to a tracked field recorded in issue history (section 4.5) and in the audit log.
- **Lifecycle**: create, rename, trash (restorable 30 days), replace with another field via bulk migration, merge options.
- **Limits** (configurable): max fields per instance, per context; warnings when a field is unused or has no context.
- **API**: `/rest/api/3/field`, `/field/{id}/context`, `/field/{id}/context/{ctx}/option`; values read/written under
  `fields.customfield_NNNNN` for Jira client compatibility.

## 9. Workflows

- Statuses with category (To Do, In Progress, Done) and color.
- Transitions: from (one, many, any), to, name, screen, and:
  - **Conditions**: user in role/group, field value, sub-tasks resolved, only assignee/reporter, approval granted.
  - **Validators**: field required, regex, permission, previous status.
  - **Post-functions**: set field, assign, clear resolution, add comment, fire event, call webhook, create subtask,
    update parent, run automation.
- Visual editor (diagram) and list editor; draft/publish with versioning; migration of in-flight issues on publish.
- Resolution handling, default initial status per issue type, "Create" transition.
- Workflow schemes map issue types to workflows per project.
- Import/export workflow as JSON; templates (simple, software dev, service desk, approval).

### 9.1 Issue lifecycle: customization and operations

**What administrators can customize** (all data edited at runtime, SPEC 1 principle 3; each in its Admin tab, section 5)

- Issue types and hierarchy levels (names, icons, which levels can parent which), per project through the issue type scheme.
- Workflows per issue type per project (statuses and categories, transitions, conditions, validators, post-functions,
  transition screens, resolutions, initial status) through the workflow scheme; draft, publish and in-flight migration.
- Which fields, screens and required rules apply per type (sections 8 and 5), and the lifecycle policies below
  (trash retention, clone defaults, move rules, who may delete or purge).

**Rules shared by all lifecycle operations**

- Each operation first produces a **plan** (dry run): what will change, what will be dropped, warnings, and counts. The
  user confirms the plan; bulk operations show it per issue type. The result then equals the plan or is reported
  as a per-issue failure (no half-applied issue).
- Operations on a single issue complete immediately; bulk operations run as resumable background jobs with progress,
  ETA, cancel and a per-issue result report (CSV). Interactive latency is unaffected (section 21).
- Each operation checks permissions (below), takes the per-issue edit lock (a save in progress finishes first; open editors get a notice and keep their drafts), writes one history entry, and emits an event (audit log, webhooks,
  automation triggers, notifications, MCP resource updates).
- Issue identity is the internal id, never the key. Everything that must survive a key change (live-editing channels,
  email thread ids and reply tokens, attachment and blob references, Git links, comments, history) is keyed by that id.

**Convert issue type** (within a project)

- The wizard asks for the new type and shows: status mapping when the workflows differ (default: same-name status, else
  same category, else the new initial status; editable per status), fields that do not exist or are not allowed for
  the new type, and new required fields to fill in.
- Dropped field values are not destroyed: they are kept in the history entry and in a per-issue "removed values" record for
  the trash retention period, so converting back restores them.
- Hierarchy is enforced: a Story cannot become a Subtask without a parent; an Epic that becomes a Story detaches its
  children (listed in the plan); a type with no subtask level converts or detaches subtasks per the plan. Key, comments,
  attachments, links and worklogs are untouched.

**Move to another project** (single or bulk)

- Needs Move Issues on the source and Create or Edit on the target. The wizard maps issue types (per source type, one target
  type), then statuses per type (as for convert), then fields: fields available in the target by context keep their
  values; fields not available are dropped with the same "removed values" record; components and versions are matched by
  name (or created or dropped by choice); security level is mapped or reset to the target default; the sprint is cleared
  unless the target board shares it; the epic link stays only if the epic moves too (otherwise dropped, listed in the
  plan); subtasks always move with their parent.
- **The key changes and the old key remains valid forever.** The issue gets the next number in the target project and
  the old key is stored as an alias. Aliases are never reused or reassigned, and resolve everywhere: browser URLs
  (redirect to the new key), REST, TQL (`key = OLD-12` finds it), MCP, Git linking (a commit that mentions the old key still
  links), email subject keys, webhook payloads (which carry both). Renaming a project key creates aliases the same way.
- Comments, attachments, links, worklogs, watchers, votes and history move with the issue. Open editors
  receive a "moved" notice and follow the issue to its new key; their drafts are kept. Watchers are notified once.
- Cross-organization or cross-instance moves are not supported (use export and import, section 18.2). A project may disable
  moves out of it or into it.

**Clone** (single or bulk, optionally into another project)

- The dialog is a checklist with per-project defaults. Always: summary (prefixed `CLONE - `, configurable), description,
  issue type, priority, labels, components, versions, and custom fields flagged "copy on clone". Optional (defaults in
  brackets): attachments [on], links to other issues [off], subtasks [on, recursively, up to a configured cap],
  comments [off], assignee [clone to the current user or keep], and a "clones" link back to the original [on].
- Never copied: history, worklogs and time estimates (reset), votes, watchers, approvals, Git and deployment links, sprint
  membership (the clone goes to the backlog), resolution (cleared), and the status (reset to the initial status).
  Created time is now and the reporter is the person cloning.
- Attachments are shared by reference (the blob is not copied; SPEC 17.6) and the clone's description becomes an independent
  copy of the current text. Cloning into another project uses the move mapping wizard for type, fields
  and initial status. Needs Browse on the source and Create on the target.

**Delete, trash, restore, purge, archive**

- **Delete is soft.** A deleted issue goes to the **trash** for a retention period (default 30 days, per org and
  per project): it disappears from lists, boards, backlog, search, reports, dashboards and counts, open editors get a notice and keep their drafts, and
  its key stays reserved (numbers are never reused). Links to it show a "deleted issue" placeholder. Deleting a parent
  deletes its subtasks (confirmed in the plan); deleting an Epic detaches its children.
- **Restore** (by the deleter, a project admin or an admin) brings back the issue with its previous status, comments,
  attachments, links and key; sprint membership returns only if that sprint is still open. Subtasks deleted with it
  restore with it.
- **Purge** happens when the retention ends, or immediately by a user with the Purge permission (confirmation, audited, never
  available to agents). It hard-deletes the issue, its drafts, history, derived data and mail
  references, and releases its blobs for garbage collection. Legal hold blocks purge; GDPR erasure purges a person's
  content immediately (and redacts it from histories, per the retention policy of section 17.3). The audit event for the
  deletion is kept.
- **Archive** is a separate, reversible issue state for finished work: read-only, hidden from boards and default searches,
  still found by a filter and still counted in historical reports. A project can archive automatically N days after
  resolution. Project archive (section 2) is unchanged.
- **Mirror mode (18.1):** mirrored issues are read-only, so convert, move, clone, delete and restore are unavailable for them
  (the UI links to Jira). An issue deleted or moved in Jira is reflected by the sync: deleted ones are removed from the
  mirror (Jira has no trash), and a Jira key change is recorded as an alias.
- **MCP (18.3):** no delete or purge tool exists; `move` and `clone` tools, if enabled, need their own scopes and are
  subject to the same plan and gates.

## 10. Search and filters

- **TQL**: JQL-compatible subset: fields, operators (`=`, `!=`, `~`, `in`, `not in`, `is`, `was`, `changed`, `>`, `<`),
  `AND`/`OR`/`NOT`, `ORDER BY`, functions (`currentUser()`, `openSprints()`, `startOfWeek()`, `endOfWeek()`,
  `membersOf()`, `releasedVersions()`, `now()`, `startOfDay()`), parentheses, text search with stemming.
- **Plain search** (the search box's default; a Text/TQL toggle switches, and the URL keeps `syntax=text|tql`): words and
  `"phrases"`, `AND`/`OR`/`NOT` in any case, brackets, `-word`; words side by side must all match. Each word runs as
  `any ~ "word"`: summary, description, labels and comments, a summary substring (three characters or more), the issue key,
  and assignee or reporter by a word of the name or the start of the email (`Alan` finds Alan Polevoi, not Catalan).
  `any` is usable in TQL too. API: `GET /api/v1/issues?q=...&syntax=text`; without `syntax` the query is TQL.
- Query builder UI (basic mode, no syntax needed) with field/operator/value pickers and autocomplete.
- Saved filters: name, description, owner, sharing (user, group, project, org), favourite, subscriptions (email digest).
- Views: list, detail split, table with chosen columns, calendar, timeline. Column picker persists per user.
- Bulk edit: transition, assign, edit fields, move, delete, watch; preview and per-issue failure report.
- Export: CSV, XLSX, JSON; scheduled export.
- Global search: issues, projects, boards, filters, people, comments.

### 10.1 Search engines: PostgreSQL and OpenSearch

PostgreSQL full-text and trigram search cannot be relied on to hold a 400 ms p95 at 5 million issues, so Tasklane has two
search engines behind one `Searcher` interface and one query language (TQL compiles to either):

| Engine | When | What it costs |
|---|---|---|
| `postgres` (default) | Up to **100,000 issues per instance** (all organizations together), the size at which it is designed and tested to meet the search target | Nothing extra to run |
| `opensearch` | Above that size, and for any deployment that wants faster, richer search | **A new service to deploy, secure, size, monitor and upgrade, and a second copy of the data that has to be kept consistent** (below) |

**PostgreSQL engine behavior** (the default; designed and tested for up to **100,000 issues per instance**, counting all
organizations)

- **Searchable text:** summary, description, labels, components and text custom fields of the issue, and comments (kept apart
  so that internal comments can be filtered). Typed non-text fields are filtered with ordinary indexes, not searched as text.
- **Language handling:** language-neutral by default (case and accent insensitive, no stemming), so multilingual instances do
  not get wrong-language stemming; an administrator can choose a language configuration (for example English stemming) per
  instance, which rebuilds the search index online. Without stemming, "running" does not match "run"; this is the stated
  trade-off.
- **Identifiers:** a token that looks like an issue key (`ABC-123`, including old keys through aliases) is resolved as an exact
  key lookup before text search and listed first. Keys inside descriptions and comments are searchable. Other code-like
  strings (`ERR_CONN_RESET`, paths, versions) are best-effort in text, and substring and prefix search is supported on summaries
  and keys with at least three characters.
- **Ranking:** results use relevance with recency as the tie-break: a match in the summary outranks one in the description,
  which outranks one in a comment. Relevance is computed over the most recent 2,000 matches; broader recall is available by
  sorting by updated time. An explicit `ORDER BY` always wins. Totals above 10,000 are shown as "10,000+".
- **Typeahead:** key exact match first, then summaries by prefix and similarity, boosted by what the user viewed recently.
- **Growth:** the instance warns administrators at 80,000 issues and again at 100,000 that search should move to OpenSearch.
  Nothing is cut off; search simply stops being guaranteed to meet the target.

**OpenSearch is an additional service.** It is an OpenSearch cluster (one node for small installs, three or more with replicas
for production) run by the operator or provided as a managed service. It has its own TLS and credentials (stored as secrets),
network exposure limits (never public), capacity planning (disk, memory, shards), monitoring, rolling upgrades within a
documented compatibility range, and a Compose and Helm profile. It is not bundled or embedded (section 17.9). Because the
index can be rebuilt from PostgreSQL, a backup of OpenSearch is optional; the recovery plan is a reindex with a published
time estimate.

**It is a second copy, never the source of truth.** PostgreSQL stays authoritative for every fact, permission and displayed
value. The index holds a derived, searchable copy (key and aliases, summary, description and comment text, typed custom
field values, status, type, people, labels, components, versions, dates, development summary, optional attachment text)
plus the access fields used for filtering. Everything below follows from that.

- **Indexing lag is acceptable and expected.** Changes reach the index through the outbox, batched and idempotent; the target
  freshness is 5 s p95, with an alarm at 60 s. When lag exceeds a threshold the UI says "search results may be a few seconds
  behind". Opening an issue always reads PostgreSQL, and a user's own just-made changes are overlaid on list results so
  they do not appear to vanish.
- **What must never be eventual.** Boards, backlog and sprints, workflow and automation conditions, bulk-operation targets,
  permission checks, notifications, exports, MCP and REST single-issue reads, and any query that needs history (`was`,
  `changed`) always run on PostgreSQL. Search boxes, issue lists and filters, dashboard gadgets and typeahead use the index.
- **Ordering and idempotency.** Each index write carries the issue's monotonic `search_seq` as an external version, so a
  late, duplicated or replayed update can never overwrite a newer one. Failed writes retry with backoff and then go to a
  dead-letter queue with an alert; an OpenSearch outage never blocks writes (the outbox backlog grows within a configured
  cap and drains afterwards).
- **Reindex.** A full online reindex builds a new versioned index while writes continue (dual write), verifies counts and
  checksums, and switches an alias atomically; it is resumable, throttled so interactive latency is unaffected, and can be
  limited to a project. It is needed for mapping or analyzer changes, new custom field types, corruption, engine upgrades
  and restores.
- **Reconcile.** A periodic job compares PostgreSQL and the index by project and `search_seq` buckets and repairs missing,
  stale and orphaned documents (for example after a purge), so silent drift is bounded and visible in Admin.
- **Permission filtering is layered so lag cannot leak data.**
  1. *Pre-filter at query time* from the user's current access, computed from PostgreSQL and cached: projects the user may
     browse and security levels they may see, applied as index filters. Changes to who has access to a project take effect
     immediately without reindexing, because the filter is built from the current permission data, not stored in the index.
  2. *Authorization on the way out:* the index returns only issue ids and scores. Every returned id is re-checked in
     PostgreSQL (batch check against the permission cache) and all displayed data, including snippets and highlights, is read
     from PostgreSQL after that check. An issue moved to a restricted project, deleted or re-secured during an indexing
     lag is dropped here.
  3. *Field and comment rules:* clauses on fields the user may not view are rejected exactly as on PostgreSQL; internal
     comment text and restricted fields are indexed in separate fields that are only queried for users entitled to them.
  4. *Limits:* a user with more accessible projects than the configured term limit falls back to an exclusion filter, then to
     a PostgreSQL query with a warning.
  5. *Tenancy:* the organization filter or index prefix is added by the client layer and cannot be omitted.
- **Totals and paging.** Totals computed from the index are approximate (they may include a few hits removed by step 2 or lag);
  the UI shows "about N" above a threshold. Deep paging uses stable cursors (`search_after`), not offsets.
- **Failure.** If OpenSearch is down or unhealthy, search falls back to the PostgreSQL engine with a notice while the
  instance is small enough; on large instances the fallback is limited to indexed shortcuts (assigned to me, recent, by
  key) and a clear "search is temporarily limited" message, never a timeout.
- **Parity.** One TQL conformance suite runs against both engines; clauses the index cannot answer exactly (history, some
  functions) are routed to PostgreSQL automatically.
- **Admin tab: Search.** Engine and connection status, index health and lag, indexing queue and dead letters, reindex and
  reconcile controls and history, permission-filter statistics, and the engine-size guidance.

## 11. Dashboards

Gadgets: filter results, assigned to me, activity stream, pie, two-dimensional filter statistics, created vs resolved,
sprint burndown, sprint health, average age, time since, heat map, workload, release burndown, saved filter table,
text/markdown, iframe (allowlisted). Layouts 1-3 columns. Sharing, favourite, default dashboard per user. Auto-refresh.

## 12. Releases, calendar, roadmap

- Versions per project, release notes generation, release/unrelease, version progress, unreleased issue warnings.
- Calendar view of due dates, sprints, releases. Timeline/roadmap of epics with dependencies and drag to reschedule.

## 13. Notifications and watching

- Watch/unwatch, auto-watch on comment/assign/report (user setting).
- Event types: created, updated, assigned, commented, comment edited, transitioned, resolved, reopened, mentioned,
  worklog, attachment, link, approval requested/decided, sprint started/completed.
- Channels: in-app inbox (Gmail-like, read/unread, archive), email (outbound and inbound, section 13.1),
  push (mobile/web), webhooks, chat integrations (Slack, Teams).
- Notification schemes per project; per-user overrides; quiet hours; digest mode.

### 13.1 Email (outbound and inbound)

Email is a full channel: notifications go out threaded per issue, and people can reply to a notification to comment, or
write to a project address to create an issue. Both directions are configured in Admin (Notifications and email) and
work without any cloud service (SMTP and IMAP only), with optional provider integrations.

**Outbound**

- **Transports** behind one `Mailer` interface: SMTP relay (host, port, implicit TLS or STARTTLS, authentication,
  certificate verification, from address, reply domain) as the default, and optional provider APIs (Amazon SES,
  SendGrid, Mailgun, Postmark). A test-send and a configuration check are in Admin.
- **Queue:** mail is sent by a worker from an outbox, never on the request path; retries with exponential backoff,
  per-provider rate limits, one message per event and recipient (idempotency key) so retries never duplicate, and
  a delivery log with status per message.
- **Threading per issue:** every message of an issue carries stable headers so Gmail, Outlook and Apple Mail group them: a
  deterministic `Message-ID`, `In-Reply-To` the previous message of the issue, and `References` that always start with
  a virtual per-issue root id. Subject format `[ABC-123] summary` (configurable), with no churn when the summary changes
  so threads do not split.
- **Content:** a plain-text part always plus an HTML part; localized per recipient (language, time zone); light,
  dark-safe HTML with no remote images and no tracking pixels; a clear reply marker line ("Reply above this line") and
  the reply address in `Reply-To`; `List-Unsubscribe` and notification preferences link; `Auto-Submitted:
  auto-generated`, so other systems do not auto-reply. Internal comments and restricted issues are never sent to
  recipients without access (visibility is evaluated at send time).
- **Preferences:** per-user and per-project choices, quiet hours, and digest mode (SPEC 13); email respects them.
- **Deliverability:** an Admin check shows the SPF, DKIM and DMARC records needed for the sending domain and whether they
  resolve; optional DKIM signing by Tasklane when the relay does not sign; the return path is unique per message (VERP).
- **Bounces and complaints:** bounce messages (DSN) and provider webhooks are matched to the original message. A hard bounce
  suppresses the address immediately, repeated soft bounces suppress it after a threshold; suppressed addresses are
  listed in Admin and flagged on the user ("email is bouncing"), with a way to clear. Spam complaints from provider
  feedback loops suppress the address. Suppression is never applied to in-app or push notifications.

**Inbound**

- **Sources** (one or more per instance): an IMAP mailbox (polling with IMAP IDLE where available, checkpointed by
  UID), a provider inbound webhook (signature verified), or an optional built-in SMTP listener for direct MX delivery.
  Messages are stored raw (`.eml`) in the blob store and processed asynchronously.
- **Reply by email:** replying to a notification adds a comment to that issue as the replying user. The reply address
  carries a signed, expiring token bound to the issue and to the recipient (`reply+token@mail-domain`), so forwarding
  a notification does not hand its power to someone else. Fallbacks when headers are lost: `In-Reply-To` and
  `References` against stored message ids, then the `[ABC-123]` key in the subject (sender must be allowed to comment).
- **Create issue by email:** each project has an address (`ABC@mail-domain`, with optional plus-tags such as
  `ABC+bug@` for issue type or labels). Subject becomes the summary, body the description (Markdown), attachments are kept,
  the sender becomes the reporter, and `Cc` addresses that match users become watchers. Later replies thread onto the
  created issue.
- **Sender matching:** the From address is matched to a user's verified email addresses (and aliases). Per-project policy
  for unknown senders: reject, quarantine for review, or accept as an external reporter (service desk, SPEC 15). An
  inbound message acts with that user's permissions only (Add Comments, Create Issues, security levels); it never
  widens access.
- **Spoofing protection:** a message is accepted only with proof of origin. Tasklane verifies DKIM itself on the stored raw
  message (works with any transport) and checks DMARC alignment; SPF and the receiving server's verdict are used only
  from a connection marked as a trusted receiver (or from the provider's verdict fields). Replies additionally require
  a valid token and a sender that matches the token's user. Anything unproven goes to **quarantine**, not into the
  tracker. No automatic replies are sent to unauthenticated senders (no backscatter).
- **Loop and abuse protection:** ignore auto-generated mail (`Auto-Submitted`, `Precedence: bulk/junk/list`,
  `X-Auto-Response-Suppress`, null return path, out-of-office and mailing-list headers), mail from Tasklane itself
  (instance header and loop counter), bounces (routed to bounce handling), and duplicates (by `Message-ID`). Rate
  limits per sender, per sender domain, per issue and per project; maximum message and attachment sizes (default 25 MB);
  attachments go through the blob store rules and antivirus (SPEC 17.6).
- **Quote stripping:** the comment contains only what the person wrote. Quoted history and signatures are removed using
  conservative, high-precision rules (reply marker line, client-specific quote markup from Gmail, Outlook, Apple Mail,
  Yahoo and Thunderbird, "On ... wrote:" and original-message header blocks in many languages, `>` blocks, signature
  delimiters). Inline replies (answers written between quoted lines) are kept in full as quoted blocks. When unsure,
  Tasklane keeps the text; the raw message is always retained and linked from the comment ("view original email").
- **Content safety:** HTML is converted to a safe subset (no scripts, no remote content), charsets and encodings are decoded
  with bounded resource use, and the text is treated as untrusted (provenance-tagged for MCP, SPEC 18.3).
- **Result visibility:** accepted mail becomes a comment or issue with the source "via email"; every message has a
  recorded outcome (accepted, ignored with reason, quarantined, rejected) visible to admins. Quarantine can be
  released or deleted, with audit.

**Interaction with other features**

- **Latency (21):** outbound mail is queued within the request that triggered it and normally leaves within 30 s; an
  inbound message is a visible comment within 10 s p95 of receipt (IMAP IDLE and webhooks) or within the poll interval
  (default 30 s).
- **Retention and privacy:** raw messages follow retention policy and GDPR erase (SPEC 17.3); credentials for mailboxes
  and relays are stored as secrets; addresses and message bodies are not logged.
- **Mirror mode (18.1):** Tasklane email is off for mirrored projects (Jira sends its own); inbound mail addressed to a
  mirrored project is quarantined with the reason "mirrored project" and no reply is sent.
- **Import/export (18.2):** the mail archive is optional in full-fidelity exports; connections and secrets are never exported.

**Admin tab: Email:** outbound configuration and test, DNS check, inbound connections and health, address mapping per
project, sender policy, quarantine, suppression list, delivery log and bounce rate, templates.

## 14. Time tracking

Original/remaining estimate, worklogs (manual, timer), time spent aggregation to parent/epic, working days and hours from the work calendar (section 7.2),
timesheet report, "log work" quick action. Optional approval of timesheets.

## 15. Approvals and service-desk style flows

- Approval step in a workflow: approver set (users, groups, field-derived), rule (all/any/N), deadline, reminder.
- Approvers act from the ticket, email or push notification. Decision recorded with comment, shown in the thread.
- Request types and a customer portal (optional module): forms mapped to issue types, SLA clocks (time to first
  response, time to resolution) with pause conditions, queues, canned responses, customer-visible vs internal comments.

### 15.1 Customer (portal) identity

Customers are people who raise and follow requests through the portal; they are **not staff** and have a deliberately
small identity.

- **Principal type `customer`:** an email-based identity with optional name, language and time zone, separate from users.
  A customer can be linked to a customer organization (a company), which can share requests among its members.
  A customer can be promoted to a staff user, keeping their history.
- **Authentication:** email one-time code or magic link by default (no password needed), optional password, and optional
  sign-in through a customer identity provider (OIDC or SAML) configured per portal. Sessions are separate from staff
  sessions and cookies, short-lived, and cannot reach the staff UI, the REST API or MCP. Staff 2FA policy does not
  apply; step-up can be required per request type.
- **Visibility:** a customer sees only requests they created or that were shared with them or their organization, and only
  the customer-visible fields and customer-visible comments; internal comments, internal fields, work logs,
  approvals, linked internal issues and Git data are never exposed. Attachments follow the customer-visible rule.
  Customer-visible text typed by staff is shown as such before sending.
- **Seats:** customers are never licensed seats and never count against any user limit; hosted plans may meter
  active customers separately (section 17.9). Staff with a project role are the licensed population.
- **Abuse and spam controls:** email verification before the first request, rate limits per address, IP and request type,
  optional CAPTCHA, honeypot and size limits, block list and allow list by address and domain, attachment rules of
  section 17.6, and the inbound email protections of section 13.1 (customers writing by email follow the same
  authentication rules, and an unverified sender cannot create requests).
- **Privacy:** customers are data subjects: export and erase on request (section 17.3), retention per portal, consent text
  and a privacy notice on the portal, no tracking scripts.
- **Notifications:** emails to customers carry only customer-visible content and a portal link; replies by email become
  customer-visible comments from that customer (section 13.1).

## 16. Automation

Rule = trigger + conditions + actions, with audit log and dry-run.

- Triggers: issue created/updated/transitioned/commented, field value changed, scheduled (cron), sprint started/
  completed, version released, webhook received, manual button.
- Conditions: JQL match, field compare, user in group, related issues.
- Actions: transition, assign, edit field, comment, create issue/subtask, link, send email/notification, call webhook,
  clone, log work, add to sprint, branch (for each related issue).
- Smart values (`{{issue.key}}`), rule limits and loop protection.

## 17. Enterprise requirements

### 17.1 Identity and access

- SSO: SAML 2.0 and OpenID Connect; SCIM 2.0 provisioning (users, groups); just-in-time provisioning.
- Local accounts optional; password policy; 2FA (TOTP, WebAuthn); session timeout; device/session list and revoke.
- API tokens (scoped, expiring), OAuth 2.0 apps (3LO and client credentials).
- Domain verification, managed accounts, IP allowlist, email domain restrictions.
- **Outbound request (egress) policy.** Every server-side fetch of an admin- or user-supplied URL (webhooks, Jira and Git
  provider base URLs, import from URL, attachment fetching during import, link unfurling) goes through one HTTP client
  that enforces: a per-connection or global allowlist, blocking of loopback, link-local, cloud-metadata and private
  address ranges unless the connection explicitly allows them, DNS-rebinding protection (resolve once, connect to the
  checked address), limited redirects (re-checked at each hop), size and time limits, and no credentials forwarded across
  hosts.

### 17.2 Authorization

- Global permissions, project roles (Administrator, Developer, Reporter, Viewer, custom), groups.
- Permission schemes (browse, create, edit, assign, transition, comment, delete own/any, manage sprints, manage
  watchers, view dev tools, set security level, etc.).
- Issue security levels (restrict individual issues to roles/groups); comment visibility restrictions.
- Field-level and attachment-level restrictions. Project-level "private" mode.

### 17.3 Audit, compliance, retention

- Immutable audit log (who, what, when, before/after, IP, user agent), filter and export, streaming to SIEM
  (syslog/HTTP/S3). Admin actions and permission changes always logged.
- Data retention policies per project, legal hold, GDPR export and erase of a user, data residency selection.
- Attachment virus scanning hook, DLP hook, configurable allowed types.

### 17.4 Operations and scale

- A single Go binary, stateless app nodes behind a load balancer, PostgreSQL primary with read replicas (metadata only), a pluggable blob store for
  large binaries (local filesystem by default, S3 and S3-compatible optional; section 17.6), Redis for
  cross-node pub/sub and cache invalidation (required with 2+ nodes, not used on a single node), OpenSearch as an additional service for large instances (section 10.1).
- Background workers: indexing, email, webhooks, automation, auto-sprint/rollover, retention, import.
- Health, readiness, Prometheus metrics, OpenTelemetry traces, structured logs.
- Backup/restore tooling, zero-downtime migrations, online reindex.
- Multi-org (tenant isolation by schema or row level security), rate limiting, quotas, per-org feature flags.
- High availability and disaster recovery documented targets (RPO, RTO).
- Backups read a consistent PostgreSQL snapshot taken at a pause marker (section 18.2), with the blob store captured after it;
  restore runs in pause mode.

### 17.5 Admin and governance

- Org-wide dashboards for adoption and usage, license/seat accounting, inactive user cleanup.
- Project templates and "locked" schemes governed centrally; change requests for scheme edits (optional).
- Bulk project operations, archive/restore, cloning a project with its config.
- Global announcement banner, maintenance mode.

### 17.6 Blob storage (screenshots, videos, logs, large files)

PostgreSQL is a poor place for large binaries (bloat, slow backups, replication lag, memory pressure), so it holds
**metadata only**. Bytes live in a pluggable **blob store**. This covers attachments, screenshots, screen recordings,
build and test logs, exports, thumbnails and any other large object, including files attached by agents (section 18.3).

**Backends** (one `BlobStore` interface, chosen per instance, optionally per blob class)

| Backend | Use | Notes |
|---|---|---|
| `fs` (default) | Single node, on-prem, evaluation | A local directory or mounted volume. No extra services. Multi-node needs shared storage (NFS or a clustered filesystem); the server refuses to start multi-node on node-local storage unless the operator confirms the volume is shared. |
| `s3` (optional) | Scaled and cloud | AWS S3 and S3-compatible stores (MinIO, Ceph, Cloudflare R2, and similar). Credentials by role, instance profile or key pair; custom endpoint and path-style option for self-hosted stores. |
| Tiered (optional) | Cost and capacity | Hot tier on `fs`, cold tier on `s3`, moved by age, size or class rules; reads are transparent. |

**Blob classes and policy.** Each class has its own max size, allowed types, retention, tier and derived-data rules:
`image` (screenshots, photos), `video` (screen recordings), `log` (large text), `document`, `archive`, `export`.
Defaults: image 25 MB, video 2 GB, log 1 GB, document 100 MB; all configurable per org and per project.

**Storage model**

- Objects are **immutable** and **content-addressed** by SHA-256 (verified on write and optionally on read), so identical
  files are stored once per org (deduplication) and corruption is detectable. An attachment row points to a blob id;
  deleting an attachment drops a reference, and a background GC removes unreferenced blobs after the retention window.
- Layout: `fs` shards by hash prefix (`ab/cd/abcd...`), `s3` uses one bucket with an org prefix. Names carry no user
  data (the original filename lives in the metadata row).
- **PostgreSQL references the objects in the store.** A `blob` row holds the object **locator** (backend, bucket or root,
  key or path, optional S3 version id), plus hash, size, MIME (sniffed and declared), class, tier, scan state,
  refcount, and dimensions or duration where known. Attachments, inline images in descriptions and comments, derived
  files (thumbnails, posters, log indexes), exports and agent evidence all point to a `blob` row by id; no row ever
  contains file bytes. Because locators are data, moving a blob between backends or tiers is a metadata update plus a
  copy, and the application never builds storage paths from user input.

**Upload**

- Resumable and chunked from every client (browser, mobile, API, MCP), with per-file progress and cancel. A small
  pasted screenshot takes a single-request fast path.
- Two modes, selected by config: **proxy** (default; the app streams the upload to the store with constant memory,
  hashing as it goes) and **direct** (`s3` only; the client uploads parts to the store with presigned multipart URLs and
  the server validates size, hash and type on completion before the attachment becomes visible).
- Uploads land in a staging area and are committed atomically after validation and scan. Abandoned uploads are removed
  after 24 hours. Upload traffic never passes through PostgreSQL and uses bounded, separate worker capacity so it cannot
  starve interactive requests.

**Download and preview**

- Streaming with HTTP **range requests** (video seeking, log tailing), `ETag` and conditional requests; content-hash
  URLs for derived files with long immutable cache headers.
- Authorization is checked when the link is issued: the app mints a short-lived signed URL (app-signed for `fs`,
  presigned for `s3`; redirect or proxy per config) only after the usual issue, security-level and attachment permission
  checks. Expiry is short (minutes), so revoked access ends quickly.
- Served from a separate origin, with `Content-Disposition: attachment` and `nosniff` for any type not on the safe-inline
  list; only images, video, audio, PDF and plain text preview inline.

**Derived data** (worker-generated, stored in the same store, regenerable, never blocking an upload)

- Images: thumbnails and a downscaled preview, optional EXIF stripping.
- Video: poster frame and duration always; optional transcode to a web-friendly format when the operator enables it
  and the media tooling is installed; otherwise the original is served and the previewer falls back to download.
- Logs: stored compressed at rest, with a line index so the previewer can open **the last N lines instantly**, page
  through, search within the file, and render ANSI colors, without loading the file. Only the first configurable
  portion of a log is added to the main search index.
- Failure of any derived step shows a generic icon and a retry; the original is unaffected.

**Security**

- Antivirus/DLP hook runs before a blob is marked `clean`; `scanning` blobs are not downloadable and `blocked` ones are
  quarantined (SPEC 4.3). Declared and sniffed types are both checked; archive and decompression bombs are limited.
- Encryption in transit always. At rest: filesystem or volume encryption for `fs` (or application envelope encryption
  with per-org keys), SSE-S3 or SSE-KMS (or client-side encryption) for `s3`. Key rotation without re-uploading.
- `s3`: private bucket only, least-privilege access policy scoped to the Tasklane prefix, no public ACLs.

**Lifecycle and governance**

- Retention per class and project; legal hold blocks GC; soft-delete window (default 30 days) before bytes are removed;
  GDPR erase of a user removes their blobs when no other reference remains, including derived files.
- Quotas per user, project and org with warnings; bandwidth and concurrent-upload limits; usage reported by class.
- **Backend migration** tool (`fs` to `s3`, `s3` to `fs`, between buckets): online, resumable, copy then verify hash
  then switch the pointer then delete, with rollback.
- **Consistency and backup:** blobs are written before the metadata that references them, so a restored database never
  points at a missing blob created after it. `tasklane blob fsck` reports missing, orphaned and corrupt objects and can
  repair references. Backup of `fs` blobs is snapshot-ordered with the database; for `s3`, versioning or replication is
  documented.

**Interaction with other features**

- **Import/export (18.2):** bundles stream blobs from and to the store with checksums and never load them in memory.
- **MCP (18.3):** `attach_file`, `attach_text` and `record_test_result` write through the same store, with agent-specific
  size caps and quotas.
- **Mirror mode (18.1):** attachment metadata is mirrored and the file is fetched from Jira into this store on first view
  (or eagerly, per setting), with size caps and antivirus; a file deleted in Jira loses its blob reference.
- **Latency (21):** thumbnails load lazily and never delay the 300 ms data path; large transfers run on separate capacity
  and must not push interactive p95 over budget.

**Admin tab: Storage**

Backend configuration and connection test, usage by class and project, quotas, retention and tiering rules, migration
progress, `fsck` results, scan queue, and health of the store.

### 17.7 Installation, packaging and configuration

**Artifacts** (all signed, with an SBOM; SPEC 21)

| Artifact | Contents | Use |
|---|---|---|
| Container image (linux/amd64 and arm64) | The Go binary, non-root, read-only root filesystem | The primary artifact |
| Docker Compose files (`deploy/compose/`) | Single-node and multi-node reference stacks, the same files used for testing | Evaluation, small and medium installs |
| Helm chart | App, optional ingress, references to external PostgreSQL, Redis and object storage | Kubernetes |
| Linux tarball with systemd unit | The binaries and the engine, a sample unit and config | Single node without containers |

The Compose files are also the test environment. An `interop` profile starts two app nodes with real PostgreSQL, Redis
or Valkey and OpenSearch, and a few end-to-end tests prove basic interoperability with those real services (search on
OpenSearch, presence and events across nodes through Redis); the rest of the tests may use in-process doubles (TESTPLAN 3.3).

Not shipped in v1: Windows services, deb/rpm packages (the tarball covers bare Linux), a managed installer for PostgreSQL.
Browsers: the latest two versions of Chrome, Edge, Firefox and Safari.

**Prerequisites:** PostgreSQL 15 or newer (with the `pg_trgm` and `unaccent` extensions; no superuser needed after the extensions are created once);
Redis or Valkey 7 or newer, required only with two or more app nodes; an optional S3-compatible store (SPEC 17.6); an
SMTP relay for email (13.1); TLS terminated by a reverse proxy or the built-in listener; a proxy must pass WebSocket
connections and allow idle timeouts above 60 s.

**First run:** `tasklane init` (or the web setup page on first start) asks for the database, public base URL, first
administrator, blob store and mail relay, applies migrations and starts. `tasklane doctor` checks the environment and prints
what is wrong and how to fix it: PostgreSQL version and extensions, Redis presence when more than one node is configured,
blob store write test, SMTP and IMAP login, clock skew, file-descriptor limits, whether the proxy passes WebSockets, and outbound reachability to configured Git and Jira hosts under the egress policy.
Sizing guidance (users, concurrent users, connections per node) is published from the performance tests (section 21).

**Configuration**

- **Layers and precedence:** built-in defaults, then a YAML config file, then environment variables (`TASKLANE_...`),
  then command-line flags; later layers win. Secrets can be supplied as `*_FILE` paths (container and Kubernetes
  secrets) or resolved from a secret manager reference; secrets are never written to logs or diagnostics.
- **Bootstrap versus runtime settings:** infrastructure (database, Redis, blob store location, listen address, TLS,
  secrets sources) is bootstrap configuration. Everything an administrator should change without a deploy
  (mail, SSO, limits, quotas, retention, Git and Jira connections, feature switches) is stored in the database and
  edited in Admin, with audit.
- **Schema and reference:** every key is defined in a typed schema with description, default, units and whether it is
  hot-reloadable. The published configuration reference is generated from that schema and checked in CI for drift.
  `tasklane config validate` and `tasklane config print --redacted` are available; unknown keys and bad values fail startup
  with a suggestion.
- **Reload:** a documented safe subset (log level, rate limits, feature switches, quotas) reloads on signal or from Admin;
  everything else requires a restart and says so. An invalid reload is rejected and the previous configuration stays.
- **Key groups:** `server`, `database`, `redis`, `blob`, `mail`, `auth`, `realtime` (presence and event limits, draft intervals),
  `search`, `egress`, `limits`, `features`, `telemetry`, `observability`.
- **Deprecation:** a renamed or removed key keeps working with a warning for one minor release.

### 17.8 Upgrade, migration and version skew

**Versioning and support.** The product follows semantic versioning. Security fixes go to the latest two minor releases.
Upgrades are supported from the previous two minor releases (N-2 to N); older versions must step through a supported
release. Release notes list schema changes, configuration changes, and any required operator action.

**Database migrations**

- Forward-only, versioned, checksummed, run by `tasklane migrate` (a single runner at a time via an advisory lock) or
  automatically on a single node; multi-node deployments run it as one explicit step before the rollout.
- **Expand and contract.** Migrations that add (tables, nullable columns, indexes created concurrently) are `expand` and are
  safe while the previous version still runs. Migrations that remove or tighten are `contract` and run only in
  `tasklane migrate finalize`, after every node is on the new version. Data backfills run as background jobs with progress,
  not inside a migration.
- **No long locks:** a CI lint rejects statements that take long locks or rewrite large tables in an `expand` migration;
  lock and statement timeouts guard every run; `tasklane migrate plan` shows the SQL, lock level and an estimated duration.
- Backups are prompted before applying migrations; a failed migration leaves no partial schema (transactional DDL where
  PostgreSQL allows it, otherwise resumable steps).

**Rolling upgrades and version skew**

- **Supported skew for live traffic:** nodes of version N-1 and N may run together during a rollout (zero downtime from N-1
  to N). Upgrading from N-2 is supported with all nodes stopped (offline), then started on N.
- **Between nodes:** messages on the pub/sub channels and the cluster protocol carry a version and ignore unknown fields,
  so N-1 and N nodes interoperate (cross-node events, presence, cache invalidation, pause).
- **Cluster version gate:** nodes register their version; a new behavior that older nodes cannot handle stays off until
  every live node reports at least that version, then activates automatically (or after an admin confirms for risky
  ones). A node refuses to start against a schema outside the range it supports.
- **Clients:** the API and the WebSocket handshake carry client and protocol versions; the server accepts the previous
  client version, tells older clients to reload, and a cached PWA shell keeps working against the new server for the
  skew window. Offline mutations queued by an older client are replayed after upgrade or surface a visible conflict;
  they are never dropped silently.
- **Rollout procedure:** drain one node at a time (readiness fails, WebSocket clients get a jittered reconnect hint), upgrade, wait for health, continue. A single node upgrades with a short pause (target under 30 s) while
  editors keep their drafts and saves retry afterwards, with a maintenance banner.
- **Rollback:** within the expand-only window, redeploy the previous version. After `finalize`, rollback is a restore from
  backup (pause mode, SPEC 18.2). Downgrade across a contract migration is not supported.
- **Data formats:** the update log and snapshot encoding, exchange bundles, API and WebSocket protocols each carry a version
  and stay readable by the previous two releases.

### 17.9 Editions, hosting and licensing

- **One codebase, no feature-gated editions.** The self-hosted build contains every feature in this specification (SSO, SCIM,
  audit, permissions, MCP, Git, import/export, S3 storage, email). There are no license keys, seat
  enforcement or phone-home in it; air-gapped installs are supported; the update check and anonymous usage statistics
  are opt-in and off by default.
- **Self-hosted** is the primary model: single organization by default, optional multiple organizations on one instance
  (SPEC 2, 17.4), limits and quotas set by the operator.
- **Hosted edition** is the same build operated as a multi-tenant service: tenant isolation (17.4), plans, seat and storage
  quotas, metering and billing (Admin tab 15), region and data-residency choice, managed upgrades, backups and support
  levels. Billing, metering and plan enforcement are isolated behind interfaces and configuration that are inactive in a
  self-hosted install.
- **Licence (proposed decision):** all code in the repository under **Apache-2.0**, with contributions accepted under a
  Developer Certificate of Origin sign-off. Reasons: it is accepted by enterprises that block AGPL, includes a patent
  grant, and is compatible with the planned dependencies. Trade-off: anyone may host it commercially; the answer to that is
  the trademark policy and the quality of the operated hosted service, not the licence. AGPL-3.0 is the alternative if
  protection against resale outweighs adoption. The dependency policy below holds for either choice.
- **Dependency policy:** components linked into or bundled with a release must be MIT, BSD, ISC, Apache-2.0, MPL-2.0 (unmodified,
  file-level), CC0 or Unlicense (OFL for fonts). GPL, AGPL, SSPL, BUSL, Elastic and RSAL licensed code is not linked or
  bundled. Planned core dependencies (ProseMirror, the Go standard library and the PostgreSQL and AWS SDK
  drivers) are expected to fit; this is verified by an automated licence scan of Go modules, npm packages, Rust crates and
  base images on every build, with a generated NOTICE file and SBOM shipped in each artifact.
- **Components run as separate processes, not bundled:** PostgreSQL, Redis or Valkey (Valkey is BSD licensed; Redis's current
  licence is the operator's concern), S3 stores such as MinIO (AGPL, used only in test environments), ffmpeg for optional
  transcoding, ClamAV, and GitLab CE in tests are operator-installed or test-only and never shipped in a release artifact.
- **Names:** "Tasklane" is the project name; Jira, GitHub, GitLab, Slack and other names are used only to describe compatibility,
  with no implied affiliation; use of the Atlassian API stays within its terms (SPEC 18.1).

## 18. Integrations and extensibility

- REST API (Jira-compatible subset), GraphQL read API (optional), webhooks (signed, retried, per-event filters).
- Git integration with GitHub and GitLab: development panel, automation, smart commits (section 18.4).
- CI/CD deployments and feature flag links on issues. Chat apps (Slack, Teams) unfurl and slash commands.
- Extension points: custom field types, gadgets, workflow functions, via out-of-process apps with signed requests
  (no in-process plugins in v1).
- Import and export: Jira, third-party tools, and Tasklane's own JSON and CSV formats (section 18.2).

### 18.1 Jira mirror mode (inbound sync; optional synchronous write-through, section 18.1.1)

Tasklane can be a fast UI over an existing Jira Cloud site. Jira data is synchronized **from Jira into Tasklane's
PostgreSQL**, and every screen reads from PostgreSQL, so the latency targets of section 21 hold regardless of Jira's
response time. A mirrored project is in one of two modes, chosen per project by an administrator:

- **`read_only`** (the default): people edit in Jira and Tasklane shows an "Edit in Jira" link. Nothing edited in Tasklane
  reaches Jira.
- **`write_through`** (section 18.1.1): people edit in Tasklane, every edit is sent to Jira at once under the person's own
  identity, and the mirror changes only after Jira accepted it. There is no outbound queue and no second source of truth:
  Jira stays the system of record.

A Tasklane instance can mix mirrored projects of either mode with native projects, which are fully writable.

**Connection and identity**

- A Jira site URL and **read-only** credentials for the sync: a service account with an API token, or an OAuth 2.0 (3LO)
  app with read scopes only. Credentials are secrets, used under the egress policy (section 17.1) and never sent to the
  browser. **Jira credentials of any kind are never stored in PostgreSQL** (nor in backups, logs, the audit trail, the
  search index or any API output); they exist only in the memory of the running process (section 18.1.1, WT-01 to WT-06).
- People sign in to Tasklane (including Atlassian sign-in through OAuth) and are mapped to their Jira account by verified
  email or account id; that mapping decides what they may read (below).
- An admin chooses what to mirror: Jira projects or a JQL scope per project.

**What is mirrored** (Jira to PostgreSQL; the read model is the same one native projects use, marked with its origin)

| Data | Source (Jira Cloud REST) | Kept in Tasklane as |
|---|---|---|
| Projects, issue types, priorities, resolutions, statuses and workflows (read only) | `/project`, `/issuetype`, `/priority`, `/resolution`, `/status`, `/workflow/search` | Reference data used for display, boards and filters |
| Fields, contexts, options, custom field values | `/field`, `/field/{id}/context`, `fields.customfield_NNNNN` | Typed values rendered by type; unknown types shown as read-only text |
| Issues | `/search/jql` by `updated`, `/issue` | Issue rows with the Jira id and key as external identity |
| Description and rich text | ADF | A read-only document plus plain text for search; unsupported ADF nodes are kept as opaque blocks |
| Comments, with visibility restrictions | `/comment` | Comment rows with their restriction |
| Change history | `/issue/{id}/changelog` | The history store (section 4.5): who, when, before and after, source `Jira` |
| Attachments | `/attachment` | Metadata always; the file fetched into the blob store on first view (or eagerly, per setting), with size caps and antivirus |
| Worklogs, links, subtasks, parent | `/worklog`, issue links | Rows as in native projects |
| Boards, sprints, rank | `/rest/agile/1.0/board`, `/sprint`, `/backlog` | Read-only boards, sprints and rank |
| Versions, components | `/project/{key}/version`, `/component` | Rows as in native projects |
| Users, groups, project roles, permission schemes, issue security levels | `/user`, `/group`, `/project/{key}/role`, `/permissionscheme`, `/issuesecurity` | External identities and the permission data used for read access |

Not mirrored: Jira's own development panel (Tasklane's Git integration, section 18.4, provides it), automation rules, and
Jira-side email.

**Synchronization**

- **Backfill** on connect: paged by `updated`, resumable, throttled, one transaction per issue (issue, comments, links,
  history, attachment metadata, search and projection updates through the outbox).
- **Incremental:** Jira webhooks (registered by an admin) are only hints that trigger an immediate catch-up; the source of
  truth is polling `updated >= watermark` with an overlap window, so missed, duplicated and out-of-order webhooks cannot
  lose a change. Upserts are idempotent per Jira issue id and `updated`.
- **Deletes and moves:** an issue deleted in Jira is removed from the mirror within the sync lag (Jira has no issue trash, and
  erasure requests must propagate); a key change caused by a Jira move is detected by issue id and recorded as an alias
  (section 9.1); archived or deleted projects and closed user accounts are handled the same way.
- **Reconcile:** a periodic job compares Jira's id sets and `updated` buckets with PostgreSQL per project and repairs drift.
- **Freshness (design targets):** a change appears within 10 s p95 on the webhook path and within 60 s on the polling
  fallback. When lag exceeds a threshold the UI shows "synced N min ago" on mirrored projects.
- **Rate limits and outages:** Jira's limits (`429`, `Retry-After`) are honoured with a per-site token bucket; backfill and
  reconcile run at low priority; when Jira is slow or down, Tasklane keeps serving the mirror with a stale marker.

**Permissions** (the data is copied, so Tasklane enforces Jira's read rules itself)

- Project roles, groups and memberships, the browse grants of each project's permission scheme, issue security levels
  with their members, and comment visibility restrictions are mirrored and evaluated by Tasklane's permission engine,
  the same one used by search filtering (section 10.1).
- Grant types Tasklane can evaluate: user, group, project role, application role or any logged-in user, reporter, assignee
  and project lead. Grants it cannot evaluate exactly (for example custom-field based grants) **fail closed**: the
  affected issues are hidden, and an Admin warning lists them. An administrator can instead choose "verify with Jira" for
  those projects, which checks access per user against Jira on first access and caches the answer briefly.
- Permission data is refreshed at least every 5 minutes and on relevant webhooks; revocation in Jira therefore reaches
  Tasklane within that bound. This lag is a stated security trade-off of mirroring and is shown in Admin.
- Never widened: a person sees nothing in Tasklane that Jira would not show them (as mapped), and a failed or stale
  permission sync fails closed.

**What works on mirrored projects, and what does not (this phase)**

- Works, read-only: issue lists, filters, TQL and search (section 10), the ticket thread view with history and attachment
  previews, boards, backlog and sprint views with drag disabled, reports and dashboards computed from the mirrored data and
  changelog, Tasklane's Git development panel, the in-app notification inbox built from mirrored events, and MCP read tools.
  Tasklane-side personal state works as usual: stars, unread markers, snoozes, saved filters and dashboards.
- Not available in `read_only` mode (in `write_through` mode only the edits listed in WT-10 are available, and the
  rest of this list still applies): any edit of an issue (fields, comments, transitions, create, convert, move, clone, delete), weekly auto-sprints and rollover, Tasklane email and inbound email for these projects (Jira keeps sending its
  own), automation actions and Git rules that would change issues, MCP write tools, and import into a mirrored project.
  Each such action explains why and links to the issue in Jira. The block is enforced in the server's mutation layer, so
  the UI, REST, MCP, automation and email all refuse writes to mirrored issues with the same typed error.

**Cut-over to native.** An administrator can convert a mirrored project to a native one: sync stops, the project becomes
writable, Jira keys remain as the project's keys and aliases, history and attachments are kept. The continuous mirror is therefore also the incremental form of the Jira import (section
18.2). After cut-over Jira is no longer updated by Tasklane.

**Write-through** is specified in section 18.1.1. It is synchronous and per project; it is not an asynchronous write-back
queue. Jira's lack of an issue trash means a Tasklane delete would be permanent in Jira, so create, delete, move, clone and
convert stay out of scope (WT-10).

**Admin tab: Jira mirror.** Connection and scopes granted, mirrored projects and scopes, per-project sync state and lag,
webhook health, rate-limit usage, backfill progress, reconcile history, permission-mapping warnings (including fail-closed
issues), freshness settings, the cut-over action, and a list of what is and is not available on mirrored projects.

**Compliance.** Use of the Atlassian API stays within Atlassian's API terms and the customer's own Jira licence. Tasklane
shows mirrored data only to people Jira would show it to (as mapped), stores it in the customer's own PostgreSQL, deletes
it when Jira deletes it, and honours account closures and erasure requests reported by Jira.

### 18.1.1 Jira write-through (editing a mirrored project in Tasklane)

Status: **built (M5.76 and M5.77) against a recorded Jira double; never run against a real Jira site.** The assumptions in the last
paragraph of this section must be verified against a real Jira site before the conflict design below is relaxed. Where the
build differs from the text below, a *Built:* note says so.

**Goal.** A person edits a mirrored issue in Tasklane's fast UI; the edit is made in Jira, as that person, at once; if Jira
or Tasklane detects that someone else changed the same thing, the person is told and decides. Jira remains the system of
record: Tasklane never holds a change that Jira has not accepted.

**Requirements**

*Credentials and identity*

- **WT-01 No Jira key in the database.** A Jira API token, OAuth token or any value that authenticates to Jira is never
  written to PostgreSQL, a backup, a log line, an audit record, the search index, an error message, a metric label, a
  request log, an MCP or REST answer, or a core dump. A migration removes `jira_connection.token_enc` (the column is
  overwritten with empty values, then dropped, and the backup format no longer has it). A connection keeps only the site
  URL, the account email and a short non-reversible fingerprint. A test dumps the whole database after an unlock and searches
  it for the token (it must not appear). *Built:* the migration (0081, expand) erases the stored values and the code never
  writes anything but an empty value to the column; the column itself is dropped by a contract migration in a later release
  (a contract migration cannot ship in the same release as the code that stops using the column, and this repository's
  migration tests assume none exists yet). A restore of an older backup erases the column again.
- **WT-02 Memory only.** Keys are held in a credential holder in the memory of the Tasklane process. An entry ends on
  logout, on session expiry, when Jira rejects it (401), when the project leaves `write_through`, and when the process
  stops (so a restart or upgrade empties the holder). The service disables core dumps and keeps the holder out of swap
  where the operating system allows (best effort, stated in the health check).
- **WT-03 The mirror's service credential is unlocked, not stored.** After a start, a restart or a node taking over a
  project, a connection is `locked`: its sync does not run, mirrored data keeps being served with the stale marker, and the
  Admin tab and `GET /api/v1/jira/status` say "waiting for credentials". An administrator unlocks it with
  `POST /api/v1/jira/connections/{id}/unlock` (admin only; also `tasklane jira-unlock`, which reads the token from a terminal
  prompt or standard input, never from a command-line argument or an environment variable, and the Admin tab). Tasklane
  checks the token with Jira's `myself` call; the email it returns must equal the connection's email, otherwise the token
  is refused and not kept. Creating a connection takes the same path. Environment variables such as `JIRA_API_TOKEN` are
  not read by Tasklane and must not be needed to run it. In a cluster each node that syncs a project needs the unlock;
  a node that takes over a project stays locked until it has been unlocked. *Built:* the API call and the
  Jira tab of the administrator panel (WT-21); the `tasklane jira-unlock` command is not built.
- **WT-04 Each person writes with their own token.** The first time a person saves to a `write_through` project and this
  node holds no live token for their session, the save is held (the draft is kept, section 4.5) and a dialog asks for a Jira
  API token. The dialog shows a link to Atlassian's token page (`https://id.atlassian.com/manage-profile/security/api-tokens`;
  the person creates the token there and pastes it back) and says, in these words or their translation: "Tasklane needs your
  personal Jira API token to save changes as you. The token is kept only in memory for this session. It is never written
  to disk or to the database, and it disappears when you sign out or when Tasklane restarts." The paste field is a password
  field, is not autofilled and is not sent in a URL. A scoped token is preferred where the site allows it, and the dialog
  says so. Validation: Jira's `myself` must return the Jira account linked to this Tasklane user (by account id, or by
  verified email); a token of someone else is refused and not kept.
- **WT-05 Identity of writes.** A write is sent with the token of the person who made it, so Jira shows that person as the
  author and enforces their permissions. A missing, expired or refused token never causes a write under another identity:
  there is no fallback to the service credential for writes. How the person signed in to Tasklane (LDAP, SSO) and to Jira
  (Google, Atlassian) does not matter, because an API token belongs to the Atlassian account.
- **WT-06 Token lifetime.** A token is tied to the Tasklane session and is dropped with it. Each node holds its own;
  another node that receives the person's request without a token asks again (session affinity is allowed but not
  required). A 401 from Jira discards the token and asks again. Tokens expire in Jira (administrators may cap their lifetime
  or disable API tokens); the dialog shows Jira's message when a token cannot be created or is refused. *Built:* a token is
  kept per person and connection (not per session id, which an actor does not carry) and ends at logout, at a refusal by Jira,
  when the project leaves write-through, and eight hours after it was supplied.

*Writes and conflicts*

- **WT-07 Synchronous and honest.** A save waits for Jira. It succeeds only when Jira accepted it. Jira's refusal is shown to
  the person as the typed error `jira_rejected` (HTTP status, Jira's messages by field), and nothing changes in the mirror.
  There is no outbound queue, no retry on the person's behalf and no "saved locally, will sync".
- **WT-08 Conflict detection does not depend on Jira.** Jira's edit call is not assumed to refuse a stale write (its web
  editor does; the public API is unproven, see the verification paragraph). The editor therefore carries a **base** with
  every save: the values of the fields being changed and Jira's `updated` stamp, as the form loaded them. Before sending, Tasklane
  reads those fields from Jira (a request limited to them) and compares each with its base. If any field differs, nothing is
  sent and the save fails with the typed error `jira_conflict` that carries Jira's current value, who changed it and when.
  The conflict dialog of section 4.5 offers: keep Jira's value, overwrite with mine (a new check against what Jira has now), or
  edit by hand. Fields that did not change are not compared, so two people editing different fields do not collide. The short race between the check and the send is accepted and documented; a collision inside it is
  last-writer-wins, as in Jira itself.
- **WT-09 Jira's own collision signals are passed through.** A transition that Jira says is not available or that it
  rejects as simultaneous (HTTP 409), a validator, a required field, a permission refusal or a deleted or moved issue is
  shown to the person with Jira's text. Tasklane retries nothing on its own: a retry could apply a decision made on stale
  data. A transition is sent with the id from a fresh read of the available transitions; its required screen fields are asked for.
- **WT-10 What can be written in this phase.** Summary, description, labels, priority, assignee, comments (add) and
  transitions. Not in this phase: create, delete, move, clone, convert, estimate, custom fields, due date, editing a comment,
  internal comments, sprints and rank, attachments, worklogs, versions, components and links. Each of these keeps the
  `read_only_mirror` answer with the Jira link. Automation, Git rules, email and MCP write tools do not write to Jira in this
  phase either; they get the same typed refusal. *Built:* a mirrored workflow has no transitions of its own, so a write-through
  project offers a "Move" to every status the mirror has seen and Jira says at save time whether that move exists (WT-09).
- **WT-11 After Jira accepts.** Tasklane reads the issue back (fields and the new changelog entries) and applies it in one
  transaction exactly as the sync would, so the mirror equals Jira at that moment; history rows carry the Jira author (the
  person). When the same change comes back later through the poll or a webhook it is a no-op, because the sync is idempotent
  per Jira issue id, `updated` and changelog id; no echo suppression or loop breaker is needed, since Tasklane writes only
  on a person's action and the sync never writes to Jira.
- **WT-12 Text fidelity.** Descriptions and comments are written as Atlassian Document Format produced from the rich-text
  model (`internal/richtext`). Nodes Tasklane does not understand are kept as opaque blocks and sent back unchanged. If a
  document cannot be converted without loss, the edit of that field is refused with a link to Jira (fail closed). Property tests
  cover round trips against documents full of unsupported nodes. *Built (WT-23):* a description is edited as Markdown, see
  WT-23. A new comment is written as plain paragraphs and line breaks, checked by reading Jira's text view of it back; the
  converters are not on the comment path yet.
- **WT-23 A description is edited as Markdown, made from Jira's own document.** For a project that writes through, the stored
  text of a description is the Markdown form (the dialect of `internal/richtext`: headings, bold, italic, strike, code, links,
  lists, quotes, rules, code blocks, tables, mentions `@[Name](user:ID)`, issue links `[KEY](issue:KEY)`) of the original Jira
  document, which the sync keeps. The conversion happens at two moments only: Jira to Markdown when the sync applies an issue and
  when a project is switched to write-through (the stored descriptions are then rewritten, as plain text again when it is
  switched back), and Markdown to Jira when a person saves. Every screen still reads PostgreSQL. A block the model does not hold
  (a panel, an expand, media, a status lozenge, a colour, a paragraph with an emoji) is kept in the Markdown as an opaque fenced
  block, `adf-opaque`, and written back byte for byte. The rules that keep it lossless: the Markdown made from Jira's document must
  read back as the same model and the model must survive a trip through Jira's JSON, or the description is edited in Jira; the
  edited Markdown must convert to a document that reads back as the same model, and carry no link with a scheme other than
  http, https, mailto or a relative address, or the save is refused with a link to Jira; the dialect's own link schemes (`user:`,
  `issue:`, `attachment:`) are kept by the cleaning that every saved text goes through, which for any other link drops it to its
  label. An emptied description clears the description in Jira. The conflict rule is WT-08's, with Jira's Markdown shown. The
  read view shows the Markdown source; search indexes it as it is.
- **WT-13 Jira slow, down or rate limited.** The save fails with a typed error that states the cause and `Retry-After`; the
  draft is kept; the mirror is unchanged. The write path honours the same egress policy and per-site limits as the sync, and
  has its own timeout shorter than the page's.
- **WT-14 Audit and logging.** Every write attempt is audited in Tasklane: who, issue, field names, outcome (accepted,
  conflict, rejected, failed) and Jira's HTTP status; never a token and never the old or new text of private fields beyond what
  the issue history already holds.
- **WT-15 Permissions.** Tasklane hides edit controls it knows would fail (from the mirrored permission data) but Jira is the
  authority: an edit Tasklane allowed and Jira refused is shown as Jira's refusal. Setting a project's mode, unlocking a
  connection and viewing the status are administrator actions.
- **WT-16 Mode changes.** Moving a project from `write_through` to `read_only`, or cutting it over to native, drops the
  people's tokens held for it. Cut-over keeps its current meaning (Jira is no longer updated by Tasklane).
- **WT-17 Admin tab.** Per project: mode. Per connection: `locked` or `unlocked` since when (never the token), the number of
  people holding a live token (a count, not names), and the recent write outcomes (accepted, conflicts, rejections, failures).
  *Built:* the status API carries the mode, the locked flag and the count; the outcomes are in the audit log (`jira.write`) and
  are not summarized in the status; the Jira tab of the administrator panel (WT-21) shows the mode, the locked state and the count.
- **WT-18 One person, several tabs (should).** A save, and a draft change, made in one tab are pushed to the same person's
  other tabs, as Jira's own editor does, so a person cannot collide with themselves. This is independent of the rest and can follow later.
  *Not built.* (A saved change already reaches the person's other tabs through the issue's live events.)
- **WT-19 Where a token is created depends on the Jira site.** Every place that asks for a token links to the page where it is
  created for that site and also shows the address in text: Atlassian's token page
  (`https://id.atlassian.com/manage-profile/security/api-tokens`) for a Jira Cloud site (`atlassian.net`, `atlassian.com`,
  `jira.com`, matched on the whole host so a look-alike name does not count), and the profile page of the site itself
  (`<site>/secure/ViewProfile.jspa`) for any other, where a Jira that Atlassian does not host manages tokens. The answer
  `428 token_required` and the connection list carry it as `token_page`.
- **WT-20 A person can see what is asked of them.** `GET /api/v1/jira/session-token` (anyone signed in) lists the connections
  that have a write-through project the person can see, with the site, the projects they can see, the token page and whether
  this node holds a token for them (never the token). A project the person cannot see is not named to them.
- **WT-21 Administrator panel, Jira tab.** An administrator sees the connections (site, account, token fingerprint, `Locked` or
  `Unlocked`, how many people hold a personal token) and can unlock or replace a connection's token in a dialog that says it is kept
  in memory only and links to where it is created for the account; and sees the mirrored projects (sync age, issue count, errors,
  whether the connection is locked) and chooses per project between "read only: edit in Jira" and "save changes to Jira". Choosing
  the second asks first, in words: saved at once as the person, with their own token; Jira decides who may edit; a change in
  Jira since the person opened the issue is reported; what can be saved; agents, automation and email do not write. Cancel leaves
  the project as it was. Going back to read only needs no question and says the tokens were forgotten.
- **WT-22 Settings, Jira tab.** A person whose projects save to Jira sees, in their Settings, each connection that needs their
  token, whether one is set, and can set it before their first save, replace it, or forget it, with the link of WT-19 and the same
  statement as the save dialog (kept only in memory, never written to disk or the database, gone at sign-out or restart). A person
  with no such project has no Jira tab.

**Design**

*Mode and the mutation layer.* `jira_project` gains `mode` (`read_only` or `write_through`). The gate that today refuses every write to a mirrored
project (`admit`, answering `read_only_mirror`) becomes a mode check: in `read_only` it refuses as before; in `write_through` it
lets only the field set of WT-10 through to the Jira write path and refuses everything else with the same typed error. The
local store is never written first: the write path is *check, send, read back, apply*.

*Credential holder.* A small package (working name `credvault`) owns two maps in memory: service credentials by connection id,
and personal tokens by session id and connection id, each entry with the account id Jira returned, the time stored and the session's expiry. It exposes
get, put, drop by session, drop by connection and a count; it has no method that returns all tokens, never implements a
string conversion that prints the token, and keeps tokens as byte slices that are overwritten on removal. Request logging, tracing and panic output skip
the unlock and token endpoints' bodies.

*Endpoints.* `POST /api/v1/jira/connections/{id}/unlock` (admin; body `{token}`), `POST /api/v1/jira/session-token` and
`DELETE /api/v1/jira/session-token` (the signed-in person; body `{connection, token}`), and a `token_required` answer
(HTTP 428, typed, with the connection and the Atlassian token-page link) from any write that finds no live token. The UI turns
that answer into the dialog of WT-04 and then repeats the held save.

*Save sequence for a field edit.*

1. The editor loads the issue and keeps the base (field values and `updated`).
2. The person saves; the request carries the changed fields and the base.
3. The mutation layer finds the project in `write_through`, finds the person's token (else `token_required`).
4. Tasklane reads the changed fields from Jira with the person's token and compares them with the base (WT-08); a difference
   returns `jira_conflict` and ends.
5. Tasklane sends the edit to Jira (`PUT /rest/api/3/issue/{key}` for fields; the comment endpoints for comments;
   `POST .../transitions` for a transition). A refusal returns `jira_rejected` or the 401 path and ends.
6. Tasklane reads the issue back with the service credential (the person's token is not needed for reading), applies it in one
   transaction as the sync does, writes the audit record and returns the new issue to the page.

*Error mapping.* `token_required` (428), `jira_conflict` (409, carries Jira's current value), `jira_rejected` (422, carries Jira's
status and messages by field), `jira_unavailable` (503, carries `Retry-After`), `read_only_mirror` (403, unchanged).

*Jira client.* The Jira client, which today only sends `GET`, gains `PUT` and `POST` under the same egress policy, pacing and
`429` handling, and takes the credential as a per-call argument rather than a field of the client, so a person's token can
never leak into a client shared with the sync.

*Test double.* The recorded Jira double (`internal/testutil/jiramock`) gains the write endpoints and can be told to answer
401, 403, 409, 429, validator errors, a changed field between the check and the send, and a slow reply, so that every branch above is tested without a real site.

**Verification and open assumptions.** Nothing here has run against a real Jira site yet. To be verified in the
real-service runs (JREAL) before the conflict design is relaxed:

1. Whether `PUT /rest/api/3/issue/{key}` refuses a write based on a stale value. The documented behaviour found so far is only
   for transitions (409 when simultaneous); Jira's web editor refuses a stale save, but how it does so is unknown. The test
   needs two different Jira identities (one person's browser tabs share drafts and saves, so they cannot collide).
   If the API does refuse, the pre-check of WT-08 can be dropped for those fields.
2. That an API token works for a person whose Jira sign-in is Google or Atlassian SSO, and that the administrators of the site
   allow API tokens and a useful lifetime.
3. That scoped tokens can do the edits of WT-10 and what endpoint they need.
4. That ADF written by the converters is accepted by Jira for every node the editor can produce.

### 18.2 Import and export

Goal: moving a project into or out of Tasklane is a self-service, one-day job, with no lock-in. Two exchange formats
(JSON and a CSV bundle), a direct Jira path in both directions, and basic importers for common third-party tools.
Everything runs as a background job from the Admin tab Import / export, and from the API.

**Tasklane exchange formats** (versioned, documented, stable across releases)

- **JSON bundle**: one `.json` file for small projects, or a `.zip` containing `manifest.json` plus one JSON Lines file
  per entity for large ones (streamed, no need to hold it in memory). The manifest has format version, source system,
  export time, project keys, entity counts and a checksum per file.
- **CSV bundle**: a `.zip` with one UTF-8 CSV per entity, header row, RFC 4180 quoting, ISO 8601 timestamps:
  `issues.csv`, `comments.csv`, `worklogs.csv`, `links.csv`, `attachments.csv` (metadata plus path), `sprints.csv`,
  `sprint_issues.csv`, `versions.csv`, `components.csv`, `users.csv`, `custom_fields.csv` (definitions and options),
  `workflows.csv` (statuses and transitions), and an `attachments/` folder holding the files.
- Both formats carry the same data: projects, issue types, workflows (statuses, transitions), custom field definitions
  and values, issues with hierarchy and rank, comments (with visibility), attachments (files included on request),
  worklogs, links, watchers, sprints and membership, versions, components, labels, users (by id, name, email).
  Rich text is exported as Markdown plus a lossless JSON document form (JSON bundle only).
- Every record has a stable `id` and an `external_id` (original key or id in the source system). Re-importing the same
  bundle is **idempotent**: records are matched on `external_id` and updated, not duplicated.
- Export scopes: whole instance (admin), a project, or the result of a saved filter. Optional fields: include
  attachments, include history, include comments. Large exports are chunked and resumable; the download link
  expires and is audited.

**Jira to Tasklane (import)**

| Source | Method | Notes |
|---|---|---|
| Jira Cloud, live | The mirror machinery of section 18.1 (read-only API token or OAuth) | Preferred. Projects, issues, comments, attachments, worklogs, links, sprints, versions, components, custom fields, workflows (read), users, change history. Continuous sync until cut-over, then the project becomes native. |
| Jira CSV export | CSV upload with a column-mapping step | Handles Jira's repeated columns (labels, comments, sprints, attachments by URL). |
| Jira backup XML | Upload, best effort | Server/Data Center backups; reports what could not be mapped. |

- **Mapping step** (a wizard, saved and reusable): issue types, statuses (to existing or new workflow statuses),
  priorities, resolutions, users (match by email, else by account id, else create a placeholder inactive user),
  custom fields (map to existing or create with type inferred), components, versions.
- Description and comments are converted from ADF (and Jira wiki markup in old exports) to Tasklane's document model;
  unsupported nodes are kept as opaque blocks, so nothing is dropped. Attachments are downloaded with the source
  credentials and re-stored. Issue keys are preserved when the project key is free, otherwise a key map is produced
  and old keys are kept as aliases so links and bookmarks resolve.
- Original `created`, `updated`, `resolved`, author and worklog authors are preserved, not set to the import time.
- **Dry run** first: counts, mapping gaps, unresolvable users and fields, estimated duration. Then run; the job is
  resumable, rate-limit aware (Jira `429`, `Retry-After`), and produces a per-record error report (CSV).

**Tasklane to Jira (export)**

- **Write-through push** to a Jira Cloud site over REST is deferred together with the outbound half of the mirror (section
  18.1); until then the Jira-importable CSV below is the way to move work into Jira.
- **Jira-importable CSV**: an export shaped for Jira's own CSV importer (Summary, Issue Type, Status, Priority,
  Assignee, Reporter, Created, Resolved, Labels, Sprint, Parent, Comment, custom field columns, attachment URLs) for
  sites where API write access is not available.
- Limits Jira imposes (fields, workflow transitions, locked users) are listed in the dry-run report, not discovered
  half way through.

**Third-party tools (basic support)**

Basic means: issues, comments, labels, assignees, status, dates, attachments where the source offers them. No
automation, boards, or permissions are migrated. Each importer is a preset over the generic CSV or JSON importer plus
a small fetcher where an API exists:

| Tool | Input | Maps to |
|---|---|---|
| Generic CSV | Any CSV with a header row | Column-mapping wizard; mapping can be saved as a template |
| Generic JSON | Tasklane JSON bundle, or any JSON array via a JSONPath mapping | Same wizard |
| GitHub Issues | REST API (repo token) or exported JSON | Issues, comments, labels, milestones (to versions), assignees, PR links |
| Trello | Board JSON export | Cards (issues), lists (statuses), checklists (subtasks), labels, members, attachments |
| Linear | CSV export | Issues, state, priority, estimate, cycle (to sprint), project (to epic), labels |
| Asana | CSV export | Tasks, sections (statuses), subtasks, assignee, due date, tags |
| GitLab, Azure Boards, ClickUp | Generic CSV with a published preset column map | Basic fields only |

- Export to third parties is via the generic CSV and JSON exports; the CSV export has selectable columns and a
  "flat" mode (one row per issue, comments joined) for spreadsheets and BI tools.
- Presets are data (editable mapping files), so new tools are added without a code release.
- Streaming and automation: webhooks and the REST API (`/rest/api/3` subset) remain the integration path for ongoing
  sync; import/export is for bulk moves and backups.

**Consistency and the pause state**

Saved content lives in PostgreSQL, which is always current: a save is acknowledged only after it is committed. Only **derived**
data (history records, the search index, projections, notifications) trails by seconds, and all of it is rebuildable from the
outbox. A backup or export therefore needs no flush of editing state; a consistent PostgreSQL snapshot is exact. The **pause
state** exists for operations that must not race with writes in their scope (imports into existing data, restores, migrations,
bulk replacements) and to let derived writers reach a known point when a job wants them complete.

- **States:** `running` to `draining` to `paused` to `running`, per scope (whole instance, an org, or a project). The state, its
  owner (job id), start and deadline are visible in Admin and the API.
- **Draining:** new writes in scope wait briefly, then receive `503` with `Retry-After`; in-flight transactions finish; background
  workers in scope (automation, sprints, webhooks) stand still; and, when the job asks for it, derived writers (history, outbox
  consumers) reach the marker. A **pause marker** records the time, the database log position and the outbox position, and is
  stored in the manifest.
- **What users see:** a banner "Maintenance in progress". Open editors keep their drafts and retry their saves afterwards, with
  the indicator "Paused for maintenance, draft kept"; reads and presence continue. After the pause, saves go through the normal
  conflict check.
- **Modes** (per job):

| Mode | Behavior | Use |
|---|---|---|
| `snapshot` (default for backups and exports) | No write hold: the job reads from a consistent database snapshot taken at the marker, optionally waiting first for derived writers (so history in an export is complete) | Backups, exports |
| `pause` | Hold writes in scope for the whole job | Imports into existing data, restores, migrations, bulk replacements |
| `none` | No marker; the manifest records that derived data may trail by seconds | Scheduled exports where that is acceptable |

- **Safety:** every pause has a deadline (drain wait default 60 s; whole `pause` job default 10 minutes, configurable). On timeout the
  job aborts, the scope returns to `running`, and nothing is lost. A crashed job releases the pause; a stale pause is cleared when
  its lease expires. Only one pause per overlapping scope at a time.
- **Backups:** the database snapshot is taken at the marker, and the blob store is captured **after** it (blobs are written before
  the metadata that references them, so a later blob copy always contains everything the snapshot refers to; extra blobs are
  harmless and reported by `fsck`). A restore runs in `pause` mode, then restarts workers, reconciles derived data from the outbox
  and rebuilds the search index if needed.
- **Import:** commits in batches while the target scope is `paused`; people with an affected issue open get an "issue was updated"
  notice and their saves conflict normally. Imports into a new, empty project need no pause of other projects.
- **Mirror mode (18.1):** the pause covers mirrored data too: the sync worker for the scope finishes its batch and holds new applies
  until resume, and the marker records the Jira sync watermark per project.
- Audit events record who paused what scope, why, for how long, and whether the job completed.

**Safety and governance**

- Permission: instance-wide import/export needs the Administer permission; project-scope needs Project Admin.
  Exports respect the requesting user's view permissions, issue security levels and field restrictions, unless an
  admin explicitly runs a full-fidelity export (always audited).
- Imports are staged: validated into a holding area, previewed, then committed in one transaction per batch; a
  failed job can be rolled back by import id (records created by it are tagged and removable).
- Imported content goes through the same attachment virus scanning, size limits and allowed-type rules as uploads.
- Secrets used for a job (tokens) are scoped to the job and deleted when it ends. A Jira credential is never stored in
  PostgreSQL (section 18.1.1, WT-01): a Jira import holds its token in memory for the job only, so a job interrupted by a
  restart asks for the token again when it resumes. Secrets of other services may be stored encrypted.
- Audit events: who, what scope, counts, source, result; downloads of exports are logged with IP.
- Limits (configurable): max file size, max records per job, concurrent jobs per org; progress, ETA and cancel are
  always shown.

### 18.3 MCP server for agent-driven development

Tasklane ships a built-in **Model Context Protocol (MCP) server** so AI coding agents (Claude Code and any other MCP
client) can work from the tracker the way a developer does: pick up a ticket, read its full context, plan, report
progress, link code, move it through the workflow, and hand it to a human for review. The tracker stays the source of
truth for what is being built and who (human or agent) is doing it. The server targets a pinned revision of the MCP
specification, tested against it in CI.

**Transports and connection**

- **Streamable HTTP** (remote; the normal case for an on-prem or hosted instance) at `/mcp`, and **stdio** through
  a `tasklane mcp` CLI subcommand for local use (it proxies to the HTTP server with the user's credentials).
- Authentication: OAuth 2.1 with PKCE per the MCP authorization specification (browser consent, per-agent scopes),
  or a scoped, expiring personal or agent API token (SPEC 17.1) for headless runs. No credentials in tool arguments.
- Same permission schemes, issue security levels and field restrictions as the REST API and the UI; an agent never
  sees or does more than its identity is granted (see Safety below).
- The server can be turned off per org, per project, and per agent from Admin.

**Agent identities**

- An agent acts either as a **dedicated agent account** (a user of type `agent` with its own role and project
  membership, the recommended setup) or **on behalf of a person** (acts with a subset of that person's access).
- Every change shows the agent in history, comments, presence and notifications, for example "Agent: claude (via Jana K.)",
  with an agent badge. Agents can be assigned issues, mentioned, watched and used in filters (`assignee in agents()`).

**Tools** (names are stable; schemas are versioned and additive)

| Group | Tools | Notes |
|---|---|---|
| Find and read | `search_issues` (TQL), `get_issue`, `get_issue_context`, `list_comments`, `get_attachment`, `list_transitions`, `get_board`, `get_sprint`, `list_projects`, `list_fields` | `get_issue_context` returns the thread view: description, newest and unread comments, collapsed earlier history, subtasks, linked issues, development links, attachment list |
| Plan and structure | `create_issue`, `create_subtask`, `link_issues`, `add_to_sprint`, `set_rank`, `update_fields` | Field edits validated by the workflow, screens and field configuration like any user edit |
| Work the ticket | `claim_issue`, `release_issue`, `transition_issue`, `assign_issue`, `log_work` | `claim_issue` assigns to the agent and moves to the in-progress status atomically, with a lease (below) |
| Communicate | `add_comment`, `edit_own_comment`, `report_progress`, `request_human_input` | `report_progress` posts a structured update (what was done, next, blockers); `request_human_input` sets a "needs human" flag with a question and notifies the owner |
| Edit text | `edit_issue_text` (summary, description, rich-text custom fields) | Replace-style: `old_text` must match exactly once, else the call fails; applied as a minimal diff, so it merges with people typing in the same field (section 4.5) |
| Code and evidence | `link_branch`, `link_commit`, `link_pull_request`, `attach_text`, `attach_file`, `record_test_result` | Fills the Development panel and attachments; `record_test_result` attaches a compact result with pass/fail counts |
| Not offered | Hard delete, permission or scheme changes, SSO and token management | Agents cannot do these; destructive operations beyond archive are out of scope for MCP |

All tools carry MCP tool annotations (read-only, destructive, idempotent) so clients can prompt appropriately, accept an
optional `idempotency_key` on writes, and support `dry_run: true` for bulk and multi-issue operations (preview of
exactly what would change).

**Resources** (readable context an agent or client can attach)

- `tasklane://issue/{key}` rendered as compact Markdown, `tasklane://issue/{key}/comments`,
  `tasklane://issue/{key}/attachment/{id}` (text extracted; binary by reference), `tasklane://project/{key}/board`,
  `tasklane://filter/{id}`, `tasklane://sprint/{id}`.
- Resource subscriptions: a client can subscribe to an issue or a filter and is notified when it changes (comment,
  status, assignment, new match), driven by the same event stream as the UI (DESIGN 12.7).

**Prompts** (reusable workflows the client can offer)

`start-work` (read the issue, check linked context, propose a plan, ask `request_human_input` if blocked),
`triage` (classify, set priority and component, link duplicates), `write-acceptance-criteria`,
`ready-for-review` (summarize the change, link the PR, attach test results, transition), `sprint-planning`,
`standup-summary`, `release-notes`.

**Agent-driven development loop**

1. **Queue**: humans (or automation rules) mark work as agent-eligible with a label, a field, or by assigning it to an
   agent account. A saved filter such as "Agent ready" is the agent's inbox (`search_issues`).
2. **Claim**: `claim_issue` takes a **lease** (default 30 minutes, renewed by any write or an explicit heartbeat). While
   the lease is live other agents cannot claim the issue; an expired lease frees it and the issue shows "agent stalled".
3. **Work**: the agent reads context, posts a plan comment, creates subtasks if needed, links branch, commits and PR,
   posts progress, and records test results.
4. **Gate**: workflows can require a human for chosen transitions. Condition `actor is human` or an approval step
   (SPEC 9, 15) keeps agents from moving work to Done or Released by themselves; the agent moves it to Review and the
   human decides.
5. **Hand off**: `request_human_input` flags the issue (visible on boards and lists, notifies the reporter and owner);
   a human reply or unflag resumes the loop. Agents never sit on a blocked issue silently.
6. **Observe**: the Admin tab shows what each agent did, with a one-click revoke or pause.

Automation hooks: the trigger "issue assigned to an agent" and "comment mentions an agent" fire webhooks and the event
stream, so an external runner can start an agent session; Tasklane does not run models itself.

**Safety and governance**

- **Scopes**, granted per agent and per project: `read`, `comment`, `edit-fields`, `create`, `transition`, `claim`,
  `attach`. A scope is the ceiling; the permission scheme is also enforced.
- **Untrusted content**: issue text, comments, attachments and external reporters are attacker-controllable. Tool
  results mark each text block with its provenance (author, agent/human, internal/external, time) and are returned as
  data, never as instructions; the tool descriptions and a built-in system prompt snippet tell clients to treat them
  that way. Sensitive fields, secrets and tokens are never returned; attachments are text-extracted with size caps.
- **Limits**: per-agent rate limits and write budgets (for example writes per hour), per-call result size caps with
  pagination cursors, maximum bulk size, loop protection (the same agent cannot trigger itself through automation).
- **Audit**: every tool call records agent identity, on-behalf-of user, client name and version, tool, arguments
  (redacted), result, and session id; filterable and exportable with the rest of the audit log (SPEC 17.3). Agent
  actions are reversible where possible (history restore, `undo` of the last agent session on an issue by an admin).
- **Kill switch**: pause or revoke one agent, one token, or all MCP access instantly; in-flight leases are released.
- **Privacy**: an org can restrict which projects and security levels agents may see, and exclude fields from
  agent views.

**Token efficiency (design for model context)**

Responses are compact by default: Markdown for text, only requested fields (`fields` and `expand` parameters), the same
"N earlier comments" collapsing as the ticket view (section 4.2), stable short identifiers, pagination with cursors,
and server-side summaries of long threads on request. No large binary or base64 payloads inline.

**Interaction with other features**

- **Editing (4.5)**: agent text edits use the anchored patch (or `If-Match` on `text_seq`), appear in history as the agent, and
  fail safely if the text changed; a person with the field open sees a banner and their own save goes through the conflict dialog.
- **Mirror mode (18.1)**: read tools work on mirrored issues; write tools return the read-only mirror error with the Jira
  link; any tool a project cannot support returns a clear "not supported here" error.
- **Latency (21)**: read tools meet the same server-time budgets as the corresponding UI endpoints; the first
  `get_issue_context` for a warm issue should return in under 100 ms server time.
- **Import/export (18.2)**: agent identities and their audit records are included in full-fidelity exports.

**Admin tab: Agents and MCP**

Agent accounts, tokens and OAuth clients, scopes and project access, last activity and usage against limits, pause and
revoke, lease overview (who holds what, stalled agents), tool-call audit view, per-org and per-project switches,
and the connection snippet for common MCP clients.

### 18.4 Git integration (GitHub and GitLab)

Tasklane links issues to the code that implements them, the way Jira's development panel does: branches, commits,
pull requests (merge requests on GitLab), build status and deployments appear on the ticket, on board cards and in
search, and can drive workflow transitions. GitHub and GitLab are the first providers; each sits behind one provider
interface and a normalized model, so Bitbucket, Gitea and Azure Repos can be added without changing the UI or the rules.

**Providers and connection**

| Provider | Hosting | Connection |
|---|---|---|
| GitHub | github.com and GitHub Enterprise Server | GitHub App (preferred; fine-grained permissions, per-repo installation) or a fine-grained token; OAuth for user-linked actions |
| GitLab | gitlab.com and self-managed | Group or project access token, or an OAuth application; webhooks at group or project level |

- Self-hosted instances take a base URL and an optional CA bundle; outbound calls go through the egress policy of
  section 17.1 (private addresses only when the connection explicitly allows them).
- Default permissions are **read-only** (repository metadata, branches, commits, pull/merge requests, checks and
  pipelines, deployments, webhooks). Write capabilities (create branch, post a comment on a PR) are opt-in per connection.
- Credentials are stored encrypted, scoped to the connection, and shown to admins only as a fingerprint.
- An admin maps repositories to projects (many repositories per project, and one repository to several projects).

**Ingest**

- **Webhooks** are the primary feed: signatures are verified (GitHub HMAC SHA-256 header; GitLab secret token), the
  payload is stored in an inbox table keyed by delivery id (or a content hash when none exists) and acknowledged
  immediately, then processed asynchronously. Duplicates and replays are ignored; out-of-order events are resolved by
  provider timestamps and head commit, so the stored state converges to the provider's.
- **Backfill** on connect (default last 90 days, configurable) and a periodic **reconcile** job repair missed
  deliveries. Both are throttled to the provider's rate limits (GitHub primary and secondary limits, GitLab limits),
  use conditional requests, and run at low priority so they never affect interactive latency.
- Large events (force-push of many commits, monorepos) are capped and summarized, not fully expanded.
- No source code or diffs are stored: only titles, subject lines (message body truncated), author, state, SHA, URLs,
  file-count summaries, check and pipeline results.

**Linking issues to code**

- A link is created automatically when an issue key of a project in this instance appears in a branch name, commit
  message, or pull request title or description (case-insensitive; several keys allowed). Only configured project
  keys count, so strings such as `UTF-8` never link. Old keys resolve through key aliases after an issue is moved.
- Users can add or remove a link by hand (paste a URL). Editing a PR title adds or removes its automatic links; manual
  links stay.
- Authors are matched to Tasklane users by verified email or a linked provider identity; unmatched authors are shown
  by their provider name.

**What the ticket shows (Development section)**

- Collapsed by default like subtasks: a one-line summary, for example "1 PR open, 3 commits, build passing, deployed to
  staging", expanding in place (DESIGN 5.8).
- Pull requests: state (draft, open, approved, changes requested, merged, closed), title, author, reviewers, checks,
  updated time. Branches (with "create branch" if enabled), commits (short SHA and subject), builds per PR and per
  branch, and deployments by environment.
- Board cards and list rows show an optional PR-state and build-state icon; filters can use them.
- TQL fields (subset): `pullRequest.state`, `pullRequest.count`, `branch ~ "..."`, `build.state`, `deployment.environment`,
  with the usual permission filtering (section 10).
- The ticket bootstrap contains the summary and the first rows from Tasklane's own store; the provider is never called
  on the read path (section 21).

**Workflow automation from code events** (per project, off by default, with dry run and audit)

- Rules map events to transitions, for example branch created to In Progress, PR opened to In Review, PR merged to
  Done or Ready for QA, build failed to flag the issue and comment. Rules respect workflow conditions and validators:
  a transition the workflow does not allow is skipped and reported, never forced.
- New workflow conditions and validators: "has a linked pull request", "all linked pull requests merged", "build is
  passing". Agents moving work are bound by the same gates as people (section 18.3).
- Actions are attributed ("moved by GitHub, PR #12"). Loop protection prevents a transition from re-triggering the
  rule that caused it.

**Smart commits** (disabled by default, enabled per project)

- Commands in a commit message or merged PR title, for example `ABC-123 #comment fixed the redirect`, `ABC-123 #time 2h`,
  `ABC-123 #in-review` (transition by name), run as the **mapped committer** and only with that user's permissions.
- Unmapped authors, commands in a PR body by someone other than the committer, and unknown commands are ignored and
  reported in the issue history. Applied only on the default branch or on merge, per setting. Every run is audited.

**Write actions** (opt-in)

- Create a branch from an issue (name template `{key}-{slug}`, base branch selectable, conflict handling), copy the
  branch name and `git` command, post a back-link comment on the PR or MR. The MCP tools in section 18.3 reuse these
  actions.

**Releases and deployments**

- GitHub deployments and GitLab environments and deployments appear on the issue by environment (staging,
  production); merged PRs and commits can be attached to a version for release notes; "deployed to production" can be a
  workflow condition or an automation trigger.

**Security and governance**

- Webhook secrets, replay protection, optional source-IP allowlists; payload size limits; strict SSRF handling for
  self-hosted URLs (section 17.1).
- **Untrusted content:** PR and commit text is attacker-controllable. It is sanitized for display and tagged with its
  provenance in MCP results (section 18.3).
- **Visibility:** by default anyone who can see the issue sees its development data. An org setting limits private-repo
  details to users with a linked provider identity and access to that repository.
- Admin disconnect or app uninstall removes webhooks, stops ingest, and offers to keep the links read-only or purge
  them. A revoked or expired token raises a health warning instead of failing silently.
- Provider identities and authors fall under retention and GDPR rules (section 17.3).

**Interaction with other features**

- **Mirror mode (18.1):** Git data attaches to mirrored issues by Jira issue id, key and aliases; transition rules and smart
  commits never change mirrored issues, and Tasklane does not write to Jira's development panel.
- **Import/export (18.2):** bundles carry links and cached metadata but never tokens or webhook secrets; after a Jira
  import, repositories are connected and keys are re-scanned to rebuild links.
- **Latency (21):** a webhook event is visible on the ticket within 2 s p95 of receipt, provider delivery latency
  excluded.

**Admin tab (Integrations, Git):** connections and health, repository to project mapping, webhook delivery log and
health, backfill progress, rate-limit usage, automation rules, smart-commit and write-action switches, visibility setting.

## 19. Mobile and offline

- Responsive web (PWA) with installability; Gmail-style swipe actions and compact lists on phones.
- Offline read of recently viewed issues, queued comments and transitions that sync on reconnect with conflict notices.
- Push notifications. Camera/gallery attachment capture. Biometric lock option.

## 20. Accessibility and i18n

WCAG 2.2 AA, full keyboard navigation (shortcuts and the command palette in section 4.6; `.` opens the actions of the
current issue), screen reader labels, reduced motion, high contrast and font size as personal settings (section 3). Localized UI strings, RTL support,
per-user locale and timezone, date and number formats.

## 21. Non-functional targets

- **On-prem load latency (native mode):** on a local network or localhost, with the app shell cached, opening a screen
  (issue list, ticket, board) reaches usable content in under 300 ms at p95, measured from navigation start to the
  first meaningful paint of real data. This is the same feel as Gmail on a good connection. Actions that change data
  (transition, assign, comment) show their result immediately (optimistic update) and confirm within the same budget.
  Not covered: first-ever visit with an empty cache (target under 1 s, initial JS under 200 KB gzipped) and backend
  mirrored Jira projects, which meet the same targets because every read comes from PostgreSQL; their freshness is bounded by the sync lag (section 18.1).
- Server time p95: ticket view under 50 ms, issue list page under 50 ms, board view under 100 ms for 500 issues
  (the remainder of the 300 ms end-to-end budget is network, decode and render).
- Search p95 under 400 ms on 5 million issues with the OpenSearch engine, including the permission pre-filter and the final
  authorization check; the PostgreSQL engine meets the same target up to 100,000 issues (section 10.1). Typeahead p95
  under 150 ms. Search results may lag changes by about 5 s p95.
- **Editing reliability (section 4.5).** No system is error-free, so the promise is stated as measured rates. The unit is a **save** (one
  explicit save of a text field, one scalar change, one comment). Targets, measured per rolling 30 days and gated per release:

| Error class | Definition | Target |
|---|---|---|
| E1 Lost acknowledged save | A save the server acknowledged is later absent without a user delete, undo or restore | at most 1 per 100,000,000 saves |
| E2 Undetected overwrite | A save replaced someone else's newer change without a conflict being raised (other than an explicit "keep mine" or a documented bare API overwrite) | at most 1 per 1,000,000,000 saves |
| E3 Lost draft | Text typed in an open editor cannot be recovered after a tab close, crash or reload | at most 1 per 1,000,000 editing sessions; the loss window is at most the local autosave interval (2 s of typing) |
| E4 Silent content change | Stored content differs from what the user saved, other than the documented sanitization (removed content is reported to the saver) | at most 1 per 100,000,000 saves |
| Availability of editing | The ticket loads and a save is accepted | follows the instance availability: 99.9% single node, 99.95% scaled HA; presence is best effort and not part of it |

  For scale: at design capacity (50 concurrent users) that is roughly 5,000 saves and 1,000 editing sessions a day, so E3 at 1 per
  million sessions is about one incident in three years, and E1 to E4 at the other rates are far rarer. Any E1, E2 or E4 occurrence
  is a severity 1 incident with a post-incident review regardless of the rate. Rates are verified by a test campaign (rule of three:
  about 3x10^8 simulated saves for the 10^8 targets, and 3x10^6 simulated editing sessions with injected crashes for E3) and by
  production counters (draft recoveries, version-check outcomes, save acknowledgements reconciled against stored versions).
- Correctness: rank and transition operations are transactional; no lost updates for scalar and structural changes
  (optimistic concurrency on `version`); text saves are conditional on `text_seq` and API text writes use an anchored patch, a
  conditional replace or an explicit last-write-wins (section 4.5).
- Consistency: PostgreSQL is authoritative and current for saved content at all times; derived data (history records, search
  index, projections, notifications) trails by seconds (search about 5 s p95). Backups and exports read a consistent snapshot;
  imports and restores run behind the pause state (section 18.2).
- Security: OWASP ASVS L2, dependency scanning, signed releases, SBOM.

## 22. Milestones

1. **M1 Core**, built and shipped as three ordered sub-steps. M1 is done when all three pass their exit gates (TESTPLAN section
   7); the sub-steps share the milestone number and nothing after them is renumbered.
   - **M1a Tracker:** projects, issues, issue types, workflows (the standard templates; the visual editor is M3), comments,
     attachments on the blob store with the local filesystem backend (section 17.6), list and ticket views, dark and light
     theme, TQL basics with PostgreSQL search (section 10.1), advisory editing presence with optimistic save, autosaved drafts and a
     conflict dialog (section 4.5), outbound email over SMTP for account mail and notifications (section 13.1), installation
     (container, Compose, tarball), the configuration reference, `tasklane doctor` and the migration framework (sections 17.7
     and 17.8).
   - **M1b Lifecycle and exchange:** issue lifecycle operations (convert, move with key aliases, clone, trash, restore, purge;
     section 9.1), JSON and CSV bundle export and import (section 18.2), generic CSV import with column mapping, and the pause
     state those jobs use (section 18.2).
   - **M1c Agents:** the MCP server (section 18.3), in two steps: first the read tools and comment, then create, claim, transition
     and edit-text, which ship only together with basic scopes (read, comment, edit-fields, create, transition, claim) and an audit
     record of every tool call. OAuth, budgets, the kill switch and human gates remain in M4.
2. **M2 Agile**: boards, backlog, weekly sprints with auto-rollover, reports; S3 blob backend with direct upload,
   video and log previewing; read-only Git integration for GitHub and GitLab (section 18.4: key linking, webhooks,
   backfill, pull request and build status in the development panel); the OpenSearch search backend with permission-safe indexing,
   lag handling, online reindex and reconcile (section 10.1); Jira mirror, one-way inbound and read-only (section 18.1).
3. **M3 Fields**: custom fields with contexts, screens, schemes, behaviors; visual workflow editor with draft, publish
   and in-flight migration; issue archive.
4. **M4 Enterprise**: SSO/SCIM, permissions, audit, notifications, automation, retention, MCP governance (OAuth,
   agent accounts, budgets, kill switch, human gates; scopes and the call audit log already exist from M1c), blob encryption at rest, retention, tiering and backend migration; Git automation rules, smart commits, write
   actions and repository-access visibility; inbound email (reply-by-email and create-issue-by-email) with spoofing and
   loop protection and bounce handling; Helm chart, rolling-upgrade and version-skew guarantees, release signing and SBOM.
5. **M5 Ecosystem**: dashboards, releases, approvals, portal, integrations, Jira import (live REST, CSV, XML), Jira-importable CSV export and third-party importers
   (GitHub, Trello, Linear, Asana), MCP prompts, resource subscriptions, and deployment and release tracking from Git providers; email provider APIs, the built-in SMTP listener, DKIM signing and
   service desk senders; hosted-edition operations and billing (section 17.9); customer (portal) identity and service desk request types (section 15.1).

## 23. Open questions

- License: Apache-2.0 is the proposed decision (section 17.9); confirm before the first public release. The dependency policy does not depend on it.
- Rich text format: resolved in DESIGN.md Part II. ProseMirror JSON is the canonical model, with Markdown and ADF
  converters.
- Real-time collaborative editing: dropped in favor of advisory presence and optimistic save (section 4.5). Revisit only if the
  conflict dialog proves painful in practice.
- Frontend framework: **recommendation: Svelte 5 with TypeScript**, confirmed by a prototype of list + editor within the JS
  budget (rationale and alternatives, including React, in DESIGN.md section 13.1). Not yet decided.
- Search engine: resolved in section 10.1. PostgreSQL by default, OpenSearch (an extra service and a second, lagging copy)
  for large instances; the PostgreSQL limit is set at 100,000 issues per instance and verified by PERF-16.
- Working fast (section 4.6): resolved. Undo of a transition always returns to the previous status, bypassing the
  workflow; desktop lists use a sticky pager; the first bulk bar acts on up to one page, "select all matching" runs as
  a background job (built, M5.74); reactions use a fixed set of eight emoji.
- Jira write-through (section 18.1.1): specified as synchronous and per user, with no key in PostgreSQL. Open: whether Jira's
  public edit API refuses stale writes (WT-08 stays until a real-site test shows it does), whether API tokens work for
  Google-SSO accounts and are allowed by the site, and whether scoped tokens are enough. Until it is built Tasklane never writes to Jira.
