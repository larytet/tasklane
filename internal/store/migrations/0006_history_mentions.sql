-- phase: expand
-- per-field "track in history" switch (SPEC 4.5): on by default, an admin can turn a field off; applies going forward
CREATE TABLE field_setting (
  field         text PRIMARY KEY,
  track_history boolean NOT NULL DEFAULT true
);

-- what kind of mail a row is, and who caused it: lets edit notices be batched per person and mentions be sent once
ALTER TABLE mail_message ADD COLUMN kind text NOT NULL DEFAULT 'event';
ALTER TABLE mail_message ADD COLUMN actor_id uuid;
