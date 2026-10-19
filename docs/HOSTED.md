# The hosted edition: plans, quotas and metering

One build runs a self-hosted installation and a hosted service (SPEC 17.9). The hosted parts are off unless the operator sets `hosted=true`
(and, for a deployment that serves one region, `hosted_region`, such as `eu-west`). Off, none of this exists: the plan and usage endpoints answer
not found, nothing is metered, and there are no limits, no licence keys and no calls home.

## Plans

An organization with no plan has no limits. The operator (an instance administrator) gives an organization one:

```
PUT /api/v1/orgs/{id}/plan
{"plan": "team", "max_seats": 25, "max_projects": 10, "max_storage_mib": 20480, "region": "eu-west"}
```

Any limit left out is unlimited. `GET` reads it and `DELETE` removes it. Setting and clearing are audited (`org.plan_set`, `org.plan_cleared`).
An organization's own administrators cannot change their plan. Lowering a limit below what is in use removes nothing; it stops new use until the
tenant is under it.

- **Seats** are active staff accounts. Agents, the portal's internal account, deactivated people and customers are not seats.
- **Projects** are the organization's projects that are not archived.
- **Storage** is the size of the attachments in the organization's projects (an attachment counts once per issue it is on).
- **Region:** a deployment serves one region; a plan that names another is refused, and a plan without one takes the deployment's.

Seats and projects are enforced by the database, so every way of making a person or a project meets the limit (the Admin screens, SCIM,
single sign-on, imports and reactivation), under a lock, so two at once cannot both take the last seat. Storage is checked when a file is
registered. A refusal is `402 plan_limit` with `{resource, limit, used}` and a message that says what to do.

Plans are data: turning `hosted` off does not delete them, and the database keeps enforcing seats and projects for organizations that still have
one, so clear plans before switching hosting off.

## Usage and metering

`GET /api/v1/orgs/{id}/usage` returns what an organization uses now: seats, projects, issues, storage, and the **active customers**
(portal customers who signed in during the last 30 days; metered apart from seats and never counted against them), with the plan. The operator may
ask about any organization and an organization's administrator about their own. A snapshot of every organization is written every hour and kept
one row a day (the day's row is rewritten while the day is open): `GET /api/v1/orgs/{id}/usage?history=30` lists them, newest first, for billing.
