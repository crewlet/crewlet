package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// Every session spool on a node is held to one room, counted in bytes: the
// package doc's "Every spool on a node in one room" has the argument.

// takeRoomBytes is the room one take holds, in two parts of one size: a
// sealed session at its bound ([executorkeep.SealedMaxBytes]) with as much as
// gzip adds to input it cannot shrink ([executorkeep.GzipMostBytes]) and the
// scrub spool's members ([executorkeep.SpoolMembersMostBytes]). Its sealed
// spool takes its part first, and its scrub's spool ([SessionSpool]), written
// at the kept archive's level so a session the seal keeps fits, the rest, so a
// scrub's spool never makes a seal fail. 96.23 MiB today.
var takeRoomBytes = 2 * (executorkeep.GzipMostBytes(executorkeep.SealedMaxBytes) +
	executorkeep.SpoolMembersMostBytes)

// spoolRoomBytes is the room every spool of one process shares: a take's
// ([takeRoomBytes]) for each save a node takes at once ([maxConcurrentSaves])
// and one more, so saves, which never wait for it, always leave a final take
// room for a session at its bound. 481.17 MiB today.
var spoolRoomBytes = int64(maxConcurrentSaves+1) * takeRoomBytes

// spoolRoomWait is the longest a take waits for room in its spool before it
// keeps nothing: room comes back as other takes' packs and uploads end,
// seconds for a session at its bound on a working store, so a minute without
// it is a store near its floor pace, which no read should hold its seat for.
// Counted in the scrub's six minutes ([SessionScrubWait]).
const spoolRoomWait = time.Minute

// errSpoolPastHold is a seal refused before it wrote past the room its spool
// holds: a save's counted size ([countedFor]), or a take's.
var errSpoolPastHold = errors.New("sandbox: the session sealed would pass the room its spool holds")

// SpoolOutcome is how a final take's wait for spool room ended.
type SpoolOutcome string

const (
	// SpoolTaken is a take that got its room, at once or after waiting.
	SpoolTaken SpoolOutcome = "taken"
	// SpoolRefused is a take the room stayed full for through its whole wait
	// ([spoolRoomWait]), which keeps nothing.
	SpoolRefused SpoolOutcome = "refused"
	// SpoolEnded is a take whose own time ran out while it waited.
	SpoolEnded SpoolOutcome = "ended"
)

// SpoolOutcomes is every outcome this build reports.
var SpoolOutcomes = []SpoolOutcome{SpoolTaken, SpoolRefused, SpoolEnded}

// Valid reports whether this build knows the outcome.
func (o SpoolOutcome) Valid() bool { return slices.Contains(SpoolOutcomes, o) }

// SpoolMeter is told what the spool room measures, so an operator sees final
// takes refused for room and can tell whether the room or the wait is short:
// each final take's wait by how it ended (saves never wait), and the bytes the
// room holds whenever they move. The engine records both into the node's
// metrics; this package links no state log, its instruments included.
type SpoolMeter interface {
	SpoolWaited(outcome SpoolOutcome, waited time.Duration)
	SpoolHeld(bytes int64)
}

// spoolRoom is the room every spool of one process shares, with what its
// holds hold counted here, each byte from its hold's handing out to its
// giving back: a [semaphore.Weighted] says only whether n more fit, and a
// refused take's line and the held measure need the bytes. A take that waited
// is counted just after its Acquire returns (r.mu is never held across one),
// so a reading in between is that take low, never high.
type spoolRoom struct {
	sem   *semaphore.Weighted
	meter SpoolMeter

	mu   sync.Mutex
	held int64
}

// newSpoolRoom is an empty room told to meter (nil measures nothing).
func newSpoolRoom(meter SpoolMeter) *spoolRoom {
	r := &spoolRoom{sem: semaphore.NewWeighted(spoolRoomBytes), meter: meter}
	if meter != nil {
		meter.SpoolHeld(0)
	}
	return r
}

// countLocked moves the count by d bytes and tells the meter; r.mu is held,
// so the meter hears every count in its order.
func (r *spoolRoom) countLocked(d int64) {
	r.held += d
	if r.meter != nil {
		r.meter.SpoolHeld(r.held)
	}
}

// waited tells the meter how a take's wait for room ended.
func (r *spoolRoom) waited(outcome SpoolOutcome, d time.Duration) {
	if r.meter != nil {
		r.meter.SpoolWaited(outcome, d)
	}
}

