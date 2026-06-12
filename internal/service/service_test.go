package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

func TestRunsScanOnStartAndOnTick(t *testing.T) {
	var calls atomic.Int32
	scan := func(ctx context.Context) []models.ReviewResult {
		calls.Add(1)
		return nil
	}
	svc := New(scan, 60*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = svc.Run(ctx); close(done) }()

	time.Sleep(220 * time.Millisecond)
	cancel()
	<-done

	if got := calls.Load(); got < 3 {
		t.Errorf("scan calls = %d, want >= 3 (start + 2 ticks)", got)
	}
}

func TestSingleScanGuardDropsOverlappingTicks(t *testing.T) {
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	release := make(chan struct{})
	var once sync.Once

	scan := func(ctx context.Context) []models.ReviewResult {
		c := concurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if c <= old || maxConcurrent.CompareAndSwap(old, c) {
				break
			}
		}
		<-release
		concurrent.Add(-1)
		return nil
	}

	svc := New(scan, 20*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = svc.Run(ctx); close(done) }()

	time.Sleep(120 * time.Millisecond)
	once.Do(func() { close(release) })
	cancel()
	<-done

	if got := maxConcurrent.Load(); got > 1 {
		t.Errorf("max concurrent scans = %d, want 1 (single-scan guard failed)", got)
	}
}

func TestStatusReflectsScanTiming(t *testing.T) {
	scan := func(ctx context.Context) []models.ReviewResult { return nil }
	svc := New(scan, time.Hour, nil)

	if st := svc.Status(); st.InFlight != 0 {
		t.Errorf("initial InFlight = %d, want 0", st.InFlight)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = svc.Run(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	st := svc.Status()
	cancel()
	<-done

	if st.LastScan.IsZero() {
		t.Errorf("LastScan still zero after a completed scan")
	}
	if !st.NextScan.After(st.LastScan) {
		t.Errorf("NextScan (%v) should be after LastScan (%v)", st.NextScan, st.LastScan)
	}
}

func TestGracefulShutdownReturnsNil(t *testing.T) {
	scan := func(ctx context.Context) []models.ReviewResult { return nil }
	svc := New(scan, time.Hour, nil)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- svc.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return within 2s of context cancel")
	}
}
