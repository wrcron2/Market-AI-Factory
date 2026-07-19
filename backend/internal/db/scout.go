package db

// Repo Scout storage — ported from Market-AI (backend/internal/db/db.go,
// "Pipeline Repo Scout" section). The factory owns scouting now: finding
// candidate repos IS finding candidate products.

import "database/sql"

const scoutSchema = `
CREATE TABLE IF NOT EXISTS github_repo_scout (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    full_name           TEXT NOT NULL UNIQUE,
    url                 TEXT NOT NULL,
    description         TEXT,
    stars               INTEGER NOT NULL DEFAULT 0,
    language            TEXT,
    topics              TEXT DEFAULT '[]',
    last_commit_at      TEXT,
    first_seen_at       TEXT NOT NULL DEFAULT (datetime('now')),
    last_checked_at     TEXT NOT NULL DEFAULT (datetime('now')),
    status              TEXT NOT NULL DEFAULT 'new',
    rejected_reason     TEXT,
    research_notion_url TEXT,
    researched_at       TEXT,
    research_report     TEXT
);
CREATE INDEX IF NOT EXISTS idx_repos_status ON github_repo_scout (status, first_seen_at DESC);
`

func (d *DB) ListRepos(statusFilter string) ([]map[string]any, error) {
	q := `SELECT id, full_name, url, description, stars, language, topics,
	             last_commit_at, first_seen_at, last_checked_at,
	             status, rejected_reason, research_notion_url, researched_at,
	             research_report IS NOT NULL AND research_report != ''
	      FROM github_repo_scout`
	args := []any{}
	if statusFilter != "" {
		q += " WHERE status = ?"
		args = append(args, statusFilter)
	}
	q += " ORDER BY first_seen_at DESC LIMIT 200"

	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id int64
		var fullName, url, firstSeen, lastChecked, status string
		var desc, lang, topics, lastCommit, rejReason, notionURL, researchedAt *string
		var stars int
		var hasReport bool
		if err := rows.Scan(&id, &fullName, &url, &desc, &stars, &lang, &topics,
			&lastCommit, &firstSeen, &lastChecked, &status, &rejReason, &notionURL, &researchedAt, &hasReport); err != nil {
			continue
		}
		result = append(result, map[string]any{
			"id": id, "full_name": fullName, "url": url, "description": desc,
			"stars": stars, "language": lang, "topics": topics,
			"last_commit_at": lastCommit, "first_seen_at": firstSeen,
			"last_checked_at": lastChecked, "status": status,
			"rejected_reason": rejReason, "research_notion_url": notionURL,
			"researched_at": researchedAt, "has_report": hasReport,
		})
	}
	return result, nil
}

// ScoutRepo is a row in github_repo_scout as used by the scout/research agents.
type ScoutRepo struct {
	ID          int64
	FullName    string
	URL         string
	Description string
	Stars       int
	Language    string
	Topics      string // JSON array string
	LastCommit  string
}

// GetRepoStatus returns the current status of a repo by full_name, or found=false.
func (d *DB) GetRepoStatus(fullName string) (status string, found bool) {
	err := d.conn.QueryRow(`SELECT status FROM github_repo_scout WHERE full_name = ?`, fullName).Scan(&status)
	return status, err == nil
}

// TouchRepo refreshes stars and last_checked_at for an already-known repo.
func (d *DB) TouchRepo(fullName string, stars int) error {
	_, err := d.conn.Exec(`UPDATE github_repo_scout
	                  SET stars = ?, last_checked_at = datetime('now')
	                  WHERE full_name = ?`, stars, fullName)
	return err
}

// InsertScoutRepo records a newly discovered repo with its classification.
func (d *DB) InsertScoutRepo(r *ScoutRepo, status, rejectedReason string) error {
	var rej *string
	if rejectedReason != "" {
		rej = &rejectedReason
	}
	_, err := d.conn.Exec(`INSERT OR IGNORE INTO github_repo_scout
	    (full_name, url, description, stars, language, topics, last_commit_at, status, rejected_reason)
	    VALUES (?,?,?,?,?,?,?,?,?)`,
		r.FullName, r.URL, r.Description, r.Stars, r.Language, r.Topics, r.LastCommit, status, rej)
	return err
}

// ListResearchCandidates returns repos with status='good' not yet researched,
// highest stars first.
func (d *DB) ListResearchCandidates(limit int) ([]*ScoutRepo, error) {
	rows, err := d.conn.Query(`SELECT id, full_name, url, COALESCE(description,''), stars,
	                             COALESCE(language,''), COALESCE(topics,'[]')
	                      FROM github_repo_scout
	                      WHERE status = 'good' AND researched_at IS NULL
	                      ORDER BY stars DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*ScoutRepo
	for rows.Next() {
		r := &ScoutRepo{}
		if err := rows.Scan(&r.ID, &r.FullName, &r.URL, &r.Description, &r.Stars, &r.Language, &r.Topics); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// GetResearchCandidate returns a single repo eligible for research
// (status='good', not yet researched), or found=false if it doesn't qualify.
func (d *DB) GetResearchCandidate(id int64) (repo *ScoutRepo, found bool, err error) {
	r := &ScoutRepo{}
	err = d.conn.QueryRow(`SELECT id, full_name, url, COALESCE(description,''), stars,
	                          COALESCE(language,''), COALESCE(topics,'[]')
	                   FROM github_repo_scout
	                   WHERE id = ? AND status = 'good' AND researched_at IS NULL`, id).
		Scan(&r.ID, &r.FullName, &r.URL, &r.Description, &r.Stars, &r.Language, &r.Topics)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return r, true, nil
}

// SaveResearchReport stores the markdown report (and optional Notion URL) and
// flips the repo to researched.
func (d *DB) SaveResearchReport(id int64, report, notionURL string) error {
	var nu *string
	if notionURL != "" {
		nu = &notionURL
	}
	_, err := d.conn.Exec(`UPDATE github_repo_scout
	                  SET status = 'researched', research_report = ?,
	                      research_notion_url = ?, researched_at = datetime('now')
	                  WHERE id = ?`, report, nu, id)
	return err
}

// GetResearchReport returns the repo name and stored markdown report.
func (d *DB) GetResearchReport(id int64) (fullName, report string, err error) {
	err = d.conn.QueryRow(`SELECT full_name, COALESCE(research_report,'')
	                  FROM github_repo_scout WHERE id = ?`, id).Scan(&fullName, &report)
	return fullName, report, err
}
