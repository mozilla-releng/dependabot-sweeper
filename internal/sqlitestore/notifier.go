package sqlitestore

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
)

// Notifier implements progress.Notifier for the web process. It polls
// PRAGMA data_version on the shared SQLite file: SQLite increments data_version
// whenever *another* database connection commits a write, so every worker
// commit is visible to the web process's connection within one poll interval.
//
// On a detected change the Notifier broadcasts to all subscribers using the
// same buffered-size-1 non-blocking fan-out as state.Store, so existing SSE
// clients receive an "event: update" and re-fetch — the browser contract is
// unchanged.
//
// Typical usage:
//
//	n := sqlitestore.NewNotifier(store.DB(), time.Second)
//	go n.Run(ctx)
//	srv := web.NewServer(store, n, ...)
type Notifier struct {
	db       *sql.DB
	interval time.Duration

	mu   sync.RWMutex
	subs map[chan struct{}]struct{}
}

// compile-time assertion
var _ progress.Notifier = (*Notifier)(nil)

// NewNotifier creates a Notifier that polls db every interval.
func NewNotifier(db *sql.DB, interval time.Duration) *Notifier {
	return &Notifier{
		db:       db,
		interval: interval,
		subs:     make(map[chan struct{}]struct{}),
	}
}

// Subscribe returns a buffered channel (capacity 1) that receives an empty
// struct on every detected commit from the worker process. Callers MUST call
// Unsubscribe when done (e.g. on SSE client disconnect).
func (n *Notifier) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.subs[ch] = struct{}{}
	n.mu.Unlock()
	return ch
}

// Unsubscribe removes and closes the channel. Safe to call once per channel.
func (n *Notifier) Unsubscribe(ch chan struct{}) {
	n.mu.Lock()
	if _, ok := n.subs[ch]; ok {
		delete(n.subs, ch)
		close(ch)
	}
	n.mu.Unlock()
}

// Run polls PRAGMA data_version at n.interval until ctx is cancelled. The
// first poll initialises the baseline and does NOT broadcast (avoids a
// spurious "re-fetch" on startup). Run returns nil when ctx is done.
func (n *Notifier) Run(ctx context.Context) error {
	ticker := time.NewTicker(n.interval)
	defer ticker.Stop()

	var last int64
	initialised := false

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var v int64
			if err := n.db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v); err != nil {
				continue // transient error — skip tick
			}
			if !initialised {
				last = v
				initialised = true
				continue
			}
			if v != last {
				last = v
				n.broadcast()
			}
		}
	}
}

// broadcast does a non-blocking send to every subscriber. A slow reader misses
// the tick and catches up on the next one — this is intentional (same as state.Store).
func (n *Notifier) broadcast() {
	n.mu.RLock()
	for ch := range n.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	n.mu.RUnlock()
}
