package wizard

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
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

	// connect_alpaca (Hangar 3) writes the product's credentials to
	// products/<name>/.env — but compose up runs from the clone dir, so the
	// product's own docker-compose.yml (if it declares env_file: .env) can't
	// see them unless we carry the file over. Best-effort: some products may
	// not use Alpaca creds at all, so a missing source file isn't fatal here.
	envSrc := ctx.RepoRoot + "/products/" + ctx.Run.ProductName + "/.env"
	if data, err := os.ReadFile(envSrc); err == nil {
		if err := os.WriteFile(dir+"/.env", data, 0o600); err != nil {
			ctx.State["deploy_error"] = "copying product .env into deploy dir: " + err.Error()
			return nil
		}
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
	//
	// The dashboard auth token is deliberately NOT collected here — it's a
	// secret, and this step persists its inputs into run state (which the
	// run-status API serves verbatim). Secrets ride in Input to the step
	// that consumes them, never into persisted State; the token is captured
	// at Publish instead. See that step's NeedsInput.
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
		// The health probe runs from the backend host's network
		// namespace — it can reach internal Docker URLs (e.g.
		// http://openalice:47331/) directly. If the URL is bad, a
		// public IP that's blocked, or the stack isn't up yet, this
		// surfaces it. Either way, the user gets an actionable hint.
		hint := "Is the product's stack running? Fix and Refresh."
		if pm, ok := ctx.State["port_map"].(map[string]any); ok && len(pm) > 0 {
			parts := make([]string, 0, len(pm))
			for svc, v := range pm {
				entry, _ := v.(map[string]any)
				parts = append(parts, fmt.Sprintf("%s → %v (published at host port %v)", svc, entry["internal_url"], entry["host_port"]))
			}
			sort.Strings(parts)
			hint += " Prefer the internal_url — it works immediately, no firewall rule needed: " + strings.Join(parts, "; ") + "."
		}
		return []Issue{{Code: "health_unreachable", Message: err.Error(), Hint: hint}}
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

func (Publish) ID() string    { return "publish" }
func (Publish) Title() string { return "Publish product" }

// NeedsInput collects the dashboard auth token HERE (not at verify_health)
// precisely because it's a secret. Every step persists its inputs into run
// state, and the run-status API serves that state verbatim — so a secret
// captured at an earlier step would be written to the DB scratchpad and
// leaked back over the wire. Publish writes the token straight to the
// product row (json:"-", never serialized) and never into State, mirroring
// how alpaca_secret already flows through Input into the product's .env.
// The field is optional: leave it blank for a public dashboard.
func (Publish) NeedsInput() []string { return []string{"dashboard_auth_token"} }

func (Publish) Execute(ctx *RunContext) error {
	budget, _ := ctx.State["budget_usd"].(float64)
	portBase := 0
	if v, ok := ctx.State["port_base"].(float64); ok { // JSON round-trip makes it float64
		portBase = int(v)
	}
	adopted, _ := ctx.State["adopted"].(bool)
	dashboardURL := ctx.StateStr("dashboard_url")
	healthURL := ctx.StateStr("health_url")
	// The token is a write-only secret: it arrives via Input at this step
	// and lands only in the product row. The login path it's POSTed to is
	// non-secret and defaults to OpenAlice's shape; override via Input only
	// for a differently-shaped product. Both are left as the raw Input here
	// (possibly blank) — defaulting and preserve-on-blank happen once the
	// existing row is known, below.
	authToken := strings.TrimSpace(ctx.Input["dashboard_auth_token"])
	authLogin := strings.TrimSpace(ctx.Input["dashboard_auth_login"])
	// For non-adopted products, rewrite a public-IP host:port in the
	// dashboard_url/health_url the human entered back to the internal
	// container URL the deploy step recorded in port_map. This is the
	// whole point of Plan B: the dashboard_url the proxy routes to
	// should never need a published host port or a per-product cloud
	// firewall rule — Docker DNS is enough. A user who copy-pastes the
	// wizard's published-port hint into the field (which is a natural
	// thing to do) would otherwise silently route via hairpin NAT, the
	// failure mode that bit open-alice. Adopted products are exempt:
	// their dashboard_url is pre-existing public infrastructure (e.g.
	// market-ai's own nginx), the proxy just routes through it.
	if !adopted {
		if pm, ok := ctx.State["port_map"].(map[string]any); ok && len(pm) > 0 {
			dashboardURL = rewriteToInternalURL(dashboardURL, pm)
			healthURL = rewriteToInternalURL(healthURL, pm)
		}
	}
	p := &db.Product{
		Name:               ctx.Run.ProductName,
		DisplayName:        ctx.Run.ProductName,
		SourceRepo:         ctx.Run.SourceRepo,
		SourceSHA:          ctx.StateStr("source_sha"),
		Status:             db.StatusLive,
		PortBase:           portBase,
		BudgetUSD:          budget,
		DashboardURL:       dashboardURL,
		DashboardAuthLogin: authLogin,
		DashboardAuthToken: authToken,
		HealthURL:          healthURL,
		AlpacaKeyID:        ctx.StateStr("alpaca_key_id"),
		Adopted:            adopted,
	}
	existing, err := ctx.DB.GetProduct(p.Name)
	isUpdate := err == nil && existing != nil
	if isUpdate {
		// Runs are replayable, so Publish can execute more than once. Both
		// auth fields are optional: an empty Input here means "unchanged",
		// not "clear it" — don't blank a value a prior run already stored.
		if authToken == "" {
			p.DashboardAuthToken = existing.DashboardAuthToken
		}
		if authLogin == "" {
			p.DashboardAuthLogin = existing.DashboardAuthLogin
		}
	}
	// Default the login path only when it's genuinely unset (fresh product,
	// or an old row that predates the column) — never override a value the
	// preserve-on-blank branch just carried forward.
	if p.DashboardAuthLogin == "" {
		p.DashboardAuthLogin = "/api/auth/login"
	}
	if isUpdate {
		return ctx.DB.UpdateProduct(p)
	}
	_, err = ctx.DB.InsertProduct(p)
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

// rewriteToInternalURL replaces a public-IP host:port in a URL with the
// internal container URL recorded in port_map, when the URL's port
// matches a port_map entry's host_port. The path/query/fragment are
// preserved verbatim; only the host:port is swapped. Returns the input
// unchanged when:
//   - the URL is empty
//   - the URL has no port (can't match anything)
//   - the URL's port doesn't match any port_map host_port
//   - the port_map is empty
//   - the URL fails to parse (e.g. a malformed input)
//
// port_map shape (from Deploy.Execute):
//
//	{
//	  "openalice:47331": {
//	    "internal_url": "http://openalice:47331",
//	    "host_port":    10100            // float64 from JSON round-trip
//	  }
//	}
//
// Example: "http://129.159.146.157:10100/login" →
// "http://openalice:47331/login" when port_map has an entry whose host_port
// is 10100.
//
// Defense in depth: even though the wizard records port_map from the
// deploy step's own output, treat the port_map's internal_url as
// untrusted — it's user-adjacent (the repo's compose file declared the
// container port and name). Parse it; if it doesn't yield a clean
// host:port, skip the rewrite and leave the original URL alone.
func rewriteToInternalURL(raw string, portMap map[string]any) string {
	if raw == "" || len(portMap) == 0 {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	portStr := u.Port()
	if portStr == "" {
		// No port in the URL — can't match a host_port in port_map.
		// (An internal Docker URL like http://openalice:47331/ already
		// has a port; a public IP without a port is almost always :80
		// or :443 which is never in the per-product port range anyway.)
		return raw
	}
	hostPort, err := strconv.Atoi(portStr)
	if err != nil {
		return raw
	}
	for _, v := range portMap {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		hp, _ := entry["host_port"].(float64) // JSON round-trips ints as float64
		if int(hp) != hostPort {
			continue
		}
		internalURL, _ := entry["internal_url"].(string)
		if internalURL == "" {
			continue
		}
		iu, err := url.Parse(internalURL)
		if err != nil || iu.Host == "" {
			continue
		}
		// Adopt the internal_url's scheme AND host, not just the host:
		// we're now targeting the internal Docker network, which speaks
		// plain http. Keeping the operator's original scheme would turn
		// a pasted "https://<public-ip>:<port>" into
		// "https://<container>:<port>", and every later health probe and
		// proxy hop would fail a TLS handshake against a plaintext
		// container — a 502 the onboarding flow never surfaces because
		// verify_health still checked the original public URL.
		u.Scheme = iu.Scheme
		u.Host = iu.Host
		return u.String()
	}
	return raw
}
