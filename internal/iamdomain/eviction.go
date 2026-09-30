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
// says nothing about which identity records to drop, and the trim evaluates
// each log's counted set on its own. The applier and the fence here always knew
// the record; nothing wrote one, so an operator's eviction lifted the tracker
// log's pin and left this one's where it was — and on this log the pin is the
// authentication trail, which grows every morning. The engine's node gate
// writes every identity-claiming log from one judgement, and reports each log's
// outcome.

// NodeGate reports a node's eviction or readmission — the one record a write
// flagged [statelog.Request.NodeGate] may carry.
//
// BY ITS KIND, which nothing but a node's eviction or readmission is published
// under — and deliberately NOT [Domain.InstallsGate]: a removal and a
// company-wide invalidation install gates too, and a generation record and a
// barrier are the log's own, but every one of them is an ordinary write the
// gate reserve and the fences a node gate is excused must hold. A removal
// flagged by mistake would spend the room kept for the eviction that unpins a
// full log.
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
	subject := EvictionSubject(node)
	return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID}
}

// Evicts decodes one record from a node's eviction subject: true for an
// eviction, false for the readmission that inverts one.
//
// THE RECORD'S BODY, which is what a verified log hands back
// ([statelog.VerifiedLog]): every record on this log is a signed frame, and a
// reader that decoded the frame itself would read every record as unreadable.
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

// Evictions is every eviction this log's applied rows hold — the rows the trim
// reads to stop counting an evicted node on THIS log once its fence window has
// passed.
//
// THIS DOMAIN'S OWN TABLE, because an eviction is a record on this log: every
// node applies it into `iam_evictions`, and every node's copy is identical,
// which is what makes the applier's gate hold when coordination cannot be
// reached. The trim read the tracker's table instead, filtered on this log's
// stream name, and found nothing for as long as the fleet existed — so an
// evicted node was counted here for ever, and this log grew toward its ceiling
// behind a machine that was never coming back.
//
// THROUGH THE NODE HANDLE'S REPLICATED PEER, for the reason [Fence.Evicted]
// gives: the peer is nil while an adoption swaps the file, and
// [store.DB.Read] answers that as [store.ErrNoEstate] rather than a panic.
func (Domain) Evictions(ctx context.Context, db *store.DB) ([]statelog.EvictionRow, error) {
	var out []statelog.EvictionRow
	err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT node_id, at, by, from_position, readmitted_position
			FROM iam_evictions ORDER BY node_id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		// RESET PER ATTEMPT: the store re-runs a read body that failed
		// transiently, and rows from the attempt before would be listed
		// twice.
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
			// THE BROKER'S OWN INSTANT, in this estate's milliseconds,
			// which is what the fence window is measured from on every
			// node alike.
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
// THE POSITION IS THE LOG'S OWN, and nothing else is: every node reaches the
// same verdict about every record from the order they all already have, with
// no clock and no coordination read — which is what makes it the fence that
// holds when coordination cannot be reached at all.
//
// UNJUDGED HERE. Whether the node may be evicted is a fleet question this
// package holds none of the inputs to, and it is asked ONCE, before the first
// log is written, by the engine's node gate: judged per log, two logs could
// reach two answers about one node. What IS asked here is this writer's own
// authority: `fleet:operate`, since a node is the deployment's and
// administering people confers no say over it ([Writer.mayOperate]).
func (w *Writer) EvictNode(ctx context.Context, opID, nodeID string) (statelog.Result, error) {
	return w.gateNode(ctx, opID, nodeID, false)
}

// ReadmitNode is the INVERSE COMMIT rather than a delete, so an eviction's
// whole history survives a replay — and a node that was evicted, readmitted and
// evicted again reads correctly rather than as one long absence.
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
//
// THE AUTHOR IS THE WRITER'S PARTY, on the record and on the payload's `by` —
// the column an operator reads first on an eviction they did not expect. The
// credential it acted through is not on the record: an eviction writes no
// trail row, and a gate is pinned at [GateRecordVersion], which predates the
// field ([namesOperator]).
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
	// PINNED AT [GateRecordVersion], as every gate record is: a node must
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
// create-only on the generation's own subject so two operators deriving the
// same number race at the broker and exactly one record lands.
//
// THE OPERATION ID NAMES THE WRITER ([statelog.GenerationFacts.OpID]): two
// nodes deriving the same number are two operations, and one id between them
// let the broker's duplicate window acknowledge the second as though it had
// landed.
//
// THE OPERATOR IS THE AUTHOR, on the record and on the `by` the audit row a
// replay writes on every node carries — who moved the log is the one fact a
// later reader of `iam_log_generations` is asking. A reanchor nobody named is
// the node's own, and is recorded as the node's writer is ([generationActor]).
// The credential is not on the record: a generation writes no trail row, so a
// field carried for it would be read by nothing while deferring the record on
// every older node ([namesOperator]).
//
// AT THE BASE VERSION ([writeVersion]): a generation record is not a gate, and
// a version a peer cannot read would defer the very record that says its log
// moved on.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	author, kind := generationActor(f)
	mutation, err := EncodeGeneration(Generation{
		V:                  DocumentVersion,
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
	// THE GENERATION AND THE WRITER ARE STAMPED, and both are this record's
	// own: it opens the generation it names, and the eviction gate reads the
	// node that wrote it.
	payload, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V: writeVersion(OpGeneration, false), OpID: opID,
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
// principal kind this domain's records carry for them.
//
// THE FACTS' KIND IS READ IN EITHER VOCABULARY — a principal's kind, or the
// kind [iam.ActorFor] records it under — as the knowledge base's and the
// tracker's generation records read it, so one reanchor names one party the
// same way on every log: the reanchor verb is the framework's and names its
// operator the way every other domain's audit columns do, while this domain's
// records carry the PRINCIPAL's kind.
//
// THREE ACTOR KINDS NAME ONE PRINCIPAL KIND EACH, AND THE FOURTH DOES NOT.
// [iam.ActorOperator] is a principal acting under its own login, which is an
// unbound PERSON's or a MACHINE's — and the login's SHAPE is which, because
// the two grammars are disjoint by construction ([iam.ValidLogin] joins with
// dots, a machine handle with a colon). Mapping every operator to a machine
// recorded an administrator who is not in the org chart, reanchoring by hand,
// as a service account.
//
// NOBODY NAMED IS THE NODE, recorded as the node's own writer records itself —
// its id, a machine — rather than as an empty author column.
func generationActor(f statelog.GenerationFacts) (string, iam.Kind) {
	if f.By == "" {
		return f.Writer, iam.KindMachine
	}
	if kind := iam.Kind(f.ByKind); kind.Valid() {
		return f.By, kind
	}
	switch iam.ActorKind(f.ByKind) {
	case iam.ActorAgent:
		return f.By, iam.KindSeat
	case iam.ActorHuman:
		return f.By, iam.KindPerson
	case iam.ActorSystem:
		return f.By, iam.KindEngine
	}
	// AN OPERATOR, and a kind nobody stated, by the login's shape: a
	// person's where it is one, and a machine's otherwise — the kind a
	// credential nobody enrolled, a Tier A token, is recorded under.
	if iam.ValidLogin(f.By) {
		return f.By, iam.KindPerson
	}
	return f.By, iam.KindMachine
}
