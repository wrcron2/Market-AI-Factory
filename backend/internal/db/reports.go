package db

// Daily AI monitor-team reports, one per product per day.

const reportsSchema = `
CREATE TABLE IF NOT EXISTS ai_reports (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id INTEGER NOT NULL REFERENCES products(id),
    report     TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_reports_product ON ai_reports (product_id, id DESC);
`

func (d *DB) InsertAIReport(productID int64, report string) error {
	_, err := d.conn.Exec(`INSERT INTO ai_reports (product_id, report) VALUES (?,?)`, productID, report)
	return err
}

// LatestAIReport returns the newest report for a product ("" when none).
func (d *DB) LatestAIReport(productID int64) (report, createdAt string) {
	_ = d.conn.QueryRow(`SELECT report, created_at FROM ai_reports
	                     WHERE product_id = ? ORDER BY id DESC LIMIT 1`, productID).
		Scan(&report, &createdAt)
	return report, createdAt
}

// HasAIReportSince reports whether a report exists newer than the cutoff
// (SQLite datetime string) — used to run the daily review exactly once.
func (d *DB) HasAIReportSince(productID int64, cutoff string) bool {
	var n int
	_ = d.conn.QueryRow(`SELECT COUNT(*) FROM ai_reports
	                     WHERE product_id = ? AND created_at >= ?`, productID, cutoff).Scan(&n)
	return n > 0
}
