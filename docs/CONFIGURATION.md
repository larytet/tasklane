# Tasklane configuration reference

All settings are environment variables. Unset variables use the default shown. `tasklane doctor` checks them.

| Variable | Default | Meaning |
|---|---|---|
| `TASKLANE_LISTEN` | `:8080` | Address the HTTP server listens on |
| `TASKLANE_DATABASE_URL` | `postgres://tl:tl@localhost:55432/tasklane` | PostgreSQL connection string. The role needs to create the `pg_trgm` and `unaccent` extensions (both are trusted extensions on PostgreSQL 13 and later, so the database owner can) |
| `TASKLANE_BASE_URL` | `http://localhost:8080` | Public URL. An `https://` value turns on the `Secure` flag of the session cookie |
| `TASKLANE_BLOB_DIR` | `./data/blobs` | Directory of the local filesystem blob store (attachments). Must be writable |
| `TASKLANE_MAX_UPLOAD_MB` | `100` | Largest accepted attachment |
| `TASKLANE_ADMIN_EMAIL`, `TASKLANE_ADMIN_PASSWORD` | unset | On start, creates this administrator if it does not exist. Remove after first start |
| `TASKLANE_SMTP_HOST`, `TASKLANE_SMTP_PORT` | unset, `25` | Outbound mail server. Unset means mail is off |
| `TASKLANE_SMTP_FROM`, `TASKLANE_SMTP_USER`, `TASKLANE_SMTP_PASS` | `tasklane@localhost`, unset, unset | Sender address and credentials |

## Commands

| Command | Purpose |
|---|---|
| `tasklane serve` | Run the server. A brand-new database is migrated automatically; an existing one is never migrated implicitly |
| `tasklane migrate` | Apply pending schema migrations (safe to run from several nodes at once) |
| `tasklane doctor` | Check database, schema version, administrator, blob directory and mail; exit status 1 on a failure |
| `tasklane reindex` | Rebuild the search index from PostgreSQL in one pass (unchanged documents are not rewritten) |
| `tasklane create-admin EMAIL NAME` | Create an administrator; the password comes from `TASKLANE_ADMIN_PASSWORD` |
| `tasklane version` | Print the version |

## Quick start

    export TASKLANE_DB_PASSWORD=change-me TASKLANE_ADMIN_EMAIL=you@example.com TASKLANE_ADMIN_PASSWORD=change-me-too
    docker compose -f deploy/compose/docker-compose.yml up -d --build
    docker compose -f deploy/compose/docker-compose.yml exec tasklane tasklane doctor
