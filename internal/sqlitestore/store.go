// Package sqlitestore implements the progress.Writer and progress.Reader
// interfaces over a SQLite database, allowing the worker and web processes to
// share live PR state through a file on disk.
//
// The worker opens the DB in writer mode (SetMaxOpenConns(1) so
// database/sql serialises the orchestrator's ~20 concurrent PR goroutines;
// no SQLITE_BUSY, no app-level mutex). The web process opens its own
// read-only-intent connection; WAL mode allows concurrent reads alongside
// the worker's serialised writes.
//
// Schema is applied idempotently by any Open call (CREATE TABLE IF NOT EXISTS),
// so start order between worker and web doesn't matter.
package sqlitestore

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed schema.sql
var schemaSQL string

// Store is the SQLite-backed implementation of progress.Writer and
// progress.Reader. Callers should call Close when done.
type Store struct {
	db     *sql.DB
	closed atomic.Bool
}

// Open opens (or creates) the SQLite database at path and applies the schema
// idempotently. If writer is true the connection pool is capped at 1 so that
// database/sql serialises all writes — required when the orchestrator drives
// ~20 concurrent PR goroutines. Both writer and reader processes call Open;
// the schema's CREATE TABLE IF NOT EXISTS / INSERT OR IGNORE make it safe.
func Open(path string, writer bool) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open %s: %w", path, err)
	}
	// Pin to a single connection in both modes.
	//   writer: serialises the orchestrator's concurrent goroutines at the sql.DB
	//           layer rather than contending at SQLite.
	//   reader: the change-notifier polls `PRAGMA data_version`, which is a
	//           PER-CONNECTION counter; with an unbounded pool, consecutive polls
	//           could land on different connections and miss or spuriously fire
	//           SSE updates. One connection keeps the baseline consistent.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitestore: apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlitestore: migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// currentSchemaVersion is the schema version stamped into PRAGMA user_version.
// Increment this and add a migration block to migrate() when the schema changes.
const currentSchemaVersion = 2

// migrate stamps user_version on a fresh DB and runs any incremental migrations
// for DBs created by older versions of the binary.
//
// Version 0 means a fresh DB — schema.sql already has all current columns, so
// no DDL is needed; we only stamp the version. For any existing DB (version > 0),
// each "if version < N" block runs for all DBs below version N, so every
// intermediate step is applied in order regardless of how far back the upgrade
// reaches.
func migrate(db *sql.DB) error {
	var version int
	_ = db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version >= currentSchemaVersion {
		return nil
	}
	if version > 0 {
		if version < 2 {
			// v1 → v2: add GitHub URL columns for dashboard linking (Phase 4a+4b).
			for _, col := range []string{
				`ALTER TABLE pr_progress ADD COLUMN pr_url TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE pr_progress ADD COLUMN replacement_pr_url TEXT NOT NULL DEFAULT ''`,
			} {
				if _, err := db.Exec(col); err != nil {
					return fmt.Errorf("migrate v1→v2: %w", err)
				}
			}
		}
		// Future migrations: add "if version < N { ... }" blocks here.
	}
	_, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, currentSchemaVersion))
	return err
}

// DB returns the underlying *sql.DB, used by the Notifier and StatusReader.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		return s.db.Close()
	}
	return nil
}

// — compile-time interface assertions —
var (
	_ progress.Writer     = (*Store)(nil)
	_ progress.Reader     = (*Store)(nil)
	_ progress.ReadWriter = (*Store)(nil)
)

// ---- Writer methods -------------------------------------------------------

