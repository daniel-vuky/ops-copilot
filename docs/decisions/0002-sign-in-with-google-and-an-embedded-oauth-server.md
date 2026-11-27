# 2. Sign in with Google and an embedded OAuth server

Date: 2026-11-27

## Status

Proposed

## Context

Ops Copilot ([0001](0001-build-ops-copilot-as-one-go-binary-on-sqlite.md)) has three kinds of callers:

- people in the web UI, about 20 of them;
- MCP clients reading the documents, such as Claude Code and claude.ai;
- alert sources (New Relic and Azure Monitor) sending webhooks.

It must not depend on Cloudflare Access, because it moves to a company server later. Daniel's sales-and-ordering-guide project already runs an embedded OAuth 2.1 server for MCP clients, layered on its own web sign-in, and it works with both Claude Code and claude.ai. This decision follows that pattern, with Google as the identity provider.

### Option A: Personal tokens only

- Pro: one table and one page.
- Con: claude.ai connectors require OAuth, and long-lived tokens leak into config files.

### Option B: An external identity server (Keycloak or similar)

- Pro: standard and complete.
- Con: a JVM and another container. The earlier attempt showed it is the heaviest part of the stack.

### Option C: Google sign-in plus an embedded OAuth server (chosen)

- Pro: one login for people; Claude Code and claude.ai connect with no copied secrets; the pattern is already proven in sales-and-ordering-guide.
- Con: the OAuth server is ours to build and test.

## Decision

### Who may enter

1. **An allowlist in env.** `ALLOWED_EMAILS` lists people. `ALLOWED_DOMAINS` is empty by default; a domain admits everyone in it, so it is set only on purpose. `ADMIN_EMAILS` is a subset of the allowed emails.
2. **The allowlist is checked on every request**, for sessions, OAuth access tokens and API keys alike. Removing an email cuts that person's access on their next request.
3. **Two roles, no more.**
   - A member uses every screen. They can stop, re-run and resend investigations; edit project names, alert sources and stacks; and see and revoke only their own MCP connections and API keys.
   - An admin can also:
     - see and revoke everyone's connections and keys;
     - create and rotate webhook URLs;
     - set project repositories and report recipients;
     - send test notifications.

### People: Google sign-in

4. **Google OIDC**, authorization code flow with PKCE, `state` and `nonce` (`coreos/go-oidc`, `golang.org/x/oauth2`). The ID token must have `email_verified` set. When a domain is allowlisted, the `hd` claim must match it.
5. **Server-side sessions.**
   - The cookie holds only a random session id, stored as a SHA-256 hash. It is `HttpOnly`, `Secure` and `SameSite=Lax`.
   - A session lasts 7 days and ends earlier on sign-out, which is a POST.
6. **Browser hardening.**
   - A state-changing `/api` request must carry a CSRF token tied to the session. The consent form carries its own.
   - Every page is sent with `Content-Security-Policy: default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'`. Pages the server renders itself, such as the consent and error pages, therefore use stylesheet classes only, with no inline styles or scripts.
   - The UI renders report and document text as text, never as HTML ([0003](0003-investigate-through-a-locked-down-tool-surface.md)).
7. **Client IPs.** Rate limits use the peer address, unless `TRUSTED_IP_HEADER` names a header set by the proxy in front (for example `CF-Connecting-IP` behind the Tunnel). That header is read only when the variable is set.

### MCP clients: the embedded OAuth server

8. **Discovery.**
   - Authorization server metadata (RFC 8414) is published at `/.well-known/oauth-authorization-server`, including `code_challenge_methods_supported: ["S256"]`.
   - Protected resource metadata (RFC 9728) is published at `/.well-known/oauth-protected-resource` and at the path-aware `/.well-known/oauth-protected-resource/mcp`.
   - A 401 from `/mcp` carries `WWW-Authenticate: Bearer resource_metadata="…"` (RFC 9728 §5.1).
