// Package monitor is each product's ops team: deterministic health checks
// every 2 hours, and once a day an AI reviewer that reads the last 24h of
// evidence and writes a short report. Failures flip the product card to
// ERROR and (on budget-floor breach) auto-pause trading — capital
// preservation beats uptime.
package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/alpaca"
	"github.com/wrcron2/market-ai-factory/backend/internal/db"
	"github.com/wrcron2/market-ai-factory/backend/internal/llm"
	"github.com/wrcron2/market-ai-factory/backend/internal/registry"
)

// BudgetFloorPct: equity below this fraction of the allocated budget pauses
// the product (mirrors Market-AI's -15% drawdown suspend).
const BudgetFloorPct = 0.85

type Monitor struct {
	db       *db.DB
	logger   *zap.Logger
	alpaca   *alpaca.Client
	llm      *llm.Client
	repoRoot string
	http     *http.Client

	CheckEvery  time.Duration
	ReviewAfter int // local hour after which the daily AI review may run
	AIModel     string
}

func New(database *db.DB, logger *zap.Logger, alpacaClient *alpaca.Client, llmClient *llm.Client, repoRoot string) *Monitor {
	return &Monitor{
		db: database, logger: logger, alpaca: alpacaClient, llm: llmClient,
		repoRoot: repoRoot, http: &http.Client{Timeout: 15 * time.Second},
		CheckEvery: 2 * time.Hour, ReviewAfter: 8, AIModel: "claude-sonnet",
	}
}

// Start runs the loop until the process exits. One immediate pass on boot so
// a freshly deployed factory shows check results without a 2h wait.
func (m *Monitor) Start() {
	go func() {
		m.RunChecks()
		m.maybeDailyReviews()
		t := time.NewTicker(m.CheckEvery)
		defer t.Stop()
		for range t.C {
			m.RunChecks()
			m.maybeDailyReviews()
		}
	}()
}

type checkDetail struct {
	Health string  `json:"health"`
	Alpaca string  `json:"alpaca"`
	Equity float64 `json:"equity,omitempty"`
	Floor  float64 `json:"floor,omitempty"`
	Note   string  `json:"note,omitempty"`
}

// RunChecks checks every LIVE or ERROR product (PAUSED products are left
// alone — pausing is a human/floor decision, recovery is explicit).
func (m *Monitor) RunChecks() {
	products, err := m.db.ListProducts()
	if err != nil {
		m.logger.Error("monitor.list_failed", zap.Error(err))
		return
	}
	for _, p := range products {
		if p.Status != db.StatusLive && p.Status != db.StatusError {
			continue
		}
		ok, detail := m.checkOne(p)
		raw, _ := json.Marshal(detail)
		_ = m.db.InsertProductCheck(p.ID, ok, raw)

		switch {
		case detail.Note == "budget_floor_breached":
			m.logger.Warn("monitor.budget_floor", zap.String("product", p.Name),
				zap.Float64("equity", detail.Equity), zap.Float64("floor", detail.Floor))
			_ = m.db.UpdateProductStatus(p.Name, db.StatusPaused)
		case !ok && p.Status == db.StatusLive:
			_ = m.db.UpdateProductStatus(p.Name, db.StatusError)
		case ok && p.Status == db.StatusError:
			_ = m.db.UpdateProductStatus(p.Name, db.StatusLive)
		}
	}
}

func (m *Monitor) checkOne(p *db.Product) (bool, checkDetail) {
	d := checkDetail{Health: "skipped", Alpaca: "skipped"}
	ok := true

	if p.HealthURL != "" {
		resp, err := m.http.Get(p.HealthURL)
		if err != nil {
			d.Health = "unreachable: " + err.Error()
			ok = false
		} else {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				d.Health = "ok"
			} else {
				d.Health = fmt.Sprintf("http %d", resp.StatusCode)
				ok = false
			}
		}
	}

	keyID, secret := registry.ReadProductCreds(m.repoRoot, p.Name)
	if keyID != "" && secret != "" {
		acct, err := m.alpaca.GetAccount(keyID, secret)
		switch {
		case err != nil:
			d.Alpaca = "error: " + err.Error()
			ok = false
		case acct.Status != "ACTIVE" || acct.TradeBlocked:
			d.Alpaca = fmt.Sprintf("status=%s blocked=%v", acct.Status, acct.TradeBlocked)
			ok = false
		default:
			d.Alpaca = "ok"
			d.Equity = acct.EquityF()
			if p.BudgetUSD > 0 {
				d.Floor = p.BudgetUSD * BudgetFloorPct
				if d.Equity < d.Floor {
					d.Note = "budget_floor_breached"
					ok = false
				}
			}
		}
	}
	return ok, d
}

// maybeDailyReviews writes one AI report per product per day, after
// ReviewAfter o'clock, off the critical path of the deterministic checks.
func (m *Monitor) maybeDailyReviews() {
	if time.Now().Hour() < m.ReviewAfter {
		return
	}
	products, err := m.db.ListProducts()
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	for _, p := range products {
		if p.Status == db.StatusDraft || m.db.HasAIReportSince(p.ID, cutoff) {
			continue
		}
		m.reviewOne(p)
	}
}

func (m *Monitor) reviewOne(p *db.Product) {
	checks, _ := m.db.ListProductChecks(p.ID, 12) // ~24h at 2h cadence
	evidence, _ := json.Marshal(map[string]any{
		"product": p.Name, "status": p.Status, "budget_usd": p.BudgetUSD,
		"adopted": p.Adopted, "checks_last_24h": checks,
	})
	system := `You are the monitor team (QA + DevOps + risk) for one autonomous trading product in the Market-AI Factory. Review the last 24h of monitoring evidence. Report in <150 words: 1) overall verdict (HEALTHY / DEGRADED / AT RISK), 2) anything needing the owner's attention, 3) one concrete recommendation. Hunt silent failures and budget drift; be direct, no filler.`
	reply, provider, err := m.llm.Call(m.AIModel, system, string(evidence))
	if err != nil {
		m.logger.Warn("monitor.ai_review_failed", zap.String("product", p.Name), zap.Error(err))
		return
	}
	_ = m.db.InsertAIReport(p.ID, reply)
	m.logger.Info("monitor.ai_review_done", zap.String("product", p.Name), zap.String("provider", provider))
}
