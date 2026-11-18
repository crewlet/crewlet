package statelog

import (
	"errors"
	"fmt"
	"slices"
)

// reorderBuffer holds records that arrived out of order, so a strict loop can
// apply a contiguous prefix.
//
// A hole in a strict log is a redelivery (a lost ack brings a record back out of
// order), and the buffer carries the loop across it. Overflow is a fault, not a
// gap to step over: applying past a hole produces state no other node holds and
// nothing later can detect, so a hole that will not close means the stream is
// not the one this loop checkpointed against. A COMPACTED domain removes
// interior sequences by design and bypasses the buffer.
type reorderBuffer struct {
	held  []Record
	bytes int
}

// admit takes a fetched batch and returns the records that may be applied now,
// holding back anything above a hole. run is what the caller already holds for
// this transaction, so contiguity runs from the checkpoint through it.
//
// The next sequence is one past the HIGHEST position in run, never its length:
// a run legitimately carries redeliveries at or below the checkpoint (passed
// through to be acknowledged) and second copies, and counting them admits a
// record past a hole (checkpoint 10, run [5, 11, 12]: the count says 14 is next
// when 13 is). TestTheReorderBufferNeverAdmitsPastAHoleAfterARedelivery.
func (b *reorderBuffer) admit(batch []Record, cursor Position, protocol ReplayProtocol, run []Record) ([]Record, error) {
	if protocol != ReplayStrict {
		// NO CONTIGUITY TEST AND NO BUFFER: a compacted stream drops an interior
		// sequence on an ordinary write, so treating it as a hole would stall for ever.
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

	next := highest(run, cursor).Seq + 1
	var ready []Record
	for _, rec := range b.held {
		switch {
		case rec.Position.Seq < next:
			// Already committed or in the run: still passed on, since a
			// redelivery nothing acknowledges is redelivered for ever.
			ready = append(ready, rec)
		case rec.Position.Seq == next:
			ready = append(ready, rec)
			next++
		default:
			// A HOLE: everything above it waits. Stopping here keeps ready a
			// PREFIX of held, so the remainder is carried by slicing.
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

// payloadBytes measures the buffer in FRAMED bytes, the memory a hole costs. The
// body is a subslice of the frame, so adding both would double the size and stop
// an applier on a hole it had room for.
func payloadBytes(held []Record) int {
	n := 0
	for _, rec := range held {
		n += len(rec.framed)
	}
	return n
}

// dedupe keeps ONE record per position and CHAINS the acks of the copies it
// drops, and their releases for [Runner.Run]'s unwind. Both copies applied write
// a record over its own rows, above the checkpoint where the loop's skip cannot
// catch it; but each copy is a separate delivery, and one never acknowledged is
// redelivered for ever.
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
		last.release = chain(last.release, rec.release)
	}
	return out
}

// chain acknowledges both deliveries, always calling both and joining their
// errors: one broker error must not leave the other delivery open.
func chain(first, second func() error) func() error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	}
	return func() error { return errors.Join(first(), second()) }
}

// topRecord is the RECORD at the highest position in a non-empty run. Not the
// last element: a run is appended in fetch order and [reorderBuffer.admit] passes
// a redelivery below the high-water mark through to be acknowledged, so the tail
// may be stale; anything derived from where a run got to reads the maximum.
func topRecord(run []Record) Record {
	top := run[0]
	for _, rec := range run[1:] {
		if rec.Position.Packed() > top.Position.Packed() {
			top = rec
		}
	}
	return top
}

// highest is [topRecord]'s position, floored at cursor, so it is safe on a run
// that is entirely redeliveries of records already committed.
func highest(run []Record, cursor Position) Position {
	top := cursor
	for _, rec := range run {
		if rec.Position.Packed() > top.Packed() {
			top = rec.Position
		}
	}
	return top
}
