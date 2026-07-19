package wizard

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/db"
)

// Fake steps to exercise the state machine without git/docker/Alpaca.

type okStep struct{ id string }

func (s okStep) ID() string           { return s.id }
func (s okStep) Title() string        { return s.id }
func (s okStep) NeedsInput() []string { return nil }
func (s okStep) Check(*RunContext) []Issue { return nil }
func (s okStep) Execute(*RunContext) error { return nil }

// gateStep fails Check until the "fixed" input is provided — models a step
// blocked on an external fix + Refresh.
type gateStep struct{ id string }

func (s gateStep) ID() string           { return s.id }
func (s gateStep) Title() string        { return s.id }
func (s gateStep) NeedsInput() []string { return []string{"fixed"} }
func (s gateStep) Execute(ctx *RunContext) error {
	if ctx.Input["fixed"] == "yes" {
		ctx.State["gate_open"] = true
	}
	return nil
}
func (s gateStep) Check(ctx *RunContext) []Issue {
	if ok, _ := ctx.State["gate_open"].(bool); ok {
		return nil
	}
	return []Issue{{Code: "gate_closed", Message: "the gate is closed"}}
}

func newTestEngine(t *testing.T, steps []Step) (*Engine, *db.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return NewEngine(database, zap.NewNop(), t.TempDir(), t.TempDir(), steps), database
}

func TestRunAdvancesThroughOkSteps(t *testing.T) {
	e, d := newTestEngine(t, []Step{okStep{"a"}, okStep{"b"}, okStep{"c"}})
	id, err := e.StartRun("prod", "https://github.com/x/y", false)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := e.Advance(id, nil); err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
	}
	run, steps, _ := d.GetWizardRun(id)
	if run.Status != db.RunDone {
		t.Fatalf("want done, got %s", run.Status)
	}
	for _, s := range steps {
		if s.Status != "ok" {
			t.Fatalf("step %s = %s, want ok", s.StepID, s.Status)
		}
	}
}

func TestBlockedStepStoresIssuesAndRefreshUnblocks(t *testing.T) {
	e, d := newTestEngine(t, []Step{okStep{"a"}, gateStep{"gate"}, okStep{"z"}})
	id, _ := e.StartRun("prod", "https://github.com/x/y", false)
	_ = e.Advance(id, nil) // a → ok

	if err := e.Advance(id, nil); err != nil { // gate blocks
		t.Fatalf("advance gate: %v", err)
	}
	run, steps, _ := d.GetWizardRun(id)
	if run.Status != db.RunBlocked || run.CurrentStep != "gate" {
		t.Fatalf("want blocked@gate, got %s@%s", run.Status, run.CurrentStep)
	}
	var gate *db.WizardStep
	for _, s := range steps {
		if s.StepID == "gate" {
			gate = s
		}
	}
	if gate == nil || gate.Status != "error" || string(gate.Issues) == "[]" {
		t.Fatalf("gate step should hold issues, got %+v", gate)
	}

	// Refresh without a fix stays blocked.
	_ = e.Refresh(id, nil)
	run, _, _ = d.GetWizardRun(id)
	if run.Status != db.RunBlocked {
		t.Fatalf("refresh without fix should stay blocked, got %s", run.Status)
	}

	// Execute with the fix, then finish.
	if err := e.Advance(id, map[string]string{"fixed": "yes"}); err != nil {
		t.Fatalf("advance with fix: %v", err)
	}
	if err := e.Advance(id, nil); err != nil {
		t.Fatalf("advance z: %v", err)
	}
	run, _, _ = d.GetWizardRun(id)
	if run.Status != db.RunDone {
		t.Fatalf("want done, got %s", run.Status)
	}
}

func TestStartRunRejectsInvalidName(t *testing.T) {
	e, _ := newTestEngine(t, []Step{okStep{"a"}})
	if _, err := e.StartRun("OpenAlice", "https://github.com/x/y", false); err == nil {
		t.Fatal("expected StartRun to reject an uppercase name — this is the OpenAlice bug")
	} else if want := `"openalice"`; !contains(err.Error(), want) {
		t.Fatalf("error should suggest the corrected slug %s, got: %v", want, err)
	}
}

