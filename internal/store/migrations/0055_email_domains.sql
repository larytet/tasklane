-- phase: expand
-- Email domain restriction (SPEC 17.1): an organization may limit which addresses its people can have. Enforced by the database, so no
-- way of making an account (the form, SCIM, single sign-on, an import that is later claimed) can go round it. People who already have
-- another address are not removed; they can no longer be created or re-activated.
ALTER TABLE organization ADD COLUMN allowed_email_domains text[] NOT NULL DEFAULT '{}';

CREATE FUNCTION app_user_email_domain() RETURNS trigger AS $$
DECLARE allowed text[];
BEGIN
  IF NEW.kind <> 'human' OR NOT NEW.active THEN
    RETURN NEW;                                 -- placeholders and deactivated accounts cannot sign in
  END IF;
  SELECT allowed_email_domains INTO allowed FROM organization WHERE id = NEW.org_id;
  IF allowed IS NOT NULL AND cardinality(allowed) > 0 AND NOT (lower(split_part(NEW.email, '@', 2)) = ANY (allowed)) THEN
    RAISE EXCEPTION 'the address is not in one of the allowed domains of this organization (%)', array_to_string(allowed, ', ') USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER app_user_email_domain BEFORE INSERT OR UPDATE OF email, active, org_id ON app_user FOR EACH ROW EXECUTE FUNCTION app_user_email_domain();
