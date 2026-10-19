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

## Service level clocks

A request type can carry a **policy**: a goal for the **first response** and one for the **resolution** (minutes), the **working calendar** the minutes count in (a work week preset, time zone, the time the day starts, hours per day, holidays; with none, every minute counts), and the **statuses that stop the clocks** (for example *Waiting for customer*). Each request raised through the type gets a clock per goal, started when it was made, **keeping the policy it started under** (changing a request type later does not move running clocks).

- The **first response** is met by the first public reply from staff (an internal note, the customer's own comment and the hidden portal account do not count). Resolving the request also answers it.
- The **resolution** is met when the request is resolved (a Done status). It stops while the request is in a stopping status and **while it sits resolved**, so a request reopened later is not charged for that time.
- Working calendars count minutes inside the working window only: weekends, holidays and the hours outside the day count nothing. A day's window is its start plus its hours, so on a day the clocks change it is that many real hours after the start.

A clock is **running**, **paused**, **met** or **breached**; one that was met after its goal shows as **met late**. The state is worked out from the facts (the status history, the first public reply, the resolution time), never from events seen, so it can always be recomputed and a missed event only delays a result. It is refreshed when the request is commented on or moves, when staff open it, and every minute for open clocks. When an open goal is missed the assignee (or, with none, the project's administrators) gets **one** notice (kind `sla`, mutable in the preferences).

Staff see the clocks on the request's ticket; **customers never see them**. In search: `sla.state = breached`, `sla.firstResponse.state in (running, paused)`, `sla.resolution.state = met`.

## Customer organizations and sharing

A **customer organization** is a company: a name and the customers who belong to it. A portal's administrators manage them on the *Customer portal settings* page or with `/api/v1/projects/{key}/portal/customer-orgs` (create, delete, add and remove members by address; the portal's allow and block lists apply). A person who has never signed in gets their record made when they are added; deleting an organization leaves its people as customers with no company.

A request is **private to the customer who raised it** until they choose, on the request, to share it:

- with **everyone in their customer organization** (one checkbox), or
- with **named people** (by address, up to 20 at a time; the lists apply; they need not belong to the company).

The people it is shared with see that request and its public conversation and **nothing else of the owner's** (not their other requests, not their details). They can comment; each sees the others by name (and themselves as *You*), and sees who raised it. **Only the owner changes who sees it.** Access follows the facts and is checked on every read: taking the sharing back, removing a person, a person leaving the company, or deleting the company ends access at once. Staff see the company and the other people on the ticket's banner. People shared with are shown each other's address: sharing is the owner's decision.

## Email to and from customers

A customer is emailed (from the server's outbound mail, when it is configured):

- **once when their request is received**, with its key and a link;
- when **staff reply publicly** (the text of the reply, introduced as "Support", never a staff name; internal notes are never mailed);
- when the request is **resolved**;
- when someone they **share a request with** writes on it (told by that person's address, never their own comment back).

Every message carries only what the portal would show them and a link to the request in the portal. Only customers who have **signed in at least once** are written to (so an address someone else typed in is never mailed), and the check that the customer still sees the request, and is not blocked, is made again **when the message is sent**: one queued before the sharing was taken back or the customer was blocked is dropped.

**Replies by email** become the customer's comment (shown to staff labelled *customer*, to the customer as *You*). The message is found by the signed reply address (issued to that customer, so a forwarded message does not carry its power), the threading headers, or the request key in the subject, and it follows the rules of every inbound message: it needs **proof of origin** (a trusted receiver's verdict or a DKIM signature) or waits in quarantine, where an administrator can release it; automatic mail and loops are ignored; limits apply per sender and per request. A customer who does not see the request, or is blocked, is not accepted, and **an email from a customer never opens a request**.

## Customers as data subjects

**Export.** A customer downloads their own data from *My details*, and an administrator of the customer's organization (or the instance) exports it from the *Customer portal settings* page (`GET /api/v1/admin/customers/{id}/personal-data`; recorded in the audit log, with the customer's id and no personal details). It holds their profile, requests (with the description and form status), their comments, the staff's public replies on their requests, requests shared with them and the people they shared with, sessions, sign-in code records (without secrets), mail sent to them, mail received from them and bounce records. It holds nothing about other customers or staff beyond the addresses the customer gave.

**Erase.** A customer erases themselves (typing their address to confirm), or an administrator does (`POST /api/v1/admin/customers/{id}/erase`). It follows the rules for staff (see `PRIVACY.md`): the customer becomes **"Deleted customer"** with an `@erased.invalid` address and no password, name, company or time zone; sessions, sign-in codes, shares, counters, the mail sent to them, the content of mail they sent, and bounce and suppression records for the address are removed; the name and address are replaced in other people's comments, texts, summaries, history, notices, the mail log and queued events. **Requests are kept** as the tracker's work records, without the person. `delete_content` also deletes their comments and empties the summary, details, form answers and mail of the requests they raised. **Requests under legal hold stop the erase** (naming them). It can be repeated, and an erased address that signs in again is a new, empty customer. A test searches every text and JSON column of every table afterwards.

**Promotion.** *Make staff* creates a staff account with the customer's address, name and an initial password (the password policy and the organization's email-domain rule apply). The customer record stays, linked to the account: their requests, comments and history are unchanged and they can still use the portal. The account is a staff seat; **a customer never is**.

