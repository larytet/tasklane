# Tasklane - Design Specification

Version 0.1 (draft). Companion to [SPEC.md](SPEC.md) (functionality). Part I (sections 0-9) fixes the visual and
interaction design: tokens, type, spacing, components and screen layouts. Dark mode is the reference; light is derived.
Part II (sections 10-17) fixes the application design, backend and frontend, centered on safe concurrent editing and the
300 ms on-prem latency target.

## 0. Design approach

Tasklane ships its own palette, typography and icons; no third-party assets, fonts or trademarks are used. The visual language is
a compact, calm tracker UI built on a small set of principles:

1. **Density for work lists, calm for reading.** Issue rows follow an 8 px rhythm with a 4 px half step so many rows fit on
   screen; the ticket reading view uses large contrast between the primary line and its meta text, few borders, and elevation
   expressed by surface step rather than shadow.
2. **Near-black blue-grey, never pure black,** for the dark theme (the reference theme); light is derived from the same tokens.
3. **Borders are translucent overlays** in dark, so they work on every elevation.
4. **Accent is a light blue in dark and a deep blue in light;** the light accent is never reused as a dark text fill.
5. **Own typography:** Inter (UI) and JetBrains Mono (keys, code), both OFL-licensed and self-hosted.

## 1. Tokens

All colors are semantic tokens; components never use raw values. Dark is authored first. Contrast ratios below were
computed (WCAG 2.x relative luminance), not estimated.

### 1.1 Surfaces and lines

| Token | Dark | Light | Use |
|---|---|---|---|
| `surface-sunken` | `#111316` | `#F4F6F8` | App background behind lists, board canvas |
| `surface` | `#171A1E` | `#FFFFFF` | Pages, list rows, ticket body |
| `surface-raised` | `#1E2227` | `#FFFFFF` + shadow-1 | Cards, composer, selected row |
| `surface-overlay` | `#262B31` | `#FFFFFF` + shadow-2 | Menus, popovers, dialogs, previewer chrome |
| `border` | `rgba(226,230,240,.12)` | `rgba(11,18,30,.14)` | Dividers, card edges |
| `border-input` | `#707886` | `#7C8490` | Inputs, checkboxes (3:1 or better) |
| `border-focus` | `accent` | `accent` | 2 px ring, 2 px offset, always visible |
| `scrim` | `rgba(0,0,0,.6)` | `rgba(10,15,25,.4)` | Behind modals |
| `hover` / `pressed` | white 6% / 10% overlay | black 5% / 9% overlay | State layers on any surface |

Elevation in dark is the surface step plus the `border`; shadows are used only in light.

### 1.2 Text and icons

| Token | Dark | Light | On `surface` (dark / light) |
|---|---|---|---|
| `text` | `#D5D9DF` | `#1C2127` | 12.3 / 16.2 |
| `text-subtle` | `#A3AAB4` | `#4B5461` | 7.5 / 7.7 |
| `text-subtlest` | `#8D95A1` | `#656E7B` | 5.8 / 5.2 (4.7 on overlay) |
| `text-disabled` | 38% of `text` | 38% of `text` | exempt |
| `link` / `accent` | `#7AAEF7` | `#1A5FD0` | 7.7 / 5.8 |
| `text-on-accent` | `#0B1B33` | `#FFFFFF` | 7.6 / 5.8 |

Icons use `text-subtle` by default, `text` when active. Never rely on color alone: status and priority always
carry an icon or label.

### 1.3 Semantic colors

| Role | Dark fg | Light fg | Dark subtle fill | Light subtle fill |
|---|---|---|---|---|
| `success` | `#7FD08F` | `#2F6B3C` | `#1F3A26` | `#DDF2E2` |
| `warning` | `#F0B84A` | `#8A5200` | `#45300A` | `#FDEBC8` |
| `danger` | `#F4857D` | `#B3261E` | `#4A1D1A` | `#FADAD7` |
| `info` | `#6CC3E0` | `#0B6A8A` | `#143642` | `#D4EEF6` |
| `accent` | `#7AAEF7` | `#1A5FD0` | `#14305A` | `#D6E4FB` |

Foregrounds meet 4.5:1 on `surface` in both themes (dark 7.1 to 9.7, light 5.8 to 6.5). Subtle fills are the
background of lozenges, banners and highlights; text on them uses the matching fg token.

### 1.4 Status categories (lozenges)

| Category | Dark (fill / text) | Light (fill / text) |
|---|---|---|
| To Do | `#2E343B` / `text-subtle` | `#E4E8ED` / `#3A4350` |
| In Progress | accent subtle / accent | accent subtle / accent |
| Done | success subtle / success | success subtle / success |
| Blocked / flagged | danger subtle / danger | danger subtle / danger |

Priority icons: Highest (double up chevron, danger), High (up, warning), Medium (equals, warning-subtle), Low (down,
info), Lowest (double down, info). Issue type icons use a 16 px rounded tile: Epic purple, Story green, Task blue, Bug
red, Subtask grey.

### 1.5 Presence colors

Eight presence colors, each validated as a 3:1 avatar ring against `surface-raised` in both themes and paired with a 4.5:1
label text color. Assigned per user per session; the name is always shown, never color alone.

Dark: `#F27AA4 #F4A261 #E9C46A #7BD389 #5FD0C5 #7AAEF7 #B39DF5 #E58FE0`.
Light: `#C2185B #B45309 #8A6D00 #1F7A3A #0F766E #1A5FD0 #6D4AD0 #A21CAF`.

## 2. Typography

- UI: **Inter** (variable). Mono: **JetBrains Mono** for keys, code, JQL/TQL, IDs. Fallback to the system UI font.
- Base 14 px, 20 px line height. Numbers use tabular figures in lists, boards and reports.

| Style | Size / line | Weight | Use |
|---|---|---|---|
| `display` | 28 / 36 | 600 | Empty states, dashboard hero numbers |
| `title` | 22 / 28 | 600 | Ticket subject, page titles |
| `heading` | 16 / 24 | 600 | Section headers, sprint names |
| `body-lg` | 16 / 24 | 400 | Issue row summary, comment body on phone |
| `body` | 14 / 20 | 400 | Default text, descriptions, comments |
| `label` | 12 / 16 | 500 | Lozenges, chips, column headers, meta |
| `mono` | 13 / 20 | 450 | Keys (`ABC-123`), code |
| `micro` | 11 / 14 | 500 | Badges, counts. Never body text |

Unread rows use weight 600 on the summary (Gmail unread model). Text scales with the user's OS setting up to 200%
without clipping; rows grow, never truncate controls.

## 3. Space, shape, motion

- **Grid:** 4 px base. Scale `4 8 12 16 24 32 40 56`. Page gutter 16 on phone, 24 on tablet and desktop.
- **Radius:** `r-sm` 4 (lozenges, inputs, small chips), `r-md` 8 (cards, buttons, composer), `r-lg` 16 (sheets, dialogs
  on phone), `r-full` (avatars, filter chips).
- **Hit targets:** 44 px minimum on touch; 32 px minimum on pointer-only desktop density. Row actions never smaller.
- **Density:** `comfortable` (row 56 px) and `compact` (row 44 px). Compact is the default at phone width, comfortable
  at tablet and desktop.
- **Motion:** 120 ms for state changes, 200 ms for panels and sheets, standard ease-out. Remote edit highlights fade
  over 1 s. `prefers-reduced-motion` replaces slides with fades and disables caret animation.
- **Icons:** one outline set, 20 px (16 px in rows and chips), 1.5 px stroke, original artwork.

## 4. Layout and responsive behavior

| Width | Layout | Navigation |
|---|---|---|
| < 600 | Single pane, list then thread as a push | Bottom bar, 5 items: Home, Issues, Boards, Search, More |
| 600 to 1023 | Two panes: list 360 + thread | Collapsible rail 72 |
| >= 1024 | Three panes: rail 72 or 240, list 380 to 440, thread flexible (max reading width 760) | Rail with labels, command palette `Ctrl K` |
| >= 1440 | Optional right inspector 320 for Details | Same |

The thread body never exceeds 760 px so lines stay readable (about 90 characters). Boards and backlog use the full
width with horizontal scroll.

## 5. Components

### 5.1 Issue row (the inbox row)

Compact is 44 px, comfortable 56 px. Left to right: 16 px type tile, key (mono, `text-subtle`), summary
(`body-lg` on phone, `body` on desktop, one line, ellipsis), then trailing: priority icon, assignee avatar 20 px
(24 comfortable), status lozenge, updated-ago (`label`, `text-subtlest`). Second line (phone only, comfortable): key,
labels, paperclip and comment counts. Unread: summary weight 600 and a 6 px accent dot at the left edge.

States: hover shows row actions in place of updated-ago; selected uses `surface-raised` and a 2 px accent bar on the
left; focus ring is inset.

### 5.2 Ticket thread (reading view)

Order: header bar, subject, chip line, Details expander, description message, attachment strip, activity and
comments, subtasks and links, sticky reply bar. Behavior is defined in SPEC section 4; the visual rules are:

- Subject: `title`, editable in place, no visible border until focus.
- Message: 32 px avatar, name `body` 600, relative time `label` `text-subtlest`, body `body`; 16 px between messages,
  1 px `border` between them, no card chrome. The newest and unread messages are expanded.
- Activity items: one line, `label`, `text-subtle`, 20 px indent aligned with message bodies.
- Reply bar: `surface-raised`, `r-md`, 48 px collapsed; expands to a composer with a toolbar of 32 px buttons.

### 5.3 Attachment strip and previewer

Horizontal chips, 160 x 56 (thumbnail 56 square on the left, filename `label`, size `label` subtle), 8 px gap,
`r-md`, `border`. Image attachments show a larger 96 px tile variant when the strip has three or fewer items.
Previewer: full-screen `surface-overlay` at 96% black, top bar with name, index, download and close; swipe or arrow
keys between files; focus is trapped; Esc closes.

### 5.4 Board

Column: 280 px wide (240 compact), `surface-sunken` header strip with name, count and WIP badge, cards stack with 8 px
gap. Card: `surface-raised`, `r-md`, 12 px padding; contents top to bottom are summary (`body`, 3 lines max), labels,
then a footer row with type tile, key, priority, estimate, avatar. Epic color is a 3 px left edge. A column over its
WIP limit shows a `danger` header fill and the count in bold. Drag lifts the card (`shadow-2` in light, `border-focus`
in dark) and shows a drop indicator line; invalid targets dim with a tooltip explaining the blocking condition.

### 5.5 Backlog and sprint panels

Sprint panel header: name `heading`, dates `label`, goal one line, totals as three lozenges (To Do, In Progress,
Done points), primary action right-aligned (Start, Complete). Rows use 5.1 at compact density with a drag handle on
hover. Capacity bar under the header: filled `accent`, overcommit segment `warning`.

### 5.6 Presence and editing chrome

