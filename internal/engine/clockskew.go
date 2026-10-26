package engine

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// How far this node's clock stands from its broker's, which `clock_skew` is
// raised from.
//
// # Why it is measured at all
//
// Two deletions judge an instant one machine stamped by another machine's
// clock: the object collector deletes an object once its key, minted by the
// node that uploaded it, is a day old by the collector's clock, while a write
// may still name a key twelve hours old by the deciding node's; and the code
// host's code-retention duty sweeps a retired pack's row once the instant the
// broker stamped on its retirement is a day old by the duty holder's clock.
// Both were sized against objstore.AssumedClockSkew and were safe only while
// that held, which nothing in the engine measured: a node whose NTP had died,
// or a virtual machine restored from a snapshot hours off, deleted objects
// early with nothing to say so.
//
// # Why off the heartbeat's own row, and nothing else
//
// The position heartbeat already writes this node's row to the coordination
// store every [statelog.PositionHeartbeat], stamped with this node's clock
// ([coord.NodePositions.At]), and already reads the whole register back on the
// next beat — and the broker stamps every entry it stores with its stream
// leader's clock, which the read hands back ([coord.NodePositions.StoredAt]).
// The two instants on this node's own row are one write seen by both clocks,
// so the measure costs no read and no write of its own. A dedicated probe — a
// write to time, a request for the server's clock — would be a round trip a
// beat for a fact the beat already holds.
//
// IT IS OFF by the heartbeat's own work between the stamp and the write — the
// register's listing and this node's per-log reads, seconds — always in the
// direction of the broker reading later, which is seconds beside the hour the
// alarm fires at. It measures against ONE clock, the broker member leading the
// position register: a fleet of nodes each within the bound of it holds any
// two machines within twice it, which is the reading every rule bounded by
// objstore.AssumedClockSkew takes.
//
// # Why a measure stands until two beats in a row could not renew it
//
// A beat whose read of the register failed, or which found no row of this
// node's yet, measures nothing and keeps the last measure: one blink of the
// coordination store would otherwise clear a standing alarm and raise it
// again a beat later. The second beat in a row that cannot measure drops it
// ([skewMissesDropped]), and so does the heartbeat stopping: past either the
// measure is a heartbeat that cannot read rather than a reading, and nothing
// is reported — unmeasured reads as nothing to report, the reading's own rule
// ([statelog.Reading]) — while a coordination store this node cannot reach is
// named by `floor_unknown`.
//
// COUNTED IN BEATS, BY THE HEARTBEAT, and never as an age the reading judges.
// A beat's measure lands when its listing of the register returns, which is
// the listing's own time after the instant the beat began — seconds on a
// loaded broker — and the next beat begins an interval after this one began
// or as soon as this one ends, whichever is later. An age counted from a
// beat's start therefore lapsed between one beat's measure and the next
// beat's whenever a beat's work and the next listing outlasted it, and after
// a single failed beat whenever the next listing did; each lapse cleared a
// standing alarm at the next evaluation and raised it again at the one after,
// resetting the instant it had stood since and dropping its gauge to zero for
// a sample. Counted in beats, a heartbeat that is still beating always holds
// the measure its last reading took, however long a beat runs.

// skewMeasure is one measure of how far this node's clock stood from its
// broker's.
type skewMeasure struct {
	// off is how far apart the two instants on this node's own row stood —
	// its stamp and the broker's instant — as a magnitude: the alarm fires on
	// a clock ahead and a clock behind alike.
	off time.Duration
}

// skewMissesDropped is how many beats in a row may fail to measure before the
// last measure is dropped: the first is kept, as one blink of the coordination
// store, and the second drops it, since two blinks in a row are a heartbeat
// that cannot read the register.
const skewMissesDropped = 2

// measureSkew is how far this node's clock stands from its broker's, off this
// node's own row in rows — the register the beat has just read, holding the
// row the previous beat wrote — or nil where it measures nothing: a register
// with no row of this node's, or a row read back with no instant on it.
func (s *stateLog) measureSkew(rows []coord.NodePositions) *skewMeasure {
	for _, row := range rows {
		if row.NodeID != s.nodeID {
			continue
		}
		if row.At.IsZero() || row.StoredAt.IsZero() {
			return nil
		}
		off := row.StoredAt.Sub(row.At)
		if off < 0 {
			off = -off
		}
		return &skewMeasure{off: off}
	}
	return nil
}

// recordSkew keeps what one publish of this node's row measured as
// [stateLog.skew]: a measure replaces the last one and resets the misses, and
// a publish that measured nothing — measured nil — is a miss, the
// [skewMissesDropped]th of which in a row drops the last measure.
//
// CALLED ONCE PER PUBLISH, with [stateLog.publishing] held, which is what
// guards the miss count: a reanchor publishes through the same path, and its
// reading of the register is a beat's as much as the heartbeat's own.
func (s *stateLog) recordSkew(measured *skewMeasure) {
	if measured != nil {
		s.skewMisses = 0
		s.skew.Store(measured)
		return
	}
	s.skewMisses++
	if s.skewMisses >= skewMissesDropped {
		s.skew.Store(nil)
	}
}

// forgetSkew drops the measure as the heartbeat stops: a heartbeat that has
// stopped measures nothing, and its last measure is not a reading of a clock
// that nothing reads any more.
//
// WITHOUT THE PUBLISHING LOCK, and the misses left as they are: the loop that
// calls it has no publish of its own in flight, and a stopping heartbeat that
// waited on the lock would wait out a reanchor's publish, which owns the misses
// it counts.
func (s *stateLog) forgetSkew() {
	s.skew.Store(nil)
}

// skewReading fills the reading's clock half: the heartbeat's last measure and
// the bound the deletions rest on, objstore.AssumedClockSkew — or neither,
// where the heartbeat holds no measure ([stateLog.recordSkew]).
//
// NO AGE IS JUDGED HERE: what the heartbeat holds is its last reading until
// the heartbeat itself drops it, so a measure never lapses while a beat is
// still running (see the file's doc).
//
// ASSIGNED HERE, in a method of the concrete runtime the reading's assembly
// calls, so engine.TestEveryAlarmReadingFieldHasAProducer follows the call to
// both fields; handed through a helper's pointer they would read as nothing
// filled them.
func (s *stateLog) skewReading(out *statelog.Reading) {
	if s == nil {
		return
	}
	measured := s.skew.Load()
	if measured == nil {
		return
	}
	out.ClockSkew, out.ClockSkewBound = measured.off, objstore.AssumedClockSkew
}
