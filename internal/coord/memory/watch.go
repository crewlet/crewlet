package memory

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
)

// watcher is one open watch on the twin, for any record family: an unbounded
// queue a writer appends to under the store's lock and never waits on, and a
// goroutine that hands it on ([watcher.serve]).
//
// UNBOUNDED, because the alternative is a writer that blocks on a slow reader
// while holding the lock every other call in the twin takes. What a watch
// carries is the changes to a small set of records — a handful of seat
// pauses, one run, a read state per human seat, the leases of one class —
// which a reader consumes promptly.
//
// ONE type for every family, for the KV backend's reason (its watch.go): the
// ordering guarantee is the same everywhere — the snapshot and the
// registration happen under ONE lock, so a write lands either before the
// snapshot and is in it, or after the registration and is delivered as a
// change — and two copies of it are two places to get that wrong.
type watcher[U any] struct {
	mu      sync.Mutex
	pending []U
	wake    chan struct{}
}

// newWatcher opens a watch whose first updates are initial, already queued.
func newWatcher[U any](initial ...U) *watcher[U] {
	w := &watcher[U]{pending: initial, wake: make(chan struct{}, 1)}
	w.signal()
	return w
}

func (w *watcher[U]) push(u U) {
	w.mu.Lock()
	w.pending = append(w.pending, u)
	w.mu.Unlock()
	w.signal()
}

func (w *watcher[U]) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *watcher[U]) drain() []U {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.pending
	w.pending = nil
	return out
}

// serve hands every update queued on w to the channel it returns, in the order
// they were queued, until ctx ends — then unregisters w through stop, so no
// writer queues onto a watch nobody reads, and closes the channel, which is
// how a consumer tells a watch that stopped from one that is merely quiet.
func (w *watcher[U]) serve(ctx context.Context, stop func()) <-chan U {
	out := make(chan U)
	go func() {
		defer close(out)
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			}
			for _, u := range w.drain() {
				select {
				case out <- u:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// ---- the lease watch ---------------------------------------------------- //

// leaseWatch is one running [Backend.WatchLive]: a [watcher] of the writes to
// one class's leases, which [Backend.wrote] queues onto it under the store's
// lock — every claim, renewal and release takes that lock, so a send there
// would hold every node's heartbeat behind a slow consumer.
type leaseWatch struct {
	class coord.Class
	*watcher[coord.LeaseWrite]
}

// WatchLive streams every write to a lease of one class made after it returns.
//
// Registered under the store's lock, so a write is either before the watch
// (and in no delivery) or after it (and in this one) — never lost between the
// two, which is the order a view relies on when it lists after the watch has
// begun.
func (b *Backend) WatchLive(ctx context.Context, class coord.Class) (<-chan coord.LeaseWrite, error) {
	if err := checkClass(class); err != nil {
		return nil, err
	}
	if err := unavailable(ctx); err != nil {
		return nil, err
	}
	w := &leaseWatch{class: class, watcher: newWatcher[coord.LeaseWrite]()}
	b.mu.Lock()
	if b.watchers == nil {
		b.watchers = map[*leaseWatch]struct{}{}
	}
	b.watchers[w] = struct{}{}
	b.mu.Unlock()
	return w.serve(ctx, func() {
		b.mu.Lock()
		delete(b.watchers, w)
		b.mu.Unlock()
	}), nil
}

// wrote tells every watch of the row's class what a write left in it. Callers
// hold b.mu, which is what orders the writes each watch is handed exactly as
// the store applied them.
func (b *Backend) wrote(r *record) {
	if len(b.watchers) == 0 {
		return
	}
	write := coord.LeaseWrite{Resource: r.resource, Owner: r.owner, Epoch: r.epoch}
	for w := range b.watchers {
		if w.class.Holds(r.resource) {
			w.push(write)
		}
	}
}
