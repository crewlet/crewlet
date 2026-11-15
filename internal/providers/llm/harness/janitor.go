package harness

import (
	"context"
	"slices"
	"sync"
	"time"
)

// janitor holds the captures something still held at their child's end, and
// keeps each of them EMPTY until its holder lets go.
//
// A pipe bounded a writer that outlived its call: once the engine closed the
// read end, its writes failed. Into a file every write succeeds, so a helper
// that escaped the harness's group with its stdout, and logs a megabyte a
// second for an hour after the call, would write 3.6 GB to an inode nobody
// will ever read: on a data pod, the volume the store and the stream live on;
// on a seat pod, scratch whose accounting never sees an unlinked file. So
// every [tailPoll] the janitor truncates each capture it holds to nothing
// through the engine's read-write description, which gives back its blocks,
// its reserved room and its size while the holder keeps appending (at the
// file's end, which is now its start), and then asks whether anything still
// holds it: once nothing does it closes the capture, which frees the inode,
// and logs `harness_capture_released`. An escaped writer so holds at most
// its rate times tailPoll on the volume, or in memory.
//
// ONE PER PROVIDER, its zero value ready, and its goroutine runs only while it
// holds a capture: the provider has no close, and a janitor with nothing to
// watch has no reason to run.
//
// What it cannot reach: a holder through a description the pin does not see
// (a holder that unlocked its own stdout, or reopened its capture by path),
// which the end read as let go; and an engine that exits, with which it goes.
type janitor struct {
	mu      sync.Mutex
	held    []*heldCapture
	running bool

	// tick, where set, is the suite's ticker in place of tailPoll's.
	tick func(d time.Duration) (<-chan time.Time, func())
	// observe, where set, is told after each tick's work on a capture its
	// stream and the bytes its blocks took once emptied.
	observe func(stream string, allocated int64)
}

// heldCapture is one capture the janitor holds.
type heldCapture struct {
	ctx       context.Context
	c         *capture
	since     time.Time
	discarded int64
}

// hold takes c, which something still holds at its child's end.
func (j *janitor) hold(ctx context.Context, c *capture) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.held = append(j.held, &heldCapture{ctx: ctx, c: c, since: time.Now()})
	if !j.running {
		j.running = true
		go j.run()
	}
}

// run empties every capture it holds each tick, and lets go of each that
// nothing holds any more, until it holds none.
func (j *janitor) run() {
	hooks := &captureHooks{ticker: j.tick}
	tick, stop := hooks.newTicker(tailPoll)
	defer stop()
	for range tick {
		j.mu.Lock()
		list := slices.Clone(j.held)
		j.mu.Unlock()
		var released []*heldCapture
		for _, h := range list {
			if size, err := h.c.size(); err == nil {
				h.discarded += size
			}
			_ = h.c.empty()
			left := allocated(h.c.r)
			if pinned, err := h.c.pinned(); err == nil && !pinned {
				_ = h.c.r.Close()
				released = append(released, h)
				log.InfoContext(h.ctx, "harness_capture_released", "stream", h.c.stream,
					"discarded_bytes", h.discarded, "held_for", time.Since(h.since).Round(time.Millisecond))
			}
			if j.observe != nil {
				j.observe(h.c.stream, left)
			}
		}
		j.mu.Lock()
		j.held = slices.DeleteFunc(j.held, func(h *heldCapture) bool { return slices.Contains(released, h) })
		if len(j.held) == 0 {
			j.running = false
			j.mu.Unlock()
			return
		}
		j.mu.Unlock()
	}
}
