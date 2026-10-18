# Inbound email

People reply to a notification to comment on the issue. This page covers what is built: reply by email and create an issue by email, over a webhook source. IMAP polling and a built-in SMTP receiver are built too.

## Sources

- **Webhook**: `POST /api/v1/mail/inbound/{id}`, signed (below).
- **IMAP mailbox** (Admin, Email, Mailboxes): Tasklane logs in, opens a folder and reads each message by UID from a **checkpoint**. Each message goes through the same pipeline as a webhook delivery, and the checkpoint moves only after it was handled, so a crash loses nothing and at worst reads one message again, which its Message-ID makes harmless. If the server changes the folder's UIDVALIDITY the old UIDs mean nothing: the folder is read again from the start and the duplicates are ignored (the reset is counted and shown). A message that was removed from the folder under us is skipped; one Tasklane could not keep (the store or the database failed) stops the pass and is tried again. Messages are read without marking them read and are left in the folder.
  - With **IDLE** the server tells Tasklane about new mail at once (a delivery takes well under a second in tests); without it the folder is polled every 30 seconds. A connection that fails is retried after 10 seconds, doubling to 5 minutes; the last error shows next to the mailbox.
  - Connections are TLS (certificates always verified) or, for a server on this machine only, plain text; a password is never sent without TLS to another host. Mailboxes on loopback or private addresses need `imap_allow_private`; cloud metadata addresses are always blocked. The password is sealed at rest and never shown. Only one node reads a mailbox at a time.
  - Mark a mailbox **trusted** only if mail reaches it through a server you run whose `Authentication-Results` header you believe; otherwise Tasklane relies on its own DKIM check.

- **Built-in SMTP receiver** (`smtp_listen`, for direct MX delivery; optional STARTTLS with `smtp_listen_tls_cert` and `smtp_listen_tls_key`). It accepts mail **only** for the installation's mail domain (the host of `base_url`) and only for addresses it would act on: `reply+…`, `bounce+…`, and the key of an existing, unarchived project (with plus-tags). Everything else is refused during the conversation (`550 5.7.1` relaying denied, `550 5.1.1` no such mailbox), so it is not an open relay and it never generates a bounce of its own: no backscatter. `250` is sent only after the raw message was stored; if it could not be, the answer is `451` and the sending server retries. A message is limited to 25 MiB (declared `SIZE` is refused early; an over-long body is read through and refused so the conversation stays in step), 50 recipients, 100 connections and 10 per address at once, 60 seconds idle, 10 errors per connection, command lines of up to 1,000 bytes. No AUTH is offered and `VRFY` confirms nothing.
  - The envelope is written into the stored message ahead of whatever the sender wrote: `Return-Path` (none for a bounce), one `X-Original-To` per recipient, and a `Received` line with the sender's address and whether TLS was used.
  - Mail received this way is never *trusted*: nothing before the listener gave a verdict, so a message needs its own **DKIM** proof (any `Authentication-Results` header the sender wrote is ignored). If you want a receiver's verdict believed, run that receiver and forward to the webhook or IMAP source marked trusted.

## How a message is handled

1. A **source** delivers the raw message. The webhook source takes `POST /api/v1/mail/inbound/{id}` with the message as the body and `X-Tasklane-Signature: sha256=<hex>`, the HMAC-SHA256 of the body under the source's secret. Add a source in Admin, Email; its secret is shown once. Every refusal (unknown source, bad signature, source removed) is the same `403`. A body over 26 MB is `413`.
2. The message is **recorded once**, by `Message-ID`: a second delivery is counted (`duplicates`) and ignored. A message with no id gets one from its hash. If the process dies half way, the relay's retry finishes the job once two minutes have passed.
3. The raw message is **stored in the blob store before anything else** (it is the `.eml` an administrator can open from the queue). If it cannot be stored the request fails with a 5xx and the sender retries; nothing is acknowledged that was not kept.
4. It ends in one **outcome**, recorded with the reason and visible in Admin, Email:
   - `ignored` — automatic or looping mail, a duplicate, a rate limit, no text, or not about an issue;
   - `rejected` — too large (25 MiB by default), a huge header section, not a message, more than one sender;
   - `quarantined` — waiting for an administrator (see below);
   - `accepted` — it became a comment.

Nothing is ever sent back to the sender, so there is no backscatter, and mail never creates a comment as anyone but the person it came from.

## What makes a message a comment

All of these, in this order:

