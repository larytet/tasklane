-- phase: expand
-- Customer sign-in through an identity provider (SPEC 15): a provider row that belongs to a portal (one project) instead of to the staff sign-in.
-- It signs customers in, never staff, and is deleted with the project.
ALTER TABLE sso_provider ADD COLUMN portal_project_id uuid REFERENCES project(id) ON DELETE CASCADE;