- **Presence stack** in the ticket header: 24 px avatars overlapping 8 px; people with an open editor get a 2 px ring in their
  presence color. Names appear on hover and focus and in the field notes below; color is never the only signal.
- **Field note:** while someone has an editor open on a text field, a small chip at the top-right of that field reads "Jana K. is
  editing (2 min)" (`info` fill, avatar, text). Opening an editor on such a field shows one gentle, dismissible warning inside the editor
  ("Jana K. is also editing this field. You can continue; if you both save you may need to merge."). Nothing is disabled.
- **Draft states** at the bottom-left of the editor: `Draft saved`, `Saving`, `Saved`, `Offline, draft kept`,
  `Paused for maintenance, draft kept`, `Mirrored, edit in Jira` (read-only); each has an icon and text. After a reload or crash a
  banner at the top of the field says "Restored your unsaved draft from 2 min ago" with Discard.
- **Changed-under-you banner:** if the field is saved by someone else (or by Automation, an Agent or the API) while your editor is
  open, a banner appears: "The description was changed by Automation rule X. See changes, or keep editing"; nothing changes in your
  text until you save.
- **Conflict dialog** (modal; `surface-overlay`, `r-lg`; full screen on phones): the title names who saved and when. Desktop shows three
  columns, Their changes, Merged result (editable) and Your changes; phones show them as tabs. Changes are highlighted (added
  `success` subtle, removed `danger` subtle); conflicting blocks carry a `warning` marker and a per-block chooser (theirs, mine, edit).
  The bottom bar has Save merged result (primary), Keep mine, Keep theirs and Copy mine and reload. It is fully keyboard operable, focus is trapped,
  and a live-region summary reads "2 changes by Jana, 1 block changed by both".

### 5.7 Other components

- **Buttons:** primary (accent fill), secondary (`surface-raised` + `border`), subtle (text only), danger. Height 36
  desktop, 44 touch, `r-md`, 16 px horizontal padding.
- **Inputs:** 36 / 44 px, `border-input`, `r-md`, label above, help text below in `label`. Error: `danger` border,
  icon and message.
- **Filter chips:** `r-full`, 28 px, selected = accent subtle fill and accent text.
- **Menus and sheets:** menus `surface-overlay`, `r-md`, 8 px padding, 36 px items. On phone the same actions open as
  a bottom sheet with `r-lg` top corners and a drag handle.
- **Toasts:** bottom, `surface-overlay`, 4 s, with Undo for reversible actions (transition, archive, delete).
- **Empty states:** icon 48 px, `heading`, one sentence, one primary action.
- **Skeletons:** rows and cards shimmer in `surface-raised`; never spinners for lists.
- **Mirrored (read-only) issues:** edit controls render disabled with a lock icon and an "Edit in Jira" link; the title bar shows a
  "Mirrored from Jira, synced N min ago" chip (stale state uses `warning`); drag handles and inline-edit affordances are absent,
  not just inactive, so the page reads as a document, not a broken form.

### 5.8 Development panel (Git)

A section in the ticket thread, collapsed by default like subtasks, so the reading flow stays compact.

- **Summary row (collapsed):** type icon `branch`, text "1 PR open, 3 commits, build passing", plus status chips using
  the semantic colors: open PR `info`, approved `success`, changes requested `warning`, merged `accent`, closed or
  failed `danger`, none `text-subtle`. Chips always carry an icon and text, not color alone.
- **Expanded:** one 44 px row per PR (state lozenge, title, author avatar, reviewer avatars, check icon with count,
  updated-ago), then branches as mono chips with copy, then commits (`label` mono short SHA, subject, author), then
  deployments by environment with a status dot. Long lists show five rows and "show more".
- **Board card and list row:** one 16 px PR icon (state color) and one build dot, optional per density; tooltip carries the
  text, and the same states are readable in the card's accessible name.
- **Empty state:** "No code linked yet" with "Create branch" (if enabled) and "Link a pull request" actions.
- **States:** loading uses a skeleton row; stale provider data (health warning) shows a subtle "last updated" time and a
  link to the integration status for admins.

## 6. Screens

Each screen is specified as regions and states; wireframes live in the README.

1. **Issues (inbox).** Search bar, preset chips, list, bulk bar on selection. Swipe left: transition; swipe right:
   assign to me; long press: select. Empty state per preset.
2. **Ticket.** Section 5.2. On desktop opens in the third pane; on phone pushes. Prev/next in header follows the list.
3. **Board.** Board header (name, quick filters, sprint selector, complete sprint), columns, swimlane headers sticky.
4. **Backlog.** Sprint panels above the ranked backlog; epics and versions side panel toggled from the toolbar.
5. **Sprint report.** Chart region 2:1, summary tiles above, issue tables below.
6. **Admin.** Left sub-navigation of the 15 management tabs (SPEC section 5), content page with a sticky save bar.
   Admin pages use the same tokens at comfortable density and never appear inside the reading flow.
7. **Workflow editor.** Canvas on `surface-sunken`, status nodes as lozenges, transitions as 1.5 px lines with labels,
   inspector on the right; draft/publish bar on top.

## 7. Accessibility checks (design level)

- Text contrast 4.5:1 and UI contrast 3:1 in both themes (tables in section 1).
- Focus ring visible on every interactive element, never removed.
- Status, priority and presence always combine color with an icon, label or name.
- Touch targets 44 px; pointer targets 32 px.
- Reduced motion, 200% text scale and high-contrast mode (borders raised to `text-subtle`) are part of the acceptance
  checklist for each component.
- Light theme is derived by the contrast checker: a token pair that fails blocks the build.

## 8. Implementation notes

- Tokens live in one JSON file, compiled to CSS custom properties (`--tl-surface`, ...). Theme is set on `<html
  data-theme>` before first paint from a server-rendered attribute, so there is no flash.
- Fonts are self-hosted WOFF2 with `font-display: swap` and size-adjusted fallbacks.
- A visual regression suite renders each component in dark, light, compact and comfortable.
- Performance budget (SPEC section 21: usable content under 300 ms on-prem, p95): service-worker cached app shell,
  initial JS under 200 KB gzipped, list and ticket data fetched in parallel with shell render, skeleton rows within
  one frame, virtualized lists, optimistic updates for all quick actions, prefetch of the next ticket in the list.

## 9. Open questions

- Icon set: commission original artwork or adopt an open set (Lucide-style, ISC).
- Whether comfortable density should be the default on desktop or follow the user's last choice.
- Board card layout: single line (denser) versus three-line summary (more readable).

---

# Part II - Application design (backend and frontend)

This part fixes how the system is built so that the two hardest requirements hold: **safe concurrent editing of issues**
(advisory presence and optimistic save, SPEC section 4.5) and **usable content in under 300 ms on-prem at p95** (SPEC section 21). Every choice below is
justified against one of those two, or against the Jira mirror (SPEC section 18.1). Numbers are design
targets, to be validated by the benchmarks in section 16.

## 10. Architecture overview

A modular monolith: a single Go binary, stateless app nodes, PostgreSQL as the system of record. Modules talk through Go
interfaces, so they can be split later without a rewrite.

```
 Browser (SPA + service worker + IndexedDB)
   |  HTTPS (HTTP/2)  REST + bootstrap        |  one WebSocket per tab (multiplexed channels)
   v                                          v
 +---------------------------- Tasklane node (Go) -----------------------------+
 | HTTP router | Auth/session | Permission cache | Rate limit                    |
 |------------------------------------------------------------------------------|
 | API layer:  /rest/api/3 (Jira-compatible)   /api/v1 (UI bootstrap, mutations)|
 | Realtime:   WS gateway -> presence + field-change events (no text sync)       |
 |------------------------------------------------------------------------------|
 | Domain: issues | workflow | fields | boards/sprints | search | import-export  |
 |------------------------------------------------------------------------------|
 | Data: PostgreSQL = native projects + read-only Jira mirror <- Jira sync worker  |
 +----------+------------------------+-------------------+-----------------------+
            |                        |                   |
       PostgreSQL            Blob store            Redis (2+ nodes)
   (data, history, drafts,   (local FS default,    presence, events, cache
    outbox, FTS)              or S3/compatible)
                                                   (required for 2+ nodes; unused on one node)
            ^
            | outbox -> workers: notifications, webhooks, search index, automation,
            |           auto-sprint, retention, import/export jobs
```

Deployment profiles, same artifact (one Go binary):

| Profile | Components | Use |
|---|---|---|
| Single node | App + PostgreSQL, in-process pub/sub, local filesystem blob store | On-prem small team, evaluation |
| Scaled | N app nodes behind a load balancer, PostgreSQL primary + replicas, Redis, S3 (or shared filesystem) blob store, and OpenSearch for large instances | Enterprise |

The single-node profile is the latency reference: no network hops other than browser to app, and app to local Postgres.
Both profiles ship as Docker Compose files under `deploy/compose/`; the same files, with test overlays, are the test
environments (TESTPLAN 3.1), so what is tested is what is shipped.

### 10.1 Suggested Go layout

```
cmd/tasklane            main, config, wiring
internal/api            HTTP handlers, REST v3 compat, UI bootstrap endpoints, WS upgrade
internal/rt             realtime: gateway, presence, field-change event fan-out
internal/text           text saves (compare-and-set), drafts, rich-text schema validation and sanitization, history writer
internal/domain/...     issue, workflow, field, board, sprint, search, notify, automation
internal/mcp            MCP server (tools, resources, prompts), agent identities, leases, agent-view projection
internal/blob           blob store interface and backends (fs, s3, tiered), uploads, signed URLs, GC, fsck
internal/scm            Git providers (GitHub, GitLab), webhook inbox, neutral model, issue linking, rules, smart commits
internal/mail           mailers (SMTP, provider APIs), inbound fetchers, MIME, authentication, quote stripping, bounces
internal/cluster        node registry, versions, feature gates, drain, migration runner, `doctor`
internal/lifecycle      plan and execute convert, move, clone, delete, restore, purge; key aliases; work calendars
internal/search         Searcher interface, postgres and opensearch engines, indexer, reindex, reconcile, permission filter
internal/store          Postgres access (pgx), migrations, read models, outbox
internal/jiramirror     Jira sync (backfill, webhooks, reconcile), ADF reader, permission mapping, cut-over
internal/jobs           worker pool and schedulers
internal/web            embedded SPA assets (go:embed, pre-compressed)
```

## 11. Backend design

### 11.1 Request path and latency discipline

Budget for any read screen is 300 ms p95 end to end; the server owns at most **50 ms p95** of it (stricter than the
SPEC section 21 server-side figure, which should be tightened to match).

- **Bootstrap endpoints.** The UI never assembles a screen from many calls. `GET /api/v1/issues/{key}/view`
  returns the whole ticket (issue, field values, first comment page, attachments metadata, transitions the user may
  perform, permissions, subtasks and links summary, presence hint) in **one round trip**. Equivalent calls exist
  for the list (`/issues/list`), board (`/boards/{id}/view`) and backlog.
