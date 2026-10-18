-- phase: expand
-- How far each node's own clock is from the database's (corrected for by the node; shown to the operator).
ALTER TABLE cluster_node ADD COLUMN clock_offset_ms bigint NOT NULL DEFAULT 0;
