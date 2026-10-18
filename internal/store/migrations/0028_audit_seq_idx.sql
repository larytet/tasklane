-- phase: expand
-- no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS audit_event_seq_idx ON audit_event (seq) WHERE seq IS NOT NULL;
