# ADR-001 — Factory architecture decisions

Date: 2026-07-19. Status: accepted. Supersedes Phase 0 of Market-AI's
`docs/fork-factory-plan.md`.

## Context

Market-AI-Factory turns one trading system into a platform running many: a
Product = a GitHub repo that, after scout + research + manual approval,
trades autonomously with its own budget. These decisions were made with the
owner before building.

## Decisions

1. **Isolation model: docker-compose stack per product, one Oracle VM.**
   Each product gets its own compose stack, volumes, `.env`, and allocated
   port range (10100, 10200, …). Strongest isolation available on a single
   free-tier VM: a crashing product cannot take down the Factory or a
   sibling; rollback is per-product. Rejected: separate servers (cost),
   separate cloud accounts (operational overhead ≫ benefit at this stage).
   Consequence: RAM is shared — products should use cloud LLM APIs, not
   their own local Ollama.

2. **Broker isolation: one Alpaca paper account per product.** Alpaca has no
   public API to create paper accounts, so the wizard's connect step takes
   pasted per-product keys, validates them via `GET /v2/account`, refuses a
   key already bound to another product, and writes them only to that
   product's server-side `.env`. The Factory stores the key *id* for
   display; the secret lives in the product's env file only.

3. **Product metrics come from Alpaca, not from the product.** The Factory
   polls each product's Alpaca account (equity, P&L, portfolio history) for
   the dashboard cards. Works for any repo — no findings-format contract
   required from day one. A standard product-report schema (fork-factory
   ADR 0.3) is deferred until the "Optimize All" phase needs it.

4. **Monitoring: deterministic every 2h, AI review daily.** 2h checks:
   containers up, health endpoint, Alpaca account ACTIVE, equity vs budget
   floor. Failures → alert + ERROR status. Once daily an AI reviewer
   (qa/devops persona prompt via the Anthropic API) reads the last 24h of
   checks/logs/P&L per product and writes a short report. Rejected: full AI
   team every 2h (12×/day × N products of LLM spend, mostly finding nothing).

5. **Market-AI is product #1 by adoption, not duplication.** GitHub cannot
   fork a repo into its own account; the first product references
   `wrcron2/Market-AI` directly and its already-running deploy is *adopted*
   (registry `adopted=true`, no second deploy).
