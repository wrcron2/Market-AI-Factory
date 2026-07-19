package wizard

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/wrcron2/market-ai-factory/backend/internal/alpaca"
)

// ─── Hangar 3: connect_alpaca ────────────────────────────────────────────────
// Takes pasted per-product paper keys, validates the account live, enforces
// one-account-per-product, and writes the product's .env. The secret is
// written to that file and NOWHERE else — not run state, not the registry.

type ConnectAlpaca struct{ Client *alpaca.Client }

func (ConnectAlpaca) ID() string    { return "connect_alpaca" }
func (ConnectAlpaca) Title() string { return "Connect Alpaca account" }
func (ConnectAlpaca) NeedsInput() []string {
	return []string{"alpaca_key_id", "alpaca_secret"}
}

func (s ConnectAlpaca) Execute(ctx *RunContext) error {
	keyID := strings.TrimSpace(ctx.Input["alpaca_key_id"])
	secret := strings.TrimSpace(ctx.Input["alpaca_secret"])
	if keyID == "" || secret == "" {
		return nil // Check reports missing input
	}
	acct, err := s.Client.GetAccount(keyID, secret)
	if err != nil {
		ctx.State["alpaca_error"] = err.Error()
		ctx.State["alpaca_ok"] = false
		return nil
	}
	if acct.Status != "ACTIVE" {
		ctx.State["alpaca_error"] = fmt.Sprintf("account status is %s, need ACTIVE", acct.Status)
		ctx.State["alpaca_ok"] = false
		return nil
	}
	// One Alpaca account per product — a shared account would mix P&L.
	products, err := ctx.DB.ListProducts()
	if err != nil {
		return err
	}
	for _, p := range products {
		if p.AlpacaKeyID == keyID && p.Name != ctx.Run.ProductName {
			ctx.State["alpaca_error"] = fmt.Sprintf("key already bound to product %q", p.Name)
			ctx.State["alpaca_ok"] = false
			return nil
		}
	}
	dir := ctx.RepoRoot + "/products/" + ctx.Run.ProductName
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	env := fmt.Sprintf("ALPACA_API_KEY=%s\nALPACA_SECRET_KEY=%s\nALPACA_BASE_URL=https://paper-api.alpaca.markets\n", keyID, secret)
	if err := os.WriteFile(dir+"/.env", []byte(env), 0o600); err != nil {
		return err
	}
	ctx.State["alpaca_key_id"] = keyID
	ctx.State["alpaca_ok"] = true
	delete(ctx.State, "alpaca_error")
	return nil
}

func (ConnectAlpaca) Check(ctx *RunContext) []Issue {
	if ok, _ := ctx.State["alpaca_ok"].(bool); ok {
		return nil
	}
	msg, _ := ctx.State["alpaca_error"].(string)
	if msg == "" {
		msg = "Alpaca keys not provided yet"
	}
	return []Issue{{
		Code: "alpaca_not_connected", Message: msg,
		Hint: "Create a paper account at alpaca.markets, paste its API key + secret, then Continue.",
	}}
}

// ─── Hangar 4: set_budget ────────────────────────────────────────────────────

type SetBudget struct{}

func (SetBudget) ID() string          { return "set_budget" }
func (SetBudget) Title() string       { return "Allocate budget" }
func (SetBudget) NeedsInput() []string { return []string{"budget_usd"} }

func (SetBudget) Execute(ctx *RunContext) error {
	raw := strings.TrimSpace(ctx.Input["budget_usd"])
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		ctx.State["budget_error"] = fmt.Sprintf("%q is not a positive dollar amount", raw)
		return nil
	}
	ctx.State["budget_usd"] = v
	delete(ctx.State, "budget_error")
	return nil
}

func (SetBudget) Check(ctx *RunContext) []Issue {
	if v, ok := ctx.State["budget_usd"].(float64); ok && v > 0 {
		return nil
	}
	msg, _ := ctx.State["budget_error"].(string)
	if msg == "" {
		msg = "no budget allocated yet"
	}
	return []Issue{{
		Code: "budget_missing", Message: msg,
		Hint: "Enter the dollar budget this product may trade with (its monitor floor derives from it).",
	}}
}
