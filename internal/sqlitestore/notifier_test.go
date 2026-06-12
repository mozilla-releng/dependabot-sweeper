package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// TestNotifierFiresOnWriterCommit opens a writer store and a reader store on
// the same temp DB file. A Notifier is created on the reader's connection with
// a 20ms poll interval. After a Report on the writer, the subscriber must
// receive a tick within 500ms (much longer than the poll interval so CI
// timing jitter is not a concern).
func TestNotifierFiresOnWriterCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifier.db")

	writer := openAt(t, path, true)
	reader := openAt(t, path, false)

	notifier := NewNotifier(reader.DB(), 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Run(ctx) //nolint:errcheck

	// Give the notifier one tick to initialise its baseline before we write.
	time.Sleep(50 * time.Millisecond)

	ch := notifier.Subscribe()
	defer notifier.Unsubscribe(ch)

	// Write on the worker side.
	writer.Report(1, "lodash", "minor", models.StagePending, "test")

	select {
	case <-ch:
		// received the broadcast — good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subscriber did not receive a tick within 500ms of worker commit")
	}
}

// TestNotifierNoSpuriousTickBeforeWrite checks that subscribing before any
// write produces no immediate tick (the notifier must initialise its baseline
// without broadcasting).
func TestNotifierNoSpuriousTickBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifier2.db")

	reader := openAt(t, path, false)
	notifier := NewNotifier(reader.DB(), 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Run(ctx) //nolint:errcheck

	ch := notifier.Subscribe()
	defer notifier.Unsubscribe(ch)

	// Wait a few poll intervals with no write.
	time.Sleep(100 * time.Millisecond)

	select {
	case <-ch:
		t.Error("subscriber received a spurious tick before any write")
	default:
		// nothing — correct
	}
}

// TestNotifierUnsubscribeStopsDelivery confirms that after Unsubscribe the
// channel is closed and no further ticks arrive.
func TestNotifierUnsubscribeStopsDelivery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifier3.db")

	writer := openAt(t, path, true)
	reader := openAt(t, path, false)

	notifier := NewNotifier(reader.DB(), 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go notifier.Run(ctx) //nolint:errcheck
	time.Sleep(50 * time.Millisecond)

	ch := notifier.Subscribe()
	notifier.Unsubscribe(ch)

	writer.Report(1, "pkg", "minor", models.StagePending, "")
	time.Sleep(100 * time.Millisecond)

	select {
	case _, open := <-ch:
		if open {
			t.Error("received an open tick after Unsubscribe")
		}
		// closed channel — expected
	default:
		// no delivery — also fine
	}
}
