-- phase: expand
-- sprints and the backlog (SPEC 7): estimates, per-user calendars, board sprint settings, sprints with a calendar snapshot,
-- membership with scope-change tracking, and per-sprint capacity
ALTER TABLE issue ADD COLUMN estimate numeric(10,2);
ALTER TABLE app_user ADD COLUMN calendar jsonb;
ALTER TABLE board ADD COLUMN auto_sprints int NOT NULL DEFAULT 2 CHECK (auto_sprints BETWEEN 0 AND 12);
ALTER TABLE board ADD COLUMN rollover text NOT NULL DEFAULT 'next' CHECK (rollover IN ('next','backlog'));
ALTER TABLE board ADD COLUMN parallel_sprints boolean NOT NULL DEFAULT false;
ALTER TABLE board ADD COLUMN name_pattern text NOT NULL DEFAULT '';
ALTER TABLE board ADD COLUMN auto_rollover boolean NOT NULL DEFAULT true;

CREATE TABLE sprint (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  board_id   uuid NOT NULL REFERENCES board(id) ON DELETE CASCADE,
  seq        int NOT NULL,
  name       text NOT NULL,
  auto_named boolean NOT NULL DEFAULT true,
  goal       text NOT NULL DEFAULT '',
  state      text NOT NULL DEFAULT 'future' CHECK (state IN ('future','active','closed')),
  starts_at  timestamptz NOT NULL,
  ends_at    timestamptz NOT NULL CHECK (ends_at > starts_at),
  calendar   jsonb NOT NULL,                -- snapshot: reports for closed sprints never change
  started_at timestamptz,
  closed_at  timestamptz,
  retro      text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (board_id, starts_at),
  UNIQUE (board_id, seq)
);
CREATE INDEX sprint_state_idx ON sprint (board_id, state, starts_at);

CREATE TABLE sprint_issue (
  sprint_id uuid NOT NULL REFERENCES sprint(id) ON DELETE CASCADE,
  issue_id  uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  added_at  timestamptz NOT NULL,
  removed_at timestamptz,
  committed boolean NOT NULL DEFAULT false, -- in the sprint when it started
  outcome   text CHECK (outcome IN ('done','not_done')),
  PRIMARY KEY (sprint_id, issue_id)
);
CREATE INDEX sprint_issue_issue_idx ON sprint_issue (issue_id) WHERE removed_at IS NULL;
CREATE INDEX sprint_issue_carry_idx ON sprint_issue (issue_id) WHERE outcome = 'not_done';

CREATE TABLE sprint_capacity (
  sprint_id uuid NOT NULL REFERENCES sprint(id) ON DELETE CASCADE,
  user_id   uuid NOT NULL REFERENCES app_user(id),
  off_hours numeric(6,2) NOT NULL DEFAULT 0 CHECK (off_hours >= 0),
  PRIMARY KEY (sprint_id, user_id)
);
