// Package registry exposes the product registry over REST. It is the read
// side of the Factory: the wizard is the only writer that creates products;
// pause/resume and monitor flip statuses.
package registry

import (
	"encoding/json"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
	"github.com/wrcron2/market-ai-factory/backend/internal/orchestrator"
)

type Handler struct {
	db       *db.DB
	logger   *zap.Logger
	metrics  *MetricsProvider
	workRoot string // where non-adopted product stacks live (compose dirs)
	sessions *sessionCache
}

func New(database *db.DB, logger *zap.Logger, metrics *MetricsProvider, workRoot string) *Handler {
	return &Handler{
		db:       database,
		logger:   logger,
		metrics:  metrics,
		workRoot: workRoot,
		sessions: newSessionCache(logger),
	}
}

// productView is a Product enriched with live Alpaca metrics for the cards.
type productView struct {
	*db.Product
	Metrics *Metrics `json:"metrics,omitempty"`
}

// Products handles GET /api/products — the Products grid datasource.
func (h *Handler) Products(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	products, err := h.db.ListProducts()
	if err != nil {
		h.logger.Error("registry.list_failed", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	views := make([]productView, 0, len(products))
	for _, p := range products {
		views = append(views, productView{Product: p, Metrics: h.metrics.For(p)})
	}
	writeJSON(w, map[string]any{"products": views})
}

// Product routes /api/products/{name}[/pause|/resume] — drill-down data and
// the per-product kill switch.
func (h *Handler) Product(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/products/")
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if name == "" {
		http.Error(w, "product name required", http.StatusBadRequest)
		return
	}
	p, err := h.db.GetProduct(name)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		checks, err := h.db.ListProductChecks(p.ID, 24)
		if err != nil {
			h.logger.Error("registry.checks_failed", zap.String("product", name), zap.Error(err))
		}
		if checks == nil {
			checks = []*db.ProductCheck{}
		}
		report, reportAt := h.db.LatestAIReport(p.ID)
		writeJSON(w, map[string]any{
			"product":      productView{Product: p, Metrics: h.metrics.For(p)},
			"checks":       checks,
			"ai_report":    report,
			"ai_report_at": reportAt,
		})

	case action == "proxy" || strings.HasPrefix(action, "proxy/"):
		// Dashboard reverse-proxy — see proxy.go. r.URL.Path is
		// already in the canonical /api/products/<name>/proxy/<rest>
		// shape here (this handler only trimmed the prefix into the
		// local `rest` var, not into r.URL.Path itself), so ServeProxy
		// can re-derive it from the request as written. Branching here
		// (rather than registering a separate mux route) keeps
		// /api/products/{name}[/action] under one path-dispatch site
		// where order is obvious — pause/resume and proxy share the
		// same <name> lookup, which "product not found" expects.
		h.ServeProxy(w, r)

	case (action == "pause" || action == "resume") && r.Method == http.MethodPost:
		h.pauseResume(w, p, action)

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// pauseResume flips trading state. Non-adopted products also get their
// compose stack stopped/started; adopted products (their deploy predates the
// factory) only change registry status — the factory must never knock over
// a stack it doesn't own.
func (h *Handler) pauseResume(w http.ResponseWriter, p *db.Product, action string) {
	target := db.StatusPaused
	if action == "resume" {
		target = db.StatusLive
	}
	if !p.Adopted && h.workRoot != "" {
		dir := h.workRoot + "/" + p.Name
		files := orchestrator.ComposeFiles(dir) // includes the port-remap override, if any
		var out string
		var err error
		if action == "pause" {
			out, err = orchestrator.ComposeDown(dir, files...)
		} else {
			out, err = orchestrator.ComposeUp(dir, files...)
		}
		if err != nil {
			h.logger.Error("registry."+action+"_compose_failed", zap.String("product", p.Name), zap.Error(err))
			http.Error(w, "compose "+action+" failed: "+out, http.StatusInternalServerError)
			return
		}
	}
	if err := h.db.UpdateProductStatus(p.Name, target); err != nil {
		h.fail(w, action, err)
		return
	}
	h.logger.Info("registry.product_"+action, zap.String("product", p.Name))
	writeJSON(w, map[string]any{"name": p.Name, "status": target})
}

// KillAll handles POST /api/killall — pauses every LIVE product.
func (h *Handler) KillAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	products, err := h.db.ListProducts()
	if err != nil {
		h.fail(w, "killall", err)
		return
	}
	paused := []string{}
	for _, p := range products {
		if p.Status != db.StatusLive {
			continue
		}
		if !p.Adopted && h.workRoot != "" {
			dir := h.workRoot + "/" + p.Name
			if _, err := orchestrator.ComposeDown(dir, orchestrator.ComposeFiles(dir)...); err != nil {
				h.logger.Error("registry.killall_compose_failed", zap.String("product", p.Name), zap.Error(err))
			}
		}
		if err := h.db.UpdateProductStatus(p.Name, db.StatusPaused); err == nil {
			paused = append(paused, p.Name)
		}
	}
	h.logger.Warn("registry.killall", zap.Strings("paused", paused))
	writeJSON(w, map[string]any{"paused": paused})
}

func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	h.logger.Error("registry."+op+"_failed", zap.Error(err))
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
