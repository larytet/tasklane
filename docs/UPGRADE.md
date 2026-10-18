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
