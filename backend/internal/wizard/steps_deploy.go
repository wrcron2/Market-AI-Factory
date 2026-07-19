package wizard

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

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
	composePath := dir + "/docker-compose.yml"
	if _, err := os.Stat(composePath); err != nil {
		ctx.State["deploy_error"] = "repo has no docker-compose.yml at its root"
		return nil
	}

	// Collision + reachability fix, applied to EVERY product unconditionally:
	// the product's own compose file publishes ports as committed (e.g. a
	// Market-AI-shaped fork reusing :3000/:8080, or even Market-AI's own
	// ${GO_SERVER_PORT:-8080} env-default syntax) and may hardcode an
	// explicit network name (Market-AI's is "marketflow-net") — remap ports
	// into this product's allocated range and rename the network to
	// <product>-net. Every service also gets a stable container_name and an
	// attachment to the shared FactoryNetwork, so the Factory can reach it
	// by name for health checks with no published port and no cloud
	// firewall rule required — internal Docker DNS, live immediately.
	portBase := 10100
	if v, ok := ctx.State["port_base"].(float64); ok && v > 0 {
		portBase = int(v)
	}
	remap, err := remapPublishedPorts(composePath, portBase, ctx.Run.ProductName)
	if err != nil {
		ctx.State["deploy_error"] = "reading compose file: " + err.Error()
		return nil
	}
	if err := writeFactoryCompose(dir, remap.Doc); err != nil {
		ctx.State["deploy_error"] = "writing remapped compose file: " + err.Error()
		return nil
	}
	if err := writePortsInfo(ctx.RepoRoot, ctx.Run.ProductName, remap.Mappings, remap.ContainerNames); err != nil {
		ctx.Logger.Warn("deploy.ports_info_write_failed", zap.Error(err))
	}
	for _, svc := range remap.Overridden {
		ctx.Logger.Warn("deploy.container_name_overridden",
			zap.String("product", ctx.Run.ProductName), zap.String("service", svc),
			zap.String("hint", "repo declared its own container_name; anything referencing it directly by that name will break"))
	}
	portMap := make(map[string]any, len(remap.Mappings))
	for _, m := range remap.Mappings {
		portMap[m.Service+":"+m.ContainerPort] = map[string]any{
			"internal_url": fmt.Sprintf("http://%s:%s", remap.ContainerNames[m.Service], m.ContainerPort),
			"host_port":    m.NewHostPort,
		}
	}
	ctx.State["port_map"] = portMap

	// The generated compose file declares FactoryNetwork as external — it
	// must exist before compose up, regardless of how the Factory itself
	// was started (its own docker-compose creates this implicitly, but a
	// backend run directly, e.g. local dev, would not have).
	if err := orchestrator.EnsureNetwork(orchestrator.FactoryNetwork); err != nil {
		ctx.State["deploy_error"] = factoryInfraErrorPrefix + "ensuring " + orchestrator.FactoryNetwork + " network: " + err.Error()
		return nil
	}

	if out, err := orchestrator.ComposeUp(dir, orchestrator.ComposeFiles(dir)...); err != nil {
		ctx.State["deploy_error"] = fmt.Sprintf("%v — %s", err, out)
		return nil
	}
	delete(ctx.State, "deploy_error")
	ctx.State["deployed"] = true
	return nil
}

// factoryInfraErrorPrefix marks a deploy_error as a Factory-side
// infrastructure problem rather than something wrong with the onboarded
// repo — Deploy.Check gives it a distinct, non-misleading hint.
const factoryInfraErrorPrefix = "factory infra: "

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
		if rest, isInfra := strings.CutPrefix(msg, factoryInfraErrorPrefix); isInfra {
			return []Issue{{Code: "factory_infra_error", Message: rest,
				Hint: "This is a Factory infrastructure problem, not something in your repo — the operator needs to fix it, then Refresh."}}
		}
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

func (VerifyHealth) ID() string    { return "verify_health" }
func (VerifyHealth) Title() string { return "Verify live health" }
func (VerifyHealth) NeedsInput() []string {
	// The deploy step only ever sets health_url on the adopted fast path
	// (Bug: it silently dropped this for newly-cloned products). This step
	// is where a new product's health_url actually gets captured.
	return []string{"health_url", "dashboard_url"}
}

func (VerifyHealth) Execute(ctx *RunContext) error {
	if u := strings.TrimSpace(ctx.Input["health_url"]); u != "" {
		ctx.State["health_url"] = u
	}
	if u := strings.TrimSpace(ctx.Input["dashboard_url"]); u != "" {
		ctx.State["dashboard_url"] = u
	}
	return nil
}

func (VerifyHealth) Check(ctx *RunContext) []Issue {
	url := ctx.StateStr("health_url")
	if url == "" {
		hint := "Set health_url to this product's liveness endpoint, then Continue."
		if pm, ok := ctx.State["port_map"].(map[string]any); ok && len(pm) > 0 {
			parts := make([]string, 0, len(pm))
			for svc, v := range pm {
				entry, _ := v.(map[string]any)
				parts = append(parts, fmt.Sprintf("%s → %v (published at host port %v)", svc, entry["internal_url"], entry["host_port"]))
			}
			sort.Strings(parts)
			hint += " Prefer the internal_url — it works immediately, no firewall rule needed: " + strings.Join(parts, "; ") + "."
		}
		return []Issue{{Code: "no_health_url", Message: "no health URL recorded yet", Hint: hint}}
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

