-- phase: expand
-- OpenSearch as a second, derived copy of the issues (SPEC 10.1): which index is live, the progress of an online rebuild,
-- the last reconcile report, and the writes that could not be applied. PostgreSQL stays the source of truth.
CREATE TABLE search_os_state (
  id             int PRIMARY KEY CHECK (id = 1),
  active_index   text NOT NULL DEFAULT '',
  building_index text NOT NULL DEFAULT '',   -- while set, every write goes to both indexes (dual write)
  build_last_id  text NOT NULL DEFAULT '',   -- keyset position of the rebuild, so a killed run resumes
  build_started  timestamptz,
  previous_index text NOT NULL DEFAULT '',   -- kept after a swap as the rollback window
  last_reconcile jsonb,
  reconciled_at  timestamptz
);
INSERT INTO search_os_state(id) VALUES (1);
CREATE TABLE search_os_dead (
  id       bigserial PRIMARY KEY,
  event_id bigint NOT NULL,
  issue_key text NOT NULL,
  error    text NOT NULL,
  at       timestamptz NOT NULL DEFAULT now(),
  resolved boolean NOT NULL DEFAULT false
);
