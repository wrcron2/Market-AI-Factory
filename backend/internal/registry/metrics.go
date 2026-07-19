package registry

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wrcron2/market-ai-factory/backend/internal/alpaca"
	"github.com/wrcron2/market-ai-factory/backend/internal/db"
)

// Metrics is the live card data pulled from a product's own Alpaca account.
type Metrics struct {
	TodayPnl     float64   `json:"today_pnl"`
	TotalPnl     float64   `json:"total_pnl"`
	Equity       float64   `json:"equity"`
	EquitySeries []float64 `json:"equity_series"`
}

// MetricsProvider reads each product's Alpaca creds from its (gitignored)
// products/<name>/.env and serves cached account metrics. The cache keeps
// dashboard polling from hammering Alpaca: one upstream fetch per product
// per TTL, stale data served during Alpaca hiccups rather than blank cards.
type MetricsProvider struct {
	repoRoot string
	client   *alpaca.Client
	logger   *zap.Logger
	ttl      time.Duration

	mu    sync.Mutex
	cache map[string]cachedMetrics
}

type cachedMetrics struct {
	at time.Time
	m  *Metrics
}

func NewMetricsProvider(repoRoot string, client *alpaca.Client, logger *zap.Logger) *MetricsProvider {
	return &MetricsProvider{
		repoRoot: repoRoot, client: client, logger: logger,
		ttl: 60 * time.Second, cache: map[string]cachedMetrics{},
	}
}

// For returns metrics for a product, or nil when unavailable (no creds yet,
// Alpaca down and nothing cached). Never blocks the registry response on an
// error — a card with dashes beats a 500.
func (mp *MetricsProvider) For(p *db.Product) *Metrics {
	if p.AlpacaKeyID == "" {
		return nil
	}
	mp.mu.Lock()
	if c, ok := mp.cache[p.Name]; ok && time.Since(c.at) < mp.ttl {
		mp.mu.Unlock()
		return c.m
	}
	mp.mu.Unlock()

	keyID, secret := mp.readCreds(p.Name)
	if keyID == "" || secret == "" {
		return nil
	}
	acct, err := mp.client.GetAccount(keyID, secret)
	if err != nil {
		mp.logger.Warn("metrics.account_failed", zap.String("product", p.Name), zap.Error(err))
		return mp.stale(p.Name)
	}
	m := &Metrics{
		Equity:   acct.EquityF(),
		TodayPnl: acct.EquityF() - acct.LastEquityF(),
	}
	if series, err := mp.client.PortfolioHistory(keyID, secret, "3M", "1D"); err == nil && len(series) > 0 {
		m.EquitySeries = series
		m.TotalPnl = m.Equity - series[0]
	} else if err != nil {
		mp.logger.Warn("metrics.history_failed", zap.String("product", p.Name), zap.Error(err))
	}

	mp.mu.Lock()
	mp.cache[p.Name] = cachedMetrics{at: time.Now(), m: m}
	mp.mu.Unlock()
	return m
}

func (mp *MetricsProvider) stale(name string) *Metrics {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	if c, ok := mp.cache[name]; ok {
		return c.m
	}
	return nil
}

func (mp *MetricsProvider) readCreds(name string) (keyID, secret string) {
	return ReadProductCreds(mp.repoRoot, name)
}

// ReadProductCreds parses ALPACA_API_KEY / ALPACA_SECRET_KEY from a product's
// env file. The secret never leaves this process — metrics and monitoring only.
func ReadProductCreds(repoRoot, name string) (keyID, secret string) {
	f, err := os.Open(repoRoot + "/products/" + name + "/.env")
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "ALPACA_API_KEY="); ok {
			keyID = v
		}
		if v, ok := strings.CutPrefix(line, "ALPACA_SECRET_KEY="); ok {
			secret = v
		}
	}
	return keyID, secret
}
