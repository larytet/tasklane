# What happens when things go wrong

What Tasklane does, and what you will see, when a node stops, a disk fills or stalls, a clock is wrong, or the live-update layer is off. Each behaviour has a test (the CHAOS items in the test plan).

## Stopping a node gracefully (SIGTERM)

1. **Readiness fails first.** `/readyz` answers `503 draining` at once, while `/healthz` (is the process alive) still answers `200`. A load balancer that checks `/readyz` moves new requests to other nodes. The node is marked as draining in `GET /api/v1/admin/cluster`.
2. **Open pages are told to reconnect.** Every live-update socket gets `{"type":"reconnect","after_ms":N}` with N random within one second, then is closed; new sockets are refused with `503` and `Retry-After`. The pages reconnect (to another node) at spread-out moments, not all in the same instant. Saves never depended on the socket: they go through REST and keep their drafts.
3. **The delay, then the stop.** After `shutdown_drain_delay` (default 5 s; 0 stops at once) the listener closes and the server waits up to 15 s for requests already in flight, which are answered normally. Then it leaves the cluster and exits with status 0.

For a rolling upgrade, set `shutdown_drain_delay` to a little more than your load balancer's health-check interval.

## A node killed, or the database unreachable
Unacknowledged saves are retried by the page with the same `save_id`, so they apply once; acknowledged saves are in the database. See the durability tests (DUR-01, DUR-02).

## A full or stalled disk (blob store)
- **Full:** an upload is refused with `507` and the code `storage_full` ("the server has run out of storage space: nothing was saved…"), with a `Retry-After`. No attachment row, blob row or staging file is left behind. The same applies when PostgreSQL itself reports a full disk (`53100`). Everything that does not write to that disk (issues, comments, text saves, reading, searching) keeps working. When space returns, the same upload simply works. The readiness check also fails while the blob store cannot be written.
- **Stalled (a disk that stops answering):** uploads waiting on it hold their slots (`MaxUploads`), so the next upload is refused at once with `503` `busy` and a retry hint instead of queueing without limit, and interactive requests are not slowed. If the disk then fails, the stuck upload fails cleanly and frees its slot.

## Clocks that disagree
Every node compares its own clock with the database's every ten seconds and corrects its time by the difference, so leases, expiries and every stored time mean the same on every node. Ordering never used the clocks: comments, history and text versions follow database identifiers and versions. A node whose clock is more than five seconds off is reported in `GET /api/v1/admin/cluster` (`clock_offset_ms`, `clock_warning`) and in its start-up log; it is corrected for, but fix its time synchronisation.

## The live-update layer switched off
With `realtime=false` the server answers `404` `realtime_disabled` on the ticket and socket endpoints; the page asks once and stops. People still appear as present (periodic requests), saves go through REST, and a save based on an old version still opens the conflict dialog. Presence that missed its heartbeats is still removed.

## The campaign through a bad network, on the real binaries

`make chaos` (`scripts/chaos.sh`, Docker required) runs two nodes of the current release with Redis and PostgreSQL, and twelve people appending to three shared
texts with compare-and-set through a network that delays every request and every answer by about 300 ms, loses some requests before they arrive, loses some
answers after the work was committed, and delivers some requests twice. While they work a node is killed with saves in flight and brought back, Redis stops
and returns, and PostgreSQL restarts. At the end the reconciler of SPEC 21 judges each text: no acknowledged save missing (E1), none that replaced a newer
change (E2), none duplicated (E4), and no gap in the sequence. A client that does not reuse its save id on a retry fails it (E4 and gaps), which is how the
measurement is known to be able to fail.

### Packet-level faults

`make chaos` also runs the same campaign with the faults made by [toxiproxy](https://github.com/Shopify/toxiproxy) between the people and each node, at the TCP level and not
in an HTTP wrapper: every packet delayed with jitter in both directions, the answer stream cut into small pieces, 5% of connections reset as they open, and 5% of
answers cut off after 40 bytes. The node kill, the Redis stop and the PostgreSQL restart are in the same run, and the reconciler judges the shared texts the same way.
