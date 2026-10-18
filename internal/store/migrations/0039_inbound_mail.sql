-- phase: expand
-- Inbound email (SPEC 13.1): sources, the reply-token key ring, and one row per received message with its outcome.

-- Keys that sign reply addresses. The newest unrevoked key signs; every unrevoked key verifies, so a key can be
-- rotated without breaking the addresses already sent. Revoking a key invalidates the addresses it signed.
CREATE TABLE mail_reply_key (
  kid        smallint PRIMARY KEY,
  key        bytea NOT NULL,            -- sealed
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
);

-- A source delivers raw messages. A webhook source is signed with its own secret; "trusted" means the path
-- before it is ours (a provider or a relay we run), so its Authentication-Results header may be believed.
CREATE TABLE mail_source (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name       text NOT NULL UNIQUE,
  kind       text NOT NULL DEFAULT 'webhook' CHECK (kind IN ('webhook')),
  secret     bytea NOT NULL,            -- sealed
  trusted    boolean NOT NULL DEFAULT false,
  enabled    boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mail_inbound (
  id          bigserial PRIMARY KEY,
  source_id   uuid REFERENCES mail_source(id) ON DELETE SET NULL,
  message_id  text NOT NULL UNIQUE,     -- duplicates are counted, never processed twice
  dupes       int NOT NULL DEFAULT 0,
  raw_sha256  text,                     -- the .eml in the blob store, kept before anything else happens
  sender      text NOT NULL DEFAULT '',
  subject     text NOT NULL DEFAULT '',
  received_at timestamptz NOT NULL DEFAULT now(),
  outcome     text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending','accepted','ignored','quarantined','rejected')),
  reason      text NOT NULL DEFAULT '',
  issue_id    uuid REFERENCES issue(id) ON DELETE SET NULL,
  user_id     uuid REFERENCES app_user(id) ON DELETE SET NULL, -- who the message would act as
  body        text NOT NULL DEFAULT '',                         -- the text a release would post
  comment_id  uuid,
  handled_by  uuid REFERENCES app_user(id) ON DELETE SET NULL,
  handled_at  timestamptz
);
CREATE INDEX mail_inbound_sender_idx ON mail_inbound (sender, received_at);
CREATE INDEX mail_inbound_outcome_idx ON mail_inbound (outcome, id);

ALTER TABLE comment ADD COLUMN mail_inbound_id bigint;
ALTER TABLE mail_message ADD COLUMN reply_to text NOT NULL DEFAULT '';
