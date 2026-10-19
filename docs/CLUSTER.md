# Running more than one node

Tasklane keeps everything that matters in PostgreSQL, so any number of nodes can serve the same database: a save is committed there before
anything else happens, and the database decides every conflict. What a second node needs beyond that is only to tell the people connected to
*it* what happened on the *others*: live change events and who is looking at which issue. A **Redis or Valkey** server (7 or newer) carries
those. Set it on every node:

```
TASKLANE_REDIS_URL=redis://redis.internal:6379/0        # rediss://user:password@host:6379 for TLS
```

Empty (the default) means a single node and no bus. `tasklane doctor` checks that the server answers, and warns when it finds several nodes
running with no `redis_url`.

## What it carries and what it does not

Change events and presence only, both as hints: nothing stored depends on Redis, nothing is lost when it is gone, and a node never waits for it.
A node publishes without blocking and drops what it cannot send promptly (a late live event is worse than none: pages catch up from the
database). What each node does with Redis down:

| | While Redis is unreachable | When it returns |
|---|---|---|
| Saves, reads, search, mail, imports | Unaffected on every node | Unaffected |
| Live events | A page sees changes made on its own node; changes on other nodes are not pushed, and the page catches up by polling (within about 30 seconds) | Events resume on their own; nothing is replayed |
| Presence | Each node knows only its own people; other nodes' people disappear after their 30 second expiry | Presence resumes with the next heartbeat (10 seconds) |
| `/readyz` | Still ready (a load balancer should not pull the node) with a note: `ready, degraded: the cluster event bus ...` | Plain `ready` |
| `GET /api/v1/admin/cluster` | `events.connected: false`, the last error, counters of what was dropped | `events.connected: true` |

Each node holds one connection to subscribe and one to publish, reconnects with backoff, and re-subscribes by itself. The pause barrier and the
version gates (docs/UPGRADE.md) use the database, not Redis.

## Partitions

If a node cannot reach Redis, or the nodes cannot reach each other, each side keeps serving. Two people on opposite sides editing the same text
from the same version cannot both win: the database accepts one save and the other gets the usual conflict. Nothing needs repairing afterwards.

## Proved against the real services

`make interop` (Docker required) starts two Tasklane nodes with a real PostgreSQL, a real Redis and a real OpenSearch from
`deploy/compose/docker-compose.interop.yml`, with search and the event bus switched on, and runs six basic scenarios: both nodes ready and
`doctor` naming Redis and OpenSearch with their versions; ordinary searches on OpenSearch agreeing with the PostgreSQL engine on the same data
for an administrator and a restricted person; the index following updates, transitions, comments, deletes and moves and surviving a rebuild; two
people on different nodes seeing each other, a save reaching the other node's socket and a stale save getting the normal conflict; a pause on one
node holding a writer on the other; and two browser contexts on different nodes seeing each other while the search box is answered by OpenSearch.
The suite fails, it does not skip, if a service is missing or if a scenario was served by a fallback.

Starting both nodes at the same moment against a new database is safe: a node that finds the schema only partly applied because another node
is migrating waits for it (up to five minutes) instead of refusing to start.

## What a node remembers

A node keeps an organization's security policy (network list, second-factor rule, session limit) and whether a person has a second factor for up to 5 seconds. A change made on one node is announced on the event bus and the other nodes drop their copy at once. Without the bus (not configured, down, or the other node an older release, which ignores the message) the 5-second time-out is the bound: a changed network list is in force on every node within 5 seconds. The pause leases have their own 1-second cache and the barrier of the pause (see above).
