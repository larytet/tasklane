# MCP server: safe writes

Write tools accept an optional **`idempotency_key`** (8 to 128 characters, your choice).

- A retry with the **same key and the same arguments** returns the first result (marked `idempotent_replay: true`) and does nothing again, and does not spend the write budget.
- The **same key with different arguments** is refused, so a key cannot be reused by mistake.
- A call that is still running makes a concurrent retry wait ("still running; retry in a few seconds"); ten simultaneous retries do the work once.
- A call that **failed** releases its key, so retrying it is not blocked by its own failure. Keys belong to the identity that made them and are kept for a day.

## Bulk tools

`bulk_update_fields` (priority, `labels_add`, `labels_remove`, assignee) and `bulk_transition` (a transition by name) act on up to **fifty** issues, named with `keys` or picked with a TQL `query` (a query matching more than fifty is refused: narrow it or work in batches).

- **`dry_run` is on unless you set it to `false`.** A dry run changes nothing and costs no write budget. It shows, for every issue, whether it can be changed, the before and after of each field (or the status move), and why an issue cannot (not found or no access, not allowed from this status, needs a person, forbidden).
- A real run applies that plan **issue by issue**, each change checked against the version that was planned (so an edit made in between is reported on that issue and not overwritten). Every issue gets its own entry in the result and in its history, as the agent.
- A real run spends **one write per issue it changes**, all or none: if the hour's budget cannot cover them, nothing is changed. Pass an `idempotency_key` so a retry cannot repeat it.
- Workflow rules apply as for any change: conditions, validators, screens and field rules, and transitions that need a person are refused for agents (already in the plan).

## Loops

An agent's own writes can set off automation rules. A rule never runs on an event it caused, directly or through other rules, and a rule acts on one issue at most ten times an hour, so a cycle that runs through something outside Tasklane (an agent that answers every automated comment) is stopped by that limit.
