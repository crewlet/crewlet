package chart

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EVICTION GATE ON THIS LOG, and the other halves of the framework's
// contract a log that claims identity owes: the record that installs or lifts
// the gate, the rows the trim reads to stop counting an evicted node, the probe
// that reads a node's standing straight off the log, and the record a reanchor
// opens a generation with.
//
// # Why this log carries its own eviction and not the tracker's
//
// A gate is a rule about records on ONE log, keyed on positions in that log's
// own sequence space — so the tracker's eviction row, at a tracker position,
// says nothing about which chart records to drop, and the trim evaluates each
// log's counted set on its own. The applier and the fence here always knew the
// record; nothing wrote one, so an operator's eviction lifted the tracker log's
// pin and left this one's where it was. The engine's node gate writes every
// identity-claiming log from one judgement, and reports each log's outcome.

// NodeGate reports a node's eviction or readmission — the one record a write
// flagged [statelog.Request.NodeGate] may carry.
//
// BY ITS KIND, which nothing but a node's eviction or readmission is published
// under — and deliberately not InstallsGate: a removal is this log's other
// gate, and it is an ordinary write that the gate reserve and the fences a
// node gate is excused must hold.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// FeedGroup is empty: nothing consumes this log as a change feed, so the
// trim's feed term is absent here rather than waiting on a consumer that never
// exists — which is what it did, reading the tracker's group on every log and
// permitting nothing on this one.
func (Domain) FeedGroup() string { return "" }

// EvictionSubject is where a node's evictions and readmissions are published
// on this log — the [statelog.EvictionProbe] half: a node the fleet
// re-anchored past never applies an eviction written after its applier
// stopped, so its standing is read off the log itself.
func (Domain) EvictionSubject(node string) statelog.Subject {
	return wire(EvictionSubject(node))
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("chart: decode a record from an eviction subject: %w", err)
	}
	mutation, err := DecodeMutation(record)
	if err != nil {
		return false, fmt.Errorf("chart: decode the eviction: %w", err)
	}
	eviction, ok := mutation.(Eviction)
	if !ok {
		return false, fmt.Errorf("chart: the record on an eviction subject carries a %T",
			mutation)
	}
	return !eviction.Readmit, nil
}

