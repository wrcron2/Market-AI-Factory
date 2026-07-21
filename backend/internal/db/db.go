// Package db owns the Factory's SQLite store: the product registry, wizard
// run state, and monitor check history. Schema is embedded and idempotent —
// the server creates it on boot exactly like the Market-AI backend does.
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	conn *sql.DB
}

func Open(dsn string) (*DB, error) {
	conn, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := conn.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := applyMigrations(conn); err != nil {
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	if _, err := conn.Exec(scoutSchema); err != nil {
		return nil, fmt.Errorf("apply scout schema: %w", err)
	}
	if _, err := conn.Exec(reportsSchema); err != nil {
		return nil, fmt.Errorf("apply reports schema: %w", err)
	}
	return &DB{conn: conn}, nil
}

// applyMigrations brings an existing database up to the current schema.
// Each migration is idempotent and checks pragma_table_info before
// ALTERing, so a fresh DB (which already has the columns from `schema`)
// is a no-op and an old DB gets the missing columns added in place.
// SQLite ALTER TABLE ADD COLUMN can't be wrapped in IF NOT EXISTS, so
// the pragma query is the gate.
func applyMigrations(conn *sql.DB) error {
	type col struct{ name, ctype string }
	productsColumns := make(map[string]string)
	rows, err := conn.Query(`PRAGMA table_info(products)`)
	if err != nil {
		return fmt.Errorf("pragma products: %w", err)
	}
	for rows.Next() {
		var cid int
		var c col
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &c.name, &c.ctype, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		productsColumns[c.name] = c.ctype
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, ok := productsColumns["dashboard_auth_login"]; !ok {
		if _, err := conn.Exec(`ALTER TABLE products ADD COLUMN dashboard_auth_login TEXT`); err != nil {
			return fmt.Errorf("add dashboard_auth_login: %w", err)
		}
	}
	if _, ok := productsColumns["dashboard_auth_token"]; !ok {
		if _, err := conn.Exec(`ALTER TABLE products ADD COLUMN dashboard_auth_token TEXT`); err != nil {
			return fmt.Errorf("add dashboard_auth_token: %w", err)
		}
	}
	return nil
}

func (d *DB) Close() error { return d.conn.Close() }

// ─── Products ────────────────────────────────────────────────────────────────

// Product statuses form a small state machine:
// DRAFT (wizard in progress) → LIVE ⇄ PAUSED; ERROR set by the monitor.
const (
	StatusDraft  = "DRAFT"
	StatusLive   = "LIVE"
	StatusPaused = "PAUSED"
	StatusError  = "ERROR"
)

type Product struct {
	ID                 int64   `json:"id"`
	Name               string  `json:"name"` // slug, unique
	DisplayName        string  `json:"display_name"`
	SourceRepo         string  `json:"source_repo"`
	SourceSHA          string  `json:"source_sha,omitempty"`
	Status             string  `json:"status"`
	PortBase           int     `json:"port_base"` // 0 = adopted product with its own ports
	BudgetUSD          float64 `json:"budget_usd"`
	DashboardURL       string  `json:"dashboard_url,omitempty"`        // product's own UI
	DashboardAuthLogin string  `json:"dashboard_auth_login,omitempty"` // path the proxy POSTs the token to (default /api/auth/login)
	DashboardAuthToken string  `json:"-"`                              // secret the proxy trades for a session cookie at DashboardAuthLogin
	HealthURL          string  `json:"health_url,omitempty"`           // probed by the monitor
	AlpacaKeyID        string  `json:"alpaca_key_id,omitempty"`        // key id only — secret never stored here
	Adopted            bool    `json:"adopted"`                        // true = pre-existing deploy (Market-AI)
	CreatedAt          string  `json:"created_at"`
	UpdatedAt          string  `json:"updated_at"`
}

func (d *DB) InsertProduct(p *Product) (int64, error) {
	res, err := d.conn.Exec(`
		INSERT INTO products (name, display_name, source_repo, source_sha, status,
		                      port_base, budget_usd, dashboard_url,
		                      dashboard_auth_login, dashboard_auth_token,
		                      health_url, alpaca_key_id, adopted)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, p.DisplayName, p.SourceRepo, p.SourceSHA, p.Status,
		p.PortBase, p.BudgetUSD, p.DashboardURL,
		p.DashboardAuthLogin, p.DashboardAuthToken,
		p.HealthURL, p.AlpacaKeyID, boolToInt(p.Adopted))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateProductStatus(name, status string) error {
	res, err := d.conn.Exec(
		`UPDATE products SET status = ?, updated_at = datetime('now') WHERE name = ?`,
		status, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("product %q not found", name)
	}
	return nil
}

func (d *DB) UpdateProduct(p *Product) error {
	res, err := d.conn.Exec(`
		UPDATE products SET display_name=?, source_repo=?, source_sha=?, status=?,
		       port_base=?, budget_usd=?, dashboard_url=?,
		       dashboard_auth_login=?, dashboard_auth_token=?,
		       health_url=?, alpaca_key_id=?, adopted=?, updated_at=datetime('now')
		WHERE name=?`,
		p.DisplayName, p.SourceRepo, p.SourceSHA, p.Status,
		p.PortBase, p.BudgetUSD, p.DashboardURL,
		p.DashboardAuthLogin, p.DashboardAuthToken,
		p.HealthURL, p.AlpacaKeyID, boolToInt(p.Adopted), p.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("product %q not found", p.Name)
	}
	return nil
}

func (d *DB) GetProduct(name string) (*Product, error) {
	row := d.conn.QueryRow(productSelect+` WHERE name = ?`, name)
	return scanProduct(row)
}

func (d *DB) ListProducts() ([]*Product, error) {
	rows, err := d.conn.Query(productSelect + ` ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MaxPortBase returns the highest allocated port base (0 when none), so the
// wizard can allocate collision-free ranges (10100, 10200, ...).
func (d *DB) MaxPortBase() (int, error) {
	var maxBase sql.NullInt64
	if err := d.conn.QueryRow(`SELECT MAX(port_base) FROM products`).Scan(&maxBase); err != nil {
		return 0, err
	}
	return int(maxBase.Int64), nil
}

const productSelect = `
	SELECT id, name, display_name, source_repo, COALESCE(source_sha,''), status,
	       port_base, budget_usd, COALESCE(dashboard_url,''),
	       COALESCE(dashboard_auth_login,''), COALESCE(dashboard_auth_token,''),
	       COALESCE(health_url,''),
	       COALESCE(alpaca_key_id,''), adopted, created_at, updated_at
	FROM products`

type rowScanner interface{ Scan(dest ...any) error }

func scanProduct(r rowScanner) (*Product, error) {
	var p Product
	var adopted int
	err := r.Scan(&p.ID, &p.Name, &p.DisplayName, &p.SourceRepo, &p.SourceSHA,
		&p.Status, &p.PortBase, &p.BudgetUSD, &p.DashboardURL,
		&p.DashboardAuthLogin, &p.DashboardAuthToken,
		&p.HealthURL,
		&p.AlpacaKeyID, &adopted, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.Adopted = adopted == 1
	return &p, nil
}

// ─── Wizard runs ─────────────────────────────────────────────────────────────

const (
	RunRunning   = "running"
	RunBlocked   = "blocked" // current step has issues; waiting for fix + Refresh
	RunDone      = "done"
	RunFailed    = "failed"
	RunCancelled = "cancelled" // user abandoned the run; nothing published
)

type WizardRun struct {
	ID          int64           `json:"id"`
	ProductName string          `json:"product_name"`
	SourceRepo  string          `json:"source_repo"`
	CurrentStep string          `json:"current_step"`
	Status      string          `json:"status"`
	State       json.RawMessage `json:"state"` // step-shared scratch (ports, sha, …)
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

type WizardStep struct {
	RunID     int64           `json:"run_id"`
	StepID    string          `json:"step_id"`
	Seq       int             `json:"seq"`
	Status    string          `json:"status"` // pending | running | ok | error
	Issues    json.RawMessage `json:"issues"` // []Issue
	UpdatedAt string          `json:"updated_at"`
}

func (d *DB) InsertWizardRun(productName, sourceRepo, firstStep string, stepIDs []string) (int64, error) {
	tx, err := d.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		INSERT INTO wizard_runs (product_name, source_repo, current_step, status, state)
		VALUES (?,?,?,?,'{}')`,
		productName, sourceRepo, firstStep, RunRunning)
	if err != nil {
		return 0, err
	}
	runID, _ := res.LastInsertId()
	for i, id := range stepIDs {
		if _, err := tx.Exec(`
			INSERT INTO wizard_steps (run_id, step_id, seq, status, issues)
			VALUES (?,?,?,'pending','[]')`, runID, id, i); err != nil {
			return 0, err
		}
	}
	return runID, tx.Commit()
}

func (d *DB) GetWizardRun(id int64) (*WizardRun, []*WizardStep, error) {
	var run WizardRun
	var state string
	err := d.conn.QueryRow(`
		SELECT id, product_name, source_repo, current_step, status, state, created_at, updated_at
		FROM wizard_runs WHERE id = ?`, id).
		Scan(&run.ID, &run.ProductName, &run.SourceRepo, &run.CurrentStep,
			&run.Status, &state, &run.CreatedAt, &run.UpdatedAt)
	if err != nil {
		return nil, nil, err
	}
	run.State = json.RawMessage(state)
	rows, err := d.conn.Query(`
		SELECT run_id, step_id, seq, status, issues, updated_at
		FROM wizard_steps WHERE run_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var steps []*WizardStep
	for rows.Next() {
		var s WizardStep
		var issues string
		if err := rows.Scan(&s.RunID, &s.StepID, &s.Seq, &s.Status, &issues, &s.UpdatedAt); err != nil {
			return nil, nil, err
		}
		s.Issues = json.RawMessage(issues)
		steps = append(steps, &s)
	}
	return &run, steps, rows.Err()
}

func (d *DB) ListWizardRuns() ([]*WizardRun, error) {
	rows, err := d.conn.Query(`
		SELECT id, product_name, source_repo, current_step, status, state, created_at, updated_at
		FROM wizard_runs ORDER BY id DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WizardRun
	for rows.Next() {
		var run WizardRun
		var state string
		if err := rows.Scan(&run.ID, &run.ProductName, &run.SourceRepo, &run.CurrentStep,
			&run.Status, &state, &run.CreatedAt, &run.UpdatedAt); err != nil {
			return nil, err
		}
		run.State = json.RawMessage(state)
		out = append(out, &run)
	}
	return out, rows.Err()
}

func (d *DB) UpdateWizardRun(id int64, currentStep, status string, state json.RawMessage) error {
	_, err := d.conn.Exec(`
		UPDATE wizard_runs SET current_step=?, status=?, state=?, updated_at=datetime('now')
		WHERE id=?`, currentStep, status, state, id)
	return err
}

func (d *DB) UpdateWizardStep(runID int64, stepID, status string, issues json.RawMessage) error {
	if issues == nil {
		issues = json.RawMessage("[]")
	}
	_, err := d.conn.Exec(`
		UPDATE wizard_steps SET status=?, issues=?, updated_at=datetime('now')
		WHERE run_id=? AND step_id=?`, status, issues, runID, stepID)
	return err
}

// ─── Monitor checks ──────────────────────────────────────────────────────────

type ProductCheck struct {
	ID        int64           `json:"id"`
	ProductID int64           `json:"product_id"`
	OK        bool            `json:"ok"`
	Details   json.RawMessage `json:"details"`
	CheckedAt string          `json:"checked_at"`
}

func (d *DB) InsertProductCheck(productID int64, ok bool, details json.RawMessage) error {
	_, err := d.conn.Exec(`
		INSERT INTO product_checks (product_id, ok, details) VALUES (?,?,?)`,
		productID, boolToInt(ok), details)
	return err
}

func (d *DB) ListProductChecks(productID int64, limit int) ([]*ProductCheck, error) {
	if limit <= 0 {
		limit = 24
	}
	rows, err := d.conn.Query(`
		SELECT id, product_id, ok, details, checked_at
		FROM product_checks WHERE product_id = ? ORDER BY id DESC LIMIT ?`,
		productID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProductCheck
	for rows.Next() {
		var c ProductCheck
		var ok int
		if err := rows.Scan(&c.ID, &c.ProductID, &ok, &c.Details, &c.CheckedAt); err != nil {
			return nil, err
		}
		c.OK = ok == 1
		out = append(out, &c)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

var _ = time.Now // keep time import for future use in this package

const schema = `
CREATE TABLE IF NOT EXISTS products (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL,
    source_repo   TEXT NOT NULL,
    source_sha    TEXT,
    status        TEXT NOT NULL DEFAULT 'DRAFT',
    port_base     INTEGER NOT NULL DEFAULT 0,
    budget_usd    REAL NOT NULL DEFAULT 0,
    dashboard_url TEXT,
    dashboard_auth_login TEXT,
    dashboard_auth_token TEXT,
    health_url    TEXT,
    alpaca_key_id TEXT,
    adopted       INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS wizard_runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    product_name TEXT NOT NULL,
    source_repo  TEXT NOT NULL,
    current_step TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'running',
    state        TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS wizard_steps (
    run_id     INTEGER NOT NULL REFERENCES wizard_runs(id),
    step_id    TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    issues     TEXT NOT NULL DEFAULT '[]',
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (run_id, step_id)
);

CREATE TABLE IF NOT EXISTS product_checks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id INTEGER NOT NULL REFERENCES products(id),
    ok         INTEGER NOT NULL,
    details    TEXT NOT NULL DEFAULT '{}',
    checked_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_checks_product ON product_checks (product_id, id DESC);
`
