package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE EVICTION GATE ON THIS LOG, both halves: the record that installs or
// lifts it, and the rows the trim reads ([Domain.Evictions]) to stop counting
// an evicted node.
//
// A gate is a rule about records on ONE log, keyed on positions in that log's
// own sequence, so the code log carries its own: an eviction written to the
// tracker's log says nothing about which code records to drop. The engine's
// node gate writes every identity-claiming log's from one judgement, and
// reports each log's outcome on its own.

// EvictNode installs the gate that drops a node's records on this log, at this
// log's own position — every node reaches the same verdict about every record
// from the order they all already have, with no clock and no coordination
// read.
//
// UNJUDGED HERE: whether the node may be evicted is a fleet question asked
// once, before the first log is written, by the engine's node gate.
func (s *Store) EvictNode(ctx context.Context, actor Actor, opID, nodeID string) (statelog.Result, error) {
	return s.gateNode(ctx, actor, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay.
func (s *Store) ReadmitNode(ctx context.Context, actor Actor, opID, nodeID string) (statelog.Result, error) {
	return s.gateNode(ctx, actor, opID, nodeID, true)
}

// gateNode publishes one eviction record, arbitrated on the node's own subject
// so an eviction and the readmission that inverts it contend with each other
// and with nothing else. A retry is answered by the framework from this
// node's ledger; what this adds is whether the record its operation landed is
// still in force ([statelog.GateStanding]).
func (s *Store) gateNode(ctx context.Context, actor Actor, opID, nodeID string,
	readmit bool) (statelog.Result, error) {

	switch {
	case nodeID == "":
		return statelog.Result{}, errors.New("code: an eviction names no node")
	case opID == "":
		return statelog.Result{}, fmt.Errorf("code: the eviction of %s has no operation "+
			"id — a retry under a fresh one would write the gate twice", nodeID)
	}
	if err := actor.validate(); err != nil {
		return statelog.Result{}, err
	}
	subject := EvictionSubject(nodeID)
	scope := []string{DomainPath()}
	at := s.now()
	return s.publisher.Publish(ctx, statelog.Request{
		Subject:  subject.statelog(),
		Scope:    statelog.ScopeSet{Paths: scope},
		OpID:     opID,
		Pattern:  statelog.PatternArbitrated,
		NodeGate: true,
		Standing: func(tx *sql.Tx, held statelog.Position) error {
			row, found, err := standingIn(ctx, tx, nodeID)
			if err != nil {
				return err
			}
			return statelog.GateStanding(opID, nodeID, readmit, held, row, found)
		},
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return recordFor(stamp, subject, OpEviction, scope, opID, actor, Eviction{
				V: GateRecordVersion, NodeID: nodeID, EvictedBy: actor.ID,
				EvictedAt: at, Readmitted: readmit,
			}, at)
		},
	})
}

// standingIn is nodeID's eviction row on this log, read in the caller's
// transaction.
func standingIn(ctx context.Context, tx *sql.Tx, nodeID string) (statelog.EvictionRow, bool, error) {
	var from sql.NullInt64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, evictionSQL, nodeID).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("code: read %s's eviction row: %w", nodeID, err)
	}
	return statelog.EvictionRow{
		NodeID: nodeID, From: uint64(from.Int64), Readmitted: uint64(readmitted.Int64),
		// THE COMPARISON [Fence.Evicted] MAKES, so a retry and the node's
		// own fence never disagree about whether it is back.
		Back: readmitted.Valid && readmitted.Int64 > from.Int64,
	}, true, nil
}