- **Not automatic.** Ignored: `Auto-Submitted` other than `no`, `Precedence: bulk|junk|list|auto_reply`, `X-Auto-Response-Suppress`, `X-Autoreply`, `List-Id`, a null `Return-Path`, a delivery report, an out-of-office subject, mail from `mailer-daemon` or `postmaster`, and any message carrying `X-Tasklane-Loop` or `X-Tasklane-Instance` (everything this server sends does, so mail between two installations, or fed back to this one, stops at once).
- **Within limits.** Per sender 30 per hour, per sender domain 300, per issue 100 (counted from the record; the clock is the server's).
- **A known sender.** The single `From` address must be an active user's email address. The display name is never used. Agents cannot write by email. An unknown sender is quarantined.
- **Proof of origin**, either of:
  - **Tasklane's own DKIM check** on the stored raw message, from any source and any transport. A signature counts if it is well formed, signs `From`, covers the whole body (a signature with `l=` is refused), has not expired, its key is published in DNS (not revoked, not in testing mode `t=y`, RSA of at least 1024 bits or ed25519), and both the body hash and the signature verify (RSA-SHA256 and ed25519-SHA256; simple and relaxed canonicalization; at most five signatures are examined). The signing domain must be aligned with the sender's: the same domain or a parent of it on a label boundary (`example.com` for `bob@mail.example.com`; never `example.com.evil.net`, `notexample.com` or a subdomain signing for its parent). Tasklane looks keys up with the system resolver; when no resolver is configured it does not look at signatures at all.
  - **A trusted receiver's verdict**: a source marked **trusted** (the path before it is yours: a provider or a relay you run) whose `Authentication-Results` header says `dmarc=pass`, or `dkim=pass` with an aligned signing domain. A source that is not trusted cannot make its own header count.

  Anything else is quarantined with "no proof of origin" and the reason (no signature, which check failed, or that a valid signature is not aligned). DMARC *policy* records (`_dmarc`) are not consulted, and SPF is not checked.
- **A target issue.** In this order: the signed reply address (`reply+TOKEN@domain` in `To`, `Cc`, `Delivered-To`, `X-Original-To` or `Envelope-To`); otherwise `In-Reply-To` or `References` naming a message this server sent; otherwise a key like `[ABC-123]` in the subject. A message that names no issue is ignored.
- **Or a new issue.** A message that is not a reply and is addressed to a project (`ABC@domain`, with optional plus-tags) opens an issue there, as the sender, so their Create Issues permission applies. See below.
- **A mirrored project** is quarantined with the reason "mirrored project".
- **The sender's own permission.** The comment is made through the same code as a comment in the browser, as that user, so Add Comments, project roles, group roles and archived (read-only) issues all apply. A sender who may not comment is quarantined, never told why.
- **Text.** The comment is what the person wrote: the reply-marker line, "On … wrote:" attributions in several languages, Outlook header blocks, "Original Message" and forwarded dividers, a trailing block of `>` quotes and a `-- ` signature are removed. An inline reply (answers between quoted lines) is kept whole, and when unsure the text stays. HTML is reduced to text (no script, style, remote content or markup); charsets (UTF-8, ISO-8859-x, Shift-JIS, GBK…), quoted-printable and base64 are decoded with bounds on size, nesting (6 levels) and parts (100). Attachments are counted but **not kept yet**.

The comment shows "via email" on the ticket.

## Create an issue by email

Each project has an address, `KEY@domain` (shown by `GET /api/v1/projects/{key}/mail`). Mail to it, from a known user with a proven origin, opens an issue:

- the **subject** is the summary (with `Re:`, `Fw:` and `Fwd:` prefixes removed, at most 200 characters; an empty one becomes "(no subject)");
- the **body** is the description, kept whole (HTML becomes safe text);
- the **sender** is the reporter and acts with their permissions: a viewer's message is quarantined and a release does not create it either;
- **plus-tags** set the type and labels: `ABC+bug+urgent@domain` is a Bug labelled `urgent` (the first tag that names an issue type is the type, the rest are labels; the default type is Task);
- **Cc** (and To) addresses that belong to users make those users watchers, each only if they can see the issue; other addresses are ignored;
- a **reply** to that message (its `In-Reply-To` or `References`) becomes a comment on the created issue, and so does a reply to any later notification.

**Unknown senders.** Each project chooses what happens to mail from an address that is not a user: `quarantine` (the default; an administrator decides, and cannot release it because it names nobody to act as) or `reject` (recorded as rejected, nothing created). Set with `PUT /api/v1/projects/{key}/mail-policy` (administrators). Accepting such a sender as an external reporter belongs to the service desk and is not built.

Releasing a quarantined message that was addressed to a project creates the issue once, as the matched sender.

## Bounces, complaints and suppression

Every message leaves with its own **return path** (the envelope sender), `bounce+TOKEN@domain`; the token names the message and is signed with the reply-address keys. A delivery report (a DSN) or an abuse feedback report (ARF) that arrives there is matched to the message it answers **and to the address that message went to**: a report about a different address, a report without a valid token, one signed by a revoked key, or a person's mail to the return path is recorded as ignored and changes nothing. A valid report is handled:

- a **hard bounce** (action `failed`, status `5.x.x`) suppresses the address at once;
- a **soft bounce** (`failed` with `4.x.x`) is counted, and the address is suppressed on the third within seven days; `delayed` and success notices change nothing;
- a **complaint** (a feedback report) suppresses at once.

Providers that report these events themselves can post to `POST /api/v1/mail/bounces/{source}` a body `{"events":[{"type":"bounce|soft|complaint","email":"…","status":"…"}]}` signed like an inbound message (the source's secret, `X-Tasklane-Signature`); an event only counts for an address Tasklane has mailed.

**Suppression stops email to that address and nothing else**: the in-app inbox still gets every notification, and the delivery log shows the held-back mail as suppressed. The person sees "Email to your address is being held back" in their notification preferences. Administrators see the list in Admin, Email and can **clear** an entry (audited), which also resets the soft-bounce count.

## Reply addresses

Every notification carries `Reply-To: reply+TOKEN@domain`. The token holds the issue, an expiry (30 days) and a signature over the issue, the **recipient** and the expiry, so a forwarded notification does not give its power to whoever received the forward: the signature fails for any other sender. A forged, expired or wrong-sender address is quarantined (and, since it names no trusted issue, cannot be released).

Keys: **Rotate reply key** (Admin, Email, or `POST /api/v1/admin/mail/reply-keys`) adds a signing key; addresses signed earlier keep working. `DELETE /api/v1/admin/mail/reply-keys/{kid}` revokes one, which invalidates every address it signed; the newest key cannot be revoked.

## The quarantine

Admin, Email lists the messages by outcome. An administrator can open the original (`.eml`), **release** a quarantined message or **delete** it. Release makes the comment once, as the matched user with that user's permissions (it fails and leaves the message quarantined if they may no longer comment), and only works for a message with a known sender, an issue and some text. Deleting drops the text and keeps the row. Both are audited.

## Configuration

The reply domain is the host of `base_url`. Limits (`InboundLimits`) have fixed defaults for now. Receiving mail does not need outbound mail to be configured.

## Retention and erase (MAIL-18)

- **Inbound messages.** `mail_raw_retention_days` (default 90, 0 keeps them) limits how long the original message, its text, its subject and its sender are kept. After that the row stays without them and its Message-ID is replaced by a hash, so a redelivery of the same message is still recognised as a duplicate and does nothing. Comments and issues the message created are the issue's own history and are not touched. Mail waiting for a decision is never purged (the decision needs the text); mail on an issue under legal hold is kept until the hold is released.
- **Bounces.** `mail_bounce_retention_days` (default 365, 0 keeps them) removes old bounce records and suppressions that an administrator cleared. An address that is still suppressed is never removed by age.
- **Erasing a person** removes at once, whatever the periods are: the messages they sent or that were matched to them (rows purged as above, the originals removed from the blob store), their bounce records and their suppression entry. It is refused while their mail is on an issue under legal hold. The personal-data export lists their inbound mail and bounce records.
- Each automatic run is audited with counts only (`mail.retention`), never with content. **Apply retention now** in Administration → Email runs it on demand. The worker runs hourly.

## Mirrored projects (MAIL-19)

While a project is mirrored from Jira, **Tasklane sends no email for it**: Jira keeps sending its own, and two systems mailing the same people about the same issue is worse than one. The in-app inbox is Tasklane's own and keeps working. Mail sent to the project's address, or a reply to an issue in it, waits in quarantine with the reason "mirrored project"; it cannot be released (a mirrored issue takes no writes from anywhere), and nothing is ever sent back to the sender. Administration's Jira status shows this in words for each project. Tasklane cannot read Jira's notification settings, so it cannot tell you whether Jira's email is on. After **cut-over to native** the project behaves like any other: email is sent and inbound mail works.
