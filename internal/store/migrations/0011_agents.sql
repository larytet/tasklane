-- phase: expand
-- scoped, expiring, revocable tokens (SPEC 17.1, 18.3): a scope list is a ceiling; the permission scheme is enforced as well
ALTER TABLE api_token ADD COLUMN scopes text[];
ALTER TABLE api_token ADD COLUMN expires_at timestamptz;
ALTER TABLE api_token ADD COLUMN revoked_at timestamptz;
ALTER TABLE api_token ADD COLUMN id uuid DEFAULT gen_random_uuid();

-- the claim lease: while it is live no other agent can claim the issue; an expired lease frees it ("agent stalled")
CREATE TABLE issue_lease (
  issue_id     uuid PRIMARY KEY REFERENCES issue(id) ON DELETE CASCADE,
  holder_id    uuid NOT NULL REFERENCES app_user(id),
  expires_at   timestamptz NOT NULL,
  heartbeat_at timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- every MCP tool call, immutable
CREATE TABLE mcp_call_log (
  id             bigserial PRIMARY KEY,
  at             timestamptz NOT NULL DEFAULT now(),
  agent_id       uuid,
  agent_label    text NOT NULL,
  client_name    text NOT NULL DEFAULT '',
  client_version text NOT NULL DEFAULT '',
  session_id     text NOT NULL DEFAULT '',
  tool           text NOT NULL,
  args           jsonb NOT NULL DEFAULT '{}',
  outcome        text NOT NULL,           -- ok, error, denied, limited
  error          text NOT NULL DEFAULT '',
  duration_ms    int NOT NULL DEFAULT 0
);
CREATE INDEX mcp_call_log_agent_idx ON mcp_call_log (agent_id, id);

CREATE FUNCTION mcp_call_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'mcp_call_log is append-only';
END $$;
CREATE TRIGGER mcp_call_log_no_update BEFORE UPDATE OR DELETE ON mcp_call_log FOR EACH ROW EXECUTE FUNCTION mcp_call_log_immutable();
