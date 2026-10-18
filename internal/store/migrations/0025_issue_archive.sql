-- phase: expand
-- Issue archive (SPEC 9.1): a reversible, read-only state for finished work. Hidden from boards, backlogs and default
-- searches, found by `archived = true`, still counted in historical reports. A project can archive automatically.
ALTER TABLE issue ADD COLUMN archived_at timestamptz;
ALTER TABLE project ADD COLUMN auto_archive_days int CHECK (auto_archive_days IS NULL OR auto_archive_days BETWEEN 1 AND 3650);
