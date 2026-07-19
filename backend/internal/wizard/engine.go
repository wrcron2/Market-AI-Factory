// Package wizard implements the onboarding workflow engine — the "hangars".
// Each Step is an expert station: Check() is an idempotent validation that
// powers the UI's Refresh button; Execute() does the work and must leave
// Check() passing. A run advances only when the current step has zero issues,
// and every run is resumable because all state lives in SQLite.
package wizard

import (
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
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

// StartRun creates a run positioned at the first step (nothing executed yet).
func (e *Engine) StartRun(productName, sourceRepo string, adopted bool) (int64, error) {
	if len(e.steps) == 0 {
		return 0, fmt.Errorf("engine has no steps")
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

// Refresh re-runs Check on the current (blocked) step without re-executing
// side effects. Fixed externally → step completes and the run moves on.
func (e *Engine) Refresh(runID int64, input map[string]string) error {
	run, _, err := e.db.GetWizardRun(runID)
	if err != nil {
		return err
	}
	if run.Status == db.RunDone {
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