9. **Dynamic client registration (RFC 7591).**
   - Public clients (`none`) and confidential clients (`client_secret_post`, `client_secret_basic`) are accepted. Client secrets are stored hashed.
   - A redirect URI must be https, or http on the loopback.
   - At authorization the redirect URI must match a registered one exactly. The only exception is the port of a loopback URI, which may differ (RFC 8252 §7.3).
   - `client_name` is plain text of at most 100 characters, with no control characters.
   - Registration is limited to 10 per hour per client IP.
   - A client that has not completed an authorization within 24 hours is deleted.
   - At most 500 such clients exist at once; a new registration beyond that evicts the oldest one.
10. **Authorization.**
    - Only the authorization code flow with PKCE `S256` is accepted.
    - `resource` (RFC 8707) must equal `PUBLIC_URL` + `/mcp`.
    - There is one scope, `docs:read`.
    - A valid request is stored server-side under a random id for 10 minutes. If there is no web session, the person goes to Google and comes back to that id.
    - The consent page shows:
      - the client's name, marked as supplied by the app and not verified;
      - the redirect host, shown prominently;
      - what the client will and will not be able to do;
      - Allow and Deny as a form POST.
    - Deny redirects to the client with `error=access_denied`. Requests that cannot safely redirect show an error page: an unknown client, a redirect URI that does not match, or an expired request.
    - A code is stored as a SHA-256 hash, lives 10 minutes and works once. Presenting a used code revokes every token issued from it.
11. **Tokens.**
    - Access and refresh tokens are opaque random strings, stored only as SHA-256 hashes.
    - An access token lives 1 hour.
    - A refresh token rotates on every use.
    - For 30 seconds after rotation, the previous refresh token, presented by the same client, returns the same new pair, so concurrent refreshes do not fail. That pair is held in memory only, never stored.
    - After those 30 seconds, presenting the old token revokes the whole connection.
    - A connection lasts at most 30 days, then the client signs in again.
    - Tokens are revoked at `/oauth/revoke` (RFC 7009) or from Settings.
12. **What `/mcp` serves.** It serves the document tools only: `search_docs` and `get_doc`. Each request checks the token, its audience and the allowlist. New Relic and the company MCP are never exposed to OAuth clients or API keys. They exist only on the investigator's internal endpoint ([0003](0003-investigate-through-a-locked-down-tool-surface.md)).
13. **API keys** serve MCP clients that cannot run OAuth.
    - A key is `oc_` followed by 32 random bytes, stored hashed, and shown once.
    - It belongs to a person and carries the scope `docs:read`. It is accepted on `/mcp` only, never on `/api/*`.
    - It must have an expiry of at most 365 days.
    - It is sent as `Authorization: Bearer` or `x-api-key`.
    - It stops working when its owner leaves the allowlist.

### Alert sources: webhook tokens

14. **Each source has a path token:** `/hooks/newrelic/<token>` and `/hooks/azure/<token>`. A header secret is not used, because Azure Monitor action groups cannot send custom headers.
    - A source has no token until an admin creates its URL in Settings. Until then, its webhook path answers 404.
    - A token is 32 random bytes, stored as a SHA-256 hash plus its last 4 characters (for display), and compared in constant time.
    - The full URL is shown once, to that admin, right after it is created or rotated.
    - The token part of the path is redacted in every log line the binary writes. Rotating it stops the old URL at once.

## Consequences

**Easier:**

- People use the Google account they already have. Claude Code and claude.ai connect through a consent page, with no secrets copied by hand.
- Cutting someone off is one env change.
- MCP clients reach only the documents, so a stolen client token cannot read code or monitoring data.

**Harder:**

- The OAuth server, token storage, rotation and revocation are ours to get right, and need tests that follow the RFCs above.
- Changing the allowlist or admins needs a restart.
- A webhook token in a path can end up in a proxy's access log. Proxies in front must not log paths under `/hooks/`.
