package sqlitestore

import (
	"database/sql"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/web"
)

// StatusReader reads the scan_status row from the DB and implements
// web.StatusProvider. Used by the web process to show scan timing from
// the shared database written by the worker process.
type StatusReader struct{ db *sql.DB }

// compile-time assertion
var _ web.StatusProvider = (*StatusReader)(nil)

// NewStatusReader returns a StatusReader backed by db.
func NewStatusReader(db *sql.DB) *StatusReader { return &StatusReader{db: db} }

// Status reads the single scan_status row. If the row is absent (should not
// happen after Open, but be defensive) returns a zero Status.
func (r *StatusReader) Status() web.Status {
	var lastNano, nextNano int64
	var inFlight int
	err := r.db.QueryRow(
		`SELECT last_scan, next_scan, in_flight FROM scan_status WHERE id=1`,
	).Scan(&lastNano, &nextNano, &inFlight)
	if err != nil {
		return web.Status{}
	}
	return web.Status{
		LastScan: fromUnixNano(lastNano),
		NextScan: fromUnixNano(nextNano),
		InFlight: inFlight,
	}
}

// — Writer-side scan status helpers (called by the worker's service layer) —

// MarkScanStart records that a scan is in flight.
func (s *Store) MarkScanStart() {
	_, _ = s.db.Exec(`UPDATE scan_status SET in_flight=1 WHERE id=1`)
}

// MarkScanDone records that the most-recent scan completed and updates last_scan.
func (s *Store) MarkScanDone() {
	now := toUnixNano(time.Now())
	_, _ = s.db.Exec(`UPDATE scan_status SET in_flight=0, last_scan=? WHERE id=1`, now)
}

// SetNextScan records when the next scan is expected.
func (s *Store) SetNextScan(t time.Time) {
	_, _ = s.db.Exec(`UPDATE scan_status SET next_scan=? WHERE id=1`, toUnixNano(t))
}
