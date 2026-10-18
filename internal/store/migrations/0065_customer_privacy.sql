-- phase: expand
-- Customers as data subjects (SPEC 15.1): an erased customer stays as an anonymous row (their requests belong to the tracker's records)
-- and is marked so, which makes an erase repeatable.
ALTER TABLE customer ADD COLUMN erased_at timestamptz;
