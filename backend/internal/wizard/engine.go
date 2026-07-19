// Package wizard implements the onboarding workflow engine — the "hangars".
// Each Step is an expert station: Check() is an idempotent validation that
// powers the UI's Refresh button; Execute() does the work and must leave
// Check() passing. A run advances only when the current step has zero issues,
// and every run is resumable because all state lives in SQLite.
package wizard

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
	"github.com/wrcron2/market-ai-factory/backend/internal/orchestrator"
)

// Issue is one actionable problem blocking a step. Field names are stable —
// the AddWizard UI renders them directly.
type Issue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// RunContext carries everything a step may need. State is the run's shared
// scratchpad (persisted as wizard_runs.state); Input is the payload the user
// submitted with this advance/refresh call (e.g. Alpaca keys) and is NEVER
// persisted — secrets flow through memory into files, not into the DB.
type RunContext struct {
	Run      *db.WizardRun
	DB       *db.DB
	Logger   *zap.Logger
	State    map[string]any
	Input    map[string]string
	RepoRoot string // factory repo root — products/<name>/ specs live here
	WorkRoot string // where product sources get cloned/deployed
}

func (c *RunContext) StateStr(key string) string {
	if v, ok := c.State[key].(string); ok {
		return v
	}
	return ""
}

type Step interface {
	ID() string
	Title() string
	// NeedsInput lists input field names the UI must collect before this
	// step can execute (empty for fully automatic steps).
	NeedsInput() []string
	Check(ctx *RunContext) []Issue
	Execute(ctx *RunContext) error
}

// Engine owns the ordered step list and the advance/refresh state machine.
type Engine struct {
	db     *db.DB
	logger *zap.Logger
	steps  []Step
	byID   map[string]Step
	// roots injected once so steps stay pure
	repoRoot string
	workRoot string
}

func NewEngine(database *db.DB, logger *zap.Logger, repoRoot, workRoot string, steps []Step) *Engine {
	byID := make(map[string]Step, len(steps))
	for _, s := range steps {
		byID[s.ID()] = s
	}
	return &Engine{db: database, logger: logger, steps: steps, byID: byID, repoRoot: repoRoot, workRoot: workRoot}
}

func (e *Engine) StepIDs() []string {
	ids := make([]string, len(e.steps))
	for i, s := range e.steps {
		ids[i] = s.ID()
	}
	return ids
}

func (e *Engine) StepMeta() []map[string]any {
	out := make([]map[string]any, len(e.steps))
	for i, s := range e.steps {
		out[i] = map[string]any{"id": s.ID(), "title": s.Title(), "needs_input": s.NeedsInput()}
	}
	return out
}

// SuggestSlug turns free-form input into a valid product slug — used both to
// pre-normalize in the UI and to give an actionable 400 when a bad name
// reaches the API anyway.
func SuggestSlug(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-':
			out = append(out, r)
		case r == ' ' || r == '_' || r == '.':
			out = append(out, '-')
		}
	}
	s := strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(string(out), "-"), "-")
	if len(s) > 41 {
		s = s[:41]
	}
	return s
}

// ValidateProductName rejects names that can't be product slugs, with an
// actionable suggestion. Called by the API handler (→ 400) and by StartRun
// itself as a belt-and-braces guard — a name is baked into a run forever, so
// letting a bad one through creates a run the user can never fix (the
// OpenAlice bug).
func ValidateProductName(name string) error {
	if slugRe.MatchString(name) {
		return nil
	}
	suggestion := SuggestSlug(name)
	if suggestion == "" || !slugRe.MatchString(suggestion) {
		return fmt.Errorf("%q is not a valid product name — use lowercase letters, digits, and dashes (2–41 chars)", name)
	}
	return fmt.Errorf("%q is not a valid product name — try %q", name, suggestion)
}

