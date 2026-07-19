# MarketFlow AI — Engineering Team of Agents

A fixed roster of specialized agents, versioned in this directory. Each agent is a
persona with project knowledge baked in; deep domain rules live in `.claude/skills/`
and each agent loads the skills it needs instead of duplicating them.

## Decision record: fixed roster, not dynamic spawning

Chosen 2026-07-19. Reasons:
- Dynamically spawned subagents start cold and re-derive project context every task;
  a fixed roster carries paths, conventions, and gotchas in its definition.
- Definitions are git-versioned — the team improves over time and is identical on
  every machine and every session.
- The Chief PM is NOT an agent here. It stays a persona skill (`/chief-pm`) worn by
  the main session, because subagents cannot spawn other subagents — orchestration
  and review can only happen in the main thread.

## Roster

Every persona follows the same expert structure: identity → domain mastery →
finance layer → decision framework → deep-thinking protocol → project map →
workflow → boundaries/handoff. Patterns distilled 2026-07-19 from
obra/superpowers (methodology), wshobson/agents quantitative-trading plugin
(finance depth), and VoltAgent/awesome-claude-code-subagents (structure) —
both plugins are also installed user-level, so their skills
(`superpowers:systematic-debugging`, `superpowers:verification-before-completion`,
`quantitative-trading:risk-metrics-calculation`,
`quantitative-trading:backtesting-frameworks`, …) are directly invokable.

| Agent | Model | Role | Loads skills |
|---|---|---|---|
| `backend-go` | sonnet | Go backend: HTTP/gRPC/WebSocket/SQLite, order-lifecycle integrity | go-backend-api-patterns, superpowers:* |
| `frontend-react` | sonnet | React dashboard, routing, real-time trading UI | dataviz, superpowers:* |
| `ml-quant` | **opus** | AI brain, LangGraph agents, backtesting, risk metrics, Phase 3 gate | langgraph-agent-patterns, trading-signal-pipeline, risk-and-portfolio, quantitative-trading:* |
| `devops-oracle` | sonnet | Oracle deploy, Docker, nginx, Ollama, capital-critical state ops | superpowers:systematic-debugging |
| `qa` | sonnet | Verification, code review, financial-correctness hunting | verify, code-review, superpowers:verification-before-completion |

## Handoff protocol

All handoffs route through the main session (the orchestrator, optionally wearing
the Chief PM persona):

1. Main session decomposes the task and dispatches to one specialist at a time
   (parallel only when work streams touch disjoint files).
2. Every specialist ends with a **handoff report** (format below). The main session
   relays it, never paraphrases test results.
3. Code-changing work goes to `qa` before commit. QA verdict: APPROVE / FIX (with
   findings). Findings go back to the same specialist via SendMessage — same agent,
   context intact — not a fresh spawn.
4. Product-level review (does this belong in the product, Notion sync, roadmap
   impact) is the main session's job via the `chief-pm` and `product-notion-sync`
   skills. Agents never push to main and never write to Notion.

### Handoff report format (every agent's final message)

```
DONE: one sentence — what was accomplished
FILES: changed files with one-line reasons
VERIFIED: exactly how it was checked (command + observed output), or "NOT VERIFIED" + why
RISKS: anything fragile, assumed, or intentionally skipped
NEXT: suggested next agent, or "none"
```

## Rules that bind every agent

- Never commit, push, or deploy — report back; the main session owns git and Oracle.
- Never relax the Phase 3 gate requirements (see CLAUDE.md).
- Portfolio limits are code-enforced in `ai-brain/agents/portfolio_limits.py`;
  prompt-level caps are advisory only. Never move enforcement back into prompts.
