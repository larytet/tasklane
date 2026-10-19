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
