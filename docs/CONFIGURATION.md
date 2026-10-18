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
| `blob_backend` | `TASKLANE_BLOB_BACKEND` | `--blob-backend` | `fs` | Where attachments are kept: `fs` (a directory, `blob_dir`) or `s3` (any S3-compatible service; `blob_dir` then holds only upload staging) |
| `s3_endpoint` | `TASKLANE_S3_ENDPOINT` | `--s3-endpoint` | unset | S3 endpoint URL for MinIO and other compatible services, such as `http://minio:9000`. Empty means AWS S3. A custom endpoint implies path-style addressing |
| `s3_region` | `TASKLANE_S3_REGION` | `--s3-region` | `us-east-1` | S3 region used for request signing |
| `s3_bucket` | `TASKLANE_S3_BUCKET` | `--s3-bucket` | unset | S3 bucket (required with `blob_backend=s3`; it must already exist) |
| `s3_prefix` | `TASKLANE_S3_PREFIX` | `--s3-prefix` | unset | Optional key prefix, so a bucket can be shared, for example `tasklane/` |
| `s3_access_key` | `TASKLANE_S3_ACCESS_KEY`, `TASKLANE_S3_ACCESS_KEY_FILE` | `--s3-access-key` | unset | S3 access key id |
| `s3_secret_key` | `TASKLANE_S3_SECRET_KEY`, `TASKLANE_S3_SECRET_KEY_FILE` | `--s3-secret-key` | unset | S3 secret access key |
| `s3_path_style` | `TASKLANE_S3_PATH_STYLE` | `--s3-path-style` | `false` | Address the bucket in the URL path instead of the host name (always on with a custom endpoint) |
| `s3_sse` | `TASKLANE_S3_SSE` | `--s3-sse` | unset | Server-side encryption requested on every write |
| `s3_kms_key_id` | `TASKLANE_S3_KMS_KEY_ID` | `--s3-kms-key-id` | unset | KMS key for `s3_sse=aws:kms`; empty uses the bucket default key |
| `s3_part_size_mb` | `TASKLANE_S3_PART_SIZE_MB` | `--s3-part-size-mb` | `16` | Multipart part size in MiB (at least 5); larger files are uploaded in parts |
| `secret_key` | `TASKLANE_SECRET_KEY`, `TASKLANE_SECRET_KEY_FILE` | `--secret-key` | unset | 64 hexadecimal characters (32 bytes) that encrypt stored credentials such as Git tokens and webhook secrets. Set it (for example from a secret file) so a database dump alone cannot decrypt them; when unset a key kept in the database is used |
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