// give releases n bytes and stops counting them, in one step.
func (r *spoolRoom) give(n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sem.Release(n)
	r.countLocked(-n)
}

// heldBytes is what the room's holds hold now.
func (r *spoolRoom) heldBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

// spoolHold is room one spool holds in its process's room, from before its
// first byte until its file is removed.
type spoolHold struct {
	room *spoolRoom
	mu   sync.Mutex
	n    int64
}

// trySpoolRoom takes n bytes of the room without waiting, nil where they are
// not free; never ahead of a take waiting for room ([semaphore.Weighted.TryAcquire]).
func (c *Coordinator) trySpoolRoom(n int64) *spoolHold {
	r := c.spoolRoom
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.sem.TryAcquire(n) {
		return nil
	}
	r.countLocked(n)
	return &spoolHold{room: r, n: n}
}

// spoolRoomRefusal is a take's wait for room that ended without it: the
// minute ran out, or the take's own time ran out first (ended, its cause).
type spoolRoomRefusal struct {
	waited time.Duration
	asked  int64
	// others is what other spools held of the room as the wait ended.
	others int64
	ended  error
}

// reason is the refusal as the take's result and its line say it.
func (r *spoolRoomRefusal) reason() string {
	if r.ended != nil {
		return fmt.Sprintf("the take's time ended while it waited for room to spool the session: %v", r.ended)
	}
	return fmt.Sprintf("this node's session spools held their whole room (%d bytes) for %s, so the session "+
		"was not taken: the next launch continues the turn's newest kept capture", spoolRoomBytes, spoolRoomWait)
}

// logAttrs are the fields its `executor_session_not_kept` line carries.
func (r *spoolRoomRefusal) logAttrs() []any {
	return []any{"waited_seconds", r.waited.Seconds(), "bytes", r.asked, "room_bytes", spoolRoomBytes,
		"others_held_bytes", r.others}
}

// awaitSpoolRoom takes n bytes of the room, waiting at most [spoolRoomWait]
// within ctx, and tells the meter how the wait ended ([SpoolMeter]).
func (c *Coordinator) awaitSpoolRoom(ctx context.Context, n int64) (*spoolHold, *spoolRoomRefusal) {
	r := c.spoolRoom
	began := time.Now()
	wait, cancel := context.WithTimeout(ctx, spoolRoomWait)
	defer cancel()
	err := r.sem.Acquire(wait, n)
	waited := time.Since(began)
	if err != nil {
		refused := &spoolRoomRefusal{waited: waited, asked: n, others: r.heldBytes(), ended: ctx.Err()}
		outcome := SpoolRefused
		if refused.ended != nil {
			outcome = SpoolEnded
		}
		r.waited(outcome, waited)
		return nil, refused
	}
	r.mu.Lock()
	r.countLocked(n)
	r.mu.Unlock()
	r.waited(SpoolTaken, waited)
	return &spoolHold{room: r, n: n}, nil
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
		h.room.give(h.n - n)
		h.n = n
	}
}

// split moves n bytes of what h holds into a hold of their own, which it
// answers, h keeping the rest: a take's sealed spool and its scrub's, each
// given back on its own ([takeRoomBytes]).
func (h *spoolHold) split(n int64) *spoolHold {
	h.mu.Lock()
	defer h.mu.Unlock()
	n = min(n, h.n)
	h.n -= n
	return &spoolHold{room: h.room, n: n}
}

// spooling is the scrub's spool a take names in dir, held by h, which it
// shrinks and gives back as the scrub tells it what the spool holds.
func (h *spoolHold) spooling(dir string) SessionSpool {
	return SessionSpool{Dir: dir, Bytes: h.bytes(), Held: h.shrinkTo}
}

// release gives back everything h holds. Safe on nil and twice.
func (h *spoolHold) release() {
	if h != nil {
		h.shrinkTo(0)
	}
}

// heldWriter writes to w at most what left counts, the room its spool holds,
// shared by every file a take spools (its session and its media), and
// refuses whole the write that would pass it, so no byte past the room
// reaches the disk.
type heldWriter struct {
	w    io.Writer
	left *int64
}

func (h *heldWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > *h.left {
		return 0, errSpoolPastHold
	}
	n, err := h.w.Write(p)
	*h.left -= int64(n)
	return n, err
}
