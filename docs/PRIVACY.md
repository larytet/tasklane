# Personal data, erasure and retention

## Export (access request)

`GET /api/v1/admin/users/{id}/personal-data` (administrators; recorded in the audit log) returns, as one JSON file, everything held about a person: profile, project memberships, issues they reported or were assigned, comments and attachments they wrote, the history of changes they made, notifications, what they watch, sessions (address and browser), API tokens (names only), connected applications, mail sent to them, notification preferences, whether two-factor authentication is on, and the audit entries made by them.

## Erase

`POST /api/v1/admin/users/{id}/erase` with `{"delete_content": false, "scrub_mentions": true}` (administrators; recorded):

- The account becomes "Deleted user" with an `@erased.invalid` address, no password, inactive, not an administrator. Their credentials, two-factor secret, sessions, tokens, connected applications, single sign-on identities, notifications, watches, preferences, memberships, mail log and sign-in records are removed.
- Issues are never deleted. What the person did stays, under the pseudonym: history and text-version entries and the events behind them name "Deleted user", the comments they wrote stay (unless `delete_content`, which also deletes their comments and attachments and clears the history lines about their files).
- `scrub_mentions` replaces their name, their address and `@theirname` with "[deleted user]" in everyone's comments, descriptions, text versions, summaries, history lines, notifications and queued events.
- The **audit log** keeps its entries, but their name and address are removed from the entries that are theirs, and the hash chain is recomputed from the first changed entry so verification still passes. This is the only operation allowed to change an audit row, and it is itself recorded ("privacy.erased", with counts and no personal details).
- The search index is rebuilt for every issue whose text or participants changed (events are queued for the indexer; with OpenSearch the documents follow the same events).
- It can be repeated: an erased person stays erased.
- It **refuses** when the person's records sit on an issue under **legal hold** (it names the issues: release the hold first), when the person is the last administrator, and when you try to erase yourself.

A test searches every text and JSON column of every table for the person's address, name and other identifiers after an erase.

Limits you should know: only their name, address, `@name` and the identifiers above are searched for in other people's free text, so a nickname or a phone number typed into a comment is not found; backups taken before the erase still contain the person (restore them only into an environment that replays erasures, and keep backup retention short); copies a person exported or received by email are outside the system.

## Retention

The hourly lifecycle job removes records that outlived their purpose: ended sessions, pending sign-ins, spent authorization codes and sign-in states, read or archived notifications older than 180 days, automation runs older than 90 days, finished webhook jobs after 30 days and failed ones after 90, sign-in attempt records after a day, tokens and revoked grants 90 and 30 days after they ended. Deleted issues stay restorable for 30 days and are then purged unless on legal hold (see the lifecycle documentation); the audit log is kept.
