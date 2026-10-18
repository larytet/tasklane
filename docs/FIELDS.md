# Custom fields

Administrators define fields; each field has contexts that say where it applies.

- **Types:** text, textarea, number, integer, float, date, datetime, select, radio, multi_select, checkbox_group, user, multi_user, url, labels, duration (`2h 30m`), rating (0-5), issue_picker.
- **Contexts:** global, or by project and issue type. The most specific context wins. A context carries options, a default and a required flag.
- **Options:** can be archived (kept on old issues, not offered) or merged into another option.
- **Roles:** `view_roles` and `edit_roles` limit who sees and changes a field (empty means everyone who can see or edit the issue). Restricted fields are searchable only by administrators.
- **Lifecycle:** a deleted field is in the trash for 30 days and can be restored. `replace` moves values to another field. Converting or moving an issue drops values that no longer apply and keeps them in the removed-values record; the plan shows the count first. Clone copies fields flagged `copy_on_clone`.
- **API:** `GET/POST /api/v1/fields`, `PATCH/DELETE /api/v1/fields/{id}`, contexts and options under `/fields/{id}/contexts`, `GET /api/v1/projects/{key}/fields?type=` for the form definition. Issues accept `custom` (key `customfield_<id>` or the field name; `null` clears) on create and PATCH.
- **Search:** `cf[123] = x`, `"Severity" = High`, `ORDER BY "Estimate"`, and `was` / `changed` for fields that track history.

## Screens, field configurations and behavior rules

- **Screen:** named tabs of fields. Built-in keys are `description`, `priority`, `assignee`, `labels`; custom fields are `customfield_<id>` (`cf[<id>]` is accepted and stored canonically).
- **Screen scheme:** maps an operation (`create`, `edit`, `view`) and optionally an issue type to a screen; the entry for the type beats the default entry. A project picks one scheme (`PUT /api/v1/projects/{key}/screen-scheme`, `null` clears). With no scheme every applicable field is on the form. With a scheme, setting a field that is not on the screen for that operation is refused.
- **Field configuration:** `required`, `hidden` and help text for one field at one scope. The most specific scope wins per attribute: project and type, then project, then type, then global. A configuration can make a field required but cannot relax a context's required flag.
- **Behavior rules:** an optional condition on another field (`eq`, `ne`, `in`, `empty`, `notempty`) and an optional role, then `show`, `hide` or `require` on a target field. Rules run in position order, later `show` and `hide` win, `require` accumulates. The server enforces them: a required field cannot be missing or cleared, a field hidden at that moment cannot be set. The same rules go to the browser in the form definition so the dialog reacts as values change.
- **Form definition:** `GET /api/v1/projects/{key}/form?type=&op=create|edit|view` returns the tabs, each field's flags, options and defaults, the person's role and the rules.
- **Checks on edit** cover the changed fields and the fields a changed value made required, so an old issue is not blocked by a rule added later.
