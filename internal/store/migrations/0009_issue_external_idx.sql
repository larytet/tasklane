-- phase: expand
-- no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS issue_external_idx ON issue (project_id, external_id) WHERE external_id IS NOT NULL;
