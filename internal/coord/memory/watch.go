package memory

import (
	"context"
	"sync"
)

// watcher is one open watch: an unbounded queue a writer appends to under the
// fleet's lock and never waits on, and a goroutine that hands it on ([serve]).
//
// UNBOUNDED, because the alternative is a writer that blocks on a slow reader
// while holding the lock every other call in the twin takes. What a watch
// carries is the changes to a handful of records — the seat pauses, one run —
// which a reader consumes promptly.
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
