-- phase: expand
-- Custom project roles (SPEC 17.2): a named role with a base (viewer or member, which is what the rest of the system reads) and
-- extra permissions it grants on top, for the permissions a scheme can set. A custom role belongs to an organization.
CREATE TABLE custom_role (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id      uuid NOT NULL DEFAULT '00000000-0000-0000-0000-0000000000a1' REFERENCES organization(id),
  name        text NOT NULL,
  base        text NOT NULL CHECK (base IN ('viewer','member')),
  grants      text[] NOT NULL DEFAULT '{}',
  description text NOT NULL DEFAULT '',
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX custom_role_name_uq ON custom_role (org_id, lower(name));
-- a role still given to someone cannot be deleted (no ON DELETE action)
ALTER TABLE project_member ADD COLUMN custom_role_id uuid REFERENCES custom_role(id);
ALTER TABLE project_group_role ADD COLUMN custom_role_id uuid REFERENCES custom_role(id);

-- the permission engine's one view of membership, now also with the extra permissions that custom roles grant
CREATE OR REPLACE VIEW effective_member AS
  SELECT g.project_id, g.user_id,
         (array_agg(g.role ORDER BY CASE g.role WHEN 'admin' THEN 3 WHEN 'member' THEN 2 ELSE 1 END DESC))[1] AS role,
         COALESCE(array_remove(array_agg(DISTINCT gr.perm), NULL), '{}') AS grants
  FROM (
    SELECT m.project_id, m.user_id, m.role, m.custom_role_id FROM project_member m
    UNION ALL
    SELECT r.project_id, gm.user_id, r.role, r.custom_role_id FROM project_group_role r JOIN user_group_member gm ON gm.group_id = r.group_id
  ) g JOIN project p ON p.id = g.project_id
      JOIN app_user u ON u.id = g.user_id AND u.org_id = p.org_id
      LEFT JOIN custom_role cr ON cr.id = g.custom_role_id
      LEFT JOIN LATERAL unnest(cr.grants) AS gr(perm) ON true
  WHERE p.mirror_of IS NULL
     OR EXISTS (SELECT 1 FROM jira_project jp WHERE jp.project_id = p.id AND jp.perm_synced_at > now() - interval '15 minutes')
  GROUP BY g.project_id, g.user_id;
