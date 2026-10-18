-- phase: expand
-- Groups, group roles in projects, and permission schemes (SPEC 17.2). A person's role in a project is the highest of
-- their own and their groups' roles; a project's permission scheme says which role each action needs.
CREATE TABLE user_group (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name        text NOT NULL,
  external_id text,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX user_group_name_uq ON user_group (lower(name));
CREATE TABLE user_group_member (
  group_id uuid NOT NULL REFERENCES user_group(id) ON DELETE CASCADE,
  user_id  uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, user_id)
);
CREATE INDEX user_group_member_user_idx ON user_group_member (user_id);
CREATE TABLE project_group_role (
  project_id uuid NOT NULL REFERENCES project(id) ON DELETE CASCADE,
  group_id   uuid NOT NULL REFERENCES user_group(id) ON DELETE CASCADE,
  role       text NOT NULL CHECK (role IN ('viewer','member','admin')),
  PRIMARY KEY (project_id, group_id)
);
CREATE TABLE permission_scheme (
  project_id uuid PRIMARY KEY REFERENCES project(id) ON DELETE CASCADE,
  rules      jsonb NOT NULL DEFAULT '{}'      -- permission name -> the lowest role that may do it
);
-- the permission engine's one view of membership: direct and group grants, the highest role wins, one row per person
CREATE OR REPLACE VIEW effective_member AS
  SELECT g.project_id, g.user_id,
         (array_agg(g.role ORDER BY CASE g.role WHEN 'admin' THEN 3 WHEN 'member' THEN 2 ELSE 1 END DESC))[1] AS role
  FROM (
    SELECT m.project_id, m.user_id, m.role FROM project_member m
    UNION ALL
    SELECT r.project_id, gm.user_id, r.role FROM project_group_role r JOIN user_group_member gm ON gm.group_id = r.group_id
  ) g JOIN project p ON p.id = g.project_id
  WHERE p.mirror_of IS NULL
     OR EXISTS (SELECT 1 FROM jira_project jp WHERE jp.project_id = p.id AND jp.perm_synced_at > now() - interval '15 minutes')
  GROUP BY g.project_id, g.user_id;
