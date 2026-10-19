# Upgrading

## The rules

Schema changes are an explicit step and never a side effect of starting a node. A release has two kinds of migration:

- **expand** migrations only add things (tables, nullable columns, indexes built concurrently). The previous release can keep running on the new schema. `tasklane migrate` applies these.
- **contract** migrations remove what the previous release still uses. They are *held*: `tasklane migrate` stops before the first one, and only `tasklane migrate finalize` applies them. Run it when no node of the previous release is left and you no longer want to be able to go back.

`tasklane migrate plan` lists what would be applied, marking held steps.

## What a node does at start

`tasklane serve` and `tasklane doctor` compare the database with the binary and refuse in these cases, with the reason and the way out:

| Database | Result |
|---|---|
| empty | initialized (the only time the server migrates by itself) |
| behind the binary | refused: "run `tasklane migrate` first" |
| older than the oldest this release upgrades from | refused, with the stepping path: upgrade to the named release first, run it once, then this one (supported: an offline upgrade from the previous two releases) |
| ahead of the binary, and every newer migration is expand-only | runs, with a warning: this is the window for a rolling upgrade and for a rollback to the previous release |
| ahead of the binary and a newer **contract** migration was applied | refused: the schema was finalized; run the newer release or restore a backup taken before `migrate finalize` |
| ahead of the binary and a newer migration has no recorded phase | refused: it cannot be classified |

The phase of each applied migration is recorded in `schema_migration.phase`.

## A rolling upgrade, in order

1. Take a backup.
2. Run the new release's `tasklane migrate` (expand only). Nodes of the previous release keep running (`doctor` on them shows the "beside a newer schema" warning).
3. Replace the nodes one by one with the new release.
4. When every node is on the new release and you are sure, run `tasklane migrate finalize`. After that the previous release will not start.

To go back inside step 2 and 3: start the previous release's binary again; nothing needs restoring. After step 4 going back means restoring the backup.

## Renamed and removed settings

A setting that was renamed keeps working for one minor release: its value is used under the new name and a warning on standard error says which name to use. A removed setting is ignored with a warning that says what replaces it. If both the old and the new name are set in one place, the new name wins. After that release the old name is an unknown setting and an error. (No setting has been renamed yet; the table is `config.Renamed`.)

## If a migration fails

Each migration runs in its own transaction with a one second lock timeout, so a migration that fails, or cannot get its lock, leaves the schema exactly as it was and is not recorded. Fix the cause and run `tasklane migrate` again. The migrations that completed before it stay applied. A migration that is marked not to run in a transaction (index builds) is a single statement for the same reason.

## What is not covered yet

The rolling upgrade under load, mixed-version clusters and feature gates (`cluster_min_version`), the rich-text and client skew windows, data-format compatibility with earlier releases and the single-node pause are not built; there has been no release to upgrade from, so the floor is empty. They are listed in STATUS.md.

## The cluster version gate (rolling upgrades)

Every node writes the version it runs to the database every 10 seconds (`GET /api/v1/admin/cluster` lists them). A node that stops reporting for 30 seconds, or leaves cleanly, stops counting. A new behaviour that older nodes cannot handle is a **gate**:

- it stays **off** until every live node runs at least the version it needs, then turns **on** by itself;
- a **risky** gate does not turn on by itself: it shows as `pending` once every node qualifies, and an administrator confirms it (`POST /api/v1/admin/cluster/gates/{name}/confirm`, audited). If an older node joins while it waits, it goes back to off;
- once on it **stays on**. A node older than an active gate refuses to start and names the gate and the version it needs: rolling back past it is not an option, and the way back is a restore from a backup taken before it was activated. During a rollout keep every node on the new version before the gate turns on, and do not start an old node after it has.
- with no node registered nothing turns on. A development build (`-dev`) counts as the release it leads up to.

This release has one gate, `mail_retention` (needs 0.1.0): the purge of old inbound mail leaves hashed Message-IDs that an older node would not recognise, so the purge does not run until every node knows them. Gates are for the release after this one to use: the mechanism is there so the first real mixed-version rollout has it.

## What people see during an upgrade or maintenance

