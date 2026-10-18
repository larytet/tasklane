-- phase: expand
-- Request types and portal requests (SPEC 15). A request type is a portal form mapped to an issue type; a request is an issue raised
-- through it by a customer. The customer's own comments are kept apart from staff comments (a customer is not a staff account).
CREATE TABLE request_type (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id    uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  name          text NOT NULL,
  description   text NOT NULL DEFAULT '',
  help_text     text NOT NULL DEFAULT '',
  issue_type_id uuid NOT NULL REFERENCES issue_type(id),
  fields        jsonb NOT NULL DEFAULT '[]',          -- the custom fields the form asks for: [{key,label,help,required}]
  visible       boolean NOT NULL DEFAULT true,
  position      int NOT NULL DEFAULT 0,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX request_type_name_uq ON request_type (project_id, lower(name));
CREATE TABLE portal_request (
  issue_id        uuid PRIMARY KEY REFERENCES issue(id) ON DELETE CASCADE,
  customer_id     uuid NOT NULL REFERENCES customer(id) ON DELETE CASCADE,
  request_type_id uuid REFERENCES request_type(id) ON DELETE SET NULL,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX portal_request_customer_idx ON portal_request (customer_id, created_at DESC);
CREATE TABLE customer_comment (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  issue_id    uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  customer_id uuid NOT NULL REFERENCES customer(id) ON DELETE CASCADE,
  body        text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_comment_issue_idx ON customer_comment (issue_id, created_at);
