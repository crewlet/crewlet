package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EVICTION GATE ON THIS LOG, both halves: the record that installs or
// lifts it, and the rows the trim reads to stop counting an evicted node.
//
// # Why this log carries its own eviction and not the tracker's
//
// A gate is a rule about records on ONE log, keyed on positions in that log's
// own sequence space — so the tracker's eviction row, at a tracker position,
// says nothing about which pages records to drop, and the trim evaluates each
// log's counted set on its own. For as long as only the tracker's writer could
// write an eviction, this half existed as an applier, a fence and a table that
// nothing in production ever filled: an operator's eviction lifted the tracker
// log's pin and left this one's where it was, and the pages log grew toward its
// ceiling behind a node that was never coming back. The engine's node gate now
// writes both, from one judgement, and reports each log's outcome on its own.

// EvictNode installs the gate that drops a node's records on this log.
//
// THE POSITION IS THE LOG'S OWN, and nothing else is: every node reaches the
// same verdict about every record from the order they all already have, with
// no clock and no coordination read — which is what makes it the fence that
// holds when coordination cannot be reached at all.
//
// UNJUDGED HERE. Whether the node may be evicted — it must not be holding a
// live presence lease — is a fleet question this package holds none of the
// inputs to, and it is asked ONCE, before the first log is written, by the
// engine's node gate: judged per log, two logs could reach two answers about
// one node.
func (s *Store) EvictNode(ctx context.Context, actor Actor, opID, nodeID string) (statelog.Result, error) {
	return s.gateNode(ctx, actor, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
//
// Unjudged here for [Store.EvictNode]'s reason: a node below a trim floor is
// refused a readmission by the engine's node gate, which reads the positions
// register, the published floors and every identity-claiming log at once.
func (s *Store) ReadmitNode(ctx context.Context, actor Actor, opID, nodeID string) (statelog.Result, error) {
	return s.gateNode(ctx, actor, opID, nodeID, true)
}

// gateNode publishes one eviction record, arbitrated on the node's own
// subject so an eviction and the readmission that inverts it contend with each
// other and with nothing else.
//
// A RETRY IS ANSWERED BY THE FRAMEWORK from this node's ledger, before this
// decision runs ([statelog.Snap.Held]); what this write adds is whether the
// record its operation landed is still in force, judged from the node's own
// row in the same snapshot ([statelog.GateStanding]).
func (s *Store) gateNode(ctx context.Context, actor Actor, opID, nodeID string,
	readmit bool) (statelog.Result, error) {

	switch {
	case nodeID == "":
		return statelog.Result{}, fmt.Errorf("pages: an eviction names no node")
	case opID == "":
		return statelog.Result{}, fmt.Errorf("pages: the eviction of %s has no "+
			"operation id — a retry under a fresh one would write the gate twice",
			nodeID)
	}
	if err := actor.validate(); err != nil {
		return statelog.Result{}, err
	}
	subject := EvictionSubject(nodeID)
	scope := ScopeSet{Subject: true}
	at := s.now()
	return s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		Scope:    scope.Resolve(subject),
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
			return s.decide(stamp, actor, subject, OpEviction, scope, opID, Eviction{
				V: GateRecordVersion, NodeID: nodeID,
				EvictedBy: actor.Name(), EvictedAt: at, Readmitted: readmit,
			}, nil, at)
		},
	})
}

// standingIn is nodeID's eviction row on this log, read in the transaction the
// caller holds.
func standingIn(ctx context.Context, tx *sql.Tx, nodeID string) (statelog.EvictionRow, bool, error) {
	var from int64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM pages_evictions
		WHERE node_id = ?`, nodeID).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("pages: read %s's "+
			"eviction row: %w", nodeID, err)
	}
	return statelog.EvictionRow{
		NodeID: nodeID, From: uint64(from), Readmitted: uint64(readmitted.Int64),
		// THE COMPARISON [Fence.Evicted] MAKES, so a retry and the node's
		// own fence can never disagree about whether it is back.
		Back: readmitted.Valid && readmitted.Int64 > from,
	}, true, nil
}

// Evictions is every eviction this log's applied rows hold — the rows the
// trim reads to stop counting an evicted node on THIS log once its fence
// window has passed.
//
// THIS DOMAIN'S OWN TABLE, because an eviction is a record on this log: every
// node applies it into `pages_evictions` and every node's copy is identical,
// which is what makes the applier's gate hold when coordination cannot be
// reached. The trim read the tracker's table instead, filtered on this log's
// stream name, and found nothing for as long as the fleet existed.
//
// THROUGH THE NODE HANDLE'S REPLICATED PEER, for the reason [Fence.Evicted]
// gives: the peer is nil while an adoption swaps the file, and [store.DB.Read]
// answers that as [store.ErrNoEstate] rather than a panic.
func (Domain) Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM pages_evictions ORDER BY node_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		out = nil
		for rows.Next() {
			var (
				e          statelog.EvictionRow
				at, from   int64
				readmitted sql.NullInt64
			)
			if err := rows.Scan(&e.NodeID, &at, &e.By, &from, &readmitted); err != nil {
				return err
			}
			e.At = store.DecodeTime(at)
			e.From = uint64(from)
			e.Readmitted = uint64(readmitted.Int64)
			// THE COMPARISON [Fence.Evicted] MAKES, so the trim and the
			// node's own fence can never disagree about whether it is
			// back.
			e.Back = readmitted.Valid && readmitted.Int64 > from
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("pages: read the evictions on this log: %w", err)
	}
	return out, nil
}
