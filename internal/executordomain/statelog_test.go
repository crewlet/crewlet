package executordomain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/store"
)

// THE EXECUTOR DOMAIN IS CERTIFIED BY THE FRAMEWORK'S OWN SUITE, from its
// declaration through its envelope, determinism, idempotency, versioned fields,
// stamp, evictions and node gates, exactly as every other domain is.
func TestTheExecutorDomainIsACertifiedDomain(t *testing.T) {
	t.Parallel()
	statelogtest.Run(t, func(t *testing.T) statelogtest.Candidate {
		return statelogtest.Candidate{
			Domain:  executordomain.Domain{},
			Applier: executordomain.NewApplier(),
			// Migrate is nil: these tables ship in the replicated estate's
			// own migration (0073), so a fresh store already has them.
			Encode: encodeSuiteRecord,
			// THE VERSIONED-FIELD TABLE, and a record carrying each of its
			// fields alone: a turn's begin, and a capture's context and its
			// cut-off, each stamped at version 2; an end's sessions that ran
			// out of context, at 3; and an end saying its turn went on past
			// its row's session, and one naming the turn's last word, at 4 —
			// each of which a build reading the version below retains rather
			// than applies.
			Fields:     executordomain.VersionedFields(),
			Carrying:   carryingSuiteRecord,
			Kinds:      kindNames(),
			Rows:       executordomain.NewRows,
			Write:      suiteWrite,
			EncodeGate: encodeSuiteGate,
			// THE PRODUCTION ENCODER of the read index's barrier, which the
			// suite holds to writing no row.
			EncodeBarrier: executordomain.EncodeBarrier,
			// THE PRODUCTION ENCODER the engine hands the trim (ADR-0080).
			EncodeTrimmed: executordomain.EncodeTrimmed,
		}
	})
}

// THE EXECUTOR GATE READER KEEPS THE RULE EVERY READER SHARES, on the one gate
// this log has: a writer's eviction window, half-open at both ends, on every
// kind the applier drops it on. A tombstone is a guard rather than a gate, so
// there is no deletion family to run.
func TestTheExecutorGateReaderKeepsTheEvictionWindow(t *testing.T) {
	t.Parallel()
	statelogtest.RunEvictionGates(t, func(t *testing.T) statelogtest.EvictionGateCandidate {
		return statelogtest.EvictionGateCandidate{
			Candidate: statelogtest.Candidate{
				Domain:  executordomain.Domain{},
				Applier: executordomain.NewApplier(),
				Encode:  encodeSuiteRecord,
				Kinds:   kindNames(),
			},
			Reader:       func(db store.ReplicatedReader) statelog.Gates { return executordomain.NewGates(db) },
			Kind:         string(executordomain.KindConversation),
			SubjectKinds: kindNames(),
			Create: func(id, writer, opID string) ([]byte, error) {
				return captureRecord(id, writer, opID, 1)
			},
			Write: func(id, writer, opID string) ([]byte, error) {
				return captureRecord(id, writer, opID, 2)
			},
			Evict: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(executordomain.EvictionSubject(node), writer, opID,
					executordomain.OpEviction, executordomain.ScopeSet{Subject: true},
					evictionOf(node, false))
			},
			Readmit: func(node, writer, opID string) ([]byte, error) {
				return rawRecord(executordomain.EvictionSubject(node), writer, opID,
					executordomain.OpEviction, executordomain.ScopeSet{Subject: true},
					evictionOf(node, true))
			},
			// THE EVICTION IS THE ONLY GATE: said with an empty map, which
			// is an answer, where nil would be one left unsaid.
			ApplyGates: map[string]func(id, writer, opID string) ([]byte, error){},
		}
	})
}

// kindNames is every kind the domain writes, from the enum.
func kindNames() []string {
	out := make([]string, 0, len(executordomain.ObjectKinds))
	for _, k := range executordomain.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteStamped is the instant every suite record is stamped at.
var suiteStamped = time.Unix(1_700_000_000, 0).UTC()

// suiteConversation is the conversation every suite capture is about.
const suiteConversation = "chat:suite"

// suiteTurn is the turn a suite record about id is about: derived, so the
// same id names the same turn in every record the suite builds.
func suiteTurn(id string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("turn/"+id)).String()
}

// suiteObject is a valid object key, minted at the suite's instant.
var suiteObject = objstore.KeyAt(suiteStamped).String()

