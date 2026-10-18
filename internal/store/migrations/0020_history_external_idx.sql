-- phase: expand
-- no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS issue_history_external_idx ON issue_history (issue_id, external_id) WHERE external_id IS NOT NULL;
