# Audit log

Every administrative change is written to the audit log. **Admin → Audit log** shows it (administrators only), with filters by person and action, paging, a check of the chain, and a CSV export.

- **What is recorded.** The domain writes a record, inside the transaction of what it describes, for lifecycle operations, imports, exports, workflow publishing, screens, fields, pausing and similar. On top of that, the API writes a record for **every successful request that changes something and is not ordinary work on issues**: who, which route, the address and status, from where (session or token) and the path. A new route is therefore audited unless someone adds it, on purpose, to the short list of ordinary-work routes in `internal/api/audit.go`; a test keeps that list free of stale entries and another walks the authorization matrix and checks that every administrator-only change leaves a record. Failed and refused requests leave nothing.
- **Append-only.** The database refuses `UPDATE`, `DELETE` and `TRUNCATE` on `audit_event`.
- **Tamper-evident.** Each entry carries the hash of the one before it (one lock keeps the chain in commit order). **Verify the chain** (or `GET /api/v1/admin/audit/verify`) recomputes every hash and reports the first entry that was changed, does not follow its predecessor, or is missing a neighbour. A database superuser can still switch the triggers off, but cannot hide that they did; for a stronger guarantee export the log on a schedule to storage the database administrator cannot write to.
- **Export.** `GET /api/v1/admin/audit/export` (CSV, formulas neutralized for spreadsheets); the export is itself recorded.
- **Older entries.** Entries written before the chain existed are kept and counted as "unchained".