// captureOf is a capture of id's turn at capture ordinal seq.
func captureOf(id string, seq int64) executordomain.Capture {
	return executordomain.Capture{
		V: 1, Handle: id, Turn: suiteTurn(id), ConversationKey: suiteConversation,
		Harness: "claude-code", SessionID: "session-" + id, Object: suiteObject,
		ObjectHash: string(objstore.HashOf([]byte(id))), ObjectSize: 1024,
		ContentKey: "k1", ArchiveFormat: 1, LaunchSeq: 1, CaptureSeq: seq,
		SeatBudget: executorkeep.SealedMaxBytes,
	}
}

// captureRecord is a capture of id's turn, written by writer.
func captureRecord(id, writer, opID string, seq int64) ([]byte, error) {
	turn := suiteTurn(id)
	return rawRecord(executordomain.ConversationSubject(id, suiteConversation, turn), writer,
		opID, executordomain.OpCapture, seatScope(id), captureOf(id, seq))
}

// seatScope is the scope a capture declares: its seat.
func seatScope(handle string) executordomain.ScopeSet {
	return executordomain.ScopeSet{Terms: []executordomain.ScopeTerm{
		{Kind: executordomain.TermSeat, Seat: handle}}}
}

// encodeSuiteRecord builds one valid record of each kind about id at any
// version.
//
// THE FIRST THREE KINDS ARE A REAL SEQUENCE: a turn's capture writes a row, its
// seat's removal deletes the row and tombstones the live turn, and a node's
// eviction writes the gate's row.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	subject, op, scope, payload := suitePayload(executordomain.ObjectKind(kind), id)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return executordomain.Encode(executordomain.MutationRecord{
		RecordEnvelope: executordomain.RecordEnvelope{
			V: version, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// suitePayload is one valid (subject, op, scope, payload) per kind about id.
func suitePayload(kind executordomain.ObjectKind, id string) (
	executordomain.Subject, executordomain.OpKind, executordomain.ScopeSet, any) {

	sentinel := executordomain.ScopeSet{Subject: true}
	switch kind {
	case executordomain.KindConversation:
		return executordomain.ConversationSubject(id, suiteConversation, suiteTurn(id)),
			executordomain.OpCapture, seatScope(id), captureOf(id, 1)
	case executordomain.KindSeat:
		return executordomain.SeatSubject(id), executordomain.OpSeatDelete, sentinel,
			executordomain.SeatDelete{V: 1, Handle: id}
	case executordomain.KindEviction:
		return executordomain.EvictionSubject(id), executordomain.OpEviction, sentinel,
			evictionOf(id, false)
	case executordomain.KindGeneration:
		return executordomain.GenerationSubject(2), executordomain.OpGeneration, sentinel,
			executordomain.Generation{V: 1, Generation: 2, By: "suite", StreamAt: suiteStamped}
	case executordomain.KindBarrier:
		return executordomain.Subject{Kind: kind, ID: id}, executordomain.OpBarrier, sentinel, nil
	}
	return executordomain.Subject{Kind: kind, ID: id}, executordomain.OpSeatDelete, sentinel,
		executordomain.SeatDelete{V: 1, Handle: id}
}

// carryingSuiteRecord is a record carrying exactly the versioned field named and
// no other, with its version left for the domain's encoder to stamp.
func carryingSuiteRecord(field statelog.VersionedField) ([]byte, error) {
	const id = "carrying"
	var (
		op      executordomain.OpKind
		scope   executordomain.ScopeSet
		payload any
	)
	switch field.Name {
	case "RecordEnvelope.Op begin":
		op, scope, payload = executordomain.OpBegin, executordomain.ScopeSet{Subject: true}, beginOf(id)
	case "Capture.ContextTokens":
		c := captureOf(id, 1)
		c.ContextTokens = 61_000
		op, scope, payload = executordomain.OpCapture, seatScope(id), c
	case "Capture.Interrupted":
		c := captureOf(id, 1)
		c.Interrupted = true
		op, scope, payload = executordomain.OpCapture, seatScope(id), c
	case "End.Ending.Exhausted":
		op, scope = executordomain.OpEnd, executordomain.ScopeSet{Subject: true}
		payload = executordomain.End{V: 1, Handle: id, Turn: suiteTurn(id), ConversationKey: suiteConversation,
			Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, Exhausted: []string{"session-" + id}}}
	case "End.Ending.WentOn":
		// A STOP WHOSE BOX WAS GONE, its turn having gone on past the session
		// its row names: the end that says so, and nothing else newer.
		op, scope = executordomain.OpEnd, executordomain.ScopeSet{Subject: true}
		payload = executordomain.End{V: 1, Handle: id, Turn: suiteTurn(id), ConversationKey: suiteConversation,
			Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, WentOn: true}}
	case "End.Ending.LastWord":
		// A TURN'S LAST WORD ALONE, without the went_on every end this build
		// writes names it beside: carrying exactly the one field, the record
		// is stamped by its own path, which a path that missed would leave at
		// a version a build reading 3 applies.
		op, scope = executordomain.OpEnd, executordomain.ScopeSet{Subject: true}
		payload = executordomain.End{V: 1, Handle: id, Turn: suiteTurn(id), ConversationKey: suiteConversation,
			Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
				LastWord: &executorkeep.CapturePlace{LaunchSeq: 1, CaptureSeq: 1}}}
	default:
		return nil, fmt.Errorf("no suite record carries %s: add one here with its row", field.Name)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return executordomain.Encode(executordomain.MutationRecord{
		RecordEnvelope: executordomain.RecordEnvelope{
			OpID: "suite-carrying", Subject: executordomain.ConversationSubject(id, suiteConversation,
				suiteTurn(id)),
			Op: op, CreatedAt: suiteStamped, Gen: 1, Writer: "suite-node", Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// beginOf is the begin of id's turn: its first read, and the base it froze.
func beginOf(id string) executordomain.Begin {
	base := suiteTurn("base/" + id)
	return executordomain.Begin{
		V: 1, Handle: id, Turn: suiteTurn(id), ConversationKey: suiteConversation,
		ReadThrough: 1, Absorbs: []string{base}, BaseTurn: base, BaseObject: suiteObject,
		BaseHash: string(objstore.HashOf([]byte(base))),
	}
}

// evictionOf is node's eviction, or its readmission.
func evictionOf(node string, readmit bool) executordomain.Eviction {
	return executordomain.Eviction{
		V: executordomain.GateRecordVersion, NodeID: node, EvictedBy: "suite",
		EvictedAt: suiteStamped, Readmitted: readmit,
	}
}

// rawRecord is one record the gate family applies, written by writer.
func rawRecord(subject executordomain.Subject, writer, opID string, op executordomain.OpKind,
	scope executordomain.ScopeSet, payload any) ([]byte, error) {

	var body json.RawMessage
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = encoded
	}
	return executordomain.Encode(executordomain.MutationRecord{
		RecordEnvelope: executordomain.RecordEnvelope{
			V: executordomain.RecordVersion, OpID: opID, Subject: subject, Op: op,
			CreatedAt: suiteStamped, Gen: 1, Writer: writer, Scope: scope,
		},
		Mutation: body, Actor: "suite", ActorKind: iam.ActorSystem,
	})
}

// encodeSuiteGate is the eviction record a peer publishes onto this log.
func encodeSuiteGate(node string, readmit bool) ([]byte, error) {
	opID := "suite-evict-" + node
	if readmit {
		opID = "suite-readmit-" + node
	}
	return rawRecord(executordomain.EvictionSubject(node), "suite-peer", opID,
		executordomain.OpEviction, executordomain.ScopeSet{Subject: true},
		evictionOf(node, readmit))
}

// suiteWrite is one write through the domain's own production writer, which is
// where the framework's stamp is kept or lost: a turn's end, which needs no
// object uploaded first — one that says its turn's newest capture was not kept,
// so the field the end's apply decides on is written, stamped and applied by
// the framework the conformance suite certifies.
func suiteWrite(ctx context.Context, pub *statelog.Publisher, db store.ReplicatedReader) error {
	w, err := executordomain.NewWriter(executordomain.WriterDeps{Publisher: pub, DB: db})
	if err != nil {
		return err
	}
	_, err = w.End(ctx, executordomain.Call{
		By:   iam.Actor{Name: "suite", Kind: iam.ActorSystem},
		OpID: statelog.NewOpID(time.Now(), "suite"),
	}, executordomain.NewEnd{
		Handle: "suite-seat", Turn: uuid.NewString(), ConversationKey: suiteConversation,
		Ask: "Suite", Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
			Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "suite: not kept"}},
	})
	return err
}
