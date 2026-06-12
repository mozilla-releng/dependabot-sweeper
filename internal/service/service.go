// Package service runs dependabot-sweeper as a persistent daemon: an internal
// ticker drives periodic scans (replacing cron), a single-scan guard prevents
// overlapping scans. Shutdown is graceful on context cancellation.
//
// The web dashboard is no longer embedded here — it is a separate process
// (the "web" subcommand) that reads state from the shared SQLite database.
// The service writes scan timing to a StatusSink (nil-safe) and keeps
// in-memory copies of the same values so tests can inspect them via Status().
package service

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/web"
)

// ScanFunc runs one full scan of the target repo and returns the per-PR results.
type ScanFunc func(ctx context.Context) []models.ReviewResult

// StatusSink is optionally provided by the DB store (sqlitestore.Store) to
// persist scan timing for the web process. Nil is accepted (tests, review).
type StatusSink interface {
	MarkScanStart()
	MarkScanDone()
	SetNextScan(t time.Time)
}

// Service is the persistent daemon.
type Service struct {
	scan     ScanFunc
	interval time.Duration
	sink     StatusSink // optional; nil for tests / nil-store paths

	scanMu sync.Mutex // held for the duration of a scan; TryLock drops overlaps

	// in-memory status fields — kept for test introspection via Status().
	// In production the web process reads these from the shared SQLite DB.
	statusMu sync.Mutex
	lastScan time.Time
	nextScan time.Time
	inFlight int
}

// New builds a Service. sink may be nil (tests, one-shot).
func New(scan ScanFunc, interval time.Duration, sink StatusSink) *Service {
	return &Service{
		scan:     scan,
		interval: interval,
		sink:     sink,
	}
}

// Status implements web.StatusProvider using the in-memory fields. Used in
// tests; the production web process reads status from the DB via
// sqlitestore.StatusReader instead.
func (s *Service) Status() web.Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return web.Status{
		LastScan: s.lastScan,
		NextScan: s.nextScan,
		InFlight: s.inFlight,
	}
}

// Run starts the ticker loop, runs an immediate first scan, then scans every
// interval until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	s.setNextScan(time.Now().Add(s.interval))
	s.runOneScan(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("Shutdown signal received — stopping service")
			return nil
		case <-ticker.C:
			s.setNextScan(time.Now().Add(s.interval))
			s.runOneScan(ctx)
		}
	}
}

// runOneScan runs a scan under the single-scan guard. If a scan is already in
// progress (TryLock fails) the tick is dropped.
func (s *Service) runOneScan(ctx context.Context) {
	if !s.scanMu.TryLock() {
		slog.Warn("Scan already in progress — skipping this tick")
		return
	}
	defer s.scanMu.Unlock()

	s.markScanStart()
	slog.Info("Starting scan")
	results := s.scan(ctx)
	s.markScanDone()
	slog.Info("Scan complete", "results", len(results))
}

func (s *Service) markScanStart() {
	s.statusMu.Lock()
	s.inFlight = 1
	s.statusMu.Unlock()
	if s.sink != nil {
		s.sink.MarkScanStart()
	}
}

func (s *Service) markScanDone() {
	s.statusMu.Lock()
	s.inFlight = 0
	s.lastScan = time.Now()
	s.statusMu.Unlock()
	if s.sink != nil {
		s.sink.MarkScanDone()
	}
}

func (s *Service) setNextScan(t time.Time) {
	s.statusMu.Lock()
	s.nextScan = t
	s.statusMu.Unlock()
	if s.sink != nil {
		s.sink.SetNextScan(t)
	}
}
