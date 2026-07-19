package wizard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/alpaca"
	"github.com/wrcron2/market-ai-factory/backend/internal/db"
)

// DefaultSteps wires the seven hangars in order.
func DefaultSteps(client *alpaca.Client) []Step {
	return []Step{
		ValidateRepo{}, CreateSpace{}, ConnectAlpaca{Client: client},
		SetBudget{}, Deploy{}, VerifyHealth{}, Publish{},
	}
}

// Handler exposes the wizard over REST:
//   GET  /api/wizard/steps          — step metadata for the UI stepper
//   GET  /api/wizard/runs           — recent runs
//   POST /api/wizard/runs           — start a run {product_name, source_repo, adopted}
//   GET  /api/wizard/runs/{id}      — run + per-step statuses/issues
//   POST /api/wizard/runs/{id}/advance — execute current step (body = input map)
//   POST /api/wizard/runs/{id}/refresh — re-check current step (body = input map)
type Handler struct {
	engine *Engine
	db     *db.DB
	logger *zap.Logger
}

func NewHandler(engine *Engine, database *db.DB, logger *zap.Logger) *Handler {
	return &Handler{engine: engine, db: database, logger: logger}
}

func (h *Handler) Steps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"steps": h.engine.StepMeta()})
}

func (h *Handler) Runs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runs, err := h.db.ListWizardRuns()
		if err != nil {
			h.fail(w, "list runs", err)
			return
		}
		if runs == nil {
			runs = []*db.WizardRun{}
		}
		writeJSON(w, map[string]any{"runs": runs})
	case http.MethodPost:
		var req struct {
			ProductName string `json:"product_name"`
			SourceRepo  string `json:"source_repo"`
			Adopted     bool   `json:"adopted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProductName == "" || req.SourceRepo == "" {
			http.Error(w, "product_name and source_repo are required", http.StatusBadRequest)
			return
		}
		id, err := h.engine.StartRun(req.ProductName, req.SourceRepo, req.Adopted)
		if err != nil {
			h.fail(w, "start run", err)
			return
		}
		writeJSON(w, map[string]any{"run_id": id})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// RunByID routes /api/wizard/runs/{id}[/advance|/refresh].
func (h *Handler) RunByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/wizard/runs/")
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		run, steps, err := h.db.GetWizardRun(id)
		if err != nil {
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"run": run, "steps": steps})

	case (action == "advance" || action == "refresh") && r.Method == http.MethodPost:
		input := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&input) // empty body is fine
		var opErr error
		if action == "advance" {
			opErr = h.engine.Advance(id, input)
		} else {
			opErr = h.engine.Refresh(id, input)
		}
		if opErr != nil {
			h.fail(w, action, opErr)
			return
		}
		run, steps, err := h.db.GetWizardRun(id)
		if err != nil {
			h.fail(w, "reload run", err)
			return
		}
		writeJSON(w, map[string]any{"run": run, "steps": steps})

	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	h.logger.Error("wizard."+op+"_failed", zap.Error(err))
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
