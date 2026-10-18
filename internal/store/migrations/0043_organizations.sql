-- phase: expand
-- Organizations: the tenant root (SPEC 2, 17.4). One organization (Default) exists from the start, so a single-organization
-- install behaves exactly as before. A user and a project each belong to one organization, and access never crosses it:
-- the membership view only joins a user to a project of their own organization, and the triggers below refuse the rows
-- that would say otherwise.
CREATE TABLE organization (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name       text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX organization_name_uq ON organization (lower(name));
INSERT INTO organization(id,name) VALUES ('00000000-0000-0000-0000-0000000000a1','Default');

ALTER TABLE app_user ADD COLUMN org_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id);
-- an instance administrator runs the installation (global settings, organizations); an organization administrator
-- (is_admin without this) administers only their own organization
ALTER TABLE app_user ADD COLUMN instance_admin boolean NOT NULL DEFAULT false;
UPDATE app_user SET instance_admin = true WHERE is_admin;
ALTER TABLE project ADD COLUMN org_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id);
ALTER TABLE user_group ADD COLUMN org_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id);

CREATE OR REPLACE VIEW effective_member AS
  SELECT g.project_id, g.user_id,
         (array_agg(g.role ORDER BY CASE g.role WHEN 'admin' THEN 3 WHEN 'member' THEN 2 ELSE 1 END DESC))[1] AS role
  FROM (
    SELECT m.project_id, m.user_id, m.role FROM project_member m
    UNION ALL
    SELECT r.project_id, gm.user_id, r.role FROM project_group_role r JOIN user_group_member gm ON gm.group_id = r.group_id
  ) g JOIN project p ON p.id = g.project_id
      JOIN app_user u ON u.id = g.user_id AND u.org_id = p.org_id
  WHERE p.mirror_of IS NULL
     OR EXISTS (SELECT 1 FROM jira_project jp WHERE jp.project_id = p.id AND jp.perm_synced_at > now() - interval '15 minutes')
  GROUP BY g.project_id, g.user_id;

CREATE FUNCTION project_member_same_org() RETURNS trigger AS $$
BEGIN
  IF (SELECT org_id FROM app_user WHERE id = NEW.user_id) IS DISTINCT FROM (SELECT org_id FROM project WHERE id = NEW.project_id) THEN
    RAISE EXCEPTION 'a user cannot be a member of a project in another organization' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER project_member_same_org BEFORE INSERT OR UPDATE ON project_member FOR EACH ROW EXECUTE FUNCTION project_member_same_org();

CREATE FUNCTION group_member_same_org() RETURNS trigger AS $$
BEGIN
  IF (SELECT org_id FROM app_user WHERE id = NEW.user_id) IS DISTINCT FROM (SELECT org_id FROM user_group WHERE id = NEW.group_id) THEN
    RAISE EXCEPTION 'a user cannot be in a group of another organization' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER group_member_same_org BEFORE INSERT OR UPDATE ON user_group_member FOR EACH ROW EXECUTE FUNCTION group_member_same_org();

CREATE FUNCTION group_role_same_org() RETURNS trigger AS $$
BEGIN
  IF (SELECT org_id FROM user_group WHERE id = NEW.group_id) IS DISTINCT FROM (SELECT org_id FROM project WHERE id = NEW.project_id) THEN
    RAISE EXCEPTION 'a group cannot have a role in a project of another organization' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER group_role_same_org BEFORE INSERT OR UPDATE ON project_group_role FOR EACH ROW EXECUTE FUNCTION group_role_same_org();
