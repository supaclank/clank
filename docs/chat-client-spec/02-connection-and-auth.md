# 02 · Connection & Auth

This layer is the gateway's concern (see [01](01-architecture.md)). It is small but every
rule here has bitten a client.

## Base URL

- **[CONN-001] (MUST)** A client MUST send all requests — JSON and SSE — to a single
  configured **gateway base URL**, never to a host directly. Paths are the host's
  ([05](05-operations.md)); the gateway proxies them. **Why:** the host is not directly
  reachable, is not addressable without the gateway's wake/route step, and requires a token
  the client must not hold. **Golden:** `pkg/gateway/gateway.go`.

## Authentication

The gateway's auth backend is pluggable (HS256 shared-secret for dev, OIDC/JWKS for prod,
etc.; `pkg/auth/`). Clients see a standard OAuth 2.0 + PKCE flow and need not know which
backend is configured.

- **[CONN-002] (MUST)** Before sign-in a client MUST fetch the OAuth configuration from the
  unauthenticated discovery endpoint `GET /auth-config` (authorize endpoint, token
  endpoint, client ID, scopes, callback parameters). **Why:** endpoints and client IDs
  differ per deployment; hardcoding them breaks self-hosters. **Golden:**
  `pkg/gateway/gateway.go` (`/auth-config`).
- **[CONN-003] (MUST)** Sign-in MUST use OAuth 2.0 Authorization Code with **PKCE**. The
  redirect target is platform-specific (desktop: loopback `127.0.0.1:<port>` per RFC 8252;
  mobile: a custom URI scheme; web: a registered https redirect) but the PKCE exchange is
  identical. **Why:** PKCE is required for public clients; the redirect shape is the only
  legitimate per-platform difference. **Golden:** `internal/cloud/oauth.go`.
- **[CONN-010] (MUST)** Every authenticated request — including the SSE connection — MUST
  carry `Authorization: Bearer <access_token>`. **Why:** the gateway gates every endpoint
  uniformly. **Golden:** `internal/daemonclient/transport.go:46` (JSON), `:136` (SSE);
  `internal/host/mux/middleware.go` (`requireBearer`).

### Token lifecycle

- **[CONN-011] (SHOULD)** A client SHOULD refresh the access token *proactively*, shortly
  before expiry (the golden mobile skew is 30 s), rather than waiting for a 401. **Why:**
  proactive refresh avoids a guaranteed first-request failure on every cold open. **Golden:**
  `internal/daemonclient/refresh.go`.
- **[CONN-012] (MUST)** On a `401` from any request, a client MUST attempt **exactly one**
  forced token refresh and retry the request once. It MUST sign the user out **only** when
  the refresh itself fails permanently (the token endpoint returns `invalid_grant`). Any
  other refresh failure — network error, 5xx, timeout, app-was-suspended — MUST be treated
  as transient: keep the session, surface a recoverable error.
  **Why:** signing out on a transient refresh failure (e.g. an OS-killed network during a
  backgrounded refresh) ejects the user for no reason; this was a recurring mobile
  regression. The *stream* still retries forever per [EVT-006]; "exactly one refresh retry"
  is the per-request rule. **Golden:** `internal/daemonclient/refresh.go`.
- **[CONN-013] (MUST)** Refresh tokens MUST be stored in the platform secure store
  (Keychain / Keystore / equivalent), never in plaintext or app-readable storage, and MUST
  NOT be logged. **Why:** a leaked refresh token is a long-lived credential. Cross-ref
  [NFR-SEC-001](09-non-functional.md).
- **[CONN-014] (MUST NOT)** A client MUST NOT possess or send the host's own bearer token;
  that token is injected by the gateway. **Why:** host credentials must never reach an
  end-user device. **Golden:** `pkg/provisioner/transport/bearer.go`.

## Sprite-wake latency

The gateway may need to wake or provision the user's host before the first request
completes. This makes the *first* request of a session (and session creation in
particular) potentially slow — seconds, occasionally longer.

- **[CONN-030] (MUST)** A client MUST use generous timeouts on session creation and the
  first request after an idle period, and MUST show a "starting / waking" affordance rather
  than failing fast. The golden create timeout is 30 s (TUI compose flow); a client SHOULD
  allow at least that. **Why:** a short timeout turns a normal cold start into a spurious
  error. **Golden:** `internal/tui/sessionview_compose.go:296`.
- **[CONN-031] (MUST NOT)** A client MUST NOT auto-retry session creation on a slow
  response without first confirming the prior attempt did not succeed. **Why:** a blind
  retry of a slow `POST /sessions` creates duplicate sessions. **Golden:**
  `internal/tui/sessionview_compose.go:235` (`submitting` single-flight guard).

## Error envelope

- **[CONN-020] (MUST)** Error responses use a stable JSON envelope:
  ```json
  { "code": "not_found", "error": "session s123: not found" }
  ```
  `code` is a stable machine-readable identifier; `error` is human text. A client MUST key
  programmatic behavior on `code` (and HTTP status), and MUST treat `error` as
  display/diagnostic text only. **Why:** matching on human strings is brittle; `code` is the
  contract. **Golden:** `internal/host/mux/mux.go:228` (`errResp`), `:198` (`writeError`).
- **[CONN-021] (MUST)** A client MUST map HTTP status to behavior: `400` invalid request
  (fix and don't blind-retry), `401` re-auth ([CONN-012]), `404` gone/never-existed,
  `409` conflict (state-dependent; e.g. worktree busy, merge conflict), `5xx` server/transient
  (safe to retry idempotent reads with backoff). **Why:** uniform status handling avoids
  per-call guesswork. **Golden:** `internal/host/mux/mux.go:198` (status mapping).

Known `code` values today: `not_found`, `invalid_argument`, `worktree_busy`,
`cannot_merge_default`, `nothing_to_merge`, `commit_message_required`, `main_dirty`,
`merge_conflict`, `reserved_branch`, `invalid_branch_name`, `internal`. Unknown codes MUST
be tolerated (treat by HTTP status) — the set is append-only.
