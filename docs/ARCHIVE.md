# Archive

Archive is a reversible state for finished work. It is separate from the trash (deleted issues) and from archiving a whole project.

- **Who and what.** Any member of a project can archive an issue that is in a Done status (**Actions → Archive**, or `POST /api/v1/issues/{key}/archive`). Unarchive is `DELETE` on the same address, or the **Unarchive** button on the ticket. Both are recorded in the history.
- **Read-only.** An archived issue cannot be edited, commented on, transitioned, moved or converted by anyone until it is restored; the ticket opens read-only with a banner.
- **Hidden.** Boards, backlogs, typeahead and every search leave archived issues out by default. A query that mentions `archived` includes them: `archived = true`, `archived = false`, `archived is EMPTY`, `archived is not EMPTY`.
- **Still counted.** Reports and epic progress read the issue itself, so archived issues stay in historical numbers.
- **Automatic.** An administrator can set a project to archive issues N days after resolution (`PUT /api/v1/projects/{key}/archive-policy` with `{"days": 90}`, `null` turns it off). The hourly lifecycle job archives them, a few hundred per pass.
- **Search backends.** With OpenSearch, archived issues are removed from the index and come back when restored; a query that asks for archived work is answered by PostgreSQL.
- **Export and import.** The archived time is part of both JSON and CSV bundles.

## Bulk operations

`POST /api/v1/bulk/lifecycle` takes `{"keys": [...], "query": "TQL", "op": {"op": "delete|restore|clone|move|convert", ...}}` (the `op` object is the same as for one issue; purge stays one at a time) and answers `202` with a job: `{"id", "state", "total", ...}`. A worker on any node does it, one issue at a time, as the person who started it, so every issue is checked against that person's permissions: one they cannot read is refused when the job is made (`not found`, as for an issue that does not exist), and one they can read but not change fails with the reason when it is reached. `GET /api/v1/bulk/{id}` shows the progress (`done`, `failed`, the reasons of the first fifty failures, and an `eta` estimated from the rate so far); `POST /api/v1/bulk/{id}/cancel` stops it after the issue in flight, and what was done stays done.

A job survives the death of its worker: each issue is marked finished in the same transaction that does the operation, so nothing is done twice and a crash costs at most the issue in flight; another node takes the job over when the lease of the dead one (one minute) has run out. A pause on a project stops the job where it is, without failing anything, and it goes on after the pause. At most 50,000 issues in one job.
