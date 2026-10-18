-- phase: expand
-- Watching, the in-app inbox, per-person preferences and per-project notification schemes (SPEC 13).
CREATE TABLE issue_watch (
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  user_id    uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  muted      boolean NOT NULL DEFAULT false,          -- true: the person asked not to hear about this issue, even as reporter or assignee
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (issue_id, user_id)
);
CREATE TABLE notification (
  id          bigserial PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  issue_id    uuid REFERENCES issue(id) ON DELETE CASCADE,
  event_id    bigint NOT NULL,                        -- the outbox event: one notification per person, event and kind
  kind        text NOT NULL,                          -- assigned, mention, comment, status, edit, created
  what        text NOT NULL,
  body        text NOT NULL DEFAULT '',
  internal    boolean NOT NULL DEFAULT false,         -- from an internal comment: only people who may see those can open it
  actor_label text NOT NULL DEFAULT '',
  state       text NOT NULL DEFAULT 'unread' CHECK (state IN ('unread','read','archived')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  read_at     timestamptz,
  UNIQUE (user_id, event_id, kind)
);
CREATE INDEX notification_user_idx ON notification (user_id, state, id DESC);
CREATE TABLE notification_pref (
  user_id uuid PRIMARY KEY REFERENCES app_user(id) ON DELETE CASCADE,
  prefs   jsonb NOT NULL DEFAULT '{}'
);
-- which kinds of people a project notifies for an event; no row means the built-in default
CREATE TABLE notification_scheme (
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  event      text NOT NULL,
  recipients text[] NOT NULL,
  PRIMARY KEY (project_id, event)
);
