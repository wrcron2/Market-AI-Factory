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
