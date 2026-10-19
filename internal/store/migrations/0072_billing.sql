-- phase: expand
-- Prices and invoices for the hosted edition (SPEC 17.9). Like plans, nothing here does anything unless the operator of a hosted deployment turns hosting on and
-- sets prices; an invoice is made from the daily usage snapshots of one calendar month and kept as it was made, so a later change of price never rewrites it.
ALTER TABLE org_plan ADD COLUMN currency text NOT NULL DEFAULT 'usd';
ALTER TABLE org_plan ADD COLUMN price_base_cents bigint NOT NULL DEFAULT 0 CHECK (price_base_cents >= 0);       -- per month
ALTER TABLE org_plan ADD COLUMN price_seat_cents bigint NOT NULL DEFAULT 0 CHECK (price_seat_cents >= 0);       -- per average seat per month
ALTER TABLE org_plan ADD COLUMN price_gib_cents bigint NOT NULL DEFAULT 0 CHECK (price_gib_cents >= 0);         -- per average GiB of attachments per month
ALTER TABLE org_plan ADD COLUMN price_customer_cents bigint NOT NULL DEFAULT 0 CHECK (price_customer_cents >= 0); -- per most active customers in the month

CREATE TABLE invoice (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      uuid NOT NULL REFERENCES organization(id),
  number      text NOT NULL UNIQUE,                 -- TL-202610-0001
  period      text NOT NULL,                        -- 2026-10
  status      text NOT NULL DEFAULT 'open' CHECK (status IN ('open','sent','void')),
  currency    text NOT NULL,
  total_cents bigint NOT NULL,
  lines       jsonb NOT NULL,
  usage       jsonb NOT NULL,
  plan        jsonb NOT NULL,                       -- the plan and prices it was made under
  created_at  timestamptz NOT NULL DEFAULT now(),
  sent_at     timestamptz,
  void_reason text NOT NULL DEFAULT ''
);
-- one live invoice per organization and month; a void one can be made again
CREATE UNIQUE INDEX invoice_period_live ON invoice (org_id, period) WHERE status <> 'void';
