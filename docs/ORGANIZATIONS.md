# Organizations (tenant isolation)

An installation starts with one organization, **Default**, and behaves as a single-organization install until a second one is created. A second organization is a separate tenant on the same instance: its people, projects, issues, attachments, search results, realtime channels and agents are invisible to everyone outside it.

## Who is who

| | can do |
|---|---|
| **Instance administrator** (every administrator that existed before organizations, and any administrator made in the Default organization) | everything, as before: global settings (workflows, fields, screens, SSO, mail, storage, audit), creating organizations, and reaching every organization's data as the operator |
| **Organization administrator** (the first administrator of a new organization, and any administrator they make) | administers their own organization only: they are a project administrator in each of its projects, can create projects, people and agents in it, and see its projects, people and search results. They cannot use instance-level settings and cannot reach any other organization |
| **Member, viewer** | what their project roles allow, inside their organization |

Create an organization with `POST /api/v1/orgs` (`name`, `admin_email`, `admin_name`, `admin_password`), list them with `GET /api/v1/orgs` (instance administrators), and see your own with `GET /api/v1/me/org`. An instance administrator can create a person in a named organization with `POST /api/v1/users` and `org`; an organization administrator's people always join their own.

## How isolation is enforced

- **The membership view.** Access to a project is decided by one view (`effective_member`, direct roles and group roles together). It joins a person to a project only when both are in the same organization, so every path that asks "may this person see this project" (issues, comments, boards, sprints, attachments, search, typeahead, realtime channels, exports, MCP tools, notifications and email) is bounded by it.
- **Triggers.** The database refuses a project membership, a group membership, or a group role that would cross organizations.
- **Administrators.** An organization administrator is a project administrator only in projects of their organization.
- **Lists.** The people list, the project list and the search scope are limited to the organization. Adding someone from another organization to a project is refused with the same answer as for a person who does not exist.
- **Agents and tokens.** An agent belongs to the organization of the administrator who made it, and an organization administrator can issue tokens only for their own organization's people and agents. Agent names are per organization.
- **Email.** Mail from a person of another organization to a project address or an issue key is quarantined, and releasing it does not act across the line.

## What is shared, and not yet isolated

Be clear about the limits of this first version:

- **Global configuration objects are shared**: workflows, custom fields, screens, notification schemes, issue types, link types and priorities are one set for the instance, edited by instance administrators. A tenant's issues use them but cannot edit them.
- **Project keys are global.** Two organizations cannot both have `ABC`, and creating a key that another organization holds is refused with a message that tells you the key is taken.
- **Groups** are created by instance administrators in the Default organization; there is no organization-level group administration yet.
- **Single sign-on, SCIM, the Jira mirror, Git connections, mail sources and OpenSearch are instance-level**; people provisioned through SCIM or SSO join the Default organization.
- **The audit log is the instance's**; an organization administrator cannot read it, and entries do not carry an organization.
- **The instance administrator is the operator** and sees every organization. A hosted edition that must keep the operator out of tenant data needs more than this.
- Quotas, plans and metering per organization (HOST-01) are not built.

## How it is tested

`TestSEC09_CrossTenantSweepEveryRouteAsAPersonOfAnotherOrganization` sends every request in the authorization matrix (the whole API) as a member and as the administrator of a second organization, against the first organization's objects, and fails if any answer contains the first organization's data or any table of the first organization changes. `TestSEC09_TheSweepCanFail` runs the same probes as a member of the first organization to show the check does see leaks and changes. Further tests cover the rules above, realtime, MCP and inbound mail.