// Report records that PR prNumber has entered stage. The first call for a PR
// creates its row (and records pkg/bump); later calls update the stage but do
// NOT overwrite package_name/bump_type — first-write-wins for metadata,
// first-write-wins for metadata. Every call appends a stage_events row. Runs
// in a single transaction.
func (s *Store) Report(prNumber int, pkg, bump string, stage models.PRStage, detail string) {
	now := toUnixNano(time.Now())
	tx, err := s.db.Begin()
	if err != nil {
		return // best-effort; store is non-critical path
	}
	defer tx.Rollback() //nolint:errcheck

	// Upsert: on conflict preserve existing package_name/bump_type.
	_, err = tx.Exec(`
		INSERT INTO pr_progress (pr_number, package_name, bump_type, stage, last_updated)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(pr_number) DO UPDATE SET
			stage        = excluded.stage,
			last_updated = excluded.last_updated`,
		prNumber, pkg, bump, string(stage), now,
	)
	if err != nil {
		slog.Error("sqlitestore: failed to upsert pr_progress", "pr", prNumber, "stage", stage, "error", err)
		return
	}
	_, err = tx.Exec(`
		INSERT INTO stage_events (pr_number, stage, at, detail) VALUES (?, ?, ?, ?)`,
		prNumber, string(stage), now, detail,
	)
	if err != nil {
		slog.Error("sqlitestore: failed to insert stage_event", "pr", prNumber, "stage", stage, "error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		slog.Error("sqlitestore: failed to commit Report", "pr", prNumber, "stage", stage, "error", err)
	}
}

// SetImplMeta records the implementation worktree metadata for a PR already
// known to the store. No-op for an unknown PR (UPDATE touches zero rows).
func (s *Store) SetImplMeta(prNumber int, sessionID, worktreePath, branch string) {
	now := toUnixNano(time.Now())
	_, _ = s.db.Exec(`
		UPDATE pr_progress
		SET session_id=?, worktree_path=?, impl_branch=?, last_updated=?
		WHERE pr_number=?`,
		sessionID, worktreePath, branch, now, prNumber,
	)
}

// SetReplacementPR records the replacement PR number and its GitHub URL for a
// known PR. No-op for an unknown PR.
func (s *Store) SetReplacementPR(prNumber, n int, replacementURL string) {
	now := toUnixNano(time.Now())
	_, _ = s.db.Exec(`
		UPDATE pr_progress SET replacement_pr=?, replacement_pr_url=?, last_updated=? WHERE pr_number=?`,
		n, replacementURL, now, prNumber,
	)
}

// Reap deletes rows for every PR whose number is not in openPRs, and their
// associated stage_events (CASCADE). Called once per scan cycle with the
// authoritative open-PR set from GitHub so closed PRs are pruned promptly.
func (s *Store) Reap(openPRs []int) {
	if len(openPRs) == 0 {
		// Defensive: never wipe the whole table on an empty set. A spurious
		// empty PR fetch (an API blip returning 200 with no results) would
		// otherwise destroy the idempotency history and force re-processing of
		// every PR. Stale rows from a genuine all-closed state are harmless and
		// self-heal on the next reap that sees at least one open PR.
		slog.Warn("Reap called with no open PRs — skipping prune to protect idempotency history")
		return
	}
	ph := make([]string, len(openPRs))
	args := make([]any, len(openPRs))
	for i, n := range openPRs {
		ph[i] = "?"
		args[i] = n
	}
	q := fmt.Sprintf(`DELETE FROM pr_progress WHERE pr_number NOT IN (%s)`, strings.Join(ph, ","))
	_, _ = s.db.Exec(q, args...)
}

// SetVersions records the version metadata and GitHub URL for a known PR. No-op for an unknown PR.
func (s *Store) SetVersions(prNumber int, oldVer, newVer, ecosystem, url string) {
	now := toUnixNano(time.Now())
	_, _ = s.db.Exec(`
		UPDATE pr_progress SET old_version=?, new_version=?, ecosystem=?, pr_url=?, last_updated=?
		WHERE pr_number=?`,
		oldVer, newVer, ecosystem, url, now, prNumber,
	)
}

// SetCI records the latest CI snapshot for a known PR. No-op for an unknown PR.
// Replaces all ci_checks rows wholesale in a single transaction.
func (s *Store) SetCI(prNumber int, ci models.CIStatus) {
	now := toUnixNano(time.Now())
	tx, err := s.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback() //nolint:errcheck
	_, err = tx.Exec(`
		UPDATE pr_progress
		SET ci_state=?, ci_total=?, ci_passed=?, ci_failed=?, ci_pending=?, last_updated=?
		WHERE pr_number=?`,
		ci.State, ci.Total, ci.Passed, ci.Failed, ci.Pending, now, prNumber,
	)
	if err != nil {
		return
	}
	if _, err = tx.Exec(`DELETE FROM ci_checks WHERE pr_number=?`, prNumber); err != nil {
		return
	}
	for _, c := range ci.Checks {
		_, err = tx.Exec(`
			INSERT INTO ci_checks (pr_number, name, status, conclusion, details_url, output, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			prNumber, c.Name, c.Status, c.Conclusion, c.DetailsURL, c.Output, toUnixNano(c.CreatedAt),
		)
		if err != nil {
			return
		}
	}
	_ = tx.Commit()
}

// SetAnalysis records the analyser verdict for a known PR. No-op for an unknown PR.
func (s *Store) SetAnalysis(prNumber int, a models.AgentAnalysis) {
	b, err := json.Marshal(a)
	if err != nil {
		return
	}
	now := toUnixNano(time.Now())
	_, _ = s.db.Exec(`
		UPDATE pr_progress SET analysis_json=?, last_updated=? WHERE pr_number=?`,
		string(b), now, prNumber,
	)
}

// SetOutcome records the terminal head SHA and outcome for a known PR (Bug #23).
// Called alongside every terminal reportStage so that the next scan cycle can
// skip re-processing at the same head SHA via a DB lookup instead of reading
// back a PR comment. No-op for an unknown PR or when headSHA is empty.
func (s *Store) SetOutcome(prNumber int, headSHA, outcome string) {
	if headSHA == "" {
		return
	}
	now := toUnixNano(time.Now())
	_, _ = s.db.Exec(`
		UPDATE pr_progress SET head_sha=?, outcome=?, last_updated=? WHERE pr_number=?`,
		headSHA, outcome, now, prNumber,
	)
}

// RecordCreatedPR records that the tool opened sweeper PR createdPR for origin
// dependabot PR originPR. Written to the reap-exempt created_prs table so it
// permanently excludes the tool's own PRs from future scans (Q14 / review C1).
// Idempotent via INSERT OR REPLACE.
func (s *Store) RecordCreatedPR(createdPR, originPR int) {
	_, err := s.db.Exec(`
		INSERT INTO created_prs (pr_number, origin_pr, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(pr_number) DO UPDATE SET origin_pr = excluded.origin_pr`,
		createdPR, originPR, toUnixNano(time.Now()),
	)
	if err != nil {
		slog.Error("sqlitestore: failed to record created PR", "pr", createdPR, "origin", originPR, "error", err)
	}
}

// ---- Reader methods -------------------------------------------------------

// CreatedPRs returns the created-PR → origin-PR map from the reap-exempt
// created_prs table. Empty map on any error (callers treat it as "exclude
// nothing extra", which is safe — the author filter still applies).
func (s *Store) CreatedPRs() map[int]int {
	out := make(map[int]int)
	rows, err := s.db.Query(`SELECT pr_number, origin_pr FROM created_prs`)
	if err != nil {
		slog.Error("sqlitestore: failed to read created_prs", "error", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var createdPR, originPR int
		if err := rows.Scan(&createdPR, &originPR); err != nil {
			continue
		}
		out[createdPR] = originPR
	}
	return out
}

// Get returns a deep copy of a PR's progress and whether it exists.
func (s *Store) Get(prNumber int) (models.PRProgress, bool) {
	row := s.db.QueryRow(`
		SELECT pr_number, package_name, bump_type, stage,
		       session_id, worktree_path, impl_branch, replacement_pr, last_updated,
		       old_version, new_version, ecosystem, pr_url, replacement_pr_url,
		       ci_state, ci_total, ci_passed, ci_failed, ci_pending, analysis_json,
		       head_sha, outcome
		FROM pr_progress WHERE pr_number=?`, prNumber)

	p, err := scanProgress(row)
	if err == sql.ErrNoRows {
		return models.PRProgress{}, false
	}
	if err != nil {
		return models.PRProgress{}, false
	}
	p.History = loadEvents(s.db, prNumber)
	p.CI = loadCIChecks(s.db, prNumber, p.CI)
	// Failures aren't persisted as a column — reconstruct them from the stored
	// checks so the dashboard's failing-checks list is populated.
	if p.CI != nil {
		p.CI.Failures = models.DeriveFailures(p.CI.Checks)
	}
	return p, true
}

// All returns deep copies of every PR's progress, sorted ascending by PR number.
// Uses separate queries (not joins) for clarity and to avoid multiplying rows.
func (s *Store) All() []models.PRProgress {
	rows, err := s.db.Query(`
		SELECT pr_number, package_name, bump_type, stage,
		       session_id, worktree_path, impl_branch, replacement_pr, last_updated,
		       old_version, new_version, ecosystem, pr_url, replacement_pr_url,
		       ci_state, ci_total, ci_passed, ci_failed, ci_pending, analysis_json,
		       head_sha, outcome
		FROM pr_progress ORDER BY pr_number`)
	if err != nil {
		return []models.PRProgress{}
	}
	defer rows.Close()

	prs := make([]models.PRProgress, 0)
	prIdx := map[int]int{} // pr_number -> index in prs
	for rows.Next() {
		p, err := scanProgress(rows)
		if err != nil {
			continue
		}
		prIdx[p.PRNumber] = len(prs)
		prs = append(prs, p)
	}
	if err := rows.Err(); err != nil {
		return prs
	}

	// Load all stage_events in one query and attach them.
	evRows, err := s.db.Query(`
		SELECT pr_number, stage, at, detail FROM stage_events ORDER BY pr_number, id`)
	if err == nil {
		defer evRows.Close()
		for evRows.Next() {
			var prNum int
			var ev models.StageEvent
			var nanos int64
			var stageStr string
			if err := evRows.Scan(&prNum, &stageStr, &nanos, &ev.Detail); err != nil {
				continue
			}
			ev.Stage = models.PRStage(stageStr)
			ev.At = fromUnixNano(nanos)
			if idx, ok := prIdx[prNum]; ok {
				prs[idx].History = append(prs[idx].History, ev)
			}
		}
	}

	// Load all ci_checks in one query and attach them.
	chkRows, err := s.db.Query(`
		SELECT pr_number, name, status, conclusion, details_url, output, created_at
		FROM ci_checks ORDER BY pr_number, id`)
	if err == nil {
		defer chkRows.Close()
		for chkRows.Next() {
			var prNum int
			var c models.CheckDetail
			var conc sql.NullString
			var nanos int64
			if err := chkRows.Scan(&prNum, &c.Name, &c.Status, &conc, &c.DetailsURL, &c.Output, &nanos); err != nil {
				continue
			}
			if conc.Valid {
				v := conc.String
				c.Conclusion = &v
			}
			c.CreatedAt = fromUnixNano(nanos)
			if idx, ok := prIdx[prNum]; ok {
				if prs[idx].CI == nil {
					prs[idx].CI = &models.CIStatus{}
				}
				prs[idx].CI.Checks = append(prs[idx].CI.Checks, c)
			}
		}
	}

	// Reconstruct each PR's Failures list from its attached checks (not stored
	// as a column) so the dashboard's failing-checks list is populated.
	for i := range prs {
		if prs[i].CI != nil {
			prs[i].CI.Failures = models.DeriveFailures(prs[i].CI.Checks)
		}
	}

	sort.Slice(prs, func(i, j int) bool { return prs[i].PRNumber < prs[j].PRNumber })
	return prs
}

// ---- helpers ---------------------------------------------------------------

// scanProgress scans one pr_progress row from either a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanProgress(row scanner) (models.PRProgress, error) {
	var p models.PRProgress
	var replPR sql.NullInt64
	var nanos int64
	var stageStr, ciState, analysisJSON string
	var ciTotal, ciPassed, ciFailed, ciPending int
	err := row.Scan(
		&p.PRNumber, &p.PackageName, &p.BumpType, &stageStr,
		&p.SessionID, &p.WorktreePath, &p.ImplBranch, &replPR, &nanos,
		&p.OldVersion, &p.NewVersion, &p.Ecosystem, &p.URL, &p.ReplacementPRURL,
		&ciState, &ciTotal, &ciPassed, &ciFailed, &ciPending, &analysisJSON,
		&p.HeadSHA, &p.Outcome,
	)
	if err != nil {
		return p, err
	}
	p.Stage = models.PRStage(stageStr)
	p.LastUpdated = fromUnixNano(nanos)
	if replPR.Valid {
		v := int(replPR.Int64)
		p.ReplacementPR = &v
	}
	// Build CI aggregate; ci_checks (Checks slice) loaded separately.
	if ciState != "" || ciTotal > 0 {
		p.CI = &models.CIStatus{
			State:   ciState,
			Total:   ciTotal,
			Passed:  ciPassed,
			Failed:  ciFailed,
			Pending: ciPending,
		}
	}
	// Unmarshal analysis JSON if present.
	if analysisJSON != "" {
		var a models.AgentAnalysis
		if err := json.Unmarshal([]byte(analysisJSON), &a); err == nil {
			p.Analysis = &a
		}
	}
	return p, nil
}

func loadEvents(db *sql.DB, prNumber int) []models.StageEvent {
	rows, err := db.Query(
		`SELECT stage, at, detail FROM stage_events WHERE pr_number=? ORDER BY id`,
		prNumber,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var evs []models.StageEvent
	for rows.Next() {
		var ev models.StageEvent
		var nanos int64
		var stageStr string
		if err := rows.Scan(&stageStr, &nanos, &ev.Detail); err != nil {
			continue
		}
		ev.Stage = models.PRStage(stageStr)
		ev.At = fromUnixNano(nanos)
		evs = append(evs, ev)
	}
	return evs
}

// loadCIChecks fetches the ci_checks rows for prNumber and appends them to the
// Checks slice of an existing *CIStatus (or creates one if ciAgg is nil). Used
// by Get to populate individual PR checks after scanProgress builds the aggregate.
func loadCIChecks(db *sql.DB, prNumber int, ciAgg *models.CIStatus) *models.CIStatus {
	rows, err := db.Query(
		`SELECT name, status, conclusion, details_url, output, created_at
		 FROM ci_checks WHERE pr_number=? ORDER BY id`,
		prNumber,
	)
	if err != nil {
		return ciAgg
	}
	defer rows.Close()
	var checks []models.CheckDetail
	for rows.Next() {
		var c models.CheckDetail
		var conc sql.NullString
		var nanos int64
		if err := rows.Scan(&c.Name, &c.Status, &conc, &c.DetailsURL, &c.Output, &nanos); err != nil {
			continue
		}
		if conc.Valid {
			v := conc.String
			c.Conclusion = &v
		}
		c.CreatedAt = fromUnixNano(nanos)
		checks = append(checks, c)
	}
	if len(checks) == 0 {
		return ciAgg
	}
	if ciAgg == nil {
		ciAgg = &models.CIStatus{}
	}
	ciAgg.Checks = checks
	return ciAgg
}

// toUnixNano converts a time.Time to unix nanoseconds for DB storage.
// time.Time{} (zero) maps to 0.
func toUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromUnixNano converts unix nanoseconds back to time.Time (UTC).
// 0 maps to the zero time.Time{}.
func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
