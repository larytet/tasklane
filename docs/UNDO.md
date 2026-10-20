# Undo

Reversible actions happen at once and offer **Undo** for 5 seconds instead of asking first (SPEC 4.6). The toast stays while
the pointer is on it or it has keyboard focus.

| Action | Undo does |
|---|---|
| A transition (the Transition list, `e`) | puts the issue back in the status it had, and its resolution date |
| Archive | brings the issue back from the archive |
| Delete to the trash (when nothing blocks it and nothing is dropped) | restores it from the trash with its key, as Restore in the Trash does |

A delete whose plan has blockers or would drop data, and every move, clone and type change, still show their plan first.

## Rules

- **Only the person who acted**, from any of their tabs or sessions, and only once. A token that is used, expired (5 minutes on
  the server) or someone else's answers 404 and says nothing more.
- **Only if nothing changed since.** If anyone (or automation) changed the issue after the action, commented on it, or edited its
  text, Undo changes nothing and says so (409 `changed_since`). Watching and viewing do not count.
- **A transition is undone even where the workflow has no way back**, or a condition would refuse the move: it corrects the
  person's own action of a moment ago and is not a new transition. No conditions, validators or post-functions run. History shows
  the status change and the audit log records `issue.undo`; the event sent out is `issue.updated` with `"undo": true`, not
  `issue.transitioned`, so rules on transitions do not fire for it and nobody is notified a second time.
- **Automation that already ran is not reversed.** Post-functions of the original transition and rules it triggered (a comment,
  a field set, a webhook) stay as they are.

## API

Send `Tasklane-Want-Undo: 1` with `POST /api/v1/issues/{key}/transitions`, `POST /api/v1/issues/{key}/archive` or
`POST /api/v1/issues/{key}/lifecycle` (op `delete`); the answer carries the token in `Tasklane-Undo` and its lifetime in seconds in
`Tasklane-Undo-Expires`. `POST /api/v1/undo/{token}` undoes it and answers what was put back. Requests without the header leave
no token. Tokens are removed by the hourly clean-up once expired, and with the person on erasure.
