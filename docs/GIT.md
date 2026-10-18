# Git integration (GitHub and GitLab), read-only

Tasklane shows branches, commits, pull or merge requests, builds and deployments on the ticket, linked by issue key. It reads
only titles, subject lines, authors, states, SHAs, URLs and results; no source code or diffs are stored, and a webhook
payload is dropped as soon as it has been applied.

## Connect

An administrator creates a connection (`POST /api/v1/git/connections`): provider (`github` or `gitlab`), base URL (GitHub
defaults to `https://api.github.com`; GitLab needs the instance URL), an optional read-only token for backfill, and
optionally `allowed_cidrs` (the source addresses webhooks may come from). The response contains the **webhook secret, once**;
afterwards only a fingerprint is shown. Tokens and secrets are stored encrypted (AES-256-GCM); set `secret_key` so a
database dump alone cannot decrypt them.

Then map repositories (`POST /api/v1/git/connections/{id}/repos`, `full_name` like `acme/shop`, `projects` such as `["ABC"]`).
A repository links to the issue keys of its mapped projects only, so a string such as `UTF-8` never links.

Point the provider's webhook at `https://<your tasklane>/api/v1/git/webhooks/<connection id>`:

| | GitHub | GitLab |
|---|---|---|
| Secret | the secret from the response (HMAC SHA-256, `X-Hub-Signature-256`) | the same value as the secret token (`X-Gitlab-Token`) |
| Events | pull requests, pull request reviews, pushes, check runs, statuses, deployment statuses | merge request, push, pipeline and deployment events |

Deliveries are verified, stored by delivery id (a replay is ignored), acknowledged at once and applied in the background.
Out-of-order deliveries converge: the provider's own timestamp decides, never arrival order.

## Backfill and reconcile

With a token, `POST /api/v1/git/repos/{id}/sync` reads the last `backfill_days` (default 90) of pull requests, branches,
commits and builds; later runs go back to just before the previous one, send the stored `ETag`, and a `304` costs one request.
A reconcile of one repository runs every five minutes in the background. Provider rate limits are honoured: a short wait is
slept through, a long one (over 30 seconds) is recorded on the repository and retried later. Only `GET` requests are sent.

## Outbound policy

All provider calls go through the egress policy: loopback, link-local and private addresses are refused unless the connection
sets `allow_private` (cloud metadata addresses are always refused), a name is resolved once and the checked address is the one
connected to, redirects are limited and re-checked, and credentials are never sent to another host. Plain `http` base URLs
are accepted only together with `allow_private`.

## What the ticket shows

A collapsed Development section with a line such as "1 PR open, 2 commits, build passing, deployed to staging", expanding to
pull requests (state, review), branches, commits, builds per head commit and the latest deployment per environment. It is
read from Tasklane's own store; the provider is never called when a ticket opens. Links are made from the branch name, commit
messages, and the pull request title and description, in any letter case, through key aliases after a move; a link can also be
added by pasting the item's URL, and a manual link survives later edits.

## Not built yet

Write actions (create branch, back-link comments), smart commits, workflow automation from code events and the workflow
conditions "has a linked pull request" and "build is passing" (all M4), TQL fields for pull requests and builds, the admin
screens (the API is complete), GitHub App installation tokens and OAuth, verified provider-identity links, and
release and version attachment.
