# MarketFlow AI — Repo Scout Pipeline

> **Note (2026-07-04):** The Scout & Research agents now run **natively inside
> the Go backend** (`backend/internal/pipeline`) — GitHub REST API for search,
> the same Anthropic/Groq/Ollama routing as Ask AI for classification and
> research, and the local SQLite DB for storage. Trigger them from the
> dashboard's Pipeline tab. No `claude` CLI, Supabase, or cron needed.
> Research reports are viewable in the dashboard; Notion export is optional
> (set NOTION_API_KEY + NOTION_PARENT_PAGE_ID). The prompt files and
> scout-cron.sh below are kept for reference only.

Two Claude Code agents that automatically find and research high-quality
open-source trading/quant repos, then log findings to Notion.

## How it works

```
[cron every 6h]
       │
       ▼
 scout-agent        ← searches GitHub, dedupes against Supabase,
                      marks new repos 'good' or 'rejected'
       │
       ▼ (always chained)
 research-agent     ← picks up any rows where status='good' AND
                      researched_at IS NULL, does deep research,
                      writes to Notion, marks rows 'researched'
```

Agent 2 no-ops naturally if nothing new was marked good.

## One-time setup

### 1. Supabase table

Open your Supabase project → SQL editor, then run:

```
agents/setup-supabase.sql
```

### 2. Environment variables

Add these to your `.env` (stubs are already in `.env.example`):

| Variable | Where to get it |
|---|---|
| `SUPABASE_URL` | Supabase project → Settings → API → Project URL |
| `SUPABASE_SERVICE_KEY` | Supabase project → Settings → API → `service_role` secret key |
| `GITHUB_TOKEN` | GitHub → Settings → Developer settings → Personal access tokens (read:repo scope is enough) |

`GITHUB_TOKEN` is optional but strongly recommended — unauthenticated GitHub
search is rate-limited to 10 requests/hour.

### 3. Schedule the cron

```bash
# Make the script executable (already done if you cloned fresh)
chmod +x agents/scout-cron.sh

# Open crontab
crontab -e

# Add this line (every 6 hours):
0 */6 * * * cd /path/to/marketflow-ai && ./agents/scout-cron.sh >> logs/scout.log 2>&1
```

Replace `/path/to/marketflow-ai` with the absolute path to this repo.

> **Note:** cron does not inherit your shell's environment. The script
> sources `.env` from the project root automatically — make sure your
> secrets live there, not only in your shell profile.

## Running manually

```bash
cd /path/to/marketflow-ai
./agents/scout-cron.sh
```

## Files

| File | Purpose |
|---|---|
| `scout-agent-prompt.md` | Prompt for the Scout Agent (GitHub search + Supabase insert) |
| `research-agent-prompt.md` | Prompt for the Research Agent (deep analysis + Notion write) |
| `scout-cron.sh` | Wrapper script: sources env, runs Agent 1 then Agent 2, timestamps output |
| `setup-supabase.sql` | DDL for the `github_repo_scout` table — run once |
| `README.md` | This file |

## Supabase row lifecycle

```
new  →  good  →  researched
     ↘  rejected
```

The scout never overwrites a `researched` row. Re-running is always safe.
