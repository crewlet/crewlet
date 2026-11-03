package engine

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// How far this node's clock, and the clock of the broker member leading the
// position register, stand from the fleet's — which `clock_skew` is raised
// from.
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
// # Why off the register the heartbeat already reads, and nothing else
//
// The position heartbeat already writes this node's row to the coordination
// store every [statelog.PositionHeartbeat], stamped with this node's clock
// ([coord.NodePositions.At]), and already reads the whole register back on the
// next beat — every data node's row, each stamped by its own node's clock —
// and the broker stamps every entry it stores with its stream leader's clock,
// which the read hands back ([coord.NodePositions.StoredAt]). So every row is
// one write seen by two clocks, its node's and the member's leading the
// register, and the measure costs no read and no write of its own. A
// dedicated probe — a write to time, a request for the server's clock — would
// be a round trip a beat for a fact the beat already holds.
//
// # Why against the fleet's clock, and never the member's alone
//
// A row's two instants say how far apart its node's clock and the member's
// stand, and nothing about which of the two moved. Measured against the
// member alone, the alarm blamed the wrong machine whenever the clock that
// moved WAS the member's: on an embedded broker that member is a data node,
// whose own row one clock stamps and stores, so it measured nothing, while
// every other node read the member's drift as its own — raising, about its
// own clock, an alarm whose remedy changed nothing, while the node whose
// clock had moved, perhaps the one holding the collector or the retired-pack
// sweep and deleting by that clock, said nothing at all.
//
// So every row is read, and their offsets compared. A row's OFFSET — the
// store's instant less its node's stamp — is how far the member's clock stands
// from that node's, every one against the same member, so two rows' offsets
// differ by how far their nodes' clocks stand apart, and the MEDIAN of the
// offsets is how far the member's clock stands from the fleet's: the median of
// the data nodes' clocks. Two clocks are measured against it:
//
//   - THIS NODE'S: its own offset against the median, which `clock_skew` is
//     raised about on this node alone, since it is the machine to fix.
//   - THE MEMBER'S: the median itself, which every node reads alike. On an
//     embedded broker the member is a data node whose own row reads in time
//     with the member — it is the member — so that node also raises the alarm
//     about its own clock; on an external NATS cluster the member is one of its
//     servers, which no node is, and only the nodes can say so.
//
// While no node raises either, every data node and that member stand within
// objstore.AssumedClockSkew of the fleet's clock, so any two of them within
// twice it: the reading every rule bounded by objstore.AssumedClockSkew takes.
//
// A MEDIAN, because one clock that moved must not move the clock it is judged
// against: with three data nodes or more the clock that moved is the one off
// the median, and it is the one named. With two the median is their midpoint,
// which neither clock outvotes, so both are named once they stand more than
// twice the bound apart and either may be the one that moved; with one the
// fleet's clock is that node's own, and only the member can stand from it.
//
// ONLY ROWS THE BROKER STORED WITHIN [skewRowFresh] OF THIS NODE'S OWN are
// read, judged by the store's instants alone — every one the member's clock —
// so the writer's own clock, the one being measured, never decides whose row
// counts. A stopped node's last row stays in the register, and an evicted
// node's for ever, and neither is the fleet's present. The window also keeps a
// leader change from mixing two members' clocks: rows two members stored are
// read together only where those members' clocks agree within it, and then
// move the median by less than it, seconds beside the hour.
//
// IT IS OFF by each beat's own work between its stamp and its write — the
// register's listing and the node's per-log reads, seconds — always in the
// direction of the member reading later, which is seconds beside the hour the
// alarm fires at.
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

// skewMeasure is one reading of the fleet's clocks off the register.
type skewMeasure struct {
	// node is how far this node's clock stands from the fleet's, and broker
	// how far the clock of the broker member leading the position register
	// stands from it, each signed: positive is ahead. The alarm fires on a
	// clock ahead and a clock behind alike, and says which.
	node, broker time.Duration

	// nodes is how many data nodes' clocks the fleet's clock is the median
	// of, this node's among them.
	nodes int
}

