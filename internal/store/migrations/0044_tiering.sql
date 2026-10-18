-- phase: expand
-- Blob tiering (SPEC 17.6): which tier holds each object, the rules that move objects to the cold tier, and a history of runs.
ALTER TABLE blob ADD COLUMN tier text NOT NULL DEFAULT 'hot' CHECK (tier IN ('hot','cold'));
ALTER TABLE blob ADD COLUMN tiered_at timestamptz;

CREATE TABLE tier_rule (
  id             bigserial PRIMARY KEY,
  name           text NOT NULL,
  class          text,                                  -- null: any class
  min_age_days   int NOT NULL DEFAULT 0 CHECK (min_age_days >= 0),
  min_size_bytes bigint NOT NULL DEFAULT 0 CHECK (min_size_bytes >= 0),
  enabled        boolean NOT NULL DEFAULT true,
  created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tier_run (
  id      bigserial PRIMARY KEY,
  at      timestamptz NOT NULL DEFAULT now(),
  moved   int NOT NULL DEFAULT 0,
  failed  int NOT NULL DEFAULT 0,
  bytes   bigint NOT NULL DEFAULT 0,
  error   text NOT NULL DEFAULT ''
);
