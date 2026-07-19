// Package pipeline exposes the HTTP API for the Repo Scout & Research pipeline.
//
// Both agents run natively inside the backend — GitHub REST API for search,
// the shared llm package (Anthropic / Groq / local Ollama, same routing as
// Ask AI) for classification and analysis, and the local SQLite database for
// storage. No external CLI is required, so the pipeline works identically on
// a laptop and inside the Docker container on Oracle. Notion export is
// optional: set NOTION_API_KEY + NOTION_PARENT_PAGE_ID to enable it.
package pipeline

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
	"github.com/wrcron2/market-ai-factory/backend/internal/llm"
)

// Handler holds shared state for all pipeline endpoints.
type Handler struct {
	mu              sync.RWMutex
	scoutRunning    bool
	researchRunning bool
	lastScoutRun    *time.Time
	lastResearchRun *time.Time

	projectRoot string
	db          *db.DB
	logger      *zap.Logger
	llm         *llm.Client
	http        *http.Client
}

// New creates a Handler using the existing SQLite DB — no external services required.
func New(projectRoot string, database *db.DB, logger *zap.Logger) *Handler {
	return &Handler{
		projectRoot: projectRoot,
		db:          database,
		logger:      logger,
		llm:         llm.New(),
		http:        &http.Client{Timeout: 30 * time.Second},
	}
}

// Repos handles GET /api/pipeline/repos
// Accepts an optional ?status= query param (e.g. "good", "new", "rejected", "researched").
func (h *Handler) Repos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	statusFilter := r.URL.Query().Get("status")
	rows, err := h.db.ListRepos(statusFilter)
	if err != nil {
		writeJSON(w, map[string]any{"repos": []any{}, "error": err.Error()})
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	writeJSON(w, map[string]any{"repos": rows})
}

// Status handles GET /api/pipeline/status
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	sr := h.scoutRunning
	rr := h.researchRunning
	lsr := h.lastScoutRun
	lrr := h.lastResearchRun
	h.mu.RUnlock()

	toPtr := func(t *time.Time) *string {
		if t == nil {
			return nil
		}
		s := t.UTC().Format(time.RFC3339)
		return &s
	}

	writeJSON(w, map[string]any{
		"scout": map[string]any{
			"running":  sr,
			"last_run": toPtr(lsr),
			"schedule": "on demand",
		},
		"research": map[string]any{
			"running":  rr,
			"last_run": toPtr(lrr),
		},
	})
}

// RunScout handles POST /api/pipeline/run/scout
// Runs the scout agent in the background, writing output to logs/scout.log.
func (h *Handler) RunScout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct{ Model string `json:"model"` }
	_ = json.NewDecoder(r.Body).Decode(&req)

	h.mu.Lock()
	if h.scoutRunning {
		h.mu.Unlock()
		writeJSON(w, map[string]any{"started": false, "message": "Scout agent already running"})
		return
	}
	h.scoutRunning = true
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			h.scoutRunning = false
			now := time.Now()
			h.lastScoutRun = &now
			h.mu.Unlock()
		}()
		h.runScout(req.Model)
	}()

	writeJSON(w, map[string]any{"started": true, "model": req.Model})
}

