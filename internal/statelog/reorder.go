package statelog

import (
	"errors"
	"fmt"
	"slices"
)

// reorderBuffer holds records that arrived out of order, so a strict loop can
// apply a contiguous prefix.
//
// # Why a strict log needs one at all
//
// A log is contiguous by construction, so a hole is never the stream's own
// doing — it is a REDELIVERY: a record whose acknowledgement was lost comes
// back after the broker's redelivery window, out of order with what has
// arrived since. The buffer is what carries the loop across that window
// instead of applying past the hole.
//
// # And why overflow is a fault rather than a gap to step over
//
// Applying past a hole in a strict log produces state no other node holds, and
// nothing later can tell that it did: the checkpoint moves, the identity claim
// compares two nodes that both think they are current, and the missing
// record's rows are simply absent everywhere the hole was. A bounded buffer
// that overflows is saying the hole is not closing — which means the stream is
// not the stream this loop thinks it is, and stopping is the only honest move.
//
// A COMPACTED domain has no such invariant: its stream removes interior
// sequences by design, so a hole there is ordinary and the buffer is bypassed
// entirely.
type reorderBuffer struct {
	held  []Record
	bytes int
}

// admit takes a fetched batch and returns the records that may be applied now,
// holding back anything above a hole.
//
// run is what the caller already holds for this transaction, which is what
// makes the contiguity test start from the right place: a run filled across
// two fetches is contiguous from the checkpoint through everything already in
// it.
//
// THE RUN ITSELF, NOT ITS LENGTH. The next sequence is one past the HIGHEST
// position the run carries, and a run legitimately carries records that are
// not part of its contiguous prefix — a redelivery at or below the checkpoint
// that is passed through so it can be acknowledged, or a second copy of a
// record already in the run. Deriving the next sequence from the count
// overshoots by one for each of those, and an overshoot admits a record PAST
// A HOLE: with the checkpoint at 10 and a run of [5, 11, 12], the count says
// 14 is next when 13 is, so 14 and 15 are applied with 13 missing. That is the
// one failure a replicated log cannot notice afterwards, and it was reachable
// from an ordinary lost acknowledgement.
func (b *reorderBuffer) admit(batch []Record, cursor Position, protocol ReplayProtocol, run []Record) ([]Record, error) {
	if protocol != ReplayStrict {
		// NO CONTIGUITY TEST AND NO BUFFER. The stream keeps one
		// message per subject, so an ordinary write removes an interior
		// sequence — a loop that treated that as a hole would stall on
		// the first one for ever.
		return batch, nil
	}

	b.held = append(b.held, batch...)
	slices.SortFunc(b.held, func(a, c Record) int {
		switch {
		case a.Position.Packed() < c.Position.Packed():
			return -1
		case a.Position.Packed() > c.Position.Packed():
			return 1
		}
		return 0
	})
	b.held = dedupe(b.held)

	// The next sequence this loop may apply is one past whatever it
	// already holds — the checkpoint, or the highest record in the run.
	next := highest(run, cursor).Seq + 1
	var ready []Record
	for _, rec := range b.held {
		switch {
		case rec.Position.Seq < next:
			// Already committed or already in the run. It still has
			// to reach the caller, which acknowledges it: a
			// redelivery nothing acknowledges is redelivered for
			// ever.
			ready = append(ready, rec)
		case rec.Position.Seq == next:
			ready = append(ready, rec)
			next++
		default:
			// A HOLE. Everything above it waits, and the loop
			// stops here so what it returns is a PREFIX of what it
			// holds — which is what lets the remainder be carried
			// by slicing rather than by rebuilding.
			b.held = b.held[len(ready):]
			b.bytes = payloadBytes(b.held)
			return b.checked(ready, next)
		}
	}
	b.held = b.held[len(ready):]
	b.bytes = payloadBytes(b.held)
	return b.checked(ready, next)
}

// checked returns the ready prefix unless the buffer has grown past its bound.
func (b *reorderBuffer) checked(ready []Record, next uint64) ([]Record, error) {
	if b.bytes > ReorderBufferBytes {
		return nil, fmt.Errorf("%w: %d bytes of records above sequence %d are "+
			"waiting for a record that has not arrived, past the %d-byte reorder "+
			"bound — a hole this wide in an ordered log is a stream that is not "+
			"the one this applier checkpointed against, and applying past it "+
			"would produce state no other node holds",
			ErrStopped, b.bytes, next, ReorderBufferBytes)
	}
	return ready, nil
}

// payloadBytes is what the buffer is measured in: the records themselves,
// which is the memory a hole actually costs.
//
// THE FRAMED BYTES, not the body, and not both. The body is a SUBSLICE of the
// frame rather than a copy of it, so one allocation is behind the two fields
// and adding them would report a buffer at twice its real size — which would
// stop an applier on a hole it had the room for.
func payloadBytes(held []Record) int {
	n := 0
	for _, rec := range held {
		n += len(rec.framed)
	}
	return n
}

// dedupe keeps ONE record per position and CHAINS the acknowledgements of the
// copies it drops.
//
// Both halves are load-bearing and they pull in opposite directions. Two
// copies of one position applied twice is a record written on top of its own
// rows — the second copy is above the checkpoint the first established, so the
// loop's own skip cannot catch it. But each copy is a separate DELIVERY the
// broker is holding open, and a delivery nothing acknowledges is redelivered
// for ever: dropping the duplicate's ack turns a transient redelivery into a
// permanent one.
func dedupe(held []Record) []Record {
	if len(held) < 2 {
		return held
	}
	out := held[:1]
	for _, rec := range held[1:] {
		last := &out[len(out)-1]
		if last.Position.Packed() != rec.Position.Packed() {
			out = append(out, rec)
			continue
		}
		last.ack = chain(last.ack, rec.ack)
	}
	return out
}

// chain acknowledges both deliveries, reporting the first failure and still
// trying the second: one broker error must not leave the other delivery open.
func chain(first, second func() error) func() error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	}
	// BOTH ARE CALLED, always. One broker error must not leave the other
	// delivery open, which is what returning early on the first would do.
	return func() error { return errors.Join(first(), second()) }
}

// highest is the furthest position a run carries, or the cursor for an empty
// run.
//
// THE MAXIMUM AND NOT THE LAST ELEMENT: a run is appended to in fetch order,
// and a fetch that delivered only a redelivery below the checkpoint leaves a
// record at the end that is lower than everything before it.
// topRecord is the RECORD at the highest position in a non-empty run.
//
// THE LAST ELEMENT IS NOT IT, which is the whole reason this exists. A run
// closes with whatever the final fetch handed over, and [reorderBuffer.admit]
// deliberately passes a record BELOW the run's high-water mark straight
// through so that the caller acknowledges it — a redelivery nothing
// acknowledges is redelivered for ever. So the tail of a run is a stale
// redelivery whenever one arrived late, and everything derived from "where
// this run got to" reads the maximum instead.
func topRecord(run []Record) Record {
	top := run[0]
	for _, rec := range run[1:] {
		if rec.Position.Packed() > top.Position.Packed() {
			top = rec
		}
	}
	return top
}

// highest is [topRecord]'s position, floored at a cursor the run may not have
// reached — which is what makes it safe on a run that is ENTIRELY
// redeliveries of records already committed.
func highest(run []Record, cursor Position) Position {
	top := cursor
	for _, rec := range run {
		if rec.Position.Packed() > top.Packed() {
			top = rec.Position
		}
	}
	return top
}
