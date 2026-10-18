-- phase: expand
-- trash (soft delete) metadata: who, when it is purged, and which issues went to the trash together (a parent and its subtasks)
ALTER TABLE issue ADD COLUMN deleted_by uuid;
ALTER TABLE issue ADD COLUMN purge_after timestamptz;
ALTER TABLE issue ADD COLUMN delete_batch uuid;
ALTER TABLE issue ADD COLUMN legal_hold boolean NOT NULL DEFAULT false;

-- an old key resolves forever: after a move or a project key change the old key is stored here and never reused
CREATE TABLE issue_key_alias (
  old_key    text PRIMARY KEY,
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX issue_key_alias_issue_idx ON issue_key_alias (issue_id);

-- values dropped by convert or move, kept for the trash retention period so that converting back restores them
CREATE TABLE removed_values (
  id         bigserial PRIMARY KEY,
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  kind       text NOT NULL,
  data       jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL
);
CREATE INDEX removed_values_issue_idx ON removed_values (issue_id);

-- typed links between issues (relates, blocks, duplicates, clones)
CREATE TABLE issue_link (
  id         bigserial PRIMARY KEY,
  from_id    uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  to_id      uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  type       text NOT NULL CHECK (type IN ('relates','blocks','duplicates','clones')),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (from_id, to_id, type)
);
CREATE INDEX issue_link_to_idx ON issue_link (to_id);
