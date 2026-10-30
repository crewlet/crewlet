package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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
// says nothing about which chat records to drop, and the trim evaluates each
// log's counted set on its own. Until this file the half here was an applier,
// a fence and a table that nothing could fill: an operator's eviction lifted
// the tracker's and the knowledge base's pins and left this log's where it
// was, so the chat log grew toward its ceiling behind a node that was never
// coming back — and on the log that refuses a message somebody typed when it
// is full. The engine's node gate writes every identity-claiming log from one
// judgement, and reports each log's outcome on its own.
//
// # Why the writer is not the room store
//
// [Store] needs a [Roster], because who a message wakes is resolved at write
// time against the company's chart. An eviction wakes nobody and reads no
// chart: it is said about a machine, not in a room. Hung off the store, the
// node gate could only be built by handing it the live roster or a fake one —
// on a node and a backend where no chat roster is wired at all, which is where
// a log still has to be unpinned — so it is a writer of its own, built from the
// one thing it uses.

// GateWriter is the node-gate writer on this log: a node's eviction and its
// readmission, and nothing else.
type GateWriter struct {
	publisher *statelog.Publisher

	// now is the clock the AUTHORED instant on an eviction is stamped
	// from. The row's own instant is the broker's; see [GateOptions.Now].
	now func() time.Time
}

// GateOptions configure a [GateWriter].
type GateOptions struct {
	// Publisher is this domain's write authority. It is the whole of what
	// the writer needs: the standing a retry is judged by is read in the
	// framework's own snapshot transaction, and an eviction resolves no
	// recipient.
	Publisher *statelog.Publisher

	// Now is the clock an eviction's authored instant is stamped from. Nil
	// takes the wall clock in UTC — the same reading [Options.Now] takes,
	// for the same reason: an argument rather than a package call, so a
	// test can pin it.
	Now func() time.Time
}

// NewGateWriter builds the node-gate writer over this domain's log.
//
// THE PUBLISHER IS REFUSED BY NAME, for [NewStore]'s reason: a gate writer with
// none would validate an eviction and then write it nowhere, and the trim would
// go on counting the node it was meant to drop.
func NewGateWriter(opts GateOptions) (*GateWriter, error) {
	if opts.Publisher == nil {
		return nil, errors.New("chatlet: a write authority is required — an " +
			"eviction is a record on the chat log, and a gate writer with no " +
			"publisher could validate one and then write it nowhere")
	}
	g := &GateWriter{publisher: opts.Publisher, now: opts.Now}
	if g.now == nil {
		g.now = nowUTC
	}
	return g, nil
}

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
//
// THE ACTOR IS THE PRINCIPAL WHO RAN IT, recorded the way the node gate
// records one on every log — the name, the kind of party and the credential
// `iam.ActorFor` gives it — so the tracker's, the wiki's and this log's records
// of one eviction name one party. An eviction is said about a machine rather
// than in a room, so unlike a remark it needs no seat: a credential acting as
// itself makes one under its own login.
func (g *GateWriter) EvictNode(ctx context.Context, actor Actor, opID, nodeID string) (statelog.Result, error) {
	return g.gateNode(ctx, actor, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
//
// Unjudged here for [GateWriter.EvictNode]'s reason: a node below a trim floor
// is refused a readmission by the engine's node gate, which reads the
// positions register, the published floors and every identity-claiming log at
// once.
func (g *GateWriter) ReadmitNode(ctx context.Context, actor Actor, opID, nodeID string) (statelog.Result, error) {
	return g.gateNode(ctx, actor, opID, nodeID, true)
}

// gateNode publishes one eviction record, arbitrated on the node's own
// subject so an eviction and the readmission that inverts it contend with each
// other and with nothing else.
//
// ARBITRATED ALTHOUGH THE KIND IS NOT IN [ObjectKind.Arbitrated]'s two. That
// set names the kinds an ANCHOR row is written for, and an eviction subject
// takes too few writes for one to be worth a row: the publisher forms this
// append's expectation from the subject's last sequence on the broker instead,
// one round trip on a gesture an operator makes by hand.
//
// A RETRY IS ANSWERED BY THE FRAMEWORK from this node's ledger, before this
// decision runs ([statelog.Snap.Held]); what this write adds is whether the
// record its operation landed is still in force, judged from the node's own
// row in the same snapshot ([statelog.GateStanding]).
//
// THE FRAMEWORK'S REFUSALS COME BACK IN ITS OWN WORDS. [Store.publish]
// translates a lost race into "the room kept changing", which is the answer a
// person posting needs; on a node's eviction subject it would name a room after
// a node id, and the operator contending with another over one node's gate is
// told what actually happened instead.
func (g *GateWriter) gateNode(ctx context.Context, actor Actor, opID, nodeID string,
	readmit bool) (statelog.Result, error) {

	if nodeID == "" {
		return statelog.Result{}, invalid("node_id", "an eviction names no node, "+
			"so it fences nobody while still stopping every applier that cannot "+
			"read it")
	}
	if err := requireOpID(opID, "eviction of "+nodeID); err != nil {
		return statelog.Result{}, err
	}
	if err := actor.validate(); err != nil {
		return statelog.Result{}, err
	}
	subject := EvictionSubject(nodeID)
	scope := ScopeSet{Subject: true}
	at := g.now()
	return g.publisher.Publish(ctx, statelog.Request{
		Subject:  wire(subject),
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
			return decide(stamp, actor, subject, OpEviction, scope, opID, Eviction{
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
		SELECT from_position, readmitted_position FROM chat_evictions
		WHERE node_id = ?`, nodeID).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("chatlet: read %s's "+
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
// node applies it into `chat_evictions` and every node's copy is identical,
// which is what makes the applier's gate hold when coordination cannot be
// reached.
//
// THROUGH THE HANDLE, for the reason [Fence.Evicted] gives: the estate is not
// open while an adoption swaps the file, and [store.ReplicatedReader.Read]
// answers that as [store.ErrNoEstate] rather than a panic.
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM chat_evictions ORDER BY node_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		// A RETRIED READ STARTS EMPTY: the store re-runs this body on a
		// conflicted transaction, and rows from the first attempt would
		// otherwise be reported twice.
		out = nil
		for rows.Next() {
			var (
				e          statelog.EvictionRow
				at, from   int64
				readmitted sql.NullInt64
			)
			if scanErr := rows.Scan(&e.NodeID, &at, &e.By, &from, &readmitted); scanErr != nil {
				return scanErr
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
		return nil, fmt.Errorf("chatlet: read the evictions on this log: %w", err)
	}
	return out, nil
}
