-- phase: expand
-- Visibility of development data (SPEC 18.4, GIT-11). By default anyone who can see an issue sees its code links. An organization
-- can restrict the details of private repositories to people who have a linked provider account that has access to the repository.
ALTER TABLE organization ADD COLUMN git_visibility text NOT NULL DEFAULT 'default' CHECK (git_visibility IN ('default','restricted'));

-- null: not known yet, which counts as private when restricted
ALTER TABLE git_repo ADD COLUMN private boolean;
ALTER TABLE git_repo ADD COLUMN private_override boolean;                       -- an administrator's word, which a sync does not overwrite
ALTER TABLE git_repo ADD COLUMN members_synced_at timestamptz;

-- who has access to a repository, as the provider last said (account names, lower case)
CREATE TABLE git_repo_member (
  repo_id uuid NOT NULL REFERENCES git_repo(id) ON DELETE CASCADE,
  login   text NOT NULL,
  PRIMARY KEY (repo_id, login)
);

-- May this person see the details of this repository? True for the system (no person), under the default setting, for an
-- instance administrator, for a repository known to be public, and for a person whose linked provider account the provider lists
-- as having access, when that list is less than a day old: an access list that has not been refreshed is not believed.
CREATE FUNCTION git_repo_visible(repo uuid, usr uuid, at timestamptz) RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT usr IS NULL
    OR COALESCE((SELECT og.git_visibility = 'default' FROM organization og JOIN app_user uu ON uu.org_id = og.id WHERE uu.id = usr), true)
    OR EXISTS (SELECT 1 FROM app_user ua WHERE ua.id = usr AND ua.is_admin AND ua.instance_admin)
    OR EXISTS (SELECT 1 FROM git_repo gr WHERE gr.id = repo AND gr.private IS FALSE)
    OR EXISTS (SELECT 1 FROM git_repo gr JOIN git_connection gc ON gc.id = gr.connection_id
               JOIN git_identity gi ON gi.user_id = usr AND gi.provider = gc.provider
               JOIN git_repo_member gm ON gm.repo_id = gr.id AND gm.login = gi.login
               WHERE gr.id = repo AND gr.members_synced_at > at - interval '24 hours')
$$;
