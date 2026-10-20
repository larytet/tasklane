-- phase: expand
-- The person's settings grow to everything the Settings dialog holds (SPEC 4.6: theme, density, columns and sort, font size, motion,
-- contrast, shortcuts, dismissed hints) in the same row. The version is what a second tab saves against (If-Match), so one tab's
-- change is never silently lost to another's.
ALTER TABLE user_setting ADD COLUMN version bigint NOT NULL DEFAULT 1;