// StartRun creates a run positioned at the first step (nothing executed yet).
func (e *Engine) StartRun(productName, sourceRepo string, adopted bool) (int64, error) {
	if len(e.steps) == 0 {
		return 0, fmt.Errorf("engine has no steps")
	}
	if err := ValidateProductName(productName); err != nil {
		return 0, err
	}
	id, err := e.db.InsertWizardRun(productName, sourceRepo, e.steps[0].ID(), e.StepIDs())
	if err != nil {
		return 0, err
	}
	if adopted {
		// Record adoption in run state so deploy/verify take the fast path.
		run, _, err := e.db.GetWizardRun(id)
		if err == nil {
			state := map[string]any{"adopted": true}
			raw, _ := json.Marshal(state)
			_ = e.db.UpdateWizardRun(run.ID, run.CurrentStep, run.Status, raw)
		}
	}
	return id, nil
}

// Advance executes the current step (with user input, if any), then re-checks
// it. Zero issues → step ok, move to the next (or finish). Issues → run
// blocked, issues stored for the UI.
func (e *Engine) Advance(runID int64, input map[string]string) error {
	run, _, err := e.db.GetWizardRun(runID)
	if err != nil {
		return err
	}
	if run.Status == db.RunDone {
		return fmt.Errorf("run already complete")
	}
	if run.Status == db.RunCancelled {
		return fmt.Errorf("run was cancelled — start a new one")
	}
	step, ok := e.byID[run.CurrentStep]
	if !ok {
		return fmt.Errorf("unknown step %q", run.CurrentStep)
	}
	ctx, err := e.buildCtx(run, input)
	if err != nil {
		return err
	}

	_ = e.db.UpdateWizardStep(runID, step.ID(), "running", nil)
	if err := step.Execute(ctx); err != nil {
		// Execution error is itself an issue — surfaced, not swallowed.
		return e.block(ctx, step, []Issue{{Code: "execute_failed", Message: err.Error()}})
	}
	if issues := step.Check(ctx); len(issues) > 0 {
		return e.block(ctx, step, issues)
	}
	return e.completeStep(ctx, step)
}

