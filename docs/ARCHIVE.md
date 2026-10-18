# Archive

Archive is a reversible state for finished work. It is separate from the trash (deleted issues) and from archiving a whole project.

- **Who and what.** Any member of a project can archive an issue that is in a Done status (**Actions → Archive**, or `POST /api/v1/issues/{key}/archive`). Unarchive is `DELETE` on the same address, or the **Unarchive** button on the ticket. Both are recorded in the history.
- **Read-only.** An archived issue cannot be edited, commented on, transitioned, moved or converted by anyone until it is restored; the ticket opens read-only with a banner.
- **Hidden.** Boards, backlogs, typeahead and every search leave archived issues out by default. A query that mentions `archived` includes them: `archived = true`, `archived = false`, `archived is EMPTY`, `archived is not EMPTY`.
- **Still counted.** Reports and epic progress read the issue itself, so archived issues stay in historical numbers.
- **Automatic.** An administrator can set a project to archive issues N days after resolution (`PUT /api/v1/projects/{key}/archive-policy` with `{"days": 90}`, `null` turns it off). The hourly lifecycle job archives them, a few hundred per pass.
- **Search backends.** With OpenSearch, archived issues are removed from the index and come back when restored; a query that asks for archived work is answered by PostgreSQL.
- **Export and import.** The archived time is part of both JSON and CSV bundles.
