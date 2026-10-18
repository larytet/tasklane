-- phase: expand
-- boards (SPEC 6): a board is a view over a project's issues; columns map to workflow statuses; rank is per board
CREATE TABLE board (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id    uuid NOT NULL REFERENCES project(id),
  name          text NOT NULL,
  kind          text NOT NULL CHECK (kind IN ('scrum','kanban')),
  filter        text NOT NULL DEFAULT '',
  estimation    text NOT NULL DEFAULT 'count' CHECK (estimation IN ('points','time','count')),
  swimlane      text NOT NULL DEFAULT 'none' CHECK (swimlane IN ('none','assignee','epic','priority','stories')),
  quick_filters jsonb NOT NULL DEFAULT '[]',
  sprint_weeks  int NOT NULL DEFAULT 1 CHECK (sprint_weeks BETWEEN 1 AND 8),
  done_days     int NOT NULL DEFAULT 14,
  calendar      jsonb,
  created_by    uuid REFERENCES app_user(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (project_id, name)
);
CREATE TABLE board_column (
  id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  board_id  uuid NOT NULL REFERENCES board(id) ON DELETE CASCADE,
  name      text NOT NULL,
  position  int NOT NULL,
  min_wip   int,
  max_wip   int,
  UNIQUE (board_id, name)
);
CREATE TABLE board_column_status (
  column_id uuid NOT NULL REFERENCES board_column(id) ON DELETE CASCADE,
  board_id  uuid NOT NULL REFERENCES board(id) ON DELETE CASCADE,
  status_id uuid NOT NULL REFERENCES status(id),
  PRIMARY KEY (column_id, status_id),
  UNIQUE (board_id, status_id)
);
CREATE TABLE board_rank (
  board_id uuid NOT NULL REFERENCES board(id) ON DELETE CASCADE,
  issue_id uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  rank     text COLLATE "C" NOT NULL,
  PRIMARY KEY (board_id, issue_id)
);
CREATE INDEX board_rank_order ON board_rank (board_id, rank);
