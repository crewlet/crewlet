package memory

import (
	"context"
	"sync"
)

// One open watch on the twin, for any record family: an unbounded queue a
// writer appends to under the fleet's lock and never waits on, and a goroutine
// that hands it on.
//
// UNBOUNDED, because the alternative is a writer that blocks on a slow reader
// while holding the lock every other call in the twin takes. The sets watched
// are small — a handful of seat pauses, a record per human seat — and a reader
// consumes them promptly.
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

func newWatcher[U any](answer []U) *watcher[U] {
	w := &watcher[U]{pending: answer, wake: make(chan struct{}, 1)}
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

// serve hands the watcher's queue on until ctx ends, then calls leave — which
// deregisters the watcher under the fleet's lock — and closes the channel.
func (w *watcher[U]) serve(ctx context.Context, leave func()) <-chan U {
	out := make(chan U)
	go func() {
		defer close(out)
		defer leave()
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
