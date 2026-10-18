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

## Connecting with OAuth

MCP clients that support it connect without a pasted token: point the client at `<your address>/mcp` and it finds the rest.

- **Discovery.** An unauthenticated call to `/mcp` answers `401` with `WWW-Authenticate: Bearer resource_metadata=".../.well-known/oauth-protected-resource"`. That document names the authorization server, whose metadata is at `/.well-known/oauth-authorization-server`.
- **Registration.** The client registers itself (`POST /oauth/register`, RFC 7591) with a name and redirect addresses: https, http on localhost, or a custom application scheme (never `javascript:`, `data:`, `file:` and the like, never a fragment). It is a public client: there is no client secret, PKCE protects the exchange.
- **Consent.** The client sends the person to `/oauth/authorize` with a PKCE challenge (S256 only) and the scopes it wants. The person signs in if needed and sees the client's name, the scopes in plain words and where they will be sent back to, and allows or denies. Only a person signed in with a password or single sign-on can approve; an API token or an agent cannot approve another client. A redirect address that was not registered, even one character off, is an error page and never a redirect.
- **Tokens.** The client exchanges the single-use code (valid for two minutes, bound to the client and the redirect address) plus its PKCE verifier for an access token (one hour) and a refresh token (thirty days) that **changes on every use**. Using an old refresh token again revokes the whole grant, the access tokens it issued included.
- **What the client can do.** The access token is the person's own scoped token: the scopes they approved are a ceiling, and the person's own rights are applied as well, so a client never does more than the person (a viewer's client cannot change anything whatever scopes it was given). In history and comments it shows as "Agent: <client name> (via <person>)", and agent rules such as transitions that need a person and write budgets apply.
- **Disconnecting.** Account → Connected applications shows each client and what it may do, and disconnects it at once (its tokens stop on their next request). `POST /oauth/revoke` does the same for a client that signs out; an administrator ending a person's access, or deactivating the account, ends their grants too.


## Git tools

`list_development` (scope `read`) and the resource `tasklane://issue/{key}/development` return the code linked to an issue; every piece of provider text in them is marked `external-git-provider` and is untrusted. `create_branch` (scope `branch`, a write: it takes an `idempotency_key`) creates a branch for the issue at the provider when an administrator has turned write actions on for the connection. See `docs/GIT.md`.

## Prompts

`prompts/list` and `prompts/get` offer four ready-made starting points: `triage_issue`, `plan_work`, `summarize_thread` (each takes an issue `key`) and `standup_report` (a `project`, and `days` from 1 to 30). A prompt is an instruction written by this server, saying which tools to use, plus the text of the issue or the list of issues. It grants nothing: every tool call it leads to is scoped, authorized, budgeted and audited like any other, and getting a prompt changes nothing. It needs the `read` scope and answers an issue the person cannot see exactly as it answers one that does not exist.

The issue text was written by people and other systems, so it is **fenced**: it sits between `<<<BEGIN UNTRUSTED …>>>` and `<<<END UNTRUSTED …>>>` markers under a label that says it is data and may contain instructions that must not be followed, and the message says before the fence that only its own instructions count. Text that contains the markers cannot close the fence (the angle brackets are replaced).

## Resource subscriptions

A client subscribes with `resources/subscribe` to:

- `tasklane://issue/KEY`: the issue (a comment, a status change, an assignment, a text edit, an attachment);
- `tasklane://issue/KEY/development`: the code linked to the issue (a pull request, build or deployment changes), and only that;
- `tasklane://search?q=<TQL>`: a filter; the client is told when an issue that matches it is created or changed. (An issue that stops matching is not announced.) Reading this resource lists the current matches.

Notifications are `notifications/resources/updated` with the resource's `uri`, sent on the session's **event stream**: a `GET` of the endpoint with `Mcp-Session-Id` and `Accept: text/event-stream` (one stream per session; a second gets `409`, a missing session id `400`, someone else's session `404`). Over stdio the proxy opens the stream itself and writes the notifications as lines. A notification says that something changed, not what: the client reads the resource again, and that read is authorized like any other. Typical delivery is well under a second.

Authorization is checked **at every notification**: the token must still work (a revoked, expired, paused or killed agent's stream ends), and the person must still be able to see the issue (otherwise nothing is said, and the stream stays quiet). A session holds at most 100 subscriptions. Notifications are queued without waiting and dropped if the client does not keep up, so a slow subscriber never slows anyone saving; the client finds out when it next reads.