## Queues and canned responses

A **queue** is a named TQL query held to one project (`#/queues/PROJECT`, or `/api/v1/projects/{key}/queues`), shown with the number of issues in it **for the person looking**: the query is run with that person's permissions and is always combined with `project = PROJECT`, so it can never reach another project even if its text names one. Queues have an order; a query that stops compiling shows its error in its own row and the others still answer. Anyone who can see the project sees its queues; project administrators manage them, and a query that does not compile is refused when it is saved.

A **canned response** is a saved reply, inserted from a menu in the ticket's reply box. `{{issue.key}}`, `{{customer}}` (the person who raised the request, or "there") and `{{agent}}` (the person replying) are filled in by the page. People who can comment see the responses; project administrators manage them.

## Not built yet

Still to build: attachments on requests, a warning before a goal is missed, SLA reports and a dashboard gadget, business-hours rules that differ by day of the week for a policy,   sign-in with a customer OIDC or SAML provider, CAPTCHA, and per-request-type step-up. A project key written as `me` (lower case) in the API path is read as the portal's own `me` route.

## Files on requests

Off by default. In the portal settings an administrator of the project turns on **Customers may add files to their requests** and sets the
largest file (1 to 100 MB, default 10), the most files customers can add to one request (1 to 20, default 5) and the kinds accepted: images,
documents, and text and logs. Archives and video are never accepted from customers.

What is checked, on the bytes and not on the name: the real type is read from the file, so a page of script named `photo.png` is refused;
anything the scanner flags is refused; the size and the kind are held to the portal's rules. A refused file is not stored at all. A resolved
request takes no more files, and a customer is limited to 30 uploads an hour.

Who sees a file: a customer sees the files they sent, and the files staff chose to share, on a request they can see (their own, or shared
with them or their company). A file staff attach is internal until someone with write access to the request marks it shared (the lock/eye
button on the file in the ticket, or `PUT /api/v1/attachments/{id}/customer-visible`); a file the scanner has not passed cannot be shared; a
file a customer sent cannot be hidden from them. Customers download through their own session (`GET /api/v1/portal/{key}/attachments/{id}`),
always as a download, never shown inline, and delete only what they sent (`DELETE` on the same address).

Privacy: a customer's export lists the files they sent, and erasing a customer removes them (the references are dropped so the collector frees
the bytes, and the file names are replaced in the history of those requests).

## The sign-in check

An optional check before the portal mails a sign-in code, set with **Sign-in check** in the portal settings (`captcha_bits`: 0 is off, 8 to 24;
16 costs a visitor's browser about a second). It is a proof of work rather than a picture puzzle, because the portal runs no third-party scripts
and sends nothing about its visitors anywhere: the browser asks `GET /api/v1/portal/{key}/challenge`, searches for a counter that makes the
SHA-256 of `challenge:counter` start with that many zero bits, and sends both with the request for a code (`check`). The server accepts a
challenge it signed itself, for this portal, unexpired (5 minutes), made at the portal's current difficulty or harder, solved, and not used
before. A failed check says so (400) and costs the address nothing against its hourly allowance; the honeypot still answers as if all were
well. Scripts that call the API directly must do the same work.
