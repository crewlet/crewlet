package engine

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// How far this node's clock, and the clock of the broker member leading the
// position register, stand from the fleet's, which `clock_skew` is raised
// from. Every rule objstore.AssumedClockSkew's doc lists (the object
// collector's grace, Gitlet's retired-pack sweep and the rest) judges an
// instant one machine stamped by another's clock and is safe only within that
// bound; that doc and deployment.md ("Every machine's clock") hold the list.
//
// Measured off the register the position heartbeat already writes and reads
// back: every row is one write seen by two clocks, its node's stamp
// ([coord.NodePositions.At]) and its stream leader's ([coord.NodePositions.StoredAt]),
// so the measure costs no round trip of its own.

// skewMeasure is one reading of the fleet's clocks off the register. The
// fleet's clock is the MEDIAN of the data nodes' clocks: a row cannot say which
// of its two clocks moved, and measured against the member alone the wrong
// machine was blamed (on an embedded broker the member's own row reads in time
// while every peer blames itself). A median keeps one moved clock from moving
// the clock it is judged against; with two data nodes, both are named once
// over twice the bound apart, neither outvoting the other, and with one only
// the member can stand off. While no node raises the alarm, every data node and the member
// stand within objstore.AssumedClockSkew of the fleet's, any two within twice it.
type skewMeasure struct {
	// node is this node's clock against the fleet's, raised on this node alone
	// as the machine to fix; broker is the member's, the median itself, which
	// every node reads alike (an embedded broker's member, a data node, also
	// raises it about its own clock; an external NATS member only the nodes can
	// name). Signed, positive ahead: the alarm fires either way and says which.
	node, broker time.Duration

	// nodes is how many data nodes' clocks the fleet's clock is the median
	// of, this node's among them.
	nodes int
}

// skewRowFresh is how far from this node's own row, by the broker's instants,
// another node's row may have been stored and still count: FOUR HEARTBEATS,
// the engine's one horizon for a running node's row ([donorRowFresh]'s
// derivation). Judged by the store's instants alone, so the clock being
// measured never decides whose row counts. A stopped node's last row, and an
// evicted node's for ever, stay in the register and are not the fleet's
// present; the window also keeps a leader change from mixing two members'
// clocks except where they agree within it, seconds beside the hour.
//
// Equal to donorRowFresh by that one reason, not by reference: the join judges
// a donor's row by the writer's stamp, and this by the store's instants.
const skewRowFresh = 4 * statelog.PositionHeartbeat

// skewMissesDropped is how many beats in a row may fail to measure before the
// last measure is dropped: the first is kept as one blink of the coordination
// store (dropping it would clear a standing alarm and raise it a beat later);
// the second is a heartbeat that cannot read the register, which reports
// nothing ([statelog.Reading]'s rule) while `floor_unknown` names the store.
//
// Counted in beats by the heartbeat, never as an age the reading judges: a
// beat's measure lands when its listing returns, seconds after the beat began
// on a loaded broker, so an age would lapse between beats and clear and
// re-raise a standing alarm, resetting its since and its gauge. A beating
// heartbeat always holds its last reading, however long a beat runs.
const skewMissesDropped = 2

// measureSkew is how far this node's clock, and the member's, stand from the
// fleet's ([skewMeasure]), off rows (the register the beat has just read,
// holding the row the previous beat wrote), or nil where it measures nothing:
// no row of this node's, or one read back with no instant on it. A row's
// offset is the member's clock less its node's; the median offset is the
// member against the fleet. Another node's row with no instant, or stored
// further than [skewRowFresh] from this node's own, is left out.
//
// Off by each beat's own work between its stamp and its write, seconds,
// always toward the member reading later: nothing beside the hour.
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
// [stateLog.skew]: a measure replaces the last one and resets the misses; a
// nil one is a miss, the [skewMissesDropped]th in a row of which drops it.
//
// Called once per publish with [stateLog.publishing] held, which guards the
// miss count: a reanchor publishes through the same path, and its reading of
// the register counts as a beat's.
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

// forgetSkew drops the measure as the heartbeat stops: nothing reads the
// clock any more. Without the publishing lock, the misses untouched: the
// calling loop has no publish in flight, and waiting on the lock would wait
// out a reanchor's publish, which owns the misses.
func (s *stateLog) forgetSkew() {
	s.skew.Store(nil)
}

// skewReading fills the reading's clock half from the heartbeat's last
// measure, with the bound the deletions rest on (objstore.AssumedClockSkew),
// or nothing where it holds none. No age is judged here: only the heartbeat
// drops a measure ([skewMissesDropped]).
//
// Assigned here, in a method of the concrete runtime, so
// engine.TestEveryAlarmReadingFieldHasAProducer follows the call to every
// field; handed through a helper's pointer they would read as unfilled.
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