// Evictions is every eviction this log's applied rows hold — the rows the trim
// reads to stop counting an evicted node on THIS log once its fence window has
// passed.
//
// THIS DOMAIN'S OWN TABLE, because an eviction is a record on this log: every
// node applies it into `chart_evictions`, and every node's copy is identical,
// which is what makes the applier's gate hold when coordination cannot be
// reached. The trim read the tracker's table instead, filtered on this log's
// stream name, and found nothing for as long as the fleet existed.
//
// THROUGH THE NODE HANDLE'S REPLICATED PEER, for the reason [Fence.Evicted]
// gives: the peer is nil while an adoption swaps the file, and
// [store.DB.Read] answers that as [store.ErrNoEstate] rather than a panic.
func (Domain) Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM chart_evictions ORDER BY node_id`)
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
		return nil, fmt.Errorf("chart: read the evictions on this log: %w", err)
	}
	return out, nil
}

// EvictNode installs the gate that drops a node's records on this log.
//
// THE POSITION IS THE LOG'S OWN, and nothing else is: every node reaches the
// same verdict about every record from the order they all already have, with
// no clock and no coordination read — which is what makes it the fence that
// holds when coordination cannot be reached at all.
//
// UNJUDGED HERE. Whether the node may be evicted is a fleet question this
// package holds none of the inputs to, and it is asked ONCE, before the first
// log is written, by the engine's node gate: judged per log, two logs could
// reach two answers about one node. What IS asked here is this writer's own
// authority ([ClassNodeGate]).
func (w *Writer) EvictNode(ctx context.Context, opID, nodeID string) (statelog.Result, error) {
	return w.gateNode(ctx, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted
// and evicted again reads correctly rather than as one long absence.
func (w *Writer) ReadmitNode(ctx context.Context, opID, nodeID string) (statelog.Result, error) {
	return w.gateNode(ctx, opID, nodeID, true)
}

// gateNode publishes one eviction record, arbitrated on the node's own subject
// so an eviction and the readmission that inverts it contend with each other
// and with nothing else.
//
// A RETRY IS ANSWERED BY THE FRAMEWORK from this node's ledger, before this
// decision runs ([statelog.Snap.Held]); what this write adds is whether the
// record its operation landed is still in force, judged from the node's own
// row in the same snapshot ([statelog.GateStanding]).
func (w *Writer) gateNode(ctx context.Context, opID, nodeID string,
	readmit bool) (statelog.Result, error) {

	switch {
	case nodeID == "":
		return statelog.Result{}, fmt.Errorf("chart: an eviction names no node")
	case opID == "":
		return statelog.Result{}, fmt.Errorf("chart: the eviction of %s has no "+
			"operation id — a retry under a fresh one would write the gate twice",
			nodeID)
	}
	subject := EvictionSubject(nodeID)
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.publish(ctx, statelog.Request{
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
			// PINNED AT [GateRecordVersion], as every gate record is: a node
			// must never be able to defer the record that gates it.
			return w.record(stamp, subject, OpEviction, opID, at, scope, Eviction{
				V: GateRecordVersion, NodeID: nodeID, Readmit: readmit, By: w.Actor,
			}, nodeGate)
		},
	})
}

// standingIn is nodeID's eviction row on this log, read in the transaction the
// caller holds.
func standingIn(ctx context.Context, tx *sql.Tx, nodeID string) (statelog.EvictionRow, bool, error) {
	var from int64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM chart_evictions
		WHERE node_id = ?`, nodeID).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("chart: read %s's "+
			"eviction row: %w", nodeID, err)
	}
	return statelog.EvictionRow{
		NodeID: nodeID, From: uint64(from), Readmitted: uint64(readmitted.Int64),
		// THE COMPARISON [Fence.Evicted] MAKES, so a retry and the node's
		// own fence can never disagree about whether it is back.
		Back: readmitted.Valid && readmitted.Int64 > from,
	}, true, nil
}

// GenerationRecord is the org chart's [statelog.GenerationEncoder]: the record
// a reanchor of this log opens its new generation with. The applier already
// writes `chart_log_generations` from it, so nothing here writes a row.
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on —
// what a reader of the log asks to learn who opened a generation
// ([statelog.GenerationOpeners]).
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return wire(GenerationSubject(gen)), true
}

// GenerationRecord encodes the reanchor's record for the NEW generation,
// create-only on the generation's own subject so two operators deriving the
// same number race at the broker and exactly one record lands.
//
// THE OPERATION ID NAMES THE WRITER ([statelog.GenerationFacts.OpID]): two
// nodes deriving the same number are two operations, and one id between them
// let the broker's duplicate window acknowledge the second as though it had
// landed.
//
// THE OPERATOR IS THE AUTHOR, in the three columns every write here carries —
// the name, the kind of party and the credential — so the audit row a replay
// writes on every node says who moved the log and through what.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	scope := ScopeSet{Subject: true}
	opID := f.OpID()
	kind := AuthorKind(f.ByKind)
	if kind == "" {
		kind = AuthorOperator
	}
	body, err := json.Marshal(Generation{
		V:                  GateRecordVersion,
		Generation:         f.Generation,
		PrevLastSeqSeen:    f.Inputs.Highest,
		NewStreamCreatedAt: f.Inputs.StreamCreatedAt.UTC(),
		By:                 f.By,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("chart: encode "+
			"generation %d: %w", f.Generation, err)
	}
	// THE GENERATION AND THE WRITER ARE STAMPED, and both are this record's
	// own: it opens the generation it names, and the eviction gate reads the
	// node that wrote it.
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: GateRecordVersion, OpID: opID, Subject: subject, Op: OpGeneration,
			Gen: f.Generation, Writer: f.Writer, CreatedAt: f.At.UTC(), Scope: scope,
		},
		Mutation:   body,
		Actor:      f.By,
		ActorKind:  kind,
		OperatorID: f.OperatorID,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	return statelog.GenerationRecord{Subject: wire(subject), OpID: opID, Payload: encoded},
		true, nil
}
