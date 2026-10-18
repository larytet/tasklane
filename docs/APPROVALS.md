# Approvals

An approval asks a set of people to say yes or no to an issue. It shows on the ticket (the **Approvals** card), on each approver's **Approvals** page, in the inbox, and in the issue's thread and history.

## Asking

Anyone who can edit the issue can ask (the ticket's card, or `POST /api/v1/issues/{key}/approvals`). The approvers are any mix of:

- people, by id or email address (`users`);
- groups, by id or name (`groups`): every active member;
- `reporter` or `assignee` (`derived`), or the key of a user field on the issue: the people it holds.

The set is fixed when the request is made (a person who joins the group later is not added). Everyone must be able to see the project, otherwise the request could never be answered, and there are at most 25. **One approval is open on an issue at a time.**

The **rule** says when it is decided:

| Rule | Granted when | Rejected when |
|---|---|---|
| `all` (default) | everyone approved | anyone rejects |
| `any` | one approves | everyone rejected |
| `n` (with `n`) | n have approved | n can no longer be reached |

An optional `deadline` brings **one reminder** to those who have not decided, when it is within a day or past; the approval is marked overdue after it. A reminder goes out once however many nodes run.

## Deciding

An approver approves or rejects (`POST /api/v1/approvals/{id}/decision`) with an optional comment. A decision is final. The comment is written into the issue's thread as `Approved: …` or `Rejected: …`, under the approver's name, and the outcome is in the issue's history. **Only people decide**: an agent token is refused, because an approval is a person's word. The person who asked can withdraw a pending request, and so can a project administrator.

## In a workflow

The transition condition `approval_granted` holds a transition until the issue's latest approval (ignoring withdrawn ones) is granted: a later request that is still open, or was rejected, closes the gate again. Add it in the workflow editor's conditions list, or with `{"kind": "approval_granted"}`.

## Notifications

Approvers get an `approval` notification when asked and when a deadline nears; the person who asked gets one when someone decides (and when the outcome is reached). Nobody is told of their own action, and `approval` can be muted like any other kind in the notification preferences. Email follows the same preferences.

## Not built yet

Deciding by replying to the email or from a push notification, a per-approver deadline, delegation while away, approving from a service-desk portal (the customer portal is not built; approvals are never shown to customers), approval of timesheets, the approval step as a node in the visual workflow editor, and consuming a grant when the transition it gated has been made.
