-- phase: expand
-- Create issue by email (SPEC 13.1): what a project does with mail from a sender it does not know, and what a
-- quarantined message would do when released.
ALTER TABLE project ADD COLUMN mail_policy text NOT NULL DEFAULT 'quarantine' CHECK (mail_policy IN ('quarantine','reject'));
ALTER TABLE mail_inbound ADD COLUMN kind text NOT NULL DEFAULT 'reply' CHECK (kind IN ('reply','create'));
ALTER TABLE mail_inbound ADD COLUMN detail jsonb NOT NULL DEFAULT '{}';
