-- phase: expand
-- Customer organizations and shared requests (SPEC 15.1): a request is seen by the customer who raised it, by the people they added to it,
-- and, when they chose, by everyone in their customer organization (their company).
ALTER TABLE portal_request ADD COLUMN shared_with_org boolean NOT NULL DEFAULT false;
CREATE TABLE request_participant (
  issue_id    uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  customer_id uuid NOT NULL REFERENCES customer(id) ON DELETE CASCADE,
  added_by    uuid REFERENCES customer(id) ON DELETE SET NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issue_id, customer_id)
);
CREATE INDEX request_participant_customer_idx ON request_participant (customer_id);
