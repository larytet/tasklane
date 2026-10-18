# Search on OpenSearch

The built-in PostgreSQL search is designed for up to 100,000 issues. Above that, or for faster and richer search, run
OpenSearch and set `search_engine=opensearch` with `opensearch_url` (and credentials). OpenSearch is a separate service you
deploy, secure and size; it is never public, and it is **a derived copy**: PostgreSQL stays the source of truth, and
Tasklane keeps working, with PostgreSQL answering, whenever the cluster is down.

```
TASKLANE_SEARCH_ENGINE=opensearch
TASKLANE_OPENSEARCH_URL=https://opensearch.internal:9200
TASKLANE_OPENSEARCH_USER=tasklane
TASKLANE_OPENSEARCH_PASSWORD_FILE=/run/secrets/os_password
TASKLANE_OPENSEARCH_ALLOW_PRIVATE=true
```

`tasklane doctor` checks the cluster (reachable, credentials, green/yellow/red). The first start creates the index
`tasklane-issues-v1` behind the alias `tasklane-issues` and loads it through the normal event stream; run a rebuild once to
load existing issues (`POST /api/v1/admin/search/rebuild`).

## What uses the index

Search boxes, issue lists and typeahead. Boards, backlogs and sprints, reports, workflow conditions, bulk targets,
permission checks, exports, notifications, MCP and REST reads of one issue, and any query with `WAS` or `CHANGED` always use
PostgreSQL, as does any query the index cannot answer faithfully (the answer says so by coming from PostgreSQL).

## Never a leak, even while the index lags

1. The index query is limited to the projects the caller can read **now**, from PostgreSQL's current permission data: a
   membership change takes effect at once, without reindexing. Internal comment text is a separate field searched only in
   projects where the caller may see internal comments.
2. The index returns only issue ids. Each id is checked again in PostgreSQL (permissions, trash, moves, security) and all
   displayed data is read from PostgreSQL after that check. An issue moved, deleted or re-secured while the index was behind is
   dropped there.
3. A caller who can read more projects than 1,024 is answered by PostgreSQL, with a warning.

## Freshness

Changes reach the index from the same event stream as everything else, batched. Each write carries an external version (the
event id), so a late, duplicated or replayed write cannot overwrite a newer one. When the index is more than ten seconds
behind, answers carry "search results may be a few seconds behind"; your own just-made changes are overlaid on lists so they
neither vanish nor linger. Writes rejected by the index three times are parked in a dead-letter table (shown in the admin
status) and do not block the rest. A cluster outage never blocks writes: the backlog grows and drains afterwards, and searches
use PostgreSQL meanwhile (with a warning), with the cluster left alone for 20 seconds after a failure.

## Operations (administrators)

| Call | What it does |
|---|---|
| `GET /api/v1/admin/search` | engine, cluster health, active, building and previous index, backlog, lag, dead letters, last reconcile |
| `POST /api/v1/admin/search/rebuild` | online rebuild into a new versioned index while writes continue to both; verifies count and every document's content hash; switches the alias atomically; resumable; keeps the old index for rollback |
| `POST /api/v1/admin/search/rollback` | points the alias back at the previous index |
| `POST /api/v1/admin/search/reindex` `{"project":"ABC"}` | rewrites one project's documents and removes ones that no longer exist |
| `POST /api/v1/admin/search/reconcile` | compares PostgreSQL and the index and repairs missing, stale and orphaned documents (also runs every six hours) |

Use a rebuild for mapping or analyzer changes, corruption, upgrades and restores. A backup of the index is optional: the
recovery plan is a rebuild.

## Not built yet

Real-cluster compatibility runs (the test double is strict but is not OpenSearch), cursors for very deep paging (deep pages
are answered by PostgreSQL), custom-field and attachment-text fields, accent-insensitive substring search on summaries (the
index keeps accents), organization isolation (there are no organizations yet), the exclusion-filter step for callers above the
term limit, TLS and credential rotation checks, and sizing guidance from the 5-million-issue run.