// Back moves a run to its previous step so inputs can be corrected — both
// step rows reset to pending (state is preserved, so already-entered values
// like the pinned SHA survive; the revisited step simply re-executes).
func (e *Engine) Back(runID int64) error {
	run, _, err := e.db.GetWizardRun(runID)
	if err != nil {
		return err
	}
	switch run.Status {
	case db.RunDone:
		return fmt.Errorf("run is complete — the product is already published")
	case db.RunCancelled:
		return fmt.Errorf("run was cancelled")
	}
	idx := -1
	for i, s := range e.steps {
		if s.ID() == run.CurrentStep {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return fmt.Errorf("already at the first step")
	}
	prev := e.steps[idx-1].ID()
	if err := e.db.UpdateWizardStep(runID, run.CurrentStep, "pending", nil); err != nil {
		return err
	}
	if err := e.db.UpdateWizardStep(runID, prev, "pending", nil); err != nil {
		return err
	}
	return e.db.UpdateWizardRun(runID, prev, db.RunRunning, run.State)
}

// Cancel abandons a run. Nothing stays half-provisioned: if the product was
// never published, its products/<name>/ dir (including the Alpaca .env — a
// secret that must not linger for an abandoned product) is removed, and a
// stack the deploy step already started is stopped.
func (e *Engine) Cancel(runID int64) error {
	run, _, err := e.db.GetWizardRun(runID)
	if err != nil {
		return err
	}
	if run.Status == db.RunDone {
		return fmt.Errorf("run is complete — pause or remove the product from its page instead")
	}
	if run.Status == db.RunCancelled {
		return nil // idempotent
	}

	state := map[string]any{}
	_ = json.Unmarshal(run.State, &state)
	adopted, _ := state["adopted"].(bool)
	deployed, _ := state["deployed"].(bool)
	if deployed && !adopted {
		dir := e.workRoot + "/" + run.ProductName
		if out, err := orchestrator.ComposeDown(dir, orchestrator.ComposeFiles(dir)...); err != nil {
			e.logger.Warn("wizard.cancel_compose_stop_failed",
				zap.String("product", run.ProductName), zap.String("out", out), zap.Error(err))
		}
	}
	// Deleting products/<name>/ is the one destructive call in this path
	// family, so it gets the strictest guards:
	//  - the name must be a valid slug (legacy runs predate StartRun
	//    validation and could carry path segments — never RemoveAll those);
	//  - only a definitive "not registered" (ErrNoRows) permits deletion.
	//    Any other DB error means we don't KNOW, and not-knowing must never
	//    delete a directory that may hold a live product's Alpaca .env.
	switch p, err := e.db.GetProduct(run.ProductName); {
	case err == nil && p != nil:
		// registered product — its files stay
	case errors.Is(err, sql.ErrNoRows):
		if !slugRe.MatchString(run.ProductName) {
			e.logger.Warn("wizard.cancel_cleanup_skipped_bad_name", zap.String("product", run.ProductName))
			break
		}
		if err := os.RemoveAll(e.repoRoot + "/products/" + run.ProductName); err != nil {
			e.logger.Warn("wizard.cancel_cleanup_failed", zap.String("product", run.ProductName), zap.Error(err))
		}
	default:
		e.logger.Warn("wizard.cancel_cleanup_skipped_db_error", zap.String("product", run.ProductName), zap.Error(err))
	}
	return e.db.UpdateWizardRun(runID, run.CurrentStep, db.RunCancelled, run.State)
}

// Refresh re-runs Check on the current (blocked) step without re-executing
// side effects. Fixed externally → step completes and the run moves on.
func (e *Engine) Refresh(runID int64, input map[string]string) error {
	run, _, err := e.db.GetWizardRun(runID)
	if err != nil {
		return err
	}
	if run.Status == db.RunDone || run.Status == db.RunCancelled {
		return nil
	}
	step, ok := e.byID[run.CurrentStep]
	if !ok {
		return fmt.Errorf("unknown step %q", run.CurrentStep)
	}
	ctx, err := e.buildCtx(run, input)
	if err != nil {
		return err
	}
	if issues := step.Check(ctx); len(issues) > 0 {
		return e.block(ctx, step, issues)
	}
	return e.completeStep(ctx, step)
}

func (e *Engine) buildCtx(run *db.WizardRun, input map[string]string) (*RunContext, error) {
	state := map[string]any{}
	if len(run.State) > 0 {
		if err := json.Unmarshal(run.State, &state); err != nil {
			return nil, fmt.Errorf("corrupt run state: %w", err)
		}
	}
	if input == nil {
		input = map[string]string{}
	}
	return &RunContext{
		Run: run, DB: e.db, Logger: e.logger,
		State: state, Input: input,
		RepoRoot: e.repoRoot, WorkRoot: e.workRoot,
	}, nil
}

func (e *Engine) block(ctx *RunContext, step Step, issues []Issue) error {
	raw, _ := json.Marshal(issues)
	if err := e.db.UpdateWizardStep(ctx.Run.ID, step.ID(), "error", raw); err != nil {
		return err
	}
	return e.saveRun(ctx, step.ID(), db.RunBlocked)
}

func (e *Engine) completeStep(ctx *RunContext, step Step) error {
	if err := e.db.UpdateWizardStep(ctx.Run.ID, step.ID(), "ok", nil); err != nil {
		return err
	}
	next := e.nextStep(step.ID())
	if next == "" {
		return e.saveRun(ctx, step.ID(), db.RunDone)
	}
	return e.saveRun(ctx, next, db.RunRunning)
}

func (e *Engine) nextStep(current string) string {
	for i, s := range e.steps {
		if s.ID() == current && i+1 < len(e.steps) {
			return e.steps[i+1].ID()
		}
	}
	return ""
}

func (e *Engine) saveRun(ctx *RunContext, currentStep, status string) error {
	raw, err := json.Marshal(ctx.State)
	if err != nil {
		return err
	}
	return e.db.UpdateWizardRun(ctx.Run.ID, currentStep, status, raw)
}
