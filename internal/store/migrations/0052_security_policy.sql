-- phase: expand
-- A security policy per organization (SPEC 17.4): require a second factor, allow sign-in and API use only from listed networks, and
-- cap how long a session may last.
ALTER TABLE organization ADD COLUMN require_second_factor boolean NOT NULL DEFAULT false;
ALTER TABLE organization ADD COLUMN ip_allowlist text[] NOT NULL DEFAULT '{}';
ALTER TABLE organization ADD COLUMN session_max_hours int NOT NULL DEFAULT 0 CHECK (session_max_hours >= 0);
-- how a session was started: a password (with its second factor when there is one) or single sign-on, where the identity provider
-- is the authority and no second factor of ours is asked
ALTER TABLE session ADD COLUMN via text NOT NULL DEFAULT 'password';
