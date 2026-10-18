-- phase: expand
-- no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS mail_inbound_received_idx ON mail_inbound (received_at) WHERE purged_at IS NULL;