- **Parallel fan-in.** Inside a handler, independent queries run concurrently (`errgroup`) over a pooled connection set
  (pgx pool sized to cores); no N+1; every query is indexed and covered by a documented plan; slow queries are logged
  with plan above 20 ms.
- **Read models.** Hot screens read from narrow, denormalized projections (`issue_list_row`: key, summary, type,
  status, priority, assignee, updated, counts) maintained transactionally with the write. A list page is one
  index-ordered scan, not a join across eight tables.
- **In-process caches** (LRU, invalidated by pub/sub): permission and role schemes, workflows, field configurations and
  contexts, user sessions, project metadata. These are read on every request and change rarely, so they must cost
  microseconds.
- **Wire format.** JSON with short stable keys for the UI endpoints, `zstd` or `br` negotiated, HTTP/2 keep-alive, `ETag`
  and `If-None-Match` so repeat opens cost one tiny 304. `Server-Timing` is sent on every response for RUM.
- **No work on the hot path that can be deferred:** notifications, search indexing, webhooks and automation run from
  the outbox after commit.

### 11.2 Write path and concurrency control

Two kinds of data, two concurrency models (SPEC sections 4.5 and 21):

| Data | Model | Conflict behavior |
|---|---|---|
| Rich text: summary, description, rich-text custom fields | Compare-and-set on a per-field `text_seq` (section 12.2) | A stale save gets `409` with the current text and the person resolves it in the conflict dialog; nothing is overwritten unseen |
| Scalar fields: status, assignee, priority, labels, selects, dates, rank | Per-field versioned last-writer-wins | Server is authoritative and stamps each field change with an issue version; a stale write to a different field merges, to the same field is resolved by order and the loser is told |
| Workflow transitions | Transactional, validated server side | Rejected with a reason when a condition or validator fails (shown on the board drag) |
| REST/API scalar writes | `PUT` with optional `If-Match` on `version` | 412 on mismatch when the caller asks for it; otherwise field-level merge as above |
| REST/API text writes | Anchored patch, `If-Match` on `text_seq`, or bare replace | Patch applies only on a unique `old_text` match; conditional replace returns 412 with the current text if the field changed; bare replace is last-write-wins and recorded in history (section 12.2) |

Every mutation is one Postgres transaction that writes the change, the read-model projection and an
**outbox** row (events for notifications, webhooks, search, automation, and the realtime fan-out). A commit therefore
produces exactly one durable event stream; nothing is lost on a crash and nothing is sent for a rolled-back change.

Rank uses lexorank-style strings; a reorder touches one row (with periodic rebalancing in the background).

### 11.3 Data model notes

- `issue`: id (uuid v7, time-ordered), project_id, key, number, version (bigint, bumps on scalar and structural changes only, never on text saves), `summary` and `summary_seq`, scalar columns,
  and `fields jsonb` for custom field values (GIN indexed). Long text lives in `issue_text(issue_id, field_id, doc jsonb, text,
  text_seq, updated_by, updated_at, last_save_id)` (section 12.2); `draft` holds private autosaved drafts (section 12.3).
- Searchable custom fields are also copied to typed side tables or generated columns, so TQL queries use ordinary
  indexes rather than JSON scans.
- `outbox`: (id, aggregate, type, payload, created_at, processed_at) with `FOR UPDATE SKIP LOCKED` workers.
- `issue_history` and `text_version`: the history store (section 12.8), written asynchronously from the outbox; the source of the thread's slim activity lines.
- UUID v7 keys keep inserts index-local, which matters for write latency on large tables.

### 11.4 Search

Two engines behind `internal/search.Searcher` (SPEC 10.1): `postgres` (full text plus trigram, default) and `opensearch`
(required to hold 400 ms p95 at 5 million issues; an additional service). TQL compiles to SQL or to an OpenSearch query;
both run against one shared conformance suite, and a router sends queries that need exact or historical data (`was`,
`changed`) and the exact consumers (boards, backlog, rules, bulk targets, permission checks, exports, single-issue reads)
to PostgreSQL regardless of engine.

**The index is derived and may lag; PostgreSQL is authoritative.**

```
 mutation --tx--> issue row + outbox(search_seq) --> indexer workers --bulk, external version=search_seq--> OpenSearch
 query:  TQL -> plan -> permission pre-filter (user's current project and security-level sets, cached from Postgres)
         -> OpenSearch: ids + scores only -> final authorization of the ids in Postgres -> hydrate display data and
         snippets from Postgres -> response (totals marked approximate)
```

- **Indexer:** one outbox consumer group; batches by 500 documents or 1 s; idempotent through the external version
  (`search_seq`, a per-issue monotonic counter bumped by any change relevant to search), so duplicates, retries and
  out-of-order delivery cannot regress a document; failures retry with backoff, then a dead-letter table with alerts;
  backlog cap with metrics, and no effect on write latency.
- **Mapping:** strict dynamic mapping versioned by name (`issues-v{N}`) behind an alias; typed custom fields get typed
  mappings generated from field definitions; internal comment text and restricted fields live in separate fields;
  the document contains `org_id`, `project_id`, `security_level_id` and `deleted`/`archived` flags, not user lists, so
  permission changes never require reindexing.
- **Reindex:** build `issues-v{N+1}` from a keyset scan of PostgreSQL (throttled, checkpointed), dual-write live changes
  to both indices while it builds, verify counts and bucket checksums, atomically move the alias, keep the old index
  for a rollback window, then delete it; the same machinery reindexes one project.
- **Reconcile:** hourly per project, compare document counts and the maximum and a checksum of `search_seq` in buckets with
  PostgreSQL; repair differing buckets and delete orphans (purged issues); results appear in Admin and metrics.
- **Permission filtering:** the pre-filter comes from the permission cache (11.1) as a `terms` filter on `project_id` and
  `security_level_id`; users above the configured term limit use an exclusion filter, then PostgreSQL. The final
  authorization of returned ids is one batched check against the same cache (microseconds per id), so lag, moves and
  deletes cannot leak an issue. Highlights and snippets are produced from PostgreSQL text after authorization, never
  from index highlighting.
- **Routing and fallback:** a health probe and circuit breaker mark the engine unavailable; `Searcher` then uses PostgreSQL
  within a statement timeout and a size guard, otherwise returns indexed shortcuts and a "limited" state.
- **Operations:** OpenSearch connection, TLS and credentials come from the configuration and secret store (17.7); `doctor`
  checks version and cluster health; Compose profile `search` runs one node in CI and three nodes for performance runs;
  rolling OpenSearch upgrades within the supported version range; the reindex time estimate for the `scale` dataset is a
  published performance result.


#### PostgreSQL engine design (up to 100,000 issues per instance)

