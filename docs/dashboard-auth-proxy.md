# Dashboard auto-login for token-gated products

Some products put their dashboard behind a sign-in wall (OpenAlice is the
reference case: it gates the UI behind a token-exchange login). The Factory's
dashboard reverse-proxy can log in on the operator's behalf, so the human
opening `/products/<name>/dashboard` never sees the product's own login form.

This builds on the base reverse-proxy (`registry/proxy.go`,
`/api/products/<name>/proxy/*`) documented in the deploy notes — that layer is
what makes a product's dashboard reachable through the Factory's port 9000 with
no per-product OCI Security List rule. Auto-login is an optional add-on for the
subset of products that require authentication.

## How it works

1. During onboarding, the operator supplies the product's **first-run admin
   token** at the **Publish** step (optional — leave blank for a public
   dashboard).
2. The token is stored in `products.dashboard_auth_token`. On the first proxied
   request for that product, the proxy POSTs `{"token":"<value>"}` to the
   product's login path (default `/api/auth/login`, overridable per product via
   `dashboard_auth_login`).
3. The proxy takes the first `Set-Cookie` the login returns and injects it as
   the `Cookie` header on every subsequent proxied request — so the product
   sees an authenticated session.
4. The session cookie is cached **in the proxy's process memory**, keyed by
   product, with a 1-hour TTL. A 401/403 from the upstream invalidates the
   cache so the next request re-logs in. A failed login backs off for 5 minutes
   so a misconfigured token can't hammer the upstream.

The login POST is made from the Factory backend directly to the product's
internal container URL over `factory-net` Docker DNS — the token never travels
to the browser.

## Security design — where the token lives (and where it must not)

The auth token is a **write-only secret**. It has exactly one resting place:
the `products.dashboard_auth_token` column, which the products API omits via
`json:"-"`. It is deliberately kept out of every other path:

- **Not in the browser.** The proxy logs in server-side; the token is never
  sent to the client, and the cached session cookie lives only in the proxy's
  memory (never on disk, cold on restart).
- **Not in logs.** The login helper logs product name and status, never the
  token.
- **Not in the wizard run state.** This is the subtle one. Every wizard step
  persists its inputs into `wizard_runs.state`, and the run-status API
  serializes that column verbatim back to clients. So a secret captured at an
  earlier step (e.g. `verify_health`) would be written to the DB scratchpad and
  served back over the wire. To avoid that, the token is collected at the
  **Publish** step — the step that consumes it — and written straight to the
  product row, never into `State`. This mirrors how `alpaca_secret` already
  flows: through `Input` into its destination, never into persisted state.
  (`RunContext.Input` is documented as never-persisted; `State` is persisted.)

If you add another product secret to the wizard, follow the same rule: collect
it at the step that consumes it and read it from `ctx.Input`, never stash it in
`ctx.State`.

## Files

| File | Role |
|------|------|
| `backend/internal/db/db.go` | `dashboard_auth_login` / `dashboard_auth_token` columns + idempotent migration; token field is `json:"-"` |
| `backend/internal/registry/proxy.go` | `sessionCache` — login, cookie injection, TTL, backoff, invalidation |
| `backend/internal/registry/handler.go` | wires the `sessionCache` into the registry `Handler` |
| `backend/internal/wizard/steps_deploy.go` | Publish collects the token from `Input`; `rewriteToInternalURL` keeps dashboard/health URLs on internal Docker DNS |
| `frontend/src/components/wizard/AddWizard.tsx` | masked token field (renders on the Publish step) |
| `frontend/src/types/index.ts` | `dashboard_auth_login` on the `Product` type (token intentionally omitted) |

## Tests

- `backend/internal/wizard/steps_deploy_test.go` — the token never lands in
  persisted run state; it reaches the product row via `Input`; a blank re-run
  doesn't wipe a stored token.
- `backend/internal/registry/proxy_session_test.go` — `sessionCache` login
  success/caching, backoff after failure, invalidation forces re-login, custom
  vs. default login path, no-`Set-Cookie` handling.
