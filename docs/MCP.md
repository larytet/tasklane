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
