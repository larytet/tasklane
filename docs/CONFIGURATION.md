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
| `blob_fallback` | `TASKLANE_BLOB_FALLBACK` | `--blob-fallback` | `none` | While moving attachments from one backend to another: the old backend. Reads that are not in `blob_backend` are answered from it (and copied across), writes go to `blob_backend` only. `fs` uses `blob_fallback_dir`; `s3` uses the `s3_*` settings. Run `tasklane blobs migrate` to copy the rest, then set this back to `none` |
| `blob_encryption_keys` | `TASKLANE_BLOB_ENCRYPTION_KEYS`, `TASKLANE_BLOB_ENCRYPTION_KEYS_FILE` | `--blob-encryption-keys` | unset | Encrypt attachments at rest. A comma-separated list of `id:hex` master keys (id 0-255, 64 hex characters = 32 bytes each), e.g. `1:ab12...`. Generate one with `openssl rand -hex 32`. Keep old keys in the list until `tasklane blobs rewrap` has moved every object to the current one. Empty: attachments are stored as they are |
| `blob_encryption_current` | `TASKLANE_BLOB_ENCRYPTION_CURRENT` | `--blob-encryption-current` | unset | The id of the master key new and rewrapped attachments use. Required when `blob_encryption_keys` is set |
| `blob_cold_backend` | `TASKLANE_BLOB_COLD_BACKEND` | `--blob-cold-backend` | `none` | A cold tier for attachments (SPEC 17.6): objects that match a tiering rule (class, age, size; set in Admin or with the API) move from `blob_backend` to this one, and reads stay transparent. `fs` uses `blob_cold_dir`; `s3` uses the `s3_*` settings, so `blob_backend` must then be `fs`. It must differ from `blob_backend` |
| `blob_cold_dir` | `TASKLANE_BLOB_COLD_DIR` | `--blob-cold-dir` | unset | The cold tier directory when `blob_cold_backend=fs` |
| `smtp_listen` | `TASKLANE_SMTP_LISTEN` | `--smtp-listen` | unset | Receive inbound email directly: the address for the built-in SMTP receiver to listen on, such as `:25` (empty: off). It accepts mail only for the reply, bounce and project addresses of the mail domain (the host of `base_url`), never relays, and never sends a bounce. Mail it receives needs its own DKIM signature to count; put a receiver you run in front and use the webhook if you want its verdict believed |
| `smtp_listen_tls_cert` | `TASKLANE_SMTP_LISTEN_TLS_CERT` | `--smtp-listen-tls-cert` | unset | Certificate file (PEM) for STARTTLS on the built-in SMTP receiver; with `smtp_listen_tls_key`. Without them STARTTLS is not offered |
| `smtp_listen_tls_key` | `TASKLANE_SMTP_LISTEN_TLS_KEY` | `--smtp-listen-tls-key` | unset | Private key file (PEM) for STARTTLS on the built-in SMTP receiver |
| `trusted_proxies` | `TASKLANE_TRUSTED_PROXIES` | `--trusted-proxies` | unset | Comma-separated networks of reverse proxies in front of Tasklane. Only from these is the X-Forwarded-For header believed, to learn a visitor's real address (the sign-in record, lockouts and an organization's allowed networks use it). Empty: the connecting address is used and the header is ignored. Never list a network that visitors can reach directly |
| `webauthn_rp_id` | `TASKLANE_WEBAUTHN_RP_ID` | `--webauthn-rp-id` | unset | The relying party id that passkeys and security keys are bound to: a host name (never an IP address). Empty: the host of `base_url`. A key registered for one id does not work for another, so change it only if you mean to re-register keys |
| `webauthn_origins` | `TASKLANE_WEBAUTHN_ORIGINS` | `--webauthn-origins` | unset | Comma-separated origins (scheme, host and port) pages may use passkeys from. Empty: the origin of `base_url` |
| `webauthn_require_user_verification` | `TASKLANE_WEBAUTHN_REQUIRE_USER_VERIFICATION` | `--webauthn-require-user-verification` | `false` | Require the key to verify the person (a PIN or biometric) as well as being present. Off: presence is enough as a second factor after the password |
| `min_client_version` | `TASKLANE_MIN_CLIENT_VERSION` | `--min-client-version` | `0` | The oldest browser client the server still serves (0: the previous one). A page older than this is told to reload, keeping its drafts. Raise it to force a reload after an upgrade that a cached page cannot survive; API tokens and MCP clients are never affected |
| `mail_raw_retention_days` | `TASKLANE_MAIL_RAW_RETENTION_DAYS` | `--mail-raw-retention-days` | `90` | How long the original of an inbound message, its text, its subject and its sender are kept, in days (SPEC 17.3). After that the message row stays without them, so a redelivery is still recognised. 0 keeps them for ever. Mail waiting for a decision and mail on an issue under legal hold is never purged. Erasing a person removes their mail at once, whatever this is |
| `mail_bounce_retention_days` | `TASKLANE_MAIL_BOUNCE_RETENTION_DAYS` | `--mail-bounce-retention-days` | `365` | How long bounce records and cleared suppressions are kept, in days (0 keeps them). An address that is still suppressed is never removed by age |
| `imap_allow_private` | `TASKLANE_IMAP_ALLOW_PRIVATE` | `--imap-allow-private` | `false` | Allow inbound-mail mailboxes (IMAP) on loopback and private addresses. Off by default so a mailbox setting cannot be used to reach internal services; cloud metadata addresses stay blocked either way |
| `blob_fallback_dir` | `TASKLANE_BLOB_FALLBACK_DIR` | `--blob-fallback-dir` | unset | The old attachment directory when `blob_fallback=fs` |
| `s3_bucket` | `TASKLANE_S3_BUCKET` | `--s3-bucket` | unset | S3 bucket (required with `blob_backend=s3`; it must already exist) |
| `s3_prefix` | `TASKLANE_S3_PREFIX` | `--s3-prefix` | unset | Optional key prefix, so a bucket can be shared, for example `tasklane/` |
| `s3_access_key` | `TASKLANE_S3_ACCESS_KEY`, `TASKLANE_S3_ACCESS_KEY_FILE` | `--s3-access-key` | unset | S3 access key id |
| `s3_secret_key` | `TASKLANE_S3_SECRET_KEY`, `TASKLANE_S3_SECRET_KEY_FILE` | `--s3-secret-key` | unset | S3 secret access key |
| `s3_path_style` | `TASKLANE_S3_PATH_STYLE` | `--s3-path-style` | `false` | Address the bucket in the URL path instead of the host name (always on with a custom endpoint) |
| `s3_sse` | `TASKLANE_S3_SSE` | `--s3-sse` | unset | Server-side encryption requested on every write |
| `s3_kms_key_id` | `TASKLANE_S3_KMS_KEY_ID` | `--s3-kms-key-id` | unset | KMS key for `s3_sse=aws:kms`; empty uses the bucket default key |
| `s3_part_size_mb` | `TASKLANE_S3_PART_SIZE_MB` | `--s3-part-size-mb` | `16` | Multipart part size in MiB (at least 5); larger files are uploaded in parts |
| `secret_key` | `TASKLANE_SECRET_KEY`, `TASKLANE_SECRET_KEY_FILE` | `--secret-key` | unset | 64 hexadecimal characters (32 bytes) that encrypt stored credentials such as Git tokens and webhook secrets. Set it (for example from a secret file) so a database dump alone cannot decrypt them; when unset a key kept in the database is used |
| `search_engine` | `TASKLANE_SEARCH_ENGINE` | `--search-engine` | `postgres` | `postgres` (built in, designed for up to 100,000 issues) or `opensearch` (a separate cluster you run; PostgreSQL stays the source of truth and answers whenever the cluster is unavailable) |
| `opensearch_url` | `TASKLANE_OPENSEARCH_URL` | `--opensearch-url` | unset | Base URL of the OpenSearch cluster, such as `https://opensearch.internal:9200`. Never expose the cluster publicly |
| `opensearch_user` | `TASKLANE_OPENSEARCH_USER` | `--opensearch-user` | unset | OpenSearch user for basic authentication |
| `opensearch_password` | `TASKLANE_OPENSEARCH_PASSWORD`, `TASKLANE_OPENSEARCH_PASSWORD_FILE` | `--opensearch-password` | unset | OpenSearch password |
| `opensearch_api_key` | `TASKLANE_OPENSEARCH_API_KEY`, `TASKLANE_OPENSEARCH_API_KEY_FILE` | `--opensearch-api-key` | unset | OpenSearch API key (used instead of user and password when set) |
| `opensearch_allow_private` | `TASKLANE_OPENSEARCH_ALLOW_PRIVATE` | `--opensearch-allow-private` | `false` | Allow the cluster to be on a private or loopback address (the usual case in a private network) |
| `webhook_allow_private` | `TASKLANE_WEBHOOK_ALLOW_PRIVATE` | `--webhook-allow-private` | `false` | Allow workflow webhook post-functions to call private or loopback addresses (refused by default so a webhook cannot reach internal services) |
| `password_min_length` | `TASKLANE_PASSWORD_MIN_LENGTH` | `--password-min-length` | `8` | Shortest accepted password for local accounts (at least 8) |
| `sso_allow_private` | `TASKLANE_SSO_ALLOW_PRIVATE` | `--sso-allow-private` | `false` | Allow single sign-on identity providers on private or loopback addresses (for a provider inside your network); refused by default |
| `opensearch_index` | `TASKLANE_OPENSEARCH_INDEX` | `--opensearch-index` | `tasklane-issues` | Alias name for the issue index; versioned indexes are created behind it |
| `opensearch_shards` | `TASKLANE_OPENSEARCH_SHARDS` | `--opensearch-shards` | `1` | Primary shards for new indexes |
| `opensearch_replicas` | `TASKLANE_OPENSEARCH_REPLICAS` | `--opensearch-replicas` | `0` | Replicas for new indexes (use at least 1 on a multi-node cluster) |
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
