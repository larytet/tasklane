-- phase: expand
-- Erasing a person (SEC-10) has to reach the audit log, which is append-only and hash-chained. Only this function may
-- change audit rows: it replaces the person's name in the entries that are theirs and then re-chains everything after the
-- first change, so verification still passes. Nothing else can set the switch it relies on without owning the table.
CREATE OR REPLACE FUNCTION audit_event_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND current_setting('tasklane.audit_redact', true) = 'on' THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'audit_event is append-only' USING ERRCODE = 'insufficient_privilege';
END $$;
CREATE FUNCTION audit_redact_actor(p_actor uuid) RETURNS int LANGUAGE plpgsql AS $$
DECLARE n int; first_seq bigint; prev text; r record;
BEGIN
  PERFORM pg_advisory_xact_lock(7242001);
  PERFORM set_config('tasklane.audit_redact', 'on', true);
  SELECT min(seq) INTO first_seq FROM audit_event WHERE actor_id = p_actor OR target = p_actor::text;
  UPDATE audit_event SET
      actor_label = CASE WHEN actor_id = p_actor THEN 'Deleted user' ELSE actor_label END,
      detail = detail - 'email' - 'name' - 'userName' - 'display_name' - 'user' - 'ip'
    WHERE actor_id = p_actor OR target = p_actor::text;
  GET DIAGNOSTICS n = ROW_COUNT;
  IF first_seq IS NOT NULL THEN
    SELECT hash INTO prev FROM audit_event WHERE seq = first_seq - 1;
    prev := COALESCE(prev, '');
    FOR r IN SELECT id, seq, actor_id, actor_label, action, target, detail, at FROM audit_event WHERE seq >= first_seq ORDER BY seq LOOP
      UPDATE audit_event SET prev_hash = prev, hash = audit_row_hash(prev, r.id, r.actor_id, r.actor_label, r.action, r.target, r.detail, r.at)
        WHERE seq = r.seq RETURNING hash INTO prev;
    END LOOP;
  END IF;
  PERFORM set_config('tasklane.audit_redact', 'off', true);
  RETURN n;
END $$;
