package jsapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/objstore"
)

// LinkWindow is the bytes a link's [Window] lets out at once, both ways
// together: what the slowest link the engine allows ([objstore.MiBPace])
// carries within [WindowCrossing], 139,810 bytes, four seconds at the floor,
// whatever lease the node holds and whatever rate the link runs at. A request
// sent behind a full window crosses it inside its [RequestTerm].
//
// NOT SIZED BY A MEASURED RATE: after a drop to the floor every request would
// sit past its term while a window sized for the faster rate drained.
const LinkWindow = int(int64(WindowCrossing) * objstore.MiB / int64(objstore.MiBPace))

// Window is the bytes a node's bulk transfers hold out across ONE LINK, both
// directions together, first come first served: the LINK'S, never a
// connection's or a direction's (the package doc, "A window across a link").
// The queue owns it and the engine hands it to every bulk sender on the link.
//
// A TAKE IS WHAT ITS TRANSFER PUTS ON THE LINK, in bytes, given back when they
// have crossed or never will. The window never holds more than its size,
// except ONE TAKE LARGER THAN IT (a replay pull at the transport's bound, a
// leader's read of a whole piece), let out alone once the window is empty. A
// waiter takes no stall of its own: every holder is bounded by its own.
type Window struct {
	size int

	mu sync.Mutex
	// held is the bytes let out and not yet given back.
	held int
	// queue is the takes not yet let out, oldest first.
	queue []*windowTake
}

// windowTake is one [Window.Take] waiting its turn; given is closed when it
// is let out.
type windowTake struct {
	n     int
	given chan struct{}
}

// NewWindow is a window of size bytes. A size below one is refused.
func NewWindow(size int) (*Window, error) {
	if size < 1 {
		return nil, fmt.Errorf("jsapi: a link's window of %d bytes lets nothing out", size)
	}
	return &Window{size: size}, nil
}

// NewLinkWindow is a window of [LinkWindow] bytes: the one a link's queue
// owns.
func NewLinkWindow() *Window { return &Window{size: LinkWindow} }

// Size is the bytes w lets out at once.
func (w *Window) Size() int { return w.size }

// Held is the bytes w has let out and not had back, and how many takes wait
// behind them: a reading for the cases that watch a transfer wait its turn.
func (w *Window) Held() (bytes, waiting int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.held, len(w.queue)
}

// errNoBytes is a take of nothing, which would let a transfer out unbounded.
var errNoBytes = errors.New("jsapi: a take of a link's window names the bytes it puts on the link")

// Take holds n bytes of w, waiting behind every take asked before it, for as
// long as ctx allows: answered with ctx's cause if it ends first. n larger
// than the window is let out alone, once w is empty.
func (w *Window) Take(ctx context.Context, n int) error {
	if n < 1 {
		return errNoBytes
	}
	w.mu.Lock()
	if len(w.queue) == 0 && w.fitsLocked(n) {
		w.held += n
		w.mu.Unlock()
		return nil
	}
	t := &windowTake{n: n, given: make(chan struct{})}
	w.queue = append(w.queue, t)
	w.mu.Unlock()
	select {
	case <-t.given:
		return nil
	case <-ctx.Done():
		w.withdraw(t)
		return context.Cause(ctx)
	}
}

// Give returns n bytes a [Window.Take] held, and lets the oldest takes out
// that now fit. More than w holds is a take given back twice, which would let
// out more than the window: a bug in its caller, so it panics rather than
// clamps.
func (w *Window) Give(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n < 1 || n > w.held {
		panic(fmt.Sprintf("jsapi: %d bytes given back to a link's window that holds %d", n, w.held))
	}
	w.held -= n
	w.admitLocked()
}

// fitsLocked reports whether a take of n can be let out now: within the
// window, or larger than it and the window empty.
func (w *Window) fitsLocked(n int) bool {
	return w.held+n <= w.size || (n > w.size && w.held == 0)
}

// admitLocked lets out the oldest takes that fit, in order: one that does
// not fit holds every take behind it back, so a take larger than the window
// is let out once those ahead of it are given back, never overtaken for ever.
func (w *Window) admitLocked() {
	for len(w.queue) > 0 && w.fitsLocked(w.queue[0].n) {
		t := w.queue[0]
		w.queue = w.queue[1:]
		w.held += t.n
		close(t.given)
	}
}

// withdraw takes t back: out of the queue if it is still waiting, which may
// let the takes behind it out, and its bytes given back if it was let out as
// its context ended.
func (w *Window) withdraw(t *windowTake) {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-t.given:
		w.held -= t.n
	default:
		w.queue = slices.DeleteFunc(w.queue, func(q *windowTake) bool { return q == t })
	}
	w.admitLocked()
}
