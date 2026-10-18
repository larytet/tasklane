-- phase: expand
-- no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS comment_external_idx ON comment (issue_id, external_id) WHERE external_id IS NOT NULL;
