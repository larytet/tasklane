# Mirroring a Jira Cloud project (read-only by default, optional write-through)

Tasklane can be a fast view of a Jira Cloud project. Data flows **from Jira into PostgreSQL**; every screen reads PostgreSQL.
A mirrored project is in one of two modes, chosen per project by an administrator:

- **`read_only`** (the default). People edit in Jira (every mirrored issue shows an **Edit in Jira** link and how fresh the copy
  is). Nothing edited in Tasklane reaches Jira: every write path (UI, REST, MCP, automation, import) refuses the project with
  the same typed error, `read_only_mirror`, which carries the Jira site.
- **`write_through`**. People edit in Tasklane and each edit is saved to Jira at once, **as that person**, with their own
  Jira API token. The mirror changes only after Jira accepted the edit. See *Write-through* below.

**No Jira credential is ever stored in PostgreSQL.** The mirror's own token and each person's token live in the memory of the
running Tasklane process; a restart empties them (see *Connect* and *Write-through*).

## Connect

1. Create a service account in Jira with read access and an API token.
2. `POST /api/v1/jira/connections` `{"name","site_url","email","token","allow_private"}` (administrators). The credentials are
   checked at once; the token is kept **in memory only** (the database keeps the site, the account email and an 8-character
   fingerprint); the response gives the **webhook secret once**. `site_url` must be `https` (plain `http` only with
   `allow_private`, for a private host). All calls go through the egress policy.
3. **After every start of Tasklane** the connection is *locked*: the sync does not run, the mirror keeps being served with a
   "synced N min ago" marker, and `GET /api/v1/jira/status` and the Admin tab say "waiting for credentials". An administrator
   unlocks it with `POST /api/v1/jira/connections/{id}/unlock` `{"token"}`. Tasklane checks the token with Jira, requires it to
   belong to the connection's account, keeps it in memory and wakes the sync. Tasklane does not read `JIRA_API_TOKEN` or any
   other environment variable for this, so the token need not be in a shell profile, a unit file or a backup. In a cluster each
   node that syncs a project needs the unlock; a node that takes a project over stays locked until it has been unlocked.
   (Databases from before this release had the token stored encrypted: the upgrade erases it, and restoring an older backup
   erases it again.)
4. `POST /api/v1/jira/connections/{id}/projects` `{"jira_key","key","name","jql"}` starts mirroring a project (optionally
   narrowed by JQL) as Tasklane project `key`.
5. Optionally register a Jira admin webhook to `/api/v1/jira/webhooks/{connection id}` with the secret
   (`X-Hub-Signature`). A webhook is only a **hint** that wakes the sync; polling is the source of truth, so a missed,
   duplicated or reordered webhook cannot lose a change.

## What is copied

Issues (type, status with its category, priority, labels, assignee, reporter, parent, dates, resolution), descriptions and
comments (Atlassian Document Format is read into searchable text; the original document is kept, unsupported nodes appear as
`[unsupported content: type]`, and links other than http, https and mailto are dropped), the changelog (history rows with the
Jira author and `source = Jira`, idempotent by changelog id), links between mirrored issues, and people (matched by Jira
account id or email; otherwise an inactive placeholder with no credentials, claimed when that person gets an account).

## Synchronization

Backfill and incremental sync read `updated >= watermark - 5 minutes` in the service account's time zone, one transaction per
issue, resumable (the watermark advances page by page), idempotent per Jira issue id and `updated`. One node syncs a project at
a time. A project is synced when a webhook hinted, or when it has not been for a minute. A key change in Jira is recorded as an
alias. Every hour a reconcile compares Jira's id set with the mirror and removes what Jira deleted (and what left the mirrored
scope); it refuses to delete anything when Jira reports no issues at all. A pause drains the sync like any writer. Jira's
`429` and `Retry-After` are honoured (a long wait is recorded, not slept through); when Jira is down the mirror keeps being
served with a "synced N min ago" marker.

## Permissions

Tasklane evaluates Jira's read rules itself, from data it mirrors: members of project roles (users, and the active members of
groups) become **readers**, matched to Tasklane accounts by Jira account id or email. The permission data is re-read at least
every 5 minutes. **A project whose permission data is older than 15 minutes is invisible to everyone but administrators**
(fail closed). Actors Tasklane cannot evaluate grant nothing and are listed as a warning in the status. Issues with a Jira
**security level**, which Tasklane cannot evaluate exactly, are hidden from non-administrators (lists, search, boards,
reports, the index) and counted in a warning; comments with a Jira visibility restriction become internal comments, which
readers never see.

## Write-through

