package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EVICTION GATE ON THIS LOG, and the rest of what a log that claims
// identity owes the framework: the record that installs or lifts the gate, the
// rows the trim reads to stop counting an evicted node, the probe that reads a
// node's standing off the log, and the record a reanchor opens a generation
// with.
//
// The log carries its own eviction, not the tracker's: a gate is keyed on
// positions in ONE log's sequence space, so a tracker position says nothing
// about which identity records to drop, and the trim judges each log's counted
// set on its own. The engine's node gate writes every identity-claiming log
// from one judgement and reports each log's outcome.

// NodeGate reports a node's eviction or readmission, the one record a write
// flagged [statelog.Request.NodeGate] may carry.
//
// BY ITS KIND, not [Domain.InstallsGate]: a removal and a company-wide
// invalidation install gates too, and a generation record and a barrier are
// the log's own, but each is an ordinary write held to the gate reserve and
// the fences a node gate is excused. A removal flagged by mistake would spend
// the room kept for the eviction that unpins a full log.
func (Domain) NodeGate(env statelog.Envelope) bool {
	return ObjectKind(env.Kind) == KindEviction
}

// FeedGroup is empty: nothing consumes this log as a change feed, so the
// trim's feed term is absent here rather than waiting on a consumer that never
// exists.
func (Domain) FeedGroup() string { return "" }

// EvictionSubject is where a node's evictions and readmissions are published
// on this log, the [statelog.EvictionProbe] half: a node the fleet re-anchored
// past never applies an eviction written after its applier stopped, so its
// standing is read off the log itself.
func (Domain) EvictionSubject(node string) statelog.Subject {
	subject := EvictionSubject(node)
	return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID}
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
//
// It takes the record's BODY, which a verified log hands back
// ([statelog.VerifiedLog]): every record here is a signed frame, and decoding
// the frame itself would read every record as unreadable.
func (Domain) Evicts(payload []byte) (bool, error) {
	record, err := Decode(payload)
	if err != nil {
		return false, fmt.Errorf("iamdomain: decode a record from an eviction "+
			"subject: %w", err)
	}
	if record.Subject.Kind != KindEviction || record.Op != OpEviction {
		return false, fmt.Errorf("iamdomain: the record on an eviction subject "+
			"is op %q on %s", record.Op, record.Subject)
	}
	eviction, err := DecodeEviction(record.Mutation)
	if err != nil {
		return false, err
	}
	return !eviction.Readmit, nil
}

// Evictions is every eviction this log's applied rows hold: the rows the trim
// reads to stop counting an evicted node on THIS log once its fence window has
// passed.
//
// THIS DOMAIN'S OWN TABLE, `iam_evictions`, because an eviction is a record on
// this log: every data node's copy is identical, so the applier's gate holds
// when coordination cannot be reached. The tracker's table holds none of this
// log's evictions, and an evicted node would be counted here for ever.
//
// THROUGH THE REPLICATED ESTATE'S HANDLE, for the reason [Fence.Evicted]
// gives: [store.ReplicatedReader.Read] answers an adoption's file swap as
// [store.ErrNoEstate] rather than a panic.
func (Domain) Evictions(ctx context.Context, db store.ReplicatedReader) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM iam_evictions ORDER BY node_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		// RESET PER ATTEMPT: the store re-runs a transiently failed read
		// body, which would list the earlier attempt's rows twice.
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
			// THE BROKER'S INSTANT, in estate milliseconds: the fence
			// window is measured from it on every node alike.
			e.At = fromMillis(at)
			e.From = uint64(from)
			e.Readmitted = uint64(readmitted.Int64)
			e.Back = back(readmitted, from)
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("iamdomain: read the evictions on this log: %w", err)
	}
	return out, nil
}

