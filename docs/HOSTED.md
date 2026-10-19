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

## Prices and invoices

Tasklane does not take payment; it prices what an organization used and hands the invoice to whatever bills the customer.

```
PUT  /api/v1/orgs/{id}/prices                      {"currency":"eur","price_base_cents":2900,"price_seat_cents":850,"price_gib_cents":50,"price_customer_cents":25}
POST /api/v1/orgs/{id}/invoices                    {"period":"2026-09"}
GET  /api/v1/orgs/{id}/invoices                    (and /{number}, or /{number}?format=csv)
POST /api/v1/orgs/{id}/invoices/{number}/send      hand it to the billing endpoint
POST /api/v1/orgs/{id}/invoices/{number}/void      {"reason":"..."}
```

Prices belong to the plan (set a plan first) and are in whole cents; they are zero until set. An invoice covers one calendar month and is made from the daily
usage snapshots of that month: the **base** price, the **average seats** over the days recorded, the **average GiB** of attachments, and the **most active
customers** in the month (customers are priced apart from seats). Each line is rounded half up on its own. The invoice keeps the plan and prices it was made
under and is never recalculated; there is one live invoice per organization and month, and a voided one (with a reason) lets the month be invoiced again.
Numbers are `TL-YYYYMM-NNNN`.

With `billing_webhook_url` (and `billing_webhook_secret`) set, `send` posts the invoice as JSON, signed with HMAC-SHA-256 of the body in
`X-Tasklane-Signature`, with the number as `Idempotency-Key` so the receiver can recognise a repeat; a failed delivery (502) leaves the invoice open to send
again. The operator makes, sends and voids; an organization's administrators read their own. Creating, voiding, sending and setting prices are audited.

## Support levels, upgrade channels and managed maintenance

The operator upgrades and maintains tenants. A plan carries the terms:

```
PUT /api/v1/orgs/{id}/terms   {"support_level":"standard","upgrade_channel":"early","window":{"weekday":0,"start_hour":2,"hours":2}}
GET /api/v1/orgs/{id}/terms
```

The **support level** (`community`, `standard`, `premium`) sets the notice a tenant is promised before planned work: none, 24 hours, 72 hours. The **upgrade
channel** (`stable`, `early`) says which releases the operator moves the tenant to first; it is recorded for the operator's rollout tooling. The **window** is
the weekly span, in UTC, in which the operator may work on this tenant (none: any time).

```
POST /api/v1/orgs/{id}/maintenance                  {"kind":"upgrade","starts_at":"...","ends_at":"...","message":"..."}
GET  /api/v1/orgs/{id}/maintenance
POST /api/v1/orgs/{id}/maintenance/{id}/cancel      {"reason":"..."}
```

A notice is refused unless the work lies inside the window, starts after the promised notice, lasts at most eight hours, and does not overlap other announced work
for that tenant. The tenant's people see the next one in a banner from two weeks before until it is over (`scheduled` in `GET /api/v1/system/status`), and a
cancelled one disappears with its reason kept in the audit log. The notice announces; the work itself is done with the pause (docs/BACKUP.md) and the
rolling upgrade procedure (docs/UPGRADE.md). The operator sets terms and notices; a tenant's administrators read their own.

## Maintenance that does what it announces

A maintenance notice is not only a banner. When its time comes the first node to find it due (every node looks every ten seconds) takes a **pause on the
tenant's organization**: the organization's writes wait a moment and are then refused with `503` and `Retry-After` while reads, drafts and presence carry
on, other tenants are not touched, and the pause ends at the work's end by itself, or at once if the work is cancelled. A notice made with
`"hold_writes": false` only announces. If the writes cannot be drained in time, or another pause is in the way, the claim is released with the reason on the notice
(`last_error`) and tried again while the window lasts. Starting and finishing are audited (`maintenance.started`).

Pauses accept three scopes: `instance`, `org:<organization id>` and `project:<KEY>`; scopes that cover any of the same writes cannot be held at once.

## Invoices on schedule

Once a month ends, the first node to look (every hour) makes the invoice of the month just past for every organization that has a plan and a price and
recorded usage, and delivers it if a billing endpoint is configured; an invoice that could not be delivered is delivered at a later look, and a month that
already has a live invoice is left alone.
