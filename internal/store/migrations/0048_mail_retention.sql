-- phase: expand
-- Retention of inbound mail (SPEC 17.3): a purged message keeps its row (so a redelivery is still recognised and the counts are
-- true) but loses the original, the text and who it was from.
ALTER TABLE mail_inbound ADD COLUMN purged_at timestamptz;
