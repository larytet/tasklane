# tasklane

An open source, self-hosted issue and project tracker built for feature parity with Jira, written in Go.

Tasklane aims to give teams the parts of Jira they actually use - boards, backlogs, weekly sprints, rich ticket views,
custom fields, workflows - without the weight, and with the enterprise basics (SSO, audit, permissions) built in.

## Quick start

Tasklane runs as two containers, Tasklane and PostgreSQL, with Docker Compose. You need Docker with the Compose plugin
(`docker compose version`), `git`, `curl`, `jq` and `openssl`. If your user cannot run Docker, use
`sudo --preserve-env docker …`, so the variables you export still reach Compose.

### 1. Clone

```bash
git clone https://github.com/larytet/tasklane.git
cd tasklane/deploy/compose
```

### 2. Set the variables

Compose reads `deploy/compose/.env`. Generate the secrets once and keep the file private:

```bash
umask 077
cat > .env <<END
TASKLANE_DB_PASSWORD=$(openssl rand -hex 16)
TASKLANE_ADMIN_EMAIL=you@example.com
TASKLANE_ADMIN_PASSWORD=$(openssl rand -base64 18 | tr -d '/+=')
TASKLANE_BASE_URL=http://$(hostname -I | awk '{print $1}'):8080
END
grep ADMIN .env    # the administrator you will sign in as
```

| Variable | Meaning |
|---|---|
| `TASKLANE_DB_PASSWORD` | PostgreSQL password (required) |
| `TASKLANE_ADMIN_EMAIL`, `TASKLANE_ADMIN_PASSWORD` | the first administrator. It is created at the first start only, so changing these later does nothing; change the password on the Account page instead. |
| `TASKLANE_BASE_URL` | the address people use; sign-in redirects and links in mail point here |
| `TASKLANE_SSO_ALLOW_PRIVATE` | `true` lets Tasklane reach identity providers on private addresses. LDAP domain controllers almost always are. Set it in `.env` or export it. |

`docker compose run --rm tasklane config docs` lists every other setting.

### 3. Build and start

```bash
export TASKLANE_SSO_ALLOW_PRIVATE=true      # needed for LDAP (step 5)
docker compose up -d --build
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/readyz   # 200 when ready
```

Open `TASKLANE_BASE_URL` in a browser and sign in with the administrator from `.env`.

The first start creates the database schema. A later release that changes the schema refuses to start until you migrate;
the log says `the database schema (version N) is behind this release`. Migrating is always an explicit step:

```bash
git pull
docker compose build tasklane
docker compose run --rm tasklane migrate
docker compose up -d
```

### 4. Collect the directory settings

LDAP sign-in lets people use their Active Directory (or OpenLDAP) name and password on the ordinary sign-in form. Ask
your directory administrator for these values. Export them in the shell you work in, for example in `~/.bashrc`:

```bash
export LDAP_SERVERS='dc1.corp.example.com'                     # one name, or several separated by commas
export LDAP_BIND_DN='CN=svc-tasklane,OU=Service Users,DC=corp,DC=example,DC=com'   # the account that searches
export LDAP_BIND_PASSWORD='...'                                # single quotes keep $ ! ` \ intact
export LDAP_USER_SEARCH='OU=Users,DC=corp,DC=example,DC=com'   # where people are searched for
```

Check the search account before involving Tasklane. This needs `ldap-utils` (Debian, Ubuntu) or `openldap-clients`.
`LDAPTLS_REQCERT=never` is only needed when you do not have the directory's CA certificate.

```bash
printf %s "$LDAP_BIND_PASSWORD" > /tmp/.ldappw && chmod 600 /tmp/.ldappw
LDAPTLS_REQCERT=never ldapwhoami -x -H "ldaps://${LDAP_SERVERS%%,*}" -D "$LDAP_BIND_DN" -y /tmp/.ldappw
rm -f /tmp/.ldappw
```

- `u:CORP\svc-tasklane` means the account works.
- `Invalid credentials (49) ... data 52e` means the password is wrong for that DN.

### 5. Create the LDAP provider

In the browser: **Admin → Identity → Add a provider → Protocol: LDAP (Active Directory)**.

1. Enter a name (`corp-ad`) and a label.
2. **Server addresses:** `ldaps://dc1.corp.example.com`, or `ldap://…` with *StartTLS* ticked. Several addresses are tried in
   order.
