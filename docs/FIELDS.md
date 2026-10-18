# Custom fields

Administrators define fields; each field has contexts that say where it applies.

- **Types:** text, textarea, number, integer, float, date, datetime, select, radio, multi_select, checkbox_group, user, multi_user, url, labels, duration (`2h 30m`), rating (0-5), issue_picker.
- **Contexts:** global, or by project and issue type. The most specific context wins. A context carries options, a default and a required flag.
- **Options:** can be archived (kept on old issues, not offered) or merged into another option.
- **Roles:** `view_roles` and `edit_roles` limit who sees and changes a field (empty means everyone who can see or edit the issue). Restricted fields are searchable only by administrators.
- **Lifecycle:** a deleted field is in the trash for 30 days and can be restored. `replace` moves values to another field. Converting or moving an issue drops values that no longer apply and keeps them in the removed-values record; the plan shows the count first. Clone copies fields flagged `copy_on_clone`.
- **API:** `GET/POST /api/v1/fields`, `PATCH/DELETE /api/v1/fields/{id}`, contexts and options under `/fields/{id}/contexts`, `GET /api/v1/projects/{key}/fields?type=` for the form definition. Issues accept `custom` (key `customfield_<id>` or the field name; `null` clears) on create and PATCH.
- **Search:** `cf[123] = x`, `"Severity" = High`, `ORDER BY "Estimate"`, and `was` / `changed` for fields that track history.
