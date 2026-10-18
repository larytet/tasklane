-- phase: expand
-- Queues and canned responses (SPEC 15): a queue is a named TQL query over a project's issues that agents work through, with a count;
-- a canned response is a saved reply that can be inserted into a comment.
CREATE TABLE queue (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  name       text NOT NULL,
  tql        text NOT NULL,
  position   int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX queue_name_uq ON queue (project_id, lower(name));
CREATE TABLE canned_response (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  name       text NOT NULL,
  body       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX canned_name_uq ON canned_response (project_id, lower(name));
