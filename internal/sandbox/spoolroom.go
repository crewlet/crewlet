package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// Every session spool on a node is held to one room, counted in bytes: the
// package doc's "Every spool on a node in one room" has the argument.

// spoolRoomBytes is the room every spool of one process shares: a sealed
// session at its bound ([executorkeep.SealedMaxBytes]) for each save a node
// takes at once ([maxConcurrentSaves]) and one more, so saves, which never
// wait for it, always leave a final take room for a session at its bound.
// 240.06 MiB today.
var spoolRoomBytes = int64(maxConcurrentSaves+1) * executorkeep.SealedMaxBytes

// spoolRoomWait is the longest a take waits for room in its spool before it
// keeps nothing: room comes back as uploads end, seconds for a session at its
// bound on a working store, so a minute without it is a store near its floor
// pace, which no read should hold its seat for. Within the take's own budget:
// this minute, the box's two readings at the bound (96 s) and the scrub's
// measured worst (238 s) end in 394 s, inside its seven minutes
// ([SessionScrubWait]).
const spoolRoomWait = time.Minute

// errSpoolPastHold is a seal refused before it wrote past the room its spool
// holds: a save's counted size ([countedFor]), or a take's.
var errSpoolPastHold = errors.New("sandbox: the session sealed would pass the room its spool holds")

// spoolHold is room one spool holds in its process's room, from before its
// first byte until its file is removed.
type spoolHold struct {
	room *semaphore.Weighted
	mu   sync.Mutex
	n    int64
}

// trySpoolRoom takes n bytes of the room without waiting, nil where they are
// not free; never ahead of a take waiting for room ([semaphore.Weighted.TryAcquire]).
func (c *Coordinator) trySpoolRoom(n int64) *spoolHold {
	if !c.spoolRoom.TryAcquire(n) {
		return nil
	}
	return &spoolHold{room: c.spoolRoom, n: n}
}

// awaitSpoolRoom takes n bytes of the room, waiting at most [spoolRoomWait]
// within ctx.
func (c *Coordinator) awaitSpoolRoom(ctx context.Context, n int64) (*spoolHold, error) {
	wait, cancel := context.WithTimeout(ctx, spoolRoomWait)
	defer cancel()
	if err := c.spoolRoom.Acquire(wait, n); err != nil {
		if ctx.Err() == nil {
			return nil, fmt.Errorf("this node's session spools held their whole room (%d bytes) for %s, "+
				"so the session was not taken: the next launch continues the turn's newest kept capture",
				spoolRoomBytes, spoolRoomWait)
		}
		return nil, fmt.Errorf("the take's time ended while it waited for room to spool the session: %w",
			ctx.Err())
	}
	return &spoolHold{room: c.spoolRoom, n: n}, nil
}

// bytes is the room h holds.
func (h *spoolHold) bytes() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// shrinkTo gives back all but n bytes of what h holds; nothing where it holds
// n or less.
func (h *spoolHold) shrinkTo(n int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n < h.n {
		h.room.Release(h.n - n)
		h.n = n
	}
}

// release gives back everything h holds. Safe on nil and twice.
func (h *spoolHold) release() {
	if h != nil {
		h.shrinkTo(0)
	}
}

// heldWriter writes to w at most left bytes, the room its spool holds, and
// refuses whole the write that would pass it, so no byte past the room
// reaches the disk.
type heldWriter struct {
	w    io.Writer
	left int64
}

func (h *heldWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > h.left {
		return 0, errSpoolPastHold
	}
	n, err := h.w.Write(p)
	h.left -= int64(n)
	return n, err
}
