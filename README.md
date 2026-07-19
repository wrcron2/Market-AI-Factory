# Market AI · Factory

A platform that runs many autonomous trading products. A **Product** is a
GitHub repo that — after the scout/research pipeline and manual approval —
is onboarded through the Add wizard and becomes a standalone trading agent
with its own Alpaca paper account, budget, container stack, and card on the
Products dashboard.

- `backend/` — Go: product registry, onboarding wizard (step engine with
  per-step error lists + Refresh), 2h monitor, product orchestrator.
- `frontend/` — React dashboard: Products grid (P&L cards + sparklines),
  product drill-down, Pipeline, Add wizard.
- `products/<name>/` — one spec dir per product (`product.yaml`, compose
  override). Product secrets live in `products/<name>/.env` (gitignored).
- `infra/docker-compose.factory.yml` — the Factory's own stack (:9080/:9000).
- `docs/adr/001-factory.md` — the architecture decisions and why.

First product: [Market-AI](https://github.com/wrcron2/Market-AI), adopted
in place (it already runs; the Factory tracks and monitors it rather than
redeploying it).

Run locally: see `.claude/skills/run-market-ai-factory/SKILL.md`.
