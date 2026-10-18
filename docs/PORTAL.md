# Customer portal: sign-in, request types and requests

The portal is a small page at `/portal/PROJECT` for people outside the organization who raise and follow requests. It is its own page with its own few lines of script, none of the staff app. **Customers are not staff**: a separate identity, separate sessions, never a licensed seat.

## Turning a portal on

A project administrator sets the portal up (`PUT /api/v1/projects/{key}/portal`):

| Field | Meaning |
|---|---|
| `enabled` | off by default; an off portal answers as if it did not exist |
| `welcome` | shown on the sign-in page |
| `privacy_notice`, `consent_text` | shown on the sign-in page and after sign-in |
| `allow` | when not empty, only these addresses and `@domains` can sign in |
| `block` | these never can; a block wins over an allow |

The server must be able to send email (`smtp_host`): a code is the only way in unless the customer has chosen a password.

## Signing in

The customer gives an email address. A **six digit code** and a **link** are mailed to it; each works **once**, for **10 minutes**; asking again cancels the earlier ones; **five wrong tries** spoil a code. The first successful sign-in verifies the address and creates the customer. A customer may also set a **password** (the password policy applies) and sign in with that. The link's token is removed from the address bar at once.

Nothing says whether an address is known, blocked or not allowed: the answer to "send me a code" is the same, and no mail is sent for a blocked or unlisted address. Wrong code and wrong password give the same words.

## Isolation

A customer session is a different table and a different cookie (`tl_portal`) from a staff session (`tl_session`). Staff authentication never reads it and portal authentication never reads a staff cookie or token. A customer session cannot open the staff app, the REST API or MCP; a staff session or token opens nothing of the portal. The routes are in the authorization matrix with every staff role getting `401` on the portal's own routes. Sessions last 12 hours.

## Abuse controls

- Codes: 5 per address and 30 per network address per hour (an answer of `429`).
- Failures: 10 wrong codes or passwords per address per hour lock verification for the address, whatever code is then sent; a flood of bad link tokens from one network address is limited the same way.
- A hidden form field (the honeypot): filled in, the request looks successful and sends nothing.
- A block list and an allow list, and **Block** for one customer (ends their sessions).
- State-changing requests need the `X-Requested-With` header, so another site cannot start a sign-in in someone's browser.

## Request types

A **request type** is a form the portal offers (project administrators manage them on the *Customer portal settings* page, reached from Releases, or with `/api/v1/projects/{key}/request-types`): a name, a description and help text for the customer, the **issue type** the request becomes (not a subtask), and the **custom fields** it asks for (text, long text, number, choice, link, date, checkbox; each with a label, help and whether it is required). A form can be hidden without deleting it. Deleting a form leaves the requests already raised through it.

## Requests

A customer picks a form, fills it in and sends it. The request is an **ordinary issue** in the project (so staff work it with the board, workflow, automation and search they already have), raised by a hidden per-organization account that never appears in lists of people and cannot sign in; the customer is attached to it. Staff see a banner on the ticket (who raised it, through which form, their address) and, when replying, a line that says **who will read it**: a reply that is not marked *Internal* is shown to the customer in the portal and an internal one is not.

Only the form's own fields can be set, whatever a client sends. At most 10 requests and 60 comments an hour per customer; a hidden field (honeypot) drops a request.

## What a customer sees

Their own requests, and for each: the summary, the description they wrote, the status name, the form's fields with the labels they saw, the comments that were not marked internal (as **Support**, never a staff member's name), and their own comments (as **You**). Not the assignee or reporter, internal comments and fields, work logs, approvals, links, development data, history, or anyone else's requests. A request of another customer, an issue no customer raised, a deleted request and a key that does not exist all answer the same: not found. A customer of another organization cannot use the portal at all.

A customer's comment goes into the issue's thread (labelled *customer*) and tells the people who watch the issue, like any comment.

## Not built yet

Still to build: attachments on requests, queues, canned responses, SLA clocks, customer organizations that share requests, customer emails and replies by email, promotion to staff, export and erase of a customer's data, sign-in with a customer OIDC or SAML provider, CAPTCHA, and per-request-type step-up. A project key written as `me` (lower case) in the API path is read as the portal's own `me` route.
