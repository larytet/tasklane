-- phase: expand
-- no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS issue_history_field_idx ON issue_history (field, issue_id);