3. **Certificate:** paste the directory's CA certificate, or tick *Do not verify the server certificate* if you do not have it.
4. **Search account and base:** fill in the search account DN, its password and the base DN.
5. **User filter:** leave it empty for the Active Directory default. The default matches the name typed against
   `sAMAccountName`, `userPrincipalName`, `uid` or `mail`. To let only one group sign in, see
   [docs/IDENTITY.md](docs/IDENTITY.md).
6. **Optional:** limit sign-in to some email domains, and choose whether accounts are created at first sign-in.
7. **Test:** save, open the provider again (*Edit*) and use **Try a sign-in** with your own name and password. It shows the
   entry found, the address, and whether the account exists or would be created. It signs nobody in.

The same steps from the shell, run in `deploy/compose` so `.env` supplies the administrator:

```bash
set -a; . ./.env; set +a
T=http://localhost:8080; J=$(mktemp)
H=(-H 'Content-Type: application/json' -H 'X-Requested-With: tasklane')
jq -n --arg e "$TASKLANE_ADMIN_EMAIL" --arg p "$TASKLANE_ADMIN_PASSWORD" '{email:$e,password:$p}' |
  curl -s -c "$J" "${H[@]}" -d @- -o /dev/null -w 'sign-in %{http_code}\n' $T/api/v1/login
URLS=$(echo "$LDAP_SERVERS" | tr ',' '\n' | sed 's/^ *//; s|^|ldaps://|' | paste -sd' ')
jq -n --arg urls "$URLS" --arg dn "$LDAP_BIND_DN" --arg pw "$LDAP_BIND_PASSWORD" --arg base "$LDAP_USER_SEARCH" \
  '{protocol:"ldap", name:"corp-ad", label:"Corporate directory", issuer:$urls, ldap_skip_verify:true,
    ldap_bind_dn:$dn, client_secret:$pw, ldap_base_dn:$base, jit:true, enabled:true}' |
  curl -s -b "$J" "${H[@]}" -d @- $T/api/v1/admin/sso/providers | tee /tmp/prov.json | jq '{id, issuer, secret_set, error}'
ID=$(jq .id /tmp/prov.json); rm -f /tmp/prov.json
# try your own account: nobody is signed in
read -rp 'name: ' U; read -rsp 'password: ' P; echo
jq -n --arg l "$U" --arg p "$P" '{login:$l,password:$p}' |
  curl -s -b "$J" "${H[@]}" -d @- $T/api/v1/admin/sso/providers/$ID/check | jq
rm -f "$J"
```

### 6. Sign in with the directory

Sign out, then sign in with `jana.k`, `CORP\jana.k` or `jana.k@corp.example.com` and the directory password.

- The first sign-in creates the account. An existing account with the same address is used instead.
- A second factor is still asked for when the account has one.
- Five failures lock the name for fifteen minutes before anything reaches the directory.
- The local administrator keeps working beside the directory.

When sign-in fails, *Try a sign-in* says why, and the server log names the provider and the reason
(`docker compose logs tasklane`). Details are in [docs/IDENTITY.md](docs/IDENTITY.md).

**Optional: mirror a Jira Cloud project.** A read-only copy gives a fast UI over Jira data; see [docs/JIRA.md](docs/JIRA.md).

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
- **Search** - plain search (`Alan AND (Arkady OR Dana) -draft`, also matching people) or a query language (TQL, JQL-compatible subset), saved filters, list and table views, bulk edit.
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

## Related projects

- [Windshift](https://github.com/Windshiftapp/core) - an open source, self-hosted work management platform written in Go
  (AGPL-3.0), covering boards, sprints, workflows, a customer portal, time tracking and test management.

## License

Apache-2.0 (proposed, to be confirmed before the first public release; see SPEC section 17.9).