`PUT /api/v1/jira/projects/{key}/mode` `{"mode":"write_through"}` (administrators; `read_only` switches it back). The mirrored
workflow then offers a "Move" to every status the mirror has seen, and Jira decides at save time whether that move exists.

**Each person writes with their own token.** The first time someone saves in a write-through project and this node holds no
token for them, the save answers `428 token_required` and the page opens a dialog: a link to Atlassian's token page
(`https://id.atlassian.com/manage-profile/security/api-tokens`), a paste field, and the statement that the token is kept only in
memory for the session, is never written to disk or to the database, and disappears when they sign out or Tasklane restarts. The
token is sent with `POST /api/v1/jira/session-token` `{"connection","token","email"}` (email only when it differs from the
person's Tasklane email); Tasklane checks it with Jira, and it must belong to the Jira account linked to that person (by account
id, or by verified email), so nobody can save under another name. The held save is then sent again. `DELETE
/api/v1/jira/session-token` forgets it; so do logout, leaving write-through, cut-over, a refusal by Jira (401) and eight hours
without being supplied again. A write is never made with the sync's credential: no token, no write. A scoped Atlassian token is
preferred where the site allows it. The login method (LDAP, SSO) and Jira's (Google, Atlassian) do not matter: an API token belongs to
the Atlassian account, though an administrator may disable API tokens or cap their lifetime, and then people cannot save.

**What can be saved:** summary, description (only when it is plain text: paragraphs and line breaks; a description with lists,
links, code or other formatting is edited in Jira, and Tasklane says so rather than flatten it), labels, priority, assignee
(for a person with a linked Jira account), new comments (public ones; plain text) and transitions. Everything else keeps the
`read_only_mirror` answer with the Jira link: create, delete, move, clone, convert, estimate, custom fields, sprints, rank,
attachments, internal comments and editing a comment. Automation, Git rules, email and MCP write tools do not write to Jira.

**A save is synchronous and honest.** Tasklane reads the fields about to change from Jira, compares them with what the person saw
(the page sends the values its form started from as `base`; without one, the mirror's current values stand in), and if any
differ nothing is sent and the answer is `409 jira_conflict` with Jira's value, who changed it and when. The page shows it and
offers **Overwrite**, which sends the save again against Jira's value. For text, a difference opens the editor's own conflict dialog
with Jira's text (keep mine, keep theirs, merge). Jira's own refusals (a validator, a permission, a move that is not available, a
`409` for two moves at once, a deleted issue) come back as `422 jira_rejected` (or `409`) with Jira's messages by field; nothing is
retried on the person's behalf. A slow, rate-limited or unavailable Jira answers `503 jira_unavailable` with `Retry-After`; the
draft is kept and the mirror is unchanged. When Jira accepts, Tasklane reads the issue back and applies it exactly as the sync does,
so the mirror equals Jira at that moment, and the later poll or webhook for the same change changes nothing (the sync is
idempotent per Jira issue id, `updated` and changelog id). Every attempt is in the audit log (`jira.write`: who, issue, field
names, accepted, conflict, rejected or failed, and Jira's status), never a token.

**Known limits.** Jira Cloud's edit call is not known to refuse a stale write by itself (its web editor does; the public API is
unproven), which is why Tasklane compares first. A collision inside the few hundred milliseconds between that check and the send
is last-writer-wins, as in Jira. Nothing has been run against a real Jira site yet (see *Not built yet*).

## Operating it

`GET /api/v1/jira/status` shows, per mirrored project: mode, lag, watermark, last error, permission freshness, issue count, warnings,
whether its connection is `locked` ("waiting for credentials") and how many people hold a personal token (a count, not names).
`POST /api/v1/jira/projects/{key}/sync` and `/reconcile` run a pass by hand. `POST /api/v1/jira/projects/{key}/cutover`
turns the project into a native one (the sync stops, the project becomes writable, keys and history stay, and Jira is no longer
updated by Tasklane).

## Not built yet

Attachment files (metadata and lazy fetch), worklogs, versions and components, Jira boards, sprints and rank, custom field
values, OAuth 2.0 (3LO) credentials, the "verify with Jira" permission mode, permission schemes and custom-field grants (these
fail closed through the security-level rule and the role-based membership), issue-level erasure reports and account closures
from Jira, Atlassian sign-in, admin screens (the API is complete), the `tasklane jira-unlock` command (the unlock is an API call
for now), dropping the now-empty `jira_connection.token_enc` column (a contract migration for a later release), and **runs against
a real Jira site**: the Jira double was written from Jira's documentation, and whether the public edit API refuses stale writes,
whether API tokens work for Google-SSO accounts and whether scoped tokens can do these edits are all unverified.
