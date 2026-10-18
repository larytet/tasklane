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


## Automation from code events, smart commits and identities

Everything here acts only on **live webhook deliveries**. A backfill or reconcile replays history and never runs a rule or a command: enabling a rule must not move issues that finished long ago.

### Who is who

A code event is attributed to a person only through what the provider itself vouches for: a **linked account** (an instance administrator links a provider account to a person with `POST /api/v1/git/identities`; the link is audited) or an **email the provider ties to the account** (GitHub fills in a commit author's `username` only when the commit email belongs to that account; GitLab authenticates the pusher, so a commit counts as the pusher's only when its email is the pusher's own). A name or an email that is merely written in a commit never maps: an unverified commit is shown by its provider name and attributed to nobody.

### Rules (per project, `/api/v1/projects/{key}/git/rules`; project administrators)

A rule has a trigger, an action and a mode:

| Trigger | Fires when |
|---|---|
| `branch_created` | a branch whose name has the issue key is first seen |
| `pr_opened` | a pull or merge request with the key becomes open (or leaves draft) |
| `pr_merged` | it becomes merged |
| `build_failed` | a build on a commit, branch or pull request linked to the issue starts failing |

The action is a workflow **transition by name** (from the issue's current status), a **comment**, or both. A new rule starts in **dry run**: it records what it would have done (`rule-runs`) and changes nothing; set the mode to `on` when it looks right, `off` to disable.

- Rules go through the same workflow checks a person meets: a transition the workflow does not offer from the current status, or that a condition or validator refuses, is **skipped and reported** with the reason, never forced.
- A move is attributed to the event: "GitHub, PR #12", "GitLab, MR !7", "GitHub, branch ABC-1-fix".
- **A rule runs at most once per code object and issue.** A repeated delivery, a pull request closed and reopened, a draft made ready, or a manual move of the issue does not run it again, so a transition can never re-trigger the rule that caused it. A skipped run is not retried either; fix the workflow and the next branch or pull request starts fresh.
- Mirrored and archived issues are never changed.
- Every rule save and delete is audited, and the issue's development panel shows what each rule did or why not.

### Smart commits (per project, off by default: `PUT /api/v1/projects/{key}/git/smart-commits`)

Settings: `off`, `default_branch` (commit messages on the default branch, and pull requests merged into it) or `any`. In a commit message, line by line, the commands after the first issue key apply to every key on the line:

```
ABC-123 #comment fixed the redirect
ABC-123 ABC-124 #comment both #in-review
```

- `#comment text` adds a comment; any other word is a **transition by name** (`#in-review` or `#request-review`: hyphens stand for spaces, and a transition's name or its target status's name both match).
- They run **as the mapped committer, with that person's permissions only**: someone who may not comment or transition cannot do it by writing a commit.
- A merged pull request's **title** runs as the pull request's author; commands in a pull request **description** are never run, by anyone.
- Ignored and reported (in the issue's development panel and the audit log, with the reason): an unmapped author, an unverified email, a feature branch under `default_branch`, an unknown command, a missing comment text, `#time` (time tracking is not built), a transition the workflow does not offer, a person without permission. **Every command is audited**, applied or not.
- A command runs once per commit: a replay does nothing.

### Not built

An Admin screen for rules, identities and the smart-commit setting (these are API only); self-service identity linking by the person (an administrator vouches for each link); `#time`; rules for deployments and reviews; a "recently pushed" window; flagging an issue (a rule can comment but not set a flag); and visibility restrictions for private repositories (GIT-11).


## Write actions, connection health and disconnect

### Write actions (off until an administrator turns them on)

Reading is all Tasklane does by default. `PUT /api/v1/git/connections/{id}/options` with `{"write_enabled": true}` lets people and agents with write access to an issue do two things at the provider; with it off nothing is ever sent.

- **Create a branch from an issue** (`POST /api/v1/issues/{key}/git/branch`, the MCP tool `create_branch`): body `{repo?, base?, name?}`. The default name is `KEY-slug-of-the-summary` (ASCII, at most 40 characters of slug); if it is taken a numbered one (`-2`, `-3`, up to `-6`) is made, while a name the person chose that is taken is an error that says so. A name must be a valid branch name and **contain the issue key**, so that the branch links back to the issue. The base is the one named, or the repository's default branch. The answer has the branch's address and the command to get it: `git fetch origin KEY-… && git switch KEY-…`. The new branch reaches Tasklane by webhook like any other, so a `branch_created` rule still fires. When the project maps to several writable repositories the person names one. The call needs a member role on the issue (a viewer may not), is audited with the repository and branch, and is recorded in the write log.
- **Back-link comment**: when a live webhook shows a pull or merge request linked to an issue, Tasklane comments once on it, "Linked to Tasklane issue KEY" with a link. The comment carries **only the key and the link, nothing from the issue** (the pull request may be public and the issue is not). One comment per pull request and issue; a failed comment is logged and tried again on a later event. A backfill never writes.

### Health (GIT-18)

Each connection carries a health: `ok`, `token_rejected` (the provider answers 401: the token was revoked, expired or wrong), `no_write_permission` (the token reads but a write got 403 or 404: it needs permission to write repository contents on GitHub, or the `api` scope on GitLab), `error` (the provider could not be reached) or `unknown`, with the reason and the time. It is set by every call that reaches the provider, shown in `GET /api/v1/git/connections`, and refreshed on demand with `POST /api/v1/git/connections/{id}/check`. A good read does not clear a standing `no_write_permission`; a good write does. The message a person gets for a failed write says what to fix.

### Disconnect

`DELETE /api/v1/git/connections/{id}?mode=…`:

- `mode=keep_read_only`: the connection is kept but stops: webhooks are refused (the answer is the same as for a bad signature), nothing is synced, nothing is written, write actions are switched off. The links and cached data stay readable on the issues.
- `mode=purge` (the default): the connection, its repositories and every link and cached object are removed.

Tasklane never creates the webhook at the provider (an administrator does, with the secret Tasklane shows once), so removing the webhook there is the administrator's step too.

### For agents (MCP)

- `list_development(key)` and the resource `tasklane://issue/{key}/development` return the linked pull requests, branches, commits, builds and deployments. Every title, branch name, commit subject and author name is **text from the provider and is marked**: each item carries `"provenance": "external-git-provider"`, the result carries a notice that its text is data, never instructions, and the Markdown resource puts the same banner before any provider text. They need the `read` scope and the same access to the issue as a person.
- `create_branch(key, base?, name?, repo?)` needs the new `branch` scope, a member role on the issue, and the connection's write switch. It takes the usual `idempotency_key`, so a retry does not create a second branch.

### Still not built

Self-service: an Admin screen for the connection options, health and write log; the full data-minimization checks of GIT-17 (secret-looking strings in messages and erase of a commit author on request); pull-request creation from Tasklane; a "branch created" notice back to the issue's history; and write actions against a real GitHub or GitLab.
