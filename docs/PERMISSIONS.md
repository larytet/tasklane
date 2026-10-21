# Roles, groups and permission schemes

- **Roles in a project:** *viewer* (reads), *member* (works on issues), *admin* (administers the project). Instance administrators are administrators everywhere.
- **Groups.** Administrators create groups and add people (`/api/v1/groups`). Project administrators give a group a role in their project (`PUT /api/v1/projects/{key}/groups/{id}`). A person's role in a project is the **highest** of their own and their groups' roles. Search, lists, notifications, exports, assignee lists and every permission check read the same membership, so a group role works everywhere at once, and removing a person from a group or deleting the group takes it away on the next request. (A project mirrored from Jira keeps its stricter rule: it is visible only while its permission data is fresh.)
- **Permission schemes.** Each project has a scheme (`GET` and `PUT /api/v1/projects/{key}/permissions`, project administrators) that sets the lowest role needed for five actions: `create` (create issues; default member), `comment` (default member), `transition` (default member), `assign` (change the assignee; default member) and `manage_sprints` (create, edit, start, complete sprints; default admin). Open `create` and `comment` to **viewers** for a reporter-style role, raise any of them to `admin`, or open `manage_sprints` to members. `transition`, `assign` and `manage_sprints` cannot be opened to viewers, because they also need edit rights. An administrator can always do everything, so a scheme cannot lock a project out. A permission left out of an update keeps its setting; an empty value goes back to the default.
- Archived issues stay read-only whatever the scheme says, and mirrored projects stay read-only, except those an administrator has set to write through to Jira: there a person's save goes to Jira with their own token, and Jira, not Tasklane's roles, decides whether they may make it (docs/JIRA.md).
- Changes to groups, group roles and schemes are recorded in the audit log.
- Not built: issue security levels, custom roles, global permissions beyond administrator, a scheme per issue type, and a screen for any of this (API only).

## Custom roles

Beside viewer, member and administrator, an organization can define its own project roles (Administration → **Roles**, or `GET/POST /api/v1/roles`, `PUT/DELETE /api/v1/roles/{id}`). A custom role is:

- a **base**, `viewer` or `member`, which is how the rest of the system sees the person (a viewer reads; a member also edits and works), and
- extra **permissions** it grants on top, among those a project's permission scheme can set: `create`, `comment`, `transition`, `assign`, `manage_sprints`.

So a **Reporter** is a viewer who may also create issues and comment; a **Triager** is a viewer who may comment, move issues through the workflow and assign them. Roles with different grants are independent of each other (one can comment and not move issues, another the reverse): they are not rungs of a ladder.

- **Giving one:** use its name where a role is given: a person in a project (`POST /api/v1/projects/{key}/members` with `"role": "Reporter"`) or a group in a project (`PUT /api/v1/projects/{key}/groups/{id}`). Project administrators give roles; only organization administrators define them. A role belongs to its organization and cannot be given in another.
- **Adding up:** a person with several roles (their own and their groups') has the highest base and all the grants.
- **A grant is explicit:** it holds even where the project's scheme asks for a higher role. The scheme still governs everyone without the grant. Setting only the assignee of an issue needs only the `assign` permission; every other edit needs a member's write access whatever is granted.
- **Changes apply at once** to everyone who has the role (a changed base moves their stored role too) and are audited with what the role was. A role still given to someone cannot be deleted.
- A mirrored project's roles come from Jira; custom roles are not given there.
