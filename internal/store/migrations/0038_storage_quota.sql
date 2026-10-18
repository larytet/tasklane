-- phase: expand
-- A storage quota per project (BLOB-18): the most attachment bytes a project may hold; null means no limit.
ALTER TABLE project ADD COLUMN storage_quota_bytes bigint CHECK (storage_quota_bytes IS NULL OR storage_quota_bytes >= 0);
