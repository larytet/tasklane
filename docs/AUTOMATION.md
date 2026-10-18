# Automation

A rule is a **trigger**, optional **conditions** and **actions**. Project administrators write rules for their project (**Admin → Automation**); instance administrators can also write rules for every project (`project: ""` through the API). Rules run in the background, as "Automation: <rule name>" in history and comments, through the same operations a person uses: workflow conditions, validators and post-functions, field rules and archive read-only all apply, and a refusal is recorded as a failed run.

**Triggers**

| Type | Parameters |
|---|---|
| `issue.created` | |
| `issue.updated` | `fields`: only when one of these changed (`priority`, `assignee`, `labels`, `customfield_N`...) |
| `issue.transitioned` | `from`, `to`: status names |
| `comment.added` | `contains`: only comments with this text |
| `schedule` | `every_minutes` (5 to 10080) or `daily_at` (`09:00`, with `timezone`), and `query`: the TQL that picks the issues (up to 200 per tick) |

**Conditions** (all must hold): `query` (TQL, such as `priority = high AND labels = release`) and `field` (`field`, `op`, `value`).

**Actions** (in order; a failing action stops the rule and is recorded): `transition` (by name), `assign` (`reporter`, `assignee`, `unassigned`, an email), `set_field` (`priority`, `labels` with `add` and `remove`, or a custom field), `comment`, `create_subtask`, `webhook` (signed, retried, secret stored encrypted, same egress rules as workflow webhooks) and `notify` (an inbox notice for people who may see the issue). Text may use smart values: `{{issue.key}}`, `{{issue.project}}`, `{{issue.summary}}`, `{{issue.status}}`, `{{issue.priority}}`, `{{issue.assignee}}`, `{{issue.reporter}}`, `{{rule.name}}`, `{{trigger.from}}`, `{{trigger.to}}`, `{{actor}}`. Unknown names stay as written; nothing else is evaluated.

**Safety**

- *Once only.* A run is recorded per rule, issue and event; a replay of the event does nothing. A scheduled tick runs once however many nodes are up. An interrupted run is marked failed, not repeated.
- *Loops.* An event caused by a rule carries the chain of rules that led to it. A rule never runs on an event it caused, directly or through another rule, and rules triggering rules stop after three levels; those runs are recorded as `loop`.
- *Limits.* At most 100 rules per project, 10 actions per rule, 300 runs per rule per hour (more are recorded as `throttled`).
- *Dry run.* `POST /api/v1/automation/rules/{id}/dry-run` with an issue key shows whether the conditions pass and what each action would do, and changes nothing.
- *Audit.* Saving and deleting rules is audited; every run is in the rule's run log.
- *Starting point.* A new installation or first start of the automation worker ignores history: rules act on what happens from then on.
