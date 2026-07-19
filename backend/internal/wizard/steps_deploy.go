package wizard

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
	"github.com/wrcron2/market-ai-factory/backend/internal/orchestrator"
)

// ─── Hangar 5: deploy ────────────────────────────────────────────────────────
// Adopted products (Market-AI): fast path — the deploy already exists; the
// user provides its URLs. New products: clone at the pinned SHA and
// docker-compose up on the allocated port range.

type Deploy struct{}

func (Deploy) ID() string    { return "deploy" }
func (Deploy) Title() string { return "Deploy product stack" }
func (Deploy) NeedsInput() []string {
	// Only consumed on the adopted fast path; automatic otherwise.
	return []string{"dashboard_url", "health_url"}
}

func (Deploy) Execute(ctx *RunContext) error {
	if adopted, _ := ctx.State["adopted"].(bool); adopted {
		if u := ctx.Input["dashboard_url"]; u != "" {
			ctx.State["dashboard_url"] = u
		}
		if u := ctx.Input["health_url"]; u != "" {
			ctx.State["health_url"] = u
		}
		return nil
	}
	dir := fmt.Sprintf("%s/%s", ctx.WorkRoot, ctx.Run.ProductName)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := orchestrator.CloneAt(ctx.Run.SourceRepo, ctx.StateStr("source_sha"), dir); err != nil {
			ctx.State["deploy_error"] = err.Error()
			return nil
		}
	}
	composeFile := "docker-compose.yml"
	if _, err := os.Stat(dir + "/" + composeFile); err != nil {
		ctx.State["deploy_error"] = "repo has no docker-compose.yml at its root"
		return nil
	}
	if out, err := orchestrator.ComposeUp(dir, composeFile); err != nil {
		ctx.State["deploy_error"] = fmt.Sprintf("%v — %s", err, out)
		return nil
	}
	delete(ctx.State, "deploy_error")
	ctx.State["deployed"] = true
	return nil
}

func (Deploy) Check(ctx *RunContext) []Issue {
	if adopted, _ := ctx.State["adopted"].(bool); adopted {
		if ctx.StateStr("health_url") == "" {
			return []Issue{{
				Code: "adopt_urls_missing", Message: "adopted product needs its existing dashboard and health URLs",
				Hint: "e.g. dashboard http://129.159.146.157:3000, health http://129.159.146.157:8080/api/orders/pending",
			}}
		}
		return nil
	}
	if msg, _ := ctx.State["deploy_error"].(string); msg != "" {
		return []Issue{{Code: "deploy_failed", Message: msg, Hint: "Fix the repo/compose problem, then Refresh."}}
	}
	if ok, _ := ctx.State["deployed"].(bool); !ok {
		return []Issue{{Code: "not_deployed", Message: "product stack not deployed yet"}}
	}
	return nil
}

// ─── Hangar 6: verify_health ─────────────────────────────────────────────────
// A green build is not a deploy: probe the product's health URL for a 200.

type VerifyHealth struct{}

func (VerifyHealth) ID() string          { return "verify_health" }
func (VerifyHealth) Title() string       { return "Verify live health" }
func (VerifyHealth) NeedsInput() []string { return nil }

func (VerifyHealth) Execute(ctx *RunContext) error { return nil } // pure check

func (VerifyHealth) Check(ctx *RunContext) []Issue {
	url := ctx.StateStr("health_url")
	if url == "" {
		return []Issue{{Code: "no_health_url", Message: "no health URL recorded by the deploy step"}}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return []Issue{{Code: "health_unreachable", Message: err.Error(), Hint: "Is the product's stack running? Fix and Refresh."}}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return []Issue{{Code: "health_not_ok", Message: fmt.Sprintf("health endpoint returned HTTP %d", resp.StatusCode)}}
	}
	return nil
}

// ─── Hangar 7: publish ───────────────────────────────────────────────────────
// Registers the product LIVE — it appears on the Products grid.

type Publish struct{}

func (Publish) ID() string          { return "publish" }
func (Publish) Title() string       { return "Publish product" }
func (Publish) NeedsInput() []string { return nil }

func (Publish) Execute(ctx *RunContext) error {
	budget, _ := ctx.State["budget_usd"].(float64)
	portBase := 0
	if v, ok := ctx.State["port_base"].(float64); ok { // JSON round-trip makes it float64
		portBase = int(v)
	}
	adopted, _ := ctx.State["adopted"].(bool)
	p := &db.Product{
		Name:         ctx.Run.ProductName,
		DisplayName:  ctx.Run.ProductName,
		SourceRepo:   ctx.Run.SourceRepo,
		SourceSHA:    ctx.StateStr("source_sha"),
		Status:       db.StatusLive,
		PortBase:     portBase,
		BudgetUSD:    budget,
		DashboardURL: ctx.StateStr("dashboard_url"),
		HealthURL:    ctx.StateStr("health_url"),
		AlpacaKeyID:  ctx.StateStr("alpaca_key_id"),
		Adopted:      adopted,
	}
	if existing, err := ctx.DB.GetProduct(p.Name); err == nil && existing != nil {
		return ctx.DB.UpdateProduct(p)
	}
	_, err := ctx.DB.InsertProduct(p)
	return err
}

func (Publish) Check(ctx *RunContext) []Issue {
	p, err := ctx.DB.GetProduct(ctx.Run.ProductName)
	if err != nil || p == nil {
		return []Issue{{Code: "not_registered", Message: "product not in registry yet"}}
	}
	if p.Status != db.StatusLive {
		return []Issue{{Code: "not_live", Message: fmt.Sprintf("product status is %s", p.Status)}}
	}
	return nil
}

