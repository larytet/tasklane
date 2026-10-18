# Tasklane configuration reference

All settings are environment variables. Unset variables use the default shown. `tasklane doctor` checks them.

Precedence, lowest to highest: built-in default, config file (`--config PATH`, `TASKLANE_CONFIG`, or `./tasklane.conf` when present; `key = value` lines), environment variable, command-line flag. Secrets can also be read from a file with `TASKLANE_<KEY>_FILE` (for Docker and Kubernetes secrets). An unknown key, flag or variable is an error with a suggestion, and every problem is reported at once. `tasklane config print` shows the effective values and where each came from; secrets are always masked.

<!-- config-table:begin -->
| Setting | Environment variable | Flag | Default | Meaning |
|---|---|---|---|---|
| `listen` | `TASKLANE_LISTEN` | `--listen` | `:8080` | Address the HTTP server listens on |
| `database_url` | `TASKLANE_DATABASE_URL`, `TASKLANE_DATABASE_URL_FILE` | `--database-url` | (secret) | PostgreSQL connection string. The role must be able to create the `pg_trgm` and `unaccent` extensions (trusted extensions on PostgreSQL 13 and later, so the database owner can) |
| `base_url` | `TASKLANE_BASE_URL` | `--base-url` | `http://localhost:8080` | Public URL. An `https://` value turns on the `Secure` flag of the session cookie and is used in links in mail |
| `blob_dir` | `TASKLANE_BLOB_DIR` | `--blob-dir` | `./data/blobs` | Directory of the local filesystem blob store (attachments). Must be writable |
| `max_upload_mb` | `TASKLANE_MAX_UPLOAD_MB` | `--max-upload-mb` | `100` | Largest accepted attachment, in MiB |
| `session_ttl` | `TASKLANE_SESSION_TTL` | `--session-ttl` | `336h` | How long a sign-in lasts |
| `presence_ttl` | `TASKLANE_PRESENCE_TTL` | `--presence-ttl` | `30s` | How long an editing indicator lives without a heartbeat |
| `admin_email` | `TASKLANE_ADMIN_EMAIL` | `--admin-email` | unset | On start, creates this administrator if it does not exist. Remove after the first start |
| `admin_password` | `TASKLANE_ADMIN_PASSWORD`, `TASKLANE_ADMIN_PASSWORD_FILE` | `--admin-password` | unset | Password for the administrator created from `admin_email` |
| `smtp_host` | `TASKLANE_SMTP_HOST` | `--smtp-host` | unset | Outbound mail server. Unset means mail is off |
| `smtp_port` | `TASKLANE_SMTP_PORT` | `--smtp-port` | `25` | Outbound mail server port |
| `smtp_tls` | `TASKLANE_SMTP_TLS` | `--smtp-tls` | `starttls` | `starttls` refuses to continue without it, `tls` is implicit TLS, `none` is plaintext. Certificates are always verified |
| `smtp_from` | `TASKLANE_SMTP_FROM` | `--smtp-from` | `Tasklane <tasklane@localhost>` | Sender address |
| `smtp_user` | `TASKLANE_SMTP_USER` | `--smtp-user` | unset | SMTP user. Credentials are never sent over an unencrypted connection to a non-local host |
| `smtp_pass` | `TASKLANE_SMTP_PASS`, `TASKLANE_SMTP_PASS_FILE` | `--smtp-pass` | unset | SMTP password |
<!-- config-table:end -->

## Commands

| Command | Purpose |
|---|---|
| `tasklane serve` | Run the server. A brand-new database is migrated automatically; an existing one is never migrated implicitly |
| `tasklane migrate` | Apply pending schema migrations (safe to run from several nodes at once) |
| `tasklane doctor` | Check database, schema version, administrator, blob directory and mail; exit status 1 on a failure |
| `tasklane config print` | Show the effective configuration and where each value came from; secrets are masked |
| `tasklane config docs` | Print the reference table above (generated from the schema) |
| `tasklane fsck [--repair]` | Compare the database with the blob store: missing, orphaned and corrupt objects |
| `tasklane reindex` | Rebuild the search index from PostgreSQL in one pass (unchanged documents are not rewritten) |
| `tasklane create-admin EMAIL NAME` | Create an administrator; the password comes from `TASKLANE_ADMIN_PASSWORD` |
| `tasklane version` | Print the version |

## Quick start

    export TASKLANE_DB_PASSWORD=change-me TASKLANE_ADMIN_EMAIL=you@example.com TASKLANE_ADMIN_PASSWORD=change-me-too
    docker compose -f deploy/compose/docker-compose.yml up -d --build
    docker compose -f deploy/compose/docker-compose.yml exec tasklane tasklane doctor