// RunResearch handles POST /api/pipeline/run/research
// Runs the research agent in the background, writing output to logs/scout.log.
func (h *Handler) RunResearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Model  string `json:"model"`
		RepoID int64  `json:"repo_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	h.mu.Lock()
	if h.researchRunning {
		h.mu.Unlock()
		writeJSON(w, map[string]any{"started": false, "message": "Research agent already running"})
		return
	}
	h.researchRunning = true
	h.mu.Unlock()

	go func() {
		defer func() {
			h.mu.Lock()
			h.researchRunning = false
			now := time.Now()
			h.lastResearchRun = &now
			h.mu.Unlock()
		}()
		if req.RepoID != 0 {
			h.runResearchOne(req.Model, req.RepoID)
		} else {
			h.runResearchBatch(req.Model)
		}
	}()

	writeJSON(w, map[string]any{"started": true, "model": req.Model})
}

// Logs handles GET /api/pipeline/logs — returns last 50 lines of logs/scout.log.
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	logFile := filepath.Join(h.projectRoot, "logs", "scout.log")
	lines := tailFile(logFile, 50)
	writeJSON(w, map[string]any{"lines": lines})
}

// ClearLogs handles POST /api/pipeline/logs/clear — truncates logs/scout.log.
func (h *Handler) ClearLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	logFile := filepath.Join(h.projectRoot, "logs", "scout.log")
	if err := os.Truncate(logFile, 0); err != nil && !os.IsNotExist(err) {
		writeJSON(w, map[string]any{"cleared": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"cleared": true})
}

// Report handles GET /api/pipeline/report/{id} — returns the stored markdown
// research report for a repo.
func (h *Handler) Report(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid repo id", http.StatusBadRequest)
		return
	}
	fullName, report, err := h.db.GetResearchReport(id)
	if err != nil {
		http.Error(w, "repo not found", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"full_name": fullName, "report": report})
}

// ─── Log helper ───────────────────────────────────────────────────────────────

// agentLog appends timestamped output to logs/scout.log (the file the
// dashboard log panel tails).
type agentLog struct {
	f *os.File
}

func (h *Handler) openLog() *agentLog {
	logFile := filepath.Join(h.projectRoot, "logs", "scout.log")
	_ = os.MkdirAll(filepath.Dir(logFile), 0755)
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		h.logger.Error("pipeline: cannot open log file", zap.Error(err))
		return &agentLog{}
	}
	return &agentLog{f: f}
}

func (l *agentLog) printf(format string, args ...any) {
	if l.f != nil {
		fmt.Fprintf(l.f, format+"\n", args...)
	}
}

func (l *agentLog) close() {
	if l.f != nil {
		l.f.Close()
	}
}

// ─── Scout agent ──────────────────────────────────────────────────────────────

var scoutQueries = []string{
	"ai trading agent",
	"multi-agent trading framework",
	"quantitative trading backtesting framework",
	"llm trading signals",
	"interactive brokers trading bot",
	"algorithmic trading strategy",
}

var scoutLanguages = map[string]bool{
	"Python": true, "Go": true, "TypeScript": true,
	"JavaScript": true, "Rust": true, "Jupyter Notebook": true,
}

type ghRepo struct {
	FullName    string   `json:"full_name"`
	HTMLURL     string   `json:"html_url"`
	Description string   `json:"description"`
	Stars       int      `json:"stargazers_count"`
	Language    string   `json:"language"`
	Topics      []string `json:"topics"`
	PushedAt    string   `json:"pushed_at"`
}

const scoutClassifySystem = `You are the Scout Agent for MarketFlow AI, an automated trading system
(React dashboard, Go backend, Python LangGraph brain, Alpaca execution, momentum breakout +
mean reversion strategies). You will receive a JSON list of GitHub repositories related to
trading/quant/AI. Classify each repo:

- "good"     — clearly relevant to MarketFlow AI's stack: multi-agent trading, LLM-driven
               signal generation, LangGraph-style orchestration, IBKR/Alpaca execution,
               or backtesting frameworks.
- "rejected" — trading/finance adjacent but not useful (generic frameworks with no finance
               tie, price-display apps, abandoned tutorials, crypto-only pump bots, etc.).
               Give a one-line reason.

