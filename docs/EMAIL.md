# Inbound email

People reply to a notification to comment on the issue. This page covers what is built: reply by email and create an issue by email, over a webhook source. IMAP and the SMTP listener are not yet.

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

## Reply addresses

Every notification carries `Reply-To: reply+TOKEN@domain`. The token holds the issue, an expiry (30 days) and a signature over the issue, the **recipient** and the expiry, so a forwarded notification does not give its power to whoever received the forward: the signature fails for any other sender. A forged, expired or wrong-sender address is quarantined (and, since it names no trusted issue, cannot be released).

Keys: **Rotate reply key** (Admin, Email, or `POST /api/v1/admin/mail/reply-keys`) adds a signing key; addresses signed earlier keep working. `DELETE /api/v1/admin/mail/reply-keys/{kid}` revokes one, which invalidates every address it signed; the newest key cannot be revoked.

## The quarantine

Admin, Email lists the messages by outcome. An administrator can open the original (`.eml`), **release** a quarantined message or **delete** it. Release makes the comment once, as the matched user with that user's permissions (it fails and leaves the message quarantined if they may no longer comment), and only works for a message with a known sender, an issue and some text. Deleting drops the text and keeps the row. Both are audited.

## Configuration

The reply domain is the host of `base_url`. Limits (`InboundLimits`) have fixed defaults for now. Receiving mail does not need outbound mail to be configured.
