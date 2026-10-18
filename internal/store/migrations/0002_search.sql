CREATE EXTENSION IF NOT EXISTS unaccent SCHEMA public;

-- Narrow side tables written by the indexer worker (an outbox consumer), so typing never
-- rewrites the wide issue row or its indexes (DESIGN 11.4).
CREATE TABLE issue_search (
  issue_id     uuid PRIMARY KEY REFERENCES issue(id) ON DELETE CASCADE,
  project_id   uuid NOT NULL,
  key_text     text NOT NULL,
  summary_norm text NOT NULL,
  tsv          tsvector NOT NULL,       -- weights: A summary, B labels, C description
  text_hash    bytea NOT NULL,          -- skip the write when nothing searchable changed
  updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX issue_search_tsv  ON issue_search USING gin (tsv);
CREATE INDEX issue_search_trgm ON issue_search USING gin (summary_norm gin_trgm_ops);
CREATE INDEX issue_search_proj ON issue_search (project_id);

CREATE TABLE comment_search (
  comment_id uuid PRIMARY KEY REFERENCES comment(id) ON DELETE CASCADE,
  issue_id   uuid NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
  project_id uuid NOT NULL,
  internal   boolean NOT NULL,
  tsv        tsvector NOT NULL,
  created_at timestamptz NOT NULL
);
CREATE INDEX comment_search_tsv   ON comment_search USING gin (tsv);
CREATE INDEX comment_search_issue ON comment_search (issue_id);

CREATE INDEX issue_history_field_idx ON issue_history (field, issue_id);
