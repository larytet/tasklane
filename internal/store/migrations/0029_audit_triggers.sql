-- phase: expand
-- Writers take one lock, so the chain follows commit order; rows are never updated, deleted or truncated.
CREATE FUNCTION audit_event_chain() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE last_seq bigint; last_hash text;
BEGIN
  PERFORM pg_advisory_xact_lock(7242001);
  SELECT seq, hash INTO last_seq, last_hash FROM audit_event WHERE seq IS NOT NULL ORDER BY seq DESC LIMIT 1;
  NEW.seq := COALESCE(last_seq, 0) + 1;
  NEW.prev_hash := COALESCE(last_hash, '');
  NEW.hash := audit_row_hash(NEW.prev_hash, NEW.id, NEW.actor_id, NEW.actor_label, NEW.action, NEW.target, NEW.detail, NEW.at);
  RETURN NEW;
END $$;
CREATE TRIGGER audit_event_chain_trg BEFORE INSERT ON audit_event FOR EACH ROW EXECUTE FUNCTION audit_event_chain();
CREATE FUNCTION audit_event_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'audit_event is append-only' USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE TRIGGER audit_event_no_change BEFORE UPDATE OR DELETE ON audit_event FOR EACH ROW EXECUTE FUNCTION audit_event_append_only();
CREATE TRIGGER audit_event_no_truncate BEFORE TRUNCATE ON audit_event FOR EACH STATEMENT EXECUTE FUNCTION audit_event_append_only();
