-- phase: expand
-- The pause barrier across nodes (SPEC 17.8, "pause barrier"): each node that has seen a pause lease and has no write of its own still running in
-- its scope says so here, so the node that asked for the pause knows every other node has drained before it reports the scope paused.
CREATE TABLE pause_ack (
  lease_id uuid NOT NULL REFERENCES pause_lease(id) ON DELETE CASCADE,
  node_id  text NOT NULL,
  acked_at timestamptz NOT NULL,
  PRIMARY KEY (lease_id, node_id)
);