**Tables.** Text lives in narrow side tables written by the indexer worker (an outbox consumer, not the user's transaction), so
two-second typing updates never rewrite the wide `issue` row or its indexes and GIN maintenance is isolated and tunable.
Rows are removed on trash and re-added on restore, so no partial indexes are needed. All of this is eventually consistent
by a second or two, which is acceptable for search and also applies to a text clause inside a board filter.

```sql
CREATE TABLE issue_search (
  issue_id          uuid PRIMARY KEY REFERENCES issue(id),
  org_id            uuid NOT NULL,
  project_id        uuid NOT NULL,
  security_level_id uuid,
  key_text          text NOT NULL,        -- 'ABC-123 OLD-12' current key and aliases
  summary_norm      text NOT NULL,        -- lower(unaccent(summary)), the trigram target
  tsv               tsvector NOT NULL,    -- weights: A summary, B labels/components/text fields, C description
  text_hash         bytea NOT NULL,       -- skip the write when nothing searchable changed
  updated_at        timestamptz NOT NULL,
  search_seq        bigint NOT NULL
);
CREATE INDEX issue_search_tsv      ON issue_search USING gin (tsv);
CREATE INDEX issue_search_trgm     ON issue_search USING gin (summary_norm gin_trgm_ops);
CREATE INDEX issue_search_proj_upd ON issue_search (project_id, updated_at DESC);

CREATE TABLE comment_search (           -- comments are separate rows: no write amplification on the issue,
  comment_id  uuid PRIMARY KEY,         -- per-comment visibility, edits touch one small row
  issue_id    uuid NOT NULL,
  project_id  uuid NOT NULL,
  internal    boolean NOT NULL,
  tsv         tsvector NOT NULL,        -- one weight
  created_at  timestamptz NOT NULL,
  search_seq  bigint NOT NULL
);
CREATE INDEX comment_search_tsv   ON comment_search USING gin (tsv);
CREATE INDEX comment_search_issue ON comment_search (issue_id);
```

Keys are looked up through the btree indexes on `issue(key)` and `key_alias(alias_key)`, never through the text index.

**Index type choices**

- **`tsvector`: GIN.** GIN is the right type for many-term containment lookups over a read-mostly corpus. Its pending list makes
  frequent small updates cheap; the pending-list size and autovacuum settings for these two tables are tuned from the write
  test (TQL-27) rather than guessed.
- **`pg_trgm`: GIN, on `summary_norm` only.** GIN answers `ILIKE`, `%` and prefix patterns faster than GiST at this size, at the
  cost of a larger index and slower updates. GiST is reserved for ordering by similarity (finding similar issues for
  duplicate detection), which is a separate, optional feature. Descriptions and comments are not trigram-indexed (too large,
  too much write cost); substring search applies to summaries and keys only, and needs at least three characters.
- **Extensions:** `pg_trgm` and `unaccent` (no others).

**Text configuration.** A custom configuration `tasklane_text` is `simple` plus the `unaccent` dictionary: lowercased, accent-
folded, no stop words, no stemming. This is deliberate for multilingual installs. An instance can switch to a language
configuration (`english` and others); the worker then builds a second `tsv` column with the new configuration, the indexes
are created concurrently, the query layer switches, and the old column is dropped, with no downtime. Dual stemmed-plus-
unstemmed columns are not the default because they double the index.

**Identifiers.** The default parser splits `ABC-123` into pieces. To avoid depending on that, the query layer (1) extracts tokens
that look like keys of this instance's projects and aliases and resolves them by exact lookup, listing those issues first,
and (2) the indexer additionally writes each key mentioned in the text as a whole lexeme into `tsv`, so "see ABC-123" in a
description is findable by key. Tokens such as `UTF-8`, versions and error codes follow the parser's behavior, which is pinned by
a contract test (TQL-22) so a PostgreSQL upgrade cannot change it silently.

**Ranking.** `ts_rank_cd(tsv, query, 33)` (weights `{0.1, 0.2, 0.5, 1.0}` for D, C, B, A; length normalization 1 and 32). Ranking every
match is the expensive part of PostgreSQL full text, so relevance is computed only over the 2,000 most recent matches that
survive the structured filter (the rank window), then ordered by rank descending and `updated_at` descending; the
UI offers "sort by updated" for full recall. Paging within the window uses a snapshot token; beyond it, results continue by
recency. A text clause with an explicit `ORDER BY` skips ranking. Highlights use `ts_headline` on the displayed rows only.

**Combining a text clause with structured filters (TQL).** The planner builds two plans and a simple rule picks one; both
are written with `MATERIALIZED` CTEs so PostgreSQL's own estimates for `tsquery` selectivity (which are weak) cannot flip the
order unpredictably.

```sql
-- filter first: the structured predicate is selective (est. <= 5,000 rows)
WITH q AS (SELECT websearch_to_tsquery('tasklane_text', $1) AS q),
base AS MATERIALIZED (
  SELECT i.id FROM issue i
  WHERE i.project_id = ANY($acl_projects) AND <structured predicates> AND i.deleted_at IS NULL
  ORDER BY i.updated_at DESC LIMIT 20000)
SELECT ... FROM base JOIN issue_search s ON s.issue_id = base.id, q WHERE s.tsv @@ q.q ...

-- text first: the structured filter is broad
WITH q AS (SELECT websearch_to_tsquery('tasklane_text', $1) AS q),
cand AS MATERIALIZED (
  SELECT issue_id FROM issue_search s, q WHERE s.tsv @@ q.q AND s.project_id = ANY($acl_projects)
  UNION
  SELECT issue_id FROM comment_search c, q
  WHERE c.tsv @@ q.q AND c.project_id = ANY($acl_projects) AND (NOT c.internal OR $can_see_internal)),
win AS MATERIALIZED (   -- rank window over post-filter matches
  SELECT i.id, i.updated_at FROM cand JOIN issue i ON i.id = cand.issue_id
  WHERE <structured predicates> AND i.deleted_at IS NULL
  ORDER BY i.updated_at DESC LIMIT 2000)
SELECT ... ORDER BY ts_rank_cd(s.tsv, q.q, 33) DESC, win.updated_at DESC LIMIT 50;
```

The rule uses cached counts per project and common filter (refreshed every few minutes): estimated structured rows at or below
5,000 use filter-first, everything else text-first. If both are broad the text-first plan still runs under its caps. Candidate
caps (20,000 and 2,000) bound the work; hitting the filter-first cap marks the result "limited". Totals use `LIMIT 10001`.
The permission set (`project_id = ANY(...)`, security levels, internal comments) is part of every plan, and the final
authorization of the returned ids is the same as for OpenSearch (SPEC 10.1).

**Typeahead.** (1) exact key or alias; (2) `summary_norm % q` / prefix `ILIKE` using the trigram GIN, minimum three characters,
restricted to the user's projects; (3) boosted by the user's recently viewed issues; `LIMIT 10`. One- and two-character
input searches keys by prefix and recent items only.

**Write path and maintenance.** The worker recomputes `tsv` only when `text_hash` changes (so typing a character in a title
and then undoing it costs nothing), writes in batches, and bumps `search_seq`. `REINDEX CONCURRENTLY` is the documented repair; an online
rebuild procedure covers configuration changes. `pg_stat_statements` and `auto_explain` (above 100 ms) feed a plan
regression check.

**Capacity.** Index sizes, build times and write cost per 100,000 issues are measured by PERF-16 and published with the sizing
guide; the 100,000 limit is a tested guarantee, with headroom measured above it and the warning thresholds (80,000 and
100,000) set from the results.

### 11.5 Jira mirror (one-way inbound sync)

`internal/jiramirror` implements SPEC 18.1. There is no second backend and no write path to Jira: Jira data is copied into
PostgreSQL by a sync worker, and every handler, search query, board and report reads PostgreSQL exactly as for native
projects. That is why mirrored screens meet the same latency budgets and why nothing here touches the live-editing path.

```
 Jira Cloud --webhook (hint)--> /jira/hook/{conn} --verify, dedupe, enqueue catch-up--+
     ^                                                                                  v
     +---- REST (read-only creds, token bucket, ETag) <---- sync workers: backfill | incremental (updated >= watermark - overlap)
                                                                | reconcile (id sets, updated buckets) | permission refresh
                                                                v  one transaction per issue
                         issue + comments + links + issue_history (from changelog) + attachment metadata + ext identities
                                                                v  outbox
                              search index, projections, realtime refresh for viewers, in-app notifications, dev panel
```

- **Identity and origin.** Mirrored rows carry `origin = 'jira'`, `external_system`, `external_id` (Jira issue id) and the Jira key;
  the project has `read_only_mirror = true`. The key resolver (11.12) records alias entries when Jira reports a key change.
- **Sync state:** `jira_connection`, `jira_project_sync` (project, JQL scope, `watermark_updated`, `watermark_id`, state, last
  full reconcile, lag), and a small webhook inbox keyed by delivery id. Webhooks only enqueue an immediate catch-up; polling by
  `updated >= watermark - overlap` is the source of truth, with an overlap that covers Jira's search indexing delay, so a
  lost, duplicated or reordered webhook never loses a change. Upserts are idempotent on (Jira issue id, `updated`); changelog
  items are written to `issue_history` idempotently by changelog id.
- **Fetching:** the Jira client has a per-site token bucket, `Retry-After` handling, conditional requests where supported,
  bounded concurrency, and low priority for backfill and reconcile so an initial sync of a large project never competes
  with incremental updates. Page cursors and progress are persisted, so every job resumes after a crash.
- **Deletes and moves:** the hourly reconcile compares Jira id sets with PostgreSQL per project (and uses the webhook when it
  arrives); a missing issue is removed through the purge path (no trash, SPEC 18.1), and a changed key becomes an alias.
- **ADF:** converted by a read-only reader to the ProseMirror document stored for display, plus materialized text for
  search; unsupported nodes are kept as opaque blocks and rendered as an "unsupported content" block with a link to Jira.
  Mirrored issues have no editor, no draft and no presence of editors.
- **Read-only enforcement is in the mutation layer**, not the UI: every command (field edit, comment, transition, create,
  convert, move, clone, delete, clone-into, import) checks the target's `read_only_mirror` flag and returns the typed error
  `read_only_mirror` containing the Jira URL. The UI disables the same controls from the same flag; REST, MCP, automation, the
  Git rules and email use the same check, so there is exactly one place to get it right. Tasklane-side personal state (stars,
  unread, snoozes, saved filters, dashboards) is stored separately and is writable.
- **Permissions.** Roles, groups and memberships, permission-scheme browse grants, security levels and their members, and
  comment restrictions are mirrored into `ext_*` tables and evaluated by the same permission engine and cache (11.1) as native
  projects, so the search pre-filter and final authorization (11.4) work unchanged. Evaluable grants: user, group, project role,
  application role or any logged-in user, reporter, assignee, project lead. Anything else (custom-field grants) fails closed:
  the issue is hidden and a warning row is recorded; the per-project "verify with Jira" option checks access per user against
  Jira on first access and caches it briefly. Permission data is refreshed at least every 5 minutes and on relevant webhooks;
  a failed refresh keeps the last data only up to a staleness limit, then fails closed.
- **Attachments:** metadata only until first view (or eagerly by setting); the fetch uses the sync credentials, stores the file
  in the blob store with the usual scan and caps, and every download is authorized by the mirrored permissions.
- **Freshness and status:** per project lag (now minus the newest Jira `updated` applied), webhook-to-applied latency, backlog,
  errors and rate-limit headroom are metrics and are shown in Admin; the UI shows "synced N min ago" past a threshold.
- **Pause and restore:** the pause barrier (11.8) makes each project sync finish its batch and hold new applies; the marker
  records the watermark; a restore resumes from it and then reconciles.
- **Cut-over:** one transaction per project flips `origin` to native, clears `read_only_mirror`, keeps Jira keys as keys and
  aliases, keeps history and attachments, removes the sync state, and lets the live-document machinery create documents
  lazily from the stored text on first edit. Before cut-over the mirror can be rebuilt from scratch without data loss because
  Jira remains the system of record.
- **Deferred (not built):** write-back. If it is added it must reuse the mutation layer (the same check becomes a policy) and
  needs echo suppression, a loop breaker and a conflict policy (SPEC 18.1). None of that is present in this phase, which is
  why the editing design has no Jira-specific behavior.

### 11.6 MCP server

The MCP server (SPEC 18.3) is a thin adapter in the Go service, `internal/mcp`, over the same domain layer the
REST and UI APIs use. It adds no second data path.

- **Transports:** Streamable HTTP mounted on the main router (same TLS, auth middleware, rate limiter and access logs);
  the `tasklane mcp` subcommand speaks stdio to the client and proxies to that HTTP endpoint, so there is one
  implementation of every tool.
- **Identity and authorization:** each request resolves to an agent identity (agent account, or on-behalf-of user with
  an intersected permission set) and a scope set from the token; the permission cache (11.1) is applied exactly as
  for users, then scopes narrow it further. A tool call that fails either check returns a normal tool error.
- **Agent view projection:** results come from a dedicated compact projection (Markdown text, requested fields only,
  collapsed history, provenance tags on every text block), built from the same read models as the UI bootstrap
  endpoints so `get_issue_context` is one query set and meets the server-time budget.
- **Writes:** go through the normal mutation path (versioned, history, outbox) with the agent actor and an optional
  idempotency key stored for a short window to make retries safe. `claim_issue` is one transaction (assignee, status,
  lease row); leases are expired by a worker.
- **Text edits:** `edit_issue_text` is the anchored patch of 12.2 with the agent as the actor: applied inside one transaction only if
  `old_text` matches once, then compare-and-set on `text_seq`, so a concurrent human save cannot be silently overwritten.
- **Subscriptions:** resource subscriptions are served from the same realtime event stream as the UI (12.7); the
  gateway sends MCP resource-updated notifications on the open Streamable HTTP session.
- **Budgets and limits:** per-agent token buckets and write budgets live beside the rate limiter; result size caps and
  cursors are enforced in the projection layer.
- **Observability:** each tool call is a trace span and an audit record (agent, client, tool, redacted arguments,
  outcome); metrics per tool: calls, errors, p50/p95/p99, bytes returned.

### 11.7 Blob store

Large binaries never touch PostgreSQL (SPEC 17.6). The Go service has an `internal/blob` package with one interface and
two implementations; everything else (attachments, MCP, import/export, thumbnails) uses the interface.

```go
type Store interface {
    Put(ctx, r io.Reader, hint PutHint) (Ref, error)      // streams, hashes on the fly, returns content-addressed ref
    Open(ctx, ref Ref, rng *Range) (io.ReadCloser, Meta, error)   // range reads for video seek and log tail
    Stat(ctx, ref Ref) (Meta, error)
    Delete(ctx, ref Ref) error                            // called by GC only
    NewUpload(ctx, hint UploadHint) (Upload, error)       // resumable: parts, complete, abort
    SignedURL(ctx, ref Ref, ttl time.Duration, op Op) (string, error)  // presigned (s3) or app-signed (fs)
}
```

- **`fs` backend:** writes to a staging directory on the same volume, fsyncs, then atomically `rename`s into the
  hash-sharded final path, so a reader never sees a partial file and a crash leaves only staging debris that the
  janitor removes. Range reads use `ReadAt`. Optional app-level envelope encryption per org.
- **`s3` backend:** AWS SDK for Go v2 with a custom-endpoint option for MinIO and other S3-compatible stores; multipart
  upload for large objects with part-size tuning and retried, idempotent parts; presigned multipart for direct mode;
  server-side encryption settings passed through; bounded concurrency per node.
- **Tiered:** a wrapper that routes by policy, reads through to the cold tier, and records the tier in metadata; the
  mover is a background job (copy, verify, switch, delete).
- **PostgreSQL holds the references** (`blob` table): id, **locator** (backend, bucket or root, key or path, optional
  version id), hash, size, MIME, class, tier, scan state, refcount, created_at. Attachment, inline-image, derived-file
  and export rows reference `blob.id`; no table stores file bytes. A tier move or backend migration rewrites the
  locator after the copy is verified, and the app never derives a storage path from user input. The commit order is blob first, then metadata in the transaction, so metadata never points
  to a missing blob; GC deletes only blobs whose refcount is zero beyond the retention window and not under hold.

**Upload and download paths (latency-aware)**

```
 proxy upload :  client --chunks--> Go (bounded buffers, SHA-256 on the fly) --> fs/s3 staging --> commit
 direct upload:  client --presigned multipart--> S3        ; client --complete--> Go validates size/hash/type --> commit
 download     :  client --> Go authorizes --> short-lived signed URL (redirect) or streamed proxy with Range
 derived      :  commit --> outbox --> worker: thumbnail / poster / log index / scan --> store
```

- Uploads and downloads use their own bounded worker pool and connection limits, so a 1 GB upload cannot delay a
  ticket open; HTTP handlers stream with fixed-size buffers (memory per transfer is constant).
- Attachment strip and thumbnails are lazy and cache-friendly (content-hash URLs, immutable), so they stay off the
  300 ms data path; the ticket view bootstrap returns only attachment metadata.
- Log preview reads the line index and a ranged slice, so opening a multi-GB log shows the tail in one small request.
- Video is served with range support; the poster frame comes from a worker; transcoding is off by default and, when
  enabled, runs in a separate worker class with CPU and time limits.
- Antivirus runs as a worker step: the blob stays `scanning` (not downloadable) until `clean`.

**Multi-node rules:** `fs` requires a shared volume (checked at startup); `s3` works on any node. Node-local temporary
staging is allowed in both. Signed `fs` URLs are verifiable by any node (shared signing key from config).

**Operations:** `tasklane blob fsck` (missing, orphan, corrupt), `tasklane blob migrate --from fs --to s3`, usage metrics
(bytes by class, transfer throughput, error rate, scan queue, GC lag), and a storage health probe in readiness.

### 11.8 Consistency model and the pause state

**What is current and what lags.** Saved content is current at every moment: a save is acknowledged only after its transaction commits
in PostgreSQL, and the compare-and-set of 12.2 makes the commit order the truth. Only **derived** data trails, and all of it is
rebuildable from the outbox:

| Data | Source of truth | Staleness bound (design target) |
|---|---|---|
| Saved text, scalar fields, comments, transitions, rank | PostgreSQL | none |
| History records (`issue_history`, `text_version`) | Outbox, written by the history writer | a few seconds |
| Search index, search side tables | Outbox, written by the indexer | about 5 s p95 (SPEC 10.1) |
| Read-model projections, counters | Updated in the writing transaction or by the outbox consumer | at most 1 s |
| Notifications, webhooks, automation | Outbox | seconds |

**Why a pause still exists.** A consistent PostgreSQL snapshot is exact for saved content, so backups and exports need no flush of
editing state. The pause (SPEC 18.2) is for jobs that must not race with writes in their scope (imports into existing data, restores,
migrations, bulk replacements) and for jobs that want derived writers caught up.

```
 running --request--> draining --in-flight transactions done, requested writers at the marker--> paused --done/timeout--> running
                        |                                                                          |
                        | new writes in scope wait, then 503 + Retry-After                         | marker stored: DB log position, outbox
                        | workers in scope stand still                                             | position, time, mode
                        | (optional) history writer / consumers reach the marker                   | job reads from a DB snapshot at the marker
```

- **Scope and lease.** A `pause` row (scope, owner job, mode, started_at, deadline) in PostgreSQL is the single source of truth; every
  node watches it (Redis notification plus a short poll as a backstop). The row is a lease: it expires at its deadline, so a crashed
  job cannot leave the system paused.
- **Admission.** In scope, mutation handlers check the pause state, wait up to a short bound, then return `503` with `Retry-After`.
  Reads, presence and out-of-scope traffic are untouched. Open editors keep their drafts and retry (UI state in DESIGN 5.6).
- **Marker and snapshot.** The coordinator records `pg_current_wal_lsn()`, the outbox position and the time and opens a `REPEATABLE READ`
  snapshot exported to the job. In `snapshot` mode no write hold is needed; physical backups use the log position as the restore point.
- **Modes and deadlines.** `snapshot`, `pause`, `none` as in SPEC 18.2. Defaults: drain wait 60 s, `pause` hold 10 minutes; on timeout the job
  aborts and the scope resumes.
- **Resume.** The pause row is cleared and announced; held and retried saves arrive and go through the normal compare-and-set.
- **Restore.** Runs in `pause` mode: restore the database and blobs, run `fsck`, restart workers, replay the outbox to rebuild history
  and projections, and rebuild or reconcile the search index.
- **Observability and safety.** Metrics: pause duration, drain duration, held writes, deadline aborts; every pause is an audit event; the
  Admin status shows state, owner, scope and deadline.

### 11.9 Git integration service

`internal/scm` implements SPEC 18.4 behind a provider interface; GitHub and GitLab are the first two implementations.

```go
type Provider interface {
    VerifyWebhook(r *http.Request, conn Connection) (RawEvent, error)       // signature, size, replay checks
    Normalize(ev RawEvent) ([]Change, error)                                // provider payload -> neutral model
    Backfill(ctx context.Context, repo Repo, since time.Time) iter.Seq2[Change, error]
    Get(ctx context.Context, repo Repo, ref ObjectRef) (Object, error)      // reconcile single objects
    CreateBranch(ctx context.Context, repo Repo, name, base string) error   // opt-in write
    CommentOnPR(ctx context.Context, repo Repo, pr int, body string) error  // opt-in write
}
```

```
 provider --webhook--> /scm/hook/{conn} --verify, store in scm_inbox (delivery id unique), 202--> (done in under 100 ms)
                                              |
                              worker pool: Normalize -> upsert neutral objects -> recompute dev_link rows
                                              |  outbox
                      realtime event to open tickets, automation rules, notifications, search projection
 reconcile job (hourly) + backfill on connect --> Provider.Backfill / Get --> same upsert path
```

- **Neutral model** (tables): `scm_repo`, `scm_branch`, `scm_commit`, `scm_pull_request`, `scm_check_run` (GitHub checks and
  statuses, GitLab pipelines and jobs), `scm_deployment`, and `dev_link` (issue id, object type and id, source
  automatic or manual). Upserts are keyed by provider and external id and guarded by the provider's `updated_at` and
  head SHA, so late or replayed events cannot move state backwards.
- **Link detection:** a compiled matcher built from the instance's project keys and their aliases scans branch names,
  commit subjects, PR titles and descriptions; results are recomputed when the object changes and diffed against the
  existing automatic links.
- **Read path:** `dev_summary` (counts, worst build state, top rows) is part of the ticket and board read models, so the
  ticket bootstrap never calls a provider and stays within the 50 ms server budget; the list of full rows is loaded
  lazily when the section is expanded.
- **Rules and smart commits:** executed by a worker after the event is stored, through the normal mutation path with an
  actor of type `integration` (or the mapped committer for smart commits), so permissions, workflow validators, audit
  and loop protection apply exactly as for any other writer.
- **Rate limits and backpressure:** a token bucket per connection, conditional requests (ETags), `Retry-After` honored,
  backfill at lowest priority; an event storm is coalesced per repository before processing.
- **Security:** webhook endpoints are unauthenticated by session, authenticated by signature; request size cap; per-
  connection secret; self-hosted URLs go through the egress allowlist and private-range blocking (SPEC 17.1).
- **Observability:** per-connection delivery counts, lag from provider timestamp to processed, failures, reconcile
  repairs, rate-limit headroom, shown in the Admin tab and exported as metrics.

### 11.10 Email service

`internal/mail` implements SPEC 13.1 with a `Mailer` interface (SMTP; SES, SendGrid, Mailgun and Postmark APIs) for sending and
three fetchers (IMAP, provider webhook, SMTP listener) for receiving.

**Outbound**

```
 event --> outbox --> mail_outbox (one row per recipient, idempotency key = event id + recipient)
                          |  worker: render at send time (recipient locale, time zone, visibility check)
                          v
                 Mailer.Send --> relay/provider --> recipient          retries with backoff; rate limit per provider
                          |
                 mail_message (direction=out, Message-ID, thread root, status, VERP return path)
```

- **Threading:** each issue has a virtual root id `<tl.{issue uuid}@domain>` (never sent as a message); every mail has
  `References: <root> <previous...>` and `In-Reply-To` the previous mail of the issue, with a deterministic
  `Message-ID` (`<tl.{issue uuid}.{event id}@domain>`). Threads survive lost messages and key moves because they use the
  issue uuid.
- **Visibility** is evaluated when rendering, not when queuing, so a security-level or permission change between event and
  send cannot leak content. Internal comments are excluded for any recipient without the right.
- **Return path:** `bounce+{message token}@domain` (VERP) so every bounce maps to one message and recipient.
- **Rendering:** templates per event and locale, plain text first, HTML as a second part; no remote resources; the reply marker
  line and reply address are injected by the renderer so quote stripping can rely on them.

**Inbound pipeline**

```
 IMAP poll/IDLE | provider webhook (signature verified) | SMTP listener
            |  store raw .eml in blob store (class mail) + mail_inbox row (Message-ID unique) -> ack source (mark/move)
            v
   parser (bounded MIME depth/size, charset decode) -> authenticate -> classify -> resolve target -> match sender
            -> authorize -> strip quotes -> create comment or issue through the normal mutation path (source "via email")
            -> outcome recorded: accepted | ignored(reason) | quarantined | rejected
```

- **Resilience:** the raw message is persisted before the source is acknowledged, so a crash never loses mail; processing is
  idempotent on `Message-ID`; IMAP progress is a UID checkpoint with UIDVALIDITY handling.
- **Authentication:** Tasklane verifies DKIM itself on the stored raw message and checks DMARC alignment of the From domain
  (DNS lookups use the egress policy and a short-lived cache). SPF and the receiving MTA's verdict are trusted only from
  a connection marked `trusted_receiver` or from provider verdict fields. A reply additionally needs a valid token and a
  sender equal to the token's user.
- **Reply token:** `reply+{base64url(issue_id, user_id, expiry, nonce)}.{HMAC}@domain`, HMAC key from the secret store with a
  key id for rotation, default expiry 90 days; bound to one recipient so forwarding a notification does not transfer it.
- **Target resolution order:** token, then `In-Reply-To` and `References` against stored message ids, then the project address
  for new issues, then the `[ABC-123]` subject key (the sender must hold Add Comments), resolved through key aliases.
- **Classification before any action:** bounce or DSN (to bounce handling), auto-generated (`Auto-Submitted`, `Precedence`,
  `X-Auto-Response-Suppress`, null return path, list headers), own mail (instance header and loop counter), duplicate,
  then human mail. Rate limits (sender, sender domain, issue, project) are token buckets in Redis (in-process on a single
  node); quarantined mail waits in a review queue.
- **Quote stripping** is an ordered set of high-precision strategies: our reply marker line; HTML structure (Gmail quote,
  `blockquote type=cite`, Outlook reply separators, Yahoo quote); text patterns (multilingual "On ... wrote:", original
  message and header blocks, `>` runs); signature delimiters and known mobile signatures. Inline replies (quoted lines
  interleaved with new text) are detected and kept whole as quoted blocks. If stripping would leave almost nothing from a
  long message, the full text is used. The raw message is always linked from the comment. The rules are developed against
  a recorded corpus of replies from real clients (TESTPLAN MAIL-12) with a measured false-strip rate.
- **Bounces:** DSN (RFC 3464 style) and provider webhooks are parsed into hard or soft with a diagnostic; `mail_suppression`
  (address, reason, since, hard count, soft streak) is consulted before every send. Hard bounce or complaint suppresses
  immediately; soft bounces after a threshold. Admin and the user see the state, and clearing it is audited.
- **Data:** `mail_connection`, `mail_outbox`, `mail_message`, `mail_inbox`, `mail_suppression`, `mail_quarantine`; raw
  messages in the blob store with retention and GDPR erase (SPEC 17.3); secrets in the secret store.
- **Observability:** queue depth and age, send latency, bounce and complaint rates, inbound lag, outcome counts by reason,
  quarantine size, authentication failures.

### 11.11 Packaging, configuration and upgrade mechanics

**Cluster membership and feature gates.** Each app node registers in `node_registry` (node id, build version, supported
protocol and schema ranges, started, heartbeat). `cluster_min_version` is the minimum over live nodes. A feature gate
(`feature_gate`: name, required version, state, activated_at) turns on only when `cluster_min_version` reaches the required
version, automatically or after admin confirmation for risky gates. A node refuses to start when the database schema is
outside the range its build declares. Pub/sub messages carry a version and ignore unknown fields.

**Migrations.** `internal/store/migrations` holds ordered migrations, each annotated `expand`, `contract` or `data` and with a
declared lock level. `tasklane migrate plan|up|finalize|status`: `up` runs only `expand` migrations under an advisory lock
with lock and statement timeouts; `finalize` runs `contract` migrations after all nodes are on the new version; `data`
changes run as background jobs with progress. A CI linter rejects, in `expand` migrations, statements that take long locks or
rewrite large tables (for example adding a non-null column with a default on a large table without a safe pattern, or an
index created without `CONCURRENTLY`). DDL is transactional where PostgreSQL allows it, and steps that cannot be are
written to be resumable.

**Drain.** On shutdown signal or deploy, a node: fails readiness (the balancer stops sending new connections), sends
WebSocket clients a reconnect hint with jitter, lets in-flight requests and saves finish,
stops workers after finishing their current item, and exits. Clients reconnect to another node and refetch on focus;
drafts are untouched.

**Configuration loader.** A typed schema (Go structs with tags for default, units, secret, hot-reload) is the single source
for validation, the `config print --redacted` output and the generated reference. Layers are merged in the fixed order
defaults, file, environment, flags; `*_FILE` and secret-manager references are resolved at load; unknown keys fail
with a nearest-match suggestion. Hot-reloadable keys are applied by an atomic swap of an immutable config snapshot, and an
invalid reload leaves the old snapshot active. Secrets are held in a separate redacting type that never formats into logs.

**Build and release.** A single CI pipeline builds the multi-arch image and the tarball from one source, runs the licence
scan (section "licensing" below), generates the NOTICE file and SBOM, signs artifacts, and publishes release notes with
migration and configuration changes extracted from the migration annotations and the config schema diff.

**Licensing mechanics.** `go-licenses`-style scanning for Go modules, a package-manager scanner for npm and Cargo, and a base
image scan run in CI with an allowlist and a denylist (SPEC 17.9); the build fails on a violation. Release artifacts
contain no code under a denylisted licence, and external services (PostgreSQL, Redis or Valkey, MinIO, ffmpeg, ClamAV)
are never copied into images. Hosted-only code (metering, billing, plan enforcement) lives behind interfaces with no-op
implementations in the self-hosted build; a test asserts the self-hosted binary makes no outbound connection when idle.

### 11.12 Workflow engine, issue lifecycle and work calendars

**Workflow storage.** `workflow` and immutable `workflow_version` (definition JSON with a checksum; states draft, published,
archived), `status` (name, category), `workflow_scheme` (project, issue type to workflow). An issue stores `status_id` and
`workflow_version_id`. Published definitions are compiled and cached per version (LRU, invalidated on publish), so a
transition costs microseconds of rule evaluation.

**Transition execution.** One transaction: lock the issue row, evaluate conditions (read-only) and validators, apply the status,
resolution and transition-screen fields, run the in-transaction post-functions (set field, assign, clear resolution), write
history and the outbox rows. Asynchronous post-functions (webhooks, automation, create subtask through the outbox, out-of-process
apps with signed requests) run after commit with idempotency keys. Conditions, validators and post-functions are typed handlers
from a registry with versioned config schemas; third-party functions can only run asynchronously.

**Publish and in-flight migration.** Publishing validates reachability and the initial status, and requires a mapping for every
removed status. A migration job moves affected issues in batches (default 500 per transaction), updates `status_id` and
`workflow_version_id`, writes history only where the status changed, and reports progress; issues not yet migrated keep
transitioning under their old version, so there is no stop-the-world window.

**Lifecycle operations** (SPEC 9.1) share one pipeline: `Plan(op, scope)` returns changes, dropped values, warnings and
counts; `Execute(plan)` runs per-issue transactions with the idempotency key (job id, issue id) as a resumable `lifecycle_job`
(cursor, state, per-issue results). Single-issue operations run inline. Before restructuring an issue the executor
takes the per-issue edit lock (a save in progress finishes first) and publishes a `moved`, `converted`, `deleted` or `cloned` event
so open editors show a notice and keep their drafts. Channel names, drafts, mail thread ids, reply tokens and blob and Git
links use the issue uuid, never the key.

- **Key aliases and numbering.** `key_alias(alias_key unique, issue_id, project_at_time, reason, created_at)` and
  `project_counter(project_id, next_number)`; moves allocate numbers from the target counter (in blocks for bulk jobs to
  avoid lock traffic). One resolver package, `ResolveKey`, serves the UI router, REST, TQL, MCP, the Git linker, the mail
  parser and webhooks (positive and negative results cached; a test fails any endpoint that looks up a key by itself).
- **Move and convert.** Move updates project, key and number, remaps status and field values through the plan, rewrites the
  read-model rows and the search document through the outbox, and leaves blobs, comments, links and history untouched.
  Values that cannot be mapped go to `issue_removed_value(issue_id, field_id, value, source_op, expires_at)` with the trash
  retention, so moving or converting back restores them.
- **Trash and archive.** Issues carry `deleted_at`, `deleted_by`, `purge_after`, `delete_batch_id` and `archived_at`. Hot
  indexes are partial (`WHERE deleted_at IS NULL`) so lists, boards and search pay nothing for trashed rows; projections
  and the search document are removed through the outbox on delete and rebuilt on restore. The purge worker (batches of 500)
  deletes children in a fixed order (comments, worklogs, attachment rows, links, history, drafts,
  mail references, dev links), decrements blob refcounts in the same transaction, honors legal hold, and never touches the
  audit log. GDPR erase uses the same path with immediate effect.
- **Clone.** The planner copies rows; attachments by blob reference (refcount incremented); the description by copying the stored
  document into the clone's `issue_text` row (`text_seq` starts at 1); comments
  (if chosen) keep author and original time with a `cloned_from` pointer; subtasks recurse up to the cap; cross-project
  clones reuse the move mapping.

**Work calendars and the sprint scheduler.** `work_calendar` rows (scope org, project, board, team, user) resolve through a
single `CalendarFor(scope)` with precedence user, team, board, project, org. The scheduler computes sprint boundaries in
the board's IANA zone using the shipped tz database (updatable with releases), converts them to UTC instants, and stores
a `calendar_snapshot` (working days, hours, holidays) on the sprint. Local times that do not exist or repeat at a
daylight-saving change resolve by a fixed rule (next valid instant, first occurrence). Week numbers come from a numbering
function per system (ISO 8601, first-day-of-year, none). Holiday data is versioned region sets plus custom lists. The
scheduler job is idempotent, takes an advisory lock per board, and runs against the injectable clock (TESTPLAN 3.1).

## 12. Concurrent editing: advisory presence and optimistic save

SPEC 4.5 replaces Google-Docs-style simultaneous editing with presence that only informs, private autosaved drafts, and saves
that are checked against a field version. There is no merge engine in the server, no per-keystroke traffic and no second runtime.

### 12.1 Choices and why

- **Rejected: real-time co-editing** (a CRDT with a doc-engine sidecar). It needed a second runtime to ship and supervise, a
  correctness surface of its own (schema repair after concurrent structure edits, garbage collection against attributed history,
  client-id collisions, restore epochs, schema checks on raw updates) and reliability budgets measured per keystroke. At the team
  sizes Tasklane targets, two people saving the same field within the same editing window is rare, and a conflict dialog is
  a good-enough answer. It can be reconsidered later; nothing in the data model prevents it.
- **Chosen:** ephemeral presence (advisory), private drafts, compare-and-set saves on a per-field version, and a client-side
  three-way merge when a conflict does happen. Postgres is authoritative and current for saved content at every moment.

### 12.2 Versions and the save protocol

- **Storage.** Long text fields (description and rich-text custom fields) live in `issue_text(issue_id, field_id, doc jsonb, text,
  text_seq, updated_by, updated_at, last_save_id)`; the summary is in `issue` with `summary_seq`. `issue.version` counts scalar and
  structural changes only. Text saves never bump it, so a REST `If-Match` on `version` is unaffected by anyone typing or saving text.
- **Save** is `PUT /api/v1/issues/{key}/text/{field}` with `{doc, base_seq, save_id}`. One transaction: permission, field and
  mirror read-only checks; validation and sanitization (12.6); then

```sql
UPDATE issue_text SET doc = $doc, text = $text, text_seq = text_seq + 1, updated_by = $user, updated_at = now(), last_save_id = $save_id
 WHERE issue_id = $id AND field_id = $field AND text_seq = $base_seq
RETURNING text_seq;
```

  If a row comes back, the same transaction writes the outbox event (history input with before and after, search, notifications,
  realtime event, projections) and deletes the user's draft. If no row matches, the answer is `409` with the current document, its
  `text_seq`, who saved it and when; the base document is already on the client. The row lock serializes two simultaneous saves, so
  exactly one wins the compare-and-set and the other gets the conflict.
- **Idempotent retries.** A save whose acknowledgement was lost is retried with the same `save_id`; if `last_save_id` matches, the
  server returns success again without applying it twice.
- **API writes** use the same row: anchored patch (apply `old_text` to the current text inside the transaction, fail unless it matches
  once, then compare-and-set against the `text_seq` read in that transaction), conditional replace (`WHERE text_seq = $if_match`), and
  bare replace (no `text_seq` condition, last write wins, recorded in history). `Agent`, `Automation` and `API` actors are recorded as
  such.
- **Scalar fields** are saved per field with the value the client saw as the precondition (`expected`); if the stored value differs, the
  answer is `409` with the current value and the UI shows a conflict notice with a reapply action. Different fields never conflict.

### 12.3 Drafts

- `draft(user_id, issue_id, field_id, client_id, base_seq, doc, updated_at)`: private to the user, never indexed or shown to others,
  expires after 30 days, removed on issue purge and GDPR erase, counted against a per-user cap.
- The editor autosaves locally (IndexedDB) every 2 s off the main thread and to the server every 10 s with a last-write-wins upsert
  (keyed by user, issue, field, so two tabs of the same person do not create a conflict; the newest wins and the restore banner says
  which tab it came from). Closing the editor, saving or cancelling deletes it.
- On opening a field with a draft, if its `base_seq` equals the current `text_seq` the draft is restored with a banner; if not,
  the conflict dialog opens immediately with the draft as "mine".

### 12.4 Conflict resolution (client side)

The merge runs in the browser, next to the editor, on the three documents the client holds after a `409`: base (loaded when editing
started), theirs (returned by the server) and mine.

- **Algorithm.** Top-level blocks are aligned by content hash and similarity (longest common subsequence). Non-overlapping block
  changes from both sides apply automatically. A block changed by one side only takes that side. A block changed by both is merged
  with a three-way text merge over its runs (marks flattened to ranges); non-overlapping ranges merge, overlapping ranges become a
  **conflict hunk**. Lists merge per item and tables per cell. If the structure is incompatible (a block deleted by one side and
  edited by the other, a table reshaped on both sides), the dialog offers keep mine or keep theirs for that block.
- **Output** is the merged document plus a list of conflict hunks and a plain-language summary ("2 changes by Jana K., 1 block changed
  by both"), which also feeds the screen reader text.
- **Save again.** The merged result is saved with the new `base_seq`; if someone saved again meanwhile the dialog repeats. A document
  above the merge limit (default 1 MB) offers only keep mine, keep theirs and copy mine.
- Overwritten text is never destroyed: "keep mine" and a bare API replace are recorded in history and restorable (12.8).

### 12.5 Presence service

- A WebSocket channel per open ticket (`presence:issue:{uuid}`) over the single per-tab socket. Each node keeps an in-memory map per
  issue of who is viewing and which fields have an open editor. State changes only on joining, leaving, opening or closing an
  editor, and a 10 s heartbeat; its entries expire after 30 s. There is no per-keystroke traffic, so presence cost is negligible.
- Across nodes, changes are published through Redis and merged by each node; if Redis is unavailable each node shows only the
  presence it hosts, which is acceptable for something advisory. Nothing about correctness depends on presence.
- Authorization per channel as for any read (project, security level, field restriction); the "appear offline in editors" user setting
  and the org-wide switch are enforced at the gateway.
- Field-change events use the same socket: scalar changes, text saves (with `version` and `text_seq`), transitions, comments and
  attachments, delivered best effort. A client that sees a gap in `version`, or returns to the tab, refetches the ticket view.

### 12.6 Validation and sanitization on save

- The rich-text schema is one data definition (JSON) used to generate the editor schema on the client and the validator in Go.
- The server validates every saved document: node types, marks and attributes against allowlists; link and image schemes limited to
  `http`, `https` and `mailto` (and Tasklane attachment references); maximum depth (default 20), node count (default 50,000) and size
  (default 1 MB); then sanitizes and reports anything removed back to the saver. It derives the plain text used by search and REST,
  and converts to Markdown or ADF on demand.
- Defense in depth: the renderer sanitizes again and a strict content security policy forbids inline script. Saves are infrequent, so
  the cost is negligible; there is no per-update check and no engine to compromise.

### 12.7 Scalar field events and optimistic UI

- The client applies a scalar change optimistically, sends it with the value it saw, and keeps it marked pending. The server commits
  and emits an `event` with the new `version` to everyone, including the sender. The sender's own event only clears the pending flag.
- If the server answers `409` (validator, permission, or a different current value), the field rolls back with a notice explaining why
  and, for a same-field conflict, naming who changed it, with a reapply action.
- Every event carries `version` (and `text_seq` for text fields); a gap in `version` makes the client refetch the ticket view.
  Board and list rows update in place from the same events (fine-grained, no list reload).

### 12.8 History store (SPEC 4.5)

- `issue_history` (issue id, field id, saver, time, `before_ref`, `after_ref`, diff, source: UI, API, automation, email, import, Jira)
  and `text_version` (content hash, compressed text); an entry's after is the next entry's before and is stored once. A history
  writer consumes the outbox, idempotent on the event id, partitioned by time; only fields flagged "track in history" produce
  records. One record per saved change; the writer computes a word-level diff for display.
- A restore is a normal save of the earlier value (with the compare-and-set), which is itself recorded.
- **Eventual correctness:** writer lag is a metric with an alert; a reconcile job compares outbox events with history rows by event
  id and replays gaps; ordering is by (issue, field, `text_seq`). Erasure redacts the text in `text_version`; legal hold blocks
  deletion. For mirrored issues the same store is fed from Jira's changelog (11.5).

### 12.9 Reliability budget and race register

SPEC 21 states the rates (E1 lost acknowledged save, E2 undetected overwrite, E3 lost draft, E4 silent content change). Known failure
sources and what bounds them:

| # | Source | Class | Mitigation | Residual |
|---|---|---|---|---|
| 1 | Two saves at the same moment | E2 | Compare-and-set on `text_seq` in one statement; the row lock serializes; the second gets `409` | none |
| 2 | Acknowledgement lost, client retries | E1, E2 | `save_id` makes the retry idempotent | none |
| 3 | Tab closed or browser crashed before saving | E3 | Local autosave every 2 s, server draft every 10 s, restore banner | storage eviction, private mode |
| 4 | Conflict resolved, then someone saves again | E2 | The resolved save is compare-and-set again; the dialog repeats | none |
| 5 | "Keep mine" or a bare API replace overwrites | E2 (intended) | Recorded in history with before and after; restorable | by design |
| 6 | Sanitizer removes something the user wanted | E4 | Removed content is reported to the saver before leaving the editor; golden tests on the schema; schema changes versioned | schema changes |
| 7 | History writer or outbox consumer lags or fails | derived data only | Outbox replay and reconcile; saved content is unaffected | lag only |
| 8 | Failover loses an acknowledged commit | E1 | Synchronous standby in the scaled profile (RPO 0); single node documents its RPO (last WAL archive) | single-node disk loss |
| 9 | Save during a pause | E1 | `503` with `Retry-After`; the draft is kept and retried | none |
| 10 | Issue moved, converted or deleted while editing | E4 | Notice in the editor; a save after a move follows the issue id; a save after a delete returns `410` and the text stays in the draft | none |
| 11 | API or automation write races a human save | E2 | Same compare-and-set; anchored patch and conditional replace fail safely | bare replace is last write wins by design |
| 12 | Stale or wrong presence | none | Expiry and refresh; advisory only | none |
| 13 | Storage corruption of text or history rows | E1, E4 | Row checksums verified by a background scrubber; replicas and backups | detected and reported |

**Measuring.** Each acknowledged save increments a counter and returns its `text_seq`; a reconciler compares counters, stored
versions and history records and counts any acknowledged save that has no matching stored version (E1). Version-check outcomes (success,
conflict, retry-success) are counted, and any save accepted with a stale `base_seq` that was not an explicit bare replace is a
bug and counted (E2). Draft recoveries and unrecoverable editor sessions are reported by the client (E3). Sanitizer removals are
counted and sampled for review (E4). The test campaign (TESTPLAN 5.1) uses the rule of three for the sample sizes.

### 12.10 Latency targets (design targets, LAN, p95)

| Interaction | Target |
|---|---|
| Save of a text field, server time | under 50 ms |
| Save as seen by the user (button to confirmation) | under 300 ms |
| Conflict response (`409` with the current document) | under 100 ms; the dialog opens without another request |
| Open the editor with the editor chunk warm | under 100 ms |
| Presence change visible to others | under 1 s |
| Scalar change visible to others | under 1 s |
| Draft autosave | no main-thread block over 16 ms |

## 13. Frontend design

### 13.1 Stack

TypeScript SPA, built once and embedded in the Go binary. Framework choice is open (section 17); the constraint is a
small runtime with fine-grained reactivity (Solid, Preact with signals, or Svelte) over a heavy virtual-DOM
framework, because the list and board re-render constantly from events. Editor: **ProseMirror** (via a
thin wrapper) as a single-user editor; the ProseMirror schema, defined once as data and shared with the server validator (12.6),
is the canonical rich-text model, with converters to Markdown and ADF (this resolves the "rich text format" open question in SPEC section 23 in favor of
ProseMirror JSON plus an ADF converter).

### 13.2 Loading strategy (how 300 ms is met on the client)

1. **App shell from a service worker cache.** After the first visit the HTML, CSS and JS come from disk: zero network
   time to first render. Assets are content-hashed and immutable; the binary serves them pre-compressed.
2. **Parallel, not sequential.** On navigation the router starts, at the same time: the data request (bootstrap
   endpoint), the route's code chunk (if not cached), and the WebSocket join. Nothing waits for the previous step.
3. **Stale-while-revalidate from IndexedDB.** A previously seen list or ticket paints immediately from the local store
   (marked fresh within seconds, or stale-marked if old), then reconciles with the response. The common reopen has no
   visible loading state.
4. **Skeleton within one frame** for anything not cached; skeletons match row and card geometry (no layout shift).
5. **Editor is lazy but pre-warmed.** The ticket first paints as a lightweight read-only render of the stored document.
   The editor chunk (ProseMirror, merge code, about 150 KB gzipped target) is prefetched on idle after the list
   renders and on hovering or focusing a row, so by the time a ticket opens it is already in memory. Presence avatars
   need only the socket, which is available immediately; the editor opens in under 100 ms when its chunk is warm.
6. **Prefetch the next ticket** in the current list (and the previous), and prefetch on row hover (pointer devices), so
   prev/next and open-from-list are served from the local store.
7. **Budgets enforced in CI:** initial JS under 200 KB gzipped, editor chunk under 150 KB gzipped, no route chunk over
   60 KB gzipped, long tasks under 50 ms on the reference mid-range phone.

### 13.3 Client state

```
 UI components (fine-grained reactive)
        |
 Normalized entity store (issues, comments, boards, users by id)   <- in memory
        |   persisted (write-behind)                 ^ events (version-checked)
 IndexedDB (entities, query results, drafts)          |
        |                                       WebSocket channels
 Mutation outbox (optimistic ops, retried, ordered) -> REST / WS
```

- **Entities by id** with a version; query results are lists of ids. An `event` updates one entity and only the rows
  and cards that show it re-render.
- **Mutation outbox:** every user action is written to a persisted, ordered queue, applied optimistically, then sent.
  It survives reloads and offline periods (SPEC section 19: queued comments and transitions), and shows "N changes pending".
- **Drafts** are persisted per field in IndexedDB every 2 s (and sent to the server every 10 s, 12.3), so a crash, reload or
  offline period never loses typed text; scalars are server-authoritative with optimistic display.
- **Cache hygiene:** per-user, per-org namespaces; cleared on logout; entity TTLs; size cap with LRU eviction.

### 13.4 Rendering performance

- **Virtualized lists** everywhere (issue list, backlog, board columns, comments over a threshold) with fixed or measured
  row heights from DESIGN section 5, so 10,000 rows cost the same as 50.
- Board drag runs on the compositor (transform only), the drop commit is optimistic with rollback on a server veto.
- Presence and field-change events update the page in place; remote scalar changes animate with a 1 s highlight and
  respect reduced motion; an open editor is never modified by someone else's save (a banner is shown instead).
- No layout thrash: reads and writes of layout are batched; images and attachment thumbnails reserve size up front.
- Typeahead (search, mention, issue-key) debounces at 80 ms, cancels stale requests, and shows cached or recent
  results first.

### 13.5 Theming, i18n, accessibility in code

Tokens are CSS custom properties set on `<html data-theme>` from a server-rendered attribute (no flash, section 8).
Strings come from per-locale bundles loaded with the route. The accessibility checklist in section 7 is enforced by
automated checks (axe in CI) plus manual screen-reader passes for the editor, board drag and the previewer.

### 13.6 Client version negotiation and cache upgrades

- The WebSocket handshake and every API request carry the client build, protocol and cached schema version; the server
  answers with `min_client` and, when the client is too old, `reload_required`. The server accepts the previous client
  version for the whole skew window (SPEC 17.8).
- A new service worker installs in the background and waits; the UI shows a non-disruptive "update available, reload"
  prompt, or reloads at a safe moment (no unsent text) when the old version is no longer supported.
- Every outbox entry (SPEC 19, DESIGN 13.3) stores the client and schema version that created it. After an upgrade the server
  offers `MigrateMutation(old)`; if a mutation cannot be migrated it stays in the outbox as an item "needs your attention"
  with the original content preserved for copy or resubmit. Mutations are never dropped silently.
- Cached entities and drafts carry a schema version and the field's base `text_seq`; a draft written under an older schema is
  migrated, or opened in the conflict dialog with the original preserved, never silently altered.

## 14. Reliability, security, observability

- **Sockets:** `Origin` check, session cookie or short-lived signed ticket, per-channel authorization, message size and
  rate limits, idle timeout, graceful drain on deploy (clients reconnect and resync in under a second).
- **Resource bounds:** max document size and merge size, max drafts per user, max connections per user, max presence entries per
  ticket; over-limit requests get a clear error.
- **Graceful degradation:** if the realtime layer is down, the ticket still loads, saves still work and conflicts are still caught by
  version; presence disappears and open pages refetch on focus. If Redis is down, the multi-node profile is degraded and reports it in
  readiness: presence and events reach only the local node and cache invalidation falls back to short TTLs until it returns;
  saves are unaffected because they depend only on PostgreSQL. Operators run Redis with a replica and automatic failover; there is no
  second transport.
- **Observability:** OpenTelemetry traces across HTTP, WS and the outbox; Prometheus metrics; structured logs. Key metrics:
  p50/p95/p99 of each bootstrap endpoint, DB time per request, WS connections, presence entries, event lag, save latency,
  conflict rate, draft recoveries, outbox lag, and **client-side first-meaningful-data time** reported by RUM.
- **Security:** SPEC section 17 applies unchanged to the socket (permission schemes, security levels, field
  restrictions); CSP with no inline scripts, strict `SameSite` cookies, and attachment origins separated from the app origin.

## 15. Request and edit walkthroughs

**Open a ticket (warm client, LAN), typical timeline**

```
 t=0    click row            router starts 3 things in parallel
 t=1    paint from IndexedDB (previous version of the ticket), skeleton for anything missing
 t=2    GET /issues/ABC-142/view (If-None-Match)      + WS join presence:ABC-142
 t=3    server: one parallel query set, p95 under 50 ms, 200 or 304
 t=~40  response decoded, store updated, only changed rows re-render
 t=~50  presence received, editor chunk already warm
 t<300  p95 ceiling, including slower hardware and first-time chunk loads
```

**Two people edit the same description**

```
 A opens editor -> presence: "A is editing the description"      B sees the note, opens the editor too (gentle warning)
 A and B type  -> each autosaves a private draft (2 s local, 10 s server); nothing is shared between them
 A saves        -> PUT {doc, base_seq=7, save_id} -> CAS 7 -> 8 -> outbox: history, search, event "description saved by A"
 B's editor     -> banner "changed by A. See changes, or keep editing"; B's text is untouched
 B saves        -> PUT {base_seq=7} -> no row at 7 -> 409 + current doc (seq 8) -> conflict dialog
 B resolves     -> 3-way merge (client): non-overlapping blocks merge automatically; overlaps shown per block
                -> Save merged result {base_seq=8} -> CAS 8 -> 9 -> history keeps A's and B's versions
```

## 16. Verification plan (how the targets are proven)

- **Concurrency tests:** many simulated users saving the same fields at once through the real API; the invariants are that exactly
  one of any two saves with the same `base_seq` succeeds, no acknowledged save is lost, no stale save succeeds, and a retried
  `save_id` applies once. Run on every change to `internal/text`.
- **Merge tests:** a corpus of base, theirs and mine document triples (paragraphs, lists, tables, marks, mentions, edge cases)
  with expected merged output and conflict hunks, plus property tests that merging with an unchanged side is the identity and
  that merge is stable when swapped; run in the browser test runner against the real merge code.
- **Draft and crash tests:** forced tab kills, reloads, storage eviction and offline periods at random points in an editing
  session; the model checks what is recoverable against the E3 target.
- **Validation tests:** hostile documents (disallowed nodes and marks, `javascript:` links, deep nesting, huge documents) are
  rejected or sanitized and reported; sanitizer golden tests protect against silently changing content (E4).
- **Load tests (k6 or equivalent):** 5 million issue dataset; ticket open, list, board and search at the p95 targets; 50
  concurrent users saving text and scalar fields with presence on, and 1,000 idle connections per node, with save and
  event latency measured end to end; presence and events across 3 nodes.
- **Chaos:** kill a node mid-save, drop and delay packets, restart Postgres; the invariant is that an acknowledged save is never lost,
  a save without an acknowledgement is retried idempotently, and drafts survive.
- **Browser performance in CI:** headless runs on a throttled reference profile; the budgets in section 13.2 fail the build.
- **RUM in production:** first-meaningful-data time per screen, alerting when p95 exceeds 300 ms.

## 17. Decisions and risks

| # | Item | Position | Retire by |
|---|---|---|---|
| 1 | Collaborative editing model | **Decided:** real-time co-editing (CRDT with a doc-engine sidecar) dropped; advisory presence, private autosaved drafts, compare-and-set saves on `text_seq`, client-side three-way merge dialog (section 12). Revisit only if conflicts prove painful. | Done |
| 2 | One WebSocket per tab with channels | Adopted: carries presence and field-change events only, no text sync | Done |
| 3 | Rich text model | ProseMirror JSON canonical, Markdown and ADF converters | M1 |
| 4 | Frontend framework | Fine-grained reactive, small runtime; choose by prototype of list + editor within the JS budget | M1 |
| 5 | Pub/sub transport | **Decided:** Redis, required for 2+ nodes (presence, events, cache invalidation), none on a single node; no `LISTEN/NOTIFY` fallback. Events are best effort; clients refetch on a `version` gap or on focus, and caches have TTLs as a backstop. | Done |
| 6 | Server-side time budget | 50 ms p95 for reads; tighten SPEC section 21 accordingly | Now |
| 7 | Presence heartbeat and limits | 10 s heartbeat, 30 s expiry, state changes only; validated by the load test | M1 |
| 9 | Minimum PostgreSQL | 15 or newer with `pg_trgm` and `unaccent`; revisit each year against supported majors | M1 |
| 10 | Licence | Apache-2.0 proposed (SPEC 17.9); dependency policy enforced in CI regardless of the final choice | Before first public release |
| 11 | Upgrade support | Rolling from N-1, offline from N-2; contract migrations only in `finalize` (SPEC 17.8) | M1 (framework), M4 (guarantees) |
| 12 | Work calendars | Org, project, board, team and user scopes; sprint boundaries use the board calendar, capacity uses user calendars | M2 |
| 16 | Concurrency counters | **Decided:** `version` for scalar and structural changes (If-Match), `text_seq` per text field bumped by every save; text writes by saves with compare-and-set, anchored patch (preferred), conditional replace (412 with the current text) or bare replace (last write wins) | M1 |
| 17 | Jira integration | **Decided:** one-way inbound mirror into PostgreSQL, read-only in Tasklane, writes in Jira; write-back deferred; write-through adapter mode dropped; fail-closed permission mapping | M2 |
| 15 | Document history | **Decided:** separate, eventually consistent store; one record per saved change per tracked field with before, after and diff; erasure by redacting stored text | M1 |
| 14 | Search engine at scale | PostgreSQL by default for up to 100,000 issues per instance; OpenSearch (extra service, derived and lagging index) above that; permission pre-filter plus final PostgreSQL authorization; ids-only index results | M2 |
| 13 | Inbound email trust | DKIM verified by Tasklane, SPF only from a trusted receiver, reply token bound to the recipient | M4 |