func TestBackReturnsToPreviousStepAndReExecutes(t *testing.T) {
	e, d := newTestEngine(t, []Step{okStep{"a"}, gateStep{"gate"}, okStep{"z"}})
	id, _ := e.StartRun("prod", "https://github.com/x/y", false)
	_ = e.Advance(id, nil) // a → ok
	_ = e.Advance(id, nil) // gate blocks

	if err := e.Back(id); err != nil {
		t.Fatalf("back: %v", err)
	}
	run, steps, _ := d.GetWizardRun(id)
	if run.CurrentStep != "a" || run.Status != db.RunRunning {
		t.Fatalf("want running@a after back, got %s@%s", run.Status, run.CurrentStep)
	}
	for _, s := range steps {
		if (s.StepID == "a" || s.StepID == "gate") && s.Status != "pending" {
			t.Fatalf("step %s should be reset to pending, got %s", s.StepID, s.Status)
		}
	}
	// The run must be fully replayable forward again.
	_ = e.Advance(id, nil)                             // a → ok
	_ = e.Advance(id, map[string]string{"fixed": "yes"}) // gate → ok
	_ = e.Advance(id, nil)                             // z → ok
	run, _, _ = d.GetWizardRun(id)
	if run.Status != db.RunDone {
		t.Fatalf("want done after replay, got %s", run.Status)
	}

	// Back is refused at the first step and after completion.
	id2, _ := e.StartRun("prod2", "https://github.com/x/y", false)
	if err := e.Back(id2); err == nil {
		t.Fatal("back at first step should error")
	}
	if err := e.Back(id); err == nil {
		t.Fatal("back on a done run should error")
	}
}

func TestCancelStopsRunAndBlocksFurtherAdvance(t *testing.T) {
	e, d := newTestEngine(t, []Step{okStep{"a"}, gateStep{"gate"}})
	id, _ := e.StartRun("prod", "https://github.com/x/y", false)
	_ = e.Advance(id, nil)

	if err := e.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	run, _, _ := d.GetWizardRun(id)
	if run.Status != db.RunCancelled {
		t.Fatalf("want cancelled, got %s", run.Status)
	}
	if err := e.Cancel(id); err != nil {
		t.Fatalf("cancel must be idempotent, got: %v", err)
	}
	if err := e.Advance(id, nil); err == nil {
		t.Fatal("advance on a cancelled run should error")
	}
	if err := e.Back(id); err == nil {
		t.Fatal("back on a cancelled run should error")
	}
}

func TestCancelNeverDeletesARegisteredProductsDir(t *testing.T) {
	e, d := newTestEngine(t, []Step{okStep{"a"}})
	id, _ := e.StartRun("prod", "https://github.com/x/y", false)

	// Product exists in the registry AND on disk — cancel must leave the dir.
	if _, err := d.InsertProduct(&db.Product{Name: "prod", DisplayName: "prod", SourceRepo: "x", Status: db.StatusLive}); err != nil {
		t.Fatalf("insert product: %v", err)
	}
	dir := filepath.Join(e.repoRoot, "products", "prod")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
		t.Fatal("cancel deleted a REGISTERED product's dir — capital-critical secrets lost")
	}
}

func TestCancelSkipsCleanupForPathTraversalLegacyName(t *testing.T) {
	e, d := newTestEngine(t, []Step{okStep{"a"}})
	// Legacy runs predate StartRun validation — insert directly with a
	// traversal name, the way an old DB row could look.
	id, err := d.InsertWizardRun("../escape", "https://github.com/x/y", "a", []string{"a"})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	victim := filepath.Join(e.repoRoot, "escape")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(victim, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("cancel followed a path-traversal product name outside products/")
	}
}

func TestStatePersistsAcrossSteps(t *testing.T) {
	e, d := newTestEngine(t, []Step{gateStep{"gate"}, okStep{"end"}})
	id, _ := e.StartRun("prod", "https://github.com/x/y", false)
	_ = e.Advance(id, map[string]string{"fixed": "yes"})
	run, _, _ := d.GetWizardRun(id)
	if want := `"gate_open":true`; run.Status == db.RunDone || !contains(string(run.State), want) {
		t.Fatalf("state should persist gate_open, got %s (status %s)", run.State, run.Status)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}
