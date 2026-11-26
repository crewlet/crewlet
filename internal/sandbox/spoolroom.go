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

// takeRoomBytes is the room one take holds: its scrub's spool ([SessionSpool])
// and its sealed spools, the session's text and each payload it keeps apart,
// in ONE ROOM, which the scrub's second reading drains into the seal: each
// file of the scrub's spool it has read is given back to the take as the seal
// writes ([executorkeep.SpoolSegmentBytes]). A session at its kept bound
// ([executorkeep.SessionKeptMaxBytes]) peaks at its scrub's spool, one file
// and its seals' and gzip's overhead ([drainPeakBytes]), which this holds.
// Twice a sealed session at the text bound ([executorkeep.SealedMaxBytes]) as
// gzip writes input it cannot shrink ([executorkeep.GzipMostBytes]) with the
// scrub spool's members ([executorkeep.SpoolMembersMostBytes]): 96.23 MiB
// today, and the kept bound is the most it holds drained (a test holds both).
var takeRoomBytes = 2 * (executorkeep.GzipMostBytes(executorkeep.SealedMaxBytes) +
	executorkeep.SpoolMembersMostBytes)

// drainPeakBytes is the most a take at the kept bound holds of its room at
// once ([drainPeakFor]): 95.18 MiB.
var drainPeakBytes = drainPeakFor(executorkeep.SessionKeptMaxBytes)

// drainPeakFor is the most a take keeping bound bytes holds of its room at
// once: its scrub's spool at its most ([scrubSpoolMaxBytes] at that bound),
// and the sealed spools, which lag what the second reading consumed (the
// take's body is a pipe) while the files it has read trail it by under one
// ([executorkeep.SpoolSegmentBytes]); so the spool, one file, and what the
// seals and their gzip add past the bound (the bound sealed,
// executorkeep.KeptSealedFor, as gzip writes it, less the bound).
func drainPeakFor(bound int64) int64 {
	return executorkeep.GzipMostBytes(bound) + executorkeep.SpoolMembersMostBytes +
		executorkeep.SpoolSegmentBytes + executorkeep.GzipMostBytes(executorkeep.KeptSealedFor(bound)) - bound
}

// scrubSpoolMaxBytes is the most a take's scrub spools: the kept bound as
// gzip writes input it cannot shrink, with every member it may open. A spool
// past it would leave the seal no room before the first file is read, so it
// is refused as it writes.
var scrubSpoolMaxBytes = executorkeep.GzipMostBytes(executorkeep.SessionKeptMaxBytes) +
	executorkeep.SpoolMembersMostBytes

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

// spoolHold is the room one take holds in its process's room, from before its
// first byte until its last file is removed, and what its spools hold of it:
// its scrub's ([spoolHold.scrubbing]) and its sealed ones'
// ([spoolHold.sealing]), each byte taken before it is written, refused past
// the hold ([errSpoolPastHold]), and given back to the HOLD as it leaves the
// disk, never to the room, so the seal writes into what the scrub's second
// reading drained (the package doc's "Every spool on a node in one room").
type spoolHold struct {
	room *spoolRoom
	mu   sync.Mutex
	n    int64
	// used is what the spools hold now, peak the most they have held, and
	// scrub and sealed each's part of used.
	used, peak, scrub, sealed int64
	// spooled is what the scrub's spool held as its first reading ended, -1
	// until it has; counted is the sealed spools' bound, the archive's
	// counted size ([countedFor]), 0 until the archive is known.
	spooled, counted int64
}

// newSpoolHold is a hold of n bytes of r, nothing used.
func newSpoolHold(r *spoolRoom, n int64) *spoolHold {
	return &spoolHold{room: r, n: n, spooled: -1}
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
	return newSpoolHold(r, n)
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
	return newSpoolHold(r, n), nil
}

// shrinkTo gives back all but n bytes of what h holds, never what its spools
// use; nothing where it holds n or less.
func (h *spoolHold) shrinkTo(n int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shrinkLocked(n)
}

func (h *spoolHold) shrinkLocked(n int64) {
	if n = max(n, h.used); n < h.n {
		h.room.give(h.n - n)
		h.n = n
	}
}

// takeLocked takes n bytes for a spool, refused past what h holds.
func (h *spoolHold) takeLocked(n int64) error {
	if h.used+n > h.n {
		return fmt.Errorf("%w (%d bytes held, %d of them written, %d more asked)", errSpoolPastHold, h.n,
			h.used, n)
	}
	h.used += n
	h.peak = max(h.peak, h.used)
	return nil
}

// counting is the archive's counted size known: the sealed spools are held to
// it, and the hold shrinks ([spoolHold.settleLocked]).
func (h *spoolHold) counting(counted int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counted = counted
	h.settleLocked()
}

// settleLocked shrinks h, once both the scrub's spool and the archive's
// counted size are known, to what the two hold together at most: the sealed
// spools are held to the counted size, so the take never needs more, drained
// or not. A small session gives its room back before it streams; one at the
// bound gives back nothing, and the drain carries it.
func (h *spoolHold) settleLocked() {
	if h.spooled >= 0 && h.counted > 0 {
		h.shrinkLocked(min(h.n, h.spooled+h.counted))
	}
}

// scrubbing is the scrub's spool a take names in dir, taking from h, held to
// [scrubSpoolMaxBytes] of it, and giving back to h as its second reading
// drains it.
func (h *spoolHold) scrubbing(dir string) SessionSpool {
	return SessionSpool{Dir: dir,
		Take: func(n int64) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.scrub+n > scrubSpoolMaxBytes {
				return fmt.Errorf("%w: the scrub's spool would pass its %d bytes", errSpoolPastHold,
					scrubSpoolMaxBytes)
			}
			if err := h.takeLocked(n); err != nil {
				return err
			}
			h.scrub += n
			return nil
		},
		Give: func(n int64) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.scrub -= n
			h.used -= n
		},
		Spooled: func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.spooled = h.scrub
			h.settleLocked()
		},
	}
}

// sealing is w taking every byte from h before it is written, the sealed
// spools held together to the archive's counted size once it is known, a
// write refused whole past either, so no byte past the room reaches the disk.
func (h *spoolHold) sealing(w io.Writer) io.Writer { return &heldWriter{w: w, hold: h} }

// heldWriter is a sealed spool's writer ([spoolHold.sealing]).
type heldWriter struct {
	w    io.Writer
	hold *spoolHold
}

func (w *heldWriter) Write(p []byte) (int, error) {
	h, n := w.hold, int64(len(p))
	h.mu.Lock()
	err := errSpoolPastHold
	if h.counted <= 0 || h.sealed+n <= h.counted {
		err = h.takeLocked(n)
	}
	if err == nil {
		h.sealed += n
	}
	h.mu.Unlock()
	if err != nil {
		return 0, err
	}
	written, err := w.w.Write(p)
	if short := n - int64(written); short > 0 {
		h.mu.Lock()
		h.sealed, h.used = h.sealed-short, h.used-short
		h.mu.Unlock()
	}
	return written, err
}

// release gives back everything h holds. Safe on nil and twice.
func (h *spoolHold) release() {
	if h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.used = 0
		h.shrinkLocked(0)
	}
}
