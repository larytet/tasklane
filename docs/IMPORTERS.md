# Importing from GitHub Issues, Trello, Linear and Asana

Open **More, Import** (`#/import`), choose the project, the tool and the tool's export file. Tick **Dry run first** to see what would happen: the report says how many issues and comments would be made and what could not be mapped, and nothing is changed. **Import for real** runs it; **Roll this import back** removes what that import made. The same thing over the REST API: `POST /api/v1/import/{github|trello|linear|asana}` with `{"project": "ABC", "data": "<the export's text>", "dry_run": true, "user_map": {...}}` (project administrators; up to 64 MB).

"Basic support" means issues, comments, labels, assignees, status, dates and the links the source offers. No automation, boards or permissions are moved, and no files are fetched. Importing is into an **existing** project and **adds** to it; **importing the same file again updates what the first import made** (every issue and comment carries the tool's own id), it does not duplicate. A problem with one row or card is reported and that one is skipped; the rest are imported.

## People

The exports name people by login or name, not by address. Say who is who with `user_map` (on the screen: one line per person, `octocat = octo@example.com`). A person the map does not cover is **reported once and left out** (an unassigned issue, a comment with no author), never guessed. Where the export has addresses (Asana's "Assignee Email", Linear when it exports addresses) they are used. People are created as inactive placeholders that claim their history when the real account is made.

## Statuses

The source's state is mapped to **your project's workflow**: a state whose name is already one of your statuses is used as it is; otherwise the words in it decide a category (done, closed, completed, shipped, canceled... mean Done; doing, in progress, review, testing... mean In Progress; anything else To Do) and the **first status of that category** in the workflow is used, so a workflow with other names still works. `status_map` (source state to your status) overrides this; naming a status your workflow does not have is reported and the nearest one is used.

## What each tool maps to

| Tool | Input | Maps to |
|---|---|---|
| **GitHub Issues** | the REST answer of `GET /repos/OWNER/REPO/issues?state=all` (a JSON array), or `{"issues": [...], "comments": [...]}` with the answer of `GET /repos/OWNER/REPO/issues/comments` | open and closed to status and resolved date; labels (`bug` or `defect` also makes the type Bug; `type_map` maps others; `priority: high` becomes the priority); assignee and author through `user_map`; comments by issue; **milestones to versions** (a closed milestone is released, with its due date) with the issues in them; the issue's link in the description; **pull requests are skipped** and counted |
| **Trello** | a board's JSON export | lists to statuses, cards to issues (the creation time is read from the card id), **checklist items to Subtasks** (complete means Done), labels (the colour when it has no name), the first member as the assignee and the others named in the description, `commentCard` actions to comments, the card's link, due date and **attachments** (links; the names of uploaded files, which are not copied) in the description, **archived cards as Done** with the label `trello-archived` |
| **Linear** | Linear's CSV export | state, priority (Urgent is highest, No priority medium), estimate, labels, created, updated, completed and canceled dates; **projects to Epics** over their issues (off with `"epics": false`); a sub-issue is a Subtask; a **cycle** is kept as a label `cycle-N` (a sprint belongs to a board) |
| **Asana** | Asana's CSV export of a project | sections to statuses, assignee by address, tags to labels, completion to Done and the resolved date, notes, the due date in the description, a task with a **parent task** is a Subtask (when the parent is not in the file it is imported as a task and said so) |
| GitLab, Azure Boards, ClickUp | the generic CSV importer (`POST /api/v1/import/csv`) with a column map | basic fields only |

## Presets are data

The whole mapping is the defaults above plus what the request sends (`user_map`, `status_map`, `type_map`, `priority_map`, `epics`), and what the request sends wins. For anything else use the generic CSV importer, whose column mapping can be saved as a template.

## Not built yet

Fetching from the tools' live APIs (the file is the input), a generic JSON importer with a JSONPath mapping, file attachments, GitHub pull request links as development links, Linear cycles as sprints, Trello's board columns as a Tasklane board, and a screen to edit presets.

## Any JSON, through a path mapping

`POST /api/v1/import/json` (and the Import screen, "Generic JSON") reads any JSON file. The request carries the file as `data` and a `mapping`:

```json
{
  "project": "ABC", "dry_run": true, "data": "...the file...",
  "mapping": {
    "items": "$.data.tickets",
    "fields": {"key": "ref", "summary": "fields.title", "description": "fields.body", "type": "fields.kind", "status": "fields.state",
               "priority": "fields.prio", "assignee": "fields.who.login", "labels": "fields.tags", "created": "fields.opened",
               "updated": "fields.touched", "resolved": "fields.closed", "parent": "parent", "estimate": "fields.points"},
    "comments": "notes",
    "comment_fields": {"id": "nid", "author": "by", "body": "text", "created": "at"},
    "label_sep": ",", "default_type": "Task"
  },
  "status_map": {"Working": "In Progress"}, "type_map": {"Defect": "Bug"}, "priority_map": {"P1": "highest"}, "user_map": {"ana": "ana@example.com"}
}
```

`items` says where the issues are (an array, or several arrays: `$..tickets` reads every `tickets` array in the file); every other path is read
inside one issue (comment paths inside one comment). `key` and `summary` are required, and a comment mapping needs `body`. A path is the
small safe subset of JSONPath: `$`, `.name`, `['name']`, `[n]` (negative from the end), `[*]` and `.*`, and `..name`; no filters or scripts,
at most 16 steps. A label path may select an array or a string (split on `label_sep`). Dates may be text (RFC 3339 and common forms) or numbers
(Unix seconds or milliseconds). Everything else works as for the other importers: `user_map`, `status_map`, `type_map` and `priority_map`
decide what the file's words become, an item that cannot be read is reported and the rest go in, importing the same file again updates in place
(an issue is `json:KEY`; a comment without an `id` is identified by its position), a dry run changes nothing, and an import can be rolled back.