Respond with ONLY a JSON array, no prose, one object per input repo:
[{"full_name": "...", "status": "good"|"rejected", "reason": "..."}]`

func (h *Handler) runScout(model string) {
	log := h.openLog()
	defer log.close()

	log.printf("\n=== Scout Agent — run triggered at %s (model: %s) ===", time.Now().Format(time.RFC3339), model)

	if err := validateModel(model); err != nil {
		log.printf("=== Scout Agent FAILED: %v ===", err)
		return
	}

	// 1. Search GitHub.
	cutoff := time.Now().AddDate(-1, 0, 0).Format("2006-01-02")
	seen := map[string]*ghRepo{}
	for _, q := range scoutQueries {
		repos, err := h.searchGitHub(q, cutoff)
		if err != nil {
			log.printf("WARN: GitHub search %q failed: %v", q, err)
			continue
		}
		fresh := 0
		for i := range repos {
			r := &repos[i]
			if seen[r.FullName] == nil && scoutLanguages[r.Language] {
				seen[r.FullName] = r
				fresh++
			}
		}
		log.printf("Searched %q — %d results, %d candidates after filters", q, len(repos), fresh)
		time.Sleep(2 * time.Second) // stay under GitHub's unauthenticated search rate limit
	}

	// 2. Dedupe against the database.
	var newRepos []*ghRepo
	known := 0
	for _, r := range seen {
		if _, found := h.db.GetRepoStatus(r.FullName); found {
			_ = h.db.TouchRepo(r.FullName, r.Stars)
			known++
			continue
		}
		newRepos = append(newRepos, r)
	}
	log.printf("Checked %d repos: %d already known (stars refreshed), %d new", len(seen), known, len(newRepos))

	if len(newRepos) == 0 {
		log.printf("=== Scout Agent complete — nothing new to classify ===")
		return
	}

	// 3. Classify new repos with the selected model, in batches.
	good, rejected, unclassified := 0, 0, 0
	for start := 0; start < len(newRepos); start += 8 {
		end := start + 8
		if end > len(newRepos) {
			end = len(newRepos)
		}
		batch := newRepos[start:end]

		verdicts, provider, err := h.classifyBatch(model, batch)
		if err != nil {
			log.printf("WARN: classification batch failed (%v) — storing %d repos as 'new' for a later re-run", err, len(batch))
			for _, r := range batch {
				_ = h.db.InsertScoutRepo(toScoutRepo(r), "new", "")
				unclassified++
			}
			continue
		}
		log.printf("Classified batch of %d via %s", len(batch), provider)

		for _, r := range batch {
			v, ok := verdicts[r.FullName]
			status, reason := "new", ""
			if ok && (v.Status == "good" || v.Status == "rejected") {
				status, reason = v.Status, v.Reason
			}
			if err := h.db.InsertScoutRepo(toScoutRepo(r), status, reason); err != nil {
				log.printf("WARN: insert %s failed: %v", r.FullName, err)
				continue
			}
			switch status {
			case "good":
				good++
				log.printf("  + good     %s (stars %d)", r.FullName, r.Stars)
			case "rejected":
				rejected++
				log.printf("  - rejected %s — %s", r.FullName, reason)
			default:
				unclassified++
				log.printf("  ? new      %s (model gave no verdict)", r.FullName)
			}
		}
	}

	log.printf("Summary: %d new repos — %d good, %d rejected, %d left as 'new'", len(newRepos), good, rejected, unclassified)
	log.printf("=== Scout Agent complete ===")
}

type verdict struct {
	FullName string `json:"full_name"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}

func (h *Handler) classifyBatch(model string, batch []*ghRepo) (map[string]verdict, string, error) {
	type item struct {
		FullName    string   `json:"full_name"`
		Description string   `json:"description"`
		Stars       int      `json:"stars"`
		Language    string   `json:"language"`
		Topics      []string `json:"topics"`
		LastPush    string   `json:"last_push"`
	}
	items := make([]item, 0, len(batch))
	for _, r := range batch {
		items = append(items, item{r.FullName, r.Description, r.Stars, r.Language, r.Topics, r.PushedAt})
	}
	payload, _ := json.MarshalIndent(items, "", "  ")

	var lastErr error
	var provider string
	for attempt := 0; attempt < 2; attempt++ {
		reply, prov, err := h.llm.Call(model, scoutClassifySystem, string(payload))
		provider = prov
		if err != nil {
			lastErr = err
			continue
		}
		jsonStr, err := llm.ExtractJSONArray(reply)
		if err != nil {
			lastErr = err
			continue
		}
		var verdicts []verdict
		if err := json.Unmarshal([]byte(jsonStr), &verdicts); err != nil {
			lastErr = fmt.Errorf("model returned invalid JSON: %w", err)
			continue
		}
		out := make(map[string]verdict, len(verdicts))
		for _, v := range verdicts {
			out[v.FullName] = v
		}
		return out, provider, nil
	}
	return nil, provider, lastErr
}