// EvictNode installs the gate that drops a node's records on this log.
//
// THE POSITION IS THE LOG'S OWN: every data node reaches the same verdict on
// every record from the order they share, with no clock and no coordination
// read, so the fence holds when coordination cannot be reached at all.
//
// UNJUDGED HERE: whether the node may be evicted is a fleet question, asked
// ONCE by the engine's node gate before the first log is written; judged per
// log, two logs could answer differently. Only this writer's authority is
// checked: admin, since a node is the deployment's ([Writer.mayOperate]).
func (w *Writer) EvictNode(ctx context.Context, opID, nodeID string) (statelog.Result, error) {
	return w.gateNode(ctx, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// history survives a replay, and a node evicted, readmitted and evicted again
// reads correctly rather than as one long absence.
func (w *Writer) ReadmitNode(ctx context.Context, opID, nodeID string) (statelog.Result, error) {
	return w.gateNode(ctx, opID, nodeID, true)
}

// gateNode publishes one eviction record, arbitrated on the node's own subject
// so an eviction and its readmission contend with each other and nothing else.
//
// A RETRY IS ANSWERED BY THE FRAMEWORK from this node's ledger before this
// decision runs ([statelog.Snap.Held]); this write adds whether the record its
// operation landed is still in force, judged from the node's row in the same
// snapshot ([statelog.GateStanding]).
//
// THE AUTHOR IS THE WRITER'S PARTY, on the record and the payload's `by` (the
// column an operator reads first on an unexpected eviction), with the
// credential it acted through on the record, as on every log's eviction.
func (w *Writer) gateNode(ctx context.Context, opID, nodeID string,
	readmit bool) (statelog.Result, error) {

	switch {
	case nodeID == "":
		return statelog.Result{}, errors.New("iamdomain: an eviction names no node")
	case opID == "":
		return statelog.Result{}, fmt.Errorf("iamdomain: the eviction of %s has "+
			"no operation id — a retry under a fresh one would write the gate "+
			"twice", nodeID)
	}
	if err := w.mayOperate(OpEviction); err != nil {
		return statelog.Result{}, err
	}
	// A GATE'S PAYLOAD IS PINNED ([GateRecordVersion]) and its record stays
	// at the base version ([MutationRecord.readByEveryBuild]): a node must
	// never be able to defer the record that gates it.
	mutation, err := EncodeEviction(Eviction{
		V: GateRecordVersion, Readmit: readmit, By: w.Actor,
	})
	if err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.record(EvictionSubject(nodeID), OpEviction, "", RootScope(),
		mutation, "")
	if err != nil {
		return statelog.Result{}, err
	}
	req := w.request(ctx, &rec, opID, statelog.PatternArbitrated, nil)
	req.NodeGate = true
	req.Standing = func(tx *sql.Tx, held statelog.Position) error {
		row, found, err := standingIn(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		return statelog.GateStanding(opID, nodeID, readmit, held, row, found)
	}
	return w.publish(ctx, req)
}

// standingIn is nodeID's eviction row on this log, read in the transaction the
// caller holds.
func standingIn(ctx context.Context, tx *sql.Tx, nodeID string) (statelog.EvictionRow, bool, error) {
	var from int64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM iam_evictions
		WHERE node_id = ?`, nodeID).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("iamdomain: read %s's "+
			"eviction row: %w", nodeID, err)
	}
	return statelog.EvictionRow{
		NodeID: nodeID, From: uint64(from), Readmitted: uint64(readmitted.Int64),
		Back: back(readmitted, from),
	}, true, nil
}

// GenerationRecord is the identity estate's [statelog.GenerationEncoder]: the
// record a reanchor of this log opens its new generation with. The applier
// already writes `iam_log_generations` from it ([Applier.applyGeneration]), so
// nothing here writes a row.
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on —
// what a reader of the log asks to learn who opened a generation
// ([statelog.GenerationOpeners]).
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	subject := GenerationSubject(gen)
	return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID}, true
}

// GenerationRecord encodes the reanchor's record for the NEW generation,
// create-only on its own subject so exactly one of two operators deriving the
// same number lands. The op id names the writer
// ([statelog.GenerationFacts.OpID]): a shared id would let the broker's
// duplicate window acknowledge the second as landed.
//
// THE OPERATOR IS THE AUTHOR, on the record and the `by` of the audit row a
// replay writes, since who moved the log is what `iam_log_generations` is read
// for; nobody named is the node ([generationActor]). No credential: it writes
// no trail row to read one. PINNED AT THE BASE VERSION ([Encode]): it is not a
// gate, and a version a peer cannot read would defer the record saying its log
// moved on.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	author, kind := generationActor(f)
	mutation, err := EncodeGeneration(Generation{
		V:                  GateRecordVersion,
		Generation:         f.Generation,
		PrevLastSeqSeen:    f.Inputs.Highest,
		NewStreamCreatedAt: f.Inputs.StreamCreatedAt.UTC(),
		By:                 author,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("iamdomain: encode "+
			"generation %d: %w", f.Generation, err)
	}
	opID := f.OpID()
	// STAMPED WITH ITS GENERATION AND WRITER: it opens the generation it
	// names, and the eviction gate reads the node that wrote it.
	payload, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: BaseRecordVersion, OpID: opID,
			Subject: subject, Op: OpGeneration, CreatedAt: f.At.UTC(),
			Scope: RootScope(), Gen: f.Generation, Writer: f.Writer,
		},
		Mutation:  mutation,
		Actor:     author,
		ActorKind: kind,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	return statelog.GenerationRecord{
		Subject: statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
		OpID:    opID, Payload: payload,
	}, true, nil
}

// generationActor is who a generation record names as its author, and the
// principal kind this domain's records carry for them: the party of
// [statelog.GenerationFacts.Author], which every domain names alike so one
// reanchor names one party on every log, in the PRINCIPAL's kind where other
// domains carry the actor's.
//
// [iam.ActorOperator] names a kind of actor, not of principal (a machine's
// login, since every person acts as their seat, ADR-0032), so its principal
// kind is read off the login's SHAPE: the grammars are disjoint
// ([iam.ValidLogin] joins with dots, a machine handle with a colon). Nobody
// named is the node, recorded as its own writer records itself ([NodeKind]),
// never as an empty author.
func generationActor(f statelog.GenerationFacts) (string, iam.Kind) {
	party, named := f.Author()
	if !named {
		return f.Writer, NodeKind
	}
	switch party.Kind {
	case iam.ActorAgent:
		return party.Name, iam.KindSeat
	case iam.ActorHuman:
		return party.Name, iam.KindPerson
	case iam.ActorSystem:
		return party.Name, iam.KindEngine
	}
	// AN OPERATOR or an unstated kind, by the login's shape; a machine's
	// covers a credential nobody enrolled (a Tier A token).
	if iam.ValidLogin(party.Name) {
		return party.Name, iam.KindPerson
	}
	return party.Name, iam.KindMachine
}
