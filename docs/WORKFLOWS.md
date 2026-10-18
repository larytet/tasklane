# Workflows

A project uses one workflow: statuses (each in a category: To Do, In progress, Done) and transitions. Administrators edit workflows under **Admin → Workflows**.

## Editing: draft, check, publish

- **Draft.** "Edit workflow" copies the live workflow into a draft. Nothing changes for anyone until it is published. A diagram and the lists show the same draft; the lists are the accessible equivalent of the diagram.
- **Check.** Errors block publishing: no status, duplicate or empty names, a bad category, no To Do status, a transition to or from something that is not a status, a transition listed twice, invalid rules. Warnings do not: a status that cannot be reached from the first status, a status with no way out, and a first status that is not To Do.
- **Publish.** Statuses and transitions that survive keep their ids, so issues, boards and links stay attached (renames and swaps are safe). A draft started before someone else published is refused. Every published version is kept.
- **Removing a status.** If issues are in it, publishing asks where they go. The status is *retired*: nothing can move into it, new issues do not start in it, and an issue still in it behaves as if it were in the status it is mapped to (it is offered that status's transitions). A background job moves the issues in batches of 500, each batch in its own transaction, writes a history row for each ("workflow migration"), shows progress in the editor, and deletes the retired status when it is empty. If the server stops, the job resumes from what is left. A status that is the target of a running migration cannot itself be removed until it finishes.
- **Templates and JSON.** Templates: simple, software, service desk, approval. A workflow is exported as JSON (no ids, no secrets) and can be imported by creating a workflow from a definition.
- **Projects.** A project that has no issues can be pointed at another workflow (`PUT /api/v1/projects/{key}/workflow`). A project with issues keeps its workflow; edit that workflow instead.

## Transition rules

Each transition can carry rules, edited in the transition's **Rules** dialog (JSON lists, checked when saved).

| Kind | Name | Parameters |
|---|---|---|
| Condition | `role` | `roles`: viewer, member, admin |
| Condition | `assignee_only`, `reporter_only`, `assignee_or_reporter` | |
| Condition | `field` | `field`, `op` (eq, ne, in, empty, notempty), `value` |
| Condition | `subtasks_resolved` | |
| Validator | `field_required` | `field`, optional `message` |
| Validator | `regex` | `field`, `pattern`, optional `message` |
| Validator | `permission` | `role`, optional `message` |
| Validator | `previous_status` | `statuses`, optional `message` |
| Post-function (in the transition's transaction) | `set_field` | `field` (priority, assignee, labels or a custom field), `value` |
| Post-function (in the transaction) | `assign` | `to`: reporter, actor, unassigned or a user |
| Post-function (in the transaction) | `add_comment` | `text` (`{{from}}`, `{{to}}`, `{{key}}`), optional `internal` |
| Post-function (asynchronous) | `webhook` | `url`, optional `secret` |
| Post-function (asynchronous) | `create_subtask` | `summary`, optional `type` |

- **Conditions** decide whether the transition is offered and allowed; a transition whose conditions fail is not listed, and calling it directly says why it is not available.
- **Validators** run when the transition is taken and refuse it with a message.
- **In-transaction post-functions** are atomic with the status change: if one fails, the status, the earlier post-functions and the comment are all rolled back.
- **Asynchronous post-functions** are queued in the same transaction and delivered by a worker with retries (growing delay, eight attempts, the last error kept). A webhook is a POST of `{event, key, project, from, to, actor, at}` with `X-Tasklane-Signature: sha256=<HMAC of the body with the secret>`; the secret is stored encrypted, and private or loopback targets are refused unless `webhook_allow_private` is on. `create_subtask` does nothing if a subtask with that summary already exists, so a retry never duplicates it.
- **Transition screen:** the `screen` list names the fields a person is asked for before the transition. The interface saves them as an ordinary edit and then takes the transition, so a validator sees them; the two steps are not one transaction.
- **Concurrency:** two people taking the same transition at once: one wins and the other is told it is not available from the current status.
