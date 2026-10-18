-- phase: expand
-- no-transaction
-- the auto-archive job looks for resolved issues that are not yet archived
CREATE INDEX CONCURRENTLY IF NOT EXISTS issue_auto_archive_idx ON issue (project_id, resolved_at) WHERE archived_at IS NULL AND deleted_at IS NULL AND resolved_at IS NOT NULL;
