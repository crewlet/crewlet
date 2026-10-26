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
// # Why a measure is kept for two beats and no longer
//
// A beat whose read of the register failed, or which found no row of this
// node's yet, measures nothing and keeps the last measure: one blink of the
// coordination store would otherwise clear a standing alarm and raise it
// again a beat later. Past two beats ([skewKept]) the measure is a beat that
// failed to read rather than a reading, and nothing is reported — unmeasured
// reads as nothing to report, the reading's own rule ([statelog.Reading]) —
// and a coordination store this node cannot reach is named by
// `floor_unknown`.

// skewMeasure is one measure of how far this node's clock stood from its
// broker's.
type skewMeasure struct {
	// off is how far apart the two instants on this node's own row stood —
	// its stamp and the broker's instant — as a magnitude: the alarm fires on
	// a clock ahead and a clock behind alike.
	off time.Duration
	// at is the beat that measured it, by this node's clock.
	at time.Time
}

// skewKept is how long a measure stands with no newer one: two beats of the
// heartbeat that takes it, which is also the interval the alarm table is
// evaluated on ([statelog.AlarmInterval]), so a measure the table reads is from
// one of the last two beats or is none.
const skewKept = 2 * statelog.PositionHeartbeat

// measureSkew records how far this node's clock stands from its broker's, off
// this node's own row in rows — the register the beat stamped at `at` has just
// read, holding the row the previous beat wrote — as [stateLog.skew].
//
// A register with no row of this node's, or a row read back with no instant on
// it, measures nothing and leaves the last measure as it was.
func (s *stateLog) measureSkew(rows []coord.NodePositions, at time.Time) {
	for _, row := range rows {
		if row.NodeID != s.nodeID {
			continue
		}
		if row.At.IsZero() || row.StoredAt.IsZero() {
			return
		}
		off := row.StoredAt.Sub(row.At)
		if off < 0 {
			off = -off
		}
		s.skew.Store(&skewMeasure{off: off, at: at})
		return
	}
}

// skewReading fills the reading's clock half: the heartbeat's last measure and
// the bound the deletions rest on, objstore.AssumedClockSkew — or neither,
// where no beat has measured or the last measure is older than [skewKept] by
// now.
//
// ASSIGNED HERE, in a method of the concrete runtime the reading's assembly
// calls, so engine.TestEveryAlarmReadingFieldHasAProducer follows the call to
// both fields; handed through a helper's pointer they would read as nothing
// filled them.
func (s *stateLog) skewReading(now time.Time, out *statelog.Reading) {
	if s == nil {
		return
	}
	measured := s.skew.Load()
	if measured == nil || now.Sub(measured.at) > skewKept {
		return
	}
	out.ClockSkew, out.ClockSkewBound = measured.off, objstore.AssumedClockSkew
}
