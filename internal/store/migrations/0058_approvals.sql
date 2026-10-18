-- phase: expand
-- Approvals (SPEC 15): a request on an issue to a set of approvers with a rule (all, any, or N of them), their decisions with comments,
-- and an optional deadline with one reminder. The approver set is fixed when the request is made (groups and fields are resolved then).
CREATE TABLE approval (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  issue_id     uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  requested_by uuid REFERENCES app_user(id) ON DELETE SET NULL,
  rule         text NOT NULL CHECK (rule IN ('all','any','n')),
  needed       int  NOT NULL DEFAULT 1 CHECK (needed >= 1),
  status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','granted','rejected','cancelled')),
  comment      text NOT NULL DEFAULT '',
  deadline     timestamptz,
  reminded_at  timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  decided_at   timestamptz
);
CREATE INDEX approval_issue_idx ON approval (issue_id, created_at DESC);
CREATE UNIQUE INDEX approval_one_pending_uq ON approval (issue_id) WHERE status = 'pending';
CREATE TABLE approval_approver (
  approval_id uuid NOT NULL REFERENCES approval(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  decision    text CHECK (decision IN ('approve','reject')),
  comment     text NOT NULL DEFAULT '',
  decided_at  timestamptz,
  PRIMARY KEY (approval_id, user_id)
);
CREATE INDEX approval_approver_user_idx ON approval_approver (user_id) WHERE decision IS NULL;