// skewRowFresh is how far from this node's own row, by the broker's instants,
// another node's row may have been stored and still count among the fleet's
// clocks: FOUR HEARTBEATS, the engine's one horizon for a row a running node
// wrote ([donorRowFresh]'s derivation) — a node may miss three beats and still
// be running, so a row stored further off is a node that has stopped, or a row
// another member stored whose clock disagrees with the one that stored this
// node's.
//
// Equal to donorRowFresh by that one reason, not by reference: the join
// judges a donor's row by the writer's stamp against its own clock, and this
// by two of the store's instants, since here the writer's clock is the one
// being measured.
const skewRowFresh = 4 * statelog.PositionHeartbeat

// skewMissesDropped is how many beats in a row may fail to measure before the
// last measure is dropped: the first is kept, as one blink of the coordination
// store, and the second drops it, since two blinks in a row are a heartbeat
// that cannot read the register.
const skewMissesDropped = 2

// measureSkew is how far this node's clock, and the clock of the broker member
// leading the position register, stand from the fleet's, off rows — the
// register the beat has just read, holding the row the previous beat wrote —
// or nil where it measures nothing: a register with no row of this node's, or
// this node's row read back with no instant on it.
//
// A row of another node read back with no instant on it is left out, as is
// one the broker stored further than [skewRowFresh] from this node's own.
func (s *stateLog) measureSkew(rows []coord.NodePositions) *skewMeasure {
	own := slices.IndexFunc(rows, func(row coord.NodePositions) bool {
		return row.NodeID == s.nodeID
	})
	if own < 0 || !bothInstants(rows[own]) {
		return nil
	}
	stored := rows[own].StoredAt
	offsets := make([]time.Duration, 0, len(rows))
	for _, row := range rows {
		if !bothInstants(row) {
			continue
		}
		if apart := row.StoredAt.Sub(stored); apart > skewRowFresh || apart < -skewRowFresh {
			continue
		}
		offsets = append(offsets, offsetOf(row))
	}
	fleet := medianOf(offsets)
	return &skewMeasure{
		node:   fleet - offsetOf(rows[own]),
		broker: fleet,
		nodes:  len(offsets),
	}
}

// bothInstants reports whether a row read back carries both instants a measure
// compares: its node's stamp, and the instant the store took it.
func bothInstants(row coord.NodePositions) bool {
	return !row.At.IsZero() && !row.StoredAt.IsZero()
}

// offsetOf is how far the broker member that stored a row stands from the
// clock of the node that stamped it, positive where the member's runs ahead.
func offsetOf(row coord.NodePositions) time.Duration {
	return row.StoredAt.Sub(row.At)
}

// medianOf is the median of offsets, the midpoint of the middle two where they
// are even in number. offsets is never empty here: it holds this node's own.
func medianOf(offsets []time.Duration) time.Duration {
	sorted := slices.Clone(offsets)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return sorted[mid-1] + (sorted[mid]-sorted[mid-1])/2
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

// skewReading fills the reading's clock half: the heartbeat's last measure —
// this node's clock and the broker member's against the fleet's, and how many
// data nodes' clocks the fleet's is the median of — and the bound the
// deletions rest on, objstore.AssumedClockSkew; or none of them, where the
// heartbeat holds no measure ([stateLog.recordSkew]).
//
// NO AGE IS JUDGED HERE: what the heartbeat holds is its last reading until
// the heartbeat itself drops it, so a measure never lapses while a beat is
// still running (see the file's doc).
//
// ASSIGNED HERE, in a method of the concrete runtime the reading's assembly
// calls, so engine.TestEveryAlarmReadingFieldHasAProducer follows the call to
// every field; handed through a helper's pointer they would read as nothing
// filled them.
func (s *stateLog) skewReading(out *statelog.Reading) {
	if s == nil {
		return
	}
	measured := s.skew.Load()
	if measured == nil {
		return
	}
	out.ClockSkew, out.BrokerClockSkew = measured.node, measured.broker
	out.ClockSkewNodes, out.ClockSkewBound = measured.nodes, objstore.AssumedClockSkew
}
