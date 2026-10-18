# Mirroring a Jira Cloud project (read-only, one way)

Tasklane can be a fast read-only view of a Jira Cloud project. Data flows **from Jira into PostgreSQL only**; every screen reads
PostgreSQL. People edit in Jira (every mirrored issue shows an **Edit in Jira** link and how fresh the copy is). **Nothing
edited in Tasklane reaches Jira**: every write path (UI, REST, MCP, automation, import) refuses a mirrored project with the
same typed error, `read_only_mirror`, which carries the Jira site.

## Connect

1. Create a service account in Jira with read access and an API token.
2. `POST /api/v1/jira/connections` `{"name","site_url","email","token","allow_private"}` (administrators). The credentials are
   checked at once; the token is stored encrypted and never shown again (a fingerprint is); the response gives the **webhook
   secret once**. `site_url` must be `https` (plain `http` only with `allow_private`, for a private host). All calls go
   through the egress policy.
3. `POST /api/v1/jira/connections/{id}/projects` `{"jira_key","key","name","jql"}` starts mirroring a project (optionally
   narrowed by JQL) as Tasklane project `key`.
4. Optionally register a Jira admin webhook to `/api/v1/jira/webhooks/{connection id}` with the secret
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

## Operating it

`GET /api/v1/jira/status` shows, per mirrored project: lag, watermark, last error, permission freshness, issue count and
warnings. `POST /api/v1/jira/projects/{key}/sync` and `/reconcile` run a pass by hand. `POST /api/v1/jira/projects/{key}/cutover`
turns the project into a native one (the sync stops, the project becomes writable, keys and history stay, and Jira is no longer
updated by Tasklane).

## Not built yet

Attachment files (metadata and lazy fetch), worklogs, versions and components, Jira boards, sprints and rank, custom field
values, OAuth 2.0 (3LO) credentials, the "verify with Jira" permission mode, permission schemes and custom-field grants (these
fail closed through the security-level rule and the role-based membership), issue-level erasure reports and account closures
from Jira, Atlassian sign-in, admin screens (the API is complete), and runs against a real Jira site.
