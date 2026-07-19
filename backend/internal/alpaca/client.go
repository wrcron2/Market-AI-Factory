// Package alpaca is the Factory's read-only Alpaca client: account
// validation at onboarding, equity/P&L for the dashboard cards, health for
// the monitor. Paper endpoint only — the Factory never places orders.
package alpaca

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const paperBase = "https://paper-api.alpaca.markets"

type Client struct{ http *http.Client }

func New() *Client {
	return &Client{http: &http.Client{Timeout: 15 * time.Second}}
}

type Account struct {
	ID           string `json:"id"`
	AccountNo    string `json:"account_number"`
	Status       string `json:"status"`
	Equity       string `json:"equity"`
	LastEquity   string `json:"last_equity"`
	BuyingPower  string `json:"buying_power"`
	TradeBlocked bool   `json:"trading_blocked"`
}

func (a *Account) EquityF() float64     { return parseF(a.Equity) }
func (a *Account) LastEquityF() float64 { return parseF(a.LastEquity) }

// GetAccount validates a key pair by fetching its paper account.
func (c *Client) GetAccount(keyID, secret string) (*Account, error) {
	req, _ := http.NewRequest("GET", paperBase+"/v2/account", nil)
	req.Header.Set("APCA-API-KEY-ID", keyID)
	req.Header.Set("APCA-API-SECRET-KEY", secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alpaca unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("alpaca rejected the key pair (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("alpaca error HTTP %d", resp.StatusCode)
	}
	var acct Account
	if err := json.NewDecoder(resp.Body).Decode(&acct); err != nil {
		return nil, fmt.Errorf("parse account: %w", err)
	}
	return &acct, nil
}

// PortfolioHistory returns the equity curve for card sparklines.
func (c *Client) PortfolioHistory(keyID, secret, period, timeframe string) ([]float64, error) {
	u := fmt.Sprintf("%s/v2/account/portfolio/history?period=%s&timeframe=%s", paperBase, period, timeframe)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("APCA-API-KEY-ID", keyID)
	req.Header.Set("APCA-API-SECRET-KEY", secret)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portfolio history HTTP %d", resp.StatusCode)
	}
	var body struct {
		Equity []*float64 `json:"equity"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(body.Equity))
	for _, v := range body.Equity {
		if v != nil && *v > 0 {
			out = append(out, *v)
		}
	}
	return out, nil
}

func parseF(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
