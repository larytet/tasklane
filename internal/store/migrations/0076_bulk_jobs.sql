-- phase: expand
-- Bulk lifecycle operations as resumable jobs (LIFE-16): a job is a list of issues and one operation. Each item is finished in the same transaction as
-- the operation on its issue, so a node that dies mid-job loses at most the item in flight, and another node carries on from the first item left.
CREATE TABLE bulk_job (
  id          bigserial PRIMARY KEY,
  op          text NOT NULL,
  params      jsonb NOT NULL DEFAULT '{}',
  created_by  uuid REFERENCES app_user(id) ON DELETE SET NULL,
  state       text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','done','cancelled')),
  total       integer NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  started_at  timestamptz,
  finished_at timestamptz,
  leased_until timestamptz
);
CREATE TABLE bulk_job_item (
  job_id     bigint NOT NULL REFERENCES bulk_job(id) ON DELETE CASCADE,
  idx        integer NOT NULL,
  key        text NOT NULL,
  state      text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done','failed')),
  result_key text NOT NULL DEFAULT '',
  error      text NOT NULL DEFAULT '',
  done_at    timestamptz,
  PRIMARY KEY (job_id, idx)
);
CREATE INDEX bulk_job_open_idx ON bulk_job (id) WHERE state IN ('pending','running');
