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
)

type Handler struct {
	db      *db.DB
	logger  *zap.Logger
	metrics *MetricsProvider
}

func New(database *db.DB, logger *zap.Logger, metrics *MetricsProvider) *Handler {
	return &Handler{db: database, logger: logger, metrics: metrics}
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

// Product handles GET /api/products/{name} — drill-down header data.
func (h *Handler) Product(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/products/")
	name = strings.SplitN(name, "/", 2)[0]
	if name == "" {
		http.Error(w, "product name required", http.StatusBadRequest)
		return
	}
	p, err := h.db.GetProduct(name)
	if err != nil {
		http.Error(w, "product not found", http.StatusNotFound)
		return
	}
	checks, err := h.db.ListProductChecks(p.ID, 24)
	if err != nil {
		h.logger.Error("registry.checks_failed", zap.String("product", name), zap.Error(err))
		checks = nil
	}
	if checks == nil {
		checks = []*db.ProductCheck{}
	}
	writeJSON(w, map[string]any{
		"product": productView{Product: p, Metrics: h.metrics.For(p)},
		"checks":  checks,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
