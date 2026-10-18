-- phase: expand
-- no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS app_user_jira_account_idx ON app_user (jira_account_id) WHERE jira_account_id IS NOT NULL;
