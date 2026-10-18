-- phase: expand
-- The customer portal (SPEC 15.1): customers are a separate kind of identity from staff, with their own sessions, never a licensed seat.
CREATE TABLE portal_setting (
  project_id     uuid PRIMARY KEY REFERENCES project(id) ON DELETE CASCADE,
  enabled        boolean NOT NULL DEFAULT false,
  welcome        text NOT NULL DEFAULT '',
  privacy_notice text NOT NULL DEFAULT '',
  consent_text   text NOT NULL DEFAULT '',
  allow          text[] NOT NULL DEFAULT '{}',   -- when not empty only these addresses and @domains may sign in
  block          text[] NOT NULL DEFAULT '{}'    -- these never may (a block wins over an allow)
);
CREATE TABLE customer_org (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id     uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id),
  name       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX customer_org_name_uq ON customer_org (org_id, lower(name));
CREATE TABLE customer (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id          uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id),
  email           text NOT NULL,
  name            text NOT NULL DEFAULT '',
  language        text NOT NULL DEFAULT '',
  timezone        text NOT NULL DEFAULT '',
  customer_org_id uuid REFERENCES customer_org(id) ON DELETE SET NULL,
  password_hash   text,
  blocked         boolean NOT NULL DEFAULT false,
  verified_at     timestamptz,
  promoted_to     uuid REFERENCES app_user(id) ON DELETE SET NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_seen       timestamptz
);
CREATE UNIQUE INDEX customer_email_uq ON customer (org_id, lower(email));
-- one issued sign-in: a six digit code and a link token, both single use, stored only as hashes
CREATE TABLE customer_code (
  id         bigserial PRIMARY KEY,
  org_id     uuid NOT NULL REFERENCES organization(id),
  email      text NOT NULL,
  code_hash  bytea NOT NULL,
  link_hash  bytea NOT NULL,
  attempts   int NOT NULL DEFAULT 0,
  expires_at timestamptz NOT NULL,
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_code_email_idx ON customer_code (org_id, lower(email), id DESC);
CREATE UNIQUE INDEX customer_code_link_uq ON customer_code (link_hash);
CREATE TABLE customer_session (
  token_hash  bytea PRIMARY KEY,
  customer_id uuid NOT NULL REFERENCES customer(id) ON DELETE CASCADE,
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX customer_session_customer_idx ON customer_session (customer_id);
-- what the abuse limits count: sign-in codes asked for and failed attempts, by address and by network address
CREATE TABLE portal_event (
  id      bigserial PRIMARY KEY,
  at      timestamptz NOT NULL DEFAULT now(),
  kind    text NOT NULL,
  subject text NOT NULL
);
CREATE INDEX portal_event_idx ON portal_event (kind, subject, at);
