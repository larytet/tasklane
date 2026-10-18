-- phase: expand
-- Mailboxes Tasklane reads inbound mail from (SPEC 13.1): IMAP polling checkpointed by UID, with IDLE where the server has it.
CREATE TABLE mail_imap (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name         text NOT NULL UNIQUE,
  host         text NOT NULL,
  port         int NOT NULL CHECK (port BETWEEN 1 AND 65535),
  tls          text NOT NULL DEFAULT 'tls' CHECK (tls IN ('tls','none')),
  username     text NOT NULL,
  password     bytea NOT NULL,                 -- sealed
  mailbox      text NOT NULL DEFAULT 'INBOX',
  trusted      boolean NOT NULL DEFAULT false, -- the receiving path is ours: its Authentication-Results header is believed
  enabled      boolean NOT NULL DEFAULT true,
  uidvalidity  bigint NOT NULL DEFAULT 0,
  last_uid     bigint NOT NULL DEFAULT 0,      -- the checkpoint: every message up to here has been handled
  resets       int NOT NULL DEFAULT 0,         -- times UIDVALIDITY changed and the mailbox was read again from the start
  received     bigint NOT NULL DEFAULT 0,
  last_poll    timestamptz,
  last_ok      timestamptz,
  last_error   text NOT NULL DEFAULT '',
  leased_until timestamptz,                    -- one node polls a mailbox at a time
  created_at   timestamptz NOT NULL DEFAULT now()
);
