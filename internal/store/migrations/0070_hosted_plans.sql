-- phase: expand
-- Plans, quotas and metering for the hosted edition (SPEC 17.9). Nothing here does anything on a self-hosted install: a plan exists only when
-- the operator of a hosted deployment sets one for an organization, so an organization with no row has no limits. The seat and project limits
-- are enforced by the database itself, so every way of making a person or a project (the screens, SCIM, single sign-on, imports, a reactivation)
-- meets them, and the check is made under a lock so two at once cannot both take the last seat.
CREATE TABLE org_plan (
  org_id            uuid PRIMARY KEY REFERENCES organization(id) ON DELETE CASCADE,
  plan              text NOT NULL DEFAULT '',
  max_seats         integer CHECK (max_seats >= 0),
  max_projects      integer CHECK (max_projects >= 0),
  max_storage_bytes bigint CHECK (max_storage_bytes >= 0),
  region            text NOT NULL DEFAULT '',
  updated_at        timestamptz NOT NULL DEFAULT now()
);

-- what was used, per organization and day, for billing: one row a day, rewritten while the day is open
CREATE TABLE usage_snapshot (
  org_id           uuid NOT NULL REFERENCES organization(id) ON DELETE CASCADE,
  day              date NOT NULL,
  seats            integer NOT NULL,
  projects         integer NOT NULL,
  issues           bigint NOT NULL,
  storage_bytes    bigint NOT NULL,
  active_customers integer NOT NULL,
  PRIMARY KEY (org_id, day)
);

CREATE FUNCTION enforce_plan_seats() RETURNS trigger AS $$
DECLARE lim integer; used integer;
BEGIN
  IF NEW.kind <> 'human' OR NOT NEW.active THEN RETURN NEW; END IF;
  IF TG_OP = 'UPDATE' AND OLD.active AND OLD.kind = 'human' AND OLD.org_id = NEW.org_id THEN RETURN NEW; END IF;
  SELECT max_seats INTO lim FROM org_plan WHERE org_id = NEW.org_id;
  IF lim IS NULL THEN RETURN NEW; END IF;
  PERFORM pg_advisory_xact_lock(hashtext('plan-seats:' || NEW.org_id::text));
  SELECT count(*) INTO used FROM app_user WHERE org_id = NEW.org_id AND active AND kind = 'human' AND id <> NEW.id;
  IF used >= lim THEN
    RAISE EXCEPTION 'plan limit: seats (% of %)', used, lim USING ERRCODE = 'TL001';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER app_user_plan_seats BEFORE INSERT OR UPDATE OF active, kind, org_id ON app_user FOR EACH ROW EXECUTE FUNCTION enforce_plan_seats();

CREATE FUNCTION enforce_plan_projects() RETURNS trigger AS $$
DECLARE lim integer; used integer;
BEGIN
  SELECT max_projects INTO lim FROM org_plan WHERE org_id = NEW.org_id;
  IF lim IS NULL THEN RETURN NEW; END IF;
  PERFORM pg_advisory_xact_lock(hashtext('plan-projects:' || NEW.org_id::text));
  SELECT count(*) INTO used FROM project WHERE org_id = NEW.org_id AND NOT archived;
  IF used >= lim THEN
    RAISE EXCEPTION 'plan limit: projects (% of %)', used, lim USING ERRCODE = 'TL001';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER project_plan_projects BEFORE INSERT ON project FOR EACH ROW EXECUTE FUNCTION enforce_plan_projects();
