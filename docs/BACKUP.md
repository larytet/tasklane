# Backup and restore

```
tasklane backup DIR          # a consistent backup of the database and the attachments, into a new directory
tasklane verify-backup DIR   # check every file against the manifest (needs no database)
tasklane restore DIR         # into an EMPTY database (it is migrated first)
```

Settings (database, attachment store, `secret_key`) come from the environment and the config file, as for the server.

## What a backup is

A directory:

- `manifest.json`: the format and its version, the Tasklane release and **schema version** it was taken at, the **marker** (the time, the database log position and the position of the event log at the instant of the snapshot, and the mode, `snapshot`), every table with its columns, row count, size and SHA-256, the sequences, the number of attachments, and a value sealed with the instance secret key. It is written last: a directory without it is an interrupted backup and is refused.
- `data/<table>.copy`: each table in PostgreSQL's `COPY` text format.
- `blobs/ab/cd/<sha256>`: the attachments the snapshot refers to, each named by its hash.

## Consistency, without stopping anything

- **One instant.** The database is read in a single repeatable-read, read-only transaction: every table is as it was at the marker, writers are not held up, and nothing is paused. (Tested with eight writers saving during the backup: what comes back is a prefix of each writer's work, never a state that never existed.)
- **Attachments after the snapshot.** Files are copied after the snapshot is fixed, and the blob garbage collector is held off by a database advisory lock while they are, so a file the snapshot refers to cannot be deleted before it is copied. Extra files (added since) are harmless. A restored database therefore never refers to a file the backup lacks. If an attachment was already missing or damaged in the live store, the backup says so (`blobs_missing`, exit status 1) and `tasklane fsck` shows which.

## What is not in a backup

Sessions and sign-in state (everyone signs in again), cluster nodes, pauses, upload sessions in progress, one-time codes. Search indexes are not backed up: they are derived, and `tasklane reindex` rebuilds them.

## Keep it safe

A backup holds password hashes, the credentials Tasklane stores sealed with the instance secret key (Git, Jira, mailbox and single sign-on secrets), the audit log and the attachments. **Attachments are written in clear even when the live store encrypts them at rest** (the backup says so). Keep a backup as carefully as the database, and encrypt the place you store it. **Restoring needs the same `secret_key`**: with a different one the restore succeeds but says that the stored credentials cannot be read until the original key is set.

## Restoring

- **Into an empty database.** A restore refuses a database that already has people or issues ("restore into a new, empty database and switch to it"). To replace a running instance, restore into a new database, check it, stop the servers and point them at it. Nothing is ever restored over live data.
- **Order.** Attachments come in first, the rows after, in one transaction: either all of it or none. The audit log and everything else keep their identifiers, hashes and sequences exactly; the audit chain is verified afterwards.
- **Older backups, newer releases.** A backup from an earlier release restores into a later schema: columns and tables the backup does not know take their defaults. A backup from a **newer** schema is refused ("restore with the release that made it, or a newer one"), and so is a backup in a newer format version.
- **After it:** the command checks the row counts against the manifest, the event log's end against the marker, the audit chain and the attachments, and lists what to do next (people sign in again; reindex if you use OpenSearch). Exit status 3 means the audit chain or the attachments did not verify.

## Compatibility

`BackupVersion` is raised when a later release could not be read by an earlier reader. The tests keep a backup of every released version (`internal/domain/testdata/backups/vN`) and restore it into the current schema; raising the version without adding a fixture fails them (see UPGRADE.md, Data formats).

## Restoring over a running instance

`tasklane restore DIR` goes into an empty database. To replace the data of an instance that is running, say so:

```
tasklane restore --replace DIR
```

What it does, in order: verifies the backup (a damaged one is refused before anything else); takes an instance-wide pause, so writers wait
a moment and then get `503` with `Retry-After` while reads, drafts and presence carry on (the Admin page shows who holds the pause); writes a
backup of the current state to `DIR.before-restore-<time>` (skip with `--no-safety-backup`; with it, a restore is itself undoable by restoring
that directory the same way); loads the backup in one transaction, so a failure leaves the data exactly as it was; and ends the pause whatever
happened. Everyone is signed out (sessions are not part of a backup). The audit log records `restore.completed` (and `restore.aborted` with the
reason when it fails); the restored log carries on from the backup's last entry.

Another job's pause (an import into a project, a migration) blocks it: the restore refuses rather than overlap. Servers that were running keep
settings and caches from the data that was replaced: restart them afterwards. The command does not migrate the database (a running instance
already has its schema); a backup from a newer release than the running schema is refused.
