-- phase: expand
-- Audit log hardening (SPEC 17, SEC-07): the log is append-only and hash-chained, so a change or a removal is detected.
-- seq numbers the chain in commit order (ids are drawn before the lock, so they can interleave).
ALTER TABLE audit_event ADD COLUMN seq bigint;
ALTER TABLE audit_event ADD COLUMN prev_hash text NOT NULL DEFAULT '';
ALTER TABLE audit_event ADD COLUMN hash text NOT NULL DEFAULT '';
-- the one definition of a row's hash, used by the trigger that writes it and by the check that verifies it
CREATE FUNCTION audit_row_hash(prev text, id bigint, actor uuid, label text, action text, target text, detail jsonb, at timestamptz) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
  SELECT encode(sha256(convert_to(prev || '|' || id::text || '|' || COALESCE(actor::text, '') || '|' || label || '|' || action || '|' || target || '|' || detail::text || '|' ||
    to_char(at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), 'UTF8')), 'hex')
$$;
