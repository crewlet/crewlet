package engine

import (
	"context"
	"sync"
	"time"
)

// memorySyncInterval is how often a node carries what its seats have just
// learned onto the memory changelog.
//
// IT IS THE CRASH WINDOW, and that is the only thing sizing it. A graceful
// handoff flushes on release, so a drain, a rolling upgrade or a rebalance
// loses nothing whatever this value is. What it bounds is the other case: a
// node that dies without releasing loses the memory written since its last
// cycle.
//
// Thirty seconds against a turn that takes minutes, so the worst case is
// roughly one turn's worth of diary entries and one episode — and a seat that
// re-runs a lost turn writes them again. Shorter buys a smaller window on a
// failure that is already rare; longer starts losing whole turns of learning
// to a single crash.
//
// It is also comfortably inside the 45-second lease TTL, so a node that stops
// publishing has usually published once more before a peer can take its seats
// and hydrate.
const memorySyncInterval = 30 * time.Second

// memoryFlushTimeout bounds the last publish a released seat gets.
//
// Five seconds, against a path that already holds the seat's teardown: it is
// long enough for a broker under load to take a cycle's worth of rows, and
// short enough that a drain of a dozen seats against a broker that has
// stopped answering finishes in under a minute rather than waiting on each
// one. What is lost when it expires is the same bounded thing a crash loses —
// whatever this node learned since its last cycle.
const memoryFlushTimeout = 5 * time.Second

// memorySync is the publish loop and the handle that stops it.
type memorySync struct {
	stop chan struct{}
	done sync.WaitGroup
}

// startMemorySync carries the memory of the seats this node holds.
//
// A PER-NODE LOOP rather than a fleet singleton, which is the opposite of the
// retention sweep beside it: a singleton is right when N nodes would do the
// same work, and here they would each do DIFFERENT work — every node has its
// own store holding its own seats' memory, and only that node can read it.
//
// Nothing here fails a turn or a seat. A publish that does not land leaves the
// changelog behind by one cycle, which the next cycle repairs and a release
// flushes outright.
func (e *Engine) startMemorySync(ctx context.Context) {
	if e.memory == nil {
		return
	}
	// DETACHED from the signal context, like the sweep and the waiter
	// beside it: a loop bound to it stops at SIGTERM, before the drain has
	// released the seats whose memory it is carrying. Its own stop channel
	// is what ends it, and stopMemorySync is what closes that — after the
	// drain, so the last cycle a seat gets is the flush on its release.
	ctx = context.WithoutCancel(ctx)
	loop := &memorySync{stop: make(chan struct{})}
	e.memorySync = loop
	ticker := time.NewTicker(memorySyncInterval)
	loop.done.Go(func() {
		defer ticker.Stop()
		for {
			select {
			case <-loop.stop:
				return
			case <-ticker.C:
				e.syncSeatMemory(ctx)
			}
		}
	})
}

// stopMemorySync ends the loop, waiting for an in-flight cycle.
//
// Waiting rather than signalling and walking away: a cycle mid-publish holds
// a store read and a broker write, and letting the process close either one
// underneath it turns an orderly shutdown into a logged failure on the way
// out. Called AFTER the drain, which has already flushed every seat this
// node held — so what this ends is a loop with nothing left to carry.
func (e *Engine) stopMemorySync() {
	if e.memorySync == nil {
		return
	}
	close(e.memorySync.stop)
	e.memorySync.done.Wait()
	e.memorySync = nil
}

// syncSeatMemory publishes one cycle for every seat this node holds.
//
// BOUNDED BY THE INTERVAL ITSELF. The loop's context is detached, so without
// this a publish against a broker that has stopped answering blocks the cycle
// for ever — and stopMemorySync, which waits for it, would then block the
// shutdown for ever with it. A cycle that cannot finish before the next one
// is due has fallen behind whatever it does next, so the interval is the
// value: what it abandons, the following cycle republishes.
func (e *Engine) syncSeatMemory(ctx context.Context) {
	node := e.node
	if node == nil {
		return
	}
	host := node.Host()
	if host == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, memorySyncInterval)
	defer cancel()
	for _, handle := range host.Held() {
		if _, err := e.memory.Publish(ctx, handle); err != nil {
			// Logged and stepped over: one seat's changelog falling a
			// cycle behind must not stop the others from being carried.
			log.WarnContext(ctx, "seat_memory_not_carried",
				"seat", handle, "error", err)
		}
	}
}
