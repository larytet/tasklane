-- phase: expand
-- Transition rules (SPEC 9): conditions, validators, post-functions and the transition screen live on the transition as data.
ALTER TABLE transition ADD COLUMN rules jsonb NOT NULL DEFAULT '{}';
-- Asynchronous post-functions are queued in the transition's own transaction and delivered by a worker with retries.
CREATE TABLE workflow_job (
  id          bigserial PRIMARY KEY,
  kind        text NOT NULL,                -- webhook, create_subtask
  issue_id    uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  params      jsonb NOT NULL DEFAULT '{}',
  payload     jsonb NOT NULL DEFAULT '{}',
  state       text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','done','failed')),
  attempts    int NOT NULL DEFAULT 0,
  next_at     timestamptz NOT NULL DEFAULT now(),
  last_error  text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now(),
  done_at     timestamptz
);
CREATE INDEX workflow_job_due_idx ON workflow_job (next_at) WHERE state = 'pending';