func toScoutRepo(r *ghRepo) *db.ScoutRepo {
	topics, _ := json.Marshal(r.Topics)
	return &db.ScoutRepo{
		FullName:    r.FullName,
		URL:         r.HTMLURL,
		Description: r.Description,
		Stars:       r.Stars,
		Language:    r.Language,
		Topics:      string(topics),
		LastCommit:  r.PushedAt,
	}
}

func (h *Handler) searchGitHub(query, pushedAfter string) ([]ghRepo, error) {
	q := fmt.Sprintf("%s stars:>=100 pushed:>=%s", query, pushedAfter)
	u := "https://api.github.com/search/repositories?q=" + url.QueryEscape(q) + "&sort=stars&order=desc&per_page=15"

	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API %d: %.200s", resp.StatusCode, string(body))
	}

	var result struct {
		Items []ghRepo `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// ─── Research agent ───────────────────────────────────────────────────────────

const researchPerRun = 3

const researchSystem = `You are the Research Agent for MarketFlow AI — an automated trading system with
a React dashboard, Go backend, Python LangGraph AI brain, local Ollama + cloud LLM inference,
Alpaca paper-trading execution, a human-in-the-loop Green Light approval gate, and two
strategies (momentum breakout, mean reversion). Backtesting infrastructure is the current #1 priority.

You will receive one GitHub repository's metadata and README. Write a concise research report
in markdown with EXACTLY these sections:

## Summary
2-3 plain-language sentences: what problem the repo solves.

## Architecture
Agent structure, orchestration pattern, language/stack.

## Execution model
Does it auto-execute trades or support human-in-the-loop approval? (Critical for MarketFlow's
Green Light Gate.) Broker/exchange integrations (IBKR, Alpaca, etc.).

## LLM / model usage
Local (Ollama) vs API models, if any.

## Backtesting
What backtesting support exists, if any.

## Relevance to MarketFlow AI
Name the specific phase or component it could inform or replace.

## Recommendation
One of: **Adopt pattern** / **Reference only** / **Not relevant** — with one sentence why.`

// runResearchBatch researches up to researchPerRun candidates, highest stars first.
func (h *Handler) runResearchBatch(model string) {
	log := h.openLog()
	defer log.close()

	log.printf("\n=== Research Agent — run triggered at %s (model: %s) ===", time.Now().Format(time.RFC3339), model)

	if err := validateModel(model); err != nil {
		log.printf("=== Research Agent FAILED: %v ===", err)
		return
	}

	candidates, err := h.db.ListResearchCandidates(researchPerRun)
	if err != nil {
		log.printf("=== Research Agent FAILED: db error: %v ===", err)
		return
	}
	if len(candidates) == 0 {
		log.printf("No new repos to research (need status='good' with no report yet)")
		log.printf("=== Research Agent complete ===")
		return
	}
	log.printf("Found %d repo(s) to research (max %d per run)", len(candidates), researchPerRun)
	h.researchCandidates(log, model, candidates)
}

// runResearchOne researches a single repo, triggered from a table row.
func (h *Handler) runResearchOne(model string, repoID int64) {
	log := h.openLog()
	defer log.close()

	log.printf("\n=== Research Agent — single-repo run triggered at %s (model: %s) ===", time.Now().Format(time.RFC3339), model)

	if err := validateModel(model); err != nil {
		log.printf("=== Research Agent FAILED: %v ===", err)
		return
	}

	repo, found, err := h.db.GetResearchCandidate(repoID)
	if err != nil {
		log.printf("=== Research Agent FAILED: db error: %v ===", err)
		return
	}
	if !found {
		log.printf("=== Research Agent FAILED: repo %d is not a pending 'good' candidate ===", repoID)
		return
	}
	h.researchCandidates(log, model, []*db.ScoutRepo{repo})
}

func (h *Handler) researchCandidates(log *agentLog, model string, candidates []*db.ScoutRepo) {
	notionReady := os.Getenv("NOTION_API_KEY") != "" && os.Getenv("NOTION_PARENT_PAGE_ID") != ""
	if !notionReady {
		log.printf("Notion export off (set NOTION_API_KEY + NOTION_PARENT_PAGE_ID to enable) — reports are saved to the dashboard")
	}

	done := 0
	for _, repo := range candidates {
		log.printf("Researching %s (stars %d)…", repo.FullName, repo.Stars)

		readme, err := h.fetchReadme(repo.FullName)
		if err != nil {
			log.printf("WARN: could not fetch README for %s: %v (analyzing metadata only)", repo.FullName, err)
		}
		maxReadme := 6000
		if model == llm.ModelClaudeSonnet {
			maxReadme = 16000
		}
		if len(readme) > maxReadme {
			readme = readme[:maxReadme] + "\n\n[README truncated]"
		}

		user := fmt.Sprintf("Repository: %s\nURL: %s\nStars: %d\nLanguage: %s\nTopics: %s\nDescription: %s\n\n--- README ---\n%s",
			repo.FullName, repo.URL, repo.Stars, repo.Language, repo.Topics, repo.Description, readme)

		reply, provider, err := h.llm.Call(model, researchSystem, user)
		if err != nil {
			log.printf("WARN: analysis of %s failed: %v — will retry next run", repo.FullName, err)
			continue
		}
		report := llm.StripThink(reply)
		header := fmt.Sprintf("# %s — ⭐ %d\n\n**Link:** %s\n**Researched:** %s\n**Model:** %s\n\n",
			repo.FullName, repo.Stars, repo.URL, time.Now().Format("2006-01-02"), provider)
		report = header + report

		notionURL := ""
		if notionReady {
			notionURL, err = h.writeNotionPage(repo.FullName, report)
			if err != nil {
				log.printf("WARN: Notion write for %s failed: %v (report still saved to dashboard)", repo.FullName, err)
			} else {
				log.printf("  Notion page created: %s", notionURL)
			}
		}

		if err := h.db.SaveResearchReport(repo.ID, report, notionURL); err != nil {
			log.printf("WARN: saving report for %s failed: %v", repo.FullName, err)
			continue
		}
		done++
		log.printf("  + %s researched via %s — report available in the dashboard", repo.FullName, provider)
	}

	log.printf("Summary: %d/%d repos researched", done, len(candidates))
	log.printf("=== Research Agent complete ===")
}

func (h *Handler) fetchReadme(fullName string) (string, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+fullName+"/readme", nil)
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub API %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	return string(body), err
}

// writeNotionPage creates a child page under NOTION_PARENT_PAGE_ID with the
// report as paragraph blocks (Notion caps rich_text at 2000 chars per block).
func (h *Handler) writeNotionPage(title, markdown string) (string, error) {
	var children []map[string]any
	for _, chunk := range chunkString(markdown, 1900) {
		children = append(children, map[string]any{
			"object": "block",
			"type":   "paragraph",
			"paragraph": map[string]any{
				"rich_text": []map[string]any{
					{"type": "text", "text": map[string]any{"content": chunk}},
				},
			},
		})
	}

	payload := map[string]any{
		"parent": map[string]any{"page_id": os.Getenv("NOTION_PARENT_PAGE_ID")},
		"properties": map[string]any{
			"title": map[string]any{
				"title": []map[string]any{
					{"type": "text", "text": map[string]any{"content": "OSS Research: " + title}},
				},
			},
		},
		"children": children,
	}
	b, _ := json.Marshal(payload)

	req, _ := http.NewRequest("POST", "https://api.notion.com/v1/pages", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("NOTION_API_KEY"))
	req.Header.Set("Notion-Version", "2022-06-28")

	resp, err := h.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Notion API %d: %.200s", resp.StatusCode, string(body))
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return result.URL, nil
}

func chunkString(s string, size int) []string {
	var chunks []string
	for len(s) > size {
		cut := strings.LastIndex(s[:size], "\n")
		if cut <= 0 {
			cut = size
		}
		chunks = append(chunks, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		chunks = append(chunks, s)
	}
	return chunks
}

// validateModel rejects unknown model values up front so the failure is a
// clear log line instead of a mid-run surprise.
func validateModel(model string) error {
	for _, m := range llm.KnownModels {
		if m == model {
			return nil
		}
	}
	return fmt.Errorf("unknown model %q (expected one of: %s)", model, strings.Join(llm.KnownModels, ", "))
}

func tailFile(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return []string{}
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