- **A pause** (a restore, an import, a single-node upgrade that holds writes) shows a banner, **Maintenance in progress**, on every open page. Reading and presence continue. A write is held for up to two seconds and then refused with `503`, a `Retry-After` header and the code `paused`; a browser keeps what the person typed as a draft, shows **Paused for maintenance, draft kept** in the editor, and saves again by itself when the pause ends. A save carries its `save_id`, so the retry can never apply twice. `GET /api/v1/system/status` tells any signed-in client whether a pause is under way, for which scope and until when. A pause lasts at most its deadline (default 10 minutes) and a crashed job's pause expires.
- **Client versions.** The page sends `X-Tasklane-Client`; every API answer carries `X-Tasklane-Server` and `X-Tasklane-Client-Min`. The server serves the previous client as well as the current one, so a tab left open across an upgrade keeps working. A page older than `min_client_version` gets `426` with the code `client_too_old`, shows **A newer version of Tasklane is available** with a Reload button, and keeps its draft (restored after the reload). Requests with no such header (API tokens, MCP, scripts) are never refused on this ground, and the page itself is always served, so a stale tab can always be reloaded.
- **Reconnecting.** A realtime connection that drops tries again with a growing, randomised delay, so a restart does not bring every tab back in the same second.

## Data formats

Exchange bundles (the JSON and CSV zips that export writes and import reads) carry a format version in their manifest (`BundleVersion`, now 1) and are checksummed file by file.

- **Reading:** a release reads every earlier format version it supports and refuses a newer one whole, before changing anything, with the words "newer than this server understands (…); upgrade Tasklane". A damaged or edited file is refused (the manifest's checksums do not match).
- **Adding is not a new version.** A later release may add a field to a record, a column to a CSV, a file, or a key in the manifest without raising the version; earlier readers ignore what they do not know. A change that an earlier reader could misread (a field changing meaning, a file being renamed) raises `BundleVersion`.
- **The guard:** `internal/domain/testdata/bundles/vN/` holds a JSON and a CSV bundle written by version N together with what they must import as. Every release must still import all of them, and **raising `BundleVersion` without adding its fixture fails the tests**. Make a version's fixture with `TASKLANE_UPDATE_GOLDEN=1 go test ./internal/domain -run TestUPG09_WriteGolden` and commit it; the files of a released version are evidence and are never rewritten.
- **Backups** (`tasklane backup`, see BACKUP.md) have their own `BackupVersion` and the same guard, with fixtures under `internal/domain/testdata/backups/vN`.
- **Support window:** the previous two releases' formats must stay readable (SPEC 17.8). With one format version so far, that is version 1.

## The pause barrier across nodes

A pause (an import, a restore) used to wait only for the writes of the node that asked for it. Now, when every live node runs a release that
knows how, each node that has seen the pause says so once it has no write of its own left in the scope (`pause_ack`), and the pause is not
reported as `paused` until every other live node has said so. A node that stops sending its heartbeat for half a minute is no longer waited for. A
node that never answers makes the pause abort at its drain deadline (the scope runs again, nothing changes) with an audit entry
(`pause.aborted`) naming the nodes it was waiting for.

During a rolling upgrade, while any live node is older than the release that acknowledges, this behaviour is a gate (`pause_barrier`) that stays
off: the pause then gives the other nodes the fixed moment it always gave them to see the lease. The gate turns on by itself once every live node
is new enough (see the cluster version gate above); `GET /api/v1/admin/cluster` shows it.

## Rolling upgrades, tested

`make upgrade` (`scripts/upgrade.sh`, Docker required) proves the procedure on the real binaries: it builds the previous release from a throwaway checkout
(`scripts/upgrade-from`, or `UPGRADE_FROM=<commit or tag>`) and the current tree, starts two old nodes behind a small load balancer that routes only to nodes
passing `/readyz`, runs twelve simulated people (saves with idempotent save ids, reads, searches, comments, and sockets that reconnect with jitter), and
then does what an operator does: `tasklane migrate` with the new release while the old nodes serve (the expand step), replaces the first node (the new
one joins, the old one is stopped and drains), checks the mixed cluster (two versions live, the behaviour that needs the new release still off, a pause
still works), replaces the second, and checks that the gate turned on by itself and that a pause now waits for the acknowledgements of both nodes.
It fails if fewer than 99.5% of requests succeed after one retry, if the 95th percentile latency passes one second, if any change a person was told was
saved is missing at the end, or if a socket stays down for more than 15 seconds.
