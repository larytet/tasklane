-- phase: expand
-- Jira write-through (SPEC 18.1.1). No Jira credential is stored in PostgreSQL any more (WT-01): a connection keeps its site, the
-- account email and a fingerprint, and the token lives in the memory of the running process (internal/credvault). What was stored
-- is erased here. The column stays, empty and never written with anything but an empty value, so a node of the previous release
-- still starts (it simply cannot sync); a later release drops it with a contract migration, run by `migrate finalize`. After this
-- migration every connection is locked until an administrator unlocks it (POST /api/v1/jira/connections/{id}/unlock).
UPDATE jira_connection SET token_enc = ''::bytea;
-- A mirrored project is read only, or writes through to Jira with each person's own token.
ALTER TABLE jira_project ADD COLUMN mode text NOT NULL DEFAULT 'read_only' CHECK (mode IN ('read_only', 'write_through'));
