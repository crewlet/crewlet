package memory

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the lease watch ---------------------------------------------------- //

// watcher is one running [Backend.WatchLive]: the writes to its class the
// store has made and its consumer has not yet been handed.
//
// A QUEUE OF ITS OWN rather than a send under the store's lock, because every
// claim, renewal and release takes that lock: a watch whose consumer was slow
// to read would otherwise hold every node's heartbeat behind it. The queue is
// unbounded for the same reason, and the contract is what keeps it short —
// its consumer drains it, and a write is a few words.
type watcher struct {
	class coord.Class

	mu    sync.Mutex
	queue []coord.LeaseWrite
	// wake holds one signal: any number of writes between two reads of the
	// queue are one wake-up, since the reader takes the whole queue.
	wake chan struct{}
}

// push queues one write and wakes the reader.
func (w *watcher) push(write coord.LeaseWrite) {
	w.mu.Lock()
	w.queue = append(w.queue, write)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take hands over every write queued so far, in order.
func (w *watcher) take() []coord.LeaseWrite {
	w.mu.Lock()
	defer w.mu.Unlock()
	writes := w.queue
	w.queue = nil
	return writes
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
	w := &watcher{class: class, wake: make(chan struct{}, 1)}
	b.mu.Lock()
	if b.watchers == nil {
		b.watchers = map[*watcher]struct{}{}
	}
	b.watchers[w] = struct{}{}
	b.mu.Unlock()

	out := make(chan coord.LeaseWrite)
	go func() {
		defer close(out)
		defer func() {
			b.mu.Lock()
			delete(b.watchers, w)
			b.mu.Unlock()
		}()
		for {
			for _, write := range w.take() {
				select {
				case out <- write:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-w.wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
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
