package sqlitestore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/web"
)

func TestStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.db")

	writer := openAt(t, path, true)
	reader := NewStatusReader(openAt(t, path, false).DB())

	// Initial state: all zeros → zero time.
	st := reader.Status()
	if st.InFlight != 0 {
		t.Errorf("initial in_flight = %d, want 0", st.InFlight)
	}
	if !st.LastScan.IsZero() {
		t.Errorf("initial last_scan = %v, want zero", st.LastScan)
	}

	// MarkScanStart sets in_flight=1.
	writer.MarkScanStart()
	st = reader.Status()
	if st.InFlight != 1 {
		t.Errorf("after MarkScanStart in_flight = %d, want 1", st.InFlight)
	}

	// MarkScanDone sets in_flight=0 and stamps last_scan.
	before := time.Now()
	writer.MarkScanDone()
	after := time.Now()
	st = reader.Status()
	if st.InFlight != 0 {
		t.Errorf("after MarkScanDone in_flight = %d, want 0", st.InFlight)
	}
	if st.LastScan.IsZero() {
		t.Error("last_scan should not be zero after MarkScanDone")
	}
	if st.LastScan.Before(before) || st.LastScan.After(after.Add(time.Millisecond)) {
		t.Errorf("last_scan %v outside expected range [%v, %v]", st.LastScan, before, after)
	}

	// SetNextScan round-trips.
	next := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	writer.SetNextScan(next)
	st = reader.Status()
	// SQLite stores nanoseconds; truncate to microseconds to absorb rounding.
	if st.NextScan.Truncate(time.Microsecond) != next.UTC().Truncate(time.Microsecond) {
		t.Errorf("next_scan = %v, want %v", st.NextScan, next)
	}
}

// TestStatusReaderImplementsProvider is a compile-time assertion (also exercised at runtime).
func TestStatusReaderImplementsProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sp.db")
	st := NewStatusReader(openAt(t, path, true).DB())
	var _ web.StatusProvider = st
}
