---
name: run-market-ai-factory
description: Run, build, smoke-test, or screenshot the Market-AI-Factory app locally — Go registry/wizard backend (:9080), React Products dashboard (:9000). Use when asked to run/start the factory, verify a change works, or take a dashboard screenshot.
---

# Run Market-AI-Factory (local dev, macOS)

Two services: Go backend (`backend/`, HTTP :9080 — registry, wizard, monitor)
and React/Vite dashboard (`frontend/`, :9000, proxies `/api` to :9080).
Paths relative to the repo root.

## Agent path: smoke driver

```bash
.claude/skills/run-market-ai-factory/smoke.sh /tmp/factory-smoke
# exit 0 = healthy; screenshot at /tmp/factory-smoke/factory.png
```

Builds the backend, launches both services on scratch ports (9180/9100)
with a throwaway DB, checks `/api/health`, `/api/products`, the SPA routes,
and screenshots the Products grid. Verified output ends `== 7 passed, 0 failed`.

## Run the real dev stack

```bash
cd backend && go build -o factory-server ./cmd/server && ./factory-server
# → "factory server listening" port=9080; FACTORY_DB_DSN overrides ./factory.db

cd frontend && npm install && npm run dev    # → http://localhost:9000
```

Health probe: `curl localhost:9080/api/health` → `{"ok":true,...}` (dedicated
endpoint — never depends on DB contents).

## Drive the UI as a user (ui.mjs — REQUIRED for user-facing changes)

curl verifies APIs; it does not verify features. The wizard once shipped a
permanently-stuck-run bug that any 30-second UI walk would have caught. So:
any change a user sees gets walked with the CDP driver before it ships —
including one wrong input, one Back, and one Cancel.

```bash
UIDRV=.claude/skills/run-market-ai-factory/ui.mjs
node $UIDRV start                                  # headless Chrome on :9333
node $UIDRV goto  "http://localhost:9000/wizard/new"
node $UIDRV type  'input[placeholder="market-ai"]' "OpenAlice"   # → normalized to "openalice"
node $UIDRV clickText "Start onboarding"           # accepts confirm() dialogs too
node $UIDRV clickText "Continue"
node $UIDRV clickText "Back"
node $UIDRV clickText "Cancel run"                 # walks through the confirm
node $UIDRV text  ".grid > div:nth-child(2) .text-\\[15px\\]"    # read active step
node $UIDRV eval  "location.pathname"
node $UIDRV shot  /tmp/walk.png                    # LOOK at it
node $UIDRV stop
```

No dependencies — Node ≥21 built-ins only (fetch + WebSocket over the
DevTools Protocol). `type` uses the native value setter + input event so
React onChange fires. `click`/`clickText` auto-accept `window.confirm`
(headless Chrome silently REJECTS dialogs otherwise — a Cancel-style flow
would look like a dead button).

## Screenshot (headless Chrome)

```bash
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless \
  --disable-gpu --window-size=1440,900 --virtual-time-budget=6000 \
  --screenshot=/tmp/factory.png http://localhost:9000/products
```

## Checks per layer

```bash
cd backend && go build ./... && go vet ./...
cd frontend && npm run build     # tsc + vite
```

## Gotchas

- Ports 9080/9000 (dev) and 9180/9100 (smoke) were chosen to never collide
  with Market-AI's 8080/3000 — both projects run side by side.
- `/api/products` returns `{"products":[]}` on an empty DB by design (the
  Market-AI empty-DB-500 lesson); if it 500s, something is actually broken.
- The compose stack (`infra/docker-compose.factory.yml`) mounts
  `/var/run/docker.sock` — the orchestrator drives product stacks through it.
  Local dev without Docker works fine for registry/wizard-UI work.
