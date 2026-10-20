-- phase: expand
-- Per-person interface settings kept on the server (the list's short or long view, the page width, how much of a description the long view shows),
-- so they follow the person to every browser. One row per person; a missing row means the defaults.
CREATE TABLE user_setting (
  user_id  uuid PRIMARY KEY REFERENCES app_user(id) ON DELETE CASCADE,
  settings jsonb NOT NULL DEFAULT '{}'
);
